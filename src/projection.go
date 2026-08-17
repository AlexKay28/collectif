package main

// projection.go — turning a CLI's transcript into notebook-shaped turns.
// #47 P0, per ADR 0002.
//
// ADR 0001 concluded that collectif should run its own agent loop because
// second-hand knowledge decays silently. ADR 0002 narrowed that: the loop
// stays, but for notebooks with no session attached. For a *running* CLI
// session the notebook is a view, and this file is the seam where someone
// else's log becomes our document.
//
// Two rules hold for every adapter that implements this.
//
// Never invent. If the transcript does not say a thing happened, the
// notebook does not show it happening. An adapter with no content to give
// declares `TranscriptContent: false` and returns nothing, and the UI says
// so — a thin session is honest, a fabricated one is the ANSI-scraping
// failure this whole design exists to leave behind.
//
// Never fail. The file is written by another process, mid-write lines are
// normal, and the format belongs to a product that ships weekly. Every
// malformed shape returns "nothing to project", never an error and never a
// panic. A parser that stops the watcher costs the user their session view;
// a parser that skips a line costs them one line.

import (
	"encoding/json"
	"strings"
	"time"
)

// PartKind names the notebook-visible shapes a transcript can yield. It is
// deliberately smaller than the set of things a transcript contains: the
// question is not "what did the CLI write down" but "what would a person
// want to read in a document".
type PartKind string

const (
	PartUserText      PartKind = "user"
	PartAssistantText PartKind = "assistant"
	PartThinking      PartKind = "thinking"
	PartToolCall      PartKind = "tool_call"
	PartToolResult    PartKind = "tool_result"

	// PartCompactSummary is the summary a CLI writes when it compacts its
	// own context. It is the only remaining record of everything above it,
	// so the notebook renders it rather than dropping it — but never as
	// something the user typed.
	PartCompactSummary PartKind = "compact_summary"

	// PartInterrupted marks the user stopping a turn. It is a state change
	// on the turn that was running, not a turn of its own.
	PartInterrupted PartKind = "interrupted"

	// PartInjection is context the harness put into the model's window that
	// nobody typed: skill bodies, hook output, system reminders, background
	// task notifications.
	//
	// These are filtered out of the conversation for good reason — a real
	// session carries thousands of them and they would bury the sentences a
	// person actually wrote. But filtered out is not thrown away. An
	// injection you cannot see is often the whole explanation for why an
	// agent did something surprising, and a record that omits them claims
	// the turn began with your prompt when it did not.
	PartInjection PartKind = "injection"
)

// injectionExcerptMax bounds what one injection contributes to the log. A
// single background-task notification runs to 47 KB; recording bodies would
// make the notebook cost the size of everything ever injected. The size is
// recorded in full, the body is not — the transcript on disk is the
// archive, and this is a view over it.
const injectionExcerptMax = 240

// claudeInterruptMarkers are the literal strings Claude Code writes to the
// transcript when a turn is stopped. They arrive as role:"user" with no
// origin and no isMeta, so every provenance filter passes them and the
// document shows a prompt nobody typed.
//
// Matching on English sentinel text is fragile and there is no better
// signal in the format — no flag, no subtype, no distinguishing field. The
// mitigation is that the match is whole-line rather than a substring
// search, so a prompt *about* interruption is still a prompt, and a wording
// change degrades to showing the sentinel as a prompt again rather than to
// losing turns.
var claudeInterruptMarkers = []string{
	"[Request interrupted by user]",
	"[Request interrupted by user for tool use]",
}

func isClaudeInterrupt(s string) bool {
	s = strings.TrimSpace(s)
	for _, m := range claudeInterruptMarkers {
		if s == m {
			return true
		}
	}
	return false
}

// TranscriptPart is one projectable piece of one transcript line.
//
// A line is not a part: a single assistant message routinely carries a
// thinking block, some prose, and a tool call, and those are three things a
// reader wants separately. Projection is therefore one-line-to-many-parts,
// which is why this is a separate call from ParseTranscriptLine (one line,
// one usage total).
type TranscriptPart struct {
	Kind PartKind

	// Text is the human-readable body: prose, thinking, or a flattened
	// tool result. Empty for a tool call, whose payload is ToolInput.
	Text string

	// ToolName/ToolInput/ToolUseID describe a call; ToolUseID is repeated
	// on the matching result so the two can be paired. Without the pairing
	// a notebook can only show two unrelated blocks.
	ToolName  string
	ToolInput json.RawMessage
	ToolUseID string

	// IsError marks a failed tool result. The model sees these as ordinary
	// results and reacts to them, so they are output, not errors.
	IsError bool

	// Sidechain marks a subagent's turn. Projected but flagged: rendering
	// it inline would interleave two conversations (M6 nests them).
	Sidechain bool

	// Model is set on assistant parts when the line records it.
	Model string

	// UUID is the CLI's own id for the line. It is the idempotency key —
	// the watcher re-reads regions of the file, and cells must not double.
	UUID string

	// ParentUUID is the line this one follows. The transcript is a tree,
	// not a list: two user turns sharing a parent means the first was
	// abandoned and re-sent, and without this the projector would mark a
	// question that was never answered as having succeeded.
	ParentUUID string

	// At is the CLI's timestamp, or the zero time if it was unparseable.
	// A bad timestamp never costs us the turn.
	At time.Time

	// Label names an injection: which hook, which tool, what kind of
	// reminder. It is what makes a list of thirty of them skimmable.
	Label string

	// Size is the injection's full length in bytes, of which Text holds at
	// most injectionExcerptMax. The reader has to be able to tell a
	// one-line reminder from a forty-kilobyte document.
	Size int
}

// ─── Claude Code ────────────────────────────────────────────────────────

// claudeLine is the subset of Claude Code's transcript schema the
// projection reads. Everything else in those lines — requestId, gitBranch,
// file-history deltas, per-iteration usage — is ignored on purpose: each
// field named here is a compatibility commitment, so the list stays as
// short as the feature allows.
type claudeLine struct {
	Type       string          `json:"type"`
	UUID       string          `json:"uuid"`
	ParentUUID string          `json:"parentUuid"`
	Timestamp  string          `json:"timestamp"`
	Sidechain  json.RawMessage `json:"isSidechain"`

	// The flag fields are raw for the same reason the payload fields are:
	// a bool that arrives as a string would otherwise fail the whole line's
	// decode and cost us a real turn. They are read through jsonBool, which
	// treats anything it cannot understand as false — the same direction
	// the filters already fail in.
	//
	// IsMeta marks a line Claude Code wrote *in the user's voice* that the
	// user never typed: command caveats, injected skill bodies, system
	// reminders. SourceToolUseID marks one a tool injected. Both are
	// role:"user" on the wire and neither is a prompt — without this
	// filter a session's document buries the three sentences a person
	// wrote under several thousand lines of machinery.
	IsMeta          json.RawMessage `json:"isMeta"`
	SourceToolUseID string          `json:"sourceToolUseID"`

	// Origin is the explicit provenance mark, and the one that matters
	// most. isMeta does not cover machine-authored user turns: background
	// task notifications arrive as ordinary prompts with `origin.kind =
	// "task-notification"` and run to tens of kilobytes each. Found by
	// projecting a real 11k-line transcript, not by reading the format.
	Origin *claudeOrigin `json:"origin"`

	// IsCompactSummary marks the summary written when the CLI compacts its
	// own context. Compaction appends rather than rewriting, so this is a
	// turn like any other — just not one the user typed.
	IsCompactSummary json.RawMessage `json:"isCompactSummary"`

	Message    *claudeMessage    `json:"message"`
	Attachment *claudeAttachment `json:"attachment"`
}

// claudeAttachment is hook output and other harness-authored context. It
// arrives on its own line type rather than as a message.
type claudeAttachment struct {
	Type     string `json:"type"`
	HookName string `json:"hookName"`
	Filename string `json:"filename"`

	// The payload arrives under a different key per type, so all of them
	// are read and the first non-empty one wins. Surveyed against a real
	// session: `content` (hooks, skill listings, the todo list), `stdout`
	// (hook output), `snippet` (a file just edited), `prompt` (a queued
	// command), `text` (reminders).
	//
	// They are raw because `content` is polymorphic — a string for a hook,
	// an array for injected context, an object for a file reference — and
	// declaring it as a string made json.Unmarshal fail on the *entire
	// line*. 79 injections vanished that way, including the preamble that
	// shaped every turn beneath it. A field whose type surprises us must
	// cost us that field, never the line.
	Content json.RawMessage `json:"content"`
	Stdout  json.RawMessage `json:"stdout"`
	Snippet json.RawMessage `json:"snippet"`
	Prompt  json.RawMessage `json:"prompt"`
	Text    json.RawMessage `json:"text"`
}

// bookkeepingAttachments are attachment types that carry no context, only
// the harness telling the model about itself.
//
// Exactly one entry, and it earns its place with a number: a single
// 3,548-line session carried 671 copies of
// `<total_tokens>N tokens left</total_tokens>`, one per turn. Recording
// them would bury the skill body and the hook instructions that actually
// explain what an agent did — and a list nobody can find anything in fails
// for the same reason as recording nothing at all.
//
// The list stays this short on purpose. A blocklist that grows by
// guesswork drifts back into hiding things, which is what #47 set out to
// stop.
var bookkeepingAttachments = map[string]bool{
	"total_tokens_reminder": true,
}

// payload returns the attachment's content and a label for it.
func (a claudeAttachment) payload() (body, label string) {
	for _, candidate := range []json.RawMessage{a.Content, a.Stdout, a.Snippet, a.Prompt, a.Text} {
		if flat := flattenJSONText(candidate); strings.TrimSpace(flat) != "" {
			body = flat
			break
		}
	}
	switch {
	case a.HookName != "":
		label = "hook: " + a.HookName
	case a.Filename != "":
		label = "edited: " + a.Filename
	case a.Type != "":
		label = strings.ReplaceAll(a.Type, "_", " ")
	default:
		label = "context"
	}
	return body, label
}

type claudeOrigin struct {
	Kind string `json:"kind"`
}

// typedByAHuman decides whether a user-role line is a prompt someone
// actually wrote. It fails *closed*: an origin kind we have never seen is
// machinery until proven otherwise. Failing open would mean the next
// injection format Claude Code invents silently becomes the loudest thing
// in every document, which is how the 47 KB "prompt" got in.
func (l claudeLine) typedByAHuman() bool {
	if jsonBool(l.IsMeta) || l.SourceToolUseID != "" || jsonBool(l.IsCompactSummary) {
		return false
	}
	if l.Origin != nil {
		return l.Origin.Kind == "human"
	}
	return true // predates the field; the filters above still apply
}

type claudeMessage struct {
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content is a string for a plain typed prompt and a block list
	// otherwise, so it is decoded twice rather than modelled as `any`.
	Content json.RawMessage `json:"content"`
}

type claudeBlock struct {
	Type string `json:"type"`

	Text     string `json:"text"`
	Thinking string `json:"thinking"`

	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// ProjectTranscriptLine turns one Claude Code transcript line into the
// parts a notebook would render.
func (a *claudeAdapter) ProjectTranscriptLine(raw []byte) ([]TranscriptPart, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	var line claudeLine
	if err := json.Unmarshal(raw, &line); err != nil {
		return nil, nil // a partial write, or a shape we do not know
	}

	// An attachment is hook output: context the model read that nobody
	// typed. Recorded rather than dropped (see PartInjection).
	if line.Type == "attachment" && line.Attachment != nil {
		if bookkeepingAttachments[line.Attachment.Type] {
			return nil, nil
		}
		body, label := line.Attachment.payload()
		if strings.TrimSpace(body) == "" {
			return nil, nil
		}
		return []TranscriptPart{injectionPart(line, label, body)}, nil
	}

	if line.Message == nil || len(line.Message.Content) == 0 {
		return nil, nil
	}

	// Everything else that is neither user nor assistant is bookkeeping —
	// turn durations, editor state, queue operations, titles. It was never
	// in the model's context, and recording it would bury what was.
	switch line.Type {
	case "user", "assistant":
	default:
		return nil, nil
	}

	base := TranscriptPart{
		Sidechain:  jsonBool(line.Sidechain),
		Model:      line.Message.Model,
		UUID:       line.UUID,
		ParentUUID: line.ParentUUID,
		At:         parseClaudeTime(line.Timestamp),
	}

	// A user line whose content is a bare string is a typed prompt — the
	// only shape that reaches this point and is unambiguously a person.
	var s string
	if json.Unmarshal(line.Message.Content, &s) == nil {
		if line.Type != "user" || strings.TrimSpace(s) == "" {
			return nil, nil
		}
		switch {
		case isClaudeInterrupt(s):
			base.Kind = PartInterrupted
			return []TranscriptPart{base}, nil
		case jsonBool(line.IsCompactSummary):
			base.Kind = PartCompactSummary
		case line.typedByAHuman():
			base.Kind = PartUserText
		default:
			return []TranscriptPart{injectionPart(line, line.injectionLabel(s), s)}, nil
		}
		base.Text = s
		return []TranscriptPart{base}, nil
	}

	var blocks []claudeBlock
	if err := json.Unmarshal(line.Message.Content, &blocks); err != nil {
		return nil, nil
	}

	var out []TranscriptPart
	for _, b := range blocks {
		p := base
		switch b.Type {
		case "text":
			// Injected user text wears the same block type as a typed
			// prompt; only the line-level flags tell them apart.
			if line.Type == "user" {
				switch {
				case isClaudeInterrupt(b.Text):
					p.Kind = PartInterrupted
					p.Text = ""
					out = append(out, p)
					continue
				case jsonBool(line.IsCompactSummary):
					p.Kind = PartCompactSummary
				case line.typedByAHuman():
					p.Kind = PartUserText
				default:
					if strings.TrimSpace(b.Text) != "" {
						out = append(out, injectionPart(line, line.injectionLabel(b.Text), b.Text))
					}
					continue
				}
			} else {
				p.Kind = PartAssistantText
			}
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			p.Text = b.Text

		case "thinking":
			// Adaptive thinking emits blocks whose text is redacted away,
			// leaving a signature we cannot render. A permanently empty
			// disclosure widget is worse than no widget.
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			p.Kind = PartThinking
			p.Text = b.Thinking

		case "tool_use":
			if b.Name == "" {
				continue
			}
			p.Kind = PartToolCall
			p.ToolName = b.Name
			p.ToolUseID = b.ID
			p.ToolInput = b.Input

		case "tool_result":
			if b.ToolUseID == "" {
				continue // unpairable; it would render as an orphan
			}
			p.Kind = PartToolResult
			p.ToolUseID = b.ToolUseID
			p.IsError = b.IsError
			p.Text = flattenClaudeResult(b.Content)

		default:
			continue // a block type this version does not know
		}
		out = append(out, p)
	}
	return out, nil
}

// flattenClaudeResult reduces a tool result's content to text. It arrives
// as a bare string, or as the same block list a message uses (images
// included, which have no text and are dropped for now).
func flattenClaudeResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(blk.Text)
	}
	return b.String()
}

// flattenJSONText pulls readable text out of a value whose shape we do not
// control: a string, an array of strings or blocks, or an object with text
// somewhere inside it. Anything it cannot read becomes empty rather than an
// error, because the alternative — refusing the line — is how 79
// injections went missing.
//
// Depth-bounded: these are other people's structures and a pathological
// one must not cost us a stack.
func flattenJSONText(raw json.RawMessage) string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var b strings.Builder
	collectJSONText(v, &b, 0)
	out := strings.TrimSpace(b.String())
	// "[]" is an empty todo list rendered as text — 77 of them in one
	// session. An injection with nothing in it is not an injection.
	if out == "[]" || out == "{}" {
		return ""
	}
	return out
}

const jsonTextMaxDepth = 6

func collectJSONText(v any, b *strings.Builder, depth int) {
	if depth > jsonTextMaxDepth || b.Len() > 4*injectionExcerptMax {
		return
	}
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(t)
	case []any:
		for _, item := range t {
			collectJSONText(item, b, depth+1)
		}
	case map[string]any:
		// Named first so a block reads as its text rather than as its
		// metadata; then everything else, so an unfamiliar shape still
		// yields whatever strings it holds.
		for _, key := range []string{"text", "content", "snippet", "prompt"} {
			if inner, ok := t[key]; ok {
				collectJSONText(inner, b, depth+1)
			}
		}
		for key, inner := range t {
			switch key {
			case "text", "content", "snippet", "prompt", "type", "filePath", "filename":
				continue
			}
			collectJSONText(inner, b, depth+1)
		}
	}
}

// injectionPart builds the bounded record of one injection.
func injectionPart(line claudeLine, label, body string) TranscriptPart {
	return TranscriptPart{
		Kind:       PartInjection,
		Label:      label,
		Text:       injectionExcerpt(body),
		Size:       len(body),
		Sidechain:  jsonBool(line.Sidechain),
		UUID:       line.UUID,
		ParentUUID: line.ParentUUID,
		At:         parseClaudeTime(line.Timestamp),
	}
}

// jsonBool reads a flag that ought to be a bool but might not be. Anything
// unreadable is false, which is the direction the filters already fail in:
// an unrecognised line is machinery until proven otherwise, and a flag we
// cannot parse is not proof.
func jsonBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s == "true" || s == "1"
	}
	return false
}

// injectionExcerpt keeps the *start* of an injection rather than eliding
// its middle. For a tool result the middle matters; for an injection the
// first line is what identifies it, and the rest is why it was excluded
// from the conversation in the first place.
func injectionExcerpt(body string) string {
	body = strings.TrimSpace(body)
	if len(body) <= injectionExcerptMax {
		return body
	}
	cut := runeBoundaryBefore(body, injectionExcerptMax)
	return body[:cut] + "…"
}

// injectionLabel names an injection from what the line says about itself,
// falling back to the shape of the text. Labels are a convenience, not a
// contract: a wrong one costs a reader a moment, where a wrong *filter*
// would cost them the document.
func (l claudeLine) injectionLabel(body string) string {
	if l.Origin != nil && l.Origin.Kind != "" && l.Origin.Kind != "human" {
		return strings.ReplaceAll(l.Origin.Kind, "-", " ")
	}
	trimmed := strings.TrimSpace(body)
	switch {
	case strings.HasPrefix(trimmed, "<local-command-caveat>"):
		return "command caveat"
	case strings.HasPrefix(trimmed, "<system-reminder>"):
		return "system reminder"
	case strings.HasPrefix(trimmed, "<command-name>"):
		return "slash command"
	case l.SourceToolUseID != "":
		return "injected by a tool"
	case jsonBool(l.IsMeta):
		return "harness context"
	}
	return "context"
}

func parseClaudeTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
