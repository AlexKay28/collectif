package main

// provider_openai.go — the OpenAI-compatible transport. #53 (M4).
//
// One hand-rolled chat-completions client covering OpenAI, Ollama,
// llama.cpp, vLLM, OpenRouter and the Vercel AI Gateway, selected by
// base_url. Hand-rolled rather than over a vendored SDK because the thing
// being served is not "OpenAI" but a family of servers that each implement
// a subset of one API — the divergences are the work, and an SDK written
// against the strictest member of the family hides them behind types that
// assume fields the others never send.
//
// Same three-part split as provider_anthropic.go, for the same reason:
//
//	buildOpenAIRequest      pure: our Request -> wire JSON
//	normaliseOpenAIResult   pure: accumulated stream -> our Result
//	openaiStream            the network wrapper
//
// What is *not* here is a second opinion about what a turn is. The
// contract is provider.go's, it is asserted by provider_conformance_test.go
// against both transports at once, and this file was written to pass it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// openAIToolErrorPrefix marks a failed tool result.
//
// Divergence with no clean answer: Anthropic's tool_result block carries
// is_error, and a role:"tool" message has no such field — the model sees
// the text and nothing else. Dropping the flag would let "read x: no such
// file" read as the file's contents, so the flag becomes text. It is a
// prefix rather than a wrapper so that a model which ignores it still
// reads the message underneath.
const openAIToolErrorPrefix = "[tool error] "

// ─── Flavours ───────────────────────────────────────────────────────────

// openAIFlavour is what we know about the server at the other end.
//
// Detection is by base_url because that is the only thing the user
// configures; there is no capability handshake in this API. Two of the
// three things a flavour decides are *claims* rather than behaviour
// (display name, cache reporting), so a mis-detected local server costs a
// label. The third — which max-tokens field to send — is decided by
// hostname, which does not guess.
type openAIFlavour struct {
	name string
	caps ProviderCapabilities

	// completionTokensField is "max_tokens" everywhere except OpenAI's own
	// endpoint, where the reasoning models reject it outright:
	// "Unsupported parameter: 'max_tokens' is not supported with this
	// model, use 'max_completion_tokens' instead".
	completionTokensField string

	// defaultModel is what a notebook that has not chosen gets. Empty
	// where model ids are whatever the operator happened to pull, which is
	// every local server — guessing there produces a 404 on the first
	// cell instead of an explanation.
	defaultModel string

	models []ModelInfo
}

// openAIFlavourFor picks a flavour from a base URL.
//
// The hosted endpoints are matched on hostname and are reliable. The local
// ones are matched on their default ports, which is a hint: llama.cpp on
// 8000 will be labelled vLLM. That mislabels a row in /api/providers and
// changes nothing else, because the three local servers declare identical
// capabilities — none of them reports prompt caching, and none of them
// takes a reasoning_effort.
func openAIFlavourFor(baseURL string) openAIFlavour {
	host := baseURL
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	host = strings.ToLower(host)

	local := ProviderCapabilities{
		// Every one of these caches the prompt prefix in its own KV cache
		// and none of them reports a token count for it. "0% cached" would
		// therefore be a statement about our request rather than theirs,
		// which is exactly the reading M2.5's chip invites.
		Cache: CacheNone,
		// Reasoning models are routinely run locally and the servers do
		// relay reasoning_content; nothing can be signed and echoed back.
		Reasoning: true,
		Usage:     true,
	}

	switch {
	case strings.Contains(host, "api.openai.com"):
		return openAIFlavour{
			name: "openai",
			caps: ProviderCapabilities{
				// Automatic prefix caching: nothing to place, and only the
				// read side is ever reported — there is no cache-write
				// counter because writes are not billed.
				Cache: CacheAutomatic, Reasoning: true, Effort: true, Usage: true,
			},
			completionTokensField: "max_completion_tokens",
			defaultModel:          "gpt-5-mini",
			models:                openAIHostedModels,
		}
	case strings.Contains(host, "openrouter.ai"):
		return openAIFlavour{
			name: "openrouter",
			caps: ProviderCapabilities{Cache: CacheAutomatic, Reasoning: true, Effort: true, Usage: true},
			// A router in front of many providers: ids are namespaced
			// ("anthropic/claude-sonnet-5") and change weekly, so there is
			// no catalog worth freezing here.
			completionTokensField: "max_tokens",
		}
	case strings.Contains(host, "ai-gateway.vercel.sh"), strings.Contains(host, "gateway.ai.vercel"):
		return openAIFlavour{
			name:                  "vercel-ai-gateway",
			caps:                  ProviderCapabilities{Cache: CacheAutomatic, Reasoning: true, Effort: true, Usage: true},
			completionTokensField: "max_tokens",
		}
	case strings.Contains(host, "11434"), strings.Contains(host, "ollama"):
		return openAIFlavour{name: "ollama", caps: local, completionTokensField: "max_tokens"}
	case strings.Contains(host, "8000"), strings.Contains(host, "vllm"):
		return openAIFlavour{name: "vllm", caps: local, completionTokensField: "max_tokens"}
	case strings.Contains(host, "8080"), strings.Contains(host, "llama"):
		return openAIFlavour{name: "llama.cpp", caps: local, completionTokensField: "max_tokens"}
	}
	// Anything else is an OpenAI-compatible server we have never met. It
	// gets the conservative claims: we do not know that it caches, so we
	// do not say it does.
	return openAIFlavour{name: "openai-compatible", caps: local, completionTokensField: "max_tokens"}
}

// openAIHostedModels is the catalog for api.openai.com, current as of
// 2026-08. Prices are USD per million tokens; the cached-read rate is a
// tenth of input, which is the whole of what automatic caching buys.
//
// It is a hint, not an authority: an id that is not here resolves to
// defaultContextLimit and no pricing, which disables the notebook's dollar
// budget rather than enforcing it wrongly (nb_agent.go).
var openAIHostedModels = []ModelInfo{
	{ID: "gpt-5.1", ContextWindow: 400_000, MaxOutput: 128_000,
		InputUSDPerMTok: 1.25, OutputUSDPerMTok: 10, CacheReadUSDPerMTok: 0.125},
	{ID: "gpt-5-mini", ContextWindow: 400_000, MaxOutput: 128_000,
		InputUSDPerMTok: 0.25, OutputUSDPerMTok: 2, CacheReadUSDPerMTok: 0.025},
	{ID: "gpt-5-nano", ContextWindow: 400_000, MaxOutput: 128_000,
		InputUSDPerMTok: 0.05, OutputUSDPerMTok: 0.4, CacheReadUSDPerMTok: 0.005},
	{ID: "gpt-5", ContextWindow: 400_000, MaxOutput: 128_000,
		InputUSDPerMTok: 1.25, OutputUSDPerMTok: 10, CacheReadUSDPerMTok: 0.125},
	{ID: "gpt-4.1-mini", ContextWindow: 1_047_576, MaxOutput: 32_768,
		InputUSDPerMTok: 0.4, OutputUSDPerMTok: 1.6, CacheReadUSDPerMTok: 0.1},
	{ID: "gpt-4.1", ContextWindow: 1_047_576, MaxOutput: 32_768,
		InputUSDPerMTok: 2, OutputUSDPerMTok: 8, CacheReadUSDPerMTok: 0.5},
}

// ─── Provider ───────────────────────────────────────────────────────────

type openaiProvider struct {
	baseURL string
	apiKey  string
	flavour openAIFlavour
	client  *http.Client
}

// newOpenAIProvider builds a transport for one base URL. The URL is taken
// as given (trailing slash trimmed) rather than normalised: "/v1" is
// conventional but Ollama, llama.cpp and several gateways each mount the
// route differently, and rewriting a working URL is a worse failure than
// requiring a complete one.
func newOpenAIProvider(baseURL, apiKey string) *openaiProvider {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	return &openaiProvider{
		baseURL: baseURL,
		apiKey:  apiKey,
		flavour: openAIFlavourFor(baseURL),
		// No overall timeout: a turn with thinking on can legitimately run
		// for minutes, and the caller's context is the cancellation story.
		// The dial and header timeouts still bound a dead endpoint.
		client: &http.Client{Transport: &http.Transport{
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		}},
	}
}

func (p *openaiProvider) Name() string                       { return p.flavour.name }
func (p *openaiProvider) Models() []ModelInfo                { return p.flavour.models }
func (p *openaiProvider) Capabilities() ProviderCapabilities { return p.flavour.caps }
func (p *openaiProvider) defaultModel() string               { return p.flavour.defaultModel }
func (p *openaiProvider) endpoint() string                   { return p.baseURL + "/chat/completions" }
func (p *openaiProvider) fail(k ProviderErrorKind, e error) error {
	return &ProviderError{Kind: k, Provider: p.flavour.name, err: e}
}

func (p *openaiProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	body, err := buildOpenAIRequest(req, p.flavour)
	if err != nil {
		return nil, p.fail(ProviderErrBadRequest, err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, p.fail(ProviderErrBadRequest, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint(), bytes.NewReader(raw))
	if err != nil {
		return nil, p.fail(ProviderErrTransport, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	res, err := p.client.Do(httpReq)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, p.fail(ProviderErrTransport, err)
	}
	if res.StatusCode != http.StatusOK {
		defer res.Body.Close()
		// Bounded: an HTML error page from a proxy in front of the model
		// server is not worth holding in memory, and the first 8 KB says
		// as much as the whole of it.
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		return nil, &ProviderError{
			Kind:     classifyHTTPStatus(res.StatusCode),
			Provider: p.flavour.name,
			Status:   res.StatusCode,
			Detail:   providerErrorDetail(detail),
		}
	}
	return newOpenAIStream(ctx, res, p.flavour.name), nil
}

// ─── Request ────────────────────────────────────────────────────────────

type openAIChatRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []openAITool    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	// StreamOptions asks for a usage report on the final chunk. Without it
	// a streamed turn reports no tokens at all on OpenAI, which would make
	// every prompt cell display as free and the notebook's dollar budget
	// unenforceable.
	StreamOptions   *openAIStreamOptions `json:"stream_options,omitempty"`
	ReasoningEffort string               `json:"reasoning_effort,omitempty"`

	// Exactly one of these is set, per flavour. Both are pointers so the
	// unused one is omitted rather than sent as zero — a literal
	// "max_tokens": 0 is a 400.
	MaxTokens           *int `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`
}

type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls is divergence 1: where Anthropic puts a tool_use *block*
	// in the assistant's content, this API puts a parallel array beside it
	// and stringifies the arguments.
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set only on a role:"tool" message — divergence 2.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
	// Strict is OpenAI's guarantee that arguments validate against the
	// schema. Servers that do not implement it ignore the field; the loop
	// does not rely on it, because most of this family will not honour it.
	Strict bool `json:"strict,omitempty"`
}

func buildOpenAIRequest(req Request, fl openAIFlavour) (openAIChatRequest, error) {
	model := req.Model
	if model == "" {
		model = fl.defaultModel
	}
	if model == "" {
		return openAIChatRequest{}, fmt.Errorf(
			"%s does not have a default model — model ids on this endpoint are whatever it was started with, "+
				"so set one on the notebook or the cell", fl.name)
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}

	out := openAIChatRequest{
		Model:         model,
		Stream:        true,
		StreamOptions: &openAIStreamOptions{IncludeUsage: true},
	}
	if fl.completionTokensField == "max_completion_tokens" {
		out.MaxCompletionTokens = &maxTokens
	} else {
		out.MaxTokens = &maxTokens
	}

	// Divergence 6: the system prompt is a message here, not a top-level
	// field, and it must come first — a system message anywhere else is
	// either ignored or an error depending on the server.
	if req.System != "" {
		out.Messages = append(out.Messages, openAIMessage{Role: "system", Content: req.System})
	}
	out.Messages = append(out.Messages, openAIMessages(req.Messages)...)

	for _, spec := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIToolFunction{
				Name:        spec.Name,
				Description: spec.Description,
				Parameters:  spec.InputSchema,
				Strict:      true,
			},
		})
	}

	// Divergence 3b: effort. Only sent where the endpoint takes it —
	// local servers reject or ignore unknown parameters inconsistently,
	// and a rejected request is a failed cell.
	if fl.caps.Effort {
		if effort, ok := openAIEffort(req.Effort); ok {
			out.ReasoningEffort = effort
		}
	}

	// Divergence 5: nothing is placed. Prefix caching on this family is
	// automatic where it exists at all, so req.StablePrefixMessages has no
	// expression here — the prefix still has to be byte-identical between
	// runs for the server to reuse it, which is why the ordering
	// guarantees in provider.go apply to both transports.
	return out, nil
}

// openAIMessages flattens our block-structured turns into this API's
// message list. It is where divergences 1, 2 and 3 land.
func openAIMessages(msgs []Message) []openAIMessage {
	var out []openAIMessage
	for _, m := range msgs {
		var text strings.Builder
		var calls []openAIToolCall
		// Tool results become their own messages and must immediately
		// follow the assistant turn that asked for them, so they are
		// emitted before any prose in the same user turn.
		var results []openAIMessage

		for _, b := range m.Content {
			switch b.Type {
			case BlockText:
				if strings.TrimSpace(b.Text) == "" {
					continue
				}
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString(b.Text)
			case BlockToolUse:
				args, err := json.Marshal(b.ToolInput)
				if err != nil || b.ToolInput == nil {
					// An empty object, never a bare "null": a tool call
					// whose arguments will not marshal is still a call the
					// model made, and dropping it would leave the next
					// tool message answering nothing.
					args = []byte("{}")
				}
				calls = append(calls, openAIToolCall{
					ID:       b.ToolUseID,
					Type:     "function",
					Function: openAIFunctionCall{Name: b.ToolName, Arguments: string(args)},
				})
			case BlockToolResult:
				content := b.Text
				if b.IsError {
					content = openAIToolErrorPrefix + content
				}
				results = append(results, openAIMessage{
					Role: "tool", ToolCallID: b.ToolUseID, Content: content,
				})
			case BlockThinking:
				// Divergence 3: dropped, deliberately. There is nowhere to
				// put it — no field on a chat-completions request accepts
				// reasoning back, and unlike Anthropic nothing requires it.
				// The signature Anthropic needs does not exist here, so a
				// notebook that switches models mid-document loses its
				// reasoning history rather than sending an unsigned copy
				// that would be rejected on the way back.
			}
		}

		out = append(out, results...)
		if text.Len() > 0 || len(calls) > 0 {
			out = append(out, openAIMessage{
				Role:      openAIRole(m.Role),
				Content:   text.String(),
				ToolCalls: calls,
			})
		}
	}
	return out
}

func openAIRole(r Role) string {
	if r == RoleAssistant {
		return "assistant"
	}
	return "user"
}

// openAIEffort maps the notebook's lever onto reasoning_effort.
//
// The two scales do not have the same top. Anthropic's runs to xhigh and
// max; this one stops at high. They clamp *up* rather than being dropped:
// a user who asked for maximum effort and silently got the default would
// be reading a cheaper answer than they paid attention for, which is the
// worse of the two wrong answers.
func openAIEffort(effort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return "minimal", true
	case "low":
		return "low", true
	case "medium":
		return "medium", true
	case "high", "xhigh", "max":
		return "high", true
	}
	return "", false
}

// ─── Response ───────────────────────────────────────────────────────────

type openAIStreamChunk struct {
	Model   string             `json:"model"`
	Choices []openAIChoice     `json:"choices"`
	Usage   *openAIUsageReport `json:"usage"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type openAIChoice struct {
	Delta        openAIDelta `json:"delta"`
	FinishReason string      `json:"finish_reason"`
}

// openAIDelta reads its text-bearing fields raw.
//
// ADR 0002's "a field's shape must not cost the line": a polymorphic field
// declared as a string makes json.Unmarshal fail on the *whole* object, so
// one server sending {"content":[{"type":"text","text":"hi"}]} would cost
// us every other field in the chunk — the tool call, the finish reason and
// the usage — rather than costing us the content. openAIText flattens by
// shape instead.
type openAIDelta struct {
	Content json.RawMessage `json:"content"`
	// Two spellings in the wild for the same thing: reasoning_content is
	// what vLLM, Ollama and DeepSeek-derived servers emit, reasoning is
	// OpenRouter's. Both are read; a server that sends neither simply has
	// no reasoning to show.
	ReasoningContent json.RawMessage       `json:"reasoning_content"`
	Reasoning        json.RawMessage       `json:"reasoning"`
	Refusal          json.RawMessage       `json:"refusal"`
	ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
}

// openAIToolCallDelta is a fragment of a tool call. Index is the only
// thing tying the fragments together: id and name arrive once, on the
// first fragment, and every later fragment carries a slice of the
// arguments string and nothing else.
type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIUsageReport struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// normaliseOpenAIUsage is divergence 5, and it is the one that silently
// produces wrong numbers rather than an error.
//
// prompt_tokens here *includes* the cached part; Anthropic's input_tokens
// *excludes* it. Copying both fields across straight would count the
// cached tokens twice in promptTokens(), inflating every prompt size and
// every cost estimate on a warm run — and the warmer the cache, the bigger
// the error, which is the wrong way round for a bug to scale.
func normaliseOpenAIUsage(u *openAIUsageReport) Usage {
	if u == nil {
		return Usage{}
	}
	var cached int64
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	}
	uncached := u.PromptTokens - cached
	if uncached < 0 {
		uncached = 0
	}
	return Usage{
		InputTokens:     uncached,
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: cached,
		// No cache-write counter exists on this API. Left at zero because
		// it is genuinely zero — writes are not billed where caching is
		// automatic — and the notebook reads the *capability* rather than
		// this field to decide whether to show a cache figure at all.
		CacheCreationTokens: 0,
	}
}

// normaliseOpenAIStopReason maps finish_reason onto the shared vocabulary.
func normaliseOpenAIStopReason(finish string, hasCalls, refused bool) string {
	switch finish {
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopRefusal
	}
	if refused {
		return StopRefusal
	}
	// Ollama and llama.cpp report "stop" on a turn that made tool calls.
	// Taking that literally ends the loop with calls outstanding, so the
	// evidence wins over the label.
	if hasCalls {
		return StopToolUse
	}
	return StopEndTurn
}

// openAIText flattens a text-bearing field by shape: a string, an object
// with a text field, or an array of either.
func openAIText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(openAIText(p))
		}
		return b.String()
	}
	var obj struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Text != "" {
			return obj.Text
		}
		return obj.Content
	}
	return ""
}

// ─── Stream ─────────────────────────────────────────────────────────────

// openaiStream decodes SSE and accumulates the turn.
//
// Accumulation is this file's job rather than the caller's because the
// deltas are not self-describing: a tool call arrives as an id, then a
// name, then arguments in fragments that are not individually valid JSON,
// and only the completed turn can be handed to the loop.
type openaiStream struct {
	ctx      context.Context
	res      *http.Response
	scanner  *bufio.Scanner
	provider string

	pending []Chunk
	done    bool

	mu        sync.Mutex
	text      strings.Builder
	reasoning strings.Builder
	calls     []*openAICallAcc
	byIndex   map[int]*openAICallAcc
	finish    string
	refused   bool
	usage     Usage
	model     string
}

type openAICallAcc struct {
	id      string
	name    string
	args    strings.Builder
	started bool
}

func newOpenAIStream(ctx context.Context, res *http.Response, provider string) *openaiStream {
	sc := bufio.NewScanner(res.Body)
	// A single SSE line is one delta, but "one delta" includes a whole
	// tool-call argument object on servers that do not fragment them.
	// 64 KB is the default and is not enough; a truncated line would fail
	// to parse as JSON and the turn would lose its tool call.
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return &openaiStream{
		ctx: ctx, res: res, scanner: sc, provider: provider,
		byIndex: map[int]*openAICallAcc{},
	}
}

func (s *openaiStream) Next() (Chunk, error) {
	for {
		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return c, nil
		}
		if s.done {
			return Chunk{}, io.EOF
		}
		if !s.scanner.Scan() {
			s.done = true
			if err := s.scanner.Err(); err != nil {
				// A read that failed because the caller cancelled is an
				// interrupt, not a provider failure. errors.Is would see
				// through a wrapper either way; what this prevents is the
				// *classification* — a cell the user stopped on purpose
				// reporting "openai (transport): context canceled".
				if ctxErr := s.ctx.Err(); ctxErr != nil {
					return Chunk{}, ctxErr
				}
				return Chunk{}, &ProviderError{
					Kind: ProviderErrTransport, Provider: s.provider, err: err,
				}
			}
			return Chunk{}, io.EOF
		}
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			// Comments (": ping"), event: lines and blank separators. The
			// keep-alives some gateways send are comments, so ignoring
			// anything that is not data is what keeps a long turn alive.
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			s.done = true
			continue
		}
		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// One malformed chunk is not a failed turn. Skipping it costs
			// the text it carried; failing the turn costs everything
			// already streamed, and these servers do emit the occasional
			// oddity mid-stream.
			continue
		}
		if chunk.Error != nil {
			s.done = true
			return Chunk{}, &ProviderError{
				Kind: ProviderErrServer, Provider: s.provider, Detail: chunk.Error.Message,
			}
		}
		s.absorb(chunk)
	}
}

// absorb folds one wire chunk into the accumulated turn and queues
// whatever the live view should see.
func (s *openaiStream) absorb(chunk openAIStreamChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = normaliseOpenAIUsage(chunk.Usage)
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != "" {
			s.finish = choice.FinishReason
		}
		if openAIText(choice.Delta.Refusal) != "" {
			s.refused = true
		}
		reasoning := openAIText(choice.Delta.ReasoningContent)
		if reasoning == "" {
			reasoning = openAIText(choice.Delta.Reasoning)
		}
		if reasoning != "" {
			s.reasoning.WriteString(reasoning)
			s.pending = append(s.pending, Chunk{Type: ChunkThinking, Text: reasoning})
		}
		if text := openAIText(choice.Delta.Content); text != "" {
			s.text.WriteString(text)
			s.pending = append(s.pending, Chunk{Type: ChunkText, Text: text})
		}
		for _, frag := range choice.Delta.ToolCalls {
			acc, ok := s.byIndex[frag.Index]
			if !ok {
				acc = &openAICallAcc{}
				s.byIndex[frag.Index] = acc
				s.calls = append(s.calls, acc)
			}
			if frag.ID != "" {
				acc.id = frag.ID
			}
			if frag.Function.Name != "" {
				acc.name = frag.Function.Name
			}
			acc.args.WriteString(frag.Function.Arguments)
			// Announce the call once, as soon as it has a name — the same
			// point Anthropic's content_block_start gives us, so the live
			// view shows "→ read" at the same moment on both transports
			// rather than only after the arguments finish streaming.
			if !acc.started && acc.name != "" {
				acc.started = true
				s.pending = append(s.pending, Chunk{
					Type: ChunkToolUse, ToolCall: &ToolCall{ID: acc.id, Name: acc.name},
				})
			}
		}
	}
}

func (s *openaiStream) Result() Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	return normaliseOpenAIResult(s)
}

// normaliseOpenAIResult assembles the accumulated turn. Block order is
// reasoning, prose, tool calls — the order Anthropic streams them in, so
// a notebook renders the two transports identically.
func normaliseOpenAIResult(s *openaiStream) Result {
	res := Result{
		Model: s.model,
		Usage: s.usage,
	}
	if txt := s.reasoning.String(); txt != "" {
		// No Signature: nothing in this family signs reasoning, and the
		// empty signature is what stops buildAnthropicRequest replaying it
		// if the same notebook is later run on Anthropic.
		res.Content = append(res.Content, ContentBlock{Type: BlockThinking, Text: txt})
	}
	if txt := s.text.String(); txt != "" {
		res.Content = append(res.Content, ContentBlock{Type: BlockText, Text: txt})
	}
	for _, call := range s.calls {
		if call.name == "" {
			continue
		}
		input := map[string]any{}
		if raw := strings.TrimSpace(call.args.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &input); err != nil {
				// A truncated argument string means the turn was cut off
				// mid-call. The call is still reported: dispatchTool
				// answers the model with a tool error it can react to,
				// which is more useful than a silently missing call.
				input = map[string]any{}
			}
		}
		res.Content = append(res.Content, ContentBlock{
			Type: BlockToolUse, ToolUseID: call.id, ToolName: call.name, ToolInput: input,
		})
	}
	res.StopReason = normaliseOpenAIStopReason(s.finish, len(s.calls) > 0, s.refused)
	if res.StopReason == StopRefusal {
		// A refusal is an empty turn on both transports. OpenAI sometimes
		// carries the model's own wording in delta.refusal; it is dropped
		// rather than rendered as prose, because a refusal that renders as
		// an answer is worse than one that renders as nothing, and the
		// loop's own message says what happened.
		res.Content = nil
	}
	return res
}

func (s *openaiStream) Close() error {
	if s.res != nil && s.res.Body != nil {
		return s.res.Body.Close()
	}
	return nil
}

// ─── Boot wiring ────────────────────────────────────────────────────────

// openAIConfigured resolves an endpoint from the environment.
//
// Two ways in, matching what people already have set: OPENAI_BASE_URL +
// OPENAI_API_KEY for a hosted or self-hosted OpenAI-compatible server, and
// OLLAMA_HOST on its own for the local case, which has no key at all. The
// key is not required — a local server that ignores Authorization is the
// normal case, and refusing to configure one without a key would exclude
// every one of them.
func openAIConfigured() (baseURL, apiKey string, ok bool) {
	if base := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); base != "" {
		return base, strings.TrimSpace(os.Getenv("OPENAI_API_KEY")), true
	}
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		return "https://api.openai.com/v1", key, true
	}
	if host := strings.TrimSpace(os.Getenv("OLLAMA_HOST")); host != "" {
		if !strings.HasPrefix(host, "http://") && !strings.HasPrefix(host, "https://") {
			host = "http://" + host
		}
		return strings.TrimRight(host, "/") + "/v1", "", true
	}
	return "", "", false
}

// initOpenAIProvider installs the transport if one is configured. Same
// contract as the Anthropic half: absent credentials mean the provider is
// not offered, so a prompt cell says "no model provider is configured"
// rather than failing with an auth error that reads like our bug.
func initOpenAIProvider() Provider {
	base, key, ok := openAIConfigured()
	if !ok {
		return nil
	}
	p := newOpenAIProvider(base, key)
	log.Printf("notebooks: provider %s ready at %s (%d catalogued models)",
		p.Name(), p.baseURL, len(p.Models()))
	return p
}
