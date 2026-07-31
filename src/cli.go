package main

// cli.go — #46 Phase 1: CLIAdapter interface + registry.
//
// The CLIAdapter is the seam between collectif and any coding-agent CLI it
// spawns. One implementation per CLI (Claude Code today; Codex, Aider, etc.
// in later phases). Adapters are stateless — session-specific state stays on
// *Session — so the registry hands out singletons.
//
// Keep this interface narrow. Phase 2 will add methods only as concrete
// second-adapter needs justify them. Anything speculative belongs elsewhere.

import (
	"os/exec"
)

// Capabilities is a compile-time-flat description of which optional signals
// a CLI adapter exposes. The UI consults these to decide which panels to
// render for a session. Missing capabilities degrade gracefully rather than
// error out.
type Capabilities struct {
	Hooks                bool
	StructuredTranscript bool
	ToolCallEvents       bool
	SubagentFiles        bool
	PreCompact           bool
	SessionIDPinning     bool
}

// TranscriptEvent is the CLI-agnostic shape the transcript watcher receives
// after the adapter parses one JSONL/whatever line. Only the fields
// collectif actually uses today are here; extend as new shared signals
// emerge.
//
// Zero-valued fields mean "the line carried no such signal" — callers must
// tolerate this. `HasUsage` distinguishes a genuine zero-token line from a
// line with no usage at all (which the transcript walker should skip).
type TranscriptEvent struct {
	Model               string
	InputTokens         uint64
	OutputTokens        uint64
	CacheReadTokens     uint64
	CacheCreationTokens uint64
	ThinkingChars       uint64
	TextChars           uint64
	ToolChars           uint64
	// HasUsage is true iff the line carried a usage block. Consumers that
	// only care about token counts should skip lines where this is false.
	HasUsage bool
}

// SpawnRequest carries the per-session parameters an adapter needs to build
// the exec.Cmd. Deliberately small — the adapter owns the settings-file
// writing (if any) and returns a cleanup func for the caller to invoke on
// session teardown.
type SpawnRequest struct {
	SessionID string
	Cwd       string
	Prompt    string
	HookURL   string
	AgentID   string // AGENTCTL_AGENT_ID env var value
}

// CLIAdapter is implemented once per supported CLI. Adapters are stateless
// singletons registered via registerAdapter in an init() function.
type CLIAdapter interface {
	// Name is the stable string identifier used on the wire (Session.CLI,
	// POST /api/agents request field, registry lookup key).
	Name() string

	// Version shells out to the CLI (`<bin> --version`) as a best effort.
	// May return "" + nil if unknown; callers must tolerate an empty result.
	Version() (string, error)

	// Capabilities reports which optional signals this adapter exposes.
	Capabilities() Capabilities

	// Spawn builds a ready-to-Start exec.Cmd for the CLI. The adapter owns
	// any temp-file writing (e.g. hook settings) and returns a cleanup
	// func the caller invokes on session teardown. cleanup is always
	// non-nil (a no-op if the adapter had nothing to clean up).
	Spawn(req SpawnRequest) (cmd *exec.Cmd, cleanup func(), err error)

	// TranscriptPath returns the well-known path this CLI writes its
	// transcript to for the given session, if such a convention exists.
	// The second return is false if the adapter cannot compute a path
	// without additional input (e.g. Claude Code emits it via a hook).
	TranscriptPath(sessionID, cwd string) (string, bool)

	// ParseTranscriptLine turns one raw line from the transcript into a
	// TranscriptEvent. Adapters must be defensive: lines that aren't
	// usage-bearing should return HasUsage=false with no error.
	ParseTranscriptLine(raw []byte) (TranscriptEvent, error)

	// ModelContextLimit returns the context-window size in tokens for a
	// model id this CLI emits. Adapters return a sensible default when
	// the model is unknown; the UI relies on non-zero for the pressure
	// gauge to render.
	ModelContextLimit(model string) int
}

// adapters is the process-wide registry, keyed by adapter Name(). Populated
// from each adapter's init(). Reads are lock-free — the map is
// write-once-during-init and never mutated afterward.
var adapters = map[string]CLIAdapter{}

// defaultAdapterName is what a request/session without an explicit cli
// selection resolves to. Keeps backward compatibility with pre-#46
// clients that always spoke Claude.
const defaultAdapterName = "claude"

// registerAdapter is called from each adapter's init(). Not safe to call
// after main() starts.
func registerAdapter(a CLIAdapter) {
	adapters[a.Name()] = a
}

// getAdapter returns the adapter for name, or nil if unknown. An empty
// name resolves to the default ("claude") so old callers keep working.
func getAdapter(name string) CLIAdapter {
	if name == "" {
		name = defaultAdapterName
	}
	return adapters[name]
}
