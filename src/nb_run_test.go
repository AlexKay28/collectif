package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// #49 M1 slice 3. Shell cells are the first thing collectif runs on the
// user's behalf through the notebook, so these tests care about three
// things: the log tells the truth about what happened, the process lands in
// the notebook's root, and a run that won't stop can be stopped.

// nbFixture creates a notebook and returns a helper set for driving it.
type nbFixture struct {
	srv  *Server
	st   *notebookStore
	base string
	root string
}

func newNBFixture(t *testing.T) *nbFixture {
	t.Helper()
	withTempNotebooks(t)
	root := t.TempDir()
	st, err := createNotebook("Runner", root)
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	return &nbFixture{srv: testServer(), st: st, base: "/api/nb/" + st.slug, root: root}
}

func (f *nbFixture) addCell(t *testing.T, typ, source string) string {
	t.Helper()
	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells", map[string]any{"type": typ, "source": source})
	if rec.Code != http.StatusOK {
		t.Fatalf("insert cell: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &out)
	return out.CellID
}

// waitForState polls the folded document until the cell leaves the running
// state. Execution is asynchronous by design — the HTTP call starts a run,
// it does not wait for one.
func (f *nbFixture) waitForState(t *testing.T, cellID string, timeout time.Duration) Cell {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		doc := f.st.Doc()
		if i := indexOfCell(doc, cellID); i >= 0 {
			c := doc.Cells[i]
			if c.State != CellRunning && c.State != CellQueued && c.State != CellIdle {
				return c
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("cell %s did not finish within %s (state=%v)", cellID, timeout, f.stateOf(cellID))
	return Cell{}
}

func (f *nbFixture) stateOf(cellID string) CellState {
	doc := f.st.Doc()
	if i := indexOfCell(doc, cellID); i >= 0 {
		return doc.Cells[i].State
	}
	return ""
}

// dialWSRaw opens a websocket without consuming anything, so a caller can
// inspect the opening fold itself.
func (f *nbFixture) dialWSRaw(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	hs := httptest.NewServer(f.srv.Router())
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/notebook/" + f.st.slug + "?token=test-token"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		hs.Close()
		t.Fatalf("dial: %v", err)
	}
	return conn, func() { conn.Close(); hs.Close() }
}

// dialWS opens a websocket onto this notebook and consumes the opening
// fold, leaving the connection positioned on live traffic.
func (f *nbFixture) dialWS(t *testing.T) (*websocket.Conn, func()) {
	t.Helper()
	hs := httptest.NewServer(f.srv.Router())
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/notebook/" + f.st.slug + "?token=test-token"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		hs.Close()
		t.Fatalf("dial: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var fold struct {
		Type string `json:"type"`
	}
	if err := conn.ReadJSON(&fold); err != nil || fold.Type != "fold" {
		conn.Close()
		hs.Close()
		t.Fatalf("expected an opening fold, got %+v (err %v)", fold, err)
	}
	return conn, func() { conn.Close(); hs.Close() }
}

// readUntilDelta blocks until a delta for cellID containing want arrives.
func (f *nbFixture) readUntilDelta(t *testing.T, conn *websocket.Conn, cellID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var msg struct {
			Type   string `json:"type"`
			CellID string `json:"cellId"`
			Text   string `json:"text"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read: %v", err)
		}
		if msg.Type == "delta" && msg.CellID == cellID && strings.Contains(msg.Text, want) {
			return
		}
	}
	t.Fatalf("no delta for cell %s containing %q within %s", cellID, want, timeout)
}

func (f *nbFixture) outputText(c Cell) string {
	var b strings.Builder
	for _, o := range c.Outputs {
		b.WriteString(o.Text)
	}
	return b.String()
}

func TestRunShellCell_CapturesStdoutAndFinishesOK(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo hello-notebook")

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}

	c := f.waitForState(t, cell, 10*time.Second)
	if c.State != CellOK {
		t.Fatalf("State = %q, want %q (output: %q)", c.State, CellOK, f.outputText(c))
	}
	if got := f.outputText(c); !strings.Contains(got, "hello-notebook") {
		t.Errorf("output = %q, want it to contain hello-notebook", got)
	}
	if c.Duration <= 0 {
		t.Errorf("Duration = %v, want > 0", c.Duration)
	}
}

func TestRunShellCell_CapturesStderr(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo to-stderr 1>&2")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)
	if got := f.outputText(c); !strings.Contains(got, "to-stderr") {
		t.Errorf("output = %q, want stderr captured", got)
	}
}

func TestRunShellCell_NonZeroExitIsAnError(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo before-failing; exit 3")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)
	if c.State != CellError {
		t.Fatalf("State = %q, want %q", c.State, CellError)
	}
	// Output produced before the failure must still be kept.
	if got := f.outputText(c); !strings.Contains(got, "before-failing") {
		t.Errorf("output = %q, want output produced before the failure", got)
	}
}

// The notebook's root is the containment boundary every tool in M3 will be
// checked against. It starts here: a shell cell runs in it.
func TestRunShellCell_RunsInTheNotebookRoot(t *testing.T) {
	f := newNBFixture(t)
	marker := filepath.Join(f.root, "marker.txt")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	cell := f.addCell(t, "shell", "ls marker.txt")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)
	if c.State != CellOK {
		t.Fatalf("State = %q, want %q — the command did not run in the notebook root (output %q)",
			c.State, CellOK, f.outputText(c))
	}
}

func TestRunCell_MarkdownIsNotExecutable(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "markdown", "# not a program")

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("run markdown: got %d, want 400 — prose is not executed", rec.Code)
	}
}

// #50 replaced M1's blanket 501 on prompt and file cells. They are now two
// different things and the responses say so: a prompt cell is runnable and
// only lacks a configured provider, while a file cell is context that is
// read during projection and has nothing of its own to run.
func TestRunCell_PromptNeedsAProviderAndFileIsNotRunnable(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, nil)

	prompt := f.addCell(t, "prompt", "ask something")
	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+prompt+"/run", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("run prompt with no provider: got %d, want 503", rec.Code)
	}

	file := f.addCell(t, "file", "notes.txt")
	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+file+"/run", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("run file: got %d, want 400 — a file cell is projected, not run", rec.Code)
	}
}

func TestRunCell_RefusesASecondConcurrentRun(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "sleep 5")

	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil); rec.Code >= 300 {
		t.Fatalf("first run: %d %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil) })

	// Wait until it is genuinely running before racing it.
	deadline := time.Now().Add(5 * time.Second)
	for f.stateOf(cell) != CellRunning && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second run: got %d, want 409", rec.Code)
	}
}

func TestInterruptCell_StopsALongRunningCommand(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo starting; sleep 30")

	// Watch the stream so we can interrupt at a defined moment. Polling for
	// state "running" is not that moment: the cell reports running as soon
	// as run_started is logged, which is before the command has produced
	// anything, so the kill would race the echo.
	conn, closeWS := f.dialWS(t)
	defer closeWS()

	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil); rec.Code >= 300 {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	f.readUntilDelta(t, conn, cell, "starting", 10*time.Second)

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil)
	if rec.Code >= 300 {
		t.Fatalf("interrupt: %d %s", rec.Code, rec.Body.String())
	}

	// A sleep 30 that is still running after this would fail the test by
	// timeout, which is the point: the kill has to reach the process.
	c := f.waitForState(t, cell, 10*time.Second)
	if c.State != CellInterrupted {
		t.Fatalf("State = %q, want %q", c.State, CellInterrupted)
	}
	// Whatever it managed to produce before the kill is kept.
	if got := f.outputText(c); !strings.Contains(got, "starting") {
		t.Errorf("output = %q, want output captured before the interrupt", got)
	}
}

// Output has to reach watchers while the command is still running — a cell
// that only shows its output after it finishes is a worse terminal.
func TestRunShellCell_StreamsDeltasWhileRunning(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo streamed-now; sleep 30")

	conn, closeWS := f.dialWS(t)
	defer closeWS()
	t.Cleanup(func() { nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil) })

	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil); rec.Code >= 300 {
		t.Fatalf("run: %d", rec.Code)
	}
	// The command is still sleeping, so a delta arriving now can only have
	// come from streaming rather than from the finalised output.
	f.readUntilDelta(t, conn, cell, "streamed-now", 10*time.Second)
	if got := f.stateOf(cell); got != CellRunning {
		t.Errorf("state = %q while streaming, want %q", got, CellRunning)
	}
}

func TestInterruptCell_IdleCellIsAConflict(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo hi")
	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("interrupt idle cell: got %d, want 409", rec.Code)
	}
}

// Streaming deltas are a live-view concern. Persisting them would turn a
// chatty run into a hundred-megabyte document, so the log must contain the
// finalised output only.
func TestRunShellCell_DeltasAreNeverPersisted(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo one; echo two; echo three")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	lines := readLines(t, filepath.Join(nbDirFn(), f.st.slug+".jsonl"))
	appended := 0
	for _, ln := range lines {
		var e Event
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("bad log line: %v", err)
		}
		if strings.Contains(e.Type, "delta") {
			t.Fatalf("found a delta event in the log: %s", ln)
		}
		if e.Type == evOutputAppended {
			appended++
		}
		if e.Type == evRunStarted || e.Type == evRunFinished {
			continue
		}
	}
	if appended != 1 {
		t.Errorf("output_appended events = %d, want exactly 1 (one finalised output per run)", appended)
	}
}

// A re-run must not stack the previous run's output — the fold clears on
// run_started, and this proves the runner relies on that rather than
// appending to whatever was there.
func TestRunShellCell_RerunReplacesOutput(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo first-run")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	if rec := nbRequest(t, f.srv, http.MethodPatch, f.base+"/cells/"+cell, map[string]any{"source": "echo second-run"}); rec.Code != http.StatusOK {
		t.Fatalf("edit: %d", rec.Code)
	}
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	got := f.outputText(c)
	if strings.Contains(got, "first-run") {
		t.Errorf("output = %q, want the previous run's output replaced", got)
	}
	if !strings.Contains(got, "second-run") {
		t.Errorf("output = %q, want the new run's output", got)
	}
}
