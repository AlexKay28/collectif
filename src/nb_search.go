package main

// nb_search.go — search across every notebook on the machine. #58, per the
// sketch in that issue and ADR 0002.
//
// Layout, extending the one nb_store.go established:
//
//	<dir>/<slug>.jsonl        append-only, the truth
//	<dir>/<slug>.snap.json    derived cache of the folded document
//	<dir>/<slug>.index.json   derived cache of what that document says
//
// ─── Why an inverted index and not SQLite FTS ───────────────────────────
//
// FTS5 would be collectif's first non-stdlib storage dependency, and it
// would be paid for a corpus that is ten megabytes: every notebook on this
// machine put together is smaller than one of the transcripts they were
// projected from. At that size the whole index fits in memory with room to
// spare, so the thing a database buys — not holding the corpus — is not a
// thing we need. What it costs is a schema to migrate, a file format we do
// not control, and cgo or a pure-Go reimplementation of it.
//
// The second reason is stronger than the size argument and would hold at a
// hundred times the corpus. This index is *disposable*: it is a summary of
// logs that are themselves the record, so the correct response to any doubt
// about it is to delete it and fold the logs again. A database earns its
// keep by being durable and transactional, and we would be buying
// durability for data whose defining property is that losing it costs a
// second of CPU.
//
// What FTS would genuinely give us — ranking, stemming, phrase queries — is
// not what this surface is for. The queries the issue names are structural:
// "every time an agent ran git push", "what did I ask about the projection
// format". They are answered by *which part of a cell* matched, not by how
// well it matched, and that axis is ours rather than the text engine's.
//
// ─── The rule this cache is subject to ──────────────────────────────────
//
// #47 P2 shipped a snapshot cache that trusted any file whose version was
// plausible, and it served a document from a history that no longer
// existed: a real notebook lost its whole Meta block and reported no
// session while its log plainly recorded one. See notebookSnapshot.
// LastEventID in nb_store.go.
//
// So the index carries the same proof, and refuses itself when it cannot
// produce it. Two facts about the log are recorded when it is built — the
// log's size in bytes, and the id of the last event folded into the
// document it summarises — and both are re-checked before any query is
// answered from it. Neither is expensive: the size is a stat, and the event
// id is one seek to the end of the file.
//
// The ordering of those two reads is load-bearing and is the one thing in
// here that is easy to get subtly wrong. The stamp is always taken *before*
// the document is read, never after. An index stamped at a position ahead
// of its own content passes verification while missing the newest turns —
// silent staleness, the exact failure above. An index stamped behind its
// content merely fails its next verification and is rebuilt, which costs a
// fold and lies about nothing.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	searchIndexSuffix = ".index.json"

	// searchIndexVersion is the shape of the file on disk. An index written
	// by another build is discarded rather than adapted, on the snapshot's
	// reasoning: unverifiable is thrown away, because re-folding is cheap
	// and a wrong answer is not.
	searchIndexVersion = 1

	// maxSearchExcerpt bounds what the index keeps of each block. The index
	// is a finding aid and the log beside it is the archive; storing every
	// tool result in full would make the cache cost as much as the corpus
	// it summarises, for the sake of a snippet. Matching still runs against
	// the whole block — only the rendered excerpt is clipped.
	//
	// Measured over the ten real notebooks on this machine (5.1 MB of
	// logs): 512 bytes gives a 2.6 MB index, 1024 gives 3.1 MB, 2048 gives
	// 3.8 MB. A kilobyte is four snippet windows, which is enough for a
	// match near the top of a block to be shown in context, and it keeps
	// the cache well under the size of what it summarises.
	maxSearchExcerpt = 1024

	// maxTokenizedText bounds what is tokenised per block. A quarter-
	// megabyte of `yes` output has nothing findable in it past the first
	// page, and indexing it in full would let one runaway command dominate
	// the index for a whole notebook.
	maxTokenizedText = 32 * 1024

	// maxSearchPrompt bounds the turn's prompt carried on every hit. It is
	// context for a result row, not the cell itself — clicking the row
	// opens the document.
	maxSearchPrompt = 300

	snippetWindow      = 220
	defaultSearchLimit = 60
	maxSearchLimit     = 500

	// maxHitsPerNotebook stops one chatty session from filling the page.
	// Without it a query that matches a thousand blocks in the notebook
	// that happens to sort first never shows the other nine notebooks at
	// all, which reads as "it is only in this one".
	maxHitsPerNotebook = 20
)

// The axis that makes searching a notebook different from grepping a
// transcript: a cell separates what a human authored from what the agent
// produced from what it ran. A query that cannot name those apart is no
// better than scrollback (ADR 0002 §3).
const (
	searchKindPrompt = "prompt" // a prompt cell's source — the question
	searchKindNote   = "note"   // markdown or shell source — authored, but not a question
	searchKindOutput = "output" // the agent's prose, thinking, errors
	searchKindTool   = "tool"   // a tool call, its result, or a request to run one
	// searchKindInjection is context the harness put in front of the model
	// that nobody typed (#47). Its own kind rather than "output" because
	// the projector went to some trouble to separate it, and folding it
	// back in would bury real turns under machinery.
	searchKindInjection = "injection"
)

func validSearchKind(k string) bool {
	switch k {
	case searchKindPrompt, searchKindNote, searchKindOutput, searchKindTool, searchKindInjection:
		return true
	}
	return false
}

// ─── The index ──────────────────────────────────────────────────────────

// searchCell is the turn a match sits in. Held once per cell rather than
// copied onto every block, because a single turn routinely carries fifty
// outputs and its prompt is the same for all of them.
type searchCell struct {
	ID     string    `json:"id"`
	Index  int       `json:"index"` // 1-based position in the document
	Type   string    `json:"type"`
	State  string    `json:"state"`
	Prompt string    `json:"prompt,omitempty"`
	At     time.Time `json:"at,omitempty"`
}

// searchDoc is one searchable block: a cell's source, or one of its
// outputs.
type searchDoc struct {
	Cell   int32  `json:"cell"` // into searchIndex.Cells
	Kind   string `json:"kind"`
	OutIdx int    `json:"out"` // -1 for the cell's own source
	// Agent names the subagent that produced this block (#55a). Empty for
	// the main agent's own work. It is what lets a match inside a child be
	// reported with the parent prompt that caused it.
	Agent   string `json:"agent,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Excerpt string `json:"excerpt,omitempty"`
}

type searchIndex struct {
	V         int    `json:"v"`
	Slug      string `json:"slug"`
	Title     string `json:"title,omitempty"`
	Root      string `json:"root,omitempty"`
	CLI       string `json:"cli,omitempty"`
	SessionID string `json:"sessionId,omitempty"`

	// LastEventID and LogSize are the proof that this index summarises the
	// log beside it and not some other history — see the file header.
	LastEventID string    `json:"lastEventId"`
	LogSize     int64     `json:"logSize"`
	Built       time.Time `json:"built"`

	Cells    []searchCell       `json:"cells"`
	Docs     []searchDoc        `json:"docs"`
	Postings map[string][]int32 `json:"postings"`
}

// logStamp is what has to hold for an index to still be believed.
type logStamp struct {
	Size        int64
	ModTime     time.Time
	LastEventID string
}

// matchesLog is the whole of the trust decision, and it fails closed:
// anything missing, unreadable or unexplained means "rebuild".
func (ix *searchIndex) matchesLog(cur logStamp) bool {
	if ix == nil || ix.V != searchIndexVersion {
		return false
	}
	if ix.LastEventID == "" || cur.LastEventID == "" {
		return false
	}
	return ix.LastEventID == cur.LastEventID && ix.LogSize == cur.Size
}

// ─── Reading the log's stamp ────────────────────────────────────────────

// readLogStamp gets the two facts an index is verified against without
// reading the log. The last event's id is the identity check; the size is
// the cheap one that catches the ordinary case of a session still running.
//
// A log whose final line is a torn write returns no id, so its index fails
// verification and is rebuilt on every query. That is the right way round:
// readLog drops a torn final line, so an index built from it would be
// stamped with an event the document does not contain.
func readLogStamp(path string) (logStamp, error) {
	f, err := os.Open(path)
	if err != nil {
		return logStamp{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return logStamp{}, err
	}
	st := logStamp{Size: fi.Size(), ModTime: fi.ModTime().UTC()}
	if st.Size == 0 {
		return st, nil
	}

	line, ok := lastLine(f, st.Size)
	if !ok {
		return st, nil // no id, so nothing will be trusted against this log
	}
	var e struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return st, nil
	}
	st.LastEventID = e.ID
	return st, nil
}

// lastLine reads back from the end of a file until it has a whole line.
//
// The window grows rather than being fixed, because one event line is not
// boundedly small: a cell's output is capped at 256 KiB, so a 64 KiB read
// off the end can easily land in the middle of the last event. A fixed
// window would not be wrong — an unknown id means "rebuild" — but it would
// mean rebuilding on every query for exactly the notebooks where rebuilding
// costs the most.
func lastLine(f *os.File, size int64) (string, bool) {
	const (
		firstWindow = 64 * 1024
		maxWindow   = 8 * 1024 * 1024
	)
	for window := int64(firstWindow); ; window *= 8 {
		start := size - window
		if start < 0 {
			start = 0
		}
		buf := make([]byte, size-start)
		if _, err := f.ReadAt(buf, start); err != nil {
			return "", false
		}
		tail := strings.TrimRight(string(buf), "\n")
		if i := strings.LastIndexByte(tail, '\n'); i >= 0 {
			return tail[i+1:], true
		}
		if start == 0 {
			return tail, true // the whole file is one line
		}
		if window >= maxWindow {
			return "", false
		}
	}
}

// ─── Building ───────────────────────────────────────────────────────────

// notebookForIndex folds a notebook and returns the stamp its index must
// carry.
//
// The stamp is taken before the document, always — see the file header for
// what the other order costs. An open notebook is read from the registry
// because that store owns the log and its in-memory fold is authoritative;
// anything else is folded through a read-only probe that owns no handle and
// joins no registry, exactly as peekNotebook does, so searching cannot pin
// every notebook on disk open for the life of the process.
func notebookForIndex(dir, slug string) (*Notebook, logStamp, bool) {
	path := filepath.Join(dir, slug+".jsonl")
	stamp, err := readLogStamp(path)
	if err != nil {
		return nil, logStamp{}, false
	}

	nbRegistryMu.Lock()
	st, open := nbRegistry[slug]
	nbRegistryMu.Unlock()
	if open && !st.isClosed() {
		doc, lastID := st.docAndLastEvent()
		stamp.LastEventID = lastID
		return doc, stamp, doc != nil
	}

	probe := &notebookStore{dir: dir, slug: slug}
	nb, err := probe.load() // snapshot fast path included, and it is verified
	if err != nil || nb == nil {
		return nil, logStamp{}, false
	}
	// load() records the id of the last event it actually parsed, which is
	// the only id consistent with the document it just produced.
	if probe.lastEventID != "" {
		stamp.LastEventID = probe.lastEventID
	}
	return nb, stamp, true
}

// buildSearchIndex turns a folded notebook into the index of it.
func buildSearchIndex(slug string, nb *Notebook, stamp logStamp) *searchIndex {
	ix := &searchIndex{
		V:           searchIndexVersion,
		Slug:        slug,
		Title:       nb.Title,
		Root:        nb.Root,
		CLI:         nb.Meta.CLI,
		SessionID:   nb.Meta.SessionID,
		LastEventID: stamp.LastEventID,
		LogSize:     stamp.Size,
		Built:       time.Now().UTC(),
		Postings:    map[string][]int32{},
	}
	for i := range nb.Cells {
		c := &nb.Cells[i]
		ref := int32(len(ix.Cells))
		ix.Cells = append(ix.Cells, searchCell{
			ID:     c.ID,
			Index:  i + 1,
			Type:   string(c.Type),
			State:  string(c.State),
			Prompt: clipText(c.Source, maxSearchPrompt),
			At:     cellTime(c, stamp.ModTime),
		})
		if strings.TrimSpace(c.Source) != "" {
			ix.add(ref, sourceKind(c.Type), -1, "", "", c.Source)
		}
		for oi := range c.Outputs {
			o := &c.Outputs[oi]
			kind, text, tool := searchableOutput(o)
			if kind == "" || strings.TrimSpace(text) == "" {
				continue
			}
			ix.add(ref, kind, oi, dataString(o.Data, "agentId"), tool, text)
		}
	}
	return ix
}

// cellTime is when the turn happened. CreatedAt comes from the log's own
// envelope, which is the only honest answer; Started covers cells folded
// before that field existed and still cached in a snapshot.
//
// The last fallback is the log's mtime, which is an upper bound on every
// cell in it. That over-includes an old cell in a recently written notebook
// rather than hiding it, and hiding is the worse failure in a search:
// seeing one row too many is a nuisance, and missing the row you came for
// looks like the thing never happened.
func cellTime(c *Cell, fallback time.Time) time.Time {
	if !c.CreatedAt.IsZero() {
		return c.CreatedAt.UTC()
	}
	if !c.Started.IsZero() {
		return c.Started.UTC()
	}
	return fallback
}

// sourceKind separates the question from the rest of what a human wrote. A
// prompt cell is what was asked; a markdown or shell cell is a note or a
// command, authored just as deliberately but not a question to the agent.
func sourceKind(t CellType) string {
	if t == CellPrompt {
		return searchKindPrompt
	}
	return searchKindNote
}

// searchableOutput says which axis a produced block belongs on and what
// text stands for it.
//
// An approval is filed with tool calls rather than given its own kind: it
// is a question *about* a tool call, carrying that tool's name and
// arguments, and someone looking for "every time it wanted to run git push"
// is looking for the same thing whether it ran or was asked about. The
// verdict half of the record carries no text and indexes to nothing.
func searchableOutput(o *Output) (kind, text, tool string) {
	switch o.Type {
	case OutputText, OutputThinking, OutputError, OutputDiff:
		return searchKindOutput, o.Text, ""
	case OutputToolCall:
		name := dataString(o.Data, "name")
		return searchKindTool, strings.TrimSpace(name + " " + flattenJSON(o.Data["input"], 0)), name
	case OutputToolResult:
		return searchKindTool, o.Text, dataString(o.Data, "name")
	case OutputApproval:
		name := dataString(o.Data, "tool")
		return searchKindTool,
			strings.TrimSpace(name + " " + o.Text + " " + flattenJSON(o.Data["input"], 0)), name
	case OutputInjection:
		return searchKindInjection, strings.TrimSpace(dataString(o.Data, "label") + " " + o.Text), ""
	}
	return "", "", ""
}

func (ix *searchIndex) add(cell int32, kind string, outIdx int, agent, tool, text string) {
	id := int32(len(ix.Docs))
	ix.Docs = append(ix.Docs, searchDoc{
		Cell: cell, Kind: kind, OutIdx: outIdx,
		Agent: agent, Tool: tool, Excerpt: clipText(text, maxSearchExcerpt),
	})
	for _, tok := range indexTokens(text) {
		// Postings stay sorted for free: documents are added in order, so
		// an append is always the largest id in the list. The intersection
		// below relies on that.
		p := ix.Postings[tok]
		if n := len(p); n > 0 && p[n-1] == id {
			continue
		}
		ix.Postings[tok] = append(p, id)
	}
}

// ─── Tokenising ─────────────────────────────────────────────────────────

// indexTokens produces the keys a block is findable by.
//
// Identifiers are the reason this is not just "split on spaces". Half of
// what is worth finding in these documents is code — snapshotMatchesLog,
// nb_store.go, CLAUDE_CODE_CHILD_SESSION — and nobody searches for
// `snapshotmatcheslog`. So a word is indexed whole *and* by its camel- and
// underscore-separated parts, which makes it reachable by any of them
// without the query side needing to guess how it was spelled.
func indexTokens(text string) []string {
	if len(text) > maxTokenizedText {
		text = text[:maxTokenizedText]
	}
	var out []string
	seen := map[string]bool{}
	emit := func(s string) {
		if len(s) < 2 || len(s) > 64 {
			return
		}
		s = strings.ToLower(s)
		if seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, word := range splitWords(text) {
		emit(word)
		if parts := splitIdentifier(word); len(parts) > 1 {
			for _, p := range parts {
				emit(p)
			}
		}
	}
	return out
}

// queryTerms tokenises what the user typed. Deliberately *not* the same
// function: the index splits identifiers so they can be found by their
// parts, while a query for `snapshotMatchesLog` means that identifier and
// must not decay into an AND over three common words.
func queryTerms(q string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range splitWords(q) {
		w = strings.ToLower(w)
		if len(w) < 2 || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

func splitWords(s string) []string {
	var out []string
	start := -1
	for i, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// splitIdentifier breaks camelCase and PascalCase runs. HTTPServer is two
// words, not one plus seven letters.
func splitIdentifier(w string) []string {
	rs := []rune(w)
	var out []string
	start := 0
	for i := 1; i < len(rs); i++ {
		boundary := unicode.IsUpper(rs[i]) && !unicode.IsUpper(rs[i-1])
		if !boundary && i+1 < len(rs) {
			boundary = unicode.IsUpper(rs[i-1]) && unicode.IsUpper(rs[i]) && unicode.IsLower(rs[i+1])
		}
		if boundary {
			out = append(out, string(rs[start:i]))
			start = i
		}
	}
	out = append(out, string(rs[start:]))
	return out
}

// ─── The on-disk cache ──────────────────────────────────────────────────

// One index per process per log, keyed by the log's path so a test's temp
// directory can never be answered from another's. Held in memory because
// the file is the slow half: a query that re-reads and re-parses every
// index on disk has bought about half of what folding the logs costs, which
// is not worth a cache.
var (
	searchCacheMu sync.Mutex
	searchCache   = map[string]*searchIndex{}
)

func cachedSearchIndex(path string) *searchIndex {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()
	return searchCache[path]
}

func putSearchIndex(path string, ix *searchIndex) {
	searchCacheMu.Lock()
	searchCache[path] = ix
	searchCacheMu.Unlock()
}

func searchIndexPath(dir, slug string) string {
	return filepath.Join(dir, slug+searchIndexSuffix)
}

// searchIndexFor returns an index that has proved it belongs to this log,
// rebuilding it when it cannot. Returns nil for a notebook that cannot be
// read at all: one broken document should cost its own rows, not the query.
func searchIndexFor(dir, slug string) *searchIndex {
	logPath := filepath.Join(dir, slug+".jsonl")
	cur, err := readLogStamp(logPath)
	if err != nil {
		return nil
	}
	if ix := cachedSearchIndex(logPath); ix.matchesLog(cur) {
		return ix
	}
	if ix := loadSearchIndexFile(dir, slug); ix.matchesLog(cur) {
		putSearchIndex(logPath, ix)
		return ix
	}

	nb, stamp, ok := notebookForIndex(dir, slug)
	if !ok {
		return nil
	}
	ix := buildSearchIndex(slug, nb, stamp)
	saveSearchIndex(dir, slug, ix)
	putSearchIndex(logPath, ix)
	return ix
}

func loadSearchIndexFile(dir, slug string) *searchIndex {
	b, err := os.ReadFile(searchIndexPath(dir, slug))
	if err != nil {
		return nil
	}
	var ix searchIndex
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil // a corrupt cache is a cache miss, as the snapshot's is
	}
	return &ix
}

// saveSearchIndex rewrites the cache atomically. Failures are ignored on
// purpose, on writeSnapshotLocked's reasoning: the index is disposable, and
// refusing to answer a query because a cache write failed would take the
// feature down with the disk.
//
// The temporary file gets a unique name rather than a fixed .tmp suffix,
// because two queries can rebuild one notebook at once — two browser tabs
// are enough — and a shared scratch file would let them interleave into a
// half-and-half index that the next reader has to throw away.
func saveSearchIndex(dir, slug string, ix *searchIndex) {
	b, err := json.Marshal(ix)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(dir, slug+".index-*.tmp")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, searchIndexPath(dir, slug)); err != nil {
		_ = os.Remove(tmp)
	}
}

// forgetSearchIndex drops a notebook's index from memory. Called when a
// notebook is deleted, so its rows cannot outlive the document.
func forgetSearchIndex(dir, slug string) {
	searchCacheMu.Lock()
	delete(searchCache, filepath.Join(dir, slug+".jsonl"))
	searchCacheMu.Unlock()
}

// ─── Querying ───────────────────────────────────────────────────────────

type searchQuery struct {
	Text  string
	Kinds []string
	CLI   string
	Since time.Time
	Limit int
}

type searchHit struct {
	CellID    string `json:"cellId"`
	CellIndex int    `json:"cellIndex"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	// Prompt is the turn's own question, carried on every hit — including
	// hits inside a subagent, which is the point of nesting them (#55a).
	Prompt string `json:"prompt,omitempty"`
	Tool   string `json:"tool,omitempty"`
	// AgentID is set when the match is inside delegated work rather than
	// the main agent's own.
	AgentID string `json:"agentId,omitempty"`
	// Matches is how many blocks inside this turn matched. The row stands
	// for the turn, so this is where the rest of them go.
	Matches int `json:"matches,omitempty"`
	// OutputIndex is where in the cell the match sits: -1 for the cell's
	// own source, otherwise the position of the output block.
	OutputIndex int       `json:"outputIndex"`
	Snippet     string    `json:"snippet"`
	At          time.Time `json:"at,omitempty"`
}

type searchGroup struct {
	Notebook  string `json:"notebook"`
	Title     string `json:"title,omitempty"`
	Root      string `json:"root,omitempty"`
	CLI       string `json:"cli,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	// Total is how many turns in this notebook matched, which is not
	// len(Hits): the list is capped so one busy session cannot fill a page
	// that has nine other notebooks to show.
	Total int         `json:"total"`
	Hits  []searchHit `json:"hits"`
}

type searchResults struct {
	Query     string        `json:"q"`
	Count     int           `json:"count"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated"`
	Groups    []searchGroup `json:"groups"`
}

// searchNotebooks answers one query across every notebook in the notebooks
// directory, newest first.
func searchNotebooks(q searchQuery) (searchResults, error) {
	res := searchResults{Query: q.Text, Groups: []searchGroup{}}
	terms := queryTerms(q.Text)
	if len(terms) == 0 {
		return res, nil
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	kinds := map[string]bool{}
	for _, k := range q.Kinds {
		if validSearchKind(k) {
			kinds[k] = true
		}
	}

	dir := nbDirFn()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, err
	}

	// Most recently written first, matching listNotebooks: what you were
	// doing today is what you are usually looking for.
	type candidate struct {
		slug string
		mod  time.Time
	}
	var cands []candidate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		slug := strings.TrimSuffix(e.Name(), ".jsonl")
		if !validNotebookSlug(slug) {
			continue
		}
		c := candidate{slug: slug}
		if info, err := e.Info(); err == nil {
			c.mod = info.ModTime()
		}
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })

	for _, c := range cands {
		ix := searchIndexFor(dir, c.slug)
		if ix == nil {
			continue // one unreadable notebook must not blank the query
		}
		if q.CLI != "" && !strings.EqualFold(ix.CLI, q.CLI) {
			continue
		}
		room := limit - res.Count
		if room > maxHitsPerNotebook {
			room = maxHitsPerNotebook
		}
		if room < 0 {
			room = 0
		}
		hits, total := ix.hits(terms, kinds, q.Since, room)
		if total == 0 {
			continue
		}
		res.Total += total
		if len(hits) > 0 {
			res.Count += len(hits)
			res.Groups = append(res.Groups, searchGroup{
				Notebook: ix.Slug, Title: ix.Title, Root: ix.Root,
				CLI: ix.CLI, SessionID: ix.SessionID,
				Total: total, Hits: hits,
			})
		}
	}
	res.Truncated = res.Total > res.Count
	return res, nil
}

// hits runs one query against one index and returns at most room rows, plus
// the number of turns that matched — so a cut result set can say so rather
// than letting the reader believe they saw everything.
//
// One row per *turn*, not per matching block. Measured against the real
// notebooks on this machine, a single "read the repo" turn carries a
// hundred tool results and matched a hundred times: a result list is a list
// of turns you might want to open, and the same turn twenty times over is
// the twenty-first way of saying one thing. How many blocks inside it
// matched is kept on the row, because that is the interesting part of the
// duplication.
func (ix *searchIndex) hits(terms []string, kinds map[string]bool, since time.Time, room int) ([]searchHit, int) {
	type turn struct {
		cell    int32
		best    int32
		score   int
		matches int
	}
	byCell := map[int32]*turn{}
	var turns []*turn

	for _, id := range ix.matching(terms) {
		d := &ix.Docs[id]
		if len(kinds) > 0 && !kinds[d.Kind] {
			continue
		}
		if d.Cell < 0 || int(d.Cell) >= len(ix.Cells) {
			continue // an index this malformed is not worth a panic
		}
		cell := &ix.Cells[d.Cell]
		if !since.IsZero() && !cell.At.IsZero() && cell.At.Before(since) {
			continue
		}
		t := byCell[d.Cell]
		if t == nil {
			t = &turn{cell: d.Cell, best: id, score: -1}
			byCell[d.Cell] = t
			turns = append(turns, t)
		}
		t.matches++
		if s := scoreDoc(d, terms); s > t.score {
			t.score, t.best = s, id
		}
	}
	if len(turns) == 0 {
		return nil, 0
	}
	sort.SliceStable(turns, func(i, j int) bool {
		if turns[i].score != turns[j].score {
			return turns[i].score > turns[j].score
		}
		// Later in the document first: within one session the turn you are
		// looking for is usually the recent one.
		return ix.Cells[turns[i].cell].Index > ix.Cells[turns[j].cell].Index
	})

	out := make([]searchHit, 0, room)
	for _, t := range turns {
		if room <= 0 || len(out) >= room {
			break
		}
		d, cell := &ix.Docs[t.best], &ix.Cells[t.cell]
		out = append(out, searchHit{
			CellID: cell.ID, CellIndex: cell.Index, Kind: d.Kind, State: cell.State,
			Prompt: cell.Prompt, Tool: d.Tool, AgentID: d.Agent, Matches: t.matches,
			OutputIndex: d.OutIdx, Snippet: snippetAround(d.Excerpt, terms), At: cell.At,
		})
	}
	return out, len(turns)
}

// scoreDoc decides which block stands for a turn, and which turns lead.
//
// Two things earn a place, and both were learned from running this against
// the real corpus rather than from a fixture. A match you can *see* beats
// one taken on faith: matching runs on the whole block while the stored
// excerpt is clipped, so a hit deep inside a 200 KB file read renders as
// the top of that file and explains nothing. And a question beats a
// directory listing that happens to contain the words — `git push` matched
// dozens of `ls -la` results through `.git` and `.gitignore` before this
// existed, and the actual `git push` was somewhere below them.
//
// It is a tie-break, not a relevance engine. Nothing is hidden by it; the
// order of what is shown first is all it decides.
func scoreDoc(d *searchDoc, terms []string) int {
	score := 0
	switch d.Kind {
	case searchKindPrompt:
		score = 4
	case searchKindNote, searchKindOutput:
		score = 3
	case searchKindTool:
		score = 1
	}
	lower := strings.ToLower(d.Excerpt)
	shown := 0
	for _, t := range terms {
		if strings.Contains(lower, t) {
			shown++
		}
	}
	switch {
	case shown == len(terms):
		score += 6
	case shown > 0:
		score += 3
	}
	return score
}

// matching intersects the posting lists, rarest term first so the lists
// shrink as early as possible. Every term must match: a two-word query that
// ORs is a query whose second word does nothing.
func (ix *searchIndex) matching(terms []string) []int32 {
	lists := make([][]int32, 0, len(terms))
	for _, t := range terms {
		p := ix.Postings[t]
		if len(p) == 0 {
			return nil
		}
		lists = append(lists, p)
	}
	sort.Slice(lists, func(i, j int) bool { return len(lists[i]) < len(lists[j]) })

	acc := lists[0]
	for _, l := range lists[1:] {
		acc = intersectSorted(acc, l)
		if len(acc) == 0 {
			return nil
		}
	}
	return acc
}

func intersectSorted(a, b []int32) []int32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]int32, 0, n)
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return out
}

// snippetAround shows the match in its context. When the term is past the
// stored excerpt — a match deep inside a long tool result, which is found
// because matching runs on the whole block — the head of the block is shown
// instead. Showing the beginning of the right block beats showing nothing.
func snippetAround(text string, terms []string) string {
	if text == "" {
		return ""
	}
	lower := strings.ToLower(text)
	at := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}
	if at < 0 {
		return clipText(text, snippetWindow)
	}
	start := at - snippetWindow/3
	if start < 0 {
		start = 0
	}
	for start > 0 && !isBoundary(text[start-1]) {
		start--
	}
	end := start + snippetWindow
	if end > len(text) {
		end = len(text)
	}
	s := strings.TrimSpace(text[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}

func isBoundary(b byte) bool {
	return b == ' ' || b == '\n' || b == '\t'
}

// ─── Small helpers ──────────────────────────────────────────────────────

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary so a clipped excerpt is still valid UTF-8 and
	// still JSON.
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func dataString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// flattenJSON renders a decoded tool input as the text it contains. A tool
// call's arguments are the searchable part of it — `git push` lives in a
// command field, not in the tool's name — and the keys are worth keeping
// too, because "file_path" is how someone finds edits to a path.
func flattenJSON(v any, depth int) string {
	if depth > 6 {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%g", t)
	case bool:
		return fmt.Sprintf("%t", t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := flattenJSON(e, depth+1); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic, so two builds of one log agree
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if s := flattenJSON(t[k], depth+1); s != "" {
				parts = append(parts, k+" "+s)
			}
		}
		return strings.Join(parts, " ")
	}
	return ""
}
