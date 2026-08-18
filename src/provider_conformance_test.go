package main

// provider_conformance_test.go — one suite, every transport. #53 (M4).
//
// ADR 0001 §6 asked for a "normalization test suite against both
// transports" and ADR 0002 kept it while dropping the phase's priority.
// This is that suite, and it is written first on purpose: the second
// transport should be built *against* a contract rather than beside the
// first one, or "provider-agnostic" means "whatever provider_anthropic.go
// happened to do".
//
// Every case runs against a real transport talking real HTTP to a test
// server that replays a recorded response, so the SSE decoding, the JSON
// shapes and the accumulation are all exercised — a fake Provider would
// prove only that the fake is consistent with itself. Nothing here touches
// the network: fixtures live in testdata/provider/<transport>/.
//
// Two halves, and both matter:
//
//   - The *response* half asserts that two different wire formats produce
//     byte-identical Chunks, ContentBlocks, stop reasons and Usage.
//   - The *request* half lifts each transport's outgoing JSON back into
//     one shape (wireRequest) and asserts those match too. That is where
//     the documented divergences — tool_use blocks vs a tool_calls array,
//     tool_result blocks vs role:"tool" messages, a top-level system field
//     vs a system message — stop being prose and become an equality check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// ─── Targets ────────────────────────────────────────────────────────────

// conformanceTarget is one transport under test.
type conformanceTarget struct {
	name string

	// start points the real transport at a handler in this process.
	start func(t *testing.T, h http.Handler) Provider

	// lift turns this transport's outgoing request body into the common
	// shape. It is the normalisation table, executable.
	lift func(t *testing.T, body []byte) wireRequest

	// model is an id this transport will accept in a request.
	model string
}

func conformanceTargets() []conformanceTarget {
	return []conformanceTarget{
		{
			name:  "anthropic",
			model: "claude-opus-5",
			start: func(t *testing.T, h http.Handler) Provider {
				srv := httptest.NewServer(h)
				t.Cleanup(srv.Close)
				// Retries off: a 429 fixture would otherwise be retried
				// with backoff and the error-mapping cases would take
				// minutes to fail.
				return newAnthropicProvider(
					option.WithBaseURL(srv.URL),
					option.WithAPIKey("test-key"),
					option.WithMaxRetries(0),
				)
			},
			lift: liftAnthropicRequest,
		},
		{
			name:  "openai",
			model: "gpt-5-mini",
			start: func(t *testing.T, h http.Handler) Provider {
				srv := httptest.NewServer(h)
				t.Cleanup(srv.Close)
				return newOpenAIProvider(srv.URL+"/v1", "test-key")
			},
			lift: liftOpenAIRequest,
		},
	}
}

// ─── The common request shape ───────────────────────────────────────────

// wireTurn is one piece of conversation as it went out on the wire,
// stripped of which format carried it.
type wireTurn struct {
	Role     string // user | assistant
	Kind     string // text | tool_use | tool_result
	Text     string
	ToolID   string
	ToolName string
	IsError  bool
}

type wireRequest struct {
	Model     string
	System    string
	MaxTokens int
	Tools     []string
	Turns     []wireTurn
	Stream    bool
}

func liftAnthropicRequest(t *testing.T, body []byte) wireRequest {
	t.Helper()
	var raw struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Stream    bool   `json:"stream"`
		System    []struct {
			Text string `json:"text"`
		} `json:"system"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Text      string          `json:"text"`
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
				IsError   bool            `json:"is_error"`
				Content   json.RawMessage `json:"content"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("anthropic request is not the shape this test expects: %v\n%s", err, body)
	}
	out := wireRequest{Model: raw.Model, MaxTokens: raw.MaxTokens, Stream: raw.Stream}
	for _, s := range raw.System {
		out.System += s.Text
	}
	for _, tool := range raw.Tools {
		out.Tools = append(out.Tools, tool.Name)
	}
	for _, m := range raw.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				out.Turns = append(out.Turns, wireTurn{Role: m.Role, Kind: "text", Text: b.Text})
			case "tool_use":
				out.Turns = append(out.Turns, wireTurn{
					Role: m.Role, Kind: "tool_use", ToolID: b.ID, ToolName: b.Name,
					Text: canonicalJSON(t, b.Input),
				})
			case "tool_result":
				out.Turns = append(out.Turns, wireTurn{
					Role: m.Role, Kind: "tool_result", ToolID: b.ToolUseID,
					Text: anthropicToolResultText(t, b.Content), IsError: b.IsError,
				})
			}
		}
	}
	return out
}

// anthropicToolResultText unwraps the block array the SDK renders a tool
// result content into.
func anthropicToolResultText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			b.WriteString(blk.Text)
		}
		return b.String()
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

func liftOpenAIRequest(t *testing.T, body []byte) wireRequest {
	t.Helper()
	var raw struct {
		Model               string `json:"model"`
		MaxTokens           int    `json:"max_tokens"`
		MaxCompletionTokens int    `json:"max_completion_tokens"`
		Stream              bool   `json:"stream"`
		Tools               []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("openai request is not the shape this test expects: %v\n%s", err, body)
	}
	out := wireRequest{Model: raw.Model, MaxTokens: raw.MaxTokens, Stream: raw.Stream}
	if out.MaxTokens == 0 {
		out.MaxTokens = raw.MaxCompletionTokens
	}
	for _, tool := range raw.Tools {
		out.Tools = append(out.Tools, tool.Function.Name)
	}
	for _, m := range raw.Messages {
		switch m.Role {
		case "system", "developer":
			out.System += m.Content
		case "tool":
			// Divergence 2: a tool result is its own message here and a
			// block inside the next user turn on Anthropic. Divergence 2b:
			// there is no is_error field, so the transport marks failures
			// in the text — see openAIToolErrorPrefix.
			text, isErr := strings.CutPrefix(m.Content, openAIToolErrorPrefix)
			out.Turns = append(out.Turns, wireTurn{
				Role: "user", Kind: "tool_result", ToolID: m.ToolCallID,
				Text: text, IsError: isErr,
			})
		default:
			if m.Content != "" {
				out.Turns = append(out.Turns, wireTurn{Role: m.Role, Kind: "text", Text: m.Content})
			}
			for _, call := range m.ToolCalls {
				out.Turns = append(out.Turns, wireTurn{
					Role: m.Role, Kind: "tool_use", ToolID: call.ID, ToolName: call.Function.Name,
					Text: canonicalJSON(t, json.RawMessage(call.Function.Arguments)),
				})
			}
		}
	}
	return out
}

// canonicalJSON re-encodes so two transports that spell the same object
// differently (whitespace, key order) still compare equal.
func canonicalJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return "{}"
	}
	var v map[string]any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("tool arguments are not an object: %v (%s)", err, raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ─── Fixture plumbing ───────────────────────────────────────────────────

// recorder serves one recorded exchange and keeps the request that asked
// for it, so a case can assert on both halves.
type recorder struct {
	mu   sync.Mutex
	body []byte
}

func (r *recorder) request() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.body...)
}

func fixtureBytes(t *testing.T, target, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", "provider", target, name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing fixture %s — the suite must run offline, so a scenario without a recording is a failure, not a skip: %v", path, err)
	}
	return b
}

// serveFixture replays a recording. SSE bodies are written in one shot and
// flushed: nothing here is testing chunk timing, only chunk content.
func serveFixture(t *testing.T, target, name string, status int, rec *recorder) http.Handler {
	t.Helper()
	body := fixtureBytes(t, target, name)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.body = read
		rec.mu.Unlock()

		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
}

// turnOutcome is everything a caller of the seam can observe.
type turnOutcome struct {
	chunks []Chunk
	result Result
	err    error
}

// drainTurn runs one turn to completion.
//
// It accepts an error from Stream *or* from Next because the two
// transports genuinely differ there and the seam does not hide it: the
// Anthropic SDK opens its stream lazily and surfaces a 401 on the first
// Next, while a hand-rolled client knows at once. nb_agent.go already
// handles both paths; this mirrors it rather than inventing a third
// contract that only the tests use.
func drainTurn(ctx context.Context, p Provider, req Request) turnOutcome {
	var out turnOutcome
	stream, err := p.Stream(ctx, req)
	if err != nil {
		out.err = err
		return out
	}
	defer stream.Close()
	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			out.err = err
			break
		}
		out.chunks = append(out.chunks, chunk)
	}
	out.result = stream.Result()
	return out
}

func chunkKinds(chunks []Chunk) []ChunkType {
	out := make([]ChunkType, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, c.Type)
	}
	return out
}

func chunkTextOf(chunks []Chunk, kind ChunkType) string {
	var b strings.Builder
	for _, c := range chunks {
		if c.Type == kind {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// ─── Response conformance ───────────────────────────────────────────────

// providerScenario is one recorded turn and the single normalised outcome
// every transport must produce from it.
type providerScenario struct {
	name string
	// fixture is the file name under testdata/provider/<target>/.
	fixture string
	status  int
	req     Request

	wantChunks []ChunkType
	wantText   string
	wantThink  string
	wantStop   string
	wantUsage  Usage
	wantBlocks []ContentBlock
	wantErr    ProviderErrorKind
}

func conformanceScenarios() []providerScenario {
	simpleReq := Request{Messages: []Message{userText("hello")}, MaxTokens: 1024}
	return []providerScenario{
		{
			name:       "plain text turn",
			fixture:    "text.sse",
			req:        simpleReq,
			wantChunks: []ChunkType{ChunkText, ChunkText},
			wantText:   "Hello, world",
			wantStop:   StopEndTurn,
			// The prompt was 1000 tokens, 100 of them served from cache.
			// The two APIs report that differently — Anthropic's
			// input_tokens excludes the cached part and OpenAI's
			// prompt_tokens includes it — so a transport that copies the
			// field across double-counts. promptTokens() has to come to
			// 1000 either way.
			wantUsage:  Usage{InputTokens: 900, OutputTokens: 20, CacheReadTokens: 100},
			wantBlocks: []ContentBlock{{Type: BlockText, Text: "Hello, world"}},
		},
		{
			name:       "reasoning arrives before the answer",
			fixture:    "reasoning.sse",
			req:        simpleReq,
			wantChunks: []ChunkType{ChunkThinking, ChunkText},
			wantThink:  "weighing it up",
			wantText:   "the answer",
			wantStop:   StopEndTurn,
			wantUsage:  Usage{InputTokens: 10, OutputTokens: 5},
			wantBlocks: []ContentBlock{
				{Type: BlockThinking, Text: "weighing it up"},
				{Type: BlockText, Text: "the answer"},
			},
		},
		{
			name:       "one tool call",
			fixture:    "tool_call.sse",
			req:        simpleReq,
			wantChunks: []ChunkType{ChunkText, ChunkToolUse},
			wantText:   "let me look",
			wantStop:   StopToolUse,
			wantUsage:  Usage{InputTokens: 30, OutputTokens: 12},
			wantBlocks: []ContentBlock{
				{Type: BlockText, Text: "let me look"},
				{Type: BlockToolUse, ToolUseID: "call_1", ToolName: "read",
					ToolInput: map[string]any{"path": "notes.md"}},
			},
		},
		{
			// Both APIs can put two calls in one turn, and both stream the
			// arguments in fragments. Splitting the results across two user
			// turns teaches the model to stop making parallel calls, so the
			// loop depends on getting both back from one response.
			name:       "two tool calls in one turn",
			fixture:    "parallel_tools.sse",
			req:        simpleReq,
			wantChunks: []ChunkType{ChunkToolUse, ChunkToolUse},
			wantStop:   StopToolUse,
			wantUsage:  Usage{InputTokens: 40, OutputTokens: 30},
			wantBlocks: []ContentBlock{
				{Type: BlockToolUse, ToolUseID: "call_1", ToolName: "read",
					ToolInput: map[string]any{"path": "a.md"}},
				{Type: BlockToolUse, ToolUseID: "call_2", ToolName: "grep",
					ToolInput: map[string]any{"pattern": "needle", "path": "b.md"}},
			},
		},
		{
			name:       "output limit reached",
			fixture:    "truncated.sse",
			req:        simpleReq,
			wantChunks: []ChunkType{ChunkText},
			wantText:   "half a sen",
			wantStop:   StopMaxTokens,
			wantUsage:  Usage{InputTokens: 12, OutputTokens: 4},
			wantBlocks: []ContentBlock{{Type: BlockText, Text: "half a sen"}},
		},
		{
			// A refusal is a *successful* response with nothing in it.
			// Code that reads the first block unconditionally panics on it,
			// which is why the loop checks the stop reason first — and why
			// both transports have to report it as a stop reason rather
			// than as an error.
			name:       "refusal is a successful empty turn",
			fixture:    "refusal.sse",
			req:        simpleReq,
			wantStop:   StopRefusal,
			wantUsage:  Usage{InputTokens: 8, OutputTokens: 0},
			wantBlocks: nil,
		},
		{
			name:    "bad credentials",
			fixture: "error_401.json",
			status:  http.StatusUnauthorized,
			req:     simpleReq,
			wantErr: ProviderErrAuth,
		},
		{
			name:    "rate limited",
			fixture: "error_429.json",
			status:  http.StatusTooManyRequests,
			req:     simpleReq,
			wantErr: ProviderErrRateLimit,
		},
		{
			name:    "malformed request",
			fixture: "error_400.json",
			status:  http.StatusBadRequest,
			req:     simpleReq,
			wantErr: ProviderErrBadRequest,
		},
		{
			name:    "provider is down",
			fixture: "error_500.json",
			status:  http.StatusInternalServerError,
			req:     simpleReq,
			wantErr: ProviderErrServer,
		},
	}
}

func TestProviderConformance_Responses(t *testing.T) {
	for _, target := range conformanceTargets() {
		for _, sc := range conformanceScenarios() {
			t.Run(target.name+"/"+sc.name, func(t *testing.T) {
				status := sc.status
				if status == 0 {
					status = http.StatusOK
				}
				rec := &recorder{}
				p := target.start(t, serveFixture(t, target.name, sc.fixture, status, rec))

				req := sc.req
				req.Model = target.model
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				got := drainTurn(ctx, p, req)

				if sc.wantErr != "" {
					assertProviderError(t, got.err, sc.wantErr, status)
					return
				}
				if got.err != nil {
					t.Fatalf("turn failed: %v", got.err)
				}

				if kinds := chunkKinds(got.chunks); !sameChunkKinds(kinds, sc.wantChunks) {
					t.Errorf("chunk order = %v, want %v", kinds, sc.wantChunks)
				}
				if txt := chunkTextOf(got.chunks, ChunkText); txt != sc.wantText {
					t.Errorf("streamed text = %q, want %q", txt, sc.wantText)
				}
				if think := chunkTextOf(got.chunks, ChunkThinking); think != sc.wantThink {
					t.Errorf("streamed reasoning = %q, want %q", think, sc.wantThink)
				}
				if got.result.StopReason != sc.wantStop {
					t.Errorf("StopReason = %q, want %q", got.result.StopReason, sc.wantStop)
				}
				if got.result.Usage != sc.wantUsage {
					t.Errorf("Usage = %+v, want %+v", got.result.Usage, sc.wantUsage)
				}
				assertBlocks(t, got.result.Content, sc.wantBlocks)
			})
		}
	}
}

// sameChunkKinds ignores how finely a transport chopped its text, because
// that is a property of the recording and not of the transport: what has
// to match is the *order* of kinds.
func sameChunkKinds(got, want []ChunkType) bool {
	collapse := func(in []ChunkType) []ChunkType {
		var out []ChunkType
		for _, k := range in {
			if len(out) == 0 || out[len(out)-1] != k || k == ChunkToolUse {
				out = append(out, k)
			}
		}
		return out
	}
	g, w := collapse(got), collapse(want)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

func assertBlocks(t *testing.T, got, want []ContentBlock) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d content blocks, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Type != w.Type || g.Text != w.Text {
			t.Errorf("block %d = {%s %q}, want {%s %q}", i, g.Type, g.Text, w.Type, w.Text)
		}
		if g.ToolUseID != w.ToolUseID || g.ToolName != w.ToolName {
			t.Errorf("block %d tool = %s/%s, want %s/%s", i, g.ToolUseID, g.ToolName, w.ToolUseID, w.ToolName)
		}
		if w.ToolInput != nil {
			gb, _ := json.Marshal(g.ToolInput)
			wb, _ := json.Marshal(w.ToolInput)
			if string(gb) != string(wb) {
				t.Errorf("block %d tool input = %s, want %s — streamed argument fragments were not reassembled", i, gb, wb)
			}
		}
	}
}

func assertProviderError(t *testing.T, err error, want ProviderErrorKind, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %s error, got a successful turn", want)
	}
	var pe *ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v (%T) is not a *ProviderError — the loop cannot tell a bad key from an outage without one", err, err)
	}
	if pe.Kind != want {
		t.Errorf("error kind = %q, want %q (%v)", pe.Kind, want, err)
	}
	if pe.Status != status {
		t.Errorf("error status = %d, want %d", pe.Status, status)
	}
	if !strings.Contains(err.Error(), "fixture-detail") {
		t.Errorf("error %q drops the provider's own explanation, which is the only part that says what to fix", err)
	}
}

// ─── Request conformance ────────────────────────────────────────────────

// The six documented divergences, as one request that exercises all of
// them and one expected shape both transports must render it into.
func TestProviderConformance_RequestShape(t *testing.T) {
	req := Request{
		System:    "you are in a notebook",
		MaxTokens: 4096,
		Effort:    "low",
		Messages: []Message{
			userText("read two files"),
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockText, Text: "on it"},
				{Type: BlockToolUse, ToolUseID: "call_1", ToolName: "read", ToolInput: map[string]any{"path": "a.md"}},
				{Type: BlockToolUse, ToolUseID: "call_2", ToolName: "read", ToolInput: map[string]any{"path": "b.md"}},
			}},
			{Role: RoleUser, Content: []ContentBlock{
				{Type: BlockToolResult, ToolUseID: "call_1", Text: "contents of a"},
				{Type: BlockToolResult, ToolUseID: "call_2", Text: "read b.md: no such file", IsError: true},
			}},
			userText("so what is in them?"),
		},
		Tools: []ToolSpec{
			{Name: "read", Description: "read a file", InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []string{"path"}, "additionalProperties": false,
			}},
			{Name: "grep", Description: "search", InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"pattern": map[string]any{"type": "string"}},
				"required":   []string{"pattern"}, "additionalProperties": false,
			}},
		},
	}

	want := wireRequest{
		System:    "you are in a notebook",
		MaxTokens: 4096,
		Stream:    true,
		Tools:     []string{"read", "grep"},
		Turns: []wireTurn{
			{Role: "user", Kind: "text", Text: "read two files"},
			{Role: "assistant", Kind: "text", Text: "on it"},
			{Role: "assistant", Kind: "tool_use", ToolID: "call_1", ToolName: "read", Text: `{"path":"a.md"}`},
			{Role: "assistant", Kind: "tool_use", ToolID: "call_2", ToolName: "read", Text: `{"path":"b.md"}`},
			{Role: "user", Kind: "tool_result", ToolID: "call_1", Text: "contents of a"},
			{Role: "user", Kind: "tool_result", ToolID: "call_2", Text: "read b.md: no such file", IsError: true},
			{Role: "user", Kind: "text", Text: "so what is in them?"},
		},
	}

	for _, target := range conformanceTargets() {
		t.Run(target.name, func(t *testing.T) {
			rec := &recorder{}
			p := target.start(t, serveFixture(t, target.name, "text.sse", http.StatusOK, rec))
			r := req
			r.Model = target.model
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if out := drainTurn(ctx, p, r); out.err != nil {
				t.Fatalf("turn failed: %v", out.err)
			}

			got := target.lift(t, rec.request())
			if got.Model != target.model {
				t.Errorf("model = %q, want %q", got.Model, target.model)
			}
			if got.System != want.System {
				t.Errorf("system = %q, want %q — the system prompt has to arrive wherever this API keeps it", got.System, want.System)
			}
			if got.MaxTokens != want.MaxTokens {
				t.Errorf("max tokens = %d, want %d", got.MaxTokens, want.MaxTokens)
			}
			if !got.Stream {
				t.Error("request was not a streaming one")
			}
			if strings.Join(got.Tools, ",") != strings.Join(want.Tools, ",") {
				t.Errorf("tools = %v, want %v", got.Tools, want.Tools)
			}
			if len(got.Turns) != len(want.Turns) {
				t.Fatalf("got %d turns, want %d:\n%+v", len(got.Turns), len(want.Turns), got.Turns)
			}
			for i := range want.Turns {
				if got.Turns[i] != want.Turns[i] {
					t.Errorf("turn %d =\n  %+v\nwant\n  %+v", i, got.Turns[i], want.Turns[i])
				}
			}
		})
	}
}

// ─── Cancellation ───────────────────────────────────────────────────────

// A cancelled turn was still billed for its prompt, so the loop salvages
// whatever the stream accumulated (nb_agent.go). That only works if every
// transport unblocks promptly on cancellation and leaves a readable
// Result behind rather than panicking or hanging.
func TestProviderConformance_CancellationMidStream(t *testing.T) {
	for _, target := range conformanceTargets() {
		t.Run(target.name, func(t *testing.T) {
			started := make(chan struct{})
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })

			body := fixtureBytes(t, target.name, "text.sse")
			// Everything up to the first text delta, then silence: the
			// stream is open and mid-turn when the cancel lands.
			head := firstDeltas(string(body))

			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, head)
				w.(http.Flusher).Flush()
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
			p := target.start(t, h)

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan turnOutcome, 1)
			go func() {
				done <- drainTurn(ctx, p, Request{Model: target.model, Messages: []Message{userText("hi")}})
			}()

			select {
			case <-started:
			case <-time.After(10 * time.Second):
				cancel()
				t.Fatal("the transport never sent its request")
			}
			cancel()

			select {
			case out := <-done:
				if out.err == nil {
					t.Fatal("a cancelled turn reported success")
				}
				if !errors.Is(out.err, context.Canceled) {
					t.Errorf("error = %v, want it to wrap context.Canceled so the loop can tell an interrupt from a failure", out.err)
				}
				// An interrupt is not a provider failure. Classifying it
				// as one puts "openai (transport): context canceled" in
				// the cell of a run the user stopped on purpose.
				var pe *ProviderError
				if errors.As(out.err, &pe) {
					t.Errorf("a cancelled turn was classified as a %s provider error: %v", pe.Kind, out.err)
				}
				// Result must be callable on the error path: the loop reads
				// it to account for a partially-billed turn.
				_ = out.result
			case <-time.After(10 * time.Second):
				t.Fatal("cancelling the context did not unblock the stream")
			}
		})
	}
}

// firstDeltas keeps the head of a recorded SSE body — enough events to
// open the turn, not enough to finish it.
func firstDeltas(body string) string {
	events := strings.SplitAfter(body, "\n\n")
	keep := 3
	if len(events) < keep {
		keep = len(events)
	}
	return strings.Join(events[:keep], "")
}

// ─── Capabilities ───────────────────────────────────────────────────────

// Every transport has to answer what it can do, because the notebook shows
// that answer rather than guessing. The specific failure this prevents is
// the per-cell cache chip reading "0% cached" on a transport with no
// breakpoints at all: zero reads as a cache miss, a miss reads as a bug,
// and the user goes looking for one that is not there.
func TestProviderConformance_CapabilitiesAreDeclared(t *testing.T) {
	for _, target := range conformanceTargets() {
		t.Run(target.name, func(t *testing.T) {
			p := target.start(t, http.NotFoundHandler())
			caps := p.Capabilities()
			switch caps.Cache {
			case CacheExplicit, CacheAutomatic, CacheNone:
			default:
				t.Errorf("cache mode = %q, want one of explicit/automatic/none", caps.Cache)
			}
			if p.Name() == "" {
				t.Error("transport has no name — /api/providers would show a blank row")
			}
		})
	}
}

// The suite's target list is written by hand, because each transport
// needs recordings in its own wire format and no registry can produce
// those. That makes "we added a third transport and forgot the suite" a
// silent gap, so this closes it: whatever initProviders can install has to
// appear above.
func TestProviderConformance_CoversEveryTransportTheProcessCanInstall(t *testing.T) {
	prevP, prevList, prevT := activeProvider, activeProviders, activeTools
	t.Cleanup(func() { activeProvider, activeProviders, activeTools = prevP, prevList, prevT })

	// Credentials for everything, so every transport installs. Nothing
	// here opens a connection — construction is local.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	t.Setenv("OPENAI_API_KEY", "sk-test")
	t.Setenv("OLLAMA_HOST", "")
	initProviders()

	covered := map[string]bool{}
	for _, target := range conformanceTargets() {
		covered[target.name] = true
	}
	for _, p := range activeProviders {
		if !covered[p.Name()] {
			t.Errorf("transport %q can be installed at boot but no conformance target covers it — "+
				"add recordings under testdata/provider/%s/ and a target to conformanceTargets()", p.Name(), p.Name())
		}
	}
	if len(activeProviders) == 0 {
		t.Fatal("no transports installed with every credential set")
	}
}

func TestProviderConformance_ModelCatalogsAreUsable(t *testing.T) {
	for _, target := range conformanceTargets() {
		t.Run(target.name, func(t *testing.T) {
			p := target.start(t, http.NotFoundHandler())
			for _, m := range p.Models() {
				if m.ID == "" {
					t.Error("catalog entry with no id")
				}
				if m.ContextWindow <= 0 {
					t.Errorf("%s has no context window — checkRequestFits would fall back to a guess", m.ID)
				}
				if m.MaxOutput <= 0 {
					t.Errorf("%s has no max output", m.ID)
				}
			}
		})
	}
}

// fmtUsage keeps failure messages readable when a transport double-counts
// its cached prompt.
func fmtUsage(u Usage) string {
	return fmt.Sprintf("prompt=%d (uncached=%d read=%d write=%d) output=%d",
		promptTokens(u), u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens, u.OutputTokens)
}
