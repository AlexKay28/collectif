package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// #50 M2 slice B. The transport splits into two pure functions — building
// the request and normalising the response — and a thin network wrapper.
// The pure halves are where the mistakes live and they are tested here; the
// network call itself cannot be exercised in this environment, which is
// stated in the commit rather than papered over.

func TestAnthropicRequest_UsesAdaptiveThinkingWithSummarisedDisplay(t *testing.T) {
	params := buildAnthropicRequest(Request{Messages: []Message{userText("hi")}})

	if params.Thinking.OfAdaptive == nil {
		t.Fatal("thinking is not adaptive — budget_tokens is removed on current models")
	}
	// The default is "omitted", which streams thinking blocks with empty
	// text. A notebook that renders reasoning would look broken rather
	// than absent, so the display is set explicitly.
	if got := params.Thinking.OfAdaptive.Display; got != anthropic.ThinkingConfigAdaptiveDisplaySummarized {
		t.Errorf("thinking display = %q, want summarized", got)
	}
}

func TestAnthropicRequest_DefaultsTheModelAndFloorsMaxTokens(t *testing.T) {
	params := buildAnthropicRequest(Request{Messages: []Message{userText("hi")}})
	if params.Model == "" {
		t.Error("no model set")
	}
	// Thinking counts against the same ceiling as the reply, so a limit
	// sized around the expected answer truncates mid-thought.
	if params.MaxTokens < 8000 {
		t.Errorf("MaxTokens = %d, want a generous floor", params.MaxTokens)
	}

	params = buildAnthropicRequest(Request{Model: "claude-sonnet-5", MaxTokens: 1234, Messages: []Message{userText("hi")}})
	if string(params.Model) != "claude-sonnet-5" {
		t.Errorf("Model = %q, want the requested one", params.Model)
	}
	if params.MaxTokens != 1234 {
		t.Errorf("MaxTokens = %d, want the requested one", params.MaxTokens)
	}
}

// The cache is a prefix match over exact bytes rendered tools → system →
// messages, so the breakpoint belongs on the last stable system block.
func TestAnthropicRequest_PutsACacheBreakpointOnTheSystemPrefix(t *testing.T) {
	params := buildAnthropicRequest(Request{
		System:   "you are in a notebook",
		Messages: []Message{userText("hi")},
	})
	if len(params.System) == 0 {
		t.Fatal("system prompt was dropped")
	}
	last := params.System[len(params.System)-1]
	if last.CacheControl.Type == "" {
		t.Error("no cache_control on the system prefix — every run would re-pay for the whole prefix")
	}
}

func TestAnthropicRequest_MapsToolsWithStrictSchemas(t *testing.T) {
	params := buildAnthropicRequest(Request{
		Messages: []Message{userText("hi")},
		Tools: []ToolSpec{{
			Name:        "read",
			Description: "read a file",
			InputSchema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"path": map[string]any{"type": "string"}},
				"required":             []string{"path"},
				"additionalProperties": false,
			},
		}},
	})
	if len(params.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(params.Tools))
	}
	tool := params.Tools[0].OfTool
	if tool == nil {
		t.Fatal("tool was not mapped to a custom tool")
	}
	if tool.Name != "read" || tool.Description.Value != "read a file" {
		t.Errorf("tool = %+v, want name and description carried across", tool)
	}
	if !tool.Strict.Value {
		t.Error("tool is not strict — arguments would not be guaranteed to validate")
	}
	if tool.InputSchema.ExtraFields["additionalProperties"] != false {
		t.Error("additionalProperties:false did not survive the mapping")
	}
	if len(tool.InputSchema.Required) != 1 || tool.InputSchema.Required[0] != "path" {
		t.Errorf("required = %v, want [path]", tool.InputSchema.Required)
	}
}

func TestAnthropicRequest_MapsEveryBlockKind(t *testing.T) {
	params := buildAnthropicRequest(Request{
		Messages: []Message{
			userText("do it"),
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockText, Text: "calling a tool"},
				{Type: BlockToolUse, ToolUseID: "tu_1", ToolName: "read", ToolInput: map[string]any{"path": "x"}},
			}},
			{Role: RoleUser, Content: []ContentBlock{
				{Type: BlockToolResult, ToolUseID: "tu_1", Text: "file contents", IsError: false},
			}},
		},
	})
	if len(params.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(params.Messages))
	}
	if params.Messages[1].Role != anthropic.MessageParamRoleAssistant {
		t.Errorf("second message role = %q", params.Messages[1].Role)
	}
	blocks := params.Messages[1].Content
	if len(blocks) != 2 || blocks[0].OfText == nil || blocks[1].OfToolUse == nil {
		t.Fatalf("assistant blocks not mapped: %+v", blocks)
	}
	if blocks[1].OfToolUse.ID != "tu_1" || blocks[1].OfToolUse.Name != "read" {
		t.Errorf("tool_use block = %+v", blocks[1].OfToolUse)
	}
	if params.Messages[2].Content[0].OfToolResult == nil {
		t.Fatalf("tool_result block not mapped: %+v", params.Messages[2].Content[0])
	}

	// Thinking is deliberately not replayed — see nb_project.go.
	withThinking := buildAnthropicRequest(Request{Messages: []Message{
		{Role: RoleAssistant, Content: []ContentBlock{{Type: BlockThinking, Text: "internal"}}},
	}})
	raw, _ := json.Marshal(withThinking.Messages)
	if strings.Contains(string(raw), "internal") {
		t.Error("a stored thinking summary was replayed to the API")
	}
}

func TestAnthropicRequest_MapsEffortWhenGiven(t *testing.T) {
	params := buildAnthropicRequest(Request{Effort: "low", Messages: []Message{userText("hi")}})
	if params.OutputConfig.Effort != anthropic.OutputConfigEffortLow {
		t.Errorf("effort = %q, want low", params.OutputConfig.Effort)
	}
	// An unknown level is dropped rather than sent through and rejected.
	params = buildAnthropicRequest(Request{Effort: "turbo", Messages: []Message{userText("hi")}})
	if params.OutputConfig.Effort != "" {
		t.Errorf("effort = %q, want it dropped", params.OutputConfig.Effort)
	}
}

// ─── Response normalisation ─────────────────────────────────────────────

func TestAnthropicResult_NormalisesContentAndUsage(t *testing.T) {
	msg := &anthropic.Message{
		Model:      "claude-opus-5",
		StopReason: anthropic.StopReasonToolUse,
		Usage: anthropic.Usage{
			InputTokens: 10, OutputTokens: 3,
			CacheReadInputTokens: 7, CacheCreationInputTokens: 5,
		},
	}
	if err := json.Unmarshal([]byte(`[
		{"type":"thinking","thinking":"pondering","signature":"sig"},
		{"type":"text","text":"here you go"},
		{"type":"tool_use","id":"tu_9","name":"grep","input":{"pattern":"x"}}
	]`), &msg.Content); err != nil {
		t.Fatalf("fixture: %v", err)
	}

	res := normaliseAnthropicResult(msg)
	if res.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopToolUse)
	}
	if res.Model != "claude-opus-5" {
		t.Errorf("Model = %q", res.Model)
	}
	want := Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 7, CacheCreationTokens: 5}
	if res.Usage != want {
		t.Errorf("Usage = %+v, want %+v", res.Usage, want)
	}
	if len(res.Content) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(res.Content), res.Content)
	}
	if res.Content[0].Type != BlockThinking || res.Content[0].Text != "pondering" {
		t.Errorf("thinking block = %+v", res.Content[0])
	}
	if res.Content[2].Type != BlockToolUse || res.Content[2].ToolName != "grep" || res.Content[2].ToolUseID != "tu_9" {
		t.Errorf("tool_use block = %+v", res.Content[2])
	}
	if res.Content[2].ToolInput["pattern"] != "x" {
		t.Errorf("tool input = %+v, want the decoded object", res.Content[2].ToolInput)
	}
}

// A refusal is a successful response with an empty content array. Reading
// the first block unconditionally panics; normalisation has to survive it.
func TestAnthropicResult_RefusalWithNoContentIsSafe(t *testing.T) {
	msg := &anthropic.Message{StopReason: anthropic.StopReasonRefusal}
	res := normaliseAnthropicResult(msg)
	if res.StopReason != StopRefusal {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopRefusal)
	}
	if len(res.Content) != 0 {
		t.Errorf("Content = %+v, want empty", res.Content)
	}
}

func TestAnthropicResult_MapsEveryStopReason(t *testing.T) {
	for from, want := range map[anthropic.StopReason]string{
		anthropic.StopReasonEndTurn:   StopEndTurn,
		anthropic.StopReasonToolUse:   StopToolUse,
		anthropic.StopReasonMaxTokens: StopMaxTokens,
		anthropic.StopReasonRefusal:   StopRefusal,
	} {
		if got := normaliseAnthropicResult(&anthropic.Message{StopReason: from}).StopReason; got != want {
			t.Errorf("stop reason %q mapped to %q, want %q", from, got, want)
		}
	}
}

func TestAnthropicProvider_AdvertisesCurrentModels(t *testing.T) {
	p := &anthropicProvider{}
	models := p.Models()
	if len(models) == 0 {
		t.Fatal("no models advertised")
	}
	var sawOpus5 bool
	for _, m := range models {
		if m.ID == "claude-opus-5" {
			sawOpus5 = true
			if m.ContextWindow != 1_000_000 {
				t.Errorf("claude-opus-5 window = %d, want 1000000", m.ContextWindow)
			}
		}
	}
	if !sawOpus5 {
		t.Error("catalog does not include the default model")
	}
}

// The credential heuristic decides which failure a key-less user sees: a
// clear "no provider configured", or an auth error inside their first cell
// that reads like a bug in the notebook.
func TestAnthropicCredentialsPresent_DetectsEachSource(t *testing.T) {
	// Isolate from the developer's real environment and profile.
	for _, env := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BEARER_TOKEN"} {
		t.Setenv(env, "")
	}
	empty := t.TempDir()
	t.Setenv("ANTHROPIC_CONFIG_DIR", empty)

	if anthropicCredentialsPresent() {
		t.Fatal("reported credentials with none available")
	}

	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if !anthropicCredentialsPresent() {
		t.Error("did not detect ANTHROPIC_API_KEY")
	}
	t.Setenv("ANTHROPIC_API_KEY", "")

	// A profile written by `ant auth login`, which the SDK reads with no
	// environment variable set at all.
	profiles := filepath.Join(empty, "credentials")
	if err := os.MkdirAll(profiles, 0o700); err != nil {
		t.Fatal(err)
	}
	if anthropicCredentialsPresent() {
		t.Error("an empty credentials directory is not a credential")
	}
	if err := os.WriteFile(filepath.Join(profiles, "default.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !anthropicCredentialsPresent() {
		t.Error("did not detect a stored profile")
	}
}

// initProviders must leave the loop in a usable state either way: tools are
// always available, and a missing provider is nil rather than a broken one.
func TestInitProviders_LeavesACleanStateWithoutCredentials(t *testing.T) {
	prevP, prevList, prevT := activeProvider, activeProviders, activeTools
	t.Cleanup(func() { activeProvider, activeProviders, activeTools = prevP, prevList, prevT })

	// Every transport, not just Anthropic's (#53). A developer with
	// OLLAMA_HOST exported would otherwise see this fail on their machine
	// and nowhere else, which is the least useful kind of red.
	for _, env := range []string{
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BEARER_TOKEN",
		"OPENAI_BASE_URL", "OPENAI_API_KEY", "OLLAMA_HOST",
	} {
		t.Setenv(env, "")
	}
	t.Setenv("ANTHROPIC_CONFIG_DIR", t.TempDir())

	activeProvider, activeProviders = nil, nil
	initProviders()

	if activeProvider != nil {
		t.Error("installed a provider with no credentials — the first cell would fail with an auth error")
	}
	if len(activeTools) == 0 {
		t.Error("tools should be registered regardless of which provider is in use")
	}
}
