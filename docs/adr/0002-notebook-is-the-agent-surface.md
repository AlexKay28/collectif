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
| User turns | transcript | — | — |
| Assistant text | transcript | — | — |
| **Thinking** | **not available** (see below) | — | — |
| Tool call + input | transcript | — | — |
| Tool result | transcript | — | — |
| Approval prompt | hook (structured) | PTY scrape (`menu.go`) | PTY scrape |
| Usage / cost | transcript | transcript | transcript |
| Subagent turns | separate files (see below) | — | — |

> **Updated 2026-08-17 from P0's findings.** The rows above were written from
> the format's documentation and my reading of it. Projecting a real 11 109-line
> transcript corrected three of them; the corrections are the reason the spike
> came first.

**Everything a turn needs is in the transcript.** User prompts, assistant
prose, tool calls with full input, and tool results with `is_error` all
survive projection — hooks are not required for the *content*, only for
approvals. `TranscriptEvent` was counts-only, so P0 adds `TranscriptPart` and
`CLIAdapter.ProjectTranscriptLine` (one line → many parts) alongside it rather
than widening it: one assistant message routinely carries thinking, prose, and
a tool call, and a one-event-per-line signature would silently drop two of the
three.

**Thinking is not recoverable.** Across 50 transcripts and 7 453 thinking
blocks on this machine, not one carries thinking *text* — Claude Code persists
the `signature` and discards the summary. A projected session can therefore
never show reasoning, however well we parse. This is a hard ceiling on the
session view and an argument in favour of D10's detached notebooks, which get
thinking directly off the stream.

**Subagents live in their own files.** `isSidechain` is never true in a main
transcript; subagent conversations are written to
`<session>/subagents/agent-*.jsonl`, where every line *is* flagged. The same
parser reads them unchanged, so M6's input problem is already solved — it
needs a file watcher and nesting, not a second parser.

**A transcript is a tree, not a list.** Every line names its `parentUuid`,
and two user turns sharing a parent means the first was abandoned and
re-sent. A projector that ignores this marks a question that was never
answered as having succeeded — which the replayed document showed, twice.
Interruptions are worse: pressing Escape writes a literal
`[Request interrupted by user]` line with role `user`, no `origin` and no
`isMeta`, so every provenance filter passes it and it renders as a prompt
nobody typed. Both are now state changes on the turn they stopped rather
than turns of their own. Matching English sentinel text is fragile and
there is no other signal in the format; the mitigation is that the match is
whole-line, so a prompt *about* interruption is still a prompt and a
wording change degrades to the old behaviour rather than to lost turns.

**The user/machine boundary is the hard part, and `isMeta` does not draw it.**
Claude Code writes a great deal as the user that the user never typed. Line
provenance is `origin.kind`; the filter fails *closed*, so an unrecognised kind
is machinery until proven otherwise. Before that filter existed, one session
projected a 47 KB background-task notification as a typed prompt. Compaction
summaries (`isCompactSummary`) are real turns and get their own part kind,
which answers open question 4 better than its own proposal did: compaction
appends rather than rewriting, so nothing needs a marker.

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
| **P0 — Projection spike** | — | **NEW, done.** A: `TranscriptPart` + `ProjectTranscriptLine`. B: `sessionProjector` folds parts into cells — a prompt is a cell, the agent's work is its output. C: sessions open their own notebook, the sidebar links to it, mirrored cells render read-only with re-ask. Verified by replaying a real 2 893-line session into a document with correct states throughout. |
| **P1 — Input + provenance** | — | **Done.** A: prompt cells write to the PTY and the projector adopts them back. B: the agent's questions arrive through the Notification hook and render as inline widgets, answerable in place; the ask and the verdict are two append-only records paired by id. Verified live — the agent asked permission, it was approved from the notebook, and the command ran. |
| **P2 — Degradation** | — | **Done.** Fidelity is modelled per *surface* (turns / approvals / send / usage), not as one boolean, and derived on every read rather than stored. codex reads as `turns: false, approvals: true, send: true` — a real and useful answer that a single flag could not express. No projectors were written for codex or opencode: neither is installed here and there are no rollout files, and inventing a parser from documentation is the mistake P0 exists to have caught. |
| M3 write tools + policy (#52) | for our loop | for the **detached** loop; session approvals come from hooks instead. Its stated purpose — "the first phase where collectif can damage something" — is already false: P1 slice B made it possible to approve a CLI's `rm -rf` from a browser tab |
| M4 provider-agnostic (#53) | core bet | detached-notebook feature; low priority — a CLI has already chosen its provider |
| M5 MCP (#54) | core | ~~unchanged~~ **detached-notebook feature, low priority.** Corrected 2026-08-17: every CLI collectif spawns already speaks MCP, so a client of our own serves only the smaller surface. Spike surfacing the CLI's `mcp__*` tool calls from hooks first — a fraction of the work, on the surface that has users |
| M6 subagents (#55) | our `task` tool | **splits.** 55a projects the CLI's own subagents and is the highest-value work left; the parser already reads `subagents/agent-*.jsonl` unchanged, so what is missing is a watcher and a correlation, not a parser. 55b is our `task` tool, deferred |
| M7 default surface (#56) | notebook replaces dashboard | **reversed.** The notebook replaces the terminal *panel*; the dashboard stays the front door. Half its scope already shipped — `cliExecutor` as P1's send path, the degraded badge as P2's fidelity block |

P0 before P1 because a read-only projection is falsifiable in an afternoon and
answers the only question that can kill this: whether the transcript carries
enough to reconstruct a turn. If it does not, we learn it before building an
input path into a view that cannot show results.

---

### Sending is harder than reading

Found by driving a real session, and it changes what slice B is for.

A CLI is not always at a prompt. It puts up modal dialogs — trust-this-folder,
set-up-auto-mode, every permission request — and while one is up, whatever is
written to the PTY answers *the dialog*. A prompt beginning with "1" would
select option 1 of a permission request. Sending blind is the wrong default
even though it usually works.

There is no reliable way to know a CLI's modal state from outside. `menu.go`'s
ANSI scraping catches some (`Session.Pending`, now a hard gate on sending) and
missed the auto-mode dialog that swallowed two live prompts during this phase.
So the design does not pretend otherwise:

- **Gate on what we can detect.** A send while a prompt is pending is refused
  with 409 and an explanation, not queued.
- **Make the residue loud.** A prompt that is not mirrored back within 20s
  settles its cell as an error saying it may never have arrived and pointing at
  the terminal. Before this the cell sat at "running" forever, which is
  indistinguishable from an agent thinking hard.

This promoted slice B from a rendering feature to a correctness one, and
slice B closes the part of it that can be closed. Permission prompts arrive
through the `Notification` hook — structured, not scraped — and now render in
the cell that provoked them, so the thing blocking your prompt is visible and
answerable without leaving the document. What remains open is startup dialogs
(trust-this-folder, set-up-auto-mode): they fire no hook, `menu.go` does not
match them, and the 20s delivery timeout is still the only thing that catches
them. That is a known gap, not a solved problem.

The approval record is deliberately two entries rather than one mutable block:
the question when the agent asks, the verdict when a human answers, paired by
id. What it records is the *decision*, not the agent's receipt of it — a
terminal acknowledges nothing — and an unanswered prompt that the sweeper
clears is recorded as `expired`, never as an approval. This is the audit trail
ADR 0001 §4.6 wanted and the scrollback could not give.

### Degradation cuts both ways

D11 is about not claiming fidelity we lack. P2 found the same error pointed
the other way. On a CLI whose turns are not projected, nothing is ever echoed
back, so P1's adoption timeout fired on *every* prompt and reported "this may
not have arrived" about prompts that arrived fine. A send on such a CLI now
settles immediately as delivered, with a line saying the reply will appear in
the terminal rather than here.

The capability statement itself moved out of the log. P0 wrote a one-time
markdown cell explaining the gap; that put a claim about the *build*
permanently into a *document*, where it would still assert "codex turns are not
shown" long after someone wrote the parser. It is now derived on every read and
stored nowhere.

### The snapshot was not disposable after all

§4.3 calls the `.snap.json` "derived and disposable". `load()` disagreed: it
trusted any snapshot whose version was `<= len(events)` and replayed the log's
tail onto it, without ever checking that the snapshot summarised *this* log's
prefix. Two collectif processes on one notebook directory, a restored backup,
or a rewritten log all produce a snapshot of a different history that happens
to reach a plausible length — and the document served afterwards is silently
wrong.

Found by regenerating a replay notebook while a server still held it open: the
served document reported no session while its own log recorded one, so the
fidelity block vanished from a notebook that should have had it. Snapshots now
carry the id of the last event they folded, and one that cannot be matched
against the log is discarded rather than believed. Re-folding a log is cheap;
serving a wrong document is not.

### Filtered out is not thrown away

The projector's filters make a session readable by keeping the machinery out
of the conversation. P0 treated that as a reason to *discard* it. That was
wrong, and reading the DeepSeek harness's account of its own session log —
"everything the model sees is recorded … and every context injection" — is
what made it obvious.

An injection you cannot see is often the whole explanation for a surprising
turn. A real session's first cell is not the prompt: it is 900 KB of skill
listings, hook output and a `<EXTREMELY_IMPORTANT>` preamble, and a document
that opens with the prompt is claiming something untrue about how the turn
began.

So injections are now recorded as their own part kind — hidden by default,
present in the record, folded by the renderer into one line per run ("12
context injections, 340 KB the model read that nobody typed"). What is stored
is the fact, the label and the size, plus a 240-byte excerpt; never the body.
The CLI's transcript is the archive and this is a view over it, so duplicating
every injection would make the notebook cost the size of everything ever put
in front of the model. Measured on a real 3,626-line session: 62 injections,
0.99 MB of context made visible, for 2% growth in the log.

One type is excluded and it earns the exception with a number: that session
carried 671 copies of `<total_tokens>N tokens left</total_tokens>`, one per
turn. Recording them would bury the preamble that matters, and a list nobody
can find anything in fails the same way as recording nothing. The exclusion
list is one entry long on purpose — a blocklist that grows by guesswork drifts
back into hiding things.

We did not take their plugin architecture. "Everything is a plugin" on a
kernel is the opposite of §8's non-goal, and MCP is already the extension
story.

### A field's shape must not cost the line

Building the above found a bug that had been silently eating data since P0.
`attachment.content` is polymorphic in the real format — a string for a hook,
an array for injected context, an object for a file reference — and it had
been declared as a string. `json.Unmarshal` therefore failed on the *whole
line*, so 79 injections vanished with no error and no log entry, including the
preamble that shaped every turn beneath it.

This is P0's lesson one level down. There the risk was misreading a field; here
it is that one surprising field discards everything around it. Payload fields
are now read raw and flattened by shape, and the boolean flags are read
through a tolerant helper, so a type that drifts between Claude Code releases
costs us that field rather than the turn.

### One more thing collectif was doing to itself

`cmd.Env = append(os.Environ(), …)` passed collectif's whole environment to
every CLI it spawned. Launched from inside a Claude Code session — which is how
it is developed — the child inherited `CLAUDE_CODE_CHILD_SESSION` and turned
its own transcript off. Before ADR 0002 that cost telemetry. Now it costs the
entire notebook: no transcript, no projection, no document, and nothing
anywhere saying why. Every adapter now scrubs the parent's session identity.

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
| ~~The transcript does not carry enough to reconstruct a turn~~ | **Retired by P0 slice A** for Claude Code — prompts, prose, tool calls and results all project. Thinking does not, and that is now a known ceiling rather than a risk |
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
4. ~~**What happens to a mirrored cell when the transcript is rewritten**
   (compaction)?~~ **Answered by P0.** Claude Code appends a summary line and
   never rewrites, so no marker is needed — the summary is projected as its own
   part kind and rendered in place.
5. **Should detached notebooks and session notebooks live in the same list?**
   The sidebar currently shows both under separate headings, which is a UI
   answer to an unanswered modelling question.
6. **What settles the last turn of a session that is still running?** Today a
   turn is closed by the next prompt or by the session ending, so the live
   cell shows `running` for as long as the agent is idle at a prompt. Hooks
   (`Stop`) would close it precisely; P1 territory.
7. **Do abandoned branches belong in the document at all?** They are currently
   shown as interrupted cells. Hiding them would read better and record less;
   the log keeps them either way.
