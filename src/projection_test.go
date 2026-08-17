package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// #47 P0 (ADR 0002) — projection, slice A.
//
// The whole of ADR 0002 rests on one question that could not be answered
// from the ADR itself: does a CLI transcript carry enough to reconstruct a
// turn? For Claude Code the answer is yes, and these tests pin down exactly
// which shapes we rely on, because they belong to someone else's product
// and will drift.
//
// The line shapes below are reduced from a real transcript
// (`~/.claude/projects/**/<session>.jsonl`, Claude Code 2.1.231). Fields
// the projection does not read are dropped; fields it does read are kept
// verbatim, including the ones whose absence changes the meaning.

// ─── Assistant turns ────────────────────────────────────────────────────

func TestProjectClaude_AssistantTextBecomesOneAssistantPart(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u1","timestamp":"2026-08-13T11:31:59.116Z","isSidechain":false,
	  "message":{"role":"assistant","model":"claude-opus-5",
	    "content":[{"type":"text","text":"Hi! What would you like to work on?"}],
	    "usage":{"input_tokens":2,"output_tokens":21}}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.Kind != PartAssistantText {
		t.Errorf("kind = %q, want assistant", p.Kind)
	}
	if p.Text != "Hi! What would you like to work on?" {
		t.Errorf("text = %q", p.Text)
	}
	if p.Model != "claude-opus-5" {
		t.Errorf("model = %q, want claude-opus-5", p.Model)
	}
	if p.UUID != "u1" {
		t.Errorf("uuid = %q, want u1", p.UUID)
	}
	if p.At.IsZero() {
		t.Error("timestamp was not parsed — cells would have no ordering key of their own")
	}
}

// One assistant message routinely carries thinking *and* a tool call. A
// parser that returns a single event per line would silently drop one of
// them, which is why projection returns a slice.
func TestProjectClaude_OneMessageCanCarrySeveralPartsInOrder(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u2","timestamp":"2026-08-13T11:32:35.942Z",
	  "message":{"role":"assistant","model":"claude-opus-5","content":[
	    {"type":"thinking","thinking":"The user wants the issue list.","signature":"abc"},
	    {"type":"text","text":"Let me look."},
	    {"type":"tool_use","id":"toolu_1","name":"Bash",
	     "input":{"command":"gh issue list","description":"List issues"}}]}}`)

	if len(parts) != 3 {
		t.Fatalf("got %d parts, want 3 (thinking, text, tool_use): %+v", len(parts), parts)
	}
	want := []PartKind{PartThinking, PartAssistantText, PartToolCall}
	for i, k := range want {
		if parts[i].Kind != k {
			t.Errorf("part %d kind = %q, want %q — block order is turn order", i, parts[i].Kind, k)
		}
	}
	if parts[0].Text != "The user wants the issue list." {
		t.Errorf("thinking text = %q", parts[0].Text)
	}
	call := parts[2]
	if call.ToolName != "Bash" || call.ToolUseID != "toolu_1" {
		t.Errorf("tool call = %q/%q, want Bash/toolu_1", call.ToolName, call.ToolUseID)
	}
	var in map[string]any
	if err := json.Unmarshal(call.ToolInput, &in); err != nil {
		t.Fatalf("tool input is not usable JSON: %v", err)
	}
	if in["command"] != "gh issue list" {
		t.Errorf("tool input lost the command: %v", in)
	}
}

// Adaptive thinking emits blocks whose text is redacted away, leaving only
// a signature. Rendering an empty thinking block would put a permanently
// blank disclosure widget in the document.
func TestProjectClaude_RedactedThinkingIsDropped(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u3","timestamp":"2026-08-13T11:32:35.942Z",
	  "message":{"role":"assistant","model":"claude-opus-5",
	    "content":[{"type":"thinking","thinking":"","signature":"CAISmAIK"}]}}`)

	if len(parts) != 0 {
		t.Errorf("got %d parts, want 0 — a signature with no text is not readable output: %+v", len(parts), parts)
	}
}

// ─── Tool results ───────────────────────────────────────────────────────

func TestProjectClaude_ToolResultCarriesItsCallID(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"u4","timestamp":"2026-08-13T11:32:04.944Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_1","content":"146\tOPEN\tsomething","is_error":false}]},
	  "toolUseResult":{"stdout":"146\tOPEN\tsomething","stderr":"","interrupted":false}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.Kind != PartToolResult {
		t.Errorf("kind = %q, want tool_result", p.Kind)
	}
	// Without the id there is no way to pair a result with its call, and
	// the notebook cannot nest them.
	if p.ToolUseID != "toolu_1" {
		t.Errorf("tool_use_id = %q, want toolu_1", p.ToolUseID)
	}
	if !strings.Contains(p.Text, "146") {
		t.Errorf("result text = %q", p.Text)
	}
	if p.IsError {
		t.Error("a successful result was marked as an error")
	}
}

func TestProjectClaude_FailedToolResultIsMarked(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"u5","timestamp":"2026-08-13T11:32:04.944Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_2","content":"No such file","is_error":true}]}}`)

	if len(parts) != 1 || !parts[0].IsError {
		t.Fatalf("is_error did not survive projection: %+v", parts)
	}
}

// Tool results are sometimes a block list rather than a string.
func TestProjectClaude_ToolResultContentMayBeBlocks(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"u6","timestamp":"2026-08-13T11:32:04.944Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_3",
	     "content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	if !strings.Contains(parts[0].Text, "line one") || !strings.Contains(parts[0].Text, "line two") {
		t.Errorf("block-list content was not flattened: %q", parts[0].Text)
	}
}

// ─── What is NOT a user turn ────────────────────────────────────────────
//
// This is the half of the format that decides whether the projection is
// usable. Claude Code writes a great deal *as* the user that the user never
// typed: skill bodies, command caveats, hook output, tool-injected context.
// Projecting those verbatim would bury the three sentences a person
// actually wrote under thousands of lines of machinery.

func TestProjectClaude_TypedPromptBecomesAUserPart(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"u7","timestamp":"2026-08-13T11:31:55.950Z","promptId":"p1",
	  "message":{"role":"user","content":"merge it and start P0"}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	if parts[0].Kind != PartUserText || parts[0].Text != "merge it and start P0" {
		t.Errorf("kind/text = %q/%q", parts[0].Kind, parts[0].Text)
	}
}

func TestProjectClaude_InjectedUserLinesAreNotTurns(t *testing.T) {
	cases := []struct {
		name string
		line string
		why  string
	}{
		{
			"isMeta",
			`{"type":"user","uuid":"a","isMeta":true,"timestamp":"2026-08-13T11:31:55.950Z",
			  "message":{"role":"user","content":"<local-command-caveat>Caveat: …</local-command-caveat>"}}`,
			"isMeta marks text the harness wrote in the user's voice",
		},
		{
			"sourceToolUseID",
			`{"type":"user","uuid":"b","sourceToolUseID":"toolu_9","timestamp":"2026-08-13T11:31:55.950Z",
			  "message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: …"}]}}`,
			"a tool injected it; the user never saw it as a prompt",
		},
		{
			"attachment",
			`{"type":"attachment","uuid":"c","timestamp":"2026-08-13T11:31:49.858Z",
			  "attachment":{"type":"hook_success","hookName":"SessionStart:startup","stdout":"…"}}`,
			"hook output is session machinery, not conversation",
		},
		{
			"system",
			`{"type":"system","uuid":"d","subtype":"turn_duration","durationMs":2084,
			  "timestamp":"2026-08-13T11:31:59.169Z"}`,
			"bookkeeping",
		},
		{
			"file-history-snapshot",
			`{"type":"file-history-snapshot","uuid":"e","timestamp":"2026-08-13T11:31:59.169Z"}`,
			"editor state, not a turn",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if parts := projectLine(t, c.line); len(parts) != 0 {
				t.Errorf("projected %d parts from a %s line — %s: %+v", len(parts), c.name, c.why, parts)
			}
		})
	}
}

// ─── Subagents ──────────────────────────────────────────────────────────

// Sidechain turns are a subagent's conversation, not the main one. They are
// projected (M6 renders them nested) but must be distinguishable, or a
// parent session's document interleaves two unrelated conversations.
func TestProjectClaude_SidechainTurnsAreFlagged(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u8","isSidechain":true,"timestamp":"2026-08-13T11:32:35.942Z",
	  "message":{"role":"assistant","model":"claude-opus-5",
	    "content":[{"type":"text","text":"searching"}]}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1", len(parts))
	}
	if !parts[0].Sidechain {
		t.Error("a sidechain turn was projected as a main-thread turn")
	}
}

// ─── Robustness ─────────────────────────────────────────────────────────

// The transcript is written by another process while we read it, and it is
// someone else's format besides. Every malformed shape has to come back as
// "nothing to project" rather than an error that stops the watcher or a
// panic that takes the server down.
func TestProjectClaude_MalformedLinesProjectNothing(t *testing.T) {
	lines := []string{
		``,
		`   `,
		`{`,
		`not json at all`,
		`{"type":"assistant"}`,
		`{"type":"assistant","message":null}`,
		`{"type":"assistant","message":{"content":null}}`,
		`{"type":"assistant","message":{"content":"a string, not blocks"}}`,
		`{"type":"assistant","message":{"content":[null,42,"x"]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result"}]}}`,
		`{"type":"user","message":{"content":[{"type":"unknown_block","text":"?"}]}}`,
		`{"type":"assistant","message":{"content":[]},"timestamp":"not-a-time"}`,
	}
	a := &claudeAdapter{}
	for _, l := range lines {
		parts, err := a.ProjectTranscriptLine([]byte(l))
		if err != nil {
			t.Errorf("line %q returned an error (%v) — the watcher would log noise on every partial write", l, err)
		}
		for _, p := range parts {
			if p.Kind == "" {
				t.Errorf("line %q produced a part with no kind: %+v", l, p)
			}
		}
	}
}

// A line whose timestamp is unparseable still carries a projectable turn.
// Dropping it would lose real output over a formatting detail.
func TestProjectClaude_BadTimestampDoesNotDropTheTurn(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u9","timestamp":"whenever",
	  "message":{"role":"assistant","content":[{"type":"text","text":"still here"}]}}`)
	if len(parts) != 1 || parts[0].Text != "still here" {
		t.Fatalf("a bad timestamp cost us the turn: %+v", parts)
	}
}

// ─── Degradation (ADR 0002 D11) ─────────────────────────────────────────

// codex and opencode have no content projection yet. They must say so
// through Capabilities rather than returning empty parts that look like a
// session with nothing in it.
func TestProjection_CapabilityIsDeclaredHonestly(t *testing.T) {
	for name, a := range adapters {
		caps := a.Capabilities()
		parts, err := a.ProjectTranscriptLine([]byte(`{
		  "type":"assistant","uuid":"z","timestamp":"2026-08-13T11:31:59.116Z",
		  "message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`))
		if err != nil {
			t.Errorf("%s: ProjectTranscriptLine errored: %v", name, err)
		}
		if caps.TranscriptContent && len(parts) == 0 {
			t.Errorf("%s claims TranscriptContent but projected nothing from an assistant turn", name)
		}
		if !caps.TranscriptContent && len(parts) > 0 {
			t.Errorf("%s projects content but does not declare TranscriptContent — the UI would not know to stop apologising", name)
		}
	}
}

func projectLine(t *testing.T, line string) []TranscriptPart {
	t.Helper()
	parts, err := (&claudeAdapter{}).ProjectTranscriptLine([]byte(line))
	if err != nil {
		t.Fatalf("ProjectTranscriptLine: %v", err)
	}
	return parts
}

// ─── Origin (found by the real-transcript spike) ────────────────────────
//
// The fixtures above were written from my reading of the format and they
// all passed. Running the same parser over a real 11k-line transcript
// surfaced a 47 KB "typed prompt", which is how these three shapes were
// found. Claude Code marks a line's provenance in `origin.kind`, and
// isMeta alone does not catch machine-authored user turns.

func TestProjectClaude_TaskNotificationsAreNotPrompts(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"o1","timestamp":"2026-08-13T11:31:55.950Z","promptId":"p",
	  "origin":{"kind":"task-notification"},
	  "message":{"role":"user","content":"<task-notification>\n<task-id>abc</task-id>\n…"}}`)

	if len(parts) != 0 {
		t.Errorf("a background-task notification was projected as a prompt (%d parts) — these run to tens of "+
			"kilobytes and would dwarf every real turn: %+v", len(parts), parts)
	}
}

func TestProjectClaude_HumanOriginIsAPrompt(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"o2","timestamp":"2026-08-13T11:31:55.950Z","promptId":"p",
	  "origin":{"kind":"human"},
	  "message":{"role":"user","content":"hi"}}`)

	if len(parts) != 1 || parts[0].Kind != PartUserText {
		t.Fatalf("an explicitly human-origin line was not projected as a prompt: %+v", parts)
	}
}

// Any origin kind we do not recognise is machinery until proven otherwise.
// The alternative fails open, and failing open here means a future
// injection format silently becomes the loudest thing in the document.
func TestProjectClaude_UnknownOriginIsNotAPrompt(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"o3","timestamp":"2026-08-13T11:31:55.950Z",
	  "origin":{"kind":"some-future-injection"},
	  "message":{"role":"user","content":"whatever this turns out to be"}}`)

	if len(parts) != 0 {
		t.Errorf("an unrecognised origin defaulted to being a prompt: %+v", parts)
	}
}

// Lines with no origin at all predate the field and are overwhelmingly
// real prompts, so absence still means "human" — the other filters cover it.
func TestProjectClaude_AbsentOriginStillCountsAsTyped(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"o4","timestamp":"2026-08-13T11:31:55.950Z","promptId":"p",
	  "message":{"role":"user","content":"an older transcript's prompt"}}`)

	if len(parts) != 1 || parts[0].Kind != PartUserText {
		t.Fatalf("a pre-origin prompt was dropped: %+v", parts)
	}
}

// ─── Compaction ─────────────────────────────────────────────────────────

// ADR 0002 open question 4 asked what happens to the document when the
// transcript is compacted, and proposed emitting a marker. The real format
// is better than the proposal: compaction appends a summary line rather
// than rewriting history, so the notebook can render the summary itself.
// What it must not do is show it as something the user typed.
func TestProjectClaude_CompactSummaryIsItsOwnKind(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"c1","timestamp":"2026-08-13T11:31:55.950Z","promptId":"p",
	  "isCompactSummary":true,"isVisibleInTranscriptOnly":true,
	  "message":{"role":"user","content":"This session is being continued from a previous conversation…"}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1 — the summary is the only record of everything above it: %+v", len(parts), parts)
	}
	if parts[0].Kind != PartCompactSummary {
		t.Errorf("kind = %q, want compact_summary — rendering it as a prompt claims the user wrote their own summary",
			parts[0].Kind)
	}
}

// ─── Interruption (found by replaying a real session) ───────────────────

// Pressing Escape writes a literal `[Request interrupted by user]` line
// with role user, no origin and no isMeta — so every provenance filter we
// have lets it through, and the replayed document showed it as something
// the user had typed. They typed nothing; they stopped the agent.
func TestProjectClaude_InterruptIsNotAPrompt(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"i1","timestamp":"2026-08-17T10:00:00.000Z","promptId":"p",
	  "message":{"role":"user","content":"[Request interrupted by user]"}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1 — the interruption is real, it is just not a prompt: %+v",
			len(parts), parts)
	}
	if parts[0].Kind != PartInterrupted {
		t.Errorf("kind = %q, want interrupted", parts[0].Kind)
	}
}

// The same sentinel appears with a trailing reason on tool interrupts.
func TestProjectClaude_InterruptVariantsAreRecognised(t *testing.T) {
	for _, text := range []string{
		"[Request interrupted by user]",
		"[Request interrupted by user for tool use]",
	} {
		parts := projectLine(t, `{"type":"user","uuid":"i2","timestamp":"2026-08-17T10:00:00.000Z",
		  "message":{"role":"user","content":`+jsonString(text)+`}}`)
		if len(parts) != 1 || parts[0].Kind != PartInterrupted {
			t.Errorf("%q projected as %+v, want an interrupt", text, parts)
		}
	}
}

// A prompt that merely mentions an interruption is still a prompt. The
// sentinel is matched as a whole line, not searched for.
func TestProjectClaude_TalkingAboutInterruptsIsStillAPrompt(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"i3","timestamp":"2026-08-17T10:00:00.000Z","origin":{"kind":"human"},
	  "message":{"role":"user","content":"why did it say [Request interrupted by user]?"}}`)

	if len(parts) != 1 || parts[0].Kind != PartUserText {
		t.Errorf("a real question about interrupts was swallowed: %+v", parts)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ─── Branches ───────────────────────────────────────────────────────────

// The transcript is a tree, not a list: every line names its parent, and
// two user turns sharing a parent means the first was abandoned and
// re-sent. Without the parent link the projector cannot tell an abandoned
// turn from a completed one, and the replayed document showed a prompt
// marked "ok" that had produced nothing at all.
func TestProjectClaude_PartsCarryTheirParent(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"b2","parentUuid":"b1","timestamp":"2026-08-17T10:00:00.000Z",
	  "origin":{"kind":"human"},"message":{"role":"user","content":"try again"}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts: %+v", len(parts), parts)
	}
	if parts[0].ParentUUID != "b1" {
		t.Errorf("parent = %q, want b1", parts[0].ParentUUID)
	}
}
