package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestSession registers a session with a random-ish hook token and returns
// it. Cleanup removes the session from the global registry so test order is
// stable.
func newTestSession(t *testing.T, id, sid string) *Session {
	t.Helper()
	s := newSession(id, sid, t.TempDir(), "")
	s.HookToken = "ht-" + id
	registerSession(s)
	t.Cleanup(func() { removeSession(id) })
	return s
}

// postHook posts a payload to handleHook using httptest. Returns the response.
func postHook(t *testing.T, s *Session, payload hookPayload) *http.Response {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/hooks?ht="+s.HookToken, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleHook(rec, req)
	return rec.Result()
}

func TestHandleHook_RejectsMissingToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/hooks", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()
	handleHook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandleHook_RejectsUnknownToken(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/hooks?ht=nobody", bytes.NewReader([]byte("{}")))
	rec := httptest.NewRecorder()
	handleHook(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandleHook_RejectsNonPOST(t *testing.T) {
	s := newTestSession(t, "agent-mm", "sid-mm")
	req := httptest.NewRequest(http.MethodGet, "/api/hooks?ht="+s.HookToken, nil)
	rec := httptest.NewRecorder()
	handleHook(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleHook_RejectsBadJSON(t *testing.T) {
	s := newTestSession(t, "agent-bj", "sid-bj")
	req := httptest.NewRequest(http.MethodPost, "/api/hooks?ht="+s.HookToken, bytes.NewReader([]byte("not json")))
	rec := httptest.NewRecorder()
	handleHook(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleHook_SessionStart_SetsRunning(t *testing.T) {
	s := newTestSession(t, "agent-ss", "sid-ss")
	resp := postHook(t, s, hookPayload{HookEventName: "SessionStart", Source: "resumed"})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "running" {
		t.Errorf("status: got %q, want %q", s.Status, "running")
	}
	if len(s.Activity) == 0 || s.Activity[len(s.Activity)-1].Event != "SessionStart" {
		t.Errorf("expected SessionStart in activity, got %+v", s.Activity)
	}
	if s.Activity[len(s.Activity)-1].Detail != "resumed" {
		t.Errorf("detail: got %q", s.Activity[len(s.Activity)-1].Detail)
	}
}

func TestHandleHook_UserPromptSubmit_PushesTask(t *testing.T) {
	s := newTestSession(t, "agent-ups", "sid-ups")
	postHook(t, s, hookPayload{HookEventName: "UserPromptSubmit", Prompt: "fix the bug"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.CurrentTask != "fix the bug" {
		t.Errorf("currentTask: got %q", s.CurrentTask)
	}
	if len(s.TaskHistory) != 1 || s.TaskHistory[0] != "fix the bug" {
		t.Errorf("taskHistory: got %v", s.TaskHistory)
	}
	if s.Status != "running" {
		t.Errorf("status: got %q, want running", s.Status)
	}
}

func TestHandleHook_PreToolUse_RecordsToolAndAskQuestion(t *testing.T) {
	s := newTestSession(t, "agent-ptu", "sid-ptu")

	postHook(t, s, hookPayload{HookEventName: "PreToolUse", ToolName: "Read"})
	s.mu.Lock()
	if s.ToolCounts["Read"] != 1 || s.LastTool != "Read" {
		t.Errorf("ToolCounts/LastTool wrong: counts=%v last=%q", s.ToolCounts, s.LastTool)
	}
	if s.Status != "running" || !strings.Contains(s.LastActivity, "Read") {
		t.Errorf("status/activity: %q / %q", s.Status, s.LastActivity)
	}
	s.mu.Unlock()

	// AskUserQuestion payload → AskQuestion should populate.
	askInput := map[string]any{
		"questions": []any{
			map[string]any{
				"question": "Which approach?",
				"header":   "Approach",
				"options": []any{
					map[string]any{"label": "Option A", "description": "safe"},
					map[string]any{"label": "Option B", "description": "fast"},
				},
			},
		},
	}
	postHook(t, s, hookPayload{HookEventName: "PreToolUse", ToolName: "AskUserQuestion", ToolInput: askInput})
	s.mu.Lock()
	q := s.AskQuestion
	s.mu.Unlock()
	if q == nil || len(q.Questions) != 1 {
		t.Fatalf("AskQuestion: got %+v", q)
	}
	if q.Questions[0].Question != "Which approach?" || len(q.Questions[0].Options) != 2 {
		t.Errorf("question fields wrong: %+v", q.Questions[0])
	}

	// PostToolUse for AskUserQuestion clears it.
	postHook(t, s, hookPayload{HookEventName: "PostToolUse", ToolName: "AskUserQuestion"})
	s.mu.Lock()
	q = s.AskQuestion
	s.mu.Unlock()
	if q != nil {
		t.Errorf("expected AskQuestion cleared, got %+v", q)
	}
}

func TestHandleHook_PostToolUseFailure_SetsError(t *testing.T) {
	s := newTestSession(t, "agent-fail", "sid-fail")
	postHook(t, s, hookPayload{HookEventName: "PostToolUseFailure", ToolName: "Bash"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "error" {
		t.Errorf("status: got %q, want error", s.Status)
	}
	if len(s.RecentFailures) != 1 || s.RecentFailures[0].Tool != "Bash" {
		t.Errorf("RecentFailures: got %+v", s.RecentFailures)
	}
}

func TestHandleHook_NotificationPermissionPrompt_SetsPending(t *testing.T) {
	s := newTestSession(t, "agent-pp", "sid-pp")
	postHook(t, s, hookPayload{
		HookEventName: "Notification",
		Message:       "Claude needs your permission to use Bash",
		Matcher:       "some-matcher-not-permission_prompt",
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "waiting_input" {
		t.Errorf("status: got %q, want waiting_input", s.Status)
	}
	if s.Pending == nil {
		t.Fatalf("expected Pending to be set")
	}
	if !strings.Contains(s.Pending.Message, "Bash") {
		t.Errorf("pending message: got %q", s.Pending.Message)
	}
}

func TestHandleHook_NotificationIdle_SetsIdle(t *testing.T) {
	s := newTestSession(t, "agent-idle", "sid-idle")
	postHook(t, s, hookPayload{
		HookEventName: "Notification",
		Message:       "Are you still there? Claude is waiting for you to respond",
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "idle" {
		t.Errorf("status: got %q, want idle", s.Status)
	}
	if s.Pending != nil {
		t.Errorf("Pending should not be set for idle notification, got %+v", s.Pending)
	}
}

func TestHandleHook_StripsAnsiFromNotificationMessage(t *testing.T) {
	s := newTestSession(t, "agent-ansi", "sid-ansi")
	postHook(t, s, hookPayload{
		HookEventName: "Notification",
		Message:       "\x1b[31mClaude needs\x1b[0m your permission",
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Pending == nil {
		t.Fatalf("expected Pending")
	}
	if strings.Contains(s.Pending.Message, "\x1b") {
		t.Errorf("ANSI not stripped: %q", s.Pending.Message)
	}
}

func TestHandleHook_StopEvent_ClearsPending(t *testing.T) {
	s := newTestSession(t, "agent-stop", "sid-stop")
	// First put us into waiting_input with a pending prompt.
	postHook(t, s, hookPayload{HookEventName: "Notification", Message: "please approve"})
	if !s.hasPending() {
		t.Fatalf("test setup: Pending should be set")
	}
	// Stop event clears pending and moves status to idle.
	postHook(t, s, hookPayload{HookEventName: "Stop"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "idle" {
		t.Errorf("status: got %q, want idle", s.Status)
	}
	if s.Pending != nil {
		t.Errorf("Pending should be cleared, got %+v", s.Pending)
	}
}

func TestHandleHook_ApprovalMatcherPreservesPending(t *testing.T) {
	s := newTestSession(t, "agent-app", "sid-app")
	// First set a pending prompt.
	postHook(t, s, hookPayload{HookEventName: "Notification", Message: "please approve"})
	if !s.hasPending() {
		t.Fatalf("test setup: Pending should be set")
	}
	// A permission_prompt matcher on Notification must NOT clear pending.
	postHook(t, s, hookPayload{
		HookEventName: "Notification",
		Matcher:       "permission_prompt",
		Message:       "still need approval",
	})
	if !s.hasPending() {
		t.Errorf("Pending should have survived permission_prompt notification")
	}
}

func TestHandleHook_SessionEnd_SetsStopped(t *testing.T) {
	s := newTestSession(t, "agent-end", "sid-end")
	postHook(t, s, hookPayload{HookEventName: "SessionEnd"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Status != "stopped" {
		t.Errorf("status: got %q, want stopped", s.Status)
	}
}

func TestHandleHook_LargeBodyRejected(t *testing.T) {
	s := newTestSession(t, "agent-big", "sid-big")
	huge := bytes.Repeat([]byte("a"), (1<<20)+2048)
	req := httptest.NewRequest(http.MethodPost, "/api/hooks?ht="+s.HookToken, bytes.NewReader(huge))
	rec := httptest.NewRecorder()
	handleHook(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestParseAskQuestion_MalformedInputReturnsNil(t *testing.T) {
	// No questions key
	if got := parseAskQuestion(map[string]any{"other": 1}); got != nil {
		t.Errorf("expected nil for missing questions, got %+v", got)
	}
	// Questions is not a list
	if got := parseAskQuestion(map[string]any{"questions": "nope"}); got != nil {
		t.Errorf("expected nil for wrong-type questions, got %+v", got)
	}
	// Question without options
	got := parseAskQuestion(map[string]any{
		"questions": []any{
			map[string]any{"question": "Hi?"},
		},
	})
	if got != nil {
		t.Errorf("expected nil when no option list, got %+v", got)
	}
}

func TestClearPendingUnlessApproval_ClearsOnUnrelatedEvent(t *testing.T) {
	s := newTestSession(t, "agent-cpu", "sid-cpu")
	s.setPending("something to approve")
	clearPendingUnlessApproval(s, hookPayload{HookEventName: "PreToolUse", ToolName: "Read"})
	if s.hasPending() {
		t.Errorf("Pending should have been cleared by non-approval event")
	}
}

func TestClearPendingUnlessApproval_KeepsOnPermissionPromptMatcher(t *testing.T) {
	s := newTestSession(t, "agent-cpu2", "sid-cpu2")
	s.setPending("something to approve")
	clearPendingUnlessApproval(s, hookPayload{
		HookEventName: "Notification",
		Matcher:       "permission_prompt",
	})
	if !s.hasPending() {
		t.Errorf("Pending should have been preserved by permission_prompt")
	}
}

func TestClearPendingUnlessApproval_UsesTypeAsFallbackMatcher(t *testing.T) {
	s := newTestSession(t, "agent-cpu3", "sid-cpu3")
	s.setPending("something")
	clearPendingUnlessApproval(s, hookPayload{
		HookEventName: "Notification",
		Type:          "permission_prompt", // matcher empty, type takes over
	})
	if !s.hasPending() {
		t.Errorf("Pending should have been preserved when Type is permission_prompt")
	}
}

// Sanity: setPending attaches recent PreTool tool + input if within 5s.
func TestSetPending_AnnotatesWithRecentPreTool(t *testing.T) {
	s := newTestSession(t, "agent-annot", "sid-annot")
	s.recordPreTool("Bash", map[string]any{"command": "ls"})
	// Simulate the small delay Claude Code has between PreToolUse and prompt.
	time.Sleep(10 * time.Millisecond)
	s.setPending("May I run this?")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Pending == nil {
		t.Fatalf("Pending nil")
	}
	if s.Pending.Tool != "Bash" || s.Pending.ToolInput["command"] != "ls" {
		t.Errorf("expected annotation from recent PreTool: %+v", s.Pending)
	}
}
