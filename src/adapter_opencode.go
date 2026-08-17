package main

// adapter_opencode.go — #46 Phase 2: OpenCode CLI implementation of CLIAdapter.
//
// OpenCode (https://opencode.ai, source https://github.com/sst/opencode) is a
// TypeScript-based, model-agnostic coding-agent CLI by SST. As of the writing
// of this adapter the binary is `opencode`; the tool is open-source but does
// not expose the same set of integration surfaces Claude Code does. Concretely:
//
//   - No documented HTTP-hook mechanism the way Claude Code has (SessionStart,
//     PreToolUse, PostToolUse, …). Some builds ship an event/log bus but the
//     shape is not stable enough to commit to a parser here.
//   - No documented well-known transcript JSONL path we can rely on across
//     versions. Session state lives under ~/.local/share/opencode/ but the
//     layout has churned; parsing it defensively is a follow-up.
//   - No subagent file convention (.claude/agents/*.md has no counterpart).
//   - Model-agnostic (Anthropic / OpenAI / etc.) — the model-name → context
//     window map has to cover both provider families.
//
// So this adapter reports Capabilities honestly (almost everything false) and
// returns graceful sentinels from TranscriptPath / ParseTranscriptLine. The
// UI already degrades unavailable panels for CLIs that don't expose a signal;
// this adapter's job is just to route the PTY through the same lifecycle so
// an OpenCode session at least *runs* inside collectif with a terminal panel,
// activity log via the process lifecycle, and a fair token accounting UX that
// reads "not available for OpenCode sessions" for anything hook-shaped.
//
// The moment OpenCode grows a stable hook config or JSONL transcript, flip the
// capability bit and fill in Spawn / ParseTranscriptLine — the seam is here.

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// opencodeAdapter is the singleton implementation for the `opencode` CLI.
// Stateless — safe to share across sessions.
type opencodeAdapter struct{}

func init() {
	registerAdapter(&opencodeAdapter{})
}

func (a *opencodeAdapter) Name() string { return "opencode" }

// Version shells out to `opencode --version`. Best-effort: any error (missing
// binary, non-zero exit) returns "" + nil so the UI can render "unknown"
// instead of failing the request. Mirrors claudeAdapter.Version.
func (a *opencodeAdapter) Version() (string, error) {
	out, err := exec.Command("opencode", "--version").Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Capabilities — deliberately conservative. Anything not proven here reports
// false so the UI hides the corresponding panel rather than rendering an empty
// or broken one.
//
//   - Hooks:                false — no stable HTTP-hook config format.
//   - StructuredTranscript: false — no documented JSONL location/shape.
//   - ToolCallEvents:       false — follows from no hooks.
//   - SubagentFiles:        false — no .opencode/agents/*.md convention.
//   - PreCompact:           false — no equivalent event.
//   - SessionIDPinning:     false — no documented `--session-id` equivalent
//                                    that survives a restart, so we don't
//                                    claim reconnect will work.
//
// Flip bits back on as OpenCode stabilises the corresponding surface.
func (a *opencodeAdapter) Capabilities() Capabilities {
	return Capabilities{
		Hooks:                false,
		StructuredTranscript: false,
		ToolCallEvents:       false,
		SubagentFiles:        false,
		PreCompact:           false,
		SessionIDPinning:     false,
	}
}

// Spawn builds the exec.Cmd for `opencode`. Since we don't have a hook
// mechanism to configure, there's no settings file to write and the cleanup
// is a no-op (but still non-nil per the interface contract).
//
// Behaviour:
//   - Invokes `opencode` (no args) in the requested cwd. When a prompt is
//     provided we append it as a positional argument, mirroring the Claude
//     adapter's shape; OpenCode's CLI accepts a trailing "run" argument for
//     non-interactive prompts in most builds. If a given build rejects the
//     arg, users see the error in the terminal panel — same failure surface
//     as any other spawn error.
//   - AGENTCTL_AGENT_ID is exported so any child process (or wrapping shell)
//     that shells back into collectif can be attributed.
//   - HookURL is *not* baked in — OpenCode has no place to accept it. The
//     env var is still exported (AGENTCTL_HOOK_URL) as a soft signal for a
//     future OpenCode plugin that might pick it up.
//   - Setsid puts the child in its own process group so the DELETE handler
//     in api.go can signal the whole group. Matches claudeAdapter.
func (a *opencodeAdapter) Spawn(req SpawnRequest) (*exec.Cmd, func(), error) {
	// No settings file to write — cleanup is a no-op sentinel. Keeping it
	// non-nil so callers can invoke unconditionally per the interface.
	cleanup := func() {}

	var args []string
	if req.Prompt != "" {
		// OpenCode's CLI has taken several shapes over versions. Passing the
		// prompt as a trailing positional arg is the most widely-supported
		// invocation. If a build rejects it, the user sees the error in the
		// terminal panel and can restart without a prompt.
		args = append(args, req.Prompt)
	}
	cmd := exec.Command("opencode", args...)
	cmd.Dir = req.Cwd
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"AGENTCTL_AGENT_ID="+req.AgentID,
		// Soft signal for a future OpenCode plugin; ignored today.
		"AGENTCTL_HOOK_URL="+req.HookURL,
		"AGENTCTL_SESSION_ID="+req.SessionID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cleanup, nil
}

// TranscriptPath returns ("", false) — OpenCode does not publish a stable
// well-known transcript path we can compute from (sessionID, cwd) alone.
// The transcript watcher tolerates ok=false by simply not starting; the UI
// will show token counts as 0 for OpenCode sessions, which is honest.
func (a *opencodeAdapter) TranscriptPath(sessionID, cwd string) (string, bool) {
	return "", false
}

// ParseTranscriptLine returns a zero-value TranscriptEvent with HasUsage=false
// unconditionally. The transcript watcher (transcript.go) treats HasUsage=false
// as "skip this line" and won't spam logs. Keeping this as a well-behaved
// no-op means if a future caller mistakenly feeds OpenCode lines through the
// pipeline (say, from a hand-written test), we degrade cleanly instead of
// error-out.
func (a *opencodeAdapter) ParseTranscriptLine(raw []byte) (TranscriptEvent, error) {
	return TranscriptEvent{HasUsage: false}, nil
}

// opencodeContextLimits covers the model families OpenCode routinely proxies
// to today. Prefix-match against the model string emitted by whichever
// provider the session is configured for. Falls back to defaultContextLimit
// (200k) so the pressure gauge still renders — same policy as the Claude
// adapter for unknown model ids.
var opencodeContextLimits = []struct {
	Prefix string
	Limit  int
}{
	// The Anthropic (Claude) family is NOT listed here. It resolves from
	// claudeModels in adapter_claude.go — see ModelContextLimit below.
	// A second copy of those numbers is what #48 was: this table said 200k
	// for models with a 1M window, so an opencode session proxying Claude
	// had exactly the same 5x-high pressure gauge as the Claude adapter.

	// OpenAI GPT family — approximate widely-published context windows.
	// Numbers are the *input* context window; the pressure gauge only
	// compares total-context vs limit, which is the right comparison here.
	{"gpt-5", 400000},
	{"gpt-4o", 128000},
	{"gpt-4.1", 1000000},
	{"gpt-4-turbo", 128000},
	{"gpt-4", 128000},
	{"o1", 200000},
	{"o3", 200000},
	{"o4", 200000},

	// Google Gemini — commonly routed through OpenCode's provider layer.
	{"gemini-2.5", 1000000},
	{"gemini-2.0", 1000000},
	{"gemini-1.5", 1000000},

	// DeepSeek and other OSS models — conservative default.
	{"deepseek", 128000},
}

// ModelContextLimit maps a model id to its context-window size in tokens.
// Prefix-matches so version suffixes (dates, revisions) don't have to be
// enumerated. Unknown models fall back to defaultContextLimit rather than
// zero so the pressure gauge always renders — the interface contract in
// cli.go promises non-zero.
func (a *opencodeAdapter) ModelContextLimit(model string) int {
	if model == "" {
		return defaultContextLimit
	}
	// Claude ids resolve from the shared catalog so there is exactly one
	// place to correct when Anthropic ships a new window (#48).
	if m, ok := lookupModel(claudeModels, model); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	for _, m := range opencodeContextLimits {
		if strings.HasPrefix(model, m.Prefix) {
			return m.Limit
		}
	}
	return defaultContextLimit
}

// ProjectTranscriptLine is not implemented for opencode. See the note on
// the codex adapter — declaring the gap is the design (ADR 0002 D11), not
// a placeholder to be filled with a scraper.
func (a *opencodeAdapter) ProjectTranscriptLine(raw []byte) ([]TranscriptPart, error) {
	return nil, nil
}
