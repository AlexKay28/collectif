package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// #58 — search across notebooks.
//
// The staleness cases come first and are the longest, because they are the
// only ones that can fail silently. A tokenizer bug returns the wrong rows
// and you see it; an index that cannot prove it belongs to its log returns
// rows from a history that no longer exists, and looks exactly like a
// correct answer. That is the bug #47 P2 shipped with the snapshot cache,
// and it cost a real notebook its entire Meta block.

// ─── Fixtures ───────────────────────────────────────────────────────────

// seedNotebook writes a notebook whose events all carry the current time.
func seedNotebook(t *testing.T, slug string, meta NotebookMeta, cells []Cell) *notebookStore {
	t.Helper()
	st, err := openNamedNotebook(slug, slug, t.TempDir())
	if err != nil {
		t.Fatalf("open %s: %v", slug, err)
	}
	if meta != (NotebookMeta{}) {
		if _, err := st.Append(evMetaSet, metaSetPayload{Meta: &meta}); err != nil {
			t.Fatalf("meta: %v", err)
		}
	}
	for _, c := range cells {
		outs := c.Outputs
		c.Outputs = nil
		if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: c}); err != nil {
			t.Fatalf("insert: %v", err)
		}
		for _, o := range outs {
			if _, err := st.Append(evOutputAppended, outputAppendedPayload{CellID: c.ID, Output: o}); err != nil {
				t.Fatalf("output: %v", err)
			}
		}
	}
	return st
}

// logStampOnDisk reads what a *correct* index would have to record about a
// log: its size, and the id of its last event.
func logStampOnDisk(t *testing.T, dir, slug string) (int64, string) {
	t.Helper()
	path := filepath.Join(dir, slug+".jsonl")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat log: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var e Event
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatalf("decode last event: %v", err)
	}
	return fi.Size(), e.ID
}

// writeIndexFile plants an index on disk, the way a previous process would
// have left one behind.
func writeIndexFile(t *testing.T, dir, slug string, ix *searchIndex) {
	t.Helper()
	b, err := json.Marshal(ix)
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+searchIndexSuffix), b, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// plantedIndex is an index carrying one made-up document, which is how
// every staleness test below tells "the file on disk was believed" from
// "the log was re-read".
func plantedIndex(slug, word string, logSize int64, lastEventID string) *searchIndex {
	ix := &searchIndex{
		V:           searchIndexVersion,
		Slug:        slug,
		Title:       slug,
		LastEventID: lastEventID,
		LogSize:     logSize,
		Built:       time.Now().UTC(),
		Cells:       []searchCell{{ID: "planted", Index: 1, Type: "prompt", State: "ok", Prompt: word}},
		Postings:    map[string][]int32{},
	}
	ix.Docs = []searchDoc{{Cell: 0, Kind: searchKindPrompt, OutIdx: -1, Excerpt: word}}
	ix.Postings[word] = []int32{0}
	return ix
}

func hitWords(res searchResults) []string {
	var out []string
	for _, g := range res.Groups {
		for _, h := range g.Hits {
			out = append(out, g.Notebook+":"+h.CellID+":"+h.Kind)
		}
	}
	return out
}

func mustSearch(t *testing.T, q searchQuery) searchResults {
	t.Helper()
	res, err := searchNotebooks(q)
	if err != nil {
		t.Fatalf("search %q: %v", q.Text, err)
	}
	return res
}

// ─── Staleness ──────────────────────────────────────────────────────────

// An index whose log stamp names an event this log has never contained is
// an index of some other history: a second collectif writing the same
// directory, a restored backup, a log rewritten by hand. Believing it
// serves matches for cells that do not exist and, worse, hides the ones
// that do.
func TestSearchIndex_AnIndexFromAnotherHistoryIsDiscarded(t *testing.T) {
	dir := withTempNotebooks(t)
	seedNotebook(t, "real-history", NotebookMeta{CLI: "claude"}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "fix the projection ordering"},
	})
	closeAllNotebooks()

	size, _ := logStampOnDisk(t, dir, "real-history")
	// Same length, different history — the exact shape the snapshot bug
	// took, because two logs reach the same size all the time.
	writeIndexFile(t, dir, "real-history", plantedIndex("real-history", "ghostword", size, uuid.NewString()))

	if res := mustSearch(t, searchQuery{Text: "ghostword"}); res.Count != 0 {
		t.Errorf("an index from another history was believed: %v", hitWords(res))
	}
	if res := mustSearch(t, searchQuery{Text: "projection"}); res.Count != 1 {
		t.Errorf("after discarding the bad index, the log should have been re-read; got %d hits", res.Count)
	}
}

// The other half of the same rule: the id can match while the log has grown
// underneath it. A notebook is appended to constantly, so this is the
// common case rather than the exotic one.
func TestSearchIndex_AnIndexBehindItsLogIsDiscarded(t *testing.T) {
	dir := withTempNotebooks(t)
	st := seedNotebook(t, "growing", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "first question"},
	})
	size, lastID := logStampOnDisk(t, dir, "growing")

	// A valid index, planted before the log moves on.
	writeIndexFile(t, dir, "growing", plantedIndex("growing", "plantedword", size, lastID))
	if res := mustSearch(t, searchQuery{Text: "plantedword"}); res.Count != 1 {
		t.Fatalf("a matching index must be used, or every query re-folds every log; got %d hits", res.Count)
	}

	// Now the session continues, as it always does.
	if _, err := st.Append(evCellInserted, cellInsertedPayload{
		Cell: Cell{ID: "c2", Type: CellPrompt, State: CellOK, Source: "second question about caching"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if res := mustSearch(t, searchQuery{Text: "caching"}); res.Count != 1 {
		t.Errorf("a turn appended after the index was built is unfindable — the index went stale silently")
	}
	if res := mustSearch(t, searchQuery{Text: "plantedword"}); res.Count != 0 {
		t.Errorf("the superseded index was still being answered from: %v", hitWords(res))
	}
}

// An index written by an older build cannot be read as one written by this
// one. Same reasoning as the snapshot's empty LastEventID: unverifiable is
// discarded, because re-reading a log is cheap and a wrong answer is not.
func TestSearchIndex_AnIndexFromAnotherBuildIsDiscarded(t *testing.T) {
	dir := withTempNotebooks(t)
	seedNotebook(t, "versioned", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "a question about versions"},
	})
	closeAllNotebooks()

	size, lastID := logStampOnDisk(t, dir, "versioned")
	old := plantedIndex("versioned", "oldbuildword", size, lastID)
	old.V = searchIndexVersion - 1
	writeIndexFile(t, dir, "versioned", old)

	if res := mustSearch(t, searchQuery{Text: "oldbuildword"}); res.Count != 0 {
		t.Errorf("an index from another schema was believed: %v", hitWords(res))
	}
	if res := mustSearch(t, searchQuery{Text: "versions"}); res.Count != 1 {
		t.Errorf("the log should have been re-read; got %d hits", res.Count)
	}
}

// A rebuilt index has to reach disk, or a restart pays the full fold for
// every notebook on the machine and the cache is decorative.
func TestSearchIndex_IsWrittenBesideTheLog(t *testing.T) {
	dir := withTempNotebooks(t)
	seedNotebook(t, "persisted", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "something worth finding"},
	})
	mustSearch(t, searchQuery{Text: "finding"})

	path := filepath.Join(dir, "persisted"+searchIndexSuffix)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no index beside the log: %v", err)
	}
	var ix searchIndex
	if err := json.Unmarshal(b, &ix); err != nil {
		t.Fatalf("index is not readable: %v", err)
	}
	if ix.LastEventID == "" {
		t.Error("index carries no log stamp — nothing later can verify it")
	}
	// Deleting it must cost a rebuild, not an error: that is what
	// "disposable" means.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if res := mustSearch(t, searchQuery{Text: "finding"}); res.Count != 1 {
		t.Errorf("after deleting the index the log should answer; got %d hits", res.Count)
	}
}

// Verification reads the tail of the log rather than the whole of it, and
// one event line is not boundedly small — a cell's output is capped at
// 256 KiB. A fixed read window lands mid-event on exactly the notebooks
// where rebuilding is most expensive, and then rebuilds them on every
// keystroke of every query.
func TestSearchIndex_VerifiesALogWhoseLastEventIsHuge(t *testing.T) {
	dir := withTempNotebooks(t)
	st := seedNotebook(t, "huge-tail", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "read the enormous file"},
	})
	if _, err := st.Append(evOutputAppended, outputAppendedPayload{
		CellID: "c1",
		Output: Output{Type: OutputToolResult, Text: strings.Repeat("x", 200*1024)},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	size, lastID := logStampOnDisk(t, dir, "huge-tail")
	writeIndexFile(t, dir, "huge-tail", plantedIndex("huge-tail", "verifiedword", size, lastID))
	if res := mustSearch(t, searchQuery{Text: "verifiedword"}); res.Count != 1 {
		t.Error("a correct index was rejected because the last event was larger than the read window")
	}
}

// ─── The distinction that makes this worth having ───────────────────────

// A transcript is unsearchable because everything in it is the same kind of
// text. A notebook separates what a human asked from what the agent said
// from what it ran, and a query that cannot use that separation is no
// better than grep over the scrollback.
func TestSearch_SeparatesAuthoredSourceFromProducedOutput(t *testing.T) {
	withTempNotebooks(t)
	seedNotebook(t, "kinds", NotebookMeta{CLI: "claude"}, []Cell{{
		ID: "c1", Type: CellPrompt, State: CellOK,
		Source: "deploy the marmalade service",
		Outputs: []Output{
			{Type: OutputText, Text: "I deployed the marmalade service"},
			{Type: OutputToolCall, Data: map[string]any{
				"name":  "Bash",
				"input": map[string]any{"command": "git push origin marmalade"},
			}},
			{Type: OutputToolResult, Text: "marmalade: everything up-to-date"},
		},
	}})

	// One row per turn: four blocks of that turn matched, and the row says
	// so rather than being repeated four times.
	all := mustSearch(t, searchQuery{Text: "marmalade"})
	if all.Count != 1 {
		t.Fatalf("unfiltered search returned %d rows, want 1 per turn: %v", all.Count, hitWords(all))
	}
	if got := all.Groups[0].Hits[0].Matches; got != 4 {
		t.Errorf("the row reports %d matching blocks, want 4", got)
	}
	// The question is what stands for the turn, not the tool result that
	// happens to contain the word too.
	if got := all.Groups[0].Hits[0].Kind; got != searchKindPrompt {
		t.Errorf("the turn is represented by a %s block; the prompt should lead", got)
	}

	cases := []struct {
		kind        string
		wantMatches int
	}{
		{searchKindPrompt, 1},
		{searchKindOutput, 1},
		{searchKindTool, 2},
	}
	for _, tc := range cases {
		res := mustSearch(t, searchQuery{Text: "marmalade", Kinds: []string{tc.kind}})
		if res.Count != 1 {
			t.Errorf("kind=%s found %d rows, want 1: %v", tc.kind, res.Count, hitWords(res))
			continue
		}
		h := res.Groups[0].Hits[0]
		if h.Kind != tc.kind {
			t.Errorf("kind=%s returned a %s hit", tc.kind, h.Kind)
		}
		if h.Matches != tc.wantMatches {
			t.Errorf("kind=%s reports %d matching blocks, want %d", tc.kind, h.Matches, tc.wantMatches)
		}
	}

	// The tool call's own name is searchable, or "every time an agent ran
	// git push" cannot be asked.
	if res := mustSearch(t, searchQuery{Text: "git push", Kinds: []string{searchKindTool}}); res.Count != 1 {
		t.Errorf("`git push` found %d rows, want 1: %v", res.Count, hitWords(res))
	}
}

// Ranking is a tie-break rather than a relevance engine, and it earns its
// place on exactly one observation from the real corpus: a 200 KB directory
// listing matches `git push` through .git and .gitignore, and before this
// existed those listings buried the turn that actually ran the command.
//
// Nothing is hidden by it. What it decides is which turn is read first.
func TestSearch_PrefersAVisibleMatchOverAHaystack(t *testing.T) {
	withTempNotebooks(t)
	haystack := strings.Repeat("irrelevant filler line\n", 500) + "buried needle at the end"
	seedNotebook(t, "ranked", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "unrelated question",
			Outputs: []Output{{Type: OutputToolResult, Text: haystack}}},
		{ID: "c2", Type: CellPrompt, State: CellOK, Source: "what about the needle"},
	})

	res := mustSearch(t, searchQuery{Text: "needle"})
	if res.Count != 2 {
		t.Fatalf("found %d rows, want 2: %v", res.Count, hitWords(res))
	}
	first := res.Groups[0].Hits[0]
	if first.CellID != "c2" {
		t.Errorf("the buried match led; a question whose match can be shown should come first")
	}
	// The buried one is still returned, and still says where it is — the
	// snippet just cannot show a match past the stored excerpt.
	second := res.Groups[0].Hits[1]
	if second.CellID != "c1" || second.Snippet == "" {
		t.Errorf("the deep match was dropped or came back empty: %+v", second)
	}
}

// #55a nests a subagent's work under the turn that spawned it, and that is
// the whole reason a match inside a child is useful: it can be reported
// with the prompt that caused it. A hit that says only "some agent said
// this, somewhere" is the transcript problem again.
func TestSearch_AMatchInsideASubagentReportsTheParentPrompt(t *testing.T) {
	withTempNotebooks(t)
	seedNotebook(t, "nested", NotebookMeta{CLI: "claude"}, []Cell{{
		ID: "c1", Type: CellPrompt, State: CellOK,
		Source: "find out which session hit the cache bug",
		Outputs: []Output{
			{Type: OutputText, Text: "delegating that", Data: map[string]any{}},
			{Type: OutputText, Text: "it was snapshotMatchesLog all along",
				Data: map[string]any{"agentId": "agent-7", "agentType": "Explore"}},
		},
	}})

	res := mustSearch(t, searchQuery{Text: "snapshotMatchesLog"})
	if res.Count != 1 {
		t.Fatalf("found %d, want 1: %v", res.Count, hitWords(res))
	}
	h := res.Groups[0].Hits[0]
	if h.Prompt != "find out which session hit the cache bug" {
		t.Errorf("hit reports prompt %q — a match inside a child must carry the turn that caused it", h.Prompt)
	}
	if h.AgentID != "agent-7" {
		t.Errorf("hit reports agentId %q, want agent-7", h.AgentID)
	}
	// Sub-token matching is what makes an identifier findable by its parts;
	// nobody searches for `snapshotmatcheslog`.
	if res := mustSearch(t, searchQuery{Text: "snapshot"}); res.Count != 1 {
		t.Errorf("`snapshot` found %d, want 1 — camel-cased identifiers must be findable by their parts", res.Count)
	}
}

// The tokenising cap is a real limit and is written down here rather than
// left to be discovered. A block longer than maxTokenizedText is findable by
// its first 32 KiB and not by the rest — the alternative was letting one
// runaway `yes` or one enormous file read dominate a whole notebook's index.
func TestSearch_StopsIndexingAVeryLongBlock(t *testing.T) {
	withTempNotebooks(t)
	seedNotebook(t, "enormous", NotebookMeta{}, []Cell{{
		ID: "c1", Type: CellPrompt, State: CellOK, Source: "read the whole thing",
		Outputs: []Output{{Type: OutputToolResult,
			Text: "findablenearthetop\n" + strings.Repeat("filler line\n", 40000) + "unreachabletail"}},
	}})

	if res := mustSearch(t, searchQuery{Text: "findablenearthetop"}); res.Count != 1 {
		t.Errorf("the head of a long block must stay findable; got %d", res.Count)
	}
	if res := mustSearch(t, searchQuery{Text: "unreachabletail"}); res.Count != 0 {
		t.Errorf("indexing ran past the cap — one runaway command can now flood the index")
	}
}

// ─── Filters ────────────────────────────────────────────────────────────

func TestSearch_FiltersByCLI(t *testing.T) {
	withTempNotebooks(t)
	seedNotebook(t, "from-claude", NotebookMeta{CLI: "claude", SessionID: "s1"}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "shared subject matter"},
	})
	seedNotebook(t, "from-codex", NotebookMeta{CLI: "codex", SessionID: "s2"}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "shared subject matter"},
	})

	if res := mustSearch(t, searchQuery{Text: "shared"}); res.Count != 2 {
		t.Fatalf("unfiltered found %d, want 2", res.Count)
	}
	res := mustSearch(t, searchQuery{Text: "shared", CLI: "codex"})
	if res.Count != 1 || res.Groups[0].Notebook != "from-codex" {
		t.Errorf("cli=codex returned %v", hitWords(res))
	}
}

// since is answered from the log's own timestamps rather than from the
// file's mtime, because a notebook is appended to for days and its mtime
// says only when it was last touched.
func TestSearch_FiltersBySince(t *testing.T) {
	dir := withTempNotebooks(t)
	old := time.Now().Add(-72 * time.Hour).UTC()
	recent := time.Now().Add(-30 * time.Minute).UTC()

	writeRawNotebook(t, dir, "timed", []Event{
		rawEvent(old, evNotebookCreated, notebookCreatedPayload{Title: "Timed"}),
		rawEvent(old, evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: "c1", Type: CellPrompt, State: CellOK, Source: "an ancient question about pelicans"},
		}),
		rawEvent(recent, evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: "c2", Type: CellPrompt, State: CellOK, Source: "a fresh question about pelicans"},
		}),
	})

	if res := mustSearch(t, searchQuery{Text: "pelicans"}); res.Count != 2 {
		t.Fatalf("unfiltered found %d, want 2", res.Count)
	}
	res := mustSearch(t, searchQuery{Text: "pelicans", Since: time.Now().Add(-24 * time.Hour)})
	if res.Count != 1 {
		t.Fatalf("since=24h found %d, want 1: %v", res.Count, hitWords(res))
	}
	if got := res.Groups[0].Hits[0].CellID; got != "c2" {
		t.Errorf("since=24h returned cell %q, want c2", got)
	}
}

// ─── Raw-log fixtures ───────────────────────────────────────────────────

func rawEvent(at time.Time, typ string, payload any) Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return Event{V: nbSchemaVersion, Type: typ, ID: uuid.NewString(), At: at, Payload: raw}
}

// writeRawNotebook writes a log directly, which is the only way to produce
// events with times of our choosing — Append stamps them with now.
func writeRawNotebook(t *testing.T, dir, slug string, evs []Event) {
	t.Helper()
	var b strings.Builder
	for _, e := range evs {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write log: %v", err)
	}
}

// ─── Ranking and bounds ─────────────────────────────────────────────────

// Results are bounded. A one-word query against ten notebooks can match
// thousands of blocks, and a page that renders all of them is a page nobody
// waits for.
func TestSearch_BoundsItsResults(t *testing.T) {
	withTempNotebooks(t)
	cells := make([]Cell, 0, 40)
	for i := 0; i < 40; i++ {
		cells = append(cells, Cell{
			ID: fmt.Sprintf("c%d", i), Type: CellPrompt, State: CellOK,
			Source: fmt.Sprintf("recurring subject %d", i),
		})
	}
	seedNotebook(t, "many", NotebookMeta{}, cells)

	res := mustSearch(t, searchQuery{Text: "recurring", Limit: 10})
	if res.Count != 10 {
		t.Errorf("limit=10 returned %d hits", res.Count)
	}
	if !res.Truncated {
		t.Error("a truncated result set must say so, or the reader believes they saw everything")
	}
}

// Every term must match, or a two-word query is a two-word OR and the
// second word does nothing.
func TestSearch_RequiresEveryTerm(t *testing.T) {
	withTempNotebooks(t)
	seedNotebook(t, "conjunction", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "alpha bravo"},
		{ID: "c2", Type: CellPrompt, State: CellOK, Source: "alpha charlie"},
	})
	res := mustSearch(t, searchQuery{Text: "alpha bravo"})
	if res.Count != 1 || res.Groups[0].Hits[0].CellID != "c1" {
		t.Errorf("`alpha bravo` returned %v, want only c1", hitWords(res))
	}
}
