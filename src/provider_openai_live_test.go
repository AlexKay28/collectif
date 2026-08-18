package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// #53 M4 — the part of this phase that recorded fixtures cannot close.
//
// Everything else about the OpenAI transport is provable offline: the
// request shape, the SSE decoding, the accumulation of fragmented tool
// arguments, the usage arithmetic, the error mapping. What none of it
// proves is that a real server *accepts* what we send — that a role:"tool"
// message with our tool_call_id is matched to the assistant turn that
// asked, that strict schemas do not trip a local server, and that the
// usage block arrives at all when we ask for it.
//
// The fixtures in testdata/provider/ were written from each API's
// documented wire format rather than captured from a live call, so this
// test is the only thing standing between "conforms to what we believe the
// format is" and "conforms to the format". It skips loudly.
//
// Against a local model (no key, nothing billed):
//
//	OLLAMA_HOST=localhost:11434 COLLECTIF_LIVE_MODEL=qwen3:8b \
//	  go test ./src -run TestLive_OpenAIToolRoundTrip -v
//
// Against the hosted endpoint (costs a few cents):
//
//	OPENAI_API_KEY=... COLLECTIF_LIVE_MODEL=gpt-5-mini \
//	  go test ./src -run TestLive_OpenAIToolRoundTrip -v
func TestLive_OpenAIToolRoundTrip(t *testing.T) {
	base, key, ok := openAIConfigured()
	if !ok {
		t.Skip("no OpenAI-compatible endpoint configured — this is the one #53 check that cannot run offline; " +
			"OLLAMA_HOST=localhost:11434 COLLECTIF_LIVE_MODEL=qwen3:8b go test ./src -run TestLive_OpenAIToolRoundTrip -v")
	}
	p := newOpenAIProvider(base, key)
	model := strings.TrimSpace(os.Getenv("COLLECTIF_LIVE_MODEL"))
	if model == "" {
		model = p.defaultModel()
	}
	if model == "" {
		t.Skipf("%s has no default model — re-run with COLLECTIF_LIVE_MODEL=<id> naming one this endpoint serves", p.Name())
	}

	tool := ToolSpec{
		Name:        "lookup_city",
		Description: "Look up the city a user is in. Call it before answering questions about the weather.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"user": map[string]any{"type": "string"}},
			"required":             []string{"user"},
			"additionalProperties": false,
		},
	}
	req := Request{
		Model:     model,
		System:    "You are terse. Use the tools you are given.",
		Messages:  []Message{userText("What city is user alex in? Use the tool.")},
		Tools:     []ToolSpec{tool},
		MaxTokens: 512,
	}

	run := func(label string, req Request) Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
		defer cancel()
		stream, err := p.Stream(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer stream.Close()
		for {
			if _, err := stream.Next(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				// Distinguishing a failed stream from a finished one
				// matters here for the same reason it does in the M2.5
				// gate: a 400 read as EOF returns an empty result and
				// lands on the assertion below, pointing at the wrong bug.
				t.Fatalf("%s: stream failed (an endpoint or model problem, not a normalisation one): %v", label, err)
			}
		}
		res := stream.Result()
		t.Logf("%s: stop=%s blocks=%d %s", label, res.StopReason, len(res.Content), fmtUsage(res.Usage))
		return res
	}

	first := run("tool turn", req)
	var call *ContentBlock
	for i := range first.Content {
		if first.Content[i].Type == BlockToolUse {
			call = &first.Content[i]
		}
	}
	if call == nil {
		t.Fatalf("the model made no tool call (stop=%s) — either this model does not support tools, "+
			"or the tools array is not reaching it in a shape it understands", first.StopReason)
	}
	if call.ToolUseID == "" || call.ToolName != tool.Name {
		t.Errorf("tool call = %+v, want an id and the tool's name", call)
	}

	// The half that only a live server can answer: a role:"tool" message
	// carrying our tool_call_id has to be matched back to the assistant
	// turn above it. Getting this wrong is a 400 on OpenAI and a confused
	// answer on a local server.
	req.Messages = append(req.Messages,
		Message{Role: RoleAssistant, Content: first.Content},
		Message{Role: RoleUser, Content: []ContentBlock{{
			Type: BlockToolResult, ToolUseID: call.ToolUseID, Text: "Lisbon",
		}}},
	)
	second := run("answer turn", req)

	if second.StopReason != StopEndTurn {
		t.Errorf("second turn stopped with %q, want %q", second.StopReason, StopEndTurn)
	}
	var answer strings.Builder
	for _, b := range second.Content {
		if b.Type == BlockText {
			answer.WriteString(b.Text)
		}
	}
	if !strings.Contains(strings.ToLower(answer.String()), "lisbon") {
		t.Errorf("answer = %q, want it to use the tool result — the result did not reach the model", answer.String())
	}
	if promptTokens(second.Usage) == 0 {
		t.Error("no usage reported: stream_options.include_usage was refused or ignored, " +
			"so every cell on this endpoint would display as free and a dollar budget would not be enforceable")
	}
}

// A guard against the gate quietly never running, matching the one on the
// M2.5 cache gate: if an endpoint is configured, this test must not skip
// for some unrelated reason.
func TestLive_OpenAIGateIsRunnableWhenAnEndpointExists(t *testing.T) {
	if os.Getenv("OPENAI_BASE_URL") == "" && os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("OLLAMA_HOST") == "" {
		t.Skip("no endpoint in the environment")
	}
	if _, _, ok := openAIConfigured(); !ok {
		t.Error("an endpoint is set but openAIConfigured does not see it — the gate would skip silently")
	}
}
