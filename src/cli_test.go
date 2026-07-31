package main

import (
	"testing"
)

// TestGetAdapterDefaultsToClaude — an empty CLI name resolves to the
// default adapter so pre-#46 clients (and Session values loaded from
// on-disk state that predates the field) keep working unchanged.
func TestGetAdapterDefaultsToClaude(t *testing.T) {
	a := getAdapter("")
	if a == nil {
		t.Fatalf("getAdapter(\"\") returned nil; expected default claude adapter")
	}
	if a.Name() != "claude" {
		t.Errorf("getAdapter(\"\").Name() = %q, want %q", a.Name(), "claude")
	}
}

// TestGetAdapterUnknown — unknown names return nil. Callers rely on the
// nil sentinel to decide between "spawn something" and "reject the
// request as 400"; see handleAgents in api.go.
func TestGetAdapterUnknown(t *testing.T) {
	if a := getAdapter("nonesuch"); a != nil {
		t.Errorf("getAdapter(\"nonesuch\") = %v, want nil", a)
	}
}

// TestClaudeAdapterCapabilitiesAllTrue — Claude Code is the reference
// implementation; every capability bit is expected to be on. This test
// is intentionally strict so future accidental disables surface loudly.
func TestClaudeAdapterCapabilitiesAllTrue(t *testing.T) {
	a := getAdapter("claude")
	if a == nil {
		t.Fatalf("no claude adapter registered")
	}
	c := a.Capabilities()
	if !c.Hooks || !c.StructuredTranscript || !c.ToolCallEvents ||
		!c.SubagentFiles || !c.PreCompact || !c.SessionIDPinning {
		t.Errorf("claude Capabilities want all true, got %+v", c)
	}
}

// TestClaudeAdapterParseTranscriptLine — one usage-bearing line with a
// content array of each block type. The event shape must match what the
// underlying extractUsageAndChars + extractModel helpers return; those
// have their own dedicated coverage in transcript_test.go, so this is a
// thin delegation test.
func TestClaudeAdapterParseTranscriptLine(t *testing.T) {
	a := getAdapter("claude")
	if a == nil {
		t.Fatalf("no claude adapter")
	}
	line := []byte(`{"message":{"role":"assistant","model":"claude-opus-4-7","content":[` +
		`{"type":"thinking","thinking":"hmm"},` +
		`{"type":"text","text":"hi"}` +
		`],"usage":{"input_tokens":11,"output_tokens":22,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}}` + "\n")

	ev, err := a.ParseTranscriptLine(line)
	if err != nil {
		t.Fatalf("ParseTranscriptLine: %v", err)
	}
	if !ev.HasUsage {
		t.Fatalf("HasUsage=false; expected true for a usage-bearing line")
	}
	if ev.Model != "claude-opus-4-7" {
		t.Errorf("Model=%q, want claude-opus-4-7", ev.Model)
	}
	if ev.InputTokens != 11 || ev.OutputTokens != 22 ||
		ev.CacheReadTokens != 3 || ev.CacheCreationTokens != 5 {
		t.Errorf("tokens: got in=%d out=%d cr=%d cc=%d, want 11/22/3/5",
			ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheCreationTokens)
	}
	if ev.ThinkingChars != uint64(len("hmm")) {
		t.Errorf("ThinkingChars=%d, want %d", ev.ThinkingChars, len("hmm"))
	}
	if ev.TextChars != uint64(len("hi")) {
		t.Errorf("TextChars=%d, want %d", ev.TextChars, len("hi"))
	}
}

// TestClaudeAdapterParseTranscriptLineNoUsage — a line without a usage
// block must return HasUsage=false and a nil error. The watcher relies
// on this to skip non-assistant lines without spamming logs.
func TestClaudeAdapterParseTranscriptLineNoUsage(t *testing.T) {
	a := getAdapter("claude")
	ev, err := a.ParseTranscriptLine([]byte(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ev.HasUsage {
		t.Errorf("HasUsage=true on a non-usage line")
	}
}

// TestClaudeAdapterModelContextLimit — sanity check that the adapter's
// context-limit lookup is wired to the same map contextLimitFor uses.
// The exact numbers live in harness.go; this test just confirms we
// route through the adapter rather than duplicating the table.
func TestClaudeAdapterModelContextLimit(t *testing.T) {
	a := getAdapter("claude")
	if got, want := a.ModelContextLimit("claude-opus-4-7-20260115"), 200000; got != want {
		t.Errorf("ModelContextLimit(opus)=%d, want %d", got, want)
	}
	if got, want := a.ModelContextLimit(""), defaultContextLimit; got != want {
		t.Errorf("ModelContextLimit(\"\")=%d, want %d", got, want)
	}
}

// TestClaudeAdapterTranscriptPath — well-known path computation. We
// mainly care that (a) it computes some non-empty path when session +
// cwd are provided and (b) the empty-session-id case returns false.
func TestClaudeAdapterTranscriptPath(t *testing.T) {
	a := getAdapter("claude")
	if _, ok := a.TranscriptPath("", "/tmp/xyz"); ok {
		t.Errorf("expected ok=false for empty sessionID")
	}
	p, ok := a.TranscriptPath("sid-1", "/tmp/xyz")
	if !ok || p == "" {
		t.Errorf("expected a non-empty path, got %q ok=%v", p, ok)
	}
}
