package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #54 spike — surfacing the CLI's own MCP servers instead of building a
// client (ADR 0002 §5, M5).
//
// The line shapes below are reduced from the only three transcripts on
// this machine that ever called an MCP tool
// (`~/.claude/projects/-home-minnesota-ClaudeSpace-solidsight/*.jsonl`,
// Claude Code 2.1.231). Fields the projection does not read are dropped;
// the ones that carry the finding are verbatim.
//
// The finding the spike turned on: an MCP call is an ordinary `tool_use`
// block in every respect but its name. There is no server field, no
// transport field, no duration — the entire signal is the
// `mcp__<server>__<tool>` convention, and it is enough for the notebook to
// name the server, the tool, its arguments, its result and its failure.

// ─── Splitting the name ─────────────────────────────────────────────────

func TestProjectClaude_MCPToolCallNamesItsServerAndTool(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u1","timestamp":"2026-08-04T18:53:14.331Z",
	  "message":{"role":"assistant","model":"claude-opus-5","content":[
	    {"type":"tool_use","id":"toolu_016ruVoMpRsqmz8iYX5nacsG",
	     "name":"mcp__plugin_paddle_paddle-live__execute",
	     "input":{"code":"async (client) => client.products.list()"}}]}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	p := parts[0]
	if p.Kind != PartToolCall {
		t.Fatalf("kind = %q, want tool_call", p.Kind)
	}
	if p.MCPServer != "plugin_paddle_paddle-live" {
		t.Errorf("server = %q, want plugin_paddle_paddle-live", p.MCPServer)
	}
	if p.MCPTool != "execute" {
		t.Errorf("tool = %q, want execute", p.MCPTool)
	}
	// The wire name is what the model called and what the result pairs
	// against. Splitting it is a reading aid, not a replacement.
	if p.ToolName != "mcp__plugin_paddle_paddle-live__execute" {
		t.Errorf("tool name was rewritten to %q — the record must keep what was actually called", p.ToolName)
	}
}

func TestProjectClaude_ABuiltInToolClaimsNoServer(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u2","timestamp":"2026-08-04T18:53:14.331Z",
	  "message":{"role":"assistant","content":[
	    {"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	if parts[0].MCPServer != "" || parts[0].MCPTool != "" {
		t.Errorf("Bash was attributed to an MCP server: server=%q tool=%q",
			parts[0].MCPServer, parts[0].MCPTool)
	}
}

// Both of these strings occur in real transcripts on this machine — not as
// tool names but inside prose and skill listings, where a trailing glob or
// a truncated sentence leaves the tool half empty. The splitter fails
// closed on them for the same reason the provenance filter does: a call
// mis-attributed to a server the user does not have is worse than a call
// shown under its plain name.
func TestProjectClaude_AnMCPNameWithNoToolIsNotAnMCPCall(t *testing.T) {
	for _, name := range []string{
		"mcp__claude-in-chrome__", // trailing separator, no tool
		"mcp__claude_ai_",         // no separator at all
		"mcp__",
		"mcp____tool", // empty server
	} {
		line := `{"type":"assistant","uuid":"u3","message":{"role":"assistant","content":[
		  {"type":"tool_use","id":"t1","name":` + quoteJSON(name) + `,"input":{}}]}}`
		parts := projectLine(t, line)
		if len(parts) != 1 {
			t.Fatalf("%q: got %d parts, want 1", name, len(parts))
		}
		if parts[0].MCPServer != "" || parts[0].MCPTool != "" {
			t.Errorf("%q split into server=%q tool=%q — an unusable name became an attribution",
				name, parts[0].MCPServer, parts[0].MCPTool)
		}
		if parts[0].ToolName != name {
			t.Errorf("%q: tool name became %q", name, parts[0].ToolName)
		}
	}
}

// A server name is not guaranteed to be one word — every server actually
// configured on this machine is `plugin_paddle_paddle-live`,
// `claude_ai_Google_Calendar` or similar — and a tool name routinely
// carries single underscores (`search_paddle_knowledge_sources`). The
// split is on the *first* double underscore, which is the only reading
// under which all 72 distinct names in this corpus come apart correctly.
func TestProjectClaude_TheSplitIsOnTheFirstDoubleUnderscore(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"assistant","uuid":"u4","message":{"role":"assistant","content":[
	    {"type":"tool_use","id":"t1","name":"mcp__plugin_paddle_paddle-docs__search_paddle_knowledge_sources",
	     "input":{"query":"pwCustomer"}}]}}`)

	if parts[0].MCPServer != "plugin_paddle_paddle-docs" {
		t.Errorf("server = %q", parts[0].MCPServer)
	}
	if parts[0].MCPTool != "search_paddle_knowledge_sources" {
		t.Errorf("tool = %q", parts[0].MCPTool)
	}
}

// ─── What the notebook can say about a failure ──────────────────────────

// Six of the 42 MCP results in this corpus failed, and every one of them
// arrived as an ordinary `is_error` result carrying the server's own
// sentence. This is what "can a projection show a dead server?" actually
// resolves to: not a health check, but the failure of the call that
// noticed — which is the only moment it mattered.
func TestProjectClaude_AFailedMCPCallCarriesTheServersOwnWords(t *testing.T) {
	parts := projectLine(t, `{
	  "type":"user","uuid":"u5","timestamp":"2026-08-04T19:01:02.001Z",
	  "message":{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"toolu_01VMDYTwxMdc2KqMY7gNwMAf",
	     "is_error":true,"content":"Error: URL called is invalid."}]},
	  "toolUseResult":"Error: URL called is invalid."}`)

	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(parts), parts)
	}
	if !parts[0].IsError {
		t.Error("a failed MCP result did not project as an error")
	}
	if !strings.Contains(parts[0].Text, "URL called is invalid") {
		t.Errorf("the server's explanation was lost: %q", parts[0].Text)
	}
}

// ─── The document ───────────────────────────────────────────────────────

func TestProjector_AnMCPCallIsRenderableAsOne(t *testing.T) {
	st, p := newProjectorFixture(t)

	p.Ingest([]TranscriptPart{part(PartUserText, "check the webhook", "l1")})
	p.Ingest([]TranscriptPart{{
		Kind: PartToolCall, UUID: "l2",
		ToolName: "mcp__plugin_paddle_paddle-live__execute", ToolUseID: "t1",
		MCPServer: "plugin_paddle_paddle-live", MCPTool: "execute",
		ToolInput: json.RawMessage(`{"code":"client.products.list()"}`),
	}})

	outs := st.Doc().Cells[0].Outputs
	if len(outs) != 1 {
		t.Fatalf("got %d outputs, want 1", len(outs))
	}
	if got := outs[0].Data["mcpServer"]; got != "plugin_paddle_paddle-live" {
		t.Errorf("the browser cannot tell which server ran this: %v", outs[0].Data)
	}
	if got := outs[0].Data["mcpTool"]; got != "execute" {
		t.Errorf("mcpTool = %v", outs[0].Data["mcpTool"])
	}
	// The full name stays, because that is what a reader searching their
	// own transcript will grep for.
	if got := outs[0].Data["name"]; got != "mcp__plugin_paddle_paddle-live__execute" {
		t.Errorf("name = %v", got)
	}
}

// A subagent's MCP calls are the parent's blind spot: 0 of the 471
// subagent transcripts here made one, so this is the surface most likely
// to be dropped and least likely to be noticed.
func TestProjector_ASubagentsMCPCallIsIdentifiedToo(t *testing.T) {
	out, ok := subagentOutput(TranscriptPart{
		Kind: PartToolCall, UUID: "l9",
		ToolName: "mcp__slack__slack_read_thread", ToolUseID: "t9",
		MCPServer: "slack", MCPTool: "slack_read_thread",
	}, "ag1", "Explore")
	if !ok {
		t.Fatal("a child's tool call produced no output")
	}
	if out.Data["mcpServer"] != "slack" || out.Data["mcpTool"] != "slack_read_thread" {
		t.Errorf("a child's MCP call lost its server: %v", out.Data)
	}
}

// ─── The honest absence ─────────────────────────────────────────────────

// D11, again. MCP identification rides entirely on Claude Code's naming
// convention; no other CLI has been checked, and neither codex nor
// opencode is installed here to check against. A session that cannot say
// which of its tools were MCP must say so rather than render every call as
// built-in and let the reader assume.
func TestFidelity_MCPIsASurfaceOfItsOwn(t *testing.T) {
	if !fidelityOf(adapters["claude"]).MCP {
		t.Error("claude cannot identify MCP calls, but its transcript names them mcp__<server>__<tool>")
	}
	for _, name := range []string{"codex", "opencode"} {
		if fidelityOf(adapters[name]).MCP {
			t.Errorf("%s claims it can identify MCP calls — no namespacing convention is known for it", name)
		}
	}
	if fidelityOf(nil).MCP {
		t.Error("an unknown adapter claims MCP identification")
	}
}

// ─── Against the machine ────────────────────────────────────────────────

// The spike's actual question, asked of real files rather than of the
// fixtures above, which I wrote both sides of.
//
// It skips when there is nothing to read, and skips again when there are
// transcripts but no MCP calls in them — that is the expected state on
// most machines and is not a failure of the parser. What it will not
// tolerate is a name that matches the convention and does not come apart,
// because that is the case where the notebook would attribute a call to a
// server that does not exist.
func TestProjectClaude_EveryMCPCallOnThisMachineComesApart(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	transcripts, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	children, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*", "subagents", "*.jsonl"))
	transcripts = append(transcripts, children...)
	if len(transcripts) == 0 {
		t.Skip("no Claude Code transcripts on this machine")
	}

	a := &claudeAdapter{}
	servers := map[string]int{}
	var allCalls, mcpCalls, mcpResults, mcpFailures, filesWith int

	// Results carry no name of their own, so a call's id is what says a
	// result belongs to a server. This is the same pairing the notebook
	// does, run over the whole corpus.
	for _, tp := range transcripts {
		f, err := os.Open(tp)
		if err != nil {
			continue
		}
		fromMCP := map[string]bool{}
		here := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			parts, err := a.ProjectTranscriptLine(sc.Bytes())
			if err != nil {
				t.Fatalf("%s: %v", filepath.Base(tp), err)
			}
			for _, p := range parts {
				switch p.Kind {
				case PartToolCall:
					allCalls++
					if !strings.HasPrefix(p.ToolName, "mcp__") {
						continue
					}
					mcpCalls++
					here++
					if p.MCPServer == "" || p.MCPTool == "" {
						t.Errorf("%s: %q matches the MCP convention but split into server=%q tool=%q",
							filepath.Base(tp), p.ToolName, p.MCPServer, p.MCPTool)
						continue
					}
					if p.MCPServer+"__"+p.MCPTool != strings.TrimPrefix(p.ToolName, "mcp__") {
						t.Errorf("%s: %q does not rejoin from %q / %q",
							filepath.Base(tp), p.ToolName, p.MCPServer, p.MCPTool)
					}
					servers[p.MCPServer]++
					fromMCP[p.ToolUseID] = true
				case PartToolResult:
					if fromMCP[p.ToolUseID] {
						mcpResults++
						if p.IsError {
							mcpFailures++
						}
					}
				}
			}
		}
		f.Close()
		if here > 0 {
			filesWith++
		}
	}

	names := make([]string, 0, len(servers))
	for s, n := range servers {
		names = append(names, s+"="+itoa(n))
	}
	t.Logf("%d transcripts, %d tool calls, %d via MCP in %d files, %d results (%d failed); servers: %s",
		len(transcripts), allCalls, mcpCalls, filesWith, mcpResults, mcpFailures, strings.Join(names, " "))

	if mcpCalls == 0 {
		t.Skip("no MCP calls in any transcript here — nothing to check the split against")
	}
	// A call whose result never pairs is a call the notebook renders with
	// no answer under it. Some legitimately have none — the last call of
	// an interrupted turn — but most must.
	if mcpResults*2 < mcpCalls {
		t.Errorf("only %d of %d MCP calls found a result — the pairing is wrong", mcpResults, mcpCalls)
	}
}

func quoteJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
