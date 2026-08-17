package main

// provider.go — model metadata shared by every CLI adapter, and later by
// the native providers in M2 (#50).
//
// Introduced by #48. The context-pressure gauge previously read its limits
// from a package-level table in harness.go that nothing owned and nothing
// could verify; it had drifted to the point where every current model was
// reported as a 200k window, so a 1M-window session read five times high.
// Model metadata now belongs to whoever actually talks to the model.

import (
	"context"
	"strings"
)

// ModelInfo describes one model's budgets. ContextWindow is the total
// input+output window in tokens; MaxOutput is the per-response ceiling.
type ModelInfo struct {
	ID            string
	ContextWindow int
	MaxOutput     int
}

// defaultContextLimit is the fallback for a model we don't recognise.
//
// It stays deliberately conservative. An unrecognised id is more likely an
// older or smaller-window model than a newer one, and the failure modes are
// not symmetric: guessing too small warns a user who had room to spare,
// while guessing too large stays silent through a compaction. Warning early
// is the cheaper mistake.
const defaultContextLimit = 200_000

// lookupModel resolves a model id against a catalog by longest-prefix
// match, so a dated snapshot (claude-opus-4-7-20260115) resolves to the
// same entry as its alias (claude-opus-4-7) without needing its own row.
// Longest match wins so a more specific id is never shadowed by a shorter
// one that happens to share a prefix.
func lookupModel(catalog []ModelInfo, model string) (ModelInfo, bool) {
	if model == "" {
		return ModelInfo{}, false
	}
	best := -1
	for i, m := range catalog {
		if !strings.HasPrefix(model, m.ID) {
			continue
		}
		if best < 0 || len(m.ID) > len(catalog[best].ID) {
			best = i
		}
	}
	if best < 0 {
		return ModelInfo{}, false
	}
	return catalog[best], true
}

// contextWindowOr resolves a model's context window from a catalog,
// falling back to defaultContextLimit. Adapters use this so the fallback
// behaviour is identical across every CLI.
func contextWindowOr(catalog []ModelInfo, model string) int {
	if m, ok := lookupModel(catalog, model); ok && m.ContextWindow > 0 {
		return m.ContextWindow
	}
	return defaultContextLimit
}

// ─── The provider seam ──────────────────────────────────────────────────
//
// #50 (M2). One interface, one transport per LLM API. It exists for two
// reasons and both are load-bearing:
//
//   - D2: the same notebook has to run on a frontier model and on a local
//     one, so the loop must not know which it is talking to.
//   - The loop can then be exercised exhaustively offline against an
//     in-process fake, which is the only way to test refusals, turn caps
//     and tool round-trips deterministically.
//
// Everything here is the union of what the transports actually need. It is
// deliberately not a superset of any one API's features: provider-specific
// extras (server-side compaction, task budgets) stay behind capability
// checks in their own transport rather than widening this.

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// BlockType names the kinds of content a turn can carry. Text and thinking
// are what the user reads; tool_use and tool_result are the loop's plumbing
// and are echoed back verbatim on the next request.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockThinking   BlockType = "thinking"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
)

type ContentBlock struct {
	Type BlockType
	Text string

	// tool_use
	ToolUseID string
	ToolName  string
	ToolInput map[string]any

	// tool_result (ToolUseID identifies which call it answers)
	IsError bool
}

type Message struct {
	Role    Role
	Content []ContentBlock
}

// ToolSpec is a tool as the model sees it. Schemas are strict —
// additionalProperties false with explicit required — so arguments are
// guaranteed to validate and the loop never hand-parses a malformed input.
type ToolSpec struct {
	Name        string
	Description string
	InputSchema map[string]any
}

type ToolCall struct {
	ID    string
	Name  string
	Input map[string]any
}

// Stop reasons, normalised across transports.
const (
	StopEndTurn   = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	// StopRefusal is a *successful* response the model declined to answer.
	// It arrives with an empty content array, so any code that reads the
	// first block unconditionally panics on it.
	StopRefusal = "refusal"
)

type Usage struct {
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
}

func (u Usage) add(o Usage) Usage {
	return Usage{
		InputTokens:         u.InputTokens + o.InputTokens,
		OutputTokens:        u.OutputTokens + o.OutputTokens,
		CacheReadTokens:     u.CacheReadTokens + o.CacheReadTokens,
		CacheCreationTokens: u.CacheCreationTokens + o.CacheCreationTokens,
	}
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int
	// Effort is the primary cost/latency lever where a transport supports
	// it (low|medium|high|xhigh|max); ignored where it does not.
	Effort string
}

type ChunkType string

const (
	ChunkText     ChunkType = "text"
	ChunkThinking ChunkType = "thinking"
	ChunkToolUse  ChunkType = "tool_use"
)

// Chunk is an incremental piece of a response, for the live view only.
type Chunk struct {
	Type     ChunkType
	Text     string
	ToolCall *ToolCall
}

// Result is the finalised turn, valid once Next has returned io.EOF.
type Result struct {
	Content    []ContentBlock
	StopReason string
	Usage      Usage
	Model      string
}

type Stream interface {
	// Next returns io.EOF when the turn is complete.
	Next() (Chunk, error)
	Result() Result
	Close() error
}

type Provider interface {
	Name() string
	Models() []ModelInfo
	Stream(ctx context.Context, req Request) (Stream, error)
}

// Tool is a capability the model can invoke. Run reports the result text
// and whether it is an error the model should see and adapt to — a failing
// tool is a normal part of a turn, not a failed run.
type Tool interface {
	Spec() ToolSpec
	Run(ctx context.Context, input map[string]any, root string) (result string, isError bool, err error)
}

// activeProvider and activeTools are the process-wide selections. Both are
// nil/empty until a transport is configured (M2 slice B) — a prompt cell
// answers 503 rather than pretending, which is the same honesty the 501 on
// prompt cells had in M1.
var (
	activeProvider Provider
	activeTools    []Tool
)

func lookupTool(name string) Tool {
	for _, t := range activeTools {
		if t.Spec().Name == name {
			return t
		}
	}
	return nil
}

func toolSpecs() []ToolSpec {
	out := make([]ToolSpec, 0, len(activeTools))
	for _, t := range activeTools {
		out = append(out, t.Spec())
	}
	return out
}
