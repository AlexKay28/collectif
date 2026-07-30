package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// testServer builds a bare *Server for handler-under-test scenarios that need
// a receiver. Keeps the constructor centralised so future fields can be added
// in one place.
func testServer() *Server {
	return NewServer("127.0.0.1", "0", "test-token", nil)
}

func TestHandleCwdCheck_MissingPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/cwd/check", nil)
	rec := httptest.NewRecorder()
	handleCwdCheck(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "path required") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestHandleCwdCheck_RelativePath(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/cwd/check?path=relative/dir", nil)
	rec := httptest.NewRecorder()
	handleCwdCheck(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "absolute") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestHandleCwdCheck_NotFound(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/cwd/check?path=/nonexistent/xyz-collectif-test", nil)
	rec := httptest.NewRecorder()
	handleCwdCheck(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleCwdCheck_OK(t *testing.T) {
	dir := t.TempDir()
	req := httptest.NewRequest(http.MethodGet, "/api/cwd/check?path="+dir, nil)
	rec := httptest.NewRecorder()
	handleCwdCheck(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("json: %v", err)
	}
	if ok, _ := v["ok"].(bool); !ok {
		t.Errorf("ok not true: %v", v)
	}
}

func TestHandleCwdCheck_RejectsFileAsCwd(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file.txt")
	writeFile(t, f, "hi")
	req := httptest.NewRequest(http.MethodGet, "/api/cwd/check?path="+f, nil)
	rec := httptest.NewRecorder()
	handleCwdCheck(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (not a dir), got %d", rec.Code)
	}
}

func TestHandleCwdCheck_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/cwd/check?path=/tmp", nil)
	rec := httptest.NewRecorder()
	handleCwdCheck(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleAgents_GET_ReturnsList(t *testing.T) {
	s := newTestSession(t, uuid.NewString(), uuid.NewString())
	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	found := false
	for _, a := range out {
		if a["id"] == s.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected registered agent in list; got %d entries", len(out))
	}
}

func TestHandleAgents_POST_RejectsMissingCwd(t *testing.T) {
	body, _ := json.Marshal(spawnReq{})
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cwd required") {
		t.Errorf("body: %s", rec.Body.String())
	}
}

func TestHandleAgents_POST_RejectsCwdNotADirectory(t *testing.T) {
	body, _ := json.Marshal(spawnReq{Cwd: "/no/such/thing/abcxyz"})
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgents_POST_RejectsBadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/agents", bytes.NewReader([]byte("{ nope")))
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgents_MethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/api/agents", nil)
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

func TestHandleAgentByID_InvalidUUID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/agents/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleAgentByID_NotFound(t *testing.T) {
	id := uuid.NewString()
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+id, nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleAgentByID_MissingID(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/agents/", nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing id, got %d", rec.Code)
	}
}

func TestHandleAgentByID_GET_ReturnsAgentJSON(t *testing.T) {
	id := uuid.NewString()
	s := newTestSession(t, id, uuid.NewString())
	req := httptest.NewRequest(http.MethodGet, "/api/agents/"+id, nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("json: %v", err)
	}
	if v["id"] != s.ID {
		t.Errorf("id in response wrong: %v", v["id"])
	}
}

func TestHandleAgentByID_DELETE_WithoutProcess(t *testing.T) {
	// A session with no *exec.Cmd attached still works — the handler just
	// skips the kill and marks the session stopped.
	id := uuid.NewString()
	_ = newTestSession(t, id, uuid.NewString())
	req := httptest.NewRequest(http.MethodDelete, "/api/agents/"+id, nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if getSession(id) != nil {
		t.Errorf("expected session removed after DELETE")
	}
}

func TestHandleAgentByID_UnknownSubpath(t *testing.T) {
	id := uuid.NewString()
	_ = newTestSession(t, id, uuid.NewString())
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+id+"/nope", nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleAgentInput_PTYNotReady(t *testing.T) {
	id := uuid.NewString()
	_ = newTestSession(t, id, uuid.NewString())
	body, _ := json.Marshal(inputReq{Data: "hi"})
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+id+"/input", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 (pty not ready), got %d", rec.Code)
	}
}

func TestHandleAgentResize_RangeCheck(t *testing.T) {
	id := uuid.NewString()
	_ = newTestSession(t, id, uuid.NewString())
	// pty not ready → 503 (short-circuits before range check)
	body, _ := json.Marshal(resizeReq{Cols: 80, Rows: 24})
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+id+"/resize", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
}

func TestHandleAgentResume_WrongStatus(t *testing.T) {
	id := uuid.NewString()
	s := newTestSession(t, id, uuid.NewString())
	s.setStatus("running", "not paused")

	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+id+"/resume", nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}
}

func TestHandleAgentReviewed_ClearsPRState(t *testing.T) {
	id := uuid.NewString()
	s := newTestSession(t, id, uuid.NewString())
	s.mu.Lock()
	s.PRURL = "https://github.com/x/y/pull/1"
	s.PRTitle = "some pr"
	s.mu.Unlock()

	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+id+"/reviewed", nil)
	rec := httptest.NewRecorder()
	handleAgentByID(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", rec.Code)
	}
	s.mu.Lock()
	url := s.PRURL
	title := s.PRTitle
	status := s.Status
	s.mu.Unlock()
	if url != "" || title != "" {
		t.Errorf("PR fields not cleared: %q %q", url, title)
	}
	if status != "stopped" {
		t.Errorf("status: got %q, want stopped", status)
	}
}

func TestDecodeBody_TooLarge(t *testing.T) {
	// Valid JSON larger than maxBodyBytes so MaxBytesReader trips *during*
	// decoding rather than bailing out earlier on a JSON syntax error.
	buf := bytes.NewBuffer(make([]byte, 0, (1<<20)+256))
	buf.WriteByte('"')
	buf.Write(bytes.Repeat([]byte("a"), (1<<20)+128))
	buf.WriteByte('"')
	req := httptest.NewRequest(http.MethodPost, "/", buf)
	rec := httptest.NewRecorder()
	var v any
	if decodeBody(rec, req, &v) {
		t.Fatalf("decodeBody: expected false on oversized body")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", rec.Code)
	}
}

func TestDecodeBody_BadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader([]byte("nope")))
	rec := httptest.NewRecorder()
	var v map[string]any
	if decodeBody(rec, req, &v) {
		t.Fatalf("decodeBody: expected false on bad json")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}
