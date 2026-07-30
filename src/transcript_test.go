package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExtractUsage_TopLevel(t *testing.T) {
	line := []byte(`{"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}` + "\n")
	in, out, cr, cc, ok := extractUsage(line)
	if !ok {
		t.Fatalf("expected ok")
	}
	if in != 10 || out != 20 || cr != 5 || cc != 7 {
		t.Errorf("got in=%d out=%d cr=%d cc=%d", in, out, cr, cc)
	}
}

func TestExtractUsage_UnderMessage(t *testing.T) {
	line := []byte(`{"message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":50}}}`)
	in, out, cr, cc, ok := extractUsage(line)
	if !ok {
		t.Fatalf("expected ok")
	}
	if in != 100 || out != 50 || cr != 0 || cc != 0 {
		t.Errorf("got in=%d out=%d cr=%d cc=%d", in, out, cr, cc)
	}
}

func TestExtractUsage_UnderResponse(t *testing.T) {
	line := []byte(`{"response":{"usage":{"input_tokens":1,"output_tokens":2}}}`)
	in, out, _, _, ok := extractUsage(line)
	if !ok {
		t.Fatalf("expected ok")
	}
	if in != 1 || out != 2 {
		t.Errorf("got in=%d out=%d", in, out)
	}
}

func TestExtractUsage_MissingReturnsNotOk(t *testing.T) {
	// Valid JSON, no usage anywhere.
	line := []byte(`{"type":"user","message":{"role":"user","content":"hi"}}`)
	if _, _, _, _, ok := extractUsage(line); ok {
		t.Errorf("expected not ok")
	}
	// Malformed JSON
	if _, _, _, _, ok := extractUsage([]byte("not json\n")); ok {
		t.Errorf("expected not ok on bad json")
	}
}

func TestExtractUsage_HandlesInt64AndFloat(t *testing.T) {
	// json.Unmarshal into map[string]any gives float64 for numbers, so this
	// path is what production uses; still, guard the getI64 switch by asserting
	// large numbers survive the float→int64 round-trip.
	line := []byte(`{"usage":{"input_tokens":2000000000,"output_tokens":1}}`)
	in, out, _, _, ok := extractUsage(line)
	if !ok || in != 2_000_000_000 || out != 1 {
		t.Errorf("large number decode wrong: ok=%v in=%d out=%d", ok, in, out)
	}
}

func TestExtractModel_LocationsProbed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"top-level", `{"model":"claude-opus-4-7"}`, "claude-opus-4-7"},
		{"under message", `{"message":{"model":"claude-sonnet-4-6","role":"assistant"}}`, "claude-sonnet-4-6"},
		{"under response", `{"response":{"model":"claude-haiku-4-5-20251001"}}`, "claude-haiku-4-5-20251001"},
		{"absent", `{"type":"user"}`, ""},
		{"malformed", `not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractModel([]byte(c.in)); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestGetI64_TypeCoverage(t *testing.T) {
	m := map[string]any{
		"a": float64(42),
		"b": int(7),
		"c": int64(9),
		"d": "not-a-number",
	}
	if getI64(m, "a") != 42 || getI64(m, "b") != 7 || getI64(m, "c") != 9 {
		t.Errorf("numeric conversions wrong")
	}
	if getI64(m, "d") != 0 || getI64(m, "missing") != 0 {
		t.Errorf("non-numeric should return 0")
	}
}

// TestTranscriptWatcher_ReadsUsageIntoSessionCounters is an integration test:
// spin up a session, hand it a transcript path, write JSONL lines, and wait
// for the goroutine to fold them into the counters. Uses ctx cancellation
// via removeSession so the watcher exits before the test returns.
func TestTranscriptWatcher_ReadsUsageIntoSessionCounters(t *testing.T) {
	s := newTestSession(t, "agent-tr", "sid-tr")
	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	// Create the file up-front (watcher polls until it exists).
	if err := os.WriteFile(tpath, nil, 0o644); err != nil {
		t.Fatalf("create transcript: %v", err)
	}
	s.mu.Lock()
	s.TranscriptPath = tpath
	s.mu.Unlock()

	startTranscriptWatcher(s.ctx, s)

	// Append two assistant turns. Each carries usage → counters increment.
	line1 := `{"message":{"role":"assistant","model":"claude-opus-4-7","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}}` + "\n"
	line2 := `{"message":{"role":"assistant","model":"claude-sonnet-4-6","usage":{"input_tokens":200,"output_tokens":40}}}` + "\n"
	if err := os.WriteFile(tpath, []byte(line1+line2), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Poll for up to ~3s for the watcher's 500ms ticker to notice.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		msgs := s.MessageCount
		s.mu.Unlock()
		if msgs == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MessageCount != 2 {
		t.Fatalf("MessageCount: got %d, want 2 (watcher never saw both lines)", s.MessageCount)
	}
	if s.InputTokens != 300 || s.OutputTokens != 90 || s.CacheReadTokens != 10 || s.CacheCreationTokens != 5 {
		t.Errorf("counters: in=%d out=%d cr=%d cc=%d",
			s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheCreationTokens)
	}
	// Model should reflect the LAST usage-bearing line's model.
	if s.Model != "claude-sonnet-4-6" {
		t.Errorf("Model: got %q, want claude-sonnet-4-6 (last-turn tracking)", s.Model)
	}
	// LastContextTokens = last turn's uncached + cache read + cache create = 200.
	if s.LastContextTokens != 200 {
		t.Errorf("LastContextTokens: got %d, want 200", s.LastContextTokens)
	}
}

// TestTranscriptWatcher_HandlesPartialTrailingLine: the watcher buffers the
// last incomplete line and processes it once the newline arrives.
func TestTranscriptWatcher_HandlesPartialTrailingLine(t *testing.T) {
	s := newTestSession(t, "agent-tp", "sid-tp")
	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(tpath, nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	s.mu.Lock()
	s.TranscriptPath = tpath
	s.mu.Unlock()
	startTranscriptWatcher(s.ctx, s)

	// Write an incomplete first line (no trailing newline) plus a full second line.
	partial := `{"message":{"role":"assistant","usage":{"input_tokens":10,"output_tokens":5}}}`
	if err := os.WriteFile(tpath, []byte(partial), 0o644); err != nil {
		t.Fatalf("write partial: %v", err)
	}

	// Give the watcher a couple of ticks so the partial is observed.
	time.Sleep(1200 * time.Millisecond)
	s.mu.Lock()
	msgsBefore := s.MessageCount
	s.mu.Unlock()
	if msgsBefore != 0 {
		t.Fatalf("expected MessageCount=0 while line is incomplete, got %d", msgsBefore)
	}

	// Now finish that line and add another one.
	full := partial + "\n" +
		`{"message":{"role":"assistant","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(tpath, []byte(full), 0o644); err != nil {
		t.Fatalf("write full: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		msgs := s.MessageCount
		s.mu.Unlock()
		if msgs == 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.MessageCount != 2 {
		t.Fatalf("MessageCount: got %d, want 2", s.MessageCount)
	}
	if s.InputTokens != 11 || s.OutputTokens != 6 {
		t.Errorf("tokens: in=%d out=%d, want 11/6", s.InputTokens, s.OutputTokens)
	}
}

func TestTranscriptWatcher_IdempotentStart(t *testing.T) {
	s := newTestSession(t, "agent-idem", "sid-idem")
	tpath := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(tpath, nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	s.mu.Lock()
	s.TranscriptPath = tpath
	s.mu.Unlock()
	startTranscriptWatcher(s.ctx, s)
	// Second call must no-op since watching is set.
	startTranscriptWatcher(s.ctx, s)
	// One goroutine will exit when the session is cancelled by removeSession.
	// Nothing to assert beyond "doesn't deadlock or panic".
	time.Sleep(50 * time.Millisecond)
	s.mu.Lock()
	if !s.watching {
		t.Errorf("watching flag should still be true")
	}
	s.mu.Unlock()
}
