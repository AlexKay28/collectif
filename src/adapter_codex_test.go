package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCodexAdapterName — stable string identifier used on the wire
// (Session.CLI, POST /api/agents `cli` field, registry lookup key).
func TestCodexAdapterName(t *testing.T) {
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter registered")
	}
	if a.Name() != "codex" {
		t.Errorf("Name()=%q, want %q", a.Name(), "codex")
	}
}

// TestSmoke_CodexAdapterRegistered — registry smoke test called out in
// the Phase 2 brief. Duplicates part of TestCodexAdapterName but kept
// separate so a future refactor of the name test can't accidentally
// drop the "is it wired at all?" check.
func TestSmoke_CodexAdapterRegistered(t *testing.T) {
	if getAdapter("codex") == nil {
		t.Fatal("codex not registered")
	}
}

// TestCodexAdapterCapabilities — asserts the exact bits Codex supports
// (see the doc comment on codexAdapter.Capabilities for intent). This
// test is intentionally strict: bits are contract, not preference. A
// future capability change should update the assertion deliberately.
func TestCodexAdapterCapabilities(t *testing.T) {
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter")
	}
	c := a.Capabilities()
	if !c.Hooks {
		t.Errorf("Hooks=false, want true (Codex has a rich hook system)")
	}
	if c.StructuredTranscript {
		t.Errorf("StructuredTranscript=true, want false (parser not yet implemented)")
	}
	if !c.ToolCallEvents {
		t.Errorf("ToolCallEvents=false, want true (PreToolUse/PostToolUse hooks)")
	}
	if c.SubagentFiles {
		t.Errorf("SubagentFiles=true, want false (Codex has no .claude/agents equivalent)")
	}
	if !c.PreCompact {
		t.Errorf("PreCompact=false, want true (Codex has PreCompact + PostCompact)")
	}
	if c.SessionIDPinning {
		t.Errorf("SessionIDPinning=true, want false (Codex allocates ids server-side)")
	}
}

// TestCodexAdapterTranscriptPath — Codex rollout paths include a wall-
// clock timestamp we don't know at spawn time, so the adapter always
// reports ok=false. The live path is delivered via the hook payload's
// transcript_path field, not this method.
func TestCodexAdapterTranscriptPath(t *testing.T) {
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter")
	}
	if _, ok := a.TranscriptPath("some-sid", "/tmp/xyz"); ok {
		t.Errorf("expected ok=false for codex (rollout path is not derivable from sessionID+cwd)")
	}
	if _, ok := a.TranscriptPath("", ""); ok {
		t.Errorf("expected ok=false for empty inputs too")
	}
}

// TestCodexAdapterParseTranscriptLine — stub returns HasUsage=false for
// any input. Paired with Capabilities.StructuredTranscript=false. Once
// a real parser lands, this test should be replaced with a fixture-
// based assertion of the parsed TranscriptEvent shape.
func TestCodexAdapterParseTranscriptLine(t *testing.T) {
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter")
	}
	// A realistic-looking rollout line. Whatever the shape, the current
	// stub must return HasUsage=false without erroring.
	line := []byte(`{"type":"turn","turn":{"model":"gpt-5","usage":{"input_tokens":100,"output_tokens":50}}}` + "\n")
	ev, err := a.ParseTranscriptLine(line)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ev.HasUsage {
		t.Errorf("HasUsage=true, want false (parser not implemented)")
	}
	// And garbage input still returns cleanly.
	ev2, err2 := a.ParseTranscriptLine([]byte("not json\n"))
	if err2 != nil {
		t.Errorf("stub should not error on garbage; got %v", err2)
	}
	if ev2.HasUsage {
		t.Errorf("garbage input: HasUsage=true, want false")
	}
}

// TestCodexAdapterModelContextLimit — spot-check the best-guess table
// plus the fallback for unknown / empty models. The exact values are
// documented in codexAdapter.ModelContextLimit; this test locks the
// mapping so accidental edits surface.
func TestCodexAdapterModelContextLimit(t *testing.T) {
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter")
	}
	cases := []struct {
		model string
		want  int
	}{
		{"gpt-5", 400000},
		{"gpt-5-codex", 400000},
		{"gpt-4o", 128000},
		{"gpt-4o-mini", 128000},
		{"gpt-4.1", 1000000},
		{"o3-mini", 200000},
		{"o1-preview", 200000},
		{"unknown-model-9000", defaultContextLimit},
		{"", defaultContextLimit},
	}
	for _, c := range cases {
		if got := a.ModelContextLimit(c.model); got != c.want {
			t.Errorf("ModelContextLimit(%q)=%d, want %d", c.model, got, c.want)
		}
	}
}

// TestCodexAdapterSpawnBuildsConfig — Spawn creates a scratch CODEX_HOME
// with a config.toml wiring every hook event to the shim script that
// POSTs stdin to our hook URL. Doesn't actually start `codex` — we only
// check the exec.Cmd shape and the generated files.
//
// Skipped when `codex` isn't on PATH so CI (which won't have the binary)
// stays green.
func TestCodexAdapterSpawnBuildsConfig(t *testing.T) {
	if _, err := exec.LookPath("codex"); err != nil {
		t.Skip("codex binary not installed; skipping spawn shape check")
	}
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter")
	}
	req := SpawnRequest{
		SessionID: "sid-1",
		Cwd:       t.TempDir(),
		Prompt:    "hello",
		HookURL:   "http://127.0.0.1:12345/api/hooks?ht=abc",
		AgentID:   "agent-1",
	}
	cmd, cleanup, err := a.Spawn(req)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer cleanup()

	if cmd == nil {
		t.Fatalf("cmd is nil")
	}
	if got := filepath.Base(cmd.Path); got != "codex" {
		t.Errorf("cmd.Path base=%q, want codex", got)
	}
	if cmd.Dir != req.Cwd {
		t.Errorf("cmd.Dir=%q, want %q", cmd.Dir, req.Cwd)
	}
	// Prompt should appear as the trailing positional arg.
	if len(cmd.Args) == 0 || cmd.Args[len(cmd.Args)-1] != "hello" {
		t.Errorf("expected trailing prompt arg, got %v", cmd.Args)
	}
	// --dangerously-bypass-hook-trust should be present so hooks fire
	// without a per-invocation trust prompt.
	found := false
	for _, arg := range cmd.Args {
		if arg == "--dangerously-bypass-hook-trust" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--dangerously-bypass-hook-trust not in args: %v", cmd.Args)
	}

	// Locate CODEX_HOME from env and verify config.toml exists with hook wiring.
	var codexHome string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			codexHome = strings.TrimPrefix(kv, "CODEX_HOME=")
			break
		}
	}
	if codexHome == "" {
		t.Fatalf("CODEX_HOME env var not set on cmd")
	}
	cfg, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	body := string(cfg)
	for _, event := range []string{
		"PreToolUse", "PostToolUse", "SessionStart", "SessionEnd",
		"UserPromptSubmit", "Stop", "PreCompact", "PostCompact",
		"PermissionRequest", "SubagentStart", "SubagentStop",
	} {
		if !strings.Contains(body, "[[hooks."+event+"]]") {
			t.Errorf("config.toml missing hook block for %s\nbody:\n%s", event, body)
		}
	}
	// The shim script exists and is executable, and points at hookURL.
	shim, err := os.ReadFile(filepath.Join(codexHome, "hook.sh"))
	if err != nil {
		t.Fatalf("read hook.sh: %v", err)
	}
	if !strings.Contains(string(shim), req.HookURL) {
		t.Errorf("shim doesn't reference hookURL %q; got:\n%s", req.HookURL, shim)
	}
}

// TestCodexAdapterWriteCodexHome — direct test of the config-writing
// helper. Independent of `codex` being installed since we're only
// checking the files we produce.
func TestCodexAdapterWriteCodexHome(t *testing.T) {
	hookURL := "http://example/api/hooks?ht=zzz"
	dir, cleanup, err := writeCodexHome(hookURL)
	if err != nil {
		t.Fatalf("writeCodexHome: %v", err)
	}
	defer cleanup()

	// Cleanup MUST be non-nil per the CLIAdapter.Spawn contract.
	if cleanup == nil {
		t.Fatalf("cleanup is nil; contract requires a non-nil (possibly no-op) func")
	}

	if _, err := os.Stat(filepath.Join(dir, "config.toml")); err != nil {
		t.Fatalf("config.toml missing: %v", err)
	}
	shimPath := filepath.Join(dir, "hook.sh")
	fi, err := os.Stat(shimPath)
	if err != nil {
		t.Fatalf("hook.sh missing: %v", err)
	}
	// Owner should have execute bit so codex can spawn it.
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("hook.sh not executable by owner (mode=%v)", fi.Mode())
	}
	body, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("read hook.sh: %v", err)
	}
	if !strings.Contains(string(body), hookURL) {
		t.Errorf("shim doesn't reference hookURL; body:\n%s", body)
	}
}

// TestSpawnAgentAcceptsCodex — #46 Phase 2 DoD: POST /api/agents with
// `cli:"codex"` must not 400 on the adapter-lookup check. Mirrors the
// negative TestSpawnAgentUnknownCLIReturns400 in api_test.go: we don't
// care whether spawn ultimately succeeds (codex binary may be absent
// in CI), we only care that routing accepted the request.
func TestSpawnAgentAcceptsCodex(t *testing.T) {
	dir := t.TempDir()
	body, _ := json.Marshal(spawnReq{Cwd: dir, CLI: "codex"})
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)

	// Accept either OK (spawn worked because codex is installed) or a
	// non-400 error (spawn failed later in the pipeline). What we MUST
	// NOT see is 400 "unknown cli" — that would mean the adapter
	// registry lookup rejected "codex".
	if rec.Code == http.StatusBadRequest &&
		strings.Contains(rec.Body.String(), "unknown cli") {
		t.Fatalf("codex adapter not resolved by handleAgents: %s", rec.Body.String())
	}
	// Best-effort cleanup: if we got an agentID back, remove it so we
	// don't leave a half-spawned session in the registry across tests.
	if rec.Code == http.StatusOK {
		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil {
			if id := resp["agentID"]; id != "" {
				t.Cleanup(func() { removeSession(id) })
			}
		}
	}
}

// TestCodexAdapterVersionMissingBinary — Version is best-effort and
// must not error when codex isn't installed. Only runs the assertion
// when the binary is absent, to avoid flakiness on developer machines
// that happen to have codex on PATH.
func TestCodexAdapterVersionMissingBinary(t *testing.T) {
	if _, err := exec.LookPath("codex"); err == nil {
		t.Skip("codex is installed locally; missing-binary path not testable here")
	}
	a := getAdapter("codex")
	if a == nil {
		t.Fatalf("no codex adapter")
	}
	v, err := a.Version()
	if err != nil {
		t.Errorf("Version returned err=%v, want nil (best-effort contract)", err)
	}
	if v != "" {
		t.Errorf("Version returned %q, want \"\" when binary missing", v)
	}
}
