package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// #47 P1 slice B — approvals in the document.
//
// This started as a rendering feature and became a correctness one. Slice
// A found that a CLI showing a dialog swallows whatever is written to its
// PTY, so an unsurfaced dialog is not merely invisible — it is an obstacle
// that eats input, and two live prompts were lost to one. A notebook that
// cannot show you the question cannot be the surface you work in.
//
// The record is append-only, like everything else in the log: one output
// when the agent asks, another when it is answered. That gives a document
// which says what was asked, what was decided, and by which route — which
// is the audit trail ADR 0001 §4.6 wanted and the PTY could never provide.

func approvalFixture(t *testing.T) (*Session, *notebookStore, *sessionProjector) {
	t.Helper()
	s, _ := ptySession(t, "agent-appr-"+t.Name()[len(t.Name())-4:])
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	p := newSessionProjector(st)
	s.mu.Lock()
	s.nb, s.projector = st, p
	s.mu.Unlock()
	return s, st, p
}

func approvalOutputs(st *notebookStore) []Output {
	var out []Output
	for _, c := range st.Doc().Cells {
		for _, o := range c.Outputs {
			if o.Type == OutputApproval {
				out = append(out, o)
			}
		}
	}
	return out
}

// ─── Recording ──────────────────────────────────────────────────────────

func TestApproval_RequestLandsOnTheTurnThatCausedIt(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "delete the temp files", "l1")})

	s.setPending("Claude wants to run: rm -rf ./tmp")
	recordApprovalRequest(s)

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — an approval belongs to the turn that provoked it: %+v",
			len(cells), cells)
	}
	outs := approvalOutputs(st)
	if len(outs) != 1 {
		t.Fatalf("got %d approval outputs, want 1: %+v", len(outs), cells[0].Outputs)
	}
	if !strings.Contains(outs[0].Text, "rm -rf ./tmp") {
		t.Errorf("the question was not recorded: %q", outs[0].Text)
	}
	if outs[0].Data["approvalId"] == nil || outs[0].Data["approvalId"] == "" {
		t.Error("no approval id — the answer has nothing to attach to")
	}
	if outs[0].Data["resolution"] != nil {
		t.Errorf("a fresh request is already resolved: %v", outs[0].Data)
	}
}

// The hook fires more than once for one prompt on some versions, and the
// sweeper re-reads state on a timer. A document that shows the same
// question four times is worse than one that shows it late.
func TestApproval_TheSameQuestionIsRecordedOnce(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "do it", "l1")})

	s.setPending("Claude wants to run: ls")
	recordApprovalRequest(s)
	recordApprovalRequest(s)
	recordApprovalRequest(s)

	if outs := approvalOutputs(st); len(outs) != 1 {
		t.Errorf("got %d approval outputs for one prompt, want 1", len(outs))
	}
}

// A second, genuinely different question during the same turn is a second
// approval — agents ask repeatedly, and collapsing them would hide one.
func TestApproval_ADifferentQuestionIsANewRecord(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "do several things", "l1")})

	s.setPending("Claude wants to run: ls")
	recordApprovalRequest(s)
	recordApprovalResolution(s, "approved")

	s.setPending("Claude wants to edit: main.go")
	recordApprovalRequest(s)

	if outs := approvalOutputs(st); len(outs) != 3 {
		t.Errorf("got %d approval outputs, want 3 (ask, answer, ask): %+v", len(outs), outs)
	}
}

// ─── Resolving ──────────────────────────────────────────────────────────

func TestApproval_ResolutionIsRecordedAgainstTheQuestion(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "go ahead", "l1")})

	s.setPending("Claude wants to run: git push")
	recordApprovalRequest(s)
	asked := approvalOutputs(st)[0].Data["approvalId"]

	recordApprovalResolution(s, "denied")

	outs := approvalOutputs(st)
	if len(outs) != 2 {
		t.Fatalf("got %d approval outputs, want 2: %+v", len(outs), outs)
	}
	// Pairing by id is what lets the renderer show one resolved widget
	// rather than a question and an unrelated verdict.
	if outs[1].Data["approvalId"] != asked {
		t.Errorf("the answer is not attached to the question: %v vs %v",
			outs[1].Data["approvalId"], asked)
	}
	if outs[1].Data["resolution"] != "denied" {
		t.Errorf("resolution = %v, want denied", outs[1].Data["resolution"])
	}
}

// Nothing to resolve must not write a dangling verdict.
func TestApproval_ResolutionWithoutAQuestionIsANoOp(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "go", "l1")})

	recordApprovalResolution(s, "approved")

	if outs := approvalOutputs(st); len(outs) != 0 {
		t.Errorf("recorded %d resolutions with nothing pending: %+v", len(outs), outs)
	}
}

// An expired prompt is not an approval. The sweeper clears pending after
// 30s; a document that records that as "approved" would be a false audit
// trail, which is worse than no audit trail.
func TestApproval_ExpiryIsRecordedAsItself(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "go", "l1")})

	s.setPending("Claude wants to run: something")
	recordApprovalRequest(s)
	recordApprovalResolution(s, "expired")

	outs := approvalOutputs(st)
	if len(outs) != 2 || outs[1].Data["resolution"] != "expired" {
		t.Fatalf("expiry was not recorded honestly: %+v", outs)
	}
}

// ─── Answering from the notebook ────────────────────────────────────────

func TestApprovalAPI_ApproveReachesTheAgent(t *testing.T) {
	s, out := ptySession(t, "agent-answer")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	p := newSessionProjector(st)
	s.mu.Lock()
	s.nb, s.projector = st, p
	s.mu.Unlock()
	p.Ingest([]TranscriptPart{part(PartUserText, "do the thing", "l1")})
	s.setPending("Claude wants to run: make test")
	recordApprovalRequest(s)

	cellID := st.Doc().Cells[0].ID
	srv := testServer()
	rec := nbRequest(t, srv, http.MethodPost,
		"/api/nb/"+st.slug+"/cells/"+cellID+"/approve", map[string]any{"answer": "approve"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("approve = %d: %s", rec.Code, rec.Body.String())
	}

	got := readWithin(t, out, 2*time.Second)
	if !strings.Contains(strings.ToLower(got), "yes") {
		t.Errorf("pty received %q, want an approval keystroke", got)
	}
}

func TestApprovalAPI_DenyReachesTheAgent(t *testing.T) {
	s, out := ptySession(t, "agent-deny")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	p := newSessionProjector(st)
	s.mu.Lock()
	s.nb, s.projector = st, p
	s.mu.Unlock()
	p.Ingest([]TranscriptPart{part(PartUserText, "do the thing", "l1")})
	s.setPending("Claude wants to run: rm -rf /")
	recordApprovalRequest(s)

	cellID := st.Doc().Cells[0].ID
	srv := testServer()
	rec := nbRequest(t, srv, http.MethodPost,
		"/api/nb/"+st.slug+"/cells/"+cellID+"/approve", map[string]any{"answer": "deny"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("deny = %d: %s", rec.Code, rec.Body.String())
	}
	if got := readWithin(t, out, 2*time.Second); !strings.Contains(strings.ToLower(got), "no") {
		t.Errorf("pty received %q, want a denial", got)
	}
}

// Answering a question the agent is no longer asking would send stray
// keystrokes into whatever it is doing now — the exact hazard slice A
// found, arriving from the other direction.
func TestApprovalAPI_RefusesWhenNothingIsPending(t *testing.T) {
	s, _ := ptySession(t, "agent-nopend")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	p := newSessionProjector(st)
	s.mu.Lock()
	s.nb, s.projector = st, p
	s.mu.Unlock()
	p.Ingest([]TranscriptPart{part(PartUserText, "do the thing", "l1")})

	cellID := st.Doc().Cells[0].ID
	srv := testServer()
	rec := nbRequest(t, srv, http.MethodPost,
		"/api/nb/"+st.slug+"/cells/"+cellID+"/approve", map[string]any{"answer": "approve"})
	if rec.Code != http.StatusConflict {
		t.Errorf("approve with nothing pending = %d, want 409: %s", rec.Code, rec.Body.String())
	}
}

// A detached notebook has no agent to answer.
func TestApprovalAPI_RefusesOnADetachedNotebook(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "prompt", "something")

	rec := nbRequest(t, f.srv, http.MethodPost,
		f.base+"/cells/"+cell+"/approve", map[string]any{"answer": "approve"})
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusConflict {
		t.Errorf("approve on a detached notebook = %d, want a refusal: %s", rec.Code, rec.Body.String())
	}
}

func TestApprovalAPI_RejectsAnUnknownAnswer(t *testing.T) {
	s, _ := ptySession(t, "agent-bogus")
	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("notebook: %v", err)
	}
	p := newSessionProjector(st)
	s.mu.Lock()
	s.nb, s.projector = st, p
	s.mu.Unlock()
	p.Ingest([]TranscriptPart{part(PartUserText, "x", "l1")})
	s.setPending("something")
	recordApprovalRequest(s)

	cellID := st.Doc().Cells[0].ID
	srv := testServer()
	rec := nbRequest(t, srv, http.MethodPost,
		"/api/nb/"+st.slug+"/cells/"+cellID+"/approve", map[string]any{"answer": "maybe"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("answer=maybe = %d, want 400 — an unmodelled answer must not be typed at the agent", rec.Code)
	}
}

// ─── The whole point ────────────────────────────────────────────────────

// Slice A's finding, closed. A pending approval blocks sending, and the
// notebook now shows why rather than leaving you to guess.
func TestApproval_SendingIsBlockedButTheQuestionIsVisible(t *testing.T) {
	s, st, p := approvalFixture(t)
	p.Ingest([]TranscriptPart{part(PartUserText, "start", "l1")})
	s.setPending("Claude wants to run: curl example.com")
	recordApprovalRequest(s)

	srv := testServer()
	base := "/api/nb/" + st.slug
	rec := nbRequest(t, srv, http.MethodPost, base+"/cells",
		map[string]any{"type": "prompt", "source": "never mind, do something else"})
	var ins struct {
		CellID string `json:"cellId"`
	}
	decodeJSON(t, rec, &ins)

	rec = nbRequest(t, srv, http.MethodPost, base+"/cells/"+ins.CellID+"/run", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("send while an approval is up = %d, want 409", rec.Code)
	}
	if outs := approvalOutputs(st); len(outs) != 1 {
		t.Errorf("the blocking question is not in the document: %+v", outs)
	}
}
