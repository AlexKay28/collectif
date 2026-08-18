# ADR 0001 — collectif becomes a notebook-native agent harness

- **Status**: Amended by [ADR 0002](0002-notebook-is-the-agent-surface.md)
- **Date**: 2026-08-16
- **Supersedes**: none
- **Related**: #46 (multi-CLI adapters), #42 (harness telemetry), #41 (visual control), #29 (Session decomposition), #17/#16/#15 (multi-agent)
- **API surface verified**: 2026-08-16, against the current Anthropic Messages API, the MCP transport spec, and the `anthropic-sdk-go` surface. Anything below marked *(beta)* is gated by a beta header and may move.

> **Read 0002 first.** This ADR aimed the notebook at the wrong layer. Its D1
> ("collectif runs its own agent loop") and D8 ("notebook is the product;
> PTY/CLI mode goes legacy") are replaced by 0002: the notebook replaces the
> **terminal**, not the CLI. Everything below about the document model,
> context projection, the event log, and cache economics stands — the loop it
> describes is now one of two backends rather than the only one.
>
> Concretely, as shipped: the dashboard is still the front door, selecting a
> session there shows its notebook where the xterm panel used to be, and the
> terminal is a tab beside it. Nothing went legacy and there is no flag to
> bring anything back, because nothing was taken away. If you are reading
> this document to find out how collectif works today, §4 (the document
> model, projection, the log) is still accurate and §2 D1/D8 and §6 are not.

---

## 1. Context

### Where collectif is today

collectif spawns coding-agent CLIs (`claude`, `codex`, `opencode`) in a PTY and
mirrors them to a browser dashboard. Everything it knows about a session is
**observed second-hand**:

| Signal | How we get it | Fidelity |
|---|---|---|
| Status | Claude Code hook callbacks → `/api/hooks` | Good, but Claude-specific |
| Tokens / model | Tailing `~/.claude/projects/**/<sid>.jsonl` | Good, but per-CLI parsing |
| Tool calls | `PreToolUse` / `PostToolUse` hooks | Name + input only |
| Agent output | Raw PTY bytes into a ring buffer | ANSI soup — unparseable |
| Approvals | Scraping ink menus with regexes (`menu.go`) | Fragile |

That last row is the ceiling. We answer a permission prompt by writing `yes\r`
into a PTY and, if the pending flag is still set 1.5 s later, guessing again
with `1\r` (`api.go:handleAgentAnswer`). This is an impressive amount of
engineering spent on *not being able to see what the agent is doing*.

The #46 adapter work made the coupling explicit and pluggable, which was the
right move — and in doing so it drew the map of exactly how much of the product
is downstream of a signal some other program chooses to emit. Adding a fourth
CLI adds a fourth transcript parser, a fourth hook dialect, and a fourth set of
degraded panels.

### Second-hand knowledge decays silently

A concrete instance, found while writing this ADR. `harness.go:contextLimits`
matches model prefixes `claude-opus-4`, `claude-sonnet-4`, `claude-haiku-4`,
and the 3-series, each mapped to 200 000 tokens, with a 200 000 default —
and `claudeAdapter.ModelContextLimit` delegates straight to it
(`adapter_claude.go:129`).

Every current model has a **1M-token** context window. A session on
`claude-opus-5` therefore falls through to the 200 000 default and the pressure
gauge reads **five times high**, so `maybeBroadcastContextPressure` fires
`warn` at ~14 % real usage and `critical` at ~18 %. The feature isn't
degraded; it's inverted — it cries wolf on every non-trivial session.

Nothing in the codebase could have caught this, because the number is a guess
about someone else's product. That is the deeper argument for owning the loop:
**a harness that makes the request knows the model and the budget first-hand**,
and can read the real usage back off the response instead of inferring it.

### What the reference points do differently

[Hermes](https://arize.com/blog/how-hermes-implements-open-source-agent-harness-architecture/)
(Nous Research) is a model-agnostic **harness**: it owns the loop — model call,
tool dispatch, append result, repeat — with provider abstraction normalizing
transport quirks, plus skills and memory. Its UI is a single HTML file over a
thin proxy. [OpenClaw](https://clawdocs.org/reference/cli) is a persistent
daemon that owns its agents and reaches them through many channels.

Both own the agent. Neither has a notebook.

### The opportunity

Jupyter's insight was never "cells are pretty". It was that an interleaved
document of *authored intent* and *reproduced output* is a better artifact than
a scrollback — you can edit it, re-run it, hand it to someone, and keep it in
the repo. No agent tool has taken that seriously. Agent work is currently
delivered as chat scrollback, which is the worst of both worlds: not a
transcript you can trust, not a document you can re-run.

If collectif owns the loop, every turn is structured by construction, and the
notebook stops being a rendering trick and becomes the actual data model.

---

## 2. Decisions

| # | Decision | Rejected alternative |
|---|---|---|
| D1 | ~~collectif runs its own agent loop in Go~~ — **narrowed by 0002 D10**: the loop is retained, but as the backend for notebooks with no CLI session attached | Keep mirroring CLIs; keep the fidelity ceiling |
| D2 | Provider-agnostic (Anthropic Messages via `anthropic-sdk-go` + an OpenAI-compatible transport) | Anthropic-only v1; reuse CLI auth via headless mode |
| D3 | Mixed-cell authored document (markdown / prompt / shell / file) | Linear prompt+turn chat; strict Jupyter re-execution |
| D4 | Server-authoritative, event-sourced notebook; thin browser client | Fat browser + stateless server; formal kernel protocol |
| D5 | Small built-in tool set + MCP client | Built-ins only; tools-as-cells |
| D6 | Policy rules with inline approval widgets; hard cwd containment | Trust-the-operator; OS sandboxing in v1 |
| D7 | Subagents via a `task` tool, rendered as nested outputs | Fan-out compare cells; one-agent-per-notebook |
| D8 | ~~Notebook is the product; PTY/CLI mode goes legacy~~ — **replaced by 0002 D8′**: the notebook is the default *view of a session*; the CLIs stay primary | Two coexisting paradigms; hard-reset new repo |

D4's rationale in full, since it's load-bearing: the Go server owning an
append-only event log per notebook means refresh and reconnect are a re-fold,
streaming is just events, subagent nesting is a `parent_id`, and **a run
survives with no browser attached** — which matters, because long agent runs
must not die with a tab. It also reuses plumbing that already works: the WS
fan-out, the drop-don't-block subscriber queues (`session.go:wsSub`), and the
50 ms upsert coalescing.

We take exactly one idea from the kernel-protocol alternative: an internal
`Executor` interface behind the cell runner, because a second implementation is
already known (legacy CLI-backed execution, later remote/sandboxed). Same bet
`CLIAdapter` made in #46. We do **not** build a wire protocol to serve one
frontend we control in the same process.

---

## 3. Architecture

```
                          browser (vanilla ES modules, no build step)
                                        │
                     HTTP mutations     │     WS event stream
                  POST /api/nb/*        │     /ws/notebook/<id>
                                        ▼
┌──────────────────────────────────────────────────────────────────┐
│  Notebook Store          fold(events) → Notebook                  │
│  ├── event log (.jsonl, append-only, source of truth)             │
│  └── snapshot (.snap.json, derived cache for fast open)           │
└───────────────┬──────────────────────────────────────────────────┘
                │ run_cell
                ▼
┌──────────────────────────────────────────────────────────────────┐
│  Cell Runner  ──── Executor iface ──── nativeExecutor (v1)        │
│                                   └──── cliExecutor (legacy)      │
│      │                                                            │
│      ├── Context Projector   cells[0..i) → []Message              │
│      ├── Agent Loop          stream → tool_use → dispatch → loop  │
│      ├── Policy Engine       allow / ask / deny + cwd containment │
│      └── Tool Registry       built-ins + MCP-discovered           │
└───────────────┬──────────────────────────────────────────────────┘
                ▼
     Provider iface ──┬── anthropicProvider   (anthropic-sdk-go)
                      └── openaiCompatProvider (OpenAI, Ollama, vLLM,
                                                OpenRouter, AI Gateway)
```

New files, keeping the existing flat-package convention:

```
src/nb_doc.go        cell + notebook types, fold(), projection
src/nb_store.go      event log, snapshot, open/save, per-notebook registry
src/nb_api.go        HTTP handlers under /api/nb/*
src/nb_ws.go         /ws/notebook/<id> — fold on connect, tail live
src/nb_run.go        cell runner, Executor interface, agent loop
src/provider.go      Provider interface + shared types
src/provider_anthropic.go   wraps anthropic-sdk-go
src/provider_openai.go      hand-rolled chat-completions transport
src/tools.go         built-in tool registry + JSON schemas
src/tools_fs.go      read / write / edit / glob / grep (+ containment)
src/tools_bash.go    bash with streaming output
src/mcp.go           MCP client: stdio + Streamable HTTP
src/policy.go        permission rules, matching, approval lifecycle
src/nb_subagent.go   task tool, nested runs, depth/concurrency caps
src/static/nb*.js    notebook UI modules
```

---

## 4. Functionality

### 4.1 The document model

A notebook is an ordered list of cells plus metadata. Following Jupyter, a cell
separates **source** (authored, user-owned) from **outputs** (produced,
machine-owned).

```go
type Notebook struct {
    ID       string            // uuid
    Title    string
    Root     string            // working directory; all tools contained here
    Meta     NotebookMeta      // default model, budget, policy overrides
    Cells    []Cell
    Version  int               // event count folded in
}

type Cell struct {
    ID       string
    Type     CellType          // markdown | prompt | shell | file
    Source   string
    Meta     CellMeta          // per-cell model/effort override, tags, collapsed
    Outputs  []Output
    State    CellState         // idle | queued | running | ok | error | stale
    RunID    string            // current/last run
    Started  time.Time
    Duration time.Duration
    Usage    Usage             // tokens + cost for this cell, first-hand
}
```

**Cell types**

| Type | Source | Runs by | Contributes to context |
|---|---|---|---|
| `markdown` | prose | not executed | as a user message (authored intent is instruction) |
| `prompt` | the instruction to the agent | agent loop | user message + the assistant turn it produced |
| `shell` | a command line | direct exec, no model | `$ cmd` + captured output, truncated |
| `file` | a path + optional line range | re-reads the file | file contents, re-read fresh on each projection |

`file` cells are how you pin context deliberately instead of hoping the agent
greps for the right thing. They re-read at projection time, so a notebook
re-run after an edit sees the new file.

**Output types** — a discriminated union rendered by the client:

`text` · `thinking` · `tool_call` · `tool_result` · `diff` · `image` ·
`error` · `subagent` · `approval`

`diff` deserves its own type rather than being text: file-editing tools produce
diffs, and a rendered unified diff is the single highest-value thing a
notebook can show that a terminal cannot.

**On `thinking`:** current models return **no raw chain of thought**, and
`thinking.display` defaults to `"omitted"` — meaning thinking blocks arrive
with empty text unless we ask for a summary. If we want the notebook to render
reasoning at all, every request must set
`thinking: {type: "adaptive", display: "summarized"}`. Miss this and the panel
looks broken (blocks present, text blank) rather than absent.

### 4.2 Context projection — the key mechanic

The hard problem with editable, out-of-order cells is that conversation context
is normally *accumulated*, and you cannot un-say a message. This is Jupyter's
hidden-state problem in a new costume: in Jupyter the kernel holds variables
that no longer match the visible code, so deleting a cell doesn't delete its
effects. [marimo](https://marimo.io/blog/dataflow) fixed that by making the
notebook a dataflow graph and *deriving* state rather than accumulating it.

We apply the same move to conversation context: **context is derived, never
accumulated.**

To run cell *i*, the projector folds cells `[0, i)` into a message list, each
cell contributing according to its type (table above), then appends cell *i*'s
own source. Nothing is snapshotted; there is no kernel state to rewind.

Consequences that fall out for free:

- Editing cell 3 and re-running it is well-defined — its context is rebuilt.
- Re-running cell 3 marks cells 4+ `stale` (a visual mark, not an auto-run).
  Staleness is advisory; the user decides whether to re-run downstream.
  (marimo re-runs dependents automatically; we don't, because re-running an
  agent turn costs real money and may touch the filesystem. Advisory staleness
  is the honest version of reactivity when effects aren't free.)
- Deleting a cell simply removes it from every future projection.
- A notebook is genuinely re-runnable on a fresh checkout.

**The cost, and how caching pays it.** Each run re-sends the prefix. Prompt
caching makes that cheap, but only if the projector is built around how the
cache actually works — it is a **prefix match over exact bytes**, rendered in
the order `tools` → `system` → `messages`, so one changed byte invalidates
everything after it. Four rules follow, and they are design constraints on the
projector, not optimizations to add later:

1. **Deterministic rendering.** No timestamps, no map iteration, no unsorted
   JSON anywhere in the projection. Tool schemas are sorted by name and stable
   across runs, because tools render at position 0 and any churn there
   invalidates the entire prefix.
2. **Breakpoint placement.** At most **4** `cache_control` breakpoints per
   request. We spend them on: end of tools+system, end of the stable cell
   prefix, and the last turn of a running loop. The minimum cacheable prefix is
   512 tokens on `claude-opus-5` (1024 on most others) — below that, caching
   silently does nothing.
3. **The 20-block lookback.** A breakpoint searches backward at most **20
   content blocks** for a prior entry. A tool-heavy cell blows past that in one
   turn, so the loop inserts an intermediate breakpoint roughly every 15 blocks
   or the next request silently misses.
4. **Truncation in projection, not in the document.** `shell` output and
   `tool_result` bodies are truncated head+tail with an elision marker when
   projected; the full text stays in the notebook. An oversized projection is
   an explicit "context too large" error, never a silent trim.

### 4.3 Event log and persistence

Truth is an append-only log; the document is a fold.

```
.collectif/notebooks/<slug>.jsonl        ← source of truth
.collectif/notebooks/<slug>.snap.json    ← derived cache, safe to delete
```

This mirrors the precedent already set by the GitHub mirror
(`.collectif/cache/gh/`, `docs/gh-api-contract.md`).

Event types: `notebook_created` · `meta_set` · `cell_inserted` ·
`cell_edited` · `cell_moved` · `cell_deleted` · `run_started` ·
`output_appended` · `run_finished` · `approval_requested` ·
`approval_resolved` · `cells_invalidated`

Every event carries `{v, type, at, id, ...}`. **Unknown event types are
preserved on read and skipped by the fold**, so a notebook written by a newer
build degrades rather than corrupts.

**Streaming deltas are never persisted.** Token-level `output_delta` frames go
to WS subscribers only; the log receives one finalized `output_appended` per
output block. Without this rule a chatty session would produce a
hundred-megabyte document.

Snapshots are written every 200 events and on clean close; the log is the
authority on mismatch. Log compaction (fold → new log) is deferred until
someone actually has a slow notebook.

### 4.4 The agent loop

```
run(cell):
  msgs   := project(notebook, cell.index)
  tools  := registry.Enabled(notebook)          // built-ins + MCP, sorted
  for turn := 0; turn < maxTurns; turn++ {
      stream := provider.Stream(model, msgs, tools, effort, taskBudget)
      for chunk := range stream {
          emit output_delta                     // WS only
      }
      if stopReason == refusal { emit error output; break }
      finalize blocks → emit output_appended    // persisted
      record usage first-hand from the response
      if no tool_use in the turn { break }
      for each tool_use (sequential in v1) {
          decision := policy.Evaluate(tool, input, notebook.Root)
          if decision == ask {
              emit approval_requested; block until resolved or timeout
          }
          if denied { result = "denied by policy" }
          else      { result = executor.Run(tool, input) }
          emit tool_call + tool_result outputs
          append result to msgs
      }
      if budget.Exceeded() { pause the run; emit error output }
  }
```

**Three different budgets, deliberately.** They are easy to conflate and do
different jobs:

| Control | Enforced by | Semantics |
|---|---|---|
| `max_tokens` | the API | Hard per-response cap. The model is *not* aware of it — hitting it truncates mid-thought. |
| `task_budget` *(beta)* | the model | Advisory token budget for the whole loop; the model sees a countdown and paces itself to finish gracefully. Minimum 20 000. |
| `Notebook.Meta.Budget` | us | Hard dollar cap, checked between turns, reusing `costcap.go`. Pauses the run. |

We set all three: a generous `max_tokens` (thinking counts against it — see
below), a `task_budget` derived from the notebook's remaining dollar budget, and
our own hard stop as the backstop.

**Thinking is on by default** on current models, and `max_tokens` caps thinking
*plus* response text together. A cell sized tightly around its expected answer
will truncate. The runner therefore floors `max_tokens` generously (64 k at
high effort) rather than trusting a per-cell guess.

**Refusals are a normal outcome, not an error.** A declined request returns
HTTP 200 with `stop_reason: "refusal"` and a possibly-empty `content` array —
code that reads `content[0]` unconditionally panics. The loop checks
`stop_reason` first, renders a distinct refusal output, and (where the provider
supports it) opts into server-side fallbacks so a false-positive on benign
security work is re-served by another model instead of dead-ending the cell.

Interrupt: `POST /api/nb/<id>/cells/<cid>/interrupt` cancels the run context.
A cancelled run finalizes whatever it has, emits `run_finished{status:
interrupted}`, and terminates in-flight tool processes via process-group kill —
the same technique `main.go:shutdownAllSessions` already uses.

Turn cap defaults to 40 with a clear terminal error, so a stuck loop costs
money once, not indefinitely.

### 4.5 Tools

**Built-ins** (Go, six of them):

| Tool | Input | Notes |
|---|---|---|
| `read` | path, offset?, limit? | contained to notebook root |
| `write` | path, content | emits a `diff` output |
| `edit` | path, old, new, replace_all? | exact-match; emits a `diff` output |
| `bash` | command, timeout? | streams stdout/stderr as output deltas |
| `glob` | pattern | contained |
| `grep` | pattern, path?, glob? | contained |

All six are declared as **custom tools with strict schemas**
(`strict: true`, `additionalProperties: false`, explicit `required`), so
arguments are guaranteed to validate and the runner never hand-parses a
malformed input. Descriptions state *when* to call the tool, not just what it
does — current models reach for tools conservatively, and trigger conditions in
the description measurably raise the should-call rate.

**Containment is not a policy rule.** Every path-taking tool resolves symlinks
and verifies the result is under `notebook.Root` before the policy engine is
consulted. Policy can loosen what you may *do*; it cannot loosen *where*.

**MCP client** — collectif speaks MCP as a client, so the tool ecosystem is
someone else's problem. The spec defines exactly two current transports —
**stdio** for local servers and **Streamable HTTP** for remote — and we
implement both; the deprecated HTTP+SSE transport is still common in the wild,
so we accept it as a compatibility path rather than a first-class target.
Config is read from `~/.config/collectif/mcp.json` using the conventional
`mcpServers` shape so users can paste an existing config. Servers connect
lazily on first notebook open that enables them; tools are namespaced
`mcp__<server>__<tool>` and surfaced in the tool picker with their server's
health. A failed server degrades to "tools unavailable" rather than failing the
run.

### 4.6 Permissions

Rules live at `~/.config/collectif/permissions.json`, overridable per notebook
in `Meta`:

```json
{
  "deny":  ["read(**/.env)", "read(**/.git/config)", "bash(rm -rf *)"],
  "allow": ["read(**)", "glob(**)", "grep(**)", "bash(git status)", "bash(git diff*)"],
  "ask":   ["write(**)", "edit(**)", "bash(*)"]
}
```

Evaluation order is `deny` → `allow` → `ask` → default **ask**. Deny always
wins; an unmatched tool call always asks. When a call falls to `ask`, the run
blocks and an `approval` output renders inline in the cell showing the tool,
the arguments, and — for `write`/`edit` — the proposed diff. The user picks:

- **Once** — this call only
- **Session** — this notebook, until the server restarts
- **Always** — appends a rule to the config file (shown before writing)
- **Deny** — returns a denial as the tool result; the agent sees it and adapts

Unanswered approvals expire after 15 minutes and resolve as denied, reusing the
sweeper pattern from `session.go:startPendingSweeper`.

Every decision — rule matched, who approved, what ran — is an event in the log.
The notebook is the audit trail.

### 4.7 Subagents

A built-in `task` tool spawns a child run with a **fresh context spine**
(system prompt + the task description only — not the parent's transcript), its
own tool policy (never broader than the parent's), and its own budget slice.

Child events carry `parent_run_id` and render as a nested, collapsed
`subagent` output inside the parent cell — expandable to the child's full
turn list. The child's final text becomes the parent's tool result.

Caps: depth 2, four concurrent children, child budget deducted from the
notebook budget.

**Stagger the fan-out.** A cache entry only becomes readable once the first
response *begins streaming*; N children launched simultaneously with the same
system prefix all miss and all pay full price. The runner fires the first
child, waits for its first streamed token, then releases the rest — a one-line
scheduling rule that turns a 4× cold-prefix bill into 1×.

Agent definitions are read from the existing `.claude/agents/*.md` files via
`subagents.go` — name, description, tools, model, prompt. This keeps the Team
tab meaningful and means anyone's existing subagent library works on day one.

### 4.8 Cost, telemetry and health

Reused rather than rebuilt — but now fed first-hand. Usage arrives on every
response (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
`cache_read_input_tokens`) instead of being scraped from a JSONL tail, which
makes `transcript.go`'s watcher unnecessary for native notebooks:

- `costcap.go` — per-notebook USD budget and the hourly global cap, unchanged
  in shape; the run loop checks it between turns.
- `harness.go` — context pressure becomes **exact**: we know the request size
  before we send it, so we can warn *before* a turn rather than after. The
  model→limit table moves to the provider, which is the only component with a
  first-hand reason to know it — and gets the current values (see §5).
- **Cache-hit rate becomes a first-class metric.** `cache_read_input_tokens`
  sitting at zero across repeated runs is the canary for a projection bug; the
  notebook surfaces it per cell.
- `notify.go` — webhook fires on run completion, approval-requested, budget
  pause.
- `/metrics` — add per-notebook run counts, tool-call counts, token totals.

---

## 5. Integrations

| Integration | Direction | Detail |
|---|---|---|
| **Anthropic Messages API** | outbound | Via the official `github.com/anthropics/anthropic-sdk-go` rather than hand-rolled HTTP — it already models streaming accumulation, tool-use blocks, thinking config, and `cache_control`. Reference implementation for the `Provider` interface. |
| **OpenAI-compatible** | outbound | One hand-rolled transport covering OpenAI, Ollama, vLLM, llama.cpp, OpenRouter and Vercel AI Gateway via `base_url`. Normalizes `tool_calls` arrays ↔ content blocks, and reasoning summaries ↔ `thinking`. |
| **MCP servers** | outbound | stdio + Streamable HTTP (the two current spec transports), with legacy HTTP+SSE accepted for compatibility. Conventional `mcpServers` config shape. Namespaced, lazily connected, health-surfaced. |
| **Filesystem** | bidirectional | Built-in tools, rooted and symlink-resolved at `notebook.Root`. `file` cells re-read at projection. |
| **git** | outbound | Diff rendering prefers `git diff` when the root is a repo; `shell` cells make git a first-class notebook citizen without a dedicated tool. |
| **GitHub mirror (#44)** | inbound | Unchanged. Later: an `issue` cell type that pulls a cached issue body into context — the read-only cache is already the right shape. |
| **`.claude/agents/*.md`** | inbound | Parsed by `subagents.go` as subagent templates for the `task` tool. |
| **Legacy PTY / CLIAdapter (#46)** | internal | Retained behind the `Executor` interface as `cliExecutor`, so a prompt cell can be executed by a spawned CLI where subscription billing or a CLI-only feature matters. |
| **Outbound webhooks (#36)** | outbound | Run lifecycle, approvals, budget pauses. |
| **Browser** | bidirectional | HTTP for mutations, WS for the event stream, existing shared-secret auth on both. |

### Model catalog (replaces the stale table in `harness.go`)

Each provider owns its own catalog. The Anthropic one, current as of this ADR:

| Model | Context | Max output | Notes |
|---|---|---|---|
| `claude-opus-5` | 1M | 128K | Default. Thinking on by default; disabling it is rejected above `high` effort. 512-token cache minimum. |
| `claude-sonnet-5` | 1M | 128K | Cost/latency step-down. |
| `claude-fable-5` | 1M | 128K | Highest capability, premium pricing, always-on thinking. |
| `claude-haiku-4-5` | 200K | 64K | Cheap subagent worker. |

Two things follow for the UI. First, the pressure gauge must read its limit
from the provider, never from a package-level map — that indirection is the
whole fix for the bug in §1. Second, a `low`/`medium`/`high`/`xhigh`/`max`
**effort** control belongs next to the model picker: it is the primary
cost/latency lever, and per-cell override is exactly the kind of knob a
notebook is good at exposing.

### API surface

Mutations are HTTP (curl-able, testable, idempotent); the stream is WS. This
matches the existing split in `server.go`.

```
GET    /api/nb                          list notebooks
POST   /api/nb                          create   {title, root}
GET    /api/nb/<id>                     folded document
DELETE /api/nb/<id>
PATCH  /api/nb/<id>                     meta: model, effort, budget, policy
POST   /api/nb/<id>/cells               insert   {type, source, afterCellID}
PATCH  /api/nb/<id>/cells/<cid>         edit source / meta
DELETE /api/nb/<id>/cells/<cid>
POST   /api/nb/<id>/cells/<cid>/move    {beforeCellID}
POST   /api/nb/<id>/cells/<cid>/run
POST   /api/nb/<id>/cells/<cid>/interrupt
POST   /api/nb/<id>/run-all
POST   /api/nb/<id>/approvals/<aid>     {decision: once|session|always|deny}
GET    /api/nb/<id>/export              markdown export
GET    /api/providers                   configured providers + model catalogs
GET    /api/tools                       built-ins + MCP tools + health
WS     /ws/notebook/<id>                fold on connect, then live events
```

### Frontend

Vanilla ES modules embedded via `go:embed`, no build step — the repo's existing
stance, and it survives the CSP-strict, offline-capable constraints that a
CDN-React single-file page does not. New modules `nb.js` (store + WS),
`nb_cells.js` (cell CRUD, keyboard model), `nb_render.js` (output renderers),
`nb.css`. Two small pieces of new rendering code are needed: a minimal markdown
renderer and a unified-diff colorizer. Jupyter's keyboard model (`Esc`/`Enter`
modes, `a`/`b`/`dd`, `Shift+Enter`) is adopted verbatim — it is muscle memory
for the target user.

---

## 6. Roadmap

Each phase ships something usable on its own and retires a specific risk.

| Phase | Scope | Exit criteria | Risk retired |
|---|---|---|---|
| **M0 — Fix the gauge** *(days)* | Correct the model→context map, route it through the adapter, add the current model IDs | A `claude-opus-5` session reports true context pressure; no false `critical` at 18 % | Ships value before the milestone starts, and proves the "first-hand beats second-hand" thesis in one diff |
| **M1 — Notebook core** | `nb_doc`/`nb_store`/`nb_api`/`nb_ws`, markdown + shell cells, run/interrupt, UI shell, keyboard model | Create a notebook, mix markdown and shell cells, run them, hard-refresh the browser, state intact; kill the server, reopen, state intact | Persistence and the event model — the thing everything else is built on |
| **M2 — Native loop, one provider** | `Provider` + `anthropic-sdk-go`, prompt cells, streaming outputs, context projection, read-only tools (`read`/`glob`/`grep`), refusal handling, `display: "summarized"` | Hold a real conversation about the repo; per-cell usage and cost from the response; edit an earlier cell, re-run, downstream marks stale; a refusal renders as a refusal | The loop, streaming, and the projection mechanic |
| **M2.5 — Make the cache pay** | Deterministic rendering, breakpoint placement, 20-block intermediate breakpoints, per-cell cache-hit metric | Re-running the last cell of a 10-cell notebook shows a non-zero `cache_read_input_tokens` and a materially lower bill than the first run | Economic viability of re-projection — the one number that decides whether D3 is affordable |
| **M3 — Write tools + policy** | `write`/`edit`/`bash`, `policy.go`, inline approval widget, containment, diff outputs, audit events | An agent makes a real, reviewed change to a file under `ask` policy; a denial is handled gracefully; `..` escape attempts are rejected | Safety — the first phase where collectif can damage something |
| **M4 — Provider-agnostic** | OpenAI-compatible transport, per-cell model + effort override, normalization test suite against both transports | The same notebook runs end-to-end on Anthropic and on a local Ollama model | Transport drift and the D2 bet |
| **M5 — MCP client** | stdio + Streamable HTTP, config, namespacing, tool picker, health surfacing | A third-party MCP server's tools are callable from a prompt cell; a dead server degrades without failing the run | Ecosystem reach without ecosystem maintenance |
| **M6 — Subagents** | `task` tool, nested rendering, staggered fan-out, `.claude/agents` templates, depth/concurrency caps | A parent cell delegates to two children that run concurrently and roll their results up, with the second child reading cache | Nested execution state — the hardest rendering problem |
| **M7 — Retirement & polish** | Notebook becomes the default route, PTY mode behind a flag, `cliExecutor`, markdown export, docs, migration note | A new user lands on the notebook; the dashboard is reachable but no longer the front door | Product coherence |

Ordering rationale: M1 before M2 because an unreliable document makes every
later bug ambiguous. **M2.5 is carved out as its own gate** because
re-projection is the design's one economic bet — if the cache doesn't land, the
mixed-cell model is unaffordable and we should know that at phase three, not
phase seven. M3 after M2 because writing tools before you can watch a loop is
how you find out about a bug by reading `git status`. M4 before M5 because
provider normalization changes tool schemas and MCP inherits them.

M1–M3 is the point at which this is a usable product for its author; M4–M6 is
the point at which it is a compelling one for anyone else.

---

## 7. Consequences

**Good**

- Every turn is structured by construction. The approval UI stops being regex
  archaeology on ANSI bytes and becomes a rendered diff with buttons.
- Telemetry stops being a guess. Usage, cost, and context pressure come off the
  response; the class of bug in §1 becomes impossible rather than latent.
- The notebook is a durable artifact: committable, reviewable, re-runnable,
  and a genuine audit trail of what an agent did to a repo.
- Provider-agnosticism becomes real leverage rather than a compatibility
  matrix — the same document runs on a frontier model and a local one.
- The per-CLI parsing burden goes away. No fourth transcript dialect.
- Existing infrastructure — auth, WS fan-out, cost caps, webhooks, gh mirror —
  transfers almost unchanged.

**Bad**

- **Billing changes for users.** Subscription-billed CLI usage is replaced by
  per-token API spend. This is the single biggest adoption cost, and it is why
  `cliExecutor` is retained rather than deleted.
- collectif now competes with the CLIs instead of orchestrating them. The
  multi-CLI fleet story built in #46 is demoted to a compatibility path.
- We take on permanent maintenance of provider transports and model catalogs,
  which drift — the difference is that now they drift somewhere we control and
  can test, rather than inside a guess.
- The event schema is a compatibility commitment from M1 onward — those files
  are user documents.

**Risks**

| Risk | Mitigation |
|---|---|
| Prompt injection now has teeth: we execute, not observe | Default-ask policy, hard cwd containment, deny rules on secrets by default, full audit log; OS sandboxing revisited post-M7 |
| Context re-projection inflates token spend | M2.5 exists to measure exactly this: deterministic rendering, breakpoint discipline, truncated tool results, explicit oversize errors, and a per-cell cache-hit number that makes regressions visible |
| Beta-gated features (`task_budget`, compaction, context editing) move | Each is optional and behind a capability check; the loop degrades to `max_tokens` + our own budget if a beta disappears |
| Scope: eight phases is a long road | Each phase is independently shippable; M0 lands in days, and M1–M3 is a coherent product on its own |
| Notebook files corrupt or drift across versions | Append-only log, forward-compatible fold, snapshot is derived and disposable |

---

## 8. Non-goals

- **Reimplementing Claude Code.** We are not chasing feature parity with the
  CLIs; we are building the surface they don't have.
- **Multi-user or hosted operation.** Loopback-bound, single-operator,
  shared-secret. Unchanged.
- **A plugin/skill marketplace.** MCP is the extension story. There is no
  second one.
- **Training or fine-tuning.** Hermes's learning loop is out of scope.
- **Full marimo-style reactivity.** We borrow derived state; we do not
  auto-re-run dependents, because agent turns cost money and touch disks.
- **Editing notebooks collaboratively.** Multiple browser tabs may watch one
  notebook; concurrent editing is last-writer-wins, not a CRDT.

---

## 9. Open questions

1. **Notebook location.** `.collectif/notebooks/` in the repo (proposed —
   matches the gh cache precedent and makes notebooks committable) versus
   `~/.config/collectif/notebooks/` (keeps user repos clean). Proposed:
   in-repo, gitignored by default, with an explicit "commit this notebook"
   action.
2. **Interrupt mid-tool.** Kill the tool process and return a partial result,
   or let it finish and discard? Proposed: kill, and record what was captured.
3. **`run-all` semantics** on a notebook with pending approvals — queue and
   prompt serially, or fail fast? Proposed: queue serially.
4. **Whether `markdown` cells belong in context at all.** Proposed yes, since
   authored prose in an agent notebook is nearly always instruction — but this
   should be a per-cell toggle if it proves surprising.
5. **Server-side compaction vs. our own truncation.** The API offers compaction
   and context editing *(both beta)* that would handle oversized projections
   for us. Deferred past M4 — our truncation is predictable and testable, and
   a beta that summarizes the user's document without telling them is a bad
   default for an artifact people commit.
