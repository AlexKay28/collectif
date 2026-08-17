# ADR 0002 — the notebook replaces the terminal, not the agent

- **Status**: Proposed
- **Date**: 2026-08-17
- **Amends**: [0001 — collectif becomes a notebook-native agent harness](0001-notebook-harness.md) (D1, D8, §6 roadmap)
- **Related**: #46 (multi-CLI adapters), #47 (notebook harness epic), #50–#56 (M2–M7), #42 (harness telemetry)

---

## 1. What 0001 got wrong

ADR 0001 asked the right question — "agent work is delivered as chat
scrollback, which is neither a transcript you can trust nor a document you can
re-run" — and then answered a different one.

Its D1 was *"collectif runs its own agent loop in Go"*, and its D8 was
*"notebook is the product; PTY/CLI mode goes legacy"*. Read together, those
build a **second agent** alongside the CLIs collectif already spawns, and make
the fleet the compatibility path.

That is not what collectif is for. collectif's job is to run *your* agents —
`claude`, `codex`, `opencode` — and show you what they are doing. The problem
worth solving is not "collectif has no agent of its own." It is:

> **the window we show those agents through is a terminal, and a terminal is
> the worst possible view of structured work.**

The notebook is the fix for *that*. It is a **replacement for xterm**, not a
replacement for the CLI. Every argument 0001 made about interleaved intent and
reproduced output holds — it was aimed at the wrong layer.

This is worth stating bluntly because 0001's framing has a real cost attached.
It listed under Consequences → Bad: *"Billing changes for users. Subscription-billed
CLI usage is replaced by per-token API spend. This is the single biggest
adoption cost."* That cost was incurred to solve a rendering problem. Under
this ADR it disappears: your Max subscription keeps paying for your agent, and
collectif renders it properly.

---

## 2. Decisions

| # | Decision | Replaces | Rejected alternative |
|---|---|---|---|
| **D1′** | A running CLI session **is** a notebook. Cells are projected from the session's transcript and hooks; the PTY is an implementation detail, not a UI. | 0001 D1 | Keep xterm as the primary view with a notebook "tab" beside it — two truths, both partial |
| **D8′** | The notebook is the **default view of a session**. xterm survives as a raw-bytes escape hatch, reachable and honest about being one. | 0001 D8 | Delete the terminal (some CLIs will always outrun the projection) |
| **D9** | Cells carry **provenance**: `mirrored` (the CLI did this) or `authored` (you did). The two get different verbs. | new | One undifferentiated cell type, with re-run silently meaning different things |
| **D10** | The native loop from M2/M2.5 is retained as **one executor among two**, not the only one. A notebook with no session attached still runs on collectif's own provider. | narrows 0001 D1 | Delete it — it is the only backend where full re-execution is real, and it is already built and tested |
| **D11** | Projection fidelity is **per-adapter and visibly degraded**, never faked. A CLI that cannot report tool calls renders a session with no tool cells and says so. | new | Regex-scrape ANSI to synthesise cells — the exact failure mode §1 of 0001 catalogued |

D9 is the load-bearing one, so here is its rationale in full.

A notebook cell is editable and re-runnable. A CLI session's history is neither:
the context lives inside a process we do not own, and there is no wire on which
to say "forget turn 3, here is a different turn 3." Pretending otherwise would
put an Edit button on a cell that cannot be edited — the interactive equivalent
of the stale context-limit map that 0001 §1 used as its founding example of
second-hand knowledge lying.

So the verbs split:

| | Mirrored cell | Authored cell |
|---|---|---|
| Source | the agent's transcript / hooks | you typed it |
| Editable | no | yes |
| Re-run | **re-ask**: copies its text into a new prompt cell at the bottom | yes, in place |
| Deletable | hidden, not deleted — the log is the record | yes |
| In context | whatever the CLI already has | projected per 0001 §4.2 |

"Re-run" degrading to "re-ask" is not a compromise; it is what actually happens
when you want a different answer from a running agent, made explicit.

---

## 3. Architecture

The change is smaller than it sounds, because M1 built the right thing at the
wrong altitude. `nb_doc` / `nb_store` / `nb_ws` know nothing about who produces
events — a fold over an append-only log is exactly as happy consuming a CLI
transcript as it is consuming our own loop.

```
                              browser — one view, two backings
                                          │
                            ┌─────────────┴─────────────┐
                            │   notebook renderer       │   (nb.js, unchanged)
                            └─────────────┬─────────────┘
                                          │ fold + events (nb_ws, unchanged)
                            ┌─────────────┴─────────────┐
                            │  notebook store / log     │   (nb_store, unchanged)
                            └──────┬─────────────┬──────┘
                    cell events    │             │   cell events
                  ┌────────────────┘             └────────────────┐
        ┌─────────┴──────────┐               ┌───────────────────┴─────────┐
        │  sessionProjector  │  NEW          │  native loop (M2/M2.5)      │
        │  transcript+hooks  │               │  Provider → tools → cells   │
        │      → cells       │               └─────────────────────────────┘
        └─────────┬──────────┘
                  │ reads                 ▲ writes prompts
        ┌─────────┴──────────┐            │
        │  CLIAdapter (#46)  │────────────┘
        │  PTY + transcript  │
        └────────────────────┘
```

One new component: `sessionProjector`. It subscribes to a session's transcript
and hook stream and appends cell events to that session's notebook store.
Input runs the other way — an authored prompt cell writes to the PTY and then
waits for the projector to mirror the turn back.

### The seam already exists

0001 §2 reserved *"an internal `Executor` interface behind the cell runner,
because a second implementation is already known (legacy CLI-backed
execution)"*. That sentence is the whole of this ADR's implementation plan.
What changes is which implementation is primary.

### What the projection can actually see

This is the part that decides whether the idea works, so it is stated against
the code rather than hoped for. `CLIAdapter` (`src/cli.go`) exposes
`Capabilities`, `TranscriptPath`, and `ParseTranscriptLine`.

| Cell / signal | Claude Code | codex | opencode |
|---|---|---|---|
| User turns | transcript | transcript | transcript |
| Assistant text | transcript | transcript | partial |
| Thinking | transcript | — | — |
| Tool call + input | `PreToolUse` hook + transcript | — | — |
| Tool result | `PostToolUse` hook | — | — |
| Approval prompt | hook (structured) | PTY scrape (`menu.go`) | PTY scrape |
| Usage / cost | transcript | transcript | transcript |

**`TranscriptEvent` is today counts-only** — `Model`, four token counters, and
three `*Chars` totals. It carries no message text at all. Projecting cells
means widening it to carry content, which is the single largest piece of new
work this ADR creates and the thing to prototype first (see §5, P0).

Where a column is empty the notebook shows fewer cells and a one-line note
naming the missing capability. It does not scrape ANSI to invent them.

---

## 4. What this keeps, and what it costs

**Kept, unchanged** — M1 in full (document model, event log, fold, WS transport,
markdown + shell cells, keyboard model), M2's projection mechanic and tool
loop, M2.5's cache discipline, and every test written for them. None of that
was wasted; all of it was built one layer below the mistake.

**Kept, demoted** — the native loop. It stops being *the* product and becomes
the backend for notebooks that have no CLI session attached: scratch analysis,
a notebook you re-run next week when the agent that wrote it is long gone,
and the only place where 0001's full re-execution story is literally true.

**Promoted** — #46. Under 0001 the adapter work was a compatibility path being
kept alive out of politeness. Here it is the primary input: fidelity of the
notebook *is* fidelity of the adapter, which makes "add a fourth CLI" a feature
with a visible payoff rather than a maintenance tax.

**New cost** — the projection is a parser, and parsers rot against other
people's formats. This is the same class of debt 0001 §1 complained about, and
it does not vanish; it is bounded by making its failure visible (D11) rather
than by owning the loop.

**New risk** — a fast agent produces turns faster than a person reads them. A
terminal handles that with a scroll bar; a notebook that reflows on every event
does not. Auto-follow with a pinned-to-bottom affordance, and coalescing
already exists in the WS layer.

---

## 5. Roadmap correction

0001's phases M3–M7 are unchanged in content but change in *purpose*: they are
now about what a projected session can do, not about growing a competing agent.

| Phase | 0001 | Now |
|---|---|---|
| M0, M1 | done | **unchanged** — the foundation was right |
| M2, M2.5 | done | **unchanged, re-scoped** as the detached-notebook backend (D10) |
| **P0 — Projection spike** | — | **NEW, next.** Widen `TranscriptEvent` to carry content; project one real `claude` session into a read-only notebook. Exit: open a running session in the browser and read its turns as cells with no xterm involved. |
| **P1 — Input + provenance** | — | **NEW.** Authored prompt cells write to the PTY; mirrored cells get their reduced verb set (D9); approvals render as inline widgets from hooks. Exit: drive a full `claude` session from the notebook, terminal never opened. |
| **P2 — Degradation** | — | **NEW.** Per-adapter capability surfacing (D11); codex and opencode render honestly. Exit: three CLIs, three fidelities, no lies. |
| M3 write tools + policy (#52) | for our loop | for the **detached** loop; session approvals come from hooks instead |
| M4 provider-agnostic (#53) | core bet | detached-notebook feature; lower priority |
| M5 MCP (#54) | core | unchanged |
| M6 subagents (#55) | our `task` tool | **also** projecting the CLI's own subagents (`SubagentFiles` capability) |
| M7 default surface (#56) | notebook replaces dashboard | **notebook replaces the terminal panel**; the dashboard stays the front door |

P0 before P1 because a read-only projection is falsifiable in an afternoon and
answers the only question that can kill this: whether the transcript carries
enough to reconstruct a turn. If it does not, we learn it before building an
input path into a view that cannot show results.

---

## 6. Consequences

**Good**

- The billing objection disappears. Subscription-billed CLI usage stays.
- collectif stops competing with the CLIs and goes back to orchestrating them,
  which is the thing it is already good at.
- The approval UI still stops being regex archaeology — for Claude Code it comes
  from hooks, which are structured, and the `menu.go` scraping becomes the
  degraded path rather than the only one.
- Sessions become durable, committable artifacts. Today a session's history is
  a ring buffer that dies with the process.
- Every line of M1/M2 survives.

**Bad**

- The strongest version of the notebook promise — edit turn 3, re-run, watch
  downstream go stale — is only true for detached notebooks. For a live session
  it degrades to re-ask (D9), and we must not oversell it.
- Two backends means two ways for a cell to be produced, and bugs will hide in
  the difference.
- We now depend on transcript formats we do not control for the *primary*
  surface, not just for telemetry.

**Risks**

| Risk | Mitigation |
|---|---|
| The transcript does not carry enough to reconstruct a turn | P0 is a spike for exactly this, before any input work |
| Projection lag makes the notebook feel behind the terminal | Hooks arrive ahead of transcript flushes; render optimistically from hooks and reconcile |
| Users want the terminal back for the 5 % it does better | D8′ keeps it, unhidden |
| Two backends drift | The cell event schema is shared and already tested; the projector and the loop are both event producers with one fold below them |

---

## 7. Open questions

1. **One notebook per session, or one per agent across sessions?** Proposed:
   per session, with the notebook outliving the process (a session ends; its
   document stays).
2. **Does an authored markdown cell interleaved into a live session go into
   the agent's context?** It cannot — the CLI owns context. Proposed: it is a
   margin note, and the UI must make that obvious.
3. **Does resuming a session (`--resume`) reopen its notebook or start a new
   one?** Proposed: reopen, appending; the log already supports it.
4. **What happens to a mirrored cell when the transcript is rewritten**
   (compaction)? Proposed: append a `compacted` marker event; never rewrite
   history.
5. **Should detached notebooks and session notebooks live in the same list?**
   The sidebar currently shows both under separate headings, which is a UI
   answer to an unanswered modelling question.
