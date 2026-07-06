package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"
)

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		}
		return false
	}
	return true
}

const maxBodyBytes = 1 << 20

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
		if !decodeBody(w, r, &req) {
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
		hookTok := uuid.NewString()
		s := newSession(agentID, sessionID, req.Cwd, req.Prompt)
		s.HookToken = hookTok

		settingsDir, settingsFile, err := writeHookSettings(hookURL(hookBind, hookPort, hookTok))
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
			// Send the literal word — works for y/n prompts and for
			// Claude's ink-select menus (typing filters to items matching
			// "yes", then Enter confirms the highlighted "Yes"). If the
			// prompt is a digit-only ink-select (no filter), the fallback
			// "1\r" kicks in after 1.5s if pending is still set.
			handleAgentAnswer(w, r, s, []string{"yes\r"}, []string{"1\r"})
		case "deny":
			handleAgentAnswer(w, r, s, []string{"no\r"}, []string{"\x1b"})
		case "resize":
			handleAgentResize(w, r, s)
		default:
			http.Error(w, "unknown subpath", http.StatusNotFound)
		}
		return
	}

	switch r.Method {
	case http.MethodDelete:
		if s.Cmd != nil && s.Cmd.Process != nil {
			proc := s.Cmd.Process
			pid := proc.Pid
			pgid, err := syscall.Getpgid(pid)
			if err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGTERM)
			} else {
				_ = proc.Kill()
			}
			time.AfterFunc(3*time.Second, func() {
				if err := proc.Signal(syscall.Signal(0)); err != nil {
					return
				}
				if pgid, err := syscall.Getpgid(pid); err == nil {
					_ = syscall.Kill(-pgid, syscall.SIGKILL)
				} else {
					_ = proc.Kill()
				}
			})
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
	if !decodeBody(w, r, &req) {
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

// handleAgentAnswer writes primary approve/deny keystrokes, then 1.5s later
// checks whether Pending was cleared; if not, writes the fallback keystrokes
// (e.g. "1\r" for approve on digit-only ink-selects, "\x1b" for deny).
func handleAgentAnswer(w http.ResponseWriter, r *http.Request, s *Session, primary, fallback []string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.PTY == nil {
		http.Error(w, "pty not ready", http.StatusServiceUnavailable)
		return
	}
	writeChunks := func(label string, chunks []string) {
		for i, k := range chunks {
			n, err := s.PTY.Write([]byte(k))
			log.Printf("[%s] pty %s chunk %d/%d: n=%d bytes=%s err=%v",
				s.ID, label, i+1, len(chunks), n, hex.EncodeToString([]byte(k)), err)
			if err != nil {
				return
			}
		}
	}
	go func() {
		writeChunks("primary", primary)
		time.Sleep(1500 * time.Millisecond)
		if s.hasPending() {
			log.Printf("[%s] pty answer: pending still set after primary, sending fallback", s.ID)
			writeChunks("fallback", fallback)
		}
	}()
	w.WriteHeader(http.StatusNoContent)
}

type resizeReq struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// handleAgentResize updates the PTY window size so Claude Code re-renders at
// the new dimensions. Without this, resizing the browser panel makes the
// terminal contents garbled — Claude keeps drawing at the original 40×120.
func handleAgentResize(w http.ResponseWriter, r *http.Request, s *Session) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.PTY == nil {
		http.Error(w, "pty not ready", http.StatusServiceUnavailable)
		return
	}
	var req resizeReq
	if !decodeBody(w, r, &req) {
		return
	}
	if req.Cols < 20 || req.Rows < 5 || req.Cols > 500 || req.Rows > 300 {
		http.Error(w, "cols/rows out of range", http.StatusBadRequest)
		return
	}
	if err := pty.Setsize(s.PTY, &pty.Winsize{Rows: req.Rows, Cols: req.Cols}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
