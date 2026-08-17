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
)

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
}

// ─── Claude Code ────────────────────────────────────────────────────────

// claudeLine is the subset of Claude Code's transcript schema the
// projection reads. Everything else in those lines — requestId, gitBranch,
// file-history deltas, per-iteration usage — is ignored on purpose: each
// field named here is a compatibility commitment, so the list stays as
// short as the feature allows.
type claudeLine struct {
	Type       string `json:"type"`
	UUID       string `json:"uuid"`
	ParentUUID string `json:"parentUuid"`
	Timestamp  string `json:"timestamp"`
	Sidechain  bool   `json:"isSidechain"`

	// IsMeta marks a line Claude Code wrote *in the user's voice* that the
	// user never typed: command caveats, injected skill bodies, system
	// reminders. SourceToolUseID marks one a tool injected. Both are
	// role:"user" on the wire and neither is a prompt — without this
	// filter a session's document buries the three sentences a person
	// wrote under several thousand lines of machinery.
	IsMeta          bool   `json:"isMeta"`
	SourceToolUseID string `json:"sourceToolUseID"`

	// Origin is the explicit provenance mark, and the one that matters
	// most. isMeta does not cover machine-authored user turns: background
	// task notifications arrive as ordinary prompts with `origin.kind =
	// "task-notification"` and run to tens of kilobytes each. Found by
	// projecting a real 11k-line transcript, not by reading the format.
	Origin *claudeOrigin `json:"origin"`

	// IsCompactSummary marks the summary written when the CLI compacts its
	// own context. Compaction appends rather than rewriting, so this is a
	// turn like any other — just not one the user typed.
	IsCompactSummary bool `json:"isCompactSummary"`

	Message *claudeMessage `json:"message"`
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
	if l.IsMeta || l.SourceToolUseID != "" || l.IsCompactSummary {
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
	if line.Message == nil || len(line.Message.Content) == 0 {
		return nil, nil
	}

	// Types other than user/assistant are session machinery: hook output
	// (`attachment`), turn bookkeeping (`system`), editor state
	// (`file-history-*`), queue operations, titles.
	switch line.Type {
	case "user", "assistant":
	default:
		return nil, nil
	}

	base := TranscriptPart{
		Sidechain:  line.Sidechain,
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
		case line.IsCompactSummary:
			base.Kind = PartCompactSummary
		case line.typedByAHuman():
			base.Kind = PartUserText
		default:
			return nil, nil
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
				case line.IsCompactSummary:
					p.Kind = PartCompactSummary
				case line.typedByAHuman():
					p.Kind = PartUserText
				default:
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
