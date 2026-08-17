package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #47 P0 slice C — the wiring, and the phase's exit criterion.
//
// Slices A and B are pure: a parser and a fold, both testable with strings.
// This is the part that has to survive a real session — a file another
// process is appending to, a server that restarts, and an adapter that
// cannot do any of it.

func TestSessionNotebook_SlugIsDerivedFromTheSessionID(t *testing.T) {
	// Deterministic, because a restart must reopen the same document
	// rather than start a second one beside it. createNotebook's
	// uniquifying slug is exactly wrong for this.
	a := sessionNotebookSlug("agent-abc123")
	b := sessionNotebookSlug("agent-abc123")
	if a != b {
		t.Fatalf("slug is not deterministic: %q then %q", a, b)
	}
	if a == sessionNotebookSlug("agent-def456") {
		t.Fatal("two sessions collide on one notebook")
	}
	if !validNotebookSlug(a) {
		t.Errorf("slug %q is not a valid notebook id, so the notebook can never be opened", a)
	}
	// Session ids are not guaranteed to be slug-shaped; the mapping has to
	// survive whatever they are.
	for _, id := range []string{"Agent-With-CAPS", "agent.with.dots", "a/../../etc/passwd", strings.Repeat("x", 200), ""} {
		if s := sessionNotebookSlug(id); !validNotebookSlug(s) {
			t.Errorf("session id %q produced invalid slug %q", id, s)
		}
	}
}

func TestSessionNotebook_ReopensTheSameDocument(t *testing.T) {
	withTempNotebooks(t)
	root := t.TempDir()

	st1, err := openSessionNotebook("agent-reopen", "claude", root, Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := newSessionProjector(st1)
	p.Ingest([]TranscriptPart{part(PartUserText, "the first prompt", "l1")})

	st2, err := openSessionNotebook("agent-reopen", "claude", root, Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	cells := st2.Doc().Cells
	if len(cells) != 1 || cells[0].Source != "the first prompt" {
		t.Fatalf("reopening produced a different document: %+v", cells)
	}
	if st1.slug != st2.slug {
		t.Errorf("slugs differ: %q vs %q", st1.slug, st2.slug)
	}
}

func TestSessionNotebook_RootIsTheSessionsWorkingDirectory(t *testing.T) {
	withTempNotebooks(t)
	root := t.TempDir()

	st, err := openSessionNotebook("agent-root", "claude", root, Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Shell cells the user adds to a session's notebook run somewhere.
	// Anywhere but the agent's own cwd would be a surprise with a
	// filesystem attached to it.
	if got := st.Doc().Root; got != root {
		t.Errorf("root = %q, want the session's cwd %q", got, root)
	}
}

// ADR 0002 D11. codex and opencode cannot project content yet. The
// notebook still opens — a session should not be unreachable because its
// CLI is less instrumented — but it must say what is missing rather than
// presenting an empty document that reads as "the agent did nothing".
func TestSessionNotebook_DegradedAdapterExplainsItself(t *testing.T) {
	withTempNotebooks(t)

	st, err := openSessionNotebook("agent-degraded", "codex", t.TempDir(), Capabilities{TranscriptContent: false})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	cells := st.Doc().Cells
	if len(cells) == 0 {
		t.Fatal("a degraded session's notebook is empty — indistinguishable from an agent that did nothing")
	}
	if cells[0].Type != CellMarkdown {
		t.Errorf("the explanation is type %q, want markdown", cells[0].Type)
	}
	if !strings.Contains(strings.ToLower(cells[0].Source), "codex") {
		t.Errorf("the note does not name the CLI whose support is missing: %q", cells[0].Source)
	}

	// And it must not be repeated on every reopen.
	st2, err := openSessionNotebook("agent-degraded", "codex", t.TempDir(), Capabilities{TranscriptContent: false})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if n := len(st2.Doc().Cells); n != len(cells) {
		t.Errorf("reopening added %d cells — the note is being appended every time", n-len(cells))
	}
}

// ─── The exit criterion ─────────────────────────────────────────────────

// P0 exists to answer one question end to end: can you read a running
// session's turns as cells? This writes a transcript the way Claude Code
// does — appended to, line by line — and asserts the document that comes
// out the other side is the conversation.
func TestSessionNotebook_TranscriptBecomesADocumentWhileTheSessionRuns(t *testing.T) {
	withTempNotebooks(t)
	s := newTestSession(t, "agent-live", "sid-live")

	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(tpath, nil, 0o644); err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	s.mu.Lock()
	s.TranscriptPath = tpath
	s.Cwd = t.TempDir()
	s.mu.Unlock()

	startTranscriptWatcher(s.ctx, s)

	// One turn, as Claude Code writes it: the prompt, a tool call, its
	// result, then the answer — four separate appends.
	appendLine(t, tpath, `{"type":"user","uuid":"w1","timestamp":"2026-08-17T10:00:00.000Z",
	  "origin":{"kind":"human"},"message":{"role":"user","content":"how many issues are open?"}}`)
	appendLine(t, tpath, `{"type":"assistant","uuid":"w2","timestamp":"2026-08-17T10:00:01.000Z",
	  "message":{"role":"assistant","model":"claude-opus-5","content":[
	    {"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"gh issue list"}}],
	    "usage":{"input_tokens":10,"output_tokens":5}}}`)
	appendLine(t, tpath, `{"type":"user","uuid":"w3","timestamp":"2026-08-17T10:00:02.000Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"tu1","content":"12 open"}]}}`)
	appendLine(t, tpath, `{"type":"assistant","uuid":"w4","timestamp":"2026-08-17T10:00:03.000Z",
	  "message":{"role":"assistant","model":"claude-opus-5","content":[
	    {"type":"text","text":"There are 12 open issues."}],
	    "usage":{"input_tokens":20,"output_tokens":8}}}`)

	st := waitForSessionCells(t, s, 1, 3, 5*time.Second)
	cells := st.Doc().Cells

	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — one prompt is one turn: %+v", len(cells), cells)
	}
	c := cells[0]
	if c.Source != "how many issues are open?" {
		t.Errorf("cell source = %q", c.Source)
	}
	if c.Meta.Provenance != ProvenanceMirrored {
		t.Errorf("provenance = %q, want mirrored", c.Meta.Provenance)
	}
	if len(c.Outputs) != 3 {
		t.Fatalf("got %d outputs, want 3 (call, result, text): %+v", len(c.Outputs), c.Outputs)
	}
	if c.Outputs[0].Data["name"] != "Bash" {
		t.Errorf("tool call: %+v", c.Outputs[0].Data)
	}
	if !strings.Contains(c.Outputs[2].Text, "12 open issues") {
		t.Errorf("answer: %q", c.Outputs[2].Text)
	}

	// The usage path must keep working — projection is additional, not a
	// replacement, and breaking the counters would regress #42.
	s.mu.Lock()
	msgs := s.MessageCount
	s.mu.Unlock()
	if msgs != 2 {
		t.Errorf("MessageCount = %d, want 2 — projection broke the usage watcher", msgs)
	}
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	defer f.Close()
	// Collapse the test's indentation so the file holds one JSON object
	// per line, exactly as the CLI writes it.
	if _, err := f.WriteString(strings.Join(strings.Fields(line), " ") + "\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func waitForSessionCells(t *testing.T, s *Session, wantCells, wantOutputs int, timeout time.Duration) *notebookStore {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		st := s.nb
		s.mu.Unlock()
		if st != nil {
			doc := st.Doc()
			if len(doc.Cells) >= wantCells && len(doc.Cells[len(doc.Cells)-1].Outputs) >= wantOutputs {
				return st
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.mu.Lock()
	st := s.nb
	s.mu.Unlock()
	if st == nil {
		t.Fatal("the session never opened a notebook")
	}
	return st
}

// The sidebar has to be able to link a session to its document, so the id
// has to be on the wire. Without it the notebook exists and is unreachable,
// which is the same as not existing.
func TestSessionJSON_CarriesTheNotebookIDOnceOneExists(t *testing.T) {
	withTempNotebooks(t)
	s := newTestSession(t, "agent-wire", "sid-wire")

	if got := s.toJSON()["notebook"]; got != "" {
		t.Errorf("notebook = %v before one is opened, want empty — a link to a document that "+
			"does not exist is a 404 with extra steps", got)
	}

	st, err := openSessionNotebook(s.ID, "claude", t.TempDir(), Capabilities{TranscriptContent: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.mu.Lock()
	s.nb = st
	s.mu.Unlock()

	if got := s.toJSON()["notebook"]; got != st.slug {
		t.Errorf("notebook = %v, want %q", got, st.slug)
	}
}
