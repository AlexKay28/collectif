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

// These are recorded (see the injection tests below) but never as turns:
// they are context the harness supplied, not conversation. The distinction
// is the whole filter — without it a session's document buries the three
// sentences a person wrote under several thousand lines of machinery.
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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, p := range projectLine(t, c.line) {
				if p.Kind != PartInjection {
					t.Errorf("a %s line projected a %s part — %s: %+v", c.name, p.Kind, c.why, p)
				}
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

	// Recorded as an injection rather than discarded, but never as a
	// prompt: these run to tens of kilobytes and would dwarf every real
	// turn in the conversation.
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	if parts[0].Kind == PartUserText {
		t.Errorf("a background-task notification was projected as something the user typed: %+v", parts[0])
	}
	if parts[0].Kind != PartInjection {
		t.Errorf("kind = %q, want injection", parts[0].Kind)
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

	// Failing closed still means "not a prompt". It is recorded, because
	// something the model read is worth keeping, but it does not join the
	// conversation on the strength of an origin kind we have never seen.
	if len(parts) != 1 || parts[0].Kind != PartInjection {
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

// ─── Context injections ─────────────────────────────────────────────────
//
// P0 filtered these out and the document became readable. But filtered out
// is not the same as thrown away, and throwing them away costs something
// real: an injection you cannot see is often the whole explanation for why
// an agent did something surprising. A skill body, a hook's output, a
// system reminder — the model read all of it, and the notebook claimed the
// turn began with your prompt.
//
// So they are recorded as their own kind: hidden by default, present in the
// record. What is recorded is the fact, the label and the size, plus a
// bounded excerpt — not the body. The transcript on disk is the archive;
// duplicating 47 KB of task notification into our log would make the
// notebook expensive to be honest.

func TestProjectClaude_HookOutputIsRecordedAsAnInjection(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"attachment","uuid":"x1","timestamp":"2026-08-13T11:31:49.858Z",
	  "attachment":{"type":"hook_success","hookName":"SessionStart:startup",
	    "content":"# Project context\nUse the house style.","exitCode":0}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.Kind != PartInjection {
		t.Errorf("kind = %q, want injection", p.Kind)
	}
	// The label is what makes a list of injections readable at a glance.
	if !strings.Contains(p.Label, "SessionStart:startup") {
		t.Errorf("label = %q, want the hook that produced it", p.Label)
	}
	if !strings.Contains(p.Text, "house style") {
		t.Errorf("excerpt = %q", p.Text)
	}
	if p.Size == 0 {
		t.Error("size = 0 — without it the reader cannot tell a one-liner from a novel")
	}
}

func TestProjectClaude_InjectedUserTextIsRecordedNotDiscarded(t *testing.T) {
	cases := []struct {
		name, line, wantLabel string
	}{
		{
			"skill body injected by a tool",
			`{"type":"user","uuid":"x2","timestamp":"2026-08-13T11:31:55.950Z",
			  "isMeta":true,"sourceToolUseID":"toolu_9",
			  "message":{"role":"user","content":[{"type":"text","text":"Base directory for this skill: /x/y"}]}}`,
			"tool",
		},
		{
			"command caveat",
			`{"type":"user","uuid":"x3","timestamp":"2026-08-13T11:31:55.950Z","isMeta":true,
			  "message":{"role":"user","content":"<local-command-caveat>Caveat: …</local-command-caveat>"}}`,
			"caveat",
		},
		{
			"background task notification",
			`{"type":"user","uuid":"x4","timestamp":"2026-08-13T11:31:55.950Z",
			  "origin":{"kind":"task-notification"},
			  "message":{"role":"user","content":"<task-notification>\n<task-id>abc</task-id>\n…"}}`,
			"task",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts := projectLine(t, c.line)
			if len(parts) != 1 {
				t.Fatalf("got %d parts, want 1 — it was discarded: %+v", len(parts), parts)
			}
			if parts[0].Kind != PartInjection {
				t.Errorf("kind = %q, want injection", parts[0].Kind)
			}
			if !strings.Contains(strings.ToLower(parts[0].Label), c.wantLabel) {
				t.Errorf("label = %q, want something mentioning %q", parts[0].Label, c.wantLabel)
			}
		})
	}
}

// A 47 KB notification must not put 47 KB into the log. The size says what
// was injected; the excerpt says what it looked like; the transcript on
// disk is where the whole thing lives.
func TestProjectClaude_InjectionExcerptsAreBounded(t *testing.T) {
	big := strings.Repeat("x", 40000)
	parts := projectLine(t, `{
	  "type":"user","uuid":"x5","timestamp":"2026-08-13T11:31:55.950Z",
	  "origin":{"kind":"task-notification"},
	  "message":{"role":"user","content":`+jsonString(big)+`}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts: %+v", len(parts), parts)
	}
	if len(parts[0].Text) > injectionExcerptMax+4 {
		t.Errorf("excerpt is %d bytes — the log would grow by the size of everything ever injected",
			len(parts[0].Text))
	}
	if parts[0].Size != len(big) {
		t.Errorf("size = %d, want %d — the reader has to know what they are not being shown",
			parts[0].Size, len(big))
	}
}

// Bookkeeping is still bookkeeping. Turn durations and editor state were
// never in the model's context and recording them would bury the things
// that were.
func TestProjectClaude_BookkeepingIsStillNotAnInjection(t *testing.T) {
	for _, line := range []string{
		`{"type":"system","uuid":"y1","subtype":"turn_duration","durationMs":2084}`,
		`{"type":"file-history-snapshot","uuid":"y2"}`,
		`{"type":"queue-operation","uuid":"y3"}`,
	} {
		if parts := projectLine(t, line); len(parts) != 0 {
			t.Errorf("%s projected %+v", line, parts)
		}
	}
}

// A real prompt is still a prompt. The whole filter would be worthless if
// recording injections re-admitted them as context.
func TestProjectClaude_RecordingInjectionsDidNotBreakTheFilter(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"y4","timestamp":"2026-08-13T11:31:55.950Z","origin":{"kind":"human"},
	  "message":{"role":"user","content":"a real prompt"}}`)
	if len(parts) != 1 || parts[0].Kind != PartUserText {
		t.Fatalf("a typed prompt became %+v", parts)
	}
}

// Attachments are not one shape. A real 3,548-line session carried 804 of
// them across fifteen types, and the split that matters is not the type
// name but what the payload is *for*:
//
//   - Context put in front of the model: a skill listing, a hook's extra
//     instructions, the snippet of a file just edited, a queued command.
//     These explain behaviour, and are exactly what this records.
//   - The harness talking to the model about itself: 671 copies of
//     `<total_tokens>N tokens left</total_tokens>`, one per turn. These
//     explain nothing, and recording 671 of them would bury the skill body
//     that does — making the list unusable is the same failure as not
//     recording anything.
//
// The rule is deliberately narrow: one excluded type, named, with its
// reason. A blocklist that grows by guesswork would drift back into
// hiding things.
func TestProjectClaude_AttachmentsAreReadFromWhicheverFieldCarriesThem(t *testing.T) {
	cases := []struct{ name, line, wantLabel, wantIn string }{
		{
			"hook stdout",
			`{"type":"attachment","uuid":"a1","attachment":{"type":"hook_success",
			  "hookName":"SessionStart:startup","stdout":"{\"hookSpecificOutput\":1}"}}`,
			"SessionStart:startup", "hookSpecificOutput",
		},
		{
			"hook additional context",
			`{"type":"attachment","uuid":"a2","attachment":{"type":"hook_additional_context",
			  "hookName":"SessionStart","content":"You have superpowers."}}`,
			"SessionStart", "superpowers",
		},
		{
			"an edited file's snippet",
			`{"type":"attachment","uuid":"a3","attachment":{"type":"edited_text_file",
			  "filename":"src/main.go","snippet":"func main() {}"}}`,
			"main.go", "func main",
		},
		{
			"a queued command",
			`{"type":"attachment","uuid":"a4","attachment":{"type":"queued_command",
			  "prompt":"run the tests"}}`,
			"queued", "run the tests",
		},
		{
			"the todo list, re-injected",
			`{"type":"attachment","uuid":"a5","attachment":{"type":"task_reminder",
			  "content":"1. do the thing","itemCount":1}}`,
			"task", "do the thing",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts := projectLine(t, c.line)
			if len(parts) != 1 {
				t.Fatalf("got %d parts, want 1 — the payload field was not read: %+v", len(parts), parts)
			}
			if !strings.Contains(parts[0].Label, c.wantLabel) {
				t.Errorf("label = %q, want it to mention %q", parts[0].Label, c.wantLabel)
			}
			if !strings.Contains(parts[0].Text, c.wantIn) {
				t.Errorf("excerpt = %q, want it to contain %q", parts[0].Text, c.wantIn)
			}
		})
	}
}

func TestProjectClaude_TokenBudgetRemindersAreNotRecorded(t *testing.T) {
	parts := projectLine(t, `{"type":"attachment","uuid":"a6","attachment":{
	  "type":"total_tokens_reminder","text":"<total_tokens>15000000 tokens left</total_tokens>"}}`)

	if len(parts) != 0 {
		t.Errorf("the token-budget reminder was recorded — a real session carries 671 of these and "+
			"they would bury every injection that matters: %+v", parts)
	}
}

// An attachment type this build has never seen still gets recorded if it
// carries a payload. Failing closed on *filtering* is right; failing
// closed on *recording* would mean the next thing Claude Code starts
// injecting is invisible until someone notices.
func TestProjectClaude_UnknownAttachmentTypesAreStillRecorded(t *testing.T) {
	parts := projectLine(t, `{"type":"attachment","uuid":"a7","attachment":{
	  "type":"some_future_thing","content":"whatever this is"}}`)

	if len(parts) != 1 || parts[0].Kind != PartInjection {
		t.Fatalf("an unknown attachment type was dropped: %+v", parts)
	}
	// The type name survives as the label, underscores softened to spaces
	// so the list reads as English rather than as a schema dump.
	if !strings.Contains(parts[0].Label, "some future thing") {
		t.Errorf("label = %q, want the type name so it can be recognised", parts[0].Label)
	}
}

// An attachment with no payload at all is nothing to record.
func TestProjectClaude_EmptyAttachmentsProjectNothing(t *testing.T) {
	for _, line := range []string{
		`{"type":"attachment","uuid":"a8","attachment":{"type":"command_permissions","allowedTools":[]}}`,
		`{"type":"attachment","uuid":"a9","attachment":{"type":"hook_success","content":"","stdout":"  "}}`,
		`{"type":"attachment","uuid":"aa"}`,
	} {
		if parts := projectLine(t, line); len(parts) != 0 {
			t.Errorf("%s projected %+v", line, parts)
		}
	}
}

// A field whose type surprises us must cost us that field, never the line.
//
// `attachment.content` is polymorphic in the real format — a string for a
// hook, an array of blocks for injected context, an object for a file
// reference. Declaring it as a string made json.Unmarshal fail on the
// *whole line*, so 79 injections vanished without a trace: no error, no
// log, just a document quietly missing the "You have superpowers" preamble
// that shaped every turn under it.
//
// This is the same failure as P0's, one level down. There the risk was
// misreading a field; here it is that one surprising field discards
// everything around it. The parser has to be tolerant per-field, not
// all-or-nothing.
func TestProjectClaude_PolymorphicContentIsFlattened(t *testing.T) {
	cases := []struct{ name, line, wantIn string }{
		{
			"content as a string",
			`{"type":"attachment","uuid":"p1","attachment":{"type":"task_reminder","content":"1. ship it"}}`,
			"ship it",
		},
		{
			"content as an array of strings",
			`{"type":"attachment","uuid":"p2","attachment":{"type":"hook_additional_context",
			  "hookName":"SessionStart","content":["You have superpowers.","And a second line."]}}`,
			"superpowers",
		},
		{
			"content as an array of blocks",
			`{"type":"attachment","uuid":"p3","attachment":{"type":"hook_additional_context",
			  "content":[{"type":"text","text":"block form"}]}}`,
			"block form",
		},
		{
			"content as an object",
			`{"type":"attachment","uuid":"p4","attachment":{"type":"file","filename":"a.go",
			  "content":{"type":"text","file":{"filePath":"a.go","content":"package main"}}}}`,
			"package main",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parts := projectLine(t, c.line)
			if len(parts) != 1 {
				t.Fatalf("got %d parts, want 1 — one field's shape discarded the whole line: %+v",
					len(parts), parts)
			}
			if !strings.Contains(parts[0].Text, c.wantIn) {
				t.Errorf("excerpt = %q, want it to contain %q", parts[0].Text, c.wantIn)
			}
		})
	}
}

// An empty array is a reminder with nothing in it — 77 of those in one
// session — and must not become 77 empty rows.
func TestProjectClaude_EmptyPolymorphicContentIsNotAnInjection(t *testing.T) {
	for _, line := range []string{
		`{"type":"attachment","uuid":"p5","attachment":{"type":"task_reminder","content":[]}}`,
		`{"type":"attachment","uuid":"p6","attachment":{"type":"task_reminder","content":"[]"}}`,
		`{"type":"attachment","uuid":"p7","attachment":{"type":"x","content":{}}}`,
	} {
		if parts := projectLine(t, line); len(parts) != 0 {
			t.Errorf("%s projected %+v", line, parts)
		}
	}
}

// The same tolerance has to hold on the conversation path, where a
// surprising scalar would cost a real turn rather than an injection.
func TestProjectClaude_ASurprisingScalarDoesNotDiscardTheTurn(t *testing.T) {
	// isMeta as a string rather than a bool: invented, but exactly the kind
	// of drift that happens to someone else's format between releases.
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"p8","isMeta":"false","timestamp":"2026-08-17T10:00:00.000Z",
	  "message":{"role":"assistant","model":"claude-opus-5",
	    "content":[{"type":"text","text":"the answer survived"}]}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1 — one unexpected scalar cost us a whole turn: %+v", len(parts), parts)
	}
	if parts[0].Text != "the answer survived" {
		t.Errorf("text = %q", parts[0].Text)
	}
}

// ─── Subagents (#55a) ───────────────────────────────────────────────────
//
// A parent turn that delegates currently shows an Agent tool call and its
// result with nothing in between — and that gap is often the majority of
// what the agent did. The child's conversation lives in
// `<session>/subagents/agent-<id>.jsonl`, which P0 proved this same parser
// reads unchanged.
//
// What was missing was the link. It turns out to be exact rather than
// inferred: the parent's tool_result carries `toolUseResult.agentId`, which
// is the child file's name. Surveyed across every transcript on this
// machine: 478 agentId-bearing results, spawned by `Agent` (475) and
// `Skill` (3). No heuristic needed, and none should be used — attaching a
// child to "whatever turn was open" would be a guess where the data has an
// answer.

func TestProjectClaude_AgentResultCarriesTheChildsID(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"s1","timestamp":"2026-08-17T09:49:16.423Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]},
	  "toolUseResult":{"status":"completed","agentId":"a3849ed00b115f042",
	    "agentType":"general-purpose","totalTokens":45534,"totalToolUseCount":29,
	    "totalDurationMs":103614}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.Kind != PartToolResult {
		t.Fatalf("kind = %q, want tool_result", p.Kind)
	}
	// The link. Without it there is no way to attach a child transcript to
	// the turn that spawned it except by guessing.
	if p.AgentID != "a3849ed00b115f042" {
		t.Errorf("agentId = %q, want a3849ed00b115f042", p.AgentID)
	}
	if p.AgentType != "general-purpose" {
		t.Errorf("agentType = %q", p.AgentType)
	}
}

// A background launch reports the id immediately and the work happens
// afterwards. Both flavours have to yield the same link — 406 of the 478
// results on this machine are async.
func TestProjectClaude_AsyncAgentLaunchAlsoCarriesTheID(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"s2","timestamp":"2026-08-17T09:49:16.423Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_2","content":"launched"}]},
	  "toolUseResult":{"status":"async_launched","agentId":"aae89cd004e57c852",
	    "agentType":"code-reviewer"}}`)

	if len(parts) != 1 || parts[0].AgentID != "aae89cd004e57c852" {
		t.Fatalf("an async launch did not report its child: %+v", parts)
	}
}

// A forked skill spawns a child too, and names it differently.
func TestProjectClaude_ForkedSkillCarriesTheID(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"s3","timestamp":"2026-08-17T09:49:16.423Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_3","content":"Running in the background"}]},
	  "toolUseResult":{"success":true,"commandName":"code-review","status":"forked",
	    "background":true,"agentId":"a95b4e04f977fdd33"}}`)

	if len(parts) != 1 || parts[0].AgentID != "a95b4e04f977fdd33" {
		t.Fatalf("a forked skill did not report its child: %+v", parts)
	}
	if parts[0].AgentType != "code-review" {
		t.Errorf("agentType = %q, want the command name", parts[0].AgentType)
	}
}

// An ordinary tool result must not grow a phantom child.
func TestProjectClaude_OrdinaryResultsHaveNoAgentID(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"s4","timestamp":"2026-08-17T09:49:16.423Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_4","content":"file contents"}]},
	  "toolUseResult":{"stdout":"file contents","stderr":""}}`)

	if len(parts) != 1 || parts[0].AgentID != "" {
		t.Fatalf("a plain tool result claimed a subagent: %+v", parts)
	}
}

// toolUseResult is another field whose shape we do not control, and the
// polymorphic-content bug is fresh enough to test for here rather than
// discover later.
func TestProjectClaude_AWeirdToolUseResultDoesNotDiscardTheLine(t *testing.T) {
	for _, tur := range []string{`"just a string"`, `[1,2,3]`, `null`, `{"agentId":42}`} {
		parts := projectLine(t, `{
		  "type":"user","uuid":"s5","timestamp":"2026-08-17T09:49:16.423Z",
		  "message":{"role":"user","content":[
		    {"type":"tool_result","tool_use_id":"toolu_5","content":"ok"}]},
		  "toolUseResult":`+tur+`}`)
		if len(parts) != 1 {
			t.Errorf("toolUseResult=%s cost us the whole result: %+v", tur, parts)
		}
	}
}

// ─── Locating the child ─────────────────────────────────────────────────

func TestClaudeAdapter_SubagentPathIsDerivedFromTheParentTranscript(t *testing.T) {
	got, ok := (&claudeAdapter{}).SubagentTranscriptPath(
		"/home/u/.claude/projects/-p/abc.jsonl", "a95b4e04f977fdd33")
	if !ok {
		t.Fatal("no subagent path for a session that has one")
	}
	want := "/home/u/.claude/projects/-p/abc/subagents/agent-a95b4e04f977fdd33.jsonl"
	if got != want {
		t.Errorf("path = %q\n want %q", got, want)
	}
}

// The id comes out of someone else's JSON and is about to become a path
// component. It has to be refused if it is not the shape we expect.
func TestClaudeAdapter_SubagentPathRefusesAnUnsafeID(t *testing.T) {
	for _, id := range []string{"", "../../etc/passwd", "a/b", "a\x00b", strings.Repeat("z", 200), "A9-!"} {
		if got, ok := (&claudeAdapter{}).SubagentTranscriptPath("/p/abc.jsonl", id); ok {
			t.Errorf("id %q was accepted and produced %q", id, got)
		}
	}
}

func TestClaudeAdapter_SubagentPathNeedsATranscript(t *testing.T) {
	if _, ok := (&claudeAdapter{}).SubagentTranscriptPath("", "a95b4e04f977fdd33"); ok {
		t.Error("a subagent path was produced with no parent transcript to hang it off")
	}
}
