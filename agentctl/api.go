package main

import (
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

type spawnReq struct {
	Cwd    string `json:"cwd"`
	Prompt string `json:"prompt"`
}

func handleAgents(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, allSessionsJSON())
	case http.MethodPost:
		var req spawnReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Cwd == "" {
			http.Error(w, "cwd required", http.StatusBadRequest)
			return
		}
		if st, err := os.Stat(req.Cwd); err != nil || !st.IsDir() {
			http.Error(w, "cwd is not a directory", http.StatusBadRequest)
			return
		}

		agentID := uuid.NewString()
		sessionID := uuid.NewString()
		s := newSession(agentID, sessionID, req.Cwd, req.Prompt)

		settingsDir, settingsFile, err := writeHookSettings(hookURL(hookBind, hookPort))
		if err != nil {
			http.Error(w, "settings gen: "+err.Error(), http.StatusInternalServerError)
			return
		}
		s.SettingsDir = settingsDir

		registerSession(s)
		if err := spawnClaude(s, settingsFile, req.Prompt); err != nil {
			removeSession(agentID)
			http.Error(w, "spawn: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"agentID": agentID, "sessionID": sessionID})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleAgentByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	if rest == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	id := rest
	subpath := ""
	if i := strings.Index(rest, "/"); i > 0 {
		id = rest[:i]
		subpath = rest[i+1:]
	}
	s := getSession(id)
	if s == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	if subpath != "" {
		switch subpath {
		case "input":
			handleAgentInput(w, r, s)
		case "approve":
			// Claude Code's permission menu is a select list — press "1"
			// (highlights + confirms "Yes" in most builds), then \r as
			// a belt-and-braces confirm for prompts that need Enter.
			handleAgentTimedKeys(w, r, s, []string{"1", "\r"}, 120*time.Millisecond)
		case "deny":
			// Escape is the universal cancel across Claude Code's TUI prompts.
			handleAgentTimedKeys(w, r, s, []string{"\x1b"}, 0)
		default:
			http.Error(w, "unknown subpath", http.StatusNotFound)
		}
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if s.Cmd != nil && s.Cmd.Process != nil {
			// Kill the whole process group; claude may spawn children.
			pgid, err := syscall.Getpgid(s.Cmd.Process.Pid)
			if err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
			} else {
				_ = s.Cmd.Process.Kill()
			}
		}
		s.setStatus("stopped", "killed")
		s.closeSubs()
		removeSession(id)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.toJSON())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type inputReq struct {
	Data string `json:"data"`
}

func handleAgentInput(w http.ResponseWriter, r *http.Request, s *Session) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.PTY == nil {
		http.Error(w, "pty not ready", http.StatusServiceUnavailable)
		return
	}
	var req inputReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	n, err := s.PTY.Write([]byte(req.Data))
	log.Printf("[%s] pty input via /input: n=%d bytes=%s err=%v", s.ID, n, hex.EncodeToString([]byte(req.Data)), err)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentTimedKeys writes a sequence of key chunks to the PTY with an
// optional delay between them (mimics human typing so TUIs settle between
// select + confirm). Every write is logged with its hex bytes so we can
// diagnose approve/deny failures.
func handleAgentTimedKeys(w http.ResponseWriter, r *http.Request, s *Session, chunks []string, gap time.Duration) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.PTY == nil {
		http.Error(w, "pty not ready", http.StatusServiceUnavailable)
		return
	}
	go func() {
		for i, k := range chunks {
			if i > 0 && gap > 0 {
				time.Sleep(gap)
			}
			n, err := s.PTY.Write([]byte(k))
			log.Printf("[%s] pty answer chunk %d/%d: n=%d bytes=%s err=%v",
				s.ID, i+1, len(chunks), n, hex.EncodeToString([]byte(k)), err)
			if err != nil {
				return
			}
		}
	}()
	s.clearPending()
	s.touch()
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
