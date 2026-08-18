# Spike #54 — can a projected session show what the CLI's MCP servers are doing?

- **Date**: 2026-08-18
- **Issue**: [#54](../../../issues/54) (M5 — MCP client, deferred)
- **ADR**: [0002 §5](../adr/0002-notebook-is-the-agent-surface.md), which rescoped M5 to
  *"spike surfacing the CLI's `mcp__*` tool calls first — a fraction of the work,
  on the surface that has users"*
- **Answer**: **yes**, and it took 40 lines. **The client in #54 should not be built.**

---

## 1. The question

Under ADR 0002 the primary surface is a mirrored CLI session, and every CLI
collectif spawns already speaks MCP with the user's own servers. A client of our
own would serve detached notebooks only, and would add a second config file to
keep in sync with the one the CLI already reads.

The cheaper idea: Claude Code names MCP tools `mcp__<server>__<tool>`, so tool
calls in the transcript should already identify them. Is that enough to show a
reader which of an agent's calls left the machine, where they went, and what came
back?

Everything below was measured by running the repo's own parser over every
transcript on this machine, not by reading the format. The counts were also
produced independently by a throwaway program that shares no code with
`projection.go`; the two agree exactly on all six MCP figures.

## 2. The numbers

| | |
|---|---|
| Transcripts on this machine | **529** (56 sessions, 473 subagents, 24 projects) |
| Tool calls in them | **39 942** |
| Tool calls that went over MCP | **38** — 0.095 % |
| Transcripts containing one | **3** of 529 (0.6 %), all in **one** project |
| Subagent transcripts containing one | **0** of 473 |
| Distinct servers actually called | **3** |
| Distinct MCP tools actually called | **9** |
| MCP results paired back to their call | **42** |
| …of which failed (`is_error`) | **6** |
| Files that *mention* `mcp__` | **523** |

The last two rows are the ones that reframe the issue.

**523 files mention `mcp__` and 3 call one.** The other 520 mentions are the
harness listing tool names in a system reminder — the deferred-tools block. Any
approach that greps for the string rather than reading the tool call finds MCP
everywhere and MCP nowhere.

**All 38 calls are one afternoon's work.** Every one is dated 2026-08-04, in the
`solidsight` project, against three servers that all arrived from a single
plugin:

```
23  plugin_paddle_paddle-live
 8  plugin_paddle_paddle-docs
 7  plugin_paddle_paddle-sandbox
```

Not one came from a hand-written `mcpServers` entry. `~/.claude.json` does define
two (`calculator`, `google-calendar`), and neither has ever produced a call.

## 3. What a projection can show today, with no client

All of it comes free, because an MCP call **is an ordinary tool call**. There is
no server field, no transport field, no duration, no separate line type. The name
is the entire signal — and the name is enough for:

- **Which server, which tool.** `mcp__plugin_paddle_paddle-live__execute` splits
  on the first `__` after the prefix. All 72 distinct MCP names in this corpus
  come apart correctly under that rule, across 11 servers; none carries a second
  `__`. Server names do carry single underscores (`claude_ai_Google_Calendar`),
  and so do tool names (`search_paddle_knowledge_sources`), which is why the split
  is on the first separator and not the last.
- **The arguments, in full.** The `input` object is verbatim in the transcript and
  was already being carried into the document.
- **The result, in full.** 26 of the 42 arrived as MCP content-block arrays and 16
  as bare strings; `flattenClaudeResult` already handled both, unchanged.
- **The failures, in the server's own words.** All six read like this, and all six
  are the moment the reader actually needed them:

  > `Error: Checkouts aren't enabled for this account. This typically means that you haven't fully completed the Paddle onboarding process.`
  > `Error: Authentication header included, but incorrectly formatted.`
  > `Error: URL called is invalid.`

- **Authentication, including the moment it was needed.** `authenticate` and
  `complete_authentication` are ordinary MCP tool calls whose results carry the
  OAuth URL and then *"Authentication complete for plugin:paddle:paddle-live. The
  server's tools should now be available."* A projected session shows an
  unauthenticated server being unblocked without knowing what OAuth is.

Two MCP-specific fields do exist, and neither changes the answer:

- `mcpMeta.structuredContent` — the MCP spec's structured result, present on 6
  lines in 2 files. It duplicates text already in the result block.
- `attachment.pendingMcpServers` on a `deferred_tools_delta` — 102 lines across
  **54** files, naming servers that are still connecting. This is the one place a
  server the session never called shows up at all (`claude.ai Gmail`,
  `claude.ai Google Calendar`, `plugin:vercel:vercel`).

## 4. What it cannot show, and what a client would not fix

**A server's health at rest.** `pendingMcpServers` names servers that are *still
connecting*; one that connected instantly and one that failed permanently are
equally absent from it. There is no "server X is down" line anywhere in the
format. The clearest case on this machine: `~/.claude/mcp-needs-auth-cache.json`
records `plugin:vercel:vercel` as needing authentication, and that server appears
in **no transcript at all** — the CLI knows, and never writes it down.

So "is a server dead?" degrades to "did the call that needed it fail?", which the
projection answers exactly. That is a worse answer than a health check and a
better one than a health check of *our* connection, which is the only thing a
client of our own could offer: collectif connecting to the user's Postgres server
and finding it healthy says nothing about whether the CLI's connection to it is.

**Latency.** No duration field on any of the 42 results. A client would have this,
for its own calls only.

**Resources and prompts.** MCP has three primitives; only tools cross into the
transcript. Resources reach Claude Code as `@`-mentions and appear as ordinary
attachments with no server attribution.

**The catalog.** What a server *offers* appears only as prose inside a
system-reminder injection. Parsing it would be exactly the ANSI-scraping mistake
one layer up.

## 5. What was built

The positive half, in full:

- `TranscriptPart.MCPServer` / `.MCPTool`, split by `claudeAdapter.SplitMCPTool`,
  which fails closed. `mcp__claude-in-chrome__` and `mcp__claude_ai_` both occur
  in this corpus — in prose, not as tool names, but they occur — and half a name
  must not become an attribution to a server the user does not have.
- `ToolName` keeps the full wire name. It is what the model called, what the
  result pairs against, and what a reader greps their own transcript for.
- The renderer names the server as a chip beside the short tool name, so a call
  that left the machine does not read as a long built-in tool name.
- `NotebookFidelity.MCP`, derived on read like every other surface, false for
  codex and opencode. It is gated on `TranscriptContent` as well as on the
  adapter knowing a convention, because naming MCP calls is worth nothing on a
  CLI whose tool calls are not projected at all.

Deliberately not built: marking the *result* as MCP. Results carry no name, so it
would need the projector to remember every call id; the call renders directly
above its result, and the pairing already exists in the document.

## 6. Recommendation

**Do not build the client.** Three reasons, in order of weight.

1. **It adds nothing to the surface that has users.** Everything a session
   notebook could render about MCP — server, tool, arguments, result, failure,
   authentication — is already in the transcript, and now renders. A client would
   duplicate the connection, not the information.
2. **Demand is not visible in the data.** 0.095 % of tool calls, one project, one
   afternoon, three servers from one vendor's plugin, zero from a hand-written
   config. #54 is already ranked below #53 and #52; this is what that ranking
   looks like measured.
3. **The one thing it would genuinely add is the wrong thing.** A client can
   report the health of *collectif's* connection to a server. A reader of a
   mirrored session wants the health of the *CLI's* connection, which we cannot
   observe and cannot substitute for. Reporting ours as if it were theirs is the
   stale context-limit map from ADR 0001 §1, rebuilt on purpose.

Revisit only if detached notebooks acquire real users — that is the surface where
a client is the *only* option — and revisit then on evidence from those notebooks,
not from this one.
