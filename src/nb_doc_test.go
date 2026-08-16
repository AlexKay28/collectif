package main

import (
	"encoding/json"
	"testing"
	"time"
)

// #49 M1 slice 1. The fold is the notebook's definition: a document is
// whatever folding its log produces. These tests pin that contract,
// including the forward-compatibility promise the ADR makes — an event
// type a newer build wrote must degrade, never corrupt.

// ev builds an envelope with a marshalled payload. Ids and timestamps are
// fixed so fold results are comparable.
func ev(t *testing.T, id, typ string, payload any) Event {
	t.Helper()
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload for %s: %v", typ, err)
		}
		raw = b
	}
	return Event{
		V:       nbSchemaVersion,
		Type:    typ,
		ID:      id,
		At:      time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		Payload: raw,
	}
}

func createdEvents(t *testing.T) []Event {
	t.Helper()
	return []Event{
		ev(t, "e1", evNotebookCreated, notebookCreatedPayload{
			Title: "Investigating the gauge",
			Root:  "/repo",
		}),
	}
}

func TestFold_NotebookCreatedSetsIdentity(t *testing.T) {
	nb, err := foldEvents(createdEvents(t))
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	if nb.Title != "Investigating the gauge" {
		t.Errorf("Title = %q", nb.Title)
	}
	if nb.Root != "/repo" {
		t.Errorf("Root = %q", nb.Root)
	}
	if nb.Version != 1 {
		t.Errorf("Version = %d, want 1 (one event folded)", nb.Version)
	}
	if len(nb.Cells) != 0 {
		t.Errorf("Cells = %d, want 0", len(nb.Cells))
	}
}

func TestFold_InsertAppendsAndOrdersCells(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: "c1", Type: CellMarkdown, Source: "# One"},
		}),
		ev(t, "e3", evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: "c2", Type: CellShell, Source: "go test ./..."},
		}),
		// Insert between c1 and c2.
		ev(t, "e4", evCellInserted, cellInsertedPayload{
			Cell:        Cell{ID: "c3", Type: CellMarkdown, Source: "# Middle"},
			AfterCellID: "c1",
		}),
	)

	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	want := []string{"c1", "c3", "c2"}
	if got := cellIDs(nb); !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if nb.Cells[0].State != CellIdle {
		t.Errorf("new cell state = %q, want %q", nb.Cells[0].State, CellIdle)
	}
}

func TestFold_EditMoveDelete(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellMarkdown, Source: "old"}}),
		ev(t, "e3", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c2", Type: CellShell, Source: "ls"}}),
		ev(t, "e4", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c3", Type: CellShell, Source: "pwd"}}),
		ev(t, "e5", evCellEdited, cellEditedPayload{CellID: "c1", Source: strPtr("new")}),
		ev(t, "e6", evCellMoved, cellMovedPayload{CellID: "c3", BeforeCellID: "c1"}),
		ev(t, "e7", evCellDeleted, cellDeletedPayload{CellID: "c2"}),
	)

	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	if got, want := cellIDs(nb), []string{"c3", "c1"}; !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	c := mustCell(t, nb, "c1")
	if c.Source != "new" {
		t.Errorf("Source = %q, want %q", c.Source, "new")
	}
	if c.Type != CellMarkdown {
		t.Errorf("edit changed Type to %q — an edit with no Type must leave it alone", c.Type)
	}
}

// The compatibility commitment: a newer build's event type must not break
// an older one's fold, and must survive being read back.
func TestFold_UnknownEventTypeIsSkippedNotFatal(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellMarkdown, Source: "hi"}}),
		ev(t, "e3", "cell_teleported_20270101", map[string]any{"cellId": "c1", "destination": "mars"}),
		ev(t, "e4", evCellEdited, cellEditedPayload{CellID: "c1", Source: strPtr("still here")}),
	)

	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents returned an error on an unknown event type: %v", err)
	}
	if got := mustCell(t, nb, "c1").Source; got != "still here" {
		t.Errorf("Source = %q — events after the unknown one must still apply", got)
	}
	if nb.Version != 4 {
		t.Errorf("Version = %d, want 4 — an unknown event is still an event in the log", nb.Version)
	}
}

// A malformed payload on a known type is a data error worth surfacing,
// unlike an unknown type which is expected during a version skew.
func TestFold_MalformedPayloadOnKnownTypeErrors(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs, Event{
		V:       nbSchemaVersion,
		Type:    evCellInserted,
		ID:      "e2",
		Payload: json.RawMessage(`{"cell": "not-an-object"}`),
	})
	if _, err := foldEvents(evs); err == nil {
		t.Fatal("expected an error for a malformed payload on a known event type")
	}
}

func TestFold_RunLifecycleSetsStateAndOutputs(t *testing.T) {
	start := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellShell, Source: "echo hi"}}),
	)

	e3 := ev(t, "e3", evRunStarted, runStartedPayload{CellID: "c1", RunID: "r1"})
	e3.At = start
	e4 := ev(t, "e4", evOutputAppended, outputAppendedPayload{
		CellID: "c1", RunID: "r1",
		Output: Output{Type: OutputText, Text: "hi\n"},
	})
	e5 := ev(t, "e5", evRunFinished, runFinishedPayload{CellID: "c1", RunID: "r1", Status: CellOK})
	e5.At = start.Add(1500 * time.Millisecond)
	evs = append(evs, e3, e4, e5)

	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	c := mustCell(t, nb, "c1")
	if c.State != CellOK {
		t.Errorf("State = %q, want %q", c.State, CellOK)
	}
	if len(c.Outputs) != 1 || c.Outputs[0].Text != "hi\n" {
		t.Errorf("Outputs = %+v, want one text output", c.Outputs)
	}
	if c.Duration != 1500*time.Millisecond {
		t.Errorf("Duration = %v, want 1.5s (derived from the run's event timestamps)", c.Duration)
	}
}

// A re-run replaces the previous run's outputs rather than appending to
// them — otherwise re-running a cell twice shows both runs stacked.
func TestFold_RunStartedClearsPreviousOutputs(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellShell, Source: "echo hi"}}),
		ev(t, "e3", evRunStarted, runStartedPayload{CellID: "c1", RunID: "r1"}),
		ev(t, "e4", evOutputAppended, outputAppendedPayload{CellID: "c1", RunID: "r1", Output: Output{Type: OutputText, Text: "first"}}),
		ev(t, "e5", evRunFinished, runFinishedPayload{CellID: "c1", RunID: "r1", Status: CellOK}),
		ev(t, "e6", evRunStarted, runStartedPayload{CellID: "c1", RunID: "r2"}),
		ev(t, "e7", evOutputAppended, outputAppendedPayload{CellID: "c1", RunID: "r2", Output: Output{Type: OutputText, Text: "second"}}),
	)

	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	c := mustCell(t, nb, "c1")
	if len(c.Outputs) != 1 {
		t.Fatalf("Outputs = %d, want 1 — a new run replaces the old outputs", len(c.Outputs))
	}
	if c.Outputs[0].Text != "second" {
		t.Errorf("Outputs[0].Text = %q, want %q", c.Outputs[0].Text, "second")
	}
	if c.State != CellRunning {
		t.Errorf("State = %q, want %q", c.State, CellRunning)
	}
}

func TestFold_CellsInvalidatedMarksStale(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellShell}}),
		ev(t, "e3", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c2", Type: CellShell}}),
		ev(t, "e4", evRunStarted, runStartedPayload{CellID: "c2", RunID: "r1"}),
		ev(t, "e5", evRunFinished, runFinishedPayload{CellID: "c2", RunID: "r1", Status: CellOK}),
		ev(t, "e6", evCellsInvalidated, cellsInvalidatedPayload{CellIDs: []string{"c2"}}),
	)

	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	if got := mustCell(t, nb, "c2").State; got != CellStale {
		t.Errorf("c2 State = %q, want %q", got, CellStale)
	}
	// An idle cell that never ran has nothing to invalidate.
	if got := mustCell(t, nb, "c1").State; got != CellIdle {
		t.Errorf("c1 State = %q, want %q — a cell that never ran cannot go stale", got, CellIdle)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────

func strPtr(s string) *string { return &s }

func cellIDs(nb *Notebook) []string {
	out := make([]string, 0, len(nb.Cells))
	for _, c := range nb.Cells {
		out = append(out, c.ID)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func mustCell(t *testing.T, nb *Notebook, id string) Cell {
	t.Helper()
	for _, c := range nb.Cells {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("cell %q not found; have %v", id, cellIDs(nb))
	return Cell{}
}
