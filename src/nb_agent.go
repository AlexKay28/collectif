package main

// nb_agent.go — the agent loop. #50 (M2), ADR 0001 §4.4.
//
// This is the phase where collectif stops mirroring somebody else's loop
// and runs its own: model call, tool dispatch, append result, repeat. The
// shape is the one M1's shell runner already established — an HTTP call
// starts a run and returns, deltas stream to watchers and are never
// persisted, and the run finalises as outputs plus a run_finished. What is
// new is that the turns come from a provider and can ask for tools.
//
// Three behaviours here are deliberate and easy to get wrong:
//
//   - A refusal is a *successful* response with an empty content array.
//     Code that reads the first block unconditionally panics on it, so the
//     stop reason is checked before the content is touched.
//   - A tool that fails, or one we don't have, is reported back to the
//     model as a tool result rather than failing the run. The model can
//     adapt; ending the turn denies it the chance.
//   - The turn cap exists so a model that keeps calling tools costs money
//     once rather than indefinitely.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxAgentTurns bounds one cell's loop. Generous enough for real
// multi-step work, finite enough that a stuck model is a bounded bill.
const maxAgentTurns = 40

// defaultMaxTokens is floored generously on purpose: thinking counts
// against the same ceiling as the reply on current models, so a limit sized
// around the expected answer truncates mid-thought.
const defaultMaxTokens = 64000

// runPromptCell drives one prompt cell to completion.
func runPromptCell(ctx context.Context, st *notebookStore, cellID string, run *nbRun, p Provider) {
	defer st.endRun(cellID, run.runID)
	defer run.cancel()

	doc := st.Doc()
	idx := indexOfCell(doc, cellID)
	if idx < 0 {
		return
	}

	msgs, err := projectContext(doc, idx)
	if err != nil {
		st.emitError(cellID, run.runID, "context: "+err.Error())
		st.finishRunWithUsage(cellID, run.runID, CellError, Usage{})
		return
	}

	model := doc.Cells[idx].Meta.Model
	if model == "" {
		model = doc.Meta.Model
	}
	effort := doc.Cells[idx].Meta.Effort
	if effort == "" {
		effort = doc.Meta.Effort
	}

	// The projected prefix is what two runs of this cell share. Turns the
	// loop appends below are new every time.
	stablePrefix := len(msgs)

	// Resolve the model's budgets once. ADR §4.4 names three, doing three
	// different jobs: max_tokens is a hard per-response cap the model
	// cannot see, the notebook's dollar cap is ours and is checked between
	// turns, and the model-facing task budget is a later addition.
	info := modelInfoFor(p, model)
	budgetUSD := doc.Meta.BudgetUSD

	// A budget we cannot price is not a budget. Running anyway would honour
	// the letter of the setting and none of its intent — the user asked for
	// a spend cap, and silently not enforcing one is how a surprise bill
	// happens.
	if budgetUSD > 0 && info.InputUSDPerMTok == 0 && info.OutputUSDPerMTok == 0 {
		st.emitError(cellID, run.runID, fmt.Sprintf(
			"This notebook has a $%.2f budget, but no pricing is known for model %q, so the budget cannot be enforced. "+
				"Remove the budget to run without one.", budgetUSD, info.ID))
		st.finishRunWithUsage(cellID, run.runID, CellError, Usage{})
		return
	}

	var total Usage
	status := CellOK

	for turn := 0; turn < maxAgentTurns; turn++ {
		if err := ctx.Err(); err != nil {
			status = CellInterrupted
			break
		}

		// Pre-flight. Owning the loop means the size is known before the
		// request is sent, so an oversized projection is an explicit error
		// rather than something the API silently truncates (ADR §4.8).
		req := Request{
			Model:                model,
			System:               notebookSystemPrompt(doc),
			Messages:             msgs,
			Tools:                toolSpecs(),
			MaxTokens:            defaultMaxTokens,
			Effort:               effort,
			StablePrefixMessages: stablePrefix,
		}
		if err := checkRequestFits(info, req); err != nil {
			st.emitError(cellID, run.runID, err.Error())
			status = CellError
			break
		}

		res, err := streamTurn(ctx, st, cellID, run, p, req)
		if err != nil {
			if run.wasInterrupted() || errors.Is(err, context.Canceled) {
				status = CellInterrupted
			} else {
				st.emitError(cellID, run.runID, "provider: "+err.Error())
				status = CellError
			}
			break
		}
		total = total.add(res.Usage)

		// The notebook's dollar cap, checked between turns. A model that
		// keeps calling tools should cost what the user agreed to and then
		// stop — the turn cap alone bounds iterations, not spend.
		if budgetUSD > 0 {
			if spent := usageCostUSD(info, total); spent >= budgetUSD {
				st.emitError(cellID, run.runID, fmt.Sprintf(
					"Stopped after spending $%.4f of this notebook's $%.2f budget. "+
						"Raise the budget in notebook settings to continue.", spent, budgetUSD))
				status = CellError
				break
			}
		}

		// Check the stop reason before touching content: a refusal arrives
		// as a successful response with nothing in it.
		if res.StopReason == StopRefusal {
			st.emitError(cellID, run.runID,
				"The model refused this request. Nothing was produced; rephrasing or a different model may help.")
			status = CellError
			break
		}

		calls := recordTurn(st, cellID, run.runID, res)

		if len(calls) == 0 {
			if res.StopReason == StopMaxTokens {
				st.emitError(cellID, run.runID,
					"The response hit the output limit and was cut off. Re-run, or split the task across cells.")
				status = CellError
			}
			break
		}

		// Echo the assistant turn back verbatim, then answer every call in
		// one user turn — splitting tool results across messages teaches
		// the model to stop making parallel calls.
		msgs = append(msgs, Message{Role: RoleAssistant, Content: res.Content})
		results := make([]ContentBlock, 0, len(calls))
		for _, call := range calls {
			text, isErr := dispatchTool(ctx, call, doc.Root)
			st.emitOutput(cellID, run.runID, Output{
				Type: OutputToolResult,
				Text: text,
				Data: map[string]any{"tool": call.Name, "isError": isErr},
			})
			results = append(results, ContentBlock{
				Type:      BlockToolResult,
				ToolUseID: call.ID,
				Text:      text,
				IsError:   isErr,
			})
		}
		msgs = append(msgs, Message{Role: RoleUser, Content: results})

		if turn == maxAgentTurns-1 {
			st.emitError(cellID, run.runID, fmt.Sprintf(
				"Stopped after %d turns without finishing. The run is capped so a loop costs money once, not indefinitely.",
				maxAgentTurns))
			status = CellError
		}
	}

	if run.wasInterrupted() {
		status = CellInterrupted
	}
	if status == CellInterrupted {
		// ADR §4.4: a cancelled run finalises whatever it has. The turn
		// never completed, so nothing was recorded from its blocks — the
		// streamed text is all there is, and discarding it would throw
		// away the part the user was actually reading.
		if partial := st.liveText(cellID, run.runID); strings.TrimSpace(partial) != "" {
			st.emitOutput(cellID, run.runID, Output{Type: OutputText, Text: partial})
		}
	}
	st.finishRunWithUsage(cellID, run.runID, status, total)
}

// streamTurn runs one model call, streaming text and reasoning to watchers
// as it arrives.
func streamTurn(ctx context.Context, st *notebookStore, cellID string, run *nbRun, p Provider, req Request) (Result, error) {
	stream, err := p.Stream(ctx, req)
	if err != nil {
		return Result{}, err
	}
	defer stream.Close()

	// Write through the same accumulator shell cells use, rather than
	// broadcasting straight to subscribers. Broadcasting alone streams to
	// whoever is already watching and leaves nothing for a client that
	// joins mid-turn — so a refresh during a long model turn showed an
	// empty cell, the exact failure M1 fixed for shell cells and this path
	// quietly did not inherit.
	out := &deltaWriter{st: st, cellID: cellID, runID: run.runID}

	for {
		chunk, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, err
		}
		switch chunk.Type {
		case ChunkText, ChunkThinking:
			if chunk.Text != "" {
				out.write(chunk.Text)
			}
		case ChunkToolUse:
			if chunk.ToolCall != nil {
				out.write("\n→ " + chunk.ToolCall.Name + "\n")
			}
		}
	}
	return stream.Result(), nil
}

// recordTurn writes the turn's blocks to the log and returns the tool calls
// it asked for.
func recordTurn(st *notebookStore, cellID, runID string, res Result) []ToolCall {
	var calls []ToolCall
	for _, blk := range res.Content {
		switch blk.Type {
		case BlockText:
			if strings.TrimSpace(blk.Text) != "" {
				st.emitOutput(cellID, runID, Output{Type: OutputText, Text: blk.Text})
			}
		case BlockThinking:
			if strings.TrimSpace(blk.Text) != "" {
				st.emitOutput(cellID, runID, Output{Type: OutputThinking, Text: blk.Text})
			}
		case BlockToolUse:
			st.emitOutput(cellID, runID, Output{
				Type: OutputToolCall,
				Text: blk.ToolName,
				Data: map[string]any{"tool": blk.ToolName, "input": blk.ToolInput},
			})
			calls = append(calls, ToolCall{ID: blk.ToolUseID, Name: blk.ToolName, Input: blk.ToolInput})
		}
	}
	return calls
}

// dispatchTool runs one call. Every failure path returns text plus an error
// flag rather than an error: the model is the one who has to react.
func dispatchTool(ctx context.Context, call ToolCall, root string) (string, bool) {
	tool := lookupTool(call.Name)
	if tool == nil {
		return fmt.Sprintf("No tool named %q is available. Available tools: %s.",
			call.Name, strings.Join(toolNames(), ", ")), true
	}
	out, isErr, err := tool.Run(ctx, call.Input, root)
	if err != nil {
		return fmt.Sprintf("Tool %s failed: %v", call.Name, err), true
	}
	return out, isErr
}

func toolNames() []string {
	names := make([]string, 0, len(activeTools))
	for _, t := range activeTools {
		names = append(names, t.Spec().Name)
	}
	if len(names) == 0 {
		return []string{"(none configured)"}
	}
	return names
}

// modelInfoFor looks a model up in the provider's own catalog, falling back
// to a conservative window so a model we do not recognise still gets a
// pre-flight check rather than none.
func modelInfoFor(p Provider, model string) ModelInfo {
	if p != nil {
		for _, m := range p.Models() {
			if m.ID == model || (model == "" && len(p.Models()) > 0) {
				return m
			}
		}
	}
	return ModelInfo{ID: model, ContextWindow: defaultContextLimit}
}

// notebookSystemPrompt states where the agent is and what it is working in.
// Deliberately short: the notebook's own markdown cells are where a user
// says what they want, and duplicating that here would compete with them.
func notebookSystemPrompt(nb *Notebook) string {
	var b strings.Builder
	b.WriteString("You are working inside a collectif notebook — a document of cells the user runs one at a time.\n")
	b.WriteString("Answer the final message. Earlier cells are the context you already have.\n")
	if nb.Root != "" {
		b.WriteString("Working directory: " + nb.Root + ". Every path you are given is relative to it.\n")
	}
	return b.String()
}

// ─── Store helpers ──────────────────────────────────────────────────────

func (st *notebookStore) emitOutput(cellID, runID string, o Output) {
	if _, err := st.Append(evOutputAppended, outputAppendedPayload{
		CellID: cellID, RunID: runID, Output: o,
	}); err != nil {
		logNotebookErr(st, cellID, "append output", err)
	}
}

func (st *notebookStore) emitError(cellID, runID, msg string) {
	st.emitOutput(cellID, runID, Output{Type: OutputError, Text: msg})
}

func (st *notebookStore) finishRunWithUsage(cellID, runID string, status CellState, usage Usage) {
	st.clearLive(cellID, runID)
	if _, err := st.Append(evRunFinished, runFinishedPayload{
		CellID: cellID, RunID: runID, Status: status, Usage: usage,
	}); err != nil {
		logNotebookErr(st, cellID, "finish run", err)
	}
}
