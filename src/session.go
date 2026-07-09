package main

// Locking model
//
// Session has three distinct locks and one immutable-after-publish contract:
//
//   - s.mu (sync.Mutex): covers most mutable Session fields — Status,
//     LastActivity, CurrentTask, LastTool, ToolCounts, TaskHistory,
//     Activity, Pending, MenuOptions, AskQuestion, LastPreTool*, token
//     totals, MessageCount, UpdatedAt, and the ring buffer state.
//
//   - Cmd, PTY, SettingsDir, and HookToken are set ONCE — in newSession or
//     spawnClaude — before the session is published to the package-level
//     registry, and are then treated as immutable. They are read through the
//     pty() / cmd() accessors, which take s.mu to establish a
//     happens-before edge with the writer in spawnClaude (see issue #3).
//     Direct field reads are avoided to keep the race detector quiet and
//     to make the ownership boundary obvious.
//
//   - s.subMu (sync.Mutex): guards s.subs ONLY. It is never held while
//     s.mu is held (and vice versa) — the two mutexes are strictly
//     independent. Keep it that way to avoid nested-lock deadlocks.
//
//   - registryMu (package-level, sync.RWMutex): guards the registry,
//     sessionToAgent, and hookToAgent maps. Lock ordering is
//     registryMu -> s.mu, NEVER the reverse. Do not call any method that
//     takes s.mu (or s.subMu) while holding registryMu for write; readers
//     that need per-session state should snapshot *Session pointers under
//     the RLock and then release before touching session methods.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const pendingTTL = 5 * time.Minute

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
	ExpiresAt time.Time      `json:"expiresAt"`
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
	HookToken      string
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

	// ── #37 PR-ready ──────────────────────────────────────────────────
	// Populated when the session opens a GitHub PR (either via a
	// `gh pr create` PostToolUse hook or via the git-state poller). When
	// PRURL is non-empty and Status == "review_ready" the dashboard's
	// Review queue surfaces this session. Cleared by the /reviewed
	// endpoint. See src/prdetect.go.
	PRURL   string
	PRTitle string
	// ──────────────────────────────────────────────────────────────────

	// Transcript watcher bookkeeping.
	transcriptOffset int64
	watching         bool

	// ctx / cancel stop the per-session background workers (menu detector,
	// transcript watcher). Set once when the session is created.
	ctx    context.Context
	cancel context.CancelFunc

	mu         sync.Mutex
	ring       []byte
	ringLen    int
	ringHead   int
	ringSerial uint64
	subs       map[*wsSub]bool
	subMu      sync.Mutex
}

func newSession(id, sid, cwd, prompt string) *Session {
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
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
		subs:        make(map[*wsSub]bool),
		ctx:         ctx,
		cancel:      cancel,
	}
	if prompt != "" {
		s.CurrentTask = prompt
		s.TaskHistory = []string{prompt}
	}
	return s
}

// pty returns the session's PTY handle under s.mu. PTY is set once in
// spawnClaude and then treated as immutable; the lock here just establishes
// the happens-before edge with that writer (see issue #3).
func (s *Session) pty() *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.PTY
}

// cmd returns the session's *exec.Cmd under s.mu. Same contract as pty().
func (s *Session) cmd() *exec.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Cmd
}

func (s *Session) writeRing(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	if n >= ringSize {
		p = p[n-ringSize:]
		n = ringSize
	}
	first := copy(s.ring[s.ringHead:], p)
	copy(s.ring, p[first:])
	s.ringHead = (s.ringHead + n) % ringSize
	if s.ringLen+n > ringSize {
		s.ringLen = ringSize
	} else {
		s.ringLen += n
	}
	s.ringSerial++
}

// ringSerial returns a monotonically increasing counter that advances every
// time writeRing appends bytes. Cheap poll for the menu detector to skip
// a scan when the PTY hasn't produced any new output since last tick.
func (s *Session) getRingSerial() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ringSerial
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
	i := bytes.IndexByte(out, '\n')
	if i < 0 {
		return nil
	}
	return out[i+1:]
}

// wsSub wraps a websocket.Conn with an outbound queue + a writer goroutine.
// The producer (hook handler, PTY reader) never blocks — it either enqueues
// or drops when the queue is full, which prevents a single slow client from
// stalling everyone else. See issue #21.
type wsSub struct {
	c      *websocket.Conn
	out    chan []byte
	closed chan struct{}
	kind   int // websocket.TextMessage or websocket.BinaryMessage
}

func newWSSub(c *websocket.Conn, kind, queue int) *wsSub {
	sub := &wsSub{c: c, out: make(chan []byte, queue), closed: make(chan struct{}), kind: kind}
	go sub.pump()
	return sub
}

func (w *wsSub) pump() {
	for msg := range w.out {
		if err := w.c.WriteMessage(w.kind, msg); err != nil {
			w.stop()
			return
		}
	}
}

// send tries to enqueue; drops the message if the queue is full, meaning
// the client is behind. Dropped bytes for the terminal show up as a gap
// in scrollback (which reconnect solves); dropped dashboard patches are
// non-fatal because state is fully re-broadcast on the next event.
func (w *wsSub) send(msg []byte) bool {
	select {
	case w.out <- msg:
		return true
	default:
		return false
	}
}

func (w *wsSub) stop() {
	select {
	case <-w.closed:
		return
	default:
	}
	close(w.closed)
	close(w.out)
	_ = w.c.Close()
}

const (
	// Per-connection outbound queue depth. Bigger = tolerate longer pauses;
	// smaller = tighter memory bound on stalled clients.
	sessionQueueDepth = 256 // ~8 MB with 32 KB PTY chunks
	dashQueueDepth    = 64
)

func (s *Session) addSub(c *websocket.Conn) *wsSub {
	sub := newWSSub(c, websocket.BinaryMessage, sessionQueueDepth)
	s.subMu.Lock()
	s.subs[sub] = true
	s.subMu.Unlock()
	return sub
}

func (s *Session) removeSub(sub *wsSub) {
	s.subMu.Lock()
	delete(s.subs, sub)
	s.subMu.Unlock()
	sub.stop()
}

func (s *Session) broadcastBytes(p []byte) {
	s.subMu.Lock()
	subs := make([]*wsSub, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subMu.Unlock()
	for _, sub := range subs {
		sub.send(p)
	}
}

func (s *Session) closeSubs() {
	s.subMu.Lock()
	subs := make([]*wsSub, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subs = make(map[*wsSub]bool)
	s.subMu.Unlock()
	for _, sub := range subs {
		sub.stop()
	}
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
	now := time.Now()
	req := &ApprovalRequest{Message: msg, At: now, ExpiresAt: now.Add(pendingTTL)}
	// If PreToolUse fired within the last 5 seconds, this permission prompt
	// is almost certainly for that tool call — surface the specifics.
	if !s.LastPreToolAt.IsZero() && time.Since(s.LastPreToolAt) < 5*time.Second {
		req.Tool = s.LastPreToolName
		req.ToolInput = s.LastPreToolInput
	}
	s.Pending = req
	s.mu.Unlock()
}

func (s *Session) pendingExpired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Pending != nil && !s.Pending.ExpiresAt.IsZero() && time.Now().After(s.Pending.ExpiresAt)
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

func (s *Session) hasPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Pending != nil
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
		if !s.Pending.ExpiresAt.IsZero() {
			m["expiresAt"] = s.Pending.ExpiresAt.Format(time.RFC3339)
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
		// #37 PR-ready
		"prURL":   s.PRURL,
		"prTitle": s.PRTitle,
	}
}

var (
	registryMu     sync.RWMutex
	registry       = map[string]*Session{}
	sessionToAgent = map[string]string{}
	hookToAgent    = map[string]string{}
)

func registerSession(s *Session) {
	registryMu.Lock()
	registry[s.ID] = s
	sessionToAgent[s.SessionID] = s.ID
	if s.HookToken != "" {
		hookToAgent[s.HookToken] = s.ID
	}
	registryMu.Unlock()
	broadcastDashboard(map[string]any{"type": "upsert", "agent": s.toJSON()})
}

func removeSession(id string) {
	registryMu.Lock()
	s, ok := registry[id]
	if ok {
		delete(registry, id)
		delete(sessionToAgent, s.SessionID)
		if s.HookToken != "" {
			delete(hookToAgent, s.HookToken)
		}
		if s.SettingsDir != "" {
			_ = os.RemoveAll(s.SettingsDir)
		}
	}
	registryMu.Unlock()
	if ok {
		if s.cancel != nil {
			s.cancel()
		}
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

func getSessionByHookToken(ht string) *Session {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if aid, ok := hookToAgent[ht]; ok {
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
	dashSubs = map[*wsSub]bool{}
)

func addDashSub(c *websocket.Conn) *wsSub {
	sub := newWSSub(c, websocket.TextMessage, dashQueueDepth)
	dashMu.Lock()
	dashSubs[sub] = true
	dashMu.Unlock()
	return sub
}

func removeDashSub(sub *wsSub) {
	dashMu.Lock()
	delete(dashSubs, sub)
	dashMu.Unlock()
	sub.stop()
}

func broadcastDashboardRaw(msg map[string]any) {
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	dashMu.Lock()
	subs := make([]*wsSub, 0, len(dashSubs))
	for sub := range dashSubs {
		subs = append(subs, sub)
	}
	dashMu.Unlock()
	for _, sub := range subs {
		sub.send(b)
	}
}

// broadcastDashboard is the public entry point — coalesces multiple upsert
// events for the same session into a single broadcast per ~50ms window.
// Non-upsert events (snapshot, remove) go through immediately.
func broadcastDashboard(msg map[string]any) {
	if t, _ := msg["type"].(string); t == "upsert" {
		if agent, ok := msg["agent"].(map[string]any); ok {
			if id, ok := agent["id"].(string); ok && id != "" {
				coalesceUpsert(id, agent)
				return
			}
		}
	}
	broadcastDashboardRaw(msg)
}

// Coalesced upserts: keep only the freshest agent snapshot per id and flush
// once every ~50ms. See issue #22.
var (
	coalesceMu     sync.Mutex
	coalescePend   = map[string]map[string]any{}
	coalesceTimer  *time.Timer
	coalesceWindow = 50 * time.Millisecond
)

func coalesceUpsert(id string, agent map[string]any) {
	coalesceMu.Lock()
	coalescePend[id] = agent
	if coalesceTimer == nil {
		coalesceTimer = time.AfterFunc(coalesceWindow, flushCoalescedUpserts)
	}
	coalesceMu.Unlock()
}

func flushCoalescedUpserts() {
	coalesceMu.Lock()
	batch := coalescePend
	coalescePend = map[string]map[string]any{}
	coalesceTimer = nil
	coalesceMu.Unlock()
	for _, agent := range batch {
		broadcastDashboardRaw(map[string]any{"type": "upsert", "agent": agent})
	}
}

// startPendingSweeper clears stale ApprovalRequests whose ExpiresAt has passed.
// A Notification hook may fire and Claude then crash or hang — without this
// the amber banner would stick forever.
func startPendingSweeper() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			registryMu.RLock()
			sessions := make([]*Session, 0, len(registry))
			for _, s := range registry {
				sessions = append(sessions, s)
			}
			registryMu.RUnlock()
			for _, s := range sessions {
				if s.pendingExpired() {
					s.clearPending()
					s.appendActivity(ActivityEntry{Event: "PermissionPromptExpired", Level: "warn"})
					s.touch()
				}
			}
		}
	}()
}
