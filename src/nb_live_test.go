package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// #49 M1 exit criterion: "hard-refresh the browser mid-run — output resumes,
// nothing lost." A refresh is a fresh connection and a fresh fold, so the
// question is whether the fold carries what a running cell has produced so
// far. Deltas are deliberately not persisted, so the fold has to carry the
// live buffer alongside the document, or a reconnecting client stares at a
// running cell with no output until it finishes.

func TestNotebookWS_FoldCarriesLiveOutputOfARunningCell(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo produced-before-refresh; sleep 30")

	first, closeFirst := f.dialWS(t)
	defer closeFirst()
	t.Cleanup(func() { nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil) })

	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil); rec.Code >= 300 {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	// Wait until output has genuinely streamed to the first connection.
	f.readUntilDelta(t, first, cell, "produced-before-refresh", 10*time.Second)

	// Now simulate the refresh: a brand-new connection, as a reloaded page
	// would make. Its opening fold is everything it will know.
	second, closeSecond := f.dialWSRaw(t)
	defer closeSecond()

	_ = second.SetReadDeadline(time.Now().Add(10 * time.Second))
	var fold struct {
		Type     string    `json:"type"`
		Version  int       `json:"version"`
		Notebook *Notebook `json:"notebook"`
		Live     map[string]struct {
			RunID string `json:"runId"`
			Text  string `json:"text"`
		} `json:"live"`
	}
	if err := second.ReadJSON(&fold); err != nil {
		t.Fatalf("read fold on reconnect: %v", err)
	}
	if fold.Type != "fold" {
		t.Fatalf("first message = %q, want fold", fold.Type)
	}

	// The document itself cannot carry it: run_started cleared the outputs
	// and the finalised output has not been written yet.
	c := mustCell(t, fold.Notebook, cell)
	if c.State != CellRunning {
		t.Fatalf("cell state in fold = %q, want %q", c.State, CellRunning)
	}

	live, ok := fold.Live[cell]
	if !ok {
		t.Fatal("fold carried no live output for the running cell — a refreshed page would show a running cell with nothing in it")
	}
	if !strings.Contains(live.Text, "produced-before-refresh") {
		t.Errorf("live text = %q, want the output produced before the refresh", live.Text)
	}
	if live.RunID == "" {
		t.Error("live output carried no run id — a client cannot tell which run it belongs to")
	}
}

// Once the run finishes, the live buffer is gone: the finalised output in
// the log is the record, and keeping a second copy would let the two drift.
func TestNotebookWS_LiveBufferIsClearedWhenTheRunFinishes(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo short-lived")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	conn, closeWS := f.dialWSRaw(t)
	defer closeWS()
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var fold struct {
		Type string `json:"type"`
		Live map[string]struct {
			Text string `json:"text"`
		} `json:"live"`
	}
	if err := conn.ReadJSON(&fold); err != nil {
		t.Fatalf("read fold: %v", err)
	}
	if _, ok := fold.Live[cell]; ok {
		t.Error("live buffer survived the run — the log is the record once a run finishes")
	}
}

// Two tabs on one notebook both receive live traffic. The registry exists
// so they fold the same document; this proves they also both get fed.
func TestNotebookWS_TwoSubscribersBothReceiveEvents(t *testing.T) {
	f := newNBFixture(t)

	a, closeA := f.dialWS(t)
	defer closeA()
	b, closeB := f.dialWS(t)
	defer closeB()

	if _, err := f.st.Append(evCellInserted, cellInsertedPayload{
		Cell: Cell{ID: "shared-cell", Type: CellMarkdown, Source: "seen by both"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	for i, conn := range []*websocket.Conn{a, b} {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		var msg struct {
			Type  string `json:"type"`
			Event *Event `json:"event"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
		if msg.Type != "event" || msg.Event == nil || msg.Event.Type != evCellInserted {
			t.Errorf("subscriber %d got %+v, want a cell_inserted event", i, msg)
		}
	}
}

// Listing is a launcher operation: it should read titles, not take out a
// write handle on every notebook on disk. acquireNotebook registers the
// store and holds its log open, so using it here would mean opening a
// directory of notebooks pins all of them for the life of the process.
func TestListNotebooks_DoesNotPinEveryNotebookOpen(t *testing.T) {
	withTempNotebooks(t)

	for _, title := range []string{"One", "Two", "Three"} {
		st, err := createNotebook(title, t.TempDir())
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if err := releaseNotebook(st.slug); err != nil {
			t.Fatalf("release %s: %v", title, err)
		}
	}

	nbRegistryMu.Lock()
	before := len(nbRegistry)
	nbRegistryMu.Unlock()
	if before != 0 {
		t.Fatalf("test setup: %d notebooks still registered", before)
	}

	list, err := listNotebooks()
	if err != nil {
		t.Fatalf("listNotebooks: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("listed %d, want 3", len(list))
	}
	for _, n := range list {
		if n.Title == "" {
			t.Errorf("notebook %s listed without a title", n.ID)
		}
	}

	nbRegistryMu.Lock()
	after := len(nbRegistry)
	nbRegistryMu.Unlock()
	if after != 0 {
		t.Errorf("listing opened and pinned %d notebooks; a launcher must not hold every log open", after)
	}
}
