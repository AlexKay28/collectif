package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// #57 — prompts sent from the notebook must not be swallowed by a dialog.
//
// A CLI is not always at a prompt. It puts up modal dialogs — trust this
// folder, "Set up auto mode", MCP authentication — and while one is up,
// whatever is written to the PTY answers *the dialog*. Two real prompts
// were lost to one while building P1, and the first advanced it a screen.
// A prompt beginning with "1" would select option 1 of a permission
// request.
//
// The fix is two layers, because neither alone is enough.
//
//  1. A gate on what we can see before sending. `Session.Pending` covered
//     permission prompts only; `MenuOptions` was already being computed by
//     the menu detector on every tick and simply never consulted. That one
//     wire catches the numbered dialogs, auto-mode included.
//
//  2. A check that the text actually landed in the input box. This is the
//     layer that matters, because it tests the property we care about
//     rather than pattern-matching the dialogs we happen to know about —
//     it works for a dialog nobody has seen yet, and for a CLI nobody has
//     written a detector for.

func gateFixture(t *testing.T, id string) (*Session, *notebookStore, *Server, string) {
	t.Helper()
	s, _ := ptySession(t, id)
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), adapters["claude"].Capabilities())
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	p := newSessionProjector(st)
	s.mu.Lock()
	s.nb, s.projector = st, p
	s.mu.Unlock()

	srv := testServer()
	base := "/api/nb/" + st.slug
	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "what is the status?"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)
	return s, st, srv, base + "/cells/" + ins.CellID
}

// ─── Layer 1: the gate ──────────────────────────────────────────────────

func TestSendGate_RefusesWhileAMenuIsOnScreen(t *testing.T) {
	s, _, srv, cell := gateFixture(t, "agent-menu")

	// Exactly what the auto-mode dialog looks like, and what the menu
	// detector has always reported and nothing ever read.
	s.setMenuOptions([]MenuOption{
		{Key: "1", Label: "Set it up"},
		{Key: "2", Label: "Not now"},
		{Key: "3", Label: "Don't show again"},
	})

	rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("send while a menu is up = %d, want 409 — this prompt would have answered the dialog: %s",
			rec.Code, rec.Body.String())
	}
	// The refusal has to name what is in the way, or it is indistinguishable
	// from the agent being busy.
	body := strings.ToLower(rec.Body.String())
	if !strings.Contains(body, "set it up") {
		t.Errorf("the refusal does not say what is on screen: %q", rec.Body.String())
	}
}

func TestSendGate_AllowsSendingWhenTheMenuClears(t *testing.T) {
	s, _, srv, cell := gateFixture(t, "agent-cleared")

	s.setMenuOptions([]MenuOption{{Key: "1", Label: "Yes"}, {Key: "2", Label: "No"}})
	if rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil); rec.Code != http.StatusConflict {
		t.Fatalf("expected the gate to hold, got %d", rec.Code)
	}

	s.setMenuOptions(nil)
	if rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil); rec.Code != http.StatusOK {
		t.Fatalf("send after the dialog cleared = %d, want 200: %s", rec.Code, rec.Body.String())
	}
}

// ─── Layer 2: did it land? ──────────────────────────────────────────────

// The CLI echoes what you type into its input box, so the prompt appears
// in the PTY output within moments. When it does not, the text went
// somewhere else — which is the whole failure this issue is about, and the
// only signal that works for a dialog we have never seen.
func TestSendVerify_ReportsAPromptThatNeverReachedTheInputBox(t *testing.T) {
	s, st, srv, cell := gateFixture(t, "agent-swallowed")

	// What actually happened when this was found live: the prompt went into
	// the dialog, and the dialog *advanced a screen*. So output does arrive
	// — it is simply not the prompt. That is the signature to detect, and
	// it is why "no new output" cannot be the test.
	go func() {
		time.Sleep(120 * time.Millisecond)
		s.writeRing([]byte("\x1b[2J How you use Claude here \r\n Also scan shell history [ ] \r\n"))
		time.Sleep(120 * time.Millisecond)
		s.writeRing([]byte("\x1b[2J Enter to continue  Esc to cancel \r\n"))
	}()

	rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}

	c := waitForCellState(t, st, 5*time.Second)
	if c.State != CellError {
		t.Fatalf("state = %q, want error — the prompt never appeared on screen and nothing said so", c.State)
	}
	var said string
	for _, o := range c.Outputs {
		said += o.Text
	}
	if !strings.Contains(strings.ToLower(said), "terminal") {
		t.Errorf("the failure does not point anywhere useful: %q", said)
	}
}

// The happy path: the CLI echoes the prompt, so nothing is reported and
// the cell waits for its turn to come back as normal.
func TestSendVerify_StaysQuietWhenThePromptIsEchoed(t *testing.T) {
	s, st, srv, cell := gateFixture(t, "agent-echoed")

	// Echo arrives right after the write, as a terminal input box does.
	go func() {
		time.Sleep(150 * time.Millisecond)
		s.writeRing([]byte("\x1b[38;5;7m> what is the status?\x1b[0m\r\n"))
	}()

	if rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil); rec.Code != http.StatusOK {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}

	time.Sleep(2 * time.Second)
	c := st.Doc().Cells[0]
	if c.State != CellRunning {
		t.Errorf("state = %q, want running — an echoed prompt was reported as lost", c.State)
	}
	for _, o := range c.Outputs {
		if o.Type == OutputError {
			t.Errorf("an error was recorded for a prompt that arrived: %q", o.Text)
		}
	}
}

// The echo is wrapped, re-flowed and interleaved with escape codes, so the
// check has to survive normalisation rather than demand a literal match.
func TestSendVerify_ToleratesTheWayTerminalsRedrawText(t *testing.T) {
	s, st, srv, cell := gateFixture(t, "agent-wrapped")

	go func() {
		time.Sleep(150 * time.Millisecond)
		// Wrapped mid-phrase across a line boundary, with cursor moves.
		s.writeRing([]byte("\x1b[1G> what is \x1b[K\r\n\x1b[2Gthe status?\x1b[0m"))
	}()

	if rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil); rec.Code != http.StatusOK {
		t.Fatalf("run = %d", rec.Code)
	}
	time.Sleep(2 * time.Second)
	if got := st.Doc().Cells[0].State; got != CellRunning {
		t.Errorf("state = %q — a wrapped echo was mistaken for a lost prompt", got)
	}
}

// A CLI that cannot be verified must not be punished for it. Verification
// is a check on an assumption, and where the assumption does not hold the
// honest thing is to skip the check rather than fail every send.
func TestSendVerify_IsSkippedWhenThereIsNoOutputToCheck(t *testing.T) {
	_, st, srv, cell := gateFixture(t, "agent-silent")

	// Nothing is ever written to the ring — as for a CLI that does not
	// echo, or one whose output we are not reading.
	if rec := nbRequest(t, srv, http.MethodPost, cell+"/run", nil); rec.Code != http.StatusOK {
		t.Fatalf("run = %d", rec.Code)
	}
	time.Sleep(2 * time.Second)
	if got := st.Doc().Cells[0].State; got == CellError {
		t.Error("a silent terminal was reported as having swallowed the prompt")
	}
}

func waitForCellState(t *testing.T, st *notebookStore, timeout time.Duration) Cell {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c := st.Doc().Cells[0]
		if c.State != CellRunning {
			return c
		}
		if time.Now().After(deadline) {
			return c
		}
		time.Sleep(30 * time.Millisecond)
	}
}
