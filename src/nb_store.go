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
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
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
	closed        bool
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

	if snap, ok := st.readSnapshot(); ok && snap.Version <= len(events) {
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
	defer st.mu.Unlock()
	if st.closed {
		return Event{}, fmt.Errorf("notebook %s is closed", st.slug)
	}

	// Fold before writing so a rejected event never reaches the log.
	if err := applyEvent(st.nb, e); err != nil {
		return Event{}, err
	}
	if _, err := st.log.Write(append(line, '\n')); err != nil {
		return Event{}, fmt.Errorf("append to notebook log: %w", err)
	}

	st.sinceSnapshot++
	if st.sinceSnapshot >= nbSnapshotEvery {
		st.writeSnapshotLocked()
	}
	return e, nil
}

// Doc returns a copy the caller can read while the store keeps folding.
func (st *notebookStore) Doc() *Notebook {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.nb.clone()
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
	b, err := json.Marshal(notebookSnapshot{Version: st.nb.Version, Notebook: st.nb})
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

// The per-notebook registry (one shared store per slug, so two browser tabs
// fold the same document rather than racing two handles onto one log) lands
// with the HTTP and WS layer that consumes it. Nothing in this slice opens a
// notebook concurrently, and a registry with no caller is a registry with no
// test.
