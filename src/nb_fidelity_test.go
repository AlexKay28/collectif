package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// #47 P2 — three CLIs, three fidelities, no lies (ADR 0002 D11).
//
// Neither codex nor opencode is installed on the machine this was written
// on, and there are no rollout files to read. Writing their projectors
// from documentation is exactly the mistake P0 proved matters: every
// hand-written fixture I wrote for Claude Code passed, and running the
// parser over a real transcript found four bugs in an hour. So P2 does not
// pretend to add fidelity. It makes the *absence* of fidelity correct.
//
// Correct turns out to be more than a banner. A CLI collectif cannot
// project is one whose prompts are never echoed back, so slice A's
// adoption timeout marks every prompt you send "this may not have
// arrived" — a lie in the opposite direction from the one D11 was written
// to prevent.

// ─── The capability model ───────────────────────────────────────────────

func TestFidelity_DescribesEachSurfaceSeparately(t *testing.T) {
	// One boolean cannot describe this. A codex session cannot show you
	// its turns but can still be sent prompts and can still surface
	// approvals, because it has hooks. Collapsing that to "degraded" tells
	// the user nothing about what they can actually do.
	f := fidelityOf(adapters["claude"])
	if !f.Turns || !f.Approvals || !f.Send {
		t.Errorf("claude fidelity = %+v, want everything", f)
	}

	c := fidelityOf(adapters["codex"])
	if c.Turns {
		t.Error("codex claims it can project turns — no parser exists for its rollout format")
	}
	if !c.Send {
		t.Error("codex cannot be sent prompts, but every CLI has a terminal")
	}
	if !c.Approvals {
		t.Error("codex has hooks, so its permission prompts do reach the notebook")
	}

	o := fidelityOf(adapters["opencode"])
	if o.Turns || o.Approvals {
		t.Errorf("opencode fidelity = %+v — it has neither a parser nor hooks", o)
	}
	if !o.Send {
		t.Error("opencode cannot be sent prompts")
	}
}

func TestFidelity_AnUnknownAdapterClaimsNothingButSend(t *testing.T) {
	f := fidelityOf(nil)
	if f.Turns || f.Approvals || f.Usage {
		t.Errorf("an unknown adapter claims capabilities it cannot have: %+v", f)
	}
}

// The statement has to reach the browser, and it must be *derived*, not
// stored. What a build can do is a property of the code, not of the
// document — a note frozen into the log would still be claiming "codex
// turns are not shown" long after someone writes the parser.
func TestFidelity_IsServedWithTheDocumentAndNotFoldedFromTheLog(t *testing.T) {
	withTempNotebooks(t)
	st, err := openSessionNotebook("agent-fid", "codex", t.TempDir(), adapters["codex"].Capabilities())
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	srv := testServer()

	rec := nbRequest(t, srv, http.MethodGet, "/api/nb/"+st.slug, nil)
	if rec.Code != 200 {
		t.Fatalf("GET = %d", rec.Code)
	}
	var got Notebook
	decodeJSON(t, rec, &got)

	if got.Fidelity == nil {
		t.Fatal("no fidelity block on a session notebook — the browser cannot say what works here")
	}
	if got.Fidelity.Turns {
		t.Error("a codex notebook claims its turns are projected")
	}
	if got.Fidelity.CLI != "codex" {
		t.Errorf("cli = %q, want codex", got.Fidelity.CLI)
	}

	// And nothing about it is in the log: reopening from disk must
	// recompute it rather than replay a stale claim.
	events, err := st.readLog()
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	for _, e := range events {
		if strings.Contains(string(e.Payload), "fidelity") {
			t.Errorf("fidelity leaked into the event log: %s", e.Payload)
		}
	}
}

// A detached notebook has no CLI and no fidelity question to answer.
func TestFidelity_IsAbsentOnADetachedNotebook(t *testing.T) {
	f := newNBFixture(t)
	rec := nbRequest(t, f.srv, http.MethodGet, f.base, nil)
	var got Notebook
	decodeJSON(t, rec, &got)
	if got.Fidelity != nil {
		t.Errorf("a detached notebook carries a fidelity block: %+v", got.Fidelity)
	}
}

// P0 wrote a one-time markdown cell explaining the gap. That was the wrong
// place — it is permanent, it is in a user document, and it describes the
// build rather than the session. The banner replaces it.
func TestFidelity_NoExplanatoryCellIsWrittenIntoTheDocument(t *testing.T) {
	withTempNotebooks(t)
	st, err := openSessionNotebook("agent-nocell", "codex", t.TempDir(), adapters["codex"].Capabilities())
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	if cells := st.Doc().Cells; len(cells) != 0 {
		t.Errorf("a fresh session notebook has %d cells before the agent has done anything: %+v",
			len(cells), cells)
	}
}

// ─── The bug ────────────────────────────────────────────────────────────

// On a CLI whose turns cannot be projected, nothing is ever echoed back,
// so adoption can never happen and slice A's 20s timeout fires on every
// single prompt. The cell then says "this may not have arrived" about a
// prompt that arrived fine. D11 is about not claiming fidelity we lack;
// this is the same error pointed the other way.
func TestDegradedSend_DoesNotClaimThePromptWentMissing(t *testing.T) {
	s, out := ptySession(t, "agent-degr")
	s.CLI = "opencode"
	st, err := openSessionNotebook(s.ID, "opencode", t.TempDir(), adapters["opencode"].Capabilities())
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
		map[string]any{"type": "prompt", "source": "do the thing"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("run = %d: %s", rec.Code, rec.Body.String())
	}
	readWithin(t, out, 2*time.Second) // it did reach the terminal

	// It must settle promptly rather than after the adoption timeout, and
	// it must not settle as an error.
	deadline := time.Now().Add(3 * time.Second)
	var cell Cell
	for time.Now().Before(deadline) {
		cell = st.Doc().Cells[0]
		if cell.State != CellRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cell.State == CellRunning {
		t.Fatal("the cell is still running — on a CLI we cannot project, nothing will ever settle it")
	}
	if cell.State == CellError {
		t.Errorf("a delivered prompt was marked failed — the adoption timeout fired on a CLI that " +
			"never echoes anything back")
	}
	// And it has to say why there is no answer under it, or an empty cell
	// reads as an agent that ignored you.
	var said string
	for _, o := range cell.Outputs {
		said += o.Text
	}
	if !strings.Contains(strings.ToLower(said), "terminal") {
		t.Errorf("nothing explains where the answer went: %q", said)
	}
}

// The projecting path must be untouched: a claude prompt still waits to be
// adopted rather than settling immediately.
func TestProjectingSend_StillWaitsForItsReflection(t *testing.T) {
	s, out := ptySession(t, "agent-proj")
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
		map[string]any{"type": "prompt", "source": "hello"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)
	nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	readWithin(t, out, 2*time.Second)

	time.Sleep(400 * time.Millisecond)
	if got := st.Doc().Cells[0].State; got != CellRunning {
		t.Errorf("state = %q — a projectable CLI's prompt settled before its turn came back", got)
	}
}
