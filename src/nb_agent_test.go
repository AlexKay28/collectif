package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// #50 M2 slice A. The agent loop, driven by an in-process fake provider.
// No network: the Provider seam exists so the loop can be exercised
// exhaustively offline, and so a second transport in M4 has a contract to
// meet rather than a shape to guess at.

// ─── Fake provider ──────────────────────────────────────────────────────

// scriptedTurn is one model response the fake will produce, in order.
type scriptedTurn struct {
	text       string
	thinking   string
	toolName   string
	toolInput  map[string]any
	stopReason string
	usage      Usage
	err        error
}

type fakeProvider struct {
	mu       sync.Mutex
	turns    []scriptedTurn
	requests []Request // what the loop actually sent, for assertions
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Models() []ModelInfo {
	return []ModelInfo{{
		ID: "fake-1", ContextWindow: 200_000, MaxOutput: 8_000,
		InputUSDPerMTok: 5, OutputUSDPerMTok: 25,
		CacheReadUSDPerMTok: 0.5, CacheWriteUSDPerMTok: 6.25,
	}}
}

// The fake claims explicit caching so tests that assert on the cache
// display exercise the same branch the Anthropic transport takes.
func (f *fakeProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		Cache: CacheExplicit, Reasoning: true, SignedReasoning: true, Effort: true, Usage: true,
	}
}

func (f *fakeProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, req)
	if len(f.turns) == 0 {
		return nil, io.ErrUnexpectedEOF
	}
	turn := f.turns[0]
	f.turns = f.turns[1:]
	if turn.err != nil {
		return nil, turn.err
	}
	return newFakeStream(turn), nil
}

func (f *fakeProvider) sent() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

type fakeStream struct {
	chunks []Chunk
	i      int
	result Result
}

func newFakeStream(t scriptedTurn) *fakeStream {
	var chunks []Chunk
	var blocks []ContentBlock
	if t.thinking != "" {
		chunks = append(chunks, Chunk{Type: ChunkThinking, Text: t.thinking})
		blocks = append(blocks, ContentBlock{Type: BlockThinking, Text: t.thinking})
	}
	// Two text chunks so the test proves streaming rather than one blob.
	if t.text != "" {
		half := len(t.text) / 2
		chunks = append(chunks,
			Chunk{Type: ChunkText, Text: t.text[:half]},
			Chunk{Type: ChunkText, Text: t.text[half:]},
		)
		blocks = append(blocks, ContentBlock{Type: BlockText, Text: t.text})
	}
	stop := t.stopReason
	if t.toolName != "" {
		call := &ToolCall{ID: "call-1", Name: t.toolName, Input: t.toolInput}
		chunks = append(chunks, Chunk{Type: ChunkToolUse, ToolCall: call})
		blocks = append(blocks, ContentBlock{
			Type: BlockToolUse, ToolUseID: call.ID, ToolName: call.Name, ToolInput: call.Input,
		})
		if stop == "" {
			stop = StopToolUse
		}
	}
	if stop == "" {
		stop = StopEndTurn
	}
	return &fakeStream{chunks: chunks, result: Result{Content: blocks, StopReason: stop, Usage: t.usage, Model: "fake-1"}}
}

func (s *fakeStream) Next() (Chunk, error) {
	if s.i >= len(s.chunks) {
		return Chunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}
func (s *fakeStream) Result() Result { return s.result }
func (s *fakeStream) Close() error   { return nil }

// ─── Fake tool ──────────────────────────────────────────────────────────

type fakeTool struct {
	name    string
	calls   int
	result  string
	isError bool
}

func (t *fakeTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        t.name,
		Description: "a test tool",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}
func (t *fakeTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	t.calls++
	return t.result, t.isError, nil
}

// withProvider installs a provider for one test.
func withProvider(t *testing.T, p Provider) *fakeProvider {
	t.Helper()
	prev := activeProvider
	activeProvider = p
	t.Cleanup(func() { activeProvider = prev })
	if fp, ok := p.(*fakeProvider); ok {
		return fp
	}
	return nil
}

func withTools(t *testing.T, tools ...Tool) {
	t.Helper()
	prev := activeTools
	activeTools = tools
	t.Cleanup(func() { activeTools = prev })
}

// ─── Tests ──────────────────────────────────────────────────────────────

func TestRunPromptCell_NoProviderConfiguredIsAClearError(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, nil)
	cell := f.addCell(t, "prompt", "hello")

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503 when no provider is configured (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "provider") {
		t.Errorf("error body %q should say what is missing", rec.Body.String())
	}
}

func TestRunPromptCell_TextTurnRecordsOutputAndUsage(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &fakeProvider{turns: []scriptedTurn{{
		thinking: "considering",
		text:     "the answer is four",
		usage:    Usage{InputTokens: 120, OutputTokens: 8, CacheReadTokens: 100},
	}}})
	cell := f.addCell(t, "prompt", "what is 2+2?")

	if rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil); rec.Code >= 300 {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellOK {
		t.Fatalf("State = %q, want %q (outputs %+v)", c.State, CellOK, c.Outputs)
	}
	if got := f.outputText(c); !strings.Contains(got, "the answer is four") {
		t.Errorf("outputs = %q, want the model's text", got)
	}
	if !hasOutputOfType(c, OutputThinking) {
		t.Error("reasoning was not recorded as a thinking output")
	}
	// Usage arrives first-hand on the response — the whole point of D1.
	if c.Usage.InputTokens != 120 || c.Usage.OutputTokens != 8 || c.Usage.CacheReadTokens != 100 {
		t.Errorf("Usage = %+v, want the provider's reported counts", c.Usage)
	}
}

func TestRunPromptCell_StreamsTextBeforeItFinishes(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &fakeProvider{turns: []scriptedTurn{{text: "streamed-from-the-model"}}})
	cell := f.addCell(t, "prompt", "say something")

	conn, closeWS := f.dialWS(t)
	defer closeWS()

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.readUntilDelta(t, conn, cell, "streamed", 10*time.Second)
}

func TestRunPromptCell_ToolCallRoundTrip(t *testing.T) {
	f := newNBFixture(t)
	tool := &fakeTool{name: "peek", result: "tool says hello"}
	withTools(t, tool)
	fp := &fakeProvider{turns: []scriptedTurn{
		{toolName: "peek", toolInput: map[string]any{"path": "x"}},
		{text: "I used the tool"},
	}}
	withProvider(t, fp)
	cell := f.addCell(t, "prompt", "use the tool")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellOK {
		t.Fatalf("State = %q, want %q (outputs %+v)", c.State, CellOK, c.Outputs)
	}
	if tool.calls != 1 {
		t.Errorf("tool ran %d times, want 1", tool.calls)
	}
	if !hasOutputOfType(c, OutputToolCall) {
		t.Error("no tool_call output recorded")
	}
	if !hasOutputOfType(c, OutputToolResult) {
		t.Error("no tool_result output recorded")
	}
	if got := f.outputText(c); !strings.Contains(got, "I used the tool") {
		t.Errorf("outputs = %q, want the final turn's text", got)
	}

	// The second request must carry the first turn plus the tool result,
	// or the model is answering without knowing what the tool said.
	reqs := fp.sent()
	if len(reqs) != 2 {
		t.Fatalf("provider called %d times, want 2", len(reqs))
	}
	var sawToolResult bool
	for _, m := range reqs[1].Messages {
		for _, blk := range m.Content {
			if blk.Type == BlockToolResult && strings.Contains(blk.Text, "tool says hello") {
				sawToolResult = true
			}
		}
	}
	if !sawToolResult {
		t.Error("the follow-up request did not carry the tool result")
	}
}

// A tool the model asks for but we do not have must come back as a tool
// error the model can react to, not as a failed cell.
func TestRunPromptCell_UnknownToolIsReportedToTheModel(t *testing.T) {
	f := newNBFixture(t)
	withTools(t)
	fp := &fakeProvider{turns: []scriptedTurn{
		{toolName: "does_not_exist"},
		{text: "understood, carrying on"},
	}}
	withProvider(t, fp)
	cell := f.addCell(t, "prompt", "go")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellOK {
		t.Fatalf("State = %q, want the run to continue after an unknown tool", c.State)
	}
	if len(fp.sent()) != 2 {
		t.Fatalf("provider called %d times, want the loop to continue to a second turn", len(fp.sent()))
	}
}

// A refusal is a successful HTTP 200 with an empty content array. Code that
// reads content[0] unconditionally panics; the cell should show a refusal.
func TestRunPromptCell_RefusalIsShownNotCrashed(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &fakeProvider{turns: []scriptedTurn{{stopReason: StopRefusal}}})
	cell := f.addCell(t, "prompt", "something declined")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellError {
		t.Fatalf("State = %q, want %q for a refusal", c.State, CellError)
	}
	if got := strings.ToLower(f.outputText(c)); !strings.Contains(got, "refus") {
		t.Errorf("outputs = %q, want the refusal named", f.outputText(c))
	}
}

// A model that keeps calling tools must cost money once, not indefinitely.
func TestRunPromptCell_StopsAtTheTurnCap(t *testing.T) {
	f := newNBFixture(t)
	tool := &fakeTool{name: "loop", result: "again"}
	withTools(t, tool)

	turns := make([]scriptedTurn, maxAgentTurns+5)
	for i := range turns {
		turns[i] = scriptedTurn{toolName: "loop"}
	}
	fp := &fakeProvider{turns: turns}
	withProvider(t, fp)
	cell := f.addCell(t, "prompt", "spin")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 20*time.Second)

	if c.State != CellError {
		t.Errorf("State = %q, want %q once the turn cap is hit", c.State, CellError)
	}
	if n := len(fp.sent()); n > maxAgentTurns {
		t.Errorf("provider called %d times, want at most %d", n, maxAgentTurns)
	}
	if got := strings.ToLower(f.outputText(c)); !strings.Contains(got, "turn") {
		t.Errorf("outputs = %q, want the cap explained", f.outputText(c))
	}
}

func TestRunPromptCell_SendsProjectedContextNotJustTheCell(t *testing.T) {
	f := newNBFixture(t)
	fp := &fakeProvider{turns: []scriptedTurn{{text: "ok"}}}
	withProvider(t, fp)

	f.addCell(t, "markdown", "PROJECT-MARKER: always use tabs")
	cell := f.addCell(t, "prompt", "what did I say?")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	reqs := fp.sent()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times, want 1", len(reqs))
	}
	var joined strings.Builder
	for _, m := range reqs[0].Messages {
		for _, c := range m.Content {
			joined.WriteString(c.Text)
		}
	}
	if !strings.Contains(joined.String(), "PROJECT-MARKER") {
		t.Errorf("request did not carry the markdown cell above it:\n%s", joined.String())
	}
}

func hasOutputOfType(c Cell, typ OutputType) bool {
	for _, o := range c.Outputs {
		if o.Type == typ {
			return true
		}
	}
	return false
}
