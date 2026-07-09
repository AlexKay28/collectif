package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// hookPayload matches the JSON Claude Code posts to HTTP hooks.
// Unknown fields are ignored; different events populate different subsets.
type hookPayload struct {
	SessionID      string         `json:"session_id"`
	HookEventName  string         `json:"hook_event_name"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
	ToolResponse   map[string]any `json:"tool_response"` // #37 PR-ready: PostToolUse response (stdout, exit_code)
	TranscriptPath string         `json:"transcript_path"`
	Message        string         `json:"message"`
	Prompt         string         `json:"prompt"`
	Source         string         `json:"source"`
	Matcher        string         `json:"matcher"`
	Type           string         `json:"type"`
	Cwd            string         `json:"cwd"`
}

// hookBind/hookPort are populated from main() so the settings generator can
// point Claude back at our /api/hooks endpoint.
var (
	hookBind = "127.0.0.1"
	hookPort = "7317"
)

func handleHook(w http.ResponseWriter, r *http.Request) {
	hooksReceived.Add(1)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ht := r.URL.Query().Get("ht")
	if ht == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s := getSessionByHookToken(ht)
	if s == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
		}
		return
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "json: "+err.Error(), http.StatusBadRequest)
		return
	}

	if p.SessionID != "" && p.SessionID != s.SessionID {
		log.Printf("hook: session_id mismatch ht=%s got=%s want=%s", ht, p.SessionID, s.SessionID)
	}
	if p.TranscriptPath != "" && s.TranscriptPath == "" {
		s.mu.Lock()
		s.TranscriptPath = p.TranscriptPath
		s.mu.Unlock()
		startTranscriptWatcher(s.ctx, s)
	}

	// Any event that isn't a fresh permission_prompt clears the pending state
	// — the user has moved on (either by answering in the terminal or the
	// agent proceeding on its own).
	clearPendingUnlessApproval(s, p)

	switch p.HookEventName {
	case "SessionStart":
		src := p.Source
		if src == "" {
			src = "started"
		}
		s.appendActivity(ActivityEntry{Event: "SessionStart", Detail: src, Level: "info"})
		s.setStatus("running", "session "+src)

	case "UserPromptSubmit":
		s.pushTask(p.Prompt)
		s.appendActivity(ActivityEntry{Event: "UserPrompt", Detail: truncate(p.Prompt, 200), Level: "info"})
		s.setStatus("running", "new prompt")

	case "PreToolUse":
		s.recordTool(p.ToolName)
		s.recordPreTool(p.ToolName, p.ToolInput)
		// Set structured state BEFORE setStatus so the broadcast that
		// setStatus emits already carries the fresh askQuestion.
		if p.ToolName == "AskUserQuestion" {
			if q := parseAskQuestion(p.ToolInput); q != nil {
				s.setAskQuestion(q)
			}
		}
		s.appendActivity(ActivityEntry{Event: "PreToolUse", Tool: p.ToolName, Level: "info"})
		s.setStatus("running", "→ "+p.ToolName)

	case "PostToolUse":
		if p.ToolName == "AskUserQuestion" {
			s.clearAskQuestion()
		}
		s.appendActivity(ActivityEntry{Event: "PostToolUse", Tool: p.ToolName, Level: "info"})
		s.setStatus("running", "✓ "+p.ToolName)
		// #37 PR-ready detection (path A): tool-use signal — a Bash call to
		// `gh pr create` that exited 0 marks the session as review_ready.
		if p.ToolName == "Bash" {
			handleBashPostToolUse(s, p.ToolInput, p.ToolResponse)
		}

	case "PostToolUseFailure":
		s.appendActivity(ActivityEntry{Event: "PostToolUseFailure", Tool: p.ToolName, Level: "error"})
		s.setStatus("error", "✗ "+p.ToolName)

	case "Notification":
		// Claude Code fires Notification whenever it wants user attention —
		// permission prompts, idle timeouts, or arbitrary agent messages.
		// The type/matcher field varies by version, so classify from the
		// message text itself.
		msg := stripAnsi(p.Message)
		low := strings.ToLower(msg)
		isIdle := strings.Contains(low, "idle") ||
			strings.Contains(low, "still there") ||
			strings.Contains(low, "waiting for you to")
		if isIdle {
			s.appendActivity(ActivityEntry{Event: "IdlePrompt", Detail: msg, Level: "info"})
			s.setStatus("idle", "idle prompt")
		} else {
			s.setPending(msg)
			s.appendActivity(ActivityEntry{Event: "PermissionPrompt", Detail: msg, Level: "warn"})
			s.setStatus("waiting_input", truncate(msg, 80))
		}

	case "Stop":
		s.appendActivity(ActivityEntry{Event: "Stop", Level: "info"})
		s.setStatus("idle", "turn complete")

	case "SessionEnd":
		s.appendActivity(ActivityEntry{Event: "SessionEnd", Level: "info"})
		s.setStatus("stopped", "session ended")

	case "SubagentStop":
		s.appendActivity(ActivityEntry{Event: "SubagentStop", Level: "info"})
		s.touch()

	case "PreCompact":
		s.appendActivity(ActivityEntry{Event: "PreCompact", Detail: p.Matcher, Level: "info"})
		s.touch()

	default:
		s.appendActivity(ActivityEntry{Event: p.HookEventName, Level: "info"})
		s.touch()
	}

	w.WriteHeader(http.StatusOK)
}

// parseAskQuestion pulls the questions[]/options structure out of the
// AskUserQuestion tool_input. Defensive: the schema might drift, so every
// step is type-checked instead of blindly cast.
func parseAskQuestion(input map[string]any) *AskQuestionRequest {
	if input == nil {
		return nil
	}
	qs, ok := input["questions"].([]any)
	if !ok || len(qs) == 0 {
		return nil
	}
	req := &AskQuestionRequest{At: time.Now()}
	for _, qi := range qs {
		q, ok := qi.(map[string]any)
		if !ok {
			continue
		}
		item := AskQuestionItem{
			Question:    getStr(q, "question"),
			Header:      getStr(q, "header"),
			MultiSelect: getBool(q, "multiSelect"),
		}
		if opts, ok := q["options"].([]any); ok {
			for _, oi := range opts {
				om, ok := oi.(map[string]any)
				if !ok {
					continue
				}
				item.Options = append(item.Options, AskOption{
					Label:       getStr(om, "label"),
					Description: getStr(om, "description"),
				})
			}
		}
		if item.Question != "" && len(item.Options) > 0 {
			req.Questions = append(req.Questions, item)
		}
	}
	if len(req.Questions) == 0 {
		return nil
	}
	return req
}

func getStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func getBool(m map[string]any, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func clearPendingUnlessApproval(s *Session, p hookPayload) {
	if p.HookEventName == "Notification" {
		matcher := p.Matcher
		if matcher == "" {
			matcher = p.Type
		}
		if matcher == "permission_prompt" {
			return
		}
	}
	s.mu.Lock()
	had := s.Pending != nil
	if had {
		s.Pending = nil
	}
	s.mu.Unlock()
}
