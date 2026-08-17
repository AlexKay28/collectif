package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #49 M1 slice 1. The log is the source of truth and the snapshot is a
// disposable cache. These tests hold that line: deleting the snapshot must
// change nothing, and a snapshot that has fallen behind must lose to the
// log rather than truncate the notebook.

func newTestStore(t *testing.T) (*notebookStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := openNotebookStore(dir, "notes", "Notes", "/repo")
	if err != nil {
		t.Fatalf("openNotebookStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

func seed(t *testing.T, st *notebookStore) {
	t.Helper()
	appends := []struct {
		typ     string
		payload any
	}{
		{evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellMarkdown, Source: "# Title"}}},
		{evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c2", Type: CellShell, Source: "go test ./..."}}},
		{evRunStarted, runStartedPayload{CellID: "c2", RunID: "r1"}},
		{evOutputAppended, outputAppendedPayload{CellID: "c2", RunID: "r1", Output: Output{Type: OutputText, Text: "ok\n"}}},
		{evRunFinished, runFinishedPayload{CellID: "c2", RunID: "r1", Status: CellOK}},
	}
	for _, a := range appends {
		if _, err := st.Append(a.typ, a.payload); err != nil {
			t.Fatalf("Append(%s): %v", a.typ, err)
		}
	}
}

func TestStore_RoundTripsThroughTheLog(t *testing.T) {
	st, dir := newTestStore(t)
	seed(t, st)
	before := st.Doc()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := openNotebookStore(dir, "notes", "Notes", "/repo")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	after := reopened.Doc()
	if after.Version != before.Version {
		t.Errorf("Version = %d, want %d", after.Version, before.Version)
	}
	if !equalStrings(cellIDs(after), cellIDs(before)) {
		t.Fatalf("cells = %v, want %v", cellIDs(after), cellIDs(before))
	}
	c := mustCell(t, after, "c2")
	if c.State != CellOK {
		t.Errorf("State = %q, want %q", c.State, CellOK)
	}
	if len(c.Outputs) != 1 || c.Outputs[0].Text != "ok\n" {
		t.Errorf("Outputs = %+v, want the persisted text output", c.Outputs)
	}
}

func TestStore_SnapshotIsDisposable(t *testing.T) {
	st, dir := newTestStore(t)
	seed(t, st)
	want := st.Doc()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	snap := filepath.Join(dir, "notes.snap.json")
	if _, err := os.Stat(snap); err != nil {
		t.Fatalf("expected a snapshot after a clean close: %v", err)
	}
	if err := os.Remove(snap); err != nil {
		t.Fatalf("remove snapshot: %v", err)
	}

	reopened, err := openNotebookStore(dir, "notes", "Notes", "/repo")
	if err != nil {
		t.Fatalf("reopen without snapshot: %v", err)
	}
	defer reopened.Close()

	got := reopened.Doc()
	if got.Version != want.Version {
		t.Errorf("Version = %d, want %d", got.Version, want.Version)
	}
	if !equalStrings(cellIDs(got), cellIDs(want)) {
		t.Errorf("cells = %v, want %v", cellIDs(got), cellIDs(want))
	}
}

// A snapshot that has fallen behind the log must not truncate the notebook:
// the store folds the remaining events on top of it.
func TestStore_StaleSnapshotLosesToTheLog(t *testing.T) {
	st, dir := newTestStore(t)
	seed(t, st)
	full := st.Doc()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Rewrite the snapshot as if it had been written three events ago,
	// before c2 was ever inserted.
	stale := notebookSnapshot{
		Version: 2,
		Notebook: &Notebook{
			ID: full.ID, Title: full.Title, Root: full.Root, Version: 2,
			Cells: []Cell{{ID: "c1", Type: CellMarkdown, Source: "# Title", State: CellIdle}},
		},
	}
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale snapshot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.snap.json"), b, 0o644); err != nil {
		t.Fatalf("write stale snapshot: %v", err)
	}

	reopened, err := openNotebookStore(dir, "notes", "Notes", "/repo")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	got := reopened.Doc()
	if got.Version != full.Version {
		t.Errorf("Version = %d, want %d — the log is authoritative", got.Version, full.Version)
	}
	if !equalStrings(cellIDs(got), cellIDs(full)) {
		t.Errorf("cells = %v, want %v", cellIDs(got), cellIDs(full))
	}
	if s := mustCell(t, got, "c2").State; s != CellOK {
		t.Errorf("c2 State = %q, want %q", s, CellOK)
	}
}

// A corrupt snapshot is a cache miss, not a failure to open.
func TestStore_CorruptSnapshotFallsBackToTheLog(t *testing.T) {
	st, dir := newTestStore(t)
	seed(t, st)
	want := st.Doc()
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.snap.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt snapshot: %v", err)
	}

	reopened, err := openNotebookStore(dir, "notes", "Notes", "/repo")
	if err != nil {
		t.Fatalf("reopen with corrupt snapshot: %v", err)
	}
	defer reopened.Close()

	if got := reopened.Doc().Version; got != want.Version {
		t.Errorf("Version = %d, want %d", got, want.Version)
	}
}

// One persisted line per event. Streaming deltas never reach the log —
// without that rule a chatty session writes a hundred-megabyte document.
func TestStore_AppendWritesExactlyOneLinePerEvent(t *testing.T) {
	st, dir := newTestStore(t)
	seed(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, filepath.Join(dir, "notes.jsonl"))
	// notebook_created + the five seeded events.
	if len(lines) != 6 {
		t.Fatalf("log has %d lines, want 6", len(lines))
	}
	for i, ln := range lines {
		var e Event
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("line %d is not a valid event: %v", i, err)
		}
		if e.V != nbSchemaVersion {
			t.Errorf("line %d has V=%d, want %d", i, e.V, nbSchemaVersion)
		}
		if e.ID == "" || e.Type == "" || e.At.IsZero() {
			t.Errorf("line %d missing envelope fields: %+v", i, e)
		}
	}
}

// An event type written by a newer build must survive a read/reopen by an
// older one — skipped by the fold, still counted, never dropped.
func TestStore_UnknownEventInLogIsToleratedOnReopen(t *testing.T) {
	st, dir := newTestStore(t)
	seed(t, st)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	logPath := filepath.Join(dir, "notes.jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	future := `{"v":99,"type":"cell_teleported_20270101","id":"eX","at":"2027-01-01T00:00:00Z","payload":{"destination":"mars"}}` + "\n"
	if _, err := f.WriteString(future); err != nil {
		t.Fatalf("append future event: %v", err)
	}
	f.Close()

	// Remove the snapshot so the reopen must fold the whole log.
	_ = os.Remove(filepath.Join(dir, "notes.snap.json"))

	reopened, err := openNotebookStore(dir, "notes", "Notes", "/repo")
	if err != nil {
		t.Fatalf("reopen with an unknown event type: %v", err)
	}
	defer reopened.Close()

	if got := reopened.Doc().Version; got != 7 {
		t.Errorf("Version = %d, want 7 — the unknown event still counts", got)
	}
	if got := len(reopened.Doc().Cells); got != 2 {
		t.Errorf("cells = %d, want 2 — the notebook is otherwise intact", got)
	}

	// And it must still be on disk afterwards.
	if !strings.Contains(strings.Join(readLines(t, logPath), "\n"), "cell_teleported_20270101") {
		t.Error("the unknown event was dropped from the log")
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return out
}

// #47 P2 — found while regenerating a replay notebook with a server still
// holding it open.
//
// load() trusts a snapshot whose version is <= len(events) and replays the
// remaining events on top. That is only valid if the snapshot is a fold of
// *this* log's first Version events, and nothing checked it. When two
// processes own one notebook directory — or a log is rewritten, or a
// snapshot restored from a backup — the snapshot summarises a different
// history, the tail is replayed onto the wrong base, and the document
// served is silently wrong.
//
// It is not a hypothetical: a stale snapshot cost a real notebook its
// entire Meta block, so the document claimed to be attached to no session
// while its log plainly said otherwise. ADR 0001 §4.3 calls the snapshot
// "derived and disposable"; that is only true if a snapshot we cannot
// verify is thrown away.
func TestNotebookStore_ASnapshotFromAnotherHistoryIsDiscarded(t *testing.T) {
	dir := withTempNotebooks(t)

	st, err := createNotebook("Divergent", t.TempDir())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	slug := st.slug
	meta := NotebookMeta{SessionID: "the-real-session", CLI: "claude"}
	if _, err := st.Append(evMetaSet, metaSetPayload{Meta: &meta}); err != nil {
		t.Fatalf("append: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.Append(evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: fmt.Sprintf("c%d", i), Type: CellMarkdown, Source: "real"},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	want := st.Doc()
	if err := releaseNotebook(slug); err != nil {
		t.Fatalf("release: %v", err)
	}

	// A snapshot from a different history: same notebook id, a plausible
	// version, and content that never came from this log. Exactly the shape
	// a second process writing the same directory produces.
	imposter := notebookSnapshot{
		Version: 2,
		Notebook: &Notebook{
			ID: want.ID, Title: want.Title, Root: want.Root,
			Cells: []Cell{{ID: "ghost", Type: CellMarkdown, Source: "from another timeline"}},
			// Note the empty Meta — this is what the real incident lost.
			Version: 2,
		},
	}
	b, err := json.Marshal(imposter)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, slug+".snap.json"), b, 0o644); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	reopened, err := acquireNotebook(slug)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Doc()

	if got.Meta.SessionID != "the-real-session" {
		t.Errorf("Meta.SessionID = %q, want the-real-session — the log said so and the snapshot was believed instead",
			got.Meta.SessionID)
	}
	for _, c := range got.Cells {
		if c.ID == "ghost" {
			t.Error("a cell from another history survived into the document")
		}
	}
	if len(got.Cells) != len(want.Cells) {
		t.Errorf("got %d cells, want %d", len(got.Cells), len(want.Cells))
	}
}

// The fast path has to keep working, or every open re-folds the whole log.
func TestNotebookStore_AMatchingSnapshotIsStillUsed(t *testing.T) {
	withTempNotebooks(t)
	st, err := createNotebook("Matching", t.TempDir())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	slug := st.slug
	for i := 0; i < 3; i++ {
		if _, err := st.Append(evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: fmt.Sprintf("c%d", i), Type: CellMarkdown, Source: "kept"},
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	want := st.Doc()
	if err := releaseNotebook(slug); err != nil {
		t.Fatalf("release: %v", err)
	}

	reopened, err := acquireNotebook(slug)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Doc()
	if len(got.Cells) != len(want.Cells) || got.Version != want.Version {
		t.Errorf("reopened as %d cells / version %d, want %d / %d",
			len(got.Cells), got.Version, len(want.Cells), want.Version)
	}
}
