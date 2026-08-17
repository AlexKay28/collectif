package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

// #51 M2.5 — the gate. The mixed-cell model re-sends the projected prefix
// on every run, which is affordable only if that prefix caches. The cache
// is a prefix match over exact bytes rendered tools → system → messages, so
// these are not optimisations to add later; they are constraints on how the
// request is built at all.
//
// Everything here is testable offline. The one thing that is not is whether
// the cache actually *lands* against the live API — that needs a key, and
// the metric below is what will read it out.

func hashRequest(t *testing.T, req Request) string {
	t.Helper()
	params := buildAnthropicRequest(req)
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func cacheTestRequest() Request {
	return Request{
		Model:  "claude-opus-5",
		System: "you are in a notebook",
		Messages: []Message{
			userText("first cell"),
			{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockText, Text: "first answer"}}},
			userText("the question"),
		},
		StablePrefixMessages: 3,
		Tools: []ToolSpec{
			{Name: "read", Description: "read a file", InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string", "description": "p"}},
				"required":   []string{"path"}, "additionalProperties": false,
			}},
			{Name: "grep", Description: "search", InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"pattern": map[string]any{"type": "string", "description": "p"}},
				"required":   []string{"pattern"}, "additionalProperties": false,
			}},
		},
	}
}

// One changed byte anywhere in the prefix invalidates everything after it,
// so an identical logical request has to serialise identically every time.
// Go map iteration is randomised — a single range over a map anywhere in
// the build path would destroy the cache silently, with nothing failing.
func TestRequest_IsByteIdenticalAcrossRepeatedBuilds(t *testing.T) {
	req := cacheTestRequest()
	want := hashRequest(t, req)
	for i := 0; i < 100; i++ {
		if got := hashRequest(t, req); got != want {
			t.Fatalf("build %d produced different bytes:\n got %s\nwant %s", i, got, want)
		}
	}
}

// Tools render at position 0, so their order is the most destructive thing
// that can vary: a different order invalidates the entire prefix including
// system and every message.
func TestToolSpecs_AreOrderedDeterministically(t *testing.T) {
	prev := activeTools
	t.Cleanup(func() { activeTools = prev })

	// Registered in a deliberately unhelpful order.
	activeTools = []Tool{&grepTool{}, &readTool{}, &globTool{}}
	first := toolSpecs()
	activeTools = []Tool{&readTool{}, &globTool{}, &grepTool{}}
	second := toolSpecs()

	if len(first) != len(second) {
		t.Fatalf("%d vs %d tools", len(first), len(second))
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Fatalf("registration order leaked into the request: %s vs %s at %d",
				first[i].Name, second[i].Name, i)
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Name > first[i].Name {
			t.Errorf("tools are not sorted: %v", toolNamesOf(first))
		}
	}
}

func toolNamesOf(specs []ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// ─── Breakpoints ────────────────────────────────────────────────────────

func breakpointCount(params any) int {
	b, _ := json.Marshal(params)
	return strings.Count(string(b), `"cache_control"`)
}

func TestRequest_MarksTheEndOfTheStablePrefix(t *testing.T) {
	params := buildAnthropicRequest(cacheTestRequest())

	// System carries one, and the last message of the projected prefix
	// carries another — that is the span worth reusing between runs.
	if len(params.System) == 0 || params.System[0].CacheControl.Type == "" {
		t.Error("no breakpoint on the system prefix")
	}
	last := params.Messages[len(params.Messages)-1]
	tail := last.Content[len(last.Content)-1]
	if tail.OfText == nil || tail.OfText.CacheControl.Type == "" {
		t.Error("no breakpoint at the end of the projected prefix — the cells above would be re-read every run")
	}
}

// A request may carry at most four breakpoints; a fifth is rejected by the
// API, so the budget is enforced here rather than discovered in production.
func TestRequest_NeverExceedsFourBreakpoints(t *testing.T) {
	req := cacheTestRequest()
	// A long tool-calling loop: many messages after the stable prefix.
	for i := 0; i < 40; i++ {
		req.Messages = append(req.Messages,
			Message{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockToolUse, ToolUseID: "t", ToolName: "read", ToolInput: map[string]any{"path": "x"}},
			}},
			Message{Role: RoleUser, Content: []ContentBlock{
				{Type: BlockToolResult, ToolUseID: "t", Text: "result"},
			}},
		)
	}
	if got := breakpointCount(buildAnthropicRequest(req)); got > 4 {
		t.Errorf("%d breakpoints, want at most 4", got)
	}
}

// A breakpoint searches back at most 20 content blocks for a prior entry.
// One tool-heavy turn blows past that, so the loop has to leave
// intermediate breakpoints or the next request silently misses.
func TestRequest_AddsIntermediateBreakpointsForLongTurns(t *testing.T) {
	short := cacheTestRequest()
	shortCount := breakpointCount(buildAnthropicRequest(short))

	long := cacheTestRequest()
	for i := 0; i < 30; i++ {
		long.Messages = append(long.Messages, Message{
			Role:    RoleUser,
			Content: []ContentBlock{{Type: BlockToolResult, ToolUseID: "t", Text: "result"}},
		})
	}
	longCount := breakpointCount(buildAnthropicRequest(long))

	if longCount <= shortCount {
		t.Errorf("a 30-block turn got %d breakpoints and a short one got %d — nothing was placed inside the lookback window",
			longCount, shortCount)
	}
}

// ─── The metric ─────────────────────────────────────────────────────────

// The number this whole phase exists to produce. Zero cache reads across
// repeated runs is the canary for a projection bug, so it has to be visible
// rather than inferable.
func TestCacheHitRatio_ReadsOutTheProportionServedFromCache(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		want  float64
	}{
		{"nothing sent", Usage{}, 0},
		{"cold: all written, none read", Usage{InputTokens: 100, CacheCreationTokens: 900}, 0},
		{"warm: nearly all read", Usage{InputTokens: 100, CacheReadTokens: 900}, 0.9},
		{"fully cached", Usage{CacheReadTokens: 1000}, 1},
		{"uncacheable: no breakpoints at all", Usage{InputTokens: 1000}, 0},
	}
	for _, c := range cases {
		if got := cacheHitRatio(c.usage); got < c.want-0.001 || got > c.want+0.001 {
			t.Errorf("%s: ratio = %.3f, want %.3f", c.name, got, c.want)
		}
	}
}

// input_tokens is the uncached remainder only, so the prompt size is the
// sum of all three. Reading input_tokens alone under-reports it badly on a
// warm run, which would make a working cache look like a shrinking prompt.
func TestPromptTokens_SumsAllThreeInputCounters(t *testing.T) {
	u := Usage{InputTokens: 100, CacheReadTokens: 800, CacheCreationTokens: 50}
	if got := promptTokens(u); got != 950 {
		t.Errorf("promptTokens = %d, want 950", got)
	}
}

// The agent loop has to tell the request builder where the reusable prefix
// ends, or every run re-reads the whole notebook.
func TestAgentLoop_MarksTheProjectedPrefixAsStable(t *testing.T) {
	f := newNBFixture(t)
	fp := &fakeProvider{turns: []scriptedTurn{{text: "done"}}}
	withProvider(t, fp)

	f.addCell(t, "markdown", "context above")
	cell := f.addCell(t, "prompt", "the question")

	nbRequest(t, f.srv, "POST", f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*1000*1000*1000)

	reqs := fp.sent()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times, want 1", len(reqs))
	}
	if reqs[0].StablePrefixMessages != len(reqs[0].Messages) {
		t.Errorf("StablePrefixMessages = %d with %d messages — the projected prefix was not marked reusable",
			reqs[0].StablePrefixMessages, len(reqs[0].Messages))
	}
}
