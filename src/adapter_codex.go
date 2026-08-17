package main

// adapter_codex.go — #46 Phase 2: OpenAI Codex CLI implementation of
// CLIAdapter.
//
// Codex CLI (github.com/openai/codex) is the highest-demand peer to Claude
// Code in the parity matrix. It ships an extensive hook system that already
// mirrors Claude Code's event names — PreToolUse/PostToolUse/SessionStart/
// SessionEnd/UserPromptSubmit/Stop/PreCompact/PostCompact/PermissionRequest/
// SubagentStart/SubagentStop — and a rich JSONL transcript (called a
// "rollout") persisted under $CODEX_HOME/sessions/YYYY/MM/DD/. Where Codex
// diverges from Claude Code we degrade capabilities honestly rather than
// pretending things work.
//
// Notable divergences vs Claude Code:
//   - Hook handlers are shell COMMANDS (JSON on stdin), not HTTP endpoints.
//     We bridge this with a tiny shim script generated in the settings dir
//     that curl-POSTs stdin to our /api/hooks URL. Payload shape overlaps
//     Claude's enough that hooks.go's decoder tolerates it (unknown fields
//     ignored, shared field names for session_id / hook_event_name /
//     tool_name / tool_input / tool_response / transcript_path / cwd).
//   - There is no client-provided session id. Codex allocates a fresh
//     timestamped UUID per invocation, so Capabilities.SessionIDPinning is
//     false. The hook payload's session_id and the rollout filename remain
//     the source of truth for identifying a live session.
//   - The rollout path incorporates a wall-clock timestamp that we don't
//     know at spawn time, so TranscriptPath returns ("", false). The live
//     path is instead delivered via the hook payload's transcript_path
//     field, which hooks.go already stashes on s.TranscriptPath.
//   - ParseTranscriptLine is a stub returning HasUsage=false. The rollout
//     line schema is rich but not yet a stable shape we've exercised end
//     to end; over-claiming StructuredTranscript would silently inflate
//     bogus token totals. Set Capabilities.StructuredTranscript = false
//     until a follow-up implements the parser against real fixtures.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// codexAdapter is the singleton implementation for the `codex` CLI.
// Stateless — safe to share across sessions.
type codexAdapter struct{}

func init() {
	registerAdapter(&codexAdapter{})
}

func (a *codexAdapter) Name() string { return "codex" }

// Version shells out to `codex --version`. Best-effort: any error (missing
// binary, non-zero exit) returns "" + nil so callers can render "unknown"
// rather than failing the request. Mirrors claudeAdapter.Version.
func (a *codexAdapter) Version() (string, error) {
	out, err := exec.Command("codex", "--version").Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Capabilities — Codex exposes a rich hook system that mirrors Claude Code's
// event set, plus a JSONL rollout. Bits are asserted individually:
//
//   Hooks                = true   // command-based, bridged via shim script
//   StructuredTranscript = false  // parser not yet implemented against real
//                                 // rollout fixtures; setting true without a
//                                 // parser would inflate token totals with
//                                 // zeros. Flip once ParseTranscriptLine
//                                 // handles Codex's RolloutLine shape.
//   ToolCallEvents       = true   // PreToolUse/PostToolUse hooks fire
//   SubagentFiles        = false  // Codex has no .claude/agents equivalent
//   PreCompact           = true   // both PreCompact and PostCompact exist
//   SessionIDPinning     = false  // Codex allocates session IDs server-side
func (a *codexAdapter) Capabilities() Capabilities {
	return Capabilities{
		Hooks:                true,
		StructuredTranscript: false,
		ToolCallEvents:       true,
		SubagentFiles:        false,
		PreCompact:           true,
		SessionIDPinning:     false,
	}
}

// Spawn builds the exec.Cmd for `codex`. Codex's interactive TUI is invoked
// as `codex [PROMPT]` (no subcommand). We inject hooks via a scratch
// $CODEX_HOME containing a config.toml with our HTTP-bridging shim script
// wired to every event. --dangerously-bypass-hook-trust skips the trust
// prompt that would otherwise block the TUI on first run.
//
// cleanup removes the scratch CODEX_HOME dir on session teardown.
func (a *codexAdapter) Spawn(req SpawnRequest) (*exec.Cmd, func(), error) {
	codexHome, cleanup, werr := writeCodexHome(req.HookURL)
	if werr != nil {
		return nil, func() {}, werr
	}

	args := []string{
		// Skip persisted hook trust for this invocation — our shim script
		// is regenerated per session, so the trust prompt would fire every
		// time. DANGEROUS in general; safe here because we own the shim.
		"--dangerously-bypass-hook-trust",
	}
	if req.Prompt != "" {
		args = append(args, req.Prompt)
	}
	cmd := exec.Command("codex", args...)
	cmd.Dir = req.Cwd
	// CODEX_HOME points at our scratch dir; auth still uses the real home
	// via the codex login flow — advanced users can override with a link.
	cmd.Env = append(inheritableEnv(),
		"TERM=xterm-256color",
		"AGENTCTL_AGENT_ID="+req.AgentID,
		"CODEX_HOME="+codexHome,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cleanup, nil
}

// TranscriptPath — Codex writes rollouts to
// $CODEX_HOME/sessions/YYYY/MM/DD/rollout-<ts>-<uuid>.jsonl where <ts> and
// <uuid> are chosen at session-start time inside the binary. We can't
// compute that path from just (sessionID, cwd), so we always return
// ok=false. The live path arrives via the hook payload's transcript_path
// field and hooks.go stashes it on s.TranscriptPath before starting the
// watcher.
func (a *codexAdapter) TranscriptPath(sessionID, cwd string) (string, bool) {
	return "", false
}

// ParseTranscriptLine — stub returning HasUsage=false. Codex's rollout
// schema (RolloutLine wrapping RolloutItem enums) is richer than Claude's
// and needs its own parser against real fixtures before we can claim
// token accuracy. Returning HasUsage=false keeps the transcript watcher
// silently skipping every line rather than accumulating zeros. Paired
// with Capabilities.StructuredTranscript = false so the UI doesn't
// pretend the numbers are real.
func (a *codexAdapter) ParseTranscriptLine(raw []byte) (TranscriptEvent, error) {
	return TranscriptEvent{}, nil
}

// ModelContextLimit — Codex supports OpenAI models (GPT-5 family in
// current builds, GPT-4-family for older configs). The Codex model
// catalog is fetched dynamically at runtime, so we can't guarantee an
// exhaustive list; this is a best-guess prefix table with a safe default.
// Unknown models fall back to defaultContextLimit so the pressure gauge
// still renders.
func (a *codexAdapter) ModelContextLimit(model string) int {
	if model == "" {
		return defaultContextLimit
	}
	// Prefix order matters: longer/more-specific first would matter if
	// entries could collide, but current entries are disjoint.
	table := []struct {
		Prefix string
		Limit  int
	}{
		// GPT-5 family: 400k context per Codex's model_info tests.
		{"gpt-5", 400000},
		// o-series reasoning models: 200k context.
		{"o4", 200000},
		{"o3", 200000},
		{"o1", 200000},
		// GPT-4.1 family: 1M context.
		{"gpt-4.1", 1000000},
		// GPT-4o family: 128k context.
		{"gpt-4o", 128000},
		// Older GPT-4 turbo/8k baselines.
		{"gpt-4-turbo", 128000},
		{"gpt-4", 128000},
	}
	for _, m := range table {
		if strings.HasPrefix(model, m.Prefix) {
			return m.Limit
		}
	}
	return defaultContextLimit
}

// writeCodexHome creates a scratch $CODEX_HOME directory with a
// config.toml that wires every supported hook event to a shim shell
// script. The shim reads stdin (the JSON hook payload) and POSTs it to
// our /api/hooks URL. Returns the directory path plus a cleanup function
// that removes it on session teardown.
//
// Cleanup is always non-nil (a no-op if we never made the dir), matching
// the CLIAdapter.Spawn contract.
func writeCodexHome(hookURL string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "collectif-codex-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() {
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
	}

	// Shim script: read stdin, POST to hookURL. --max-time bounds the
	// worst-case impact of a hung endpoint; --silent keeps the terminal
	// clean. Codex ignores hook stdout unless a control payload is
	// returned, so we drop it (`>/dev/null`).
	shimPath := filepath.Join(dir, "hook.sh")
	shim := "#!/bin/sh\n" +
		"exec curl --silent --show-error --max-time 5 " +
		"-H 'Content-Type: application/json' " +
		"--data-binary @- " +
		shellQuote(hookURL) + " >/dev/null 2>&1\n"
	if err := os.WriteFile(shimPath, []byte(shim), 0o755); err != nil {
		cleanup()
		return "", func() {}, err
	}

	// Every event routes to the same shim; server-side hooks.go
	// discriminates on the hook_event_name field of the JSON payload.
	// Matcher "*" catches everything, matching what settings_gen.go does
	// for Claude Code.
	//
	// Note: PermissionRequest (Codex) does not have a Claude equivalent
	// on hooks.go's switch, so it falls through to the default arm as
	// an unknown event — still recorded in the activity log via
	// appendActivity, just not classified. That's acceptable graceful
	// degradation until hooks.go grows a Codex-aware branch.
	events := []string{
		"PreToolUse",
		"PostToolUse",
		"PermissionRequest",
		"PreCompact",
		"PostCompact",
		"SessionStart",
		"SessionEnd",
		"UserPromptSubmit",
		"SubagentStart",
		"SubagentStop",
		"Stop",
	}
	var toml strings.Builder
	toml.WriteString("# Generated by collectif — per-session CODEX_HOME. Safe to delete.\n")
	toml.WriteString("[hooks]\n\n")
	for _, ev := range events {
		fmt.Fprintf(&toml, "[[hooks.%s]]\n", ev)
		toml.WriteString("matcher = \"*\"\n\n")
		fmt.Fprintf(&toml, "[[hooks.%s.hooks]]\n", ev)
		toml.WriteString("type = \"command\"\n")
		fmt.Fprintf(&toml, "command = %s\n", tomlQuote(shimPath))
		toml.WriteString("timeout = 5\n\n")
	}

	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(toml.String()), 0o600); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return dir, cleanup, nil
}

// shellQuote wraps s in single quotes for POSIX shells, escaping any
// embedded single quotes. Small helper to keep the shim script safe
// against pathological hookURL values.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// tomlQuote returns a TOML basic-string literal for s. TOML basic
// strings support \" and \\ escapes; other characters we care about (the
// shim path is always a filesystem path we just created) don't need
// special handling.
func tomlQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// ProjectTranscriptLine has no implementation for codex yet: its rollout
// files are a different schema, and ADR 0002 D11 says an adapter with
// nothing to give says so rather than guessing. A codex session therefore
// renders as status and counters with a note naming what is missing —
// which is the honest view, and the one that makes adding the parser a
// visible improvement rather than a silent one.
func (a *codexAdapter) ProjectTranscriptLine(raw []byte) ([]TranscriptPart, error) {
	return nil, nil
}
