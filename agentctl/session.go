package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	ringSize     = 256 * 1024
	activityMax  = 40
	taskHistoryN = 5
)

type ActivityEntry struct {
	T      time.Time `json:"t"`
	Event  string    `json:"event"`
	Tool   string    `json:"tool,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Level  string    `json:"level,omitempty"` // info | warn | error
}

type ApprovalRequest struct {
	Message   string         `json:"message"`
	Tool      string         `json:"tool,omitempty"`
	ToolInput map[string]any `json:"toolInput,omitempty"`
	At        time.Time      `json:"at"`
}

// MenuOption is one item in a numbered TUI menu detected from PTY output.
type MenuOption struct {
	Key       string `json:"key"`   // "1", "2", ...
	Label     string `json:"label"` // display text with ANSI stripped
	Highlight bool   `json:"highlight,omitempty"`
}

// AskUserQuestion-tool payload, parsed from tool_input at PreToolUse time.
// One tool call can carry multiple questions; we track them all.
type AskOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type AskQuestionItem struct {
	Question    string      `json:"question"`
	Header      string      `json:"header,omitempty"`
	MultiSelect bool        `json:"multiSelect,omitempty"`
	Options     []AskOption `json:"options"`
}

type AskQuestionRequest struct {
	Questions []AskQuestionItem `json:"questions"`
	At        time.Time         `json:"at"`
}

type Session struct {
	ID             string
	SessionID      string
	Cwd            string
	Prompt         string
	Cmd            *exec.Cmd
	PTY            *os.File
	SettingsDir    string
	Status         string
	LastActivity   string
	CurrentTask    string
	LastTool       string
	TranscriptPath string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ToolCounts     map[string]int
	TaskHistory    []string
	Activity       []ActivityEntry

	// Token totals populated by the transcript watcher (transcript.go).
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	MessageCount        int

	// Pending permission prompt, if any. Cleared on next non-approval event.
	Pending *ApprovalRequest

	// MenuOptions is populated by the PTY output scanner (transcript.go)
	// when a numbered selection menu is visible in the terminal.
	MenuOptions []MenuOption

	// AskQuestion is populated when PreToolUse fires with tool AskUserQuestion,
	// cleared on the matching PostToolUse. Structured (from tool_input) so we
	// don't need to scrape it from the TUI.
	AskQuestion *AskQuestionRequest

	// Most recent PreToolUse — used to enrich a following permission prompt
	// with the tool name + tool_input so the UI can show what's proposed.
	LastPreToolName  string
	LastPreToolInput map[string]any
	LastPreToolAt    time.Time

	// Transcript watcher bookkeeping.
	transcriptOffset int64
	watching         bool

	mu       sync.Mutex
	ring     []byte
	ringLen  int
	ringHead int
	subs     map[*websocket.Conn]bool
	subMu    sync.Mutex
	closed   bool
}

func newSession(id, sid, cwd, prompt string) *Session {
	now := time.Now()
	s := &Session{
		ID:          id,
		SessionID:   sid,
		Cwd:         cwd,
		Prompt:      prompt,
		Status:      "starting",
		CreatedAt:   now,
		UpdatedAt:   now,
		ToolCounts:  make(map[string]int),
		TaskHistory: nil,
		Activity:    nil,
		ring:        make([]byte, ringSize),
		subs:        make(map[*websocket.Conn]bool),
	}
	if prompt != "" {
		s.CurrentTask = prompt
		s.TaskHistory = []string{prompt}
	}
	return s
}

func (s *Session) writeRing(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range p {
		s.ring[s.ringHead] = b
		s.ringHead = (s.ringHead + 1) % ringSize
		if s.ringLen < ringSize {
			s.ringLen++
		}
	}
}

func (s *Session) snapshotRing() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]byte, s.ringLen)
	if s.ringLen < ringSize {
		copy(out, s.ring[:s.ringLen])
		return out
	}
	tail := s.ringHead
	copy(out, s.ring[tail:])
	copy(out[ringSize-tail:], s.ring[:tail])
	return out
}

func (s *Session) addSub(c *websocket.Conn) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.subs[c] = true
}

func (s *Session) removeSub(c *websocket.Conn) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	delete(s.subs, c)
}

func (s *Session) broadcastBytes(p []byte) {
	s.subMu.Lock()
	conns := make([]*websocket.Conn, 0, len(s.subs))
	for c := range s.subs {
		conns = append(conns, c)
	}
	s.subMu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(websocket.BinaryMessage, p)
	}
}

func (s *Session) closeSubs() {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for c := range s.subs {
		_ = c.Close()
	}
	s.subs = make(map[*websocket.Conn]bool)
}

func (s *Session) appendActivity(e ActivityEntry) {
	s.mu.Lock()
	e.T = time.Now()
	s.Activity = append(s.Activity, e)
	if len(s.Activity) > activityMax {
		s.Activity = s.Activity[len(s.Activity)-activityMax:]
	}
	s.UpdatedAt = e.T
	s.mu.Unlock()
}

func (s *Session) recordTool(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	s.ToolCounts[name]++
	s.LastTool = name
	s.mu.Unlock()
}

func (s *Session) setPending(msg string) {
	s.mu.Lock()
	req := &ApprovalRequest{Message: msg, At: time.Now()}
	// If PreToolUse fired within the last 5 seconds, this permission prompt
	// is almost certainly for that tool call — surface the specifics.
	if !s.LastPreToolAt.IsZero() && time.Since(s.LastPreToolAt) < 5*time.Second {
		req.Tool = s.LastPreToolName
		req.ToolInput = s.LastPreToolInput
	}
	s.Pending = req
	s.mu.Unlock()
}

func (s *Session) setAskQuestion(q *AskQuestionRequest) {
	s.mu.Lock()
	s.AskQuestion = q
	s.mu.Unlock()
}

func (s *Session) clearAskQuestion() {
	s.mu.Lock()
	s.AskQuestion = nil
	s.mu.Unlock()
}

func (s *Session) getMenuOptions() []MenuOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]MenuOption, len(s.MenuOptions))
	copy(out, s.MenuOptions)
	return out
}

func (s *Session) setMenuOptions(opts []MenuOption) {
	s.mu.Lock()
	s.MenuOptions = opts
	s.mu.Unlock()
}

func (s *Session) recordPreTool(name string, input map[string]any) {
	s.mu.Lock()
	s.LastPreToolName = name
	s.LastPreToolInput = input
	s.LastPreToolAt = time.Now()
	s.mu.Unlock()
}

func (s *Session) clearPending() {
	s.mu.Lock()
	s.Pending = nil
	s.mu.Unlock()
}

func (s *Session) pushTask(prompt string) {
	if prompt == "" {
		return
	}
	s.mu.Lock()
	s.CurrentTask = prompt
	s.TaskHistory = append(s.TaskHistory, prompt)
	if len(s.TaskHistory) > taskHistoryN {
		s.TaskHistory = s.TaskHistory[len(s.TaskHistory)-taskHistoryN:]
	}
	s.mu.Unlock()
}

func (s *Session) toJSON() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	activity := make([]ActivityEntry, len(s.Activity))
	copy(activity, s.Activity)
	toolCounts := make(map[string]int, len(s.ToolCounts))
	for k, v := range s.ToolCounts {
		toolCounts[k] = v
	}
	history := make([]string, len(s.TaskHistory))
	copy(history, s.TaskHistory)
	var pending any
	if s.Pending != nil {
		m := map[string]any{
			"message": s.Pending.Message,
			"at":      s.Pending.At.Format(time.RFC3339),
		}
		if s.Pending.Tool != "" {
			m["tool"] = s.Pending.Tool
		}
		if len(s.Pending.ToolInput) > 0 {
			m["toolInput"] = s.Pending.ToolInput
		}
		pending = m
	}
	return map[string]any{
		"id":                  s.ID,
		"sessionId":           s.SessionID,
		"cwd":                 s.Cwd,
		"prompt":              s.Prompt,
		"status":              s.Status,
		"lastActivity":        s.LastActivity,
		"lastTool":            s.LastTool,
		"currentTask":         s.CurrentTask,
		"taskHistory":         history,
		"toolCounts":          toolCounts,
		"activity":            activity,
		"transcriptPath":      s.TranscriptPath,
		"pending":             pending,
		"menuOptions":         append([]MenuOption(nil), s.MenuOptions...),
		"askQuestion":         s.AskQuestion,
		"inputTokens":         s.InputTokens,
		"outputTokens":        s.OutputTokens,
		"cacheReadTokens":     s.CacheReadTokens,
		"cacheCreationTokens": s.CacheCreationTokens,
		"messageCount":        s.MessageCount,
		"createdAt":           s.CreatedAt.Format(time.RFC3339),
		"updatedAt":           s.UpdatedAt.Format(time.RFC3339),
	}
}

var (
	registryMu     sync.RWMutex
	registry       = map[string]*Session{}
	sessionToAgent = map[string]string{}
)

func registerSession(s *Session) {
	registryMu.Lock()
	registry[s.ID] = s
	sessionToAgent[s.SessionID] = s.ID
	registryMu.Unlock()
	broadcastDashboard(map[string]any{"type": "upsert", "agent": s.toJSON()})
}

func removeSession(id string) {
	registryMu.Lock()
	s, ok := registry[id]
	if ok {
		delete(registry, id)
		delete(sessionToAgent, s.SessionID)
		if s.SettingsDir != "" {
			_ = os.RemoveAll(s.SettingsDir)
		}
	}
	registryMu.Unlock()
	if ok {
		broadcastDashboard(map[string]any{"type": "remove", "id": id})
	}
}

func getSession(id string) *Session {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[id]
}

func getSessionBySID(sid string) *Session {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if aid, ok := sessionToAgent[sid]; ok {
		return registry[aid]
	}
	return nil
}

func allSessionsJSON() []map[string]any {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]map[string]any, 0, len(registry))
	for _, s := range registry {
		out = append(out, s.toJSON())
	}
	return out
}

// setStatus updates status/lastActivity and broadcasts the full refreshed
// agent JSON to dashboard subscribers so the UI always has the richest state.
func (s *Session) setStatus(status, activity string) {
	s.mu.Lock()
	s.Status = status
	if activity != "" {
		s.LastActivity = activity
	}
	s.UpdatedAt = time.Now()
	s.mu.Unlock()
	broadcastDashboard(map[string]any{
		"type":  "upsert",
		"agent": s.toJSON(),
	})
}

// touch broadcasts a fresh snapshot without changing status — used when the
// activity log or task list changed but status stayed put.
func (s *Session) touch() {
	broadcastDashboard(map[string]any{
		"type":  "upsert",
		"agent": s.toJSON(),
	})
}

// dashboard subscribers
var (
	dashMu   sync.Mutex
	dashSubs = map[*websocket.Conn]bool{}
)

func addDashSub(c *websocket.Conn) {
	dashMu.Lock()
	dashSubs[c] = true
	dashMu.Unlock()
}

func removeDashSub(c *websocket.Conn) {
	dashMu.Lock()
	delete(dashSubs, c)
	dashMu.Unlock()
}

func broadcastDashboard(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	dashMu.Lock()
	conns := make([]*websocket.Conn, 0, len(dashSubs))
	for c := range dashSubs {
		conns = append(conns, c)
	}
	dashMu.Unlock()
	for _, c := range conns {
		_ = c.WriteMessage(websocket.TextMessage, b)
	}
}
