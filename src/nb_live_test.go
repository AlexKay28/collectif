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

// ─── Findings from the M0/M1 code review ────────────────────────────────

// Append folds before it writes, so a failed write must not leave the
// in-memory document ahead of the log. Otherwise Doc(), GET /api/nb/<id>
// and the WS fold all report a mutation that is not durable and that no
// client ever saw.
func TestStore_FailedWriteDoesNotAdvanceTheDocument(t *testing.T) {
	st, _ := newTestStore(t)
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellShell}}); err != nil {
		t.Fatalf("seed append: %v", err)
	}
	before := st.Doc()

	// Break the log underneath the store without marking it closed, the
	// way ENOSPC or an EIO would.
	st.mu.Lock()
	_ = st.log.Close()
	st.mu.Unlock()

	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c2", Type: CellShell}}); err == nil {
		t.Fatal("expected the append to fail once the log is unwritable")
	}

	after := st.Doc()
	if after.Version != before.Version {
		t.Errorf("Version = %d after a failed write, want %d — the document ran ahead of the log",
			after.Version, before.Version)
	}
	if len(after.Cells) != len(before.Cells) {
		t.Errorf("cells = %d after a failed write, want %d", len(after.Cells), len(before.Cells))
	}
}

// A cell has to be insertable at the top. afterCellId alone cannot express
// it: empty means "append", so inserting above the first cell silently put
// the new cell at the bottom.
func TestFold_InsertBeforeCellPlacesAtThatIndex(t *testing.T) {
	evs := createdEvents(t)
	evs = append(evs,
		ev(t, "e2", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellShell}}),
		ev(t, "e3", evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c2", Type: CellShell}}),
		// Above the first cell.
		ev(t, "e4", evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: "c0", Type: CellShell}, BeforeCellID: "c1",
		}),
		// Between them.
		ev(t, "e5", evCellInserted, cellInsertedPayload{
			Cell: Cell{ID: "cmid", Type: CellShell}, BeforeCellID: "c2",
		}),
	)
	nb, err := foldEvents(evs)
	if err != nil {
		t.Fatalf("foldEvents: %v", err)
	}
	if got, want := cellIDs(nb), []string{"c0", "c1", "cmid", "c2"}; !equalStrings(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestNotebookAPI_InsertBeforeFirstCell(t *testing.T) {
	f := newNBFixture(t)
	first := f.addCell(t, "shell", "original first")

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells",
		map[string]any{"type": "shell", "source": "new top", "beforeCellId": first})
	if rec.Code != http.StatusOK {
		t.Fatalf("insert before: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &out)

	doc := f.st.Doc()
	if got, want := cellIDs(doc), []string{out.CellID, first}; !equalStrings(got, want) {
		t.Errorf("order = %v, want the new cell first", got)
	}
}

// Past the output cap the store stops accumulating; it must also stop
// telling the writer to broadcast, or a `yes` keeps pushing frames at every
// subscriber forever.
func TestAppendLive_StopsAcceptingPastTheCap(t *testing.T) {
	st, _ := newTestStore(t)
	chunk := strings.Repeat("x", 64*1024)

	accepted := 0
	for i := 0; i < 8; i++ { // 512 KiB against a 256 KiB cap
		if st.appendLive("c1", "r1", chunk) {
			accepted++
		}
	}
	if accepted == 8 {
		t.Error("every chunk was accepted past the cap — output would broadcast without bound")
	}
	if got := len(st.liveText("c1", "r1")); got > maxCellOutput+128 {
		t.Errorf("live text = %d bytes, want it capped near %d", got, maxCellOutput)
	}
}

// The handlers arm a 60s read deadline and re-arm it from the pong
// handler, but nothing was ever sending pings — so a one-directional
// socket (the notebook and dashboard streams both are, by design) was torn
// down by the server every minute and silently reconnected.
func TestWSSub_SendsPingsSoIdleSocketsSurvive(t *testing.T) {
	prev := wsPingEvery
	wsPingEvery = 50 * time.Millisecond
	t.Cleanup(func() { wsPingEvery = prev })

	f := newNBFixture(t)
	conn, closeWS := f.dialWS(t)
	defer closeWS()

	got := make(chan struct{}, 1)
	conn.SetPingHandler(func(string) error {
		select {
		case got <- struct{}{}:
		default:
		}
		return nil
	})

	// ReadMessage is what dispatches control frames to the handler.
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("no ping within 5s — an idle socket would hit the 60s read deadline and be dropped")
	}
}
