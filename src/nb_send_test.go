package main

import (
	"bufio"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// #47 P1 slice A — driving the session from the document.
//
// P0 made a session readable. This makes it usable: a prompt cell you
// author in a session's notebook goes to the agent, and the turn it causes
// comes back as that same cell's output.
//
// The design problem is the round trip. You type a prompt; the CLI writes
// that prompt to its transcript; the projector reads it back. Left alone
// the document shows your prompt twice — once as the cell you wrote and
// once as the cell it mirrored. Adoption is the answer: the mirrored turn
// attaches to the cell you already have rather than opening a new one.

// ptySession gives a session a pipe standing in for its PTY, and returns a
// reader for whatever gets written to it.
func ptySession(t *testing.T, id string) (*Session, *bufio.Reader) {
	t.Helper()
	s := newTestSession(t, id, "sid-"+id)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { pr.Close(); pw.Close() })
	s.mu.Lock()
	s.PTY = pw
	s.mu.Unlock()
	return s, bufio.NewReader(pr)
}

func readWithin(t *testing.T, r *bufio.Reader, d time.Duration) string {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := r.ReadString('\r')
		ch <- res{line, err}
	}()
	select {
	case got := <-ch:
		if got.err != nil {
			t.Fatalf("read from pty: %v", got.err)
		}
		return got.s
	case <-time.After(d):
		t.Fatal("nothing was written to the pty — the prompt never reached the agent")
		return ""
	}
}

// ─── Sending ────────────────────────────────────────────────────────────

func TestSessionRun_PromptCellIsWrittenToThePTY(t *testing.T) {
	s, out := ptySession(t, "agent-send")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	srv := testServer()
	base := "/api/nb/" + st.slug

	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "what changed today?"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}

	got := readWithin(t, out, 2*time.Second)
	if !strings.Contains(got, "what changed today?") {
		t.Errorf("pty received %q, want the prompt", got)
	}
	// The trailing carriage return is what submits it. Without it the text
	// sits in the CLI's input box and nothing happens — which would look
	// exactly like a hung agent.
	if !strings.HasSuffix(got, "\r") {
		t.Errorf("pty received %q with no submit", got)
	}
}

// A session notebook has no provider of its own and must not be judged by
// one: the agent is the CLI. Before this, running a prompt in a session
// notebook on a machine with no API key returned 503.
func TestSessionRun_DoesNotRequireAModelProvider(t *testing.T) {
	prev := activeProvider
	activeProvider = nil
	t.Cleanup(func() { activeProvider = prev })

	s, out := ptySession(t, "agent-noprov")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	srv := testServer()
	base := "/api/nb/" + st.slug

	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "hello"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d: %s — a session notebook is driven by its CLI, not by our provider",
			rec.Code, rec.Body.String())
	}
	readWithin(t, out, 2*time.Second)
}

// A detached notebook must keep going through the native loop. The two
// backends are the whole of D10, and the dispatch is the only thing
// keeping them apart.
func TestDetachedRun_StillUsesTheNativeLoop(t *testing.T) {
	f := newNBFixture(t)
	fp := &fakeProvider{turns: []scriptedTurn{{text: "from the loop"}}}
	withProvider(t, fp)

	cell := f.addCell(t, "prompt", "ask the model")
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	if len(fp.sent()) != 1 {
		t.Errorf("the native provider was called %d times, want 1", len(fp.sent()))
	}
}

// A session whose process is gone cannot be driven, and saying so on the
// cell is the difference between a clear failure and a cell that sits at
// "running" until you reload.
func TestSessionRun_ReportsADeadSession(t *testing.T) {
	withTempNotebooks(t)
	st, err := openSessionNotebook("agent-ghost", "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	srv := testServer()
	base := "/api/nb/" + st.slug

	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "anyone there?"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("run against a dead session = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "session") {
		t.Errorf("the error does not mention the session: %q", rec.Body.String())
	}
}

// ─── Adoption ───────────────────────────────────────────────────────────

func TestProjector_AdoptsTheCellYouTyped(t *testing.T) {
	st, p := newProjectorFixture(t)

	// The cell as the run path leaves it: authored, running, awaiting its
	// own reflection.
	cell := Cell{ID: "c1", Type: CellPrompt, Source: "run the tests", State: CellRunning}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: cell}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	p.AwaitAdoption("c1", "run the tests")

	p.Ingest([]TranscriptPart{part(PartUserText, "run the tests", "l1")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "All 214 pass.", "l2")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — the prompt was shown twice: %+v", len(cells), cells)
	}
	c := cells[0]
	if c.ID != "c1" {
		t.Errorf("a new cell replaced the authored one: %q", c.ID)
	}
	// It is a mirrored cell now: the CLI owns the turn, so the read-only
	// rules apply to it exactly as they do to any other projected cell.
	if c.Meta.Provenance != ProvenanceMirrored {
		t.Errorf("provenance = %q, want mirrored after adoption", c.Meta.Provenance)
	}
	if c.Meta.SourceUUID != "l1" {
		t.Errorf("sourceUuid = %q, want l1 — without it a restart re-inserts the turn", c.Meta.SourceUUID)
	}
	if len(c.Outputs) != 1 || c.Outputs[0].Text != "All 214 pass." {
		t.Errorf("the answer did not land on the adopted cell: %+v", c.Outputs)
	}
}

// If the next prompt is not the one we sent — you typed into the terminal
// instead, or the send never landed — the authored cell must not be left
// spinning, and the real prompt must still appear.
func TestProjector_UnmatchedAdoptionSettlesTheAuthoredCell(t *testing.T) {
	st, p := newProjectorFixture(t)

	cell := Cell{ID: "c1", Type: CellPrompt, Source: "the one I sent", State: CellRunning}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: cell}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	p.AwaitAdoption("c1", "the one I sent")

	p.Ingest([]TranscriptPart{part(PartUserText, "something else entirely", "l1")})

	cells := st.Doc().Cells
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2: %+v", len(cells), cells)
	}
	if cells[0].State == CellRunning {
		t.Error("the authored cell is still running — it will spin until the page is reloaded")
	}
	if cells[1].Source != "something else entirely" {
		t.Errorf("the real prompt was lost: %q", cells[1].Source)
	}
}

// Adoption is a one-shot. A later identical prompt is a new turn, not a
// second chance to adopt the same cell.
func TestProjector_AdoptionAppliesOnlyOnce(t *testing.T) {
	st, p := newProjectorFixture(t)

	cell := Cell{ID: "c1", Type: CellPrompt, Source: "again", State: CellRunning}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: cell}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	p.AwaitAdoption("c1", "again")

	p.Ingest([]TranscriptPart{part(PartUserText, "again", "l1")})
	p.Ingest([]TranscriptPart{part(PartUserText, "again", "l2")})

	if cells := st.Doc().Cells; len(cells) != 2 {
		t.Fatalf("got %d cells, want 2 — the second turn was swallowed: %+v", len(cells), cells)
	}
}

// ─── Interrupt ──────────────────────────────────────────────────────────

// Stopping a mirrored turn means stopping the agent, which is Escape on
// the PTY. Without this the notebook's stop button works only for the
// native loop and silently does nothing on a session.
func TestSessionRun_InterruptSendsEscape(t *testing.T) {
	s, out := ptySession(t, "agent-stop")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	srv := testServer()
	base := "/api/nb/" + st.slug

	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "a long job"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)
	nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	readWithin(t, out, 2*time.Second)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/interrupt", nil)
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("interrupt = %d: %s", rec.Code, rec.Body.String())
	}

	buf := make([]byte, 8)
	done := make(chan int, 1)
	go func() { n, _ := out.Read(buf); done <- n }()
	select {
	case n := <-done:
		if n == 0 || buf[0] != 0x1b {
			t.Errorf("pty received %q, want an escape", string(buf[:n]))
		}
	case <-time.After(2 * time.Second):
		t.Error("interrupt sent nothing to the pty — the stop button is decorative on a session")
	}
}

// ─── Not sending ────────────────────────────────────────────────────────
//
// Found by driving a real session. A CLI is not always at a prompt: it
// puts up modal dialogs — trust-this-folder, set-up-auto-mode, and every
// permission request — and while one is up, whatever you write to the PTY
// answers *the dialog*. My first live send went into "Set up auto mode for
// your environment?" and advanced it a screen.
//
// That is not merely ineffective, it is dangerous: a prompt whose first
// character is "1" would select option 1 of a permission request. Sending
// blind is the wrong default even when it usually works.

func TestSessionRun_RefusesWhileTheAgentIsWaitingOnAnAnswer(t *testing.T) {
	s, _ := ptySession(t, "agent-modal")
	s.setPending("Claude wants to run: rm -rf /")

	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	srv := testServer()
	base := "/api/nb/" + st.slug

	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "1"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("run while a prompt is pending = %d, want 409 — this prompt would have been "+
			"read as an answer to the dialog: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "waiting") {
		t.Errorf("the refusal does not explain itself: %q", rec.Body.String())
	}
}

// The gate is best-effort: modal detection is regex archaeology on ANSI
// bytes (menu.go) and it does not catch every dialog — the auto-mode one
// it missed is why this test exists. So a prompt that is never mirrored
// back must not leave its cell spinning forever; it has to say that it may
// not have arrived, and point at the terminal.
func TestProjector_UnadoptedPromptGivesUpAndSaysSo(t *testing.T) {
	st, p := newProjectorFixture(t)

	cell := Cell{ID: "c1", Type: CellPrompt, Source: "did this arrive?", State: CellRunning}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: cell}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	p.AwaitAdoptionFor("c1", "did this arrive?", 40*time.Millisecond)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.Doc().Cells[0].State != CellRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	c := st.Doc().Cells[0]
	if c.State == CellRunning {
		t.Fatal("the cell is still running — a prompt that never reached the agent looks identical to one it is working on")
	}
	if c.State != CellError {
		t.Errorf("state = %q, want error", c.State)
	}
	var said string
	for _, o := range c.Outputs {
		if o.Type == OutputError {
			said = o.Text
		}
	}
	if !strings.Contains(strings.ToLower(said), "terminal") {
		t.Errorf("the failure does not point anywhere useful: %q", said)
	}
}

// The timeout must not fire on a prompt that was adopted normally.
func TestProjector_AdoptionCancelsTheTimeout(t *testing.T) {
	st, p := newProjectorFixture(t)

	cell := Cell{ID: "c1", Type: CellPrompt, Source: "arrived fine", State: CellRunning}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: cell}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	p.AwaitAdoptionFor("c1", "arrived fine", 40*time.Millisecond)
	p.Ingest([]TranscriptPart{part(PartUserText, "arrived fine", "l1")})

	time.Sleep(300 * time.Millisecond)

	c := st.Doc().Cells[0]
	if c.State != CellRunning {
		t.Errorf("state = %q — the timeout fired on a prompt that did arrive", c.State)
	}
	for _, o := range c.Outputs {
		if o.Type == OutputError {
			t.Errorf("an error was recorded on a healthy turn: %q", o.Text)
		}
	}
}
