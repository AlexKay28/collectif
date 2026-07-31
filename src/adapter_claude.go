package main

// adapter_claude.go — #46 Phase 1: Claude Code implementation of CLIAdapter.
//
// This file owns everything Claude-Code-specific that used to be scattered
// across pty.go / settings_gen.go / transcript.go / harness.go. The behaviour
// is unchanged — call-sites now go through the interface instead of naming
// Claude directly.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// claudeAdapter is the singleton implementation for the `claude` CLI.
// Stateless — safe to share across sessions.
type claudeAdapter struct{}

func init() {
	registerAdapter(&claudeAdapter{})
}

func (a *claudeAdapter) Name() string { return "claude" }

// Version shells out to `claude --version`. Best-effort: any error (missing
// binary, non-zero exit) returns "" + nil so callers can render "unknown"
// rather than failing the request.
func (a *claudeAdapter) Version() (string, error) {
	out, err := exec.Command("claude", "--version").Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// Capabilities — Claude Code is the reference implementation; everything on.
func (a *claudeAdapter) Capabilities() Capabilities {
	return Capabilities{
		Hooks:                true,
		StructuredTranscript: true,
		ToolCallEvents:       true,
		SubagentFiles:        true,
		PreCompact:           true,
		SessionIDPinning:     true,
	}
}

// Spawn builds the exec.Cmd for `claude`, writing the hook settings file
// under a scratch dir and returning a cleanup that removes it. Mirrors the
// pre-#46 spawnClaude behaviour exactly; the caller (pty.go) is now
// responsible for actually starting the process and wiring the PTY.
func (a *claudeAdapter) Spawn(req SpawnRequest) (*exec.Cmd, func(), error) {
	settingsDir, settingsFile, werr := writeHookSettings(req.HookURL)
	if werr != nil {
		return nil, func() {}, werr
	}
	cleanup := func() {
		if settingsDir != "" {
			_ = os.RemoveAll(settingsDir)
		}
	}

	args := []string{"--session-id", req.SessionID, "--settings", settingsFile}
	if req.Prompt != "" {
		args = append(args, req.Prompt)
	}
	cmd := exec.Command("claude", args...)
	cmd.Dir = req.Cwd
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"AGENTCTL_AGENT_ID="+req.AgentID,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd, cleanup, nil
}

// TranscriptPath returns Claude Code's well-known transcript location:
// ~/.claude/projects/<hashed-cwd>/<sessionID>.jsonl. In practice the hook
// payload also carries `transcript_path`, so live sessions never need this
// computed path — but adapters that don't send a hook (Phase 2) will.
//
// Returns ("", false) if the home directory can't be resolved.
func (a *claudeAdapter) TranscriptPath(sessionID, cwd string) (string, bool) {
	if sessionID == "" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", false
	}
	// Claude Code encodes cwd by replacing path separators with '-'. This
	// matches the observed on-disk layout; if the encoding ever drifts we
	// still degrade gracefully because the live path is delivered via the
	// hook payload and takes precedence.
	hashed := strings.ReplaceAll(strings.TrimPrefix(cwd, "/"), "/", "-")
	if hashed == "" {
		hashed = "root"
	}
	return filepath.Join(home, ".claude", "projects", hashed, sessionID+".jsonl"), true
}

// ParseTranscriptLine wraps the existing extractUsageAndChars + extractModel
// helpers in the CLI-agnostic TranscriptEvent shape. Kept as thin as
// possible so the existing test coverage of those helpers still applies.
func (a *claudeAdapter) ParseTranscriptLine(raw []byte) (TranscriptEvent, error) {
	in, out, cr, cc, thinkCh, textCh, toolCh, ok := extractUsageAndChars(raw)
	if !ok {
		return TranscriptEvent{}, nil
	}
	return TranscriptEvent{
		Model:               extractModel(raw),
		InputTokens:         uint64(in),
		OutputTokens:        uint64(out),
		CacheReadTokens:     uint64(cr),
		CacheCreationTokens: uint64(cc),
		ThinkingChars:       thinkCh,
		TextChars:           textCh,
		ToolChars:           toolCh,
		HasUsage:            true,
	}, nil
}

// ModelContextLimit delegates to the existing contextLimitFor map. Kept in
// harness.go so the model list stays discoverable next to the pressure
// logic that consumes it; the adapter is just the routing layer.
func (a *claudeAdapter) ModelContextLimit(model string) int {
	return contextLimitFor(model)
}
