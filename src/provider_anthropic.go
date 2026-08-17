package main

// provider_anthropic.go — the Anthropic transport. #50 (M2 slice B).
//
// Written over the official anthropic-sdk-go rather than hand-rolled HTTP:
// it already models streaming accumulation, tool-use blocks, thinking
// configuration and cache_control, and those are exactly the parts that are
// tedious to get right and easy to get subtly wrong.
//
// The file is deliberately three pieces:
//
//	buildAnthropicRequest      pure: our Request  -> SDK params
//	normaliseAnthropicResult   pure: SDK Message  -> our Result
//	anthropicStream            the thin network wrapper
//
// The two pure halves carry the decisions, so they carry the tests. The
// network call in between is the part that cannot be exercised without a
// key — see the commit message rather than a claim of coverage here.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
)

// anthropicDefaultModel is what a notebook gets when it has not chosen.
const anthropicDefaultModel = "claude-opus-5"

// anthropicModels is the catalog this transport owns. Model metadata lives
// with whoever talks to the model — the whole point of #48.
var anthropicModels = []ModelInfo{
	{ID: "claude-opus-5", ContextWindow: 1_000_000, MaxOutput: 128_000},
	{ID: "claude-sonnet-5", ContextWindow: 1_000_000, MaxOutput: 128_000},
	{ID: "claude-fable-5", ContextWindow: 1_000_000, MaxOutput: 128_000},
	{ID: "claude-opus-4-8", ContextWindow: 1_000_000, MaxOutput: 128_000},
	{ID: "claude-haiku-4-5", ContextWindow: 200_000, MaxOutput: 64_000},
}

type anthropicProvider struct {
	client anthropic.Client
}

// newAnthropicProvider builds a client. Credentials are resolved by the SDK
// itself — an API key in the environment, or a profile written by `ant auth
// login` — so an unset ANTHROPIC_API_KEY does not mean unauthenticated and
// we do not second-guess it here.
func newAnthropicProvider(opts ...option.RequestOption) *anthropicProvider {
	return &anthropicProvider{client: anthropic.NewClient(opts...)}
}

func (p *anthropicProvider) Name() string        { return "anthropic" }
func (p *anthropicProvider) Models() []ModelInfo { return anthropicModels }

func (p *anthropicProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	params := buildAnthropicRequest(req)
	stream := p.client.Messages.NewStreaming(ctx, params)
	return &anthropicStream{stream: stream, acc: &anthropic.Message{}}, nil
}

// ─── Request ────────────────────────────────────────────────────────────

func buildAnthropicRequest(req Request) anthropic.MessageNewParams {
	model := req.Model
	if model == "" {
		model = anthropicDefaultModel
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Messages:  make([]anthropic.MessageParam, 0, len(req.Messages)),
		// Adaptive is the only supported mode on current models, and the
		// display has to be set explicitly: it defaults to "omitted", which
		// streams thinking blocks with empty text. A notebook rendering
		// reasoning would look broken rather than absent.
		Thinking: anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		},
	}

	if req.System != "" {
		// The cache is a prefix match over exact bytes rendered
		// tools -> system -> messages, so the breakpoint goes on the last
		// stable block. Without it every run re-pays for the whole prefix,
		// which is what makes re-projection affordable or not (#51).
		params.System = []anthropic.TextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewCacheControlEphemeralParam(),
		}}
	}

	if effort, ok := anthropicEffort(req.Effort); ok {
		params.OutputConfig = anthropic.OutputConfigParam{Effort: effort}
	}

	for _, spec := range req.Tools {
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: anthropicTool(spec)})
	}
	for _, m := range req.Messages {
		if mapped, ok := anthropicMessage(m); ok {
			params.Messages = append(params.Messages, mapped)
		}
	}
	return params
}

func anthropicEffort(effort string) (anthropic.OutputConfigEffort, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return anthropic.OutputConfigEffortLow, true
	case "medium":
		return anthropic.OutputConfigEffortMedium, true
	case "high":
		return anthropic.OutputConfigEffortHigh, true
	case "xhigh":
		return anthropic.OutputConfigEffortXhigh, true
	case "max":
		return anthropic.OutputConfigEffortMax, true
	}
	// An unrecognised level is dropped rather than sent through to be
	// rejected — the notebook's effort field is free text.
	return "", false
}

func anthropicTool(spec ToolSpec) *anthropic.ToolParam {
	tool := &anthropic.ToolParam{
		Name:        spec.Name,
		Description: anthropic.String(spec.Description),
		// Strict is what guarantees the arguments validate, so the loop
		// never has to hand-parse a malformed input.
		Strict: anthropic.Bool(true),
	}
	schema := anthropic.ToolInputSchemaParam{ExtraFields: map[string]any{}}
	if props, ok := spec.InputSchema["properties"]; ok {
		schema.Properties = props
	}
	if req, ok := spec.InputSchema["required"].([]string); ok {
		schema.Required = req
	}
	// additionalProperties has no field of its own on the params struct;
	// ExtraFields is the documented way to carry the rest of the schema.
	if extra, ok := spec.InputSchema["additionalProperties"]; ok {
		schema.ExtraFields["additionalProperties"] = extra
	}
	tool.InputSchema = schema
	return tool
}

func anthropicMessage(m Message) (anthropic.MessageParam, bool) {
	role := anthropic.MessageParamRoleUser
	if m.Role == RoleAssistant {
		role = anthropic.MessageParamRoleAssistant
	}
	var blocks []anthropic.ContentBlockParamUnion
	for _, b := range m.Content {
		switch b.Type {
		case BlockText:
			if strings.TrimSpace(b.Text) != "" {
				blocks = append(blocks, anthropic.NewTextBlock(b.Text))
			}
		case BlockToolUse:
			blocks = append(blocks, anthropic.NewToolUseBlock(b.ToolUseID, b.ToolInput, b.ToolName))
		case BlockToolResult:
			blocks = append(blocks, anthropic.NewToolResultBlock(b.ToolUseID, b.Text, b.IsError))
		case BlockThinking:
			// Not replayed. A stored summary is not the signed block the
			// API would accept, and other models drop foreign thinking
			// anyway — see nb_project.go.
		}
	}
	if len(blocks) == 0 {
		return anthropic.MessageParam{}, false
	}
	return anthropic.MessageParam{Role: role, Content: blocks}, true
}

// ─── Response ───────────────────────────────────────────────────────────

func normaliseAnthropicResult(msg *anthropic.Message) Result {
	if msg == nil {
		return Result{StopReason: StopEndTurn}
	}
	res := Result{
		Model:      string(msg.Model),
		StopReason: normaliseStopReason(msg.StopReason),
		Usage: Usage{
			InputTokens:         msg.Usage.InputTokens,
			OutputTokens:        msg.Usage.OutputTokens,
			CacheReadTokens:     msg.Usage.CacheReadInputTokens,
			CacheCreationTokens: msg.Usage.CacheCreationInputTokens,
		},
	}
	// A refusal arrives as a successful response with an empty content
	// array, so this loop simply produces nothing rather than needing a
	// special case — the caller checks StopReason first either way.
	for _, block := range msg.Content {
		switch v := block.AsAny().(type) {
		case anthropic.TextBlock:
			res.Content = append(res.Content, ContentBlock{Type: BlockText, Text: v.Text})
		case anthropic.ThinkingBlock:
			res.Content = append(res.Content, ContentBlock{Type: BlockThinking, Text: v.Thinking})
		case anthropic.ToolUseBlock:
			input := map[string]any{}
			// block.Input is raw JSON; decoding is the documented path and
			// string-matching it is explicitly not.
			if raw := v.JSON.Input.Raw(); raw != "" {
				_ = json.Unmarshal([]byte(raw), &input)
			}
			res.Content = append(res.Content, ContentBlock{
				Type: BlockToolUse, ToolUseID: v.ID, ToolName: v.Name, ToolInput: input,
			})
		}
	}
	return res
}

func normaliseStopReason(r anthropic.StopReason) string {
	switch r {
	case anthropic.StopReasonToolUse:
		return StopToolUse
	case anthropic.StopReasonMaxTokens:
		return StopMaxTokens
	case anthropic.StopReasonRefusal:
		return StopRefusal
	case anthropic.StopReasonEndTurn:
		return StopEndTurn
	}
	// Anything new is treated as a completed turn: the content is still
	// there, and inventing an error for an unknown-but-successful stop
	// reason would fail runs that actually worked.
	return StopEndTurn
}

// ─── Stream ─────────────────────────────────────────────────────────────

type anthropicStream struct {
	stream *ssestream.Stream[anthropic.MessageStreamEventUnion]
	acc    *anthropic.Message
	err    error
}

func (s *anthropicStream) Next() (Chunk, error) {
	for s.stream.Next() {
		event := s.stream.Current()
		if err := s.acc.Accumulate(event); err != nil {
			s.err = err
			return Chunk{}, err
		}
		switch ev := event.AsAny().(type) {
		case anthropic.ContentBlockDeltaEvent:
			switch d := ev.Delta.AsAny().(type) {
			case anthropic.TextDelta:
				if d.Text != "" {
					return Chunk{Type: ChunkText, Text: d.Text}, nil
				}
			case anthropic.ThinkingDelta:
				if d.Thinking != "" {
					return Chunk{Type: ChunkThinking, Text: d.Thinking}, nil
				}
			}
		case anthropic.ContentBlockStartEvent:
			if tu, ok := ev.ContentBlock.AsAny().(anthropic.ToolUseBlock); ok {
				return Chunk{Type: ChunkToolUse, ToolCall: &ToolCall{ID: tu.ID, Name: tu.Name}}, nil
			}
		}
		// Any other event advances the accumulator without producing a
		// user-visible chunk; keep reading rather than surfacing noise.
	}
	if err := s.stream.Err(); err != nil {
		s.err = err
		return Chunk{}, fmt.Errorf("anthropic stream: %w", err)
	}
	return Chunk{}, io.EOF
}

func (s *anthropicStream) Result() Result { return normaliseAnthropicResult(s.acc) }

func (s *anthropicStream) Close() error { return s.stream.Close() }

// ─── Boot wiring ────────────────────────────────────────────────────────

// anthropicCredentialsPresent reports whether it is worth offering the
// Anthropic transport at all.
//
// This is a heuristic and only a heuristic: the SDK is the authority on
// credential resolution and checks more places than this does. The point is
// the failure mode. If we install the provider unconditionally, a user with
// no credentials gets an authentication error inside their first cell,
// which reads like a bug in the notebook. If we decline to install it, they
// get "no model provider is configured", which is true and actionable.
//
// A false negative here costs a user with exotic credentials one config
// setting; a false positive costs every key-less user a confusing failure.
func anthropicCredentialsPresent() bool {
	for _, env := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BEARER_TOKEN"} {
		if strings.TrimSpace(os.Getenv(env)) != "" {
			return true
		}
	}
	// A profile written by `ant auth login`, which the SDK picks up with no
	// environment variable set at all.
	dir := os.Getenv("ANTHROPIC_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = filepath.Join(home, ".config", "anthropic")
	}
	entries, err := os.ReadDir(filepath.Join(dir, "credentials"))
	return err == nil && len(entries) > 0
}

// initProviders selects the transport and tools the notebook loop will use.
// Called once at boot; leaves activeProvider nil when nothing is available,
// so a prompt cell answers 503 rather than failing mid-run.
func initProviders() {
	activeTools = builtinTools()
	if !anthropicCredentialsPresent() {
		log.Printf("notebooks: no Anthropic credentials found — prompt cells will report that no provider is configured")
		return
	}
	activeProvider = newAnthropicProvider()
	log.Printf("notebooks: provider %s ready (default model %s, %d tools)",
		activeProvider.Name(), anthropicDefaultModel, len(activeTools))
}
