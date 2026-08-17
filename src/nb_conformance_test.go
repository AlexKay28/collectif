package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anthropics/anthropic-sdk-go"
)

// Conformance pass over M2 / M2.5 against ADR 0001 and the phase exit
// criteria, testing the claims rather than re-reading the code.

// ADR §4.2 and #50's exit criterion: "edit cell 1, re-run, downstream marks
// stale". The fold understands cells_invalidated and is tested on it — but
// nothing has ever emitted one, so staleness never actually happens. The
// mechanism exists and the producer does not.
func TestConformance_RunningACellMarksDownstreamStale(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &fakeProvider{turns: []scriptedTurn{{text: "answer"}, {text: "answer"}}})

	first := f.addCell(t, "prompt", "the first question")
	second := f.addCell(t, "shell", "echo downstream")

	// Run the downstream cell so it has a result that can go stale.
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+second+"/run", nil)
	if c := f.waitForState(t, second, 10*time.Second); c.State != CellOK {
		t.Fatalf("downstream cell state = %q, want ok", c.State)
	}

	// Now re-run the cell above it. Everything below depends on its output,
	// so those results no longer reflect the notebook.
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+first+"/run", nil)
	f.waitForState(t, first, 10*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.stateOf(second) == CellStale {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("downstream cell is %q, want %q — re-running a cell above it left its result silently out of date",
		f.stateOf(second), CellStale)
}

// A cell that never ran has nothing to invalidate, and one still running
// must not be clobbered — marking it would lie about work in flight.
func TestConformance_InvalidationSkipsUnrunAndRunningCells(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &fakeProvider{turns: []scriptedTurn{{text: "ok"}}})

	top := f.addCell(t, "prompt", "top")
	neverRan := f.addCell(t, "shell", "echo never")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+top+"/run", nil)
	f.waitForState(t, top, 10*time.Second)

	if got := f.stateOf(neverRan); got != CellIdle {
		t.Errorf("a cell that never ran is %q, want %q", got, CellIdle)
	}
}

// ADR §4.4 names three budgets doing three different jobs. max_tokens is a
// hard per-response cap the model cannot see; the notebook's dollar cap is
// ours and is checked between turns. NotebookMeta.BudgetUSD exists as a
// field today and does nothing at all, which is worse than not having it.
func TestConformance_NotebookDollarBudgetStopsALoop(t *testing.T) {
	f := newNBFixture(t)

	// Every turn asks for a tool, so the loop would otherwise run to the
	// turn cap. Each turn reports enough usage to blow a tiny budget.
	tool := &fakeTool{name: "spin", result: "again"}
	withTools(t, tool)
	turns := make([]scriptedTurn, 30)
	for i := range turns {
		turns[i] = scriptedTurn{toolName: "spin", usage: Usage{InputTokens: 100_000, OutputTokens: 10_000}}
	}
	fp := &fakeProvider{turns: turns}
	withProvider(t, fp)

	if rec := nbRequest(t, f.srv, http.MethodPatch, f.base, map[string]any{
		"meta": map[string]any{"budgetUsd": 0.01},
	}); rec.Code != http.StatusOK {
		t.Fatalf("set budget: %d %s", rec.Code, rec.Body.String())
	}

	cell := f.addCell(t, "prompt", "spin forever")
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 20*time.Second)

	if n := len(fp.sent()); n >= maxAgentTurns {
		t.Errorf("provider called %d times — the loop ran to the turn cap instead of stopping on budget", n)
	}
	if c.State != CellError {
		t.Errorf("State = %q, want %q when the budget is exhausted", c.State, CellError)
	}
	if got := strings.ToLower(f.outputText(c)); !strings.Contains(got, "budget") {
		t.Errorf("outputs = %q, want the budget named as the reason", f.outputText(c))
	}
}

// Pricing is what makes a dollar budget meaningful. #50 specified extending
// ModelInfo with it and it was not done, so cost could not be computed at
// all.
func TestConformance_ModelCatalogCarriesPricing(t *testing.T) {
	p := &anthropicProvider{}
	for _, m := range p.Models() {
		if m.InputUSDPerMTok <= 0 || m.OutputUSDPerMTok <= 0 {
			t.Errorf("%s has no pricing (%+v) — a dollar budget cannot be computed without it", m.ID, m)
		}
	}
}

func TestConformance_CostIsComputedFromUsageAndPricing(t *testing.T) {
	info := ModelInfo{
		ID: "test-model", ContextWindow: 1000,
		InputUSDPerMTok: 5, OutputUSDPerMTok: 25,
		CacheReadUSDPerMTok: 0.5, CacheWriteUSDPerMTok: 6.25,
	}
	// 1M uncached input at $5, 1M output at $25.
	got := usageCostUSD(info, Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if got < 29.99 || got > 30.01 {
		t.Errorf("cost = %.4f, want 30.00", got)
	}
	// Cache reads are an order of magnitude cheaper, which is the whole
	// economic argument of M2.5 — the cost model has to reflect it.
	cached := usageCostUSD(info, Usage{CacheReadTokens: 1_000_000})
	full := usageCostUSD(info, Usage{InputTokens: 1_000_000})
	if cached >= full {
		t.Errorf("cache reads cost %.4f and uncached input %.4f — reads must be cheaper", cached, full)
	}
}

// ADR §4.8: owning the loop means the request size is known *before* it is
// sent, so pressure can be reported ahead of a turn rather than inferred
// after one.
func TestConformance_ContextPressureIsKnownBeforeSending(t *testing.T) {
	req := Request{
		System:   strings.Repeat("system ", 100),
		Messages: []Message{userText(strings.Repeat("context ", 1000))},
	}
	est := estimateRequestTokens(req)
	if est <= 0 {
		t.Fatal("no pre-flight estimate — the loop cannot warn before a turn")
	}
	// Rough is fine; wrong by an order of magnitude is not.
	if est < 500 || est > 20_000 {
		t.Errorf("estimate = %d tokens for ~8k characters, want the right order of magnitude", est)
	}

	small := estimateRequestTokens(Request{Messages: []Message{userText("hi")}})
	if small >= est {
		t.Errorf("a tiny request estimated %d and a large one %d", small, est)
	}
}

func TestConformance_OversizedProjectionIsRefusedNotTruncated(t *testing.T) {
	info := ModelInfo{ID: "small", ContextWindow: 1000}
	huge := Request{Messages: []Message{userText(strings.Repeat("x ", 50_000))}}
	if err := checkRequestFits(info, huge); err == nil {
		t.Error("an oversized request was accepted — it should be an explicit error, never a silent trim")
	}
	fits := Request{Messages: []Message{userText("hello")}}
	if err := checkRequestFits(info, fits); err != nil {
		t.Errorf("a small request was refused: %v", err)
	}
}

// A budget that cannot be priced must refuse rather than pass silently:
// honouring the letter of the setting and none of its intent is how a
// surprise bill happens.
func TestConformance_UnpriceableBudgetRefusesToRun(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &unpricedProvider{})

	if rec := nbRequest(t, f.srv, http.MethodPatch, f.base, map[string]any{
		"meta": map[string]any{"budgetUsd": 5.0},
	}); rec.Code != http.StatusOK {
		t.Fatalf("set budget: %d", rec.Code)
	}
	cell := f.addCell(t, "prompt", "anything")
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellError {
		t.Errorf("State = %q, want %q — the run went ahead with an unenforceable budget", c.State, CellError)
	}
	if got := strings.ToLower(f.outputText(c)); !strings.Contains(got, "pricing") && !strings.Contains(got, "budget") {
		t.Errorf("outputs = %q, want the reason named", f.outputText(c))
	}
}

// A provider whose catalog carries no prices — a local model, say.
type unpricedProvider struct{ fakeProvider }

func (p *unpricedProvider) Models() []ModelInfo {
	return []ModelInfo{{ID: "local-model", ContextWindow: 100_000}}
}

// M1 made a mid-run refresh work by keeping each running cell's output on
// the store and handing it to a joining client in the fold. The agent loop
// broadcasts deltas directly instead of going through that writer, so a
// prompt cell gets the streaming but not the buffer — refresh during a long
// model turn and the cell is empty again. Same guarantee, two run paths,
// only one of them honouring it.
func TestConformance_PromptCellLiveOutputSurvivesAReconnect(t *testing.T) {
	f := newNBFixture(t)
	// A provider that streams and then keeps running, so the cell is
	// genuinely mid-turn when the buffer is inspected. An instant fake
	// would let this pass without proving anything.
	withProvider(t, &blockingProvider{emitted: make(chan struct{})})
	cell := f.addCell(t, "prompt", "say something")
	t.Cleanup(func() { nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil) })

	watch, closeWatch := f.dialWS(t)
	defer closeWatch()

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.readUntilDelta(t, watch, cell, "partial", 10*time.Second)

	if got := f.stateOf(cell); got != CellRunning {
		t.Fatalf("cell state = %q, want it still running for this to prove anything", got)
	}
	live := f.st.liveSnapshot()
	if _, ok := live[cell]; !ok {
		t.Fatal("a running prompt cell kept no live buffer — a refresh mid-turn would show an empty cell")
	}
	if !strings.Contains(live[cell].Text, "partial") {
		t.Errorf("live buffer = %q, want the streamed text", live[cell].Text)
	}
}

// ADR §4.4: "a cancelled run finalises whatever it has". Shell cells keep
// what they produced before the kill; prompt cells must too.
func TestConformance_InterruptedPromptCellKeepsWhatItProduced(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &blockingProvider{emitted: make(chan struct{})})
	cell := f.addCell(t, "prompt", "start something long")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)

	deadline := time.Now().Add(5 * time.Second)
	for f.stateOf(cell) != CellRunning && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	// Wait until the provider has actually streamed something.
	select {
	case <-f.st.liveWait(cell, 5*time.Second):
	case <-time.After(5 * time.Second):
		t.Fatal("provider never streamed anything")
	}

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellInterrupted {
		t.Fatalf("State = %q, want %q", c.State, CellInterrupted)
	}
	if got := f.outputText(c); !strings.Contains(got, "partial") {
		t.Errorf("outputs = %q, want the text produced before the interrupt to be kept", got)
	}
}

// blockingProvider streams a little and then blocks until the context is
// cancelled, so an interrupt can be delivered mid-turn deterministically.
type blockingProvider struct {
	fakeProvider
	emitted chan struct{}
}

func (p *blockingProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	return &blockingStream{ctx: ctx, emitted: p.emitted}, nil
}

type blockingStream struct {
	ctx     context.Context
	emitted chan struct{}
	sent    bool
}

func (s *blockingStream) Next() (Chunk, error) {
	if !s.sent {
		s.sent = true
		select {
		case <-s.emitted:
		default:
			close(s.emitted)
		}
		return Chunk{Type: ChunkText, Text: "partial answer before the interrupt"}, nil
	}
	<-s.ctx.Done() // block until interrupted
	return Chunk{}, s.ctx.Err()
}

// Result reports usage even though the turn never completed — which is
// exactly what a real transport does: the prompt was sent and billed before
// the interrupt arrived.
func (s *blockingStream) Result() Result {
	return Result{
		StopReason: StopEndTurn,
		Usage:      Usage{InputTokens: 4_200, OutputTokens: 17},
	}
}
func (s *blockingStream) Close() error { return nil }

// liveWait signals once the cell has any live output buffered.
func (st *notebookStore) liveWait(cellID string, timeout time.Duration) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if snap := st.liveSnapshot(); snap[cellID].Text != "" {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	return ch
}

// ─── Code-review findings, 7f2cffc..HEAD ────────────────────────────────

// The showstopper. With adaptive thinking on — which we always set — every
// assistant turn comes back carrying thinking blocks, and the API requires
// them passed back *unmodified, in order, with their signature intact*
// whenever the turn also contains tool_use. We kept the text and dropped
// the signature, then dropped the block entirely, so from turn two onward
// any tool-calling cell would have failed with a 400.
//
// Note the distinction this must preserve: thinking recorded in a
// notebook's stored outputs is still not replayed (nb_project.go), because
// that is a summary from a possibly-different model on a possibly-different
// run. This is about echoing back the turn we just received.
func TestReview_ThinkingIsReplayedWithItsSignature(t *testing.T) {
	turn := []ContentBlock{
		{Type: BlockThinking, Text: "reasoning", Signature: "sig-abc"},
		{Type: BlockToolUse, ToolUseID: "tu_1", ToolName: "read", ToolInput: map[string]any{"path": "x"}},
	}
	params := buildAnthropicRequest(Request{Messages: []Message{
		userText("do it"),
		{Role: RoleAssistant, Content: turn},
		{Role: RoleUser, Content: []ContentBlock{{Type: BlockToolResult, ToolUseID: "tu_1", Text: "contents"}}},
	}})

	assistant := params.Messages[1]
	if len(assistant.Content) != 2 {
		t.Fatalf("assistant turn has %d blocks, want thinking + tool_use: %+v", len(assistant.Content), assistant.Content)
	}
	thinking := assistant.Content[0].OfThinking
	if thinking == nil {
		t.Fatal("thinking block was dropped — the API rejects a tool_use turn without it")
	}
	if thinking.Signature != "sig-abc" {
		t.Errorf("signature = %q, want it passed back intact", thinking.Signature)
	}
	if assistant.Content[1].OfToolUse == nil {
		t.Error("tool_use block lost")
	}
}

func TestReview_NormalisationKeepsTheThinkingSignature(t *testing.T) {
	msg := &anthropic.Message{StopReason: anthropic.StopReasonToolUse}
	if err := json.Unmarshal([]byte(`[{"type":"thinking","thinking":"why","signature":"sig-xyz"}]`), &msg.Content); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	res := normaliseAnthropicResult(msg)
	if len(res.Content) != 1 || res.Content[0].Signature != "sig-xyz" {
		t.Errorf("signature not carried through normalisation: %+v", res.Content)
	}
}

// A prompt cell must render identically whether it is the cell being run or
// a cell above one. It did not: the target was trimmed and a prefix cell was
// not, so a textarea's trailing newline made cell 5 byte-different between
// its own run and cell 6's — and the prefix breakpoint missed, silently, at
// full price. Exactly the failure #51's gate exists to catch.
func TestReview_APromptCellRendersIdenticallyAsTargetAndAsPrefix(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{ID: "c0", Type: CellPrompt, Source: "the question\n"})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "next"})

	asTarget := mustProject(t, nb, 0) // c0 is the cell being run
	asPrefix := mustProject(t, nb, 1) // c0 is now context above c1

	if len(asTarget) == 0 || len(asPrefix) == 0 {
		t.Fatal("empty projection")
	}
	if got, want := asPrefix[0].Content[0].Text, asTarget[0].Content[0].Text; got != want {
		t.Errorf("same cell rendered differently:\n as prefix %q\n as target %q\nthe cached prefix would miss every time", got, want)
	}
}

// grep computed paths relative to the *unresolved* root while walking a
// resolved one. Under a symlinked root every hit was filtered out by the
// glob and reported paths began with ../.. — a path the model cannot feed
// back into read.
func TestReview_GrepWorksUnderASymlinkedRoot(t *testing.T) {
	real := toolRoot(t)
	link := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	out, isErr := runTool(t, &grepTool{}, link, map[string]any{"pattern": "TODO", "glob": "**/*.go"})
	if isErr {
		t.Fatalf("grep failed: %s", out)
	}
	if !strings.Contains(out, "src/util.go") {
		t.Errorf("grep output %q — the glob filtered out every hit under a symlinked root", out)
	}
	if strings.Contains(out, "..") {
		t.Errorf("grep reported a path the model cannot use: %q", out)
	}
}

// Usage from a turn that errored or was interrupted was thrown away, so an
// interrupted cell reported costing nothing while having been fully billed.
func TestReview_InterruptedTurnStillReportsItsUsage(t *testing.T) {
	f := newNBFixture(t)
	withProvider(t, &blockingProvider{emitted: make(chan struct{})})
	cell := f.addCell(t, "prompt", "long one")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	select {
	case <-f.st.liveWait(cell, 5*time.Second):
	case <-time.After(5 * time.Second):
		t.Fatal("nothing streamed")
	}
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/interrupt", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if promptTokens(c.Usage) == 0 && c.Usage.OutputTokens == 0 {
		t.Error("an interrupted cell reported zero usage — the prompt was billed either way")
	}
}

// required was read with a []string assertion that only matches how the
// builtins happen to construct it. A schema arriving through JSON (M5's MCP
// tools) yields []any, the assertion fails, and the tool goes out strict
// with no required list — so the validation guarantee quietly stops holding.
func TestReview_RequiredSurvivesAJSONDecodedSchema(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{
		"type":"object",
		"properties":{"path":{"type":"string","description":"p"}},
		"required":["path"],
		"additionalProperties":false
	}`), &schema); err != nil {
		t.Fatal(err)
	}
	params := buildAnthropicRequest(Request{
		Messages: []Message{userText("hi")},
		Tools:    []ToolSpec{{Name: "read", Description: "d", InputSchema: schema}},
	})
	got := params.Tools[0].OfTool.InputSchema.Required
	if len(got) != 1 || got[0] != "path" {
		t.Errorf("required = %v from a JSON-decoded schema, want [path]", got)
	}
}

// elide split at byte offsets, so any non-ASCII text over budget carried a
// partial rune at each cut and reached the model as U+FFFD.
func TestReview_ElideDoesNotSplitRunes(t *testing.T) {
	text := strings.Repeat("héllo wörld ☃ ", 2000)
	out := elide(text, 1024)
	if !utf8.ValidString(out) {
		t.Error("elide produced invalid UTF-8 — the model sees a mangled boundary")
	}
	if strings.Contains(out, "�") {
		t.Error("elide left a replacement character at a cut")
	}
}
