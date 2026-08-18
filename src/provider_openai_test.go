package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #53 M4. The cross-transport contract lives in
// provider_conformance_test.go; what is here is the part that is only true
// of this transport — flavour selection, the fields that differ per
// endpoint, and the wire quirks that no Anthropic recording can express.

func TestOpenAIFlavour_IsSelectedByBaseURL(t *testing.T) {
	cases := []struct {
		baseURL   string
		name      string
		cache     CacheMode
		maxTokens string
	}{
		{"https://api.openai.com/v1", "openai", CacheAutomatic, "max_completion_tokens"},
		{"https://openrouter.ai/api/v1", "openrouter", CacheAutomatic, "max_tokens"},
		{"https://ai-gateway.vercel.sh/v1", "vercel-ai-gateway", CacheAutomatic, "max_tokens"},
		{"http://localhost:11434/v1", "ollama", CacheNone, "max_tokens"},
		{"http://127.0.0.1:8000/v1", "vllm", CacheNone, "max_tokens"},
		{"http://127.0.0.1:8080/v1", "llama.cpp", CacheNone, "max_tokens"},
		// An endpoint we have never met claims nothing it cannot back up.
		{"https://models.example.internal/v1", "openai-compatible", CacheNone, "max_tokens"},
	}
	for _, c := range cases {
		fl := openAIFlavourFor(c.baseURL)
		if fl.name != c.name {
			t.Errorf("%s -> %q, want %q", c.baseURL, fl.name, c.name)
		}
		if fl.caps.Cache != c.cache {
			t.Errorf("%s cache mode = %q, want %q", c.baseURL, fl.caps.Cache, c.cache)
		}
		if fl.completionTokensField != c.maxTokens {
			t.Errorf("%s token field = %q, want %q", c.baseURL, fl.completionTokensField, c.maxTokens)
		}
	}
}

// The reasoning models on api.openai.com reject max_tokens outright, and
// every other server in the family rejects max_completion_tokens or
// ignores it. Sending both would be a 400 on the strictest member.
func TestOpenAIRequest_SendsExactlyOneTokenLimitField(t *testing.T) {
	for _, base := range []string{"https://api.openai.com/v1", "http://localhost:11434/v1"} {
		fl := openAIFlavourFor(base)
		req, err := buildOpenAIRequest(Request{Model: "m", MaxTokens: 99, Messages: []Message{userText("hi")}}, fl)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(req)
		hasOld := strings.Contains(string(raw), `"max_tokens"`)
		hasNew := strings.Contains(string(raw), `"max_completion_tokens"`)
		if hasOld == hasNew {
			t.Errorf("%s sent max_tokens=%v and max_completion_tokens=%v — exactly one is right", base, hasOld, hasNew)
		}
	}
}

// A streamed turn reports no usage at all unless it is asked for, which
// would make every prompt cell look free and the notebook's dollar budget
// silently unenforceable.
func TestOpenAIRequest_AsksForUsageOnAStreamedTurn(t *testing.T) {
	req, err := buildOpenAIRequest(Request{Model: "m", Messages: []Message{userText("hi")}}, openAIFlavourFor("https://api.openai.com/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage was not set — the turn would report no tokens")
	}
}

// A local server's model ids are whatever it was started with, so there is
// nothing to default to. Guessing produces a 404 inside the user's first
// cell; saying so produces something they can act on.
func TestOpenAIRequest_RefusesToGuessAModelWhereThereIsNoDefault(t *testing.T) {
	_, err := buildOpenAIRequest(Request{Messages: []Message{userText("hi")}}, openAIFlavourFor("http://localhost:11434/v1"))
	if err == nil {
		t.Fatal("built a request with no model")
	}
	if !strings.Contains(err.Error(), "model") {
		t.Errorf("error = %q, want it to name the missing setting", err)
	}

	// Where there is a sensible default, it is used.
	req, err := buildOpenAIRequest(Request{Messages: []Message{userText("hi")}}, openAIFlavourFor("https://api.openai.com/v1"))
	if err != nil {
		t.Fatalf("api.openai.com has a default model but the build failed: %v", err)
	}
	if req.Model == "" {
		t.Error("no model on the request")
	}
}

// The two effort scales do not share a top. Clamping up rather than
// dropping keeps a user who asked for maximum effort from silently getting
// the default.
func TestOpenAIRequest_ClampsEffortToTheScaleThisAPIHas(t *testing.T) {
	for effort, want := range map[string]string{
		"low": "low", "medium": "medium", "high": "high",
		"xhigh": "high", "max": "high", "turbo": "",
	} {
		req, err := buildOpenAIRequest(
			Request{Model: "m", Effort: effort, Messages: []Message{userText("hi")}},
			openAIFlavourFor("https://api.openai.com/v1"))
		if err != nil {
			t.Fatal(err)
		}
		if req.ReasoningEffort != want {
			t.Errorf("effort %q -> %q, want %q", effort, req.ReasoningEffort, want)
		}
	}

	// Not sent at all where the endpoint does not take it: local servers
	// reject unknown parameters inconsistently, and a rejected request is
	// a failed cell rather than a degraded one.
	req, err := buildOpenAIRequest(
		Request{Model: "m", Effort: "high", Messages: []Message{userText("hi")}},
		openAIFlavourFor("http://localhost:11434/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if req.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want it withheld from an endpoint that does not support it", req.ReasoningEffort)
	}
}

// Reasoning has no signature anywhere in this family, and an unsigned
// thinking block is a 400 on the way back to Anthropic. A notebook that
// starts on a local model and is re-run on Anthropic must not carry one
// across.
func TestOpenAIResult_ReasoningComesBackUnsigned(t *testing.T) {
	res := replayOpenAIFixture(t, "reasoning.sse")
	if len(res.Content) == 0 || res.Content[0].Type != BlockThinking {
		t.Fatalf("no reasoning block: %+v", res.Content)
	}
	if res.Content[0].Signature != "" {
		t.Error("a signature was invented for reasoning that was never signed")
	}
}

// OpenRouter spells it "reasoning"; vLLM, Ollama and the DeepSeek-derived
// servers spell it "reasoning_content". Reading only one of them loses the
// whole of a reasoning model's output on half the family.
func TestOpenAIStream_ReadsBothSpellingsOfReasoning(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"reasoning":"routed thought"},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	res := replayOpenAIBody(t, body)
	if len(res.Content) != 2 || res.Content[0].Type != BlockThinking {
		t.Fatalf("blocks = %+v, want reasoning then text", res.Content)
	}
	if res.Content[0].Text != "routed thought" {
		t.Errorf("reasoning = %q", res.Content[0].Text)
	}
}

// ADR 0002's "a field's shape must not cost the line", one API down. A
// server that sends content as an array of parts must cost us the shape of
// that field and nothing else — declaring it as a string makes the whole
// chunk fail to decode, taking the finish reason and the usage with it.
func TestOpenAIStream_SurvivesAPolymorphicContentField(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"content":[{"type":"text","text":"from an array"}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2}}` + "\n\n" +
		"data: [DONE]\n\n"
	res := replayOpenAIBody(t, body)
	if res.StopReason != StopMaxTokens {
		t.Errorf("StopReason = %q, want %q — an unexpected field shape took the rest of the turn with it", res.StopReason, StopMaxTokens)
	}
	if res.Usage.OutputTokens != 2 {
		t.Errorf("Usage = %+v, want the counts to survive", res.Usage)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "from an array" {
		t.Errorf("content = %+v", res.Content)
	}
}

// Ollama and llama.cpp report finish_reason "stop" on a turn that made
// tool calls. Taking that literally ends the loop with the calls
// unanswered, so the calls themselves are the stronger evidence.
func TestOpenAIResult_TreatsToolCallsAsToolUseWhateverTheFinishReasonSays(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"read","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	res := replayOpenAIBody(t, body)
	if res.StopReason != StopToolUse {
		t.Errorf("StopReason = %q, want %q — the loop would have stopped with a call unanswered", res.StopReason, StopToolUse)
	}
}

// A refusal is a successful turn with nothing in it on Anthropic. OpenAI
// can also report one through message.refusal with finish_reason "stop",
// which would otherwise read as a completed answer.
func TestOpenAIResult_RefusalFieldIsAStopReasonNotProse(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"refusal":"I can't help with that"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	res := replayOpenAIBody(t, body)
	if res.StopReason != StopRefusal {
		t.Errorf("StopReason = %q, want %q", res.StopReason, StopRefusal)
	}
	if len(res.Content) != 0 {
		t.Errorf("content = %+v, want empty — a refusal rendered as prose reads as an answer", res.Content)
	}
}

// One malformed chunk mid-stream must not cost the turn: these servers do
// emit the occasional oddity, and failing the run discards everything
// already streamed.
func TestOpenAIStream_SkipsAMalformedChunkRatherThanFailingTheTurn(t *testing.T) {
	body := `data: {"choices":[{"index":0,"delta":{"content":"before"},"finish_reason":null}]}` + "\n\n" +
		"data: {not json at all\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":" after"},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	res := replayOpenAIBody(t, body)
	if len(res.Content) != 1 || res.Content[0].Text != "before after" {
		t.Errorf("content = %+v, want both halves", res.Content)
	}
}

// The cache is a prefix match over exact bytes on every transport that has
// one, so an identical logical request has to serialise identically. Go
// map iteration is randomised; one range over a map in the build path
// would destroy the cache silently, with nothing failing.
func TestOpenAIRequest_IsByteIdenticalAcrossRepeatedBuilds(t *testing.T) {
	fl := openAIFlavourFor("https://api.openai.com/v1")
	src := Request{
		Model:  "gpt-5-mini",
		System: "you are in a notebook",
		Messages: []Message{
			userText("first cell"),
			{Role: RoleAssistant, Content: []ContentBlock{
				{Type: BlockToolUse, ToolUseID: "c1", ToolName: "read",
					ToolInput: map[string]any{"path": "a", "limit": 3, "offset": 1}},
			}},
			{Role: RoleUser, Content: []ContentBlock{{Type: BlockToolResult, ToolUseID: "c1", Text: "ok"}}},
		},
		Tools: toolSpecsOfTest(),
	}
	hash := func() string {
		built, err := buildOpenAIRequest(src, fl)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(built)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	want := hash()
	for i := 0; i < 100; i++ {
		if got := hash(); got != want {
			t.Fatalf("build %d produced different bytes — the prefix would never match", i)
		}
	}
}

func toolSpecsOfTest() []ToolSpec {
	return []ToolSpec{
		{Name: "read", Description: "read a file", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required": []string{"path"}, "additionalProperties": false,
		}},
		{Name: "grep", Description: "search", InputSchema: map[string]any{
			"type": "object", "properties": map[string]any{"pattern": map[string]any{"type": "string"}},
			"required": []string{"pattern"}, "additionalProperties": false,
		}},
	}
}

// The environment shapes people already have, and the one that has no key
// at all.
func TestOpenAIConfigured_ReadsTheUsualEnvironment(t *testing.T) {
	for _, env := range []string{"OPENAI_BASE_URL", "OPENAI_API_KEY", "OLLAMA_HOST"} {
		t.Setenv(env, "")
	}
	if _, _, ok := openAIConfigured(); ok {
		t.Fatal("claimed a configured endpoint with nothing set")
	}

	t.Setenv("OPENAI_API_KEY", "sk-test")
	base, key, ok := openAIConfigured()
	if !ok || key != "sk-test" || !strings.Contains(base, "api.openai.com") {
		t.Errorf("a bare key gave %q/%q/%v, want the hosted endpoint", base, key, ok)
	}

	t.Setenv("OPENAI_API_KEY", "")
	// A local server needs no key, and requiring one would exclude every
	// one of them.
	t.Setenv("OLLAMA_HOST", "localhost:11434")
	base, key, ok = openAIConfigured()
	if !ok || key != "" || base != "http://localhost:11434/v1" {
		t.Errorf("OLLAMA_HOST gave %q/%q/%v", base, key, ok)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────

func replayOpenAIFixture(t *testing.T, name string) Result {
	t.Helper()
	return replayOpenAIBody(t, string(fixtureBytes(t, "openai", name)))
}

// replayOpenAIBody runs the real transport against an in-process server
// serving one body, so the SSE decoding is exercised rather than bypassed.
func replayOpenAIBody(t *testing.T, body string) Result {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	p := newOpenAIProvider(srv.URL+"/v1", "k")
	out := drainTurn(t.Context(), p, Request{Model: "test-model", Messages: []Message{userText("hi")}})
	if out.err != nil {
		t.Fatalf("turn failed: %v", out.err)
	}
	return out.result
}
