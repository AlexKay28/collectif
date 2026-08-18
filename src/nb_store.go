package main

// nb_store.go — the append-only notebook log, its snapshot cache, and the
// per-notebook registry. #49 (M1), per ADR 0001 §4.3.
//
// Layout, following the precedent set by the GitHub mirror in gh.go:
//
//	<dir>/<slug>.jsonl        append-only, one event per line, the truth
//	<dir>/<slug>.snap.json    derived cache; safe to delete at any time
//
// The snapshot exists only so opening a long notebook doesn't re-fold
// thousands of events. Every disagreement is resolved in the log's favour,
// and a snapshot that is corrupt, stale, or ahead of the log is discarded
// rather than trusted — a cache that can truncate a user's document is
// worse than no cache.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// nbSnapshotEvery is how many appends pass before the cache is rewritten.
// Low enough that a crash re-folds little, high enough that a chatty run
// isn't rewriting the whole document constantly.
const nbSnapshotEvery = 200

// notebookSnapshot is the on-disk cache. Version is the log position the
// document was folded to, so a reopen can apply just the events after it.
type notebookSnapshot struct {
	Version  int       `json:"version"`
	Notebook *Notebook `json:"notebook"`

	// LastEventID is the id of the last event folded into this snapshot.
	// It is what makes the cache verifiable: a snapshot is only usable if
	// the log's event at Version-1 is that same event, which proves the
	// snapshot summarises *this* history rather than some other one that
	// happened to reach the same length.
	//
	// Without it, load() replayed the log's tail onto whatever base the
	// snapshot happened to hold. Two collectif processes sharing a notebook
	// directory, a restored backup, or a rewritten log all produce a
	// snapshot of a different history — and the document served afterwards
	// is silently wrong. A real notebook lost its entire Meta block this
	// way and reported no session while its log plainly recorded one.
	//
	// Empty means a snapshot written before this field existed. Those are
	// treated as unverifiable and discarded: re-folding a log is cheap and
	// serving a wrong document is not.
	LastEventID string `json:"lastEventId,omitempty"`
}

// notebookStore owns one notebook's log and its in-memory fold.
//
// Locking: mu guards nb, sinceSnapshot and the log file handle. It is never
// held while calling out to anything that might take another lock — the
// same discipline session.go documents for s.mu.
type notebookStore struct {
	dir  string
	slug string

	mu            sync.Mutex
	nb            *Notebook
	log           *os.File
	sinceSnapshot int
	// lastEventID is the id of the most recently folded event, stamped into
	// each snapshot so a later load can prove the snapshot belongs to this
	// log rather than to some other history of the same length.
	lastEventID string
	closed      bool

	// subMu guards subs ONLY, and is never held while mu is held (or vice
	// versa) — the two are strictly independent, as in session.go.
	subMu sync.Mutex
	subs  map[*wsSub]bool

	// runsMu guards runs ONLY — in-flight cell executions, keyed by cell
	// id. Independent of mu and subMu for the same reason: a running
	// command must never be able to block an append or a broadcast.
	runsMu sync.Mutex
	runs   map[string]*nbRun

	// liveMu guards liveOut ONLY — what each running cell has produced so
	// far, keyed by cell id. Deltas are never persisted, so without this a
	// client that connects mid-run (a page refresh) would see a running
	// cell with nothing in it until the run finished. Cleared the moment
	// the finalised output reaches the log, which is then the record.
	liveMu  sync.Mutex
	liveOut map[string]*liveOutput
}

// liveOutput is the un-persisted, in-progress output of one run.
type liveOutput struct {
	RunID  string `json:"runId"`
	Text   string `json:"text"`
	capped bool   `json:"-"` // cap reached; stop accepting and stop broadcasting
}

func (st *notebookStore) logPath() string  { return filepath.Join(st.dir, st.slug+".jsonl") }
func (st *notebookStore) snapPath() string { return filepath.Join(st.dir, st.slug+".snap.json") }

// openNotebookStore opens an existing notebook or creates one. title and
// root are used only when creating; an existing notebook keeps whatever its
// log says, because the log is the truth.
func openNotebookStore(dir, slug, title, root string) (*notebookStore, error) {
	if slug == "" {
		return nil, fmt.Errorf("notebook slug required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("notebook dir: %w", err)
	}
	st := &notebookStore{dir: dir, slug: slug}

	created := false
	if _, err := os.Stat(st.logPath()); os.IsNotExist(err) {
		created = true
	} else if err != nil {
		return nil, fmt.Errorf("stat notebook log: %w", err)
	}

	if !created {
		nb, err := st.load()
		if err != nil {
			return nil, err
		}
		st.nb = nb
	} else {
		st.nb = &Notebook{}
	}

	f, err := os.OpenFile(st.logPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open notebook log: %w", err)
	}
	st.log = f

	if created {
		if _, err := st.Append(evNotebookCreated, notebookCreatedPayload{Title: title, Root: root}); err != nil {
			f.Close()
			return nil, err
		}
	}
	// The slug is the notebook's identity on disk; carry it into the
	// document rather than storing it redundantly in the log.
	st.mu.Lock()
	st.nb.ID = slug
	st.mu.Unlock()
	return st, nil
}

// load folds the notebook from disk, using the snapshot as a starting point
// when it is usable and re-folding from scratch when it isn't.
func (st *notebookStore) load() (*Notebook, error) {
	events, err := st.readLog()
	if err != nil {
		return nil, err
	}

	if n := len(events); n > 0 {
		st.lastEventID = events[n-1].ID
	}
	if snap, ok := st.readSnapshot(); ok && snapshotMatchesLog(snap, events) {
		nb := snap.Notebook
		for _, e := range events[snap.Version:] {
			if err := applyEvent(nb, e); err != nil {
				// The snapshot led us somewhere inconsistent; the log is
				// the authority, so start over from it.
				return foldEvents(events)
			}
		}
		return nb, nil
	}
	return foldEvents(events)
}

// snapshotMatchesLog reports whether a snapshot can be trusted as the fold
// of this log's first Version events.
//
// The length check alone is not enough, and believing it is the bug this
// function exists to close: two histories can reach the same length while
// containing entirely different events. Comparing the last folded event's
// id makes divergence detectable — the ids are UUIDs, so two histories
// would have to collide on one at the same index.
func snapshotMatchesLog(snap notebookSnapshot, events []Event) bool {
	if snap.Notebook == nil || snap.Version < 0 || snap.Version > len(events) {
		return false
	}
	if snap.Version == 0 {
		return true // nothing folded in; the base is empty either way
	}
	if snap.LastEventID == "" {
		return false // predates the check, so unverifiable — re-fold instead
	}
	return events[snap.Version-1].ID == snap.LastEventID
}

// readLog parses every line. A trailing partial line (a crash mid-write) is
// dropped rather than failing the open — losing the last event beats losing
// the notebook.
func (st *notebookStore) readLog() ([]Event, error) {
	f, err := os.Open(st.logPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read notebook log: %w", err)
	}
	defer f.Close()

	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(line, &e); err != nil {
			// A line we cannot parse at all is almost certainly a torn
			// final write. Stop here and keep everything before it.
			break
		}
		events = append(events, e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan notebook log: %w", err)
	}
	return events, nil
}

func (st *notebookStore) readSnapshot() (notebookSnapshot, bool) {
	b, err := os.ReadFile(st.snapPath())
	if err != nil {
		return notebookSnapshot{}, false
	}
	var snap notebookSnapshot
	if err := json.Unmarshal(b, &snap); err != nil || snap.Notebook == nil {
		return notebookSnapshot{}, false // corrupt cache is a cache miss
	}
	return snap, true
}

// Append writes one event and folds it in. It is the only way the document
// changes: there is no path that mutates the notebook without a log line.
func (st *notebookStore) Append(typ string, payload any) (Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal %s payload: %w", typ, err)
	}
	e := Event{
		V:       nbSchemaVersion,
		Type:    typ,
		ID:      uuid.NewString(),
		At:      time.Now().UTC(),
		Payload: raw,
	}
	line, err := json.Marshal(e)
	if err != nil {
		return Event{}, fmt.Errorf("marshal %s event: %w", typ, err)
	}

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return Event{}, fmt.Errorf("notebook %s is closed", st.slug)
	}

	// Fold into a copy, write, then publish. Two failures have to be
	// impossible and this ordering rules out both: an event the fold
	// rejects must never reach the log, and an event the log rejects must
	// never reach the document. Folding in place would satisfy the first
	// and break the second — a failed write (ENOSPC, EIO, a log removed
	// underneath a still-open store) would leave Doc(), the HTTP read and
	// the WS fold all reporting a mutation that is not durable and that no
	// client ever saw.
	next := st.nb.clone()
	if err := applyEvent(next, e); err != nil {
		st.mu.Unlock()
		return Event{}, err
	}
	if _, err := st.log.Write(append(line, '\n')); err != nil {
		st.mu.Unlock()
		return Event{}, fmt.Errorf("append to notebook log: %w", err)
	}
	st.nb = next
	st.lastEventID = e.ID

	// The position this event was applied at — see broadcastEvent.
	seq := st.nb.Version

	st.sinceSnapshot++
	if st.sinceSnapshot >= nbSnapshotEvery {
		st.writeSnapshotLocked()
	}
	st.mu.Unlock()

	// Fan out after releasing mu: a slow subscriber must never hold up the
	// next append, and taking subMu under mu would couple the two locks.
	st.broadcastEvent(seq, e)
	return e, nil
}

// Doc returns a copy the caller can read while the store keeps folding.
func (st *notebookStore) Doc() *Notebook {
	st.mu.Lock()
	doc := st.nb.clone()
	st.mu.Unlock()
	// Derived, not folded (#47 P2). Filled here because this is the single
	// read path, so no caller can accidentally serve a document without it.
	doc.Fidelity = fidelityFor(doc)
	doc.Provider = providerInfoFor(doc)
	annotateCacheModes(doc)
	return doc
}

// Close flushes a final snapshot and releases the log handle.
func (st *notebookStore) Close() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return nil
	}
	st.closed = true
	st.writeSnapshotLocked()
	err := st.log.Close()
	st.log = nil
	return err
}

// writeSnapshotLocked rewrites the cache atomically. Failures are ignored on
// purpose: the snapshot is disposable, and refusing to continue because a
// cache write failed would take the notebook down with it.
func (st *notebookStore) writeSnapshotLocked() {
	b, err := json.Marshal(notebookSnapshot{
		Version:     st.nb.Version,
		Notebook:    st.nb,
		LastEventID: st.lastEventID,
	})
	if err != nil {
		return
	}
	tmp := st.snapPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, st.snapPath()); err != nil {
		_ = os.Remove(tmp)
		return
	}
	st.sinceSnapshot = 0
}

// ─── Subscribers ────────────────────────────────────────────────────────

// nbQueueDepth is the per-connection outbound queue. Deeper than the
// dashboard's because a running cell emits events in bursts; still bounded,
// so one stalled browser tab can't grow memory without limit.
const nbQueueDepth = 256

// addSub registers a websocket for this notebook's event stream. Reuses
// wsSub from session.go: it already drops for a client that has fallen
// behind rather than blocking the writer, which is the behaviour we want
// here too (a dropped event is recovered by reconnecting and re-folding).
func (st *notebookStore) addSub(c *websocket.Conn) *wsSub {
	sub := newWSSub(c, websocket.TextMessage, nbQueueDepth)
	st.subMu.Lock()
	if st.subs == nil {
		st.subs = map[*wsSub]bool{}
	}
	st.subs[sub] = true
	st.subMu.Unlock()
	return sub
}

func (st *notebookStore) removeSub(sub *wsSub) {
	st.subMu.Lock()
	delete(st.subs, sub)
	st.subMu.Unlock()
	sub.stop()
}

// broadcastEvent fans one event out to subscribers. Never called with
// st.mu held — subMu and mu are independent, in the same way session.go
// keeps s.subMu and s.mu apart.
//
// seq is the log position the event was applied at, and it is what makes
// the connect handshake safe: a client subscribes before it receives the
// fold, so an event racing that window can arrive first. Carrying the
// position lets the client drop anything already folded in (seq <=
// fold.version) instead of applying it twice.
func (st *notebookStore) broadcastEvent(seq int, e Event) {
	if msg, err := json.Marshal(map[string]any{"type": "event", "seq": seq, "event": e}); err == nil {
		st.sendAll(msg)
	}
}

// broadcastDelta streams a fragment of a running cell's output. Deltas are
// live-view only: they carry no sequence number because they are not log
// positions, and they are never persisted. A client that misses them
// recovers the finalised text from the output_appended event that follows.
func (st *notebookStore) broadcastDelta(cellID, runID, text string) {
	if text == "" {
		return
	}
	msg, err := json.Marshal(map[string]any{
		"type": "delta", "cellId": cellID, "runId": runID, "text": text,
	})
	if err != nil {
		return
	}
	st.sendAll(msg)
}

// ─── Live output ────────────────────────────────────────────────────────

// appendLive accumulates a running cell's output, capped so a runaway
// command costs a truncated cell rather than the process.
//
// Reports whether the text was accepted. Past the cap it returns false and
// the caller stops broadcasting too: a `yes` or a chatty build would
// otherwise keep pushing frames at every subscriber forever, held back only
// by the drop queue — that is, by corrupting the live view instead of
// ending it.
func (st *notebookStore) appendLive(cellID, runID, text string) bool {
	st.liveMu.Lock()
	defer st.liveMu.Unlock()
	if st.liveOut == nil {
		st.liveOut = map[string]*liveOutput{}
	}
	cur, ok := st.liveOut[cellID]
	if !ok || cur.RunID != runID {
		cur = &liveOutput{RunID: runID}
		st.liveOut[cellID] = cur
	}
	if cur.capped {
		return false
	}
	if room := maxCellOutput - len(cur.Text); len(text) >= room {
		if room > 0 {
			cur.Text += text[:room]
		}
		cur.Text += "\n… output truncated at 256 KiB …\n"
		cur.capped = true
		return true // the truncation notice itself is worth streaming
	}
	cur.Text += text
	return true
}

func (st *notebookStore) liveText(cellID, runID string) string {
	st.liveMu.Lock()
	defer st.liveMu.Unlock()
	if cur, ok := st.liveOut[cellID]; ok && cur.RunID == runID {
		return cur.Text
	}
	return ""
}

func (st *notebookStore) clearLive(cellID, runID string) {
	st.liveMu.Lock()
	if cur, ok := st.liveOut[cellID]; ok && cur.RunID == runID {
		delete(st.liveOut, cellID)
	}
	st.liveMu.Unlock()
}

// liveSnapshot is what the opening fold carries so a client joining
// mid-run sees what has already been produced.
func (st *notebookStore) liveSnapshot() map[string]liveOutput {
	st.liveMu.Lock()
	defer st.liveMu.Unlock()
	out := make(map[string]liveOutput, len(st.liveOut))
	for k, v := range st.liveOut {
		out[k] = *v
	}
	return out
}

func (st *notebookStore) sendAll(msg []byte) {
	st.subMu.Lock()
	subs := make([]*wsSub, 0, len(st.subs))
	for sub := range st.subs {
		subs = append(subs, sub)
	}
	st.subMu.Unlock()
	for _, sub := range subs {
		sub.send(msg)
	}
}

func (st *notebookStore) closeSubs() {
	st.subMu.Lock()
	subs := make([]*wsSub, 0, len(st.subs))
	for sub := range st.subs {
		subs = append(subs, sub)
	}
	st.subs = nil
	st.subMu.Unlock()
	for _, sub := range subs {
		sub.stop()
	}
}

// invalidateBelow marks every finished cell after cellID as stale. Cells
// that never ran have nothing to invalidate, and a running one is left
// alone rather than told it is out of date while it is still working.
func (st *notebookStore) invalidateBelow(cellID string) {
	doc := st.Doc()
	i := indexOfCell(doc, cellID)
	if i < 0 {
		return
	}
	var ids []string
	for _, c := range doc.Cells[i+1:] {
		if c.State == CellOK || c.State == CellError || c.State == CellInterrupted {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	if _, err := st.Append(evCellsInvalidated, cellsInvalidatedPayload{CellIDs: ids}); err != nil {
		logNotebookErr(st, cellID, "invalidate downstream cells", err)
	}
}

// ─── Registry ───────────────────────────────────────────────────────────

// One shared store per slug, so two browser tabs fold the same document
// rather than racing two handles onto one log.
//
// Lock ordering is nbRegistryMu -> st.mu, never the reverse — the same rule
// session.go documents for registryMu.
var (
	nbRegistryMu sync.Mutex
	nbRegistry   = map[string]*notebookStore{}
)

// nbDirFn is the notebooks-directory seam. Tests point it at a temp dir;
// the default anchors to the repo like the gh cache does.
var nbDirFn = defaultNotebooksDir

func defaultNotebooksDir() string {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return filepath.Join(repoRoot(cwd), ".collectif", "notebooks")
}

var errNotebookNotFound = errors.New("notebook not found")

// nbSlugRe bounds a slug to something that is unambiguously one path
// segment. The slug becomes a filename, so this is a containment check
// before it is a formatting preference: no separators, no dots, no escapes,
// no control bytes, bounded length.
var nbSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validNotebookSlug(slug string) bool { return nbSlugRe.MatchString(slug) }

// acquireNotebook returns the shared store for slug, opening it if it is
// not already open. Unknown notebooks are an error, not an empty notebook —
// a typo in a URL must not silently create a document.
func acquireNotebook(slug string) (*notebookStore, error) {
	if !validNotebookSlug(slug) {
		return nil, fmt.Errorf("invalid notebook id %q", slug)
	}
	nbRegistryMu.Lock()
	defer nbRegistryMu.Unlock()

	if st, ok := nbRegistry[slug]; ok && !st.isClosed() {
		return st, nil
	}
	dir := nbDirFn()
	if _, err := os.Stat(filepath.Join(dir, slug+".jsonl")); err != nil {
		return nil, errNotebookNotFound
	}
	st, err := openNotebookStore(dir, slug, "", "")
	if err != nil {
		return nil, err
	}
	nbRegistry[slug] = st
	return st, nil
}

// createNotebook makes a new notebook whose slug is derived from the title
// and made unique against what is already on disk.
func createNotebook(title, root string) (*notebookStore, error) {
	nbRegistryMu.Lock()
	defer nbRegistryMu.Unlock()

	dir := nbDirFn()
	slug, err := uniqueNotebookSlug(dir, title)
	if err != nil {
		return nil, err
	}
	st, err := openNotebookStore(dir, slug, title, root)
	if err != nil {
		return nil, err
	}
	nbRegistry[slug] = st
	return st, nil
}

// openNamedNotebook creates a notebook at a caller-chosen slug. Used for
// documents whose identity comes from something outside the notebook —
// today, the session a notebook mirrors (ADR 0002). Ordinary notebooks go
// through createNotebook, which derives a unique slug from the title.
func openNamedNotebook(slug, title, root string) (*notebookStore, error) {
	if !validNotebookSlug(slug) {
		return nil, fmt.Errorf("invalid notebook id %q", slug)
	}
	nbRegistryMu.Lock()
	defer nbRegistryMu.Unlock()

	if st, ok := nbRegistry[slug]; ok && !st.isClosed() {
		return st, nil
	}
	st, err := openNotebookStore(nbDirFn(), slug, title, root)
	if err != nil {
		return nil, err
	}
	nbRegistry[slug] = st
	return st, nil
}

func releaseNotebook(slug string) error {
	nbRegistryMu.Lock()
	st, ok := nbRegistry[slug]
	delete(nbRegistry, slug)
	nbRegistryMu.Unlock()
	if !ok {
		return nil
	}
	st.interruptAllRuns()
	st.closeSubs()
	return st.Close()
}

// closeAllNotebooks releases every open notebook. Used on shutdown, and by
// tests to keep package-level state from leaking between cases.
func closeAllNotebooks() {
	nbRegistryMu.Lock()
	stores := make([]*notebookStore, 0, len(nbRegistry))
	for _, st := range nbRegistry {
		stores = append(stores, st)
	}
	nbRegistry = map[string]*notebookStore{}
	nbRegistryMu.Unlock()
	for _, st := range stores {
		// Stop in-flight commands before releasing the log, so a shutdown
		// doesn't leave orphaned process groups behind.
		st.interruptAllRuns()
		st.closeSubs()
		_ = st.Close()
	}
}

func (st *notebookStore) isClosed() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.closed
}

// slugifyTitle reduces a title to the slug charset. Anything outside it
// becomes a separator, so a title in any script still yields a usable
// filename rather than being rejected.
func slugifyTitle(title string) string {
	var b strings.Builder
	lastDash := true // leading dashes are dropped
	for _, r := range strings.ToLower(title) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '_':
			b.WriteRune('_')
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	if !validNotebookSlug(s) {
		return "notebook"
	}
	return s
}

func uniqueNotebookSlug(dir, title string) (string, error) {
	base := slugifyTitle(title)
	for i := 0; i < 1000; i++ {
		slug := base
		if i > 0 {
			slug = fmt.Sprintf("%s-%d", base, i+1)
		}
		if !validNotebookSlug(slug) {
			return "", fmt.Errorf("could not derive a valid id from title %q", title)
		}
		if _, inMem := nbRegistry[slug]; inMem {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, slug+".jsonl")); os.IsNotExist(err) {
			return slug, nil
		}
	}
	return "", fmt.Errorf("could not find a free id for title %q", title)
}

// notebookSummary is the list-view shape: enough to render a launcher
// without folding every notebook on disk.
type notebookSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Root      string    `json:"root"`
	Cells     int       `json:"cells"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// peekNotebook reads a notebook's summary without opening it.
//
// Listing must not go through acquireNotebook: that registers the store and
// holds its log open, so rendering a launcher would pin every notebook on
// disk for the life of the process. An already-open notebook is read from
// the registry (authoritative and free); anything else is folded from a
// read-only probe that owns no handle and joins no registry.
func peekNotebook(dir, slug string) (notebookSummary, bool) {
	nbRegistryMu.Lock()
	st, open := nbRegistry[slug]
	nbRegistryMu.Unlock()
	if open && !st.isClosed() {
		nb := st.Doc()
		return notebookSummary{ID: slug, Title: nb.Title, Root: nb.Root, Cells: len(nb.Cells)}, true
	}

	probe := &notebookStore{dir: dir, slug: slug}
	nb, err := probe.load() // snapshot + remaining events, same as a real open
	if err != nil || nb == nil {
		return notebookSummary{}, false
	}
	return notebookSummary{ID: slug, Title: nb.Title, Root: nb.Root, Cells: len(nb.Cells)}, true
}

// listNotebooks enumerates the notebooks directory.
func listNotebooks() ([]notebookSummary, error) {
	dir := nbDirFn()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []notebookSummary{}, nil
		}
		return nil, err
	}
	out := []notebookSummary{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		slug := strings.TrimSuffix(e.Name(), ".jsonl")
		if !validNotebookSlug(slug) {
			continue
		}
		s, ok := peekNotebook(dir, slug)
		if !ok {
			continue // one unreadable notebook shouldn't blank the whole list
		}
		if info, err := e.Info(); err == nil {
			s.UpdatedAt = info.ModTime().UTC()
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}
