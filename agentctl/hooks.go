package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// hookPayload matches the JSON Claude Code posts to HTTP hooks.
// Unknown fields are ignored; different events populate different subsets.
type hookPayload struct {
	SessionID      string `json:"session_id"`
	HookEventName  string `json:"hook_event_name"`
	ToolName       string `json:"tool_name"`
	TranscriptPath string `json:"transcript_path"`
	Message        string `json:"message"`
	Prompt         string `json:"prompt"`
	Source         string `json:"source"`
	Matcher        string `json:"matcher"`
	Type           string `json:"type"`
	Cwd            string `json:"cwd"`
}

// hookBind/hookPort are populated from main() so the settings generator can
// point Claude back at our /api/hooks endpoint.
var (
	hookBind = "127.0.0.1"
	hookPort = "7317"
)

func handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
		return
	}
	var p hookPayload
	if err := json.Unmarshal(body, &p); err != nil {
		http.Error(w, "json: "+err.Error(), http.StatusBadRequest)
		return
	}

	s := getSessionBySID(p.SessionID)
	if s == nil {
		log.Printf("hook: unknown session_id=%s event=%s", p.SessionID, p.HookEventName)
		w.WriteHeader(http.StatusOK)
		return
	}
	if p.TranscriptPath != "" && s.TranscriptPath == "" {
		s.mu.Lock()
		s.TranscriptPath = p.TranscriptPath
		s.mu.Unlock()
		startTranscriptWatcher(s)
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
		s.appendActivity(ActivityEntry{Event: "PreToolUse", Tool: p.ToolName, Level: "info"})
		s.setStatus("running", "→ "+p.ToolName)

	case "PostToolUse":
		s.appendActivity(ActivityEntry{Event: "PostToolUse", Tool: p.ToolName, Level: "info"})
		s.setStatus("running", "✓ "+p.ToolName)

	case "PostToolUseFailure":
		s.appendActivity(ActivityEntry{Event: "PostToolUseFailure", Tool: p.ToolName, Level: "error"})
		s.setStatus("error", "✗ "+p.ToolName)

	case "Notification":
		matcher := p.Matcher
		if matcher == "" {
			matcher = p.Type
		}
		switch matcher {
		case "permission_prompt":
			s.setPending(p.Message)
			s.appendActivity(ActivityEntry{Event: "PermissionPrompt", Detail: p.Message, Level: "warn"})
			s.setStatus("waiting_input", "permission prompt")
		case "idle_prompt":
			s.appendActivity(ActivityEntry{Event: "IdlePrompt", Detail: p.Message, Level: "info"})
			s.setStatus("idle", "idle prompt")
		default:
			s.appendActivity(ActivityEntry{Event: "Notification", Detail: p.Message, Level: "warn"})
			s.setStatus("waiting_input", truncate(p.Message, 80))
		}

	case "Stop":
		s.appendActivity(ActivityEntry{Event: "Stop", Level: "info"})
		s.setStatus("idle", "turn complete")

	case "SessionEnd":
		s.appendActivity(ActivityEntry{Event: "SessionEnd", Level: "info"})
		s.setStatus("stopped", "session ended")

	default:
		s.appendActivity(ActivityEntry{Event: p.HookEventName, Level: "info"})
		s.touch()
	}

	w.WriteHeader(http.StatusOK)
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
