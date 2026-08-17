package main

// nb_approval.go — the agent's questions, in the document.
// #47 P1 slice B, per ADR 0002 and ADR 0001 §4.6.
//
// ADR 0001 opened by describing what collectif does when an agent asks
// permission: write `yes\r` into a PTY and, if the pending flag is still
// set 1.5s later, guess again with `1\r`. It called that "an impressive
// amount of engineering spent on not being able to see what the agent is
// doing".
//
// The keystrokes are still how the answer is delivered — the CLI has no
// other input — but the *question* no longer has to be scraped off a
// screen. It arrives through the Notification hook, structured, and this
// file puts it in the notebook where it can be read and answered in place.
//
// Slice A is why this is a correctness feature and not a nicety. A CLI
// showing a dialog reads whatever is written to its PTY as an answer to
// that dialog, so an unsurfaced question is an obstacle that silently eats
// the next prompt. Two live prompts were lost to one before this existed.
//
// The record is append-only, like the rest of the log: one output when the
// agent asks, one when it is resolved, paired by id. The document then
// says what was asked, what was decided, and how — which is the audit
// trail §4.6 wanted and a scrollback could never give.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// approvalKey identifies a question by its content rather than by an id
// the CLI does not give us. The Notification hook fires more than once for
// one prompt on some versions, and the sweeper re-reads state on a timer;
// without this the same question appears four times.
func approvalKey(req *ApprovalRequest) string {
	if req == nil {
		return ""
	}
	return req.Tool + "\x00" + req.Message
}

// recordApprovalRequest writes the agent's question into its notebook.
// Safe to call repeatedly for the same question — the second call is a
// no-op, which matters because the caller is a hook that repeats.
func recordApprovalRequest(s *Session) {
	if s == nil {
		return
	}
	s.mu.Lock()
	req, p := s.Pending, s.projector
	s.mu.Unlock()
	if req == nil || p == nil {
		return
	}
	p.recordApproval(req)
}

// recordApprovalResolution closes the open question.
//
// What this records is the *decision*, not the agent's receipt of it. When
// you press Approve, the keystrokes go out and this writes "approved" —
// but the CLI's only input is a terminal, and there is no acknowledgement
// coming back. If the keystrokes miss, the log says a human approved,
// which is true, and the turn simply does not proceed, which is visible.
//
// What it must never do is invent a decision nobody made. The sweeper
// clears a prompt nobody answered after 30s and that is recorded as
// `expired`, because writing "approved" there would be a false audit
// trail — worse than no audit trail at all.
func recordApprovalResolution(s *Session, how string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	p := s.projector
	s.mu.Unlock()
	if p == nil {
		return
	}
	p.resolveApproval(how)
}

func (p *sessionProjector) recordApproval(req *ApprovalRequest) {
	p.mu.Lock()
	if p.closed || p.approvalKey == approvalKey(req) {
		p.mu.Unlock()
		return
	}
	id := uuid.NewString()
	p.approvalKey, p.approvalID = approvalKey(req), id
	p.mu.Unlock()

	data := map[string]any{"approvalId": id}
	if req.Tool != "" {
		data["tool"] = req.Tool
	}
	if len(req.ToolInput) > 0 {
		data["input"] = req.ToolInput
	}
	p.appendApproval(Output{Type: OutputApproval, Text: req.Message, Data: data})
}

func (p *sessionProjector) resolveApproval(how string) {
	p.mu.Lock()
	if p.closed || p.approvalID == "" {
		p.mu.Unlock()
		return
	}
	id := p.approvalID
	p.approvalKey, p.approvalID = "", ""
	p.mu.Unlock()

	p.appendApproval(Output{
		Type: OutputApproval,
		Data: map[string]any{"approvalId": id, "resolution": how},
	})
}

// appendApproval lands the block on the turn that provoked it, opening an
// honest placeholder if the question arrived before any turn was seen.
func (p *sessionProjector) appendApproval(out Output) {
	p.mu.Lock()
	if p.current == "" && !p.openCell(CellPrompt, "", ProvenanceMirrored, "", "", CellRunning) {
		p.mu.Unlock()
		return
	}
	cellID := p.current
	p.mu.Unlock()

	p.st.Append(evOutputAppended, outputAppendedPayload{ //nolint:errcheck // a lost record is not worth failing a hook
		CellID: cellID,
		Output: out,
	})
}

// ─── Answering ──────────────────────────────────────────────────────────

// errNothingPending is a 409 rather than a 404: the question was real, it
// is simply no longer being asked. Answering it anyway would type stray
// keystrokes into whatever the agent is doing now — slice A's hazard,
// arriving from the other direction.
var errNothingPending = errors.New(
	"the agent is not waiting on an answer any more — it may have timed out, " +
		"or been answered from the terminal")

func handleCellApprove(w http.ResponseWriter, r *http.Request, st *notebookStore, cellID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc := st.Doc()
	sessionID := doc.Meta.SessionID
	if sessionID == "" {
		http.Error(w, "this notebook is not attached to a session — there is no agent to answer",
			http.StatusBadRequest)
		return
	}
	var req struct {
		Answer string `json:"answer"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	// Only the two answers we model. An arbitrary string here would be
	// typed straight at the agent, which is how a permission dialog gets
	// answered by accident.
	var primary, fallback []string
	var resolution string
	switch strings.ToLower(strings.TrimSpace(req.Answer)) {
	case "approve", "yes", "allow":
		primary, fallback, resolution = []string{"yes\r"}, []string{"1\r"}, "approved"
	case "deny", "no", "reject":
		primary, fallback, resolution = []string{"no\r"}, []string{"\x1b"}, "denied"
	default:
		http.Error(w, fmt.Sprintf("unknown answer %q — expected approve or deny", req.Answer),
			http.StatusBadRequest)
		return
	}

	s := getSession(sessionID)
	if s == nil {
		http.Error(w, "session "+sessionID+" is no longer running", http.StatusServiceUnavailable)
		return
	}
	if !s.hasPending() {
		http.Error(w, errNothingPending.Error(), http.StatusConflict)
		return
	}
	pt := s.pty()
	if pt == nil {
		http.Error(w, "session "+sessionID+" has no terminal attached", http.StatusServiceUnavailable)
		return
	}

	// The keystroke dance is the CLI's, not ours: the literal word works
	// for y/n prompts and filters Claude's ink-select menus, and the
	// fallback covers digit-only selects. Kept identical to the dashboard's
	// path so the two surfaces cannot drift into answering differently.
	answerViaPTY(s, primary, fallback)
	recordApprovalResolution(s, resolution)

	writeJSON(w, http.StatusOK, map[string]any{"cellId": cellID, "answer": resolution})
}
