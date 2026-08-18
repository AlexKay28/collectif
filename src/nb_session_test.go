package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// #47 P0 slice B — parts become a document.
//
// Slice A proved the transcript carries the turns. This proves they fold
// into something a person would read: your prompt as the cell, the agent's
// work as its output. The shape is the one Jupyter got right and a chat
// scrollback loses — authored source separated from produced output.
//
// The hard requirements are elsewhere, though. A session's notebook is
// written by a watcher that re-reads a growing file across restarts, so
// idempotence is not a nicety here: a duplicate cell is a document that
// lies about what the agent did.

func newProjectorFixture(t *testing.T) (*notebookStore, *sessionProjector) {
	t.Helper()
	withTempNotebooks(t)
	st, err := createNotebook("Session", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	return st, newSessionProjector(st)
}

func part(kind PartKind, text string, uuid string) TranscriptPart {
	return TranscriptPart{Kind: kind, Text: text, UUID: uuid, At: time.Now()}
}

// ─── The basic fold ─────────────────────────────────────────────────────

func TestProjector_APromptBecomesAMirroredCell(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "read the repo", "l1")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1: %+v", len(cells), cells)
	}
	c := cells[0]
	if c.Type != CellPrompt {
		t.Errorf("type = %q, want prompt", c.Type)
	}
	if c.Source != "read the repo" {
		t.Errorf("source = %q", c.Source)
	}
	// ADR 0002 D9. Without this the UI offers an Edit button on a cell
	// whose context lives inside a process we do not own.
	if c.Meta.Provenance != ProvenanceMirrored {
		t.Errorf("provenance = %q, want mirrored — the UI cannot tell this is read-only", c.Meta.Provenance)
	}
}

func TestProjector_AssistantWorkHangsUnderThePrompt(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "list the issues", "l1")})
	p.Ingest([]TranscriptPart{
		{Kind: PartToolCall, ToolName: "Bash", ToolUseID: "t1", UUID: "l2",
			ToolInput: json.RawMessage(`{"command":"gh issue list"}`)},
	})
	p.Ingest([]TranscriptPart{
		{Kind: PartToolResult, ToolUseID: "t1", Text: "146 OPEN something", UUID: "l3"},
	})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "There are 146 open issues.", "l4")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — the turn's work belongs to the prompt that caused it: %+v",
			len(cells), cells)
	}
	outs := cells[0].Outputs
	want := []OutputType{OutputToolCall, OutputToolResult, OutputText}
	if len(outs) != len(want) {
		t.Fatalf("got %d outputs, want %d: %+v", len(outs), len(want), outs)
	}
	for i, w := range want {
		if outs[i].Type != w {
			t.Errorf("output %d = %q, want %q", i, outs[i].Type, w)
		}
	}
	if got := outs[0].Data["name"]; got != "Bash" {
		t.Errorf("tool call lost its name: %v", outs[0].Data)
	}
	// The pairing id is what lets the renderer nest a result under its call
	// instead of showing two unrelated blocks.
	if outs[0].Data["toolUseId"] != "t1" || outs[1].Data["toolUseId"] != "t1" {
		t.Errorf("call and result are not paired: %v / %v", outs[0].Data, outs[1].Data)
	}
}

func TestProjector_ASecondPromptOpensASecondCell(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "first", "l1")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "answer one", "l2")})
	p.Ingest([]TranscriptPart{part(PartUserText, "second", "l3")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "answer two", "l4")})

	cells := st.Doc().Cells
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2: %+v", len(cells), cells)
	}
	if cells[0].Source != "first" || cells[1].Source != "second" {
		t.Fatalf("prompts out of order: %q then %q", cells[0].Source, cells[1].Source)
	}
	if len(cells[0].Outputs) != 1 || cells[0].Outputs[0].Text != "answer one" {
		t.Errorf("first cell's output leaked or was lost: %+v", cells[0].Outputs)
	}
	// A finished turn must not sit at "running" forever, or the whole
	// document reads as permanently in flight.
	if cells[0].State != CellOK {
		t.Errorf("first cell state = %q, want ok once the next prompt arrived", cells[0].State)
	}
	if cells[1].State != CellRunning {
		t.Errorf("last cell state = %q, want running — the agent is still working on it", cells[1].State)
	}
}

// Attaching to a session already in flight is the normal case, not an edge
// one: you open the browser after the agent has been going for ten minutes.
// The output has to land somewhere, and that somewhere must not pretend to
// be a prompt the user typed.
func TestProjector_OutputBeforeAnyPromptGetsAnHonestCell(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartAssistantText, "…continuing", "l1")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1: %+v", len(cells), cells)
	}
	if cells[0].Source != "" {
		t.Errorf("source = %q — an orphan cell must not invent a prompt", cells[0].Source)
	}
	if cells[0].Meta.Provenance != ProvenanceMirrored {
		t.Errorf("provenance = %q, want mirrored", cells[0].Meta.Provenance)
	}
	if len(cells[0].Outputs) != 1 {
		t.Errorf("the output was dropped: %+v", cells[0].Outputs)
	}
}

// ─── Compaction ─────────────────────────────────────────────────────────

func TestProjector_CompactSummaryIsAMarkdownCell(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "do the thing", "l1")})
	p.Ingest([]TranscriptPart{part(PartCompactSummary, "Summary of the conversation so far…", "l2")})
	p.Ingest([]TranscriptPart{part(PartUserText, "carry on", "l3")})

	cells := st.Doc().Cells
	if len(cells) != 3 {
		t.Fatalf("got %d cells, want 3: %+v", len(cells), cells)
	}
	if cells[1].Type != CellMarkdown {
		t.Errorf("summary cell type = %q, want markdown — it is prose about the session, not an instruction",
			cells[1].Type)
	}
	if cells[1].Meta.Provenance != ProvenanceCompact {
		t.Errorf("provenance = %q, want compact", cells[1].Meta.Provenance)
	}
}

// ─── Subagents ──────────────────────────────────────────────────────────

// M6 renders these nested. Until then they must not be interleaved into the
// parent's document, where they would read as the main agent contradicting
// itself mid-turn.
func TestProjector_SidechainPartsAreNotInterleaved(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "the real prompt", "l1")})
	p.Ingest([]TranscriptPart{{Kind: PartAssistantText, Text: "a subagent's words", UUID: "l2", Sidechain: true}})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "the real answer", "l3")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1: %+v", len(cells), cells)
	}
	if len(cells[0].Outputs) != 1 {
		t.Fatalf("got %d outputs, want 1 — the subagent's turn was interleaved: %+v",
			len(cells[0].Outputs), cells[0].Outputs)
	}
	if cells[0].Outputs[0].Text != "the real answer" {
		t.Errorf("wrong output survived: %q", cells[0].Outputs[0].Text)
	}
}

// ─── Idempotence ────────────────────────────────────────────────────────

func TestProjector_ReingestingTheSameLineChangesNothing(t *testing.T) {
	st, p := newProjectorFixture(t)

	parts := []TranscriptPart{part(PartUserText, "once", "l1")}
	p.Ingest(parts)
	before := st.Doc().Version
	p.Ingest(parts)
	p.Ingest(parts)

	if cells := st.Doc().Cells; len(cells) != 1 {
		t.Fatalf("got %d cells from three ingests of one line: %+v", len(cells), cells)
	}
	if after := st.Doc().Version; after != before {
		t.Errorf("re-ingest appended %d events to the log — the document is derived from that log, "+
			"so writing no-ops corrupts its history", after-before)
	}
}

// The restart case, and the reason idempotence cannot live only in memory.
// collectif dies, comes back, and re-reads the session's transcript from
// the top. A fresh projector over an existing document must recognise its
// own past work.
func TestProjector_RestartDoesNotDuplicateTheDocument(t *testing.T) {
	st, p := newProjectorFixture(t)

	lines := [][]TranscriptPart{
		{part(PartUserText, "first", "l1")},
		{{Kind: PartToolCall, ToolName: "Read", ToolUseID: "t1", UUID: "l2"}},
		{{Kind: PartToolResult, ToolUseID: "t1", Text: "contents", UUID: "l3"}},
		{part(PartAssistantText, "done", "l4")},
	}
	for _, l := range lines {
		p.Ingest(l)
	}
	want := st.Doc()

	// A new projector over the same store, replaying the whole file.
	restarted := newSessionProjector(st)
	for _, l := range lines {
		restarted.Ingest(l)
	}

	got := st.Doc()
	if len(got.Cells) != len(want.Cells) {
		t.Fatalf("restart produced %d cells, want %d — the session's history doubled",
			len(got.Cells), len(want.Cells))
	}
	if len(got.Cells[0].Outputs) != len(want.Cells[0].Outputs) {
		t.Errorf("restart produced %d outputs, want %d",
			len(got.Cells[0].Outputs), len(want.Cells[0].Outputs))
	}
}

// A line with no id cannot be deduplicated, so it must not be *silently*
// duplicated either. Dropping it loses one turn; appending it twice
// corrupts every restart. We drop, and the choice is deliberate.
func TestProjector_PartsWithNoIDAreDropped(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "identified", "l1")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "anonymous", "")})

	if outs := st.Doc().Cells[0].Outputs; len(outs) != 0 {
		t.Errorf("an unidentifiable part was projected: %+v", outs)
	}
}

// ─── Session end ────────────────────────────────────────────────────────

// A session that stops leaves its last cell running forever otherwise, and
// a document full of spinners is indistinguishable from a hung agent.
func TestProjector_ClosingSettlesTheLastCell(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "last thing", "l1")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "and the answer", "l2")})
	p.Close()

	c := st.Doc().Cells[0]
	if c.State != CellOK {
		t.Errorf("state = %q after the session ended, want ok", c.State)
	}
	// Closing twice happens on any teardown path that is not perfectly
	// sequenced, and must not append a second terminal event.
	before := st.Doc().Version
	p.Close()
	if after := st.Doc().Version; after != before {
		t.Errorf("a second Close appended %d events", after-before)
	}
}

// An interruption is a state change on the turn that was running, not a
// new turn. Rendering it as its own cell is what made the replayed
// document show a prompt the user never typed, immediately followed by the
// prompt they had actually typed twice.
func TestProjector_InterruptSettlesTheTurnItStopped(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "do a long thing", "l1")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "starting…", "l2")})
	p.Ingest([]TranscriptPart{part(PartInterrupted, "", "l3")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — an interruption is not a turn: %+v", len(cells), cells)
	}
	if cells[0].State != CellInterrupted {
		t.Errorf("state = %q, want interrupted — the cell must not read as having succeeded", cells[0].State)
	}
	// Whatever the agent produced before being stopped is kept: that is
	// the difference between interrupted and failed.
	if len(cells[0].Outputs) != 1 {
		t.Errorf("the partial output was discarded: %+v", cells[0].Outputs)
	}
}

// The pattern from the real transcript: interrupted, then the same prompt
// re-sent. Both are real; the document has to make the sequence legible
// rather than showing one prompt twice with no explanation.
func TestProjector_ReaskingAfterAnInterruptReadsAsTwoTurns(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "yes... i think A", "l1")})
	p.Ingest([]TranscriptPart{part(PartInterrupted, "", "l2")})
	p.Ingest([]TranscriptPart{part(PartUserText, "yes... i think A", "l3")})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "Approach A it is.", "l4")})

	cells := st.Doc().Cells
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2: %+v", len(cells), cells)
	}
	if cells[0].State != CellInterrupted {
		t.Errorf("first attempt state = %q, want interrupted", cells[0].State)
	}
	if cells[1].State != CellRunning || len(cells[1].Outputs) != 1 {
		t.Errorf("the answer landed on the wrong cell: %+v", cells[1])
	}
}

// Two prompts branching from the same parent: the first was abandoned. It
// must not settle as "ok", which reads as a question that was answered
// with silence.
func TestProjector_AnAbandonedBranchIsNotMarkedOK(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{{Kind: PartUserText, Text: "yes... i think A", UUID: "l1", ParentUUID: "p0"}})
	p.Ingest([]TranscriptPart{{Kind: PartUserText, Text: "yes... i think A", UUID: "l2", ParentUUID: "p0"}})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "Approach A it is.", "l3")})

	cells := st.Doc().Cells
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2: %+v", len(cells), cells)
	}
	if cells[0].State == CellOK {
		t.Errorf("the abandoned attempt is marked ok — it produced nothing and was superseded")
	}
	if cells[0].State != CellInterrupted {
		t.Errorf("state = %q, want interrupted", cells[0].State)
	}
	if cells[1].State != CellRunning || len(cells[1].Outputs) != 1 {
		t.Errorf("the answer did not land on the live branch: %+v", cells[1])
	}
}

// A normal sequence of prompts must keep settling as ok — the branch rule
// only applies when two turns genuinely share a parent.
func TestProjector_ConsecutivePromptsStillSettleOK(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{{Kind: PartUserText, Text: "first", UUID: "l1", ParentUUID: "p0"}})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "answer", "l2")})
	p.Ingest([]TranscriptPart{{Kind: PartUserText, Text: "second", UUID: "l3", ParentUUID: "l2"}})

	if got := st.Doc().Cells[0].State; got != CellOK {
		t.Errorf("state = %q, want ok — an answered turn is not an abandoned branch", got)
	}
}

// ─── Context injections ─────────────────────────────────────────────────

func TestProjector_InjectionsLandOnTheTurnTheyEntered(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "do the thing", "l1")})
	p.Ingest([]TranscriptPart{{
		Kind: PartInjection, Label: "hook: PreToolUse", Text: "reminder text",
		Size: 4096, UUID: "l2",
	}})
	p.Ingest([]TranscriptPart{part(PartAssistantText, "done", "l3")})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — an injection is not a turn: %+v", len(cells), cells)
	}
	outs := cells[0].Outputs
	if len(outs) != 2 {
		t.Fatalf("got %d outputs, want 2: %+v", len(outs), outs)
	}
	if outs[0].Type != OutputInjection {
		t.Errorf("output 0 = %q, want injection", outs[0].Type)
	}
	if outs[0].Data["label"] != "hook: PreToolUse" {
		t.Errorf("the label was lost: %v", outs[0].Data)
	}
	// The size is the point: it is what tells you a one-line reminder from
	// a forty-kilobyte skill body you never saw.
	if outs[0].Data["size"] != float64(4096) && outs[0].Data["size"] != 4096 {
		t.Errorf("size = %v, want 4096", outs[0].Data["size"])
	}
	// And it must not disturb the turn.
	if cells[0].State != CellRunning {
		t.Errorf("state = %q — an injection changed the turn's state", cells[0].State)
	}
}

// Injections arrive before the first prompt: session-start hooks run
// before anyone has typed anything. They still belong in the record.
func TestProjector_InjectionsBeforeAnyTurnAreKept(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{{Kind: PartInjection, Label: "hook: SessionStart", Text: "ctx", Size: 900, UUID: "l1"}})
	p.Ingest([]TranscriptPart{part(PartUserText, "the first prompt", "l2")})

	cells := st.Doc().Cells
	if len(cells) != 2 {
		t.Fatalf("got %d cells, want 2 (the startup context, then the prompt): %+v", len(cells), cells)
	}
	if len(cells[0].Outputs) != 1 || cells[0].Outputs[0].Type != OutputInjection {
		t.Errorf("the startup context was dropped: %+v", cells[0].Outputs)
	}
	if cells[1].Source != "the first prompt" {
		t.Errorf("the prompt landed wrong: %q", cells[1].Source)
	}
}

// ─── Subagents (#55a) ───────────────────────────────────────────────────
//
// A delegating turn shows an Agent call and its result with nothing in
// between. The child's work — often the majority of what happened — goes
// in there, attached by the exact link the transcript gives us rather than
// by guessing which turn was open.

func TestProjector_SubagentWorkAttachesToTheTurnThatSpawnedIt(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "review the diff", "l1")})
	p.Ingest([]TranscriptPart{{Kind: PartToolCall, ToolName: "Agent", ToolUseID: "t1", UUID: "l2"}})
	p.Ingest([]TranscriptPart{{
		Kind: PartToolResult, ToolUseID: "t1", Text: "launched", UUID: "l3",
		AgentID: "child1", AgentType: "code-reviewer",
	}})

	// The child's own conversation, read from its own file.
	p.IngestSubagent("child1", []TranscriptPart{
		{Kind: PartAssistantText, Text: "Reading the diff.", UUID: "c1", Sidechain: true},
		{Kind: PartToolCall, ToolName: "Read", ToolUseID: "ct1", UUID: "c2", Sidechain: true},
		{Kind: PartToolResult, ToolUseID: "ct1", Text: "package main", UUID: "c3", Sidechain: true},
	})

	cells := st.Doc().Cells
	if len(cells) != 1 {
		t.Fatalf("got %d cells, want 1 — a subagent is not a turn of its own: %+v", len(cells), cells)
	}
	var child []Output
	for _, o := range cells[0].Outputs {
		if (o.Data != nil) && o.Data["agentId"] == "child1" {
			child = append(child, o)
		}
	}
	if len(child) != 3 {
		t.Fatalf("got %d child outputs, want 3: %+v", len(child), cells[0].Outputs)
	}
	// The child's work keeps its own shapes — a tool call is a tool call
	// whoever made it — and is tagged so the renderer can nest it.
	if child[0].Type != OutputText || child[1].Type != OutputToolCall {
		t.Errorf("child output types = %q, %q", child[0].Type, child[1].Type)
	}
	if child[0].Data["agentType"] != "code-reviewer" {
		t.Errorf("the child is unlabelled: %v", child[0].Data)
	}
}

// The synchronous case: a child writes its whole transcript before the
// parent's result names it, so the link arrives last. Nothing may be lost
// waiting for it.
func TestProjector_SubagentWorkSeenBeforeTheLinkIsNotLost(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "delegate this", "l1")})
	p.Ingest([]TranscriptPart{{Kind: PartToolCall, ToolName: "Agent", ToolUseID: "t1", UUID: "l2"}})

	// Child lines arrive while the Agent call is still running.
	p.IngestSubagent("child2", []TranscriptPart{
		{Kind: PartAssistantText, Text: "working", UUID: "c1", Sidechain: true},
	})
	if got := len(st.Doc().Cells[0].Outputs); got != 1 {
		t.Fatalf("an unlinked child was written to the document anyway (%d outputs)", got)
	}

	// Then the result names it.
	p.Ingest([]TranscriptPart{{
		Kind: PartToolResult, ToolUseID: "t1", Text: "done", UUID: "l3", AgentID: "child2",
	}})

	var child int
	for _, o := range st.Doc().Cells[0].Outputs {
		if o.Data != nil && o.Data["agentId"] == "child2" {
			child++
		}
	}
	if child != 1 {
		t.Errorf("the child's work was lost while waiting for its link: %d outputs", child)
	}
}

// Held work cannot accumulate forever. A child that is never claimed by
// any result would otherwise be a slow leak on a long session.
func TestProjector_UnclaimedSubagentWorkIsBounded(t *testing.T) {
	_, p := newProjectorFixture(t)

	for i := 0; i < maxHeldSubagentParts+40; i++ {
		p.IngestSubagent("ghost", []TranscriptPart{
			{Kind: PartAssistantText, Text: "orphan", UUID: fmt.Sprintf("g%d", i), Sidechain: true},
		})
	}
	p.mu.Lock()
	held := len(p.heldSubagent["ghost"])
	p.mu.Unlock()
	if held > maxHeldSubagentParts {
		t.Errorf("held %d parts for a child nothing ever claimed, cap is %d", held, maxHeldSubagentParts)
	}
}

// Re-reading a child's file must not double its turns, exactly as for the
// parent transcript.
func TestProjector_SubagentIngestIsIdempotent(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "go", "l1")})
	p.Ingest([]TranscriptPart{{Kind: PartToolResult, ToolUseID: "t1", UUID: "l2", AgentID: "child3"}})

	work := []TranscriptPart{{Kind: PartAssistantText, Text: "once", UUID: "c1", Sidechain: true}}
	p.IngestSubagent("child3", work)
	before := st.Doc().Version
	p.IngestSubagent("child3", work)
	p.IngestSubagent("child3", work)

	if after := st.Doc().Version; after != before {
		t.Errorf("re-ingesting a child appended %d events", after-before)
	}
}

// Two children of one turn stay distinguishable — a parent routinely fans
// out, and merging them would read as one very confused agent.
func TestProjector_SiblingSubagentsStaySeparate(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "fan out", "l1")})
	p.Ingest([]TranscriptPart{{Kind: PartToolResult, ToolUseID: "t1", UUID: "l2", AgentID: "a", AgentType: "explorer"}})
	p.Ingest([]TranscriptPart{{Kind: PartToolResult, ToolUseID: "t2", UUID: "l3", AgentID: "b", AgentType: "reviewer"}})

	p.IngestSubagent("a", []TranscriptPart{{Kind: PartAssistantText, Text: "from a", UUID: "ca", Sidechain: true}})
	p.IngestSubagent("b", []TranscriptPart{{Kind: PartAssistantText, Text: "from b", UUID: "cb", Sidechain: true}})

	seen := map[string]string{}
	for _, o := range st.Doc().Cells[0].Outputs {
		if o.Data != nil && o.Data["agentId"] != nil {
			seen[o.Data["agentId"].(string)] = o.Text
		}
	}
	if seen["a"] != "from a" || seen["b"] != "from b" {
		t.Errorf("siblings were merged or mislabelled: %v", seen)
	}
}
