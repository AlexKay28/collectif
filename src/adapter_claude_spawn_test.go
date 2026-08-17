package main

import (
	"strings"
	"testing"
)

// #47 P1 — found by spawning a real session.
//
// collectif inherits its environment from whatever launched it, and passes
// the whole thing to every CLI it spawns. When collectif is launched from
// inside a Claude Code session — which is exactly how it gets developed —
// the child inherits CLAUDE_CODE_CHILD_SESSION and quietly turns its own
// transcript off:
//
//	Transcript saving is off — inherited CLAUDE_CODE_CHILD_SESSION marker
//
// Before ADR 0002 that cost telemetry. Now it costs the entire notebook:
// no transcript, no projection, no document, and nothing anywhere says
// why. A session that looks fine and silently records nothing is the
// precise failure mode this milestone exists to stop shipping.
func TestClaudeSpawn_DoesNotInheritTheChildSessionMarker(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "the-parent-session")
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "/run/user/1000/cc-socks/1.sock")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "secret")

	cmd, cleanup, err := (&claudeAdapter{}).Spawn(SpawnRequest{
		SessionID: "sid", Cwd: t.TempDir(), Prompt: "hi", AgentID: "agent-1",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cleanup()

	for _, bad := range []string{
		"CLAUDE_CODE_CHILD_SESSION",
		"CLAUDE_CODE_SESSION_ID",
		"CLAUDE_CODE_MESSAGING_SOCKET",
		"CLAUDE_CODE_MESSAGING_TOKEN",
	} {
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, bad+"=") {
				t.Errorf("%s leaked into the spawned session (%q) — it belongs to the process that "+
					"launched collectif, not to the agent collectif is starting", bad, kv)
			}
		}
	}
}

// The variables collectif does set must survive the scrub, and so must the
// rest of the environment — PATH above all, or the CLI cannot even start.
func TestClaudeSpawn_KeepsTheEnvironmentItNeeds(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("COLLECTIF_TEST_MARKER", "kept")

	cmd, cleanup, err := (&claudeAdapter{}).Spawn(SpawnRequest{
		SessionID: "sid", Cwd: t.TempDir(), AgentID: "agent-2",
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	defer cleanup()

	env := strings.Join(cmd.Env, "\n")
	for _, want := range []string{"PATH=", "COLLECTIF_TEST_MARKER=kept", "AGENTCTL_AGENT_ID=agent-2", "TERM="} {
		if !strings.Contains(env, want) {
			t.Errorf("%s is missing from the spawned environment", want)
		}
	}
}

// Every adapter inherits collectif's environment, so every adapter carries
// the same hazard. The marker is Claude Code's, but a `codex` session that
// believes it is a child of a Claude session is not a state anyone has
// reasoned about — and the variables are meaningless to it either way.
func TestSpawn_NoAdapterPassesTheParentSessionIdentity(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "parent")

	for name, a := range adapters {
		cmd, cleanup, err := a.Spawn(SpawnRequest{
			SessionID: "sid", Cwd: t.TempDir(), Prompt: "hi", AgentID: "agent-x",
		})
		if err != nil {
			t.Errorf("%s: spawn: %v", name, err)
			continue
		}
		for _, kv := range cmd.Env {
			if strings.HasPrefix(kv, "CLAUDE_CODE_CHILD_SESSION=") || strings.HasPrefix(kv, "CLAUDE_CODE_SESSION_ID=") {
				t.Errorf("%s: leaked %q", name, kv)
			}
		}
		cleanup()
	}
}
