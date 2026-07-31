package main

// adapter_opencode_test.go — #46 Phase 2 test coverage for opencodeAdapter.
//
// Two flavors of test:
//
//   - Pure-Go assertions on the adapter's contract (Name, Capabilities,
//     TranscriptPath sentinel, ParseTranscriptLine no-op, ModelContextLimit
//     prefix table). These run in every CI environment.
//
//   - Live-spawn smoke test guarded by exec.LookPath("opencode") — skipped
//     when the binary isn't installed. Mirrors the pattern used for the
//     Claude adapter in api_test.go so CI without opencode still passes.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
)

// TestSmoke_OpenCodeAdapterRegistered — mandated smoke test from the Phase 2
// spec. Any registry regression that silently drops the opencode init()
// registration fails here before dependent tests need to run.
func TestSmoke_OpenCodeAdapterRegistered(t *testing.T) {
	if getAdapter("opencode") == nil {
		t.Fatal("opencode not registered")
	}
}

// TestOpenCodeAdapterName — the wire-facing identifier must be exactly
// "opencode". POST /api/agents relies on this string as the registry key,
// so a typo (OpenCode / open-code / opencode-ai) would silently break the
// API's `cli` field.
func TestOpenCodeAdapterName(t *testing.T) {
	a := getAdapter("opencode")
	if a == nil {
		t.Fatalf("no opencode adapter registered")
	}
	if a.Name() != "opencode" {
		t.Errorf("Name()=%q, want %q", a.Name(), "opencode")
	}
}

// TestOpenCodeAdapterCapabilities — assert every capability bit explicitly
// so any future flip surfaces here. The Phase 2 policy is "capabilities are
// truthful about what OpenCode currently exposes"; today that's all-false.
// Flipping a bit on without wiring the corresponding behaviour would fail
// this test and force a docs/adapter update in the same change.
func TestOpenCodeAdapterCapabilities(t *testing.T) {
	a := getAdapter("opencode")
	if a == nil {
		t.Fatalf("no opencode adapter registered")
	}
	c := a.Capabilities()
	if c.Hooks {
		t.Errorf("Hooks=true, want false")
	}
	if c.StructuredTranscript {
		t.Errorf("StructuredTranscript=true, want false")
	}
	if c.ToolCallEvents {
		t.Errorf("ToolCallEvents=true, want false")
	}
	if c.SubagentFiles {
		t.Errorf("SubagentFiles=true, want false")
	}
	if c.PreCompact {
		t.Errorf("PreCompact=true, want false")
	}
	if c.SessionIDPinning {
		t.Errorf("SessionIDPinning=true, want false")
	}
}

// TestOpenCodeAdapterTranscriptPathAlwaysUnknown — the adapter can't compute
// a reliable transcript path from (sessionID, cwd) because OpenCode doesn't
// publish a stable well-known location. Every input must return ("", false)
// so the watcher noops instead of tailing a bogus path.
func TestOpenCodeAdapterTranscriptPathAlwaysUnknown(t *testing.T) {
	a := getAdapter("opencode")
	if a == nil {
		t.Fatalf("no opencode adapter")
	}
	cases := []struct {
		sid, cwd string
	}{
		{"", ""},
		{"", "/tmp/abc"},
		{"sid-1", "/tmp/abc"},
		{"sid-2", ""},
	}
	for _, tc := range cases {
		p, ok := a.TranscriptPath(tc.sid, tc.cwd)
		if ok || p != "" {
			t.Errorf("TranscriptPath(%q,%q)=(%q,%v), want (%q,%v)",
				tc.sid, tc.cwd, p, ok, "", false)
		}
	}
}

// TestOpenCodeAdapterParseTranscriptLineNoUsage — the parser is a well-behaved
// no-op. Anything fed in returns HasUsage=false and no error so the watcher
// treats it as "skip this line" (mirrors the non-usage branch in the Claude
// adapter). We assert both a realistic-looking JSON payload and empty input
// so any future field-populating logic can be spotted by this test flipping.
func TestOpenCodeAdapterParseTranscriptLineNoUsage(t *testing.T) {
	a := getAdapter("opencode")
	inputs := [][]byte{
		nil,
		[]byte(""),
		[]byte(`{"role":"assistant","content":"hi"}`),
		[]byte(`{"usage":{"input_tokens":10,"output_tokens":5}}`),
		[]byte(`not-json-at-all`),
	}
	for _, in := range inputs {
		ev, err := a.ParseTranscriptLine(in)
		if err != nil {
			t.Errorf("ParseTranscriptLine(%q) err=%v; want nil", string(in), err)
		}
		if ev.HasUsage {
			t.Errorf("ParseTranscriptLine(%q) HasUsage=true; want false — the adapter is a stub", string(in))
		}
		if ev.Model != "" || ev.InputTokens != 0 || ev.OutputTokens != 0 {
			t.Errorf("ParseTranscriptLine(%q) leaked fields: %+v", string(in), ev)
		}
	}
}

// TestOpenCodeAdapterModelContextLimit — sanity check the prefix table
// covers each provider family we advertise support for, plus the empty and
// unknown fallbacks. The exact numbers are documented in the adapter
// source; if a provider ships a wildly different window this test should
// be updated alongside the map.
func TestOpenCodeAdapterModelContextLimit(t *testing.T) {
	a := getAdapter("opencode")
	if a == nil {
		t.Fatalf("no opencode adapter")
	}
	cases := []struct {
		model string
		want  int
	}{
		{"", defaultContextLimit},
		{"unknown-model-xyz", defaultContextLimit},
		{"claude-opus-4-7-20260115", 200000},
		{"claude-sonnet-4-1", 200000},
		{"gpt-5-mini-2025", 400000},
		{"gpt-4o-2024-11", 128000},
		{"gpt-4.1-turbo", 1000000},
		{"o1-preview", 200000},
		{"o3-mini", 200000},
		{"gemini-2.5-pro", 1000000},
		{"gemini-1.5-flash", 1000000},
		{"deepseek-coder", 128000},
	}
	for _, tc := range cases {
		if got := a.ModelContextLimit(tc.model); got != tc.want {
			t.Errorf("ModelContextLimit(%q)=%d, want %d", tc.model, got, tc.want)
		}
	}
}

// TestOpenCodeAdapterModelContextLimitNonZero — the interface contract in
// cli.go promises a non-zero result so the pressure gauge always renders.
// Assert the fallback path holds that promise even for empty/garbage input.
func TestOpenCodeAdapterModelContextLimitNonZero(t *testing.T) {
	a := getAdapter("opencode")
	for _, m := range []string{"", "garbage", "  ", "some-future-model"} {
		if got := a.ModelContextLimit(m); got <= 0 {
			t.Errorf("ModelContextLimit(%q)=%d, want > 0 (interface contract)", m, got)
		}
	}
}

// TestOpenCodeAdapterSpawnCleanupNonNil — the interface promises cleanup is
// always non-nil so callers can invoke it unconditionally. The OpenCode
// adapter writes no settings file so cleanup is a no-op sentinel; assert
// it's still safe to call. Uses a dummy tempdir as cwd so exec.Command
// doesn't complain about a missing directory when the caller Starts it.
func TestOpenCodeAdapterSpawnCleanupNonNil(t *testing.T) {
	a := getAdapter("opencode")
	if a == nil {
		t.Fatalf("no opencode adapter")
	}
	dir := t.TempDir()
	cmd, cleanup, err := a.Spawn(SpawnRequest{
		SessionID: "sid",
		Cwd:       dir,
		Prompt:    "hello",
		HookURL:   "http://127.0.0.1:0/api/hooks?ht=x",
		AgentID:   "agent-abc",
	})
	if err != nil {
		t.Fatalf("Spawn err=%v", err)
	}
	if cmd == nil {
		t.Fatalf("Spawn returned nil cmd")
	}
	if cleanup == nil {
		t.Fatalf("Spawn returned nil cleanup — interface promises non-nil")
	}
	// Should be safe to call even without starting the process.
	cleanup()
	// The env should carry the agent-id marker so a child that shells back
	// into collectif can be attributed. Spot-check without asserting order.
	var haveAgentID bool
	for _, e := range cmd.Env {
		if e == "AGENTCTL_AGENT_ID=agent-abc" {
			haveAgentID = true
			break
		}
	}
	if !haveAgentID {
		t.Errorf("AGENTCTL_AGENT_ID not exported in cmd.Env")
	}
	if cmd.Dir != dir {
		t.Errorf("cmd.Dir=%q, want %q", cmd.Dir, dir)
	}
}

// TestAPI_AcceptsOpenCodeCLI — end-to-end route check: POST /api/agents with
// {"cli":"opencode"} must NOT be rejected with 400 unknown-cli. If opencode
// isn't installed the spawn itself will fail (500), which is fine — we're
// only asserting the routing / registry lookup path recognises the adapter.
// Mirrors TestSpawnAgentDefaultsToClaude's split-on-status pattern.
func TestAPI_AcceptsOpenCodeCLI(t *testing.T) {
	dir := t.TempDir()
	body, _ := json.Marshal(spawnReq{Cwd: dir, CLI: "opencode"})
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)

	// The only forbidden outcome is a 400 with "unknown cli" — that would
	// mean the registry didn't see the adapter.
	if rec.Code == http.StatusBadRequest &&
		bytes.Contains(rec.Body.Bytes(), []byte("unknown cli")) {
		t.Fatalf("opencode should be registered; got 400 unknown-cli: %s", rec.Body.String())
	}

	// If spawn succeeded, tidy up the session so we don't leak into later
	// tests. On failure (opencode not installed) removeSession has already
	// run inside the handler.
	if rec.Code == http.StatusOK {
		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err == nil {
			if id := resp["agentID"]; id != "" {
				t.Cleanup(func() { removeSession(id) })
				if s := getSession(id); s != nil {
					if s.CLI != "opencode" {
						t.Errorf("session CLI=%q, want opencode", s.CLI)
					}
				}
			}
		}
	}
}

// TestOpenCodeAdapterLiveSpawn — live smoke test guarded on the presence of
// the `opencode` binary. When available, we exercise the full spawn path so
// any breakage in the argv shape / env plumbing surfaces here. Skipped
// (rather than failed) in CI without opencode installed, matching the
// Claude live-spawn convention.
func TestOpenCodeAdapterLiveSpawn(t *testing.T) {
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode CLI not on PATH; skipping live spawn")
	}
	// Only assert that Spawn returns a well-formed cmd we could Start.
	// Actually starting it would consume real API tokens; that's the
	// wrapping api_test end-to-end coverage's job, not this test's.
	a := getAdapter("opencode")
	dir := t.TempDir()
	cmd, cleanup, err := a.Spawn(SpawnRequest{
		SessionID: "live-sid",
		Cwd:       dir,
		AgentID:   "live-agent",
	})
	defer cleanup()
	if err != nil {
		t.Fatalf("live Spawn err=%v", err)
	}
	if cmd == nil || cmd.Path == "" {
		t.Fatalf("live Spawn returned degenerate cmd: %+v", cmd)
	}
}
