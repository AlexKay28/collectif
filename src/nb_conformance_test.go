package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
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
