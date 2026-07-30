package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// #38 Per-block-type char counters + double-count guard + pro-rating math.
//
// The transcript walker feeds a `message.content[]` array into
// sumContentChars, keyed to the same assistant turn as `message.usage`.
// A line with both top-level `usage` AND `message.usage` should credit
// tokens once (findUsage returns top-level first) but must NOT scan
// content twice — and content that only lives under `message` should
// still be picked up in that case.

func TestExtractUsageAndChars_AllBlockTypes(t *testing.T) {
	// A single assistant turn with one block of each type + a usage.
	// tool_use.input is an object; sumContentChars json-encodes it to
	// approximate the token surface. We compute expected lengths from
	// the same encoder so the test is robust to key ordering.
	toolInput := map[string]any{"path": "/tmp/x", "n": float64(3)}
	toolBytes, _ := json.Marshal(toolInput)
	line := []byte(`{"message":{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"reasoning here"},` +
		`{"type":"text","text":"hello world"},` +
		`{"type":"tool_use","name":"Read","input":{"path":"/tmp/x","n":3}}` +
		`],"usage":{"input_tokens":10,"output_tokens":50}}}` + "\n")
	in, out, _, _, thinkCh, textCh, toolCh, ok := extractUsageAndChars(line)
	if !ok {
		t.Fatalf("expected ok")
	}
	if in != 10 || out != 50 {
		t.Errorf("tokens: in=%d out=%d, want 10/50", in, out)
	}
	if thinkCh != uint64(len("reasoning here")) {
		t.Errorf("thinkCh=%d want %d", thinkCh, len("reasoning here"))
	}
	if textCh != uint64(len("hello world")) {
		t.Errorf("textCh=%d want %d", textCh, len("hello world"))
	}
	if toolCh != uint64(len(toolBytes)) {
		t.Errorf("toolCh=%d want %d (json len of %s)", toolCh, len(toolBytes), string(toolBytes))
	}
}

func TestExtractUsageAndChars_UnknownBlockTypesIgnored(t *testing.T) {
	// Unknown block types (image, redacted_thinking, whatever future
	// shapes Anthropic adds) contribute zero. The counter should not
	// change compared with a text-only message.
	line := []byte(`{"message":{"role":"assistant","content":[` +
		`{"type":"text","text":"hi"},` +
		`{"type":"image","source":{"data":"..."}}` +
		`],"usage":{"output_tokens":1}}}` + "\n")
	_, _, _, _, thinkCh, textCh, toolCh, ok := extractUsageAndChars(line)
	if !ok {
		t.Fatalf("expected ok")
	}
	if thinkCh != 0 || toolCh != 0 || textCh != 2 {
		t.Errorf("expected only textCh=2, got think=%d text=%d tool=%d", thinkCh, textCh, toolCh)
	}
}

func TestExtractUsageAndChars_NoContent(t *testing.T) {
	// Historic transcripts where the walker sees usage but no content
	// array — chars stay zero and ok stays true so the token counters
	// still increment.
	line := []byte(`{"message":{"role":"assistant","usage":{"output_tokens":42}}}` + "\n")
	_, out, _, _, thinkCh, textCh, toolCh, ok := extractUsageAndChars(line)
	if !ok || out != 42 {
		t.Fatalf("expected ok with output_tokens=42")
	}
	if thinkCh != 0 || textCh != 0 || toolCh != 0 {
		t.Errorf("expected zero chars when content is absent")
	}
}

func TestExtractUsageAndChars_TopLevelUsageDoesNotDoubleCountContent(t *testing.T) {
	// A JSONL line that carries BOTH a top-level `usage` (which
	// findUsage will pick up first) AND a nested `message.usage` +
	// `message.content[]`. The walker must credit the tokens once and
	// walk content exactly once — even though usage comes from the
	// top-level branch, content still lives only under `message`, so
	// we should see the char counters increment.
	line := []byte(`{"usage":{"input_tokens":1,"output_tokens":10},` +
		`"message":{"role":"assistant","usage":{"input_tokens":999,"output_tokens":999},` +
		`"content":[{"type":"text","text":"abc"}]}}` + "\n")
	in, out, _, _, thinkCh, textCh, toolCh, ok := extractUsageAndChars(line)
	if !ok {
		t.Fatalf("expected ok")
	}
	// Top-level usage wins (matches findUsage precedence). Nested is ignored.
	if in != 1 || out != 10 {
		t.Errorf("tokens: got in=%d out=%d, want 1/10 (top-level should win)", in, out)
	}
	// Chars are still credited from message.content, but only ONCE.
	// If the walker double-counted (e.g. scanning both top-level and
	// message branches for content) textCh would be 6, not 3.
	if textCh != 3 || thinkCh != 0 || toolCh != 0 {
		t.Errorf("chars: got think=%d text=%d tool=%d, want 0/3/0 (double-count guard)", thinkCh, textCh, toolCh)
	}
}

// TestToJSON_ProRateNoDrift asserts that thinking + text + tool tokens
// exactly equal outputTokens. The remainder-goes-to-tool trick in
// Session.toJSON prevents rounding drift so the UI can render three
// segments of a stacked bar and have them sum to 100% of the whole.
func TestToJSON_ProRateNoDrift(t *testing.T) {
	// Deliberately picked numbers so that integer division produces
	// remainders: chars 7 / 11 / 13 (total 31) and outputTokens 100.
	// 100*7/31 = 22, 100*11/31 = 35, remainder = 43. Sum must be 100.
	s := newSession("agent-pr", "sid-pr", "/tmp", "")
	s.OutputTokens = 100
	s.OutputThinkingChars = 7
	s.OutputTextChars = 11
	s.OutputToolChars = 13
	j := s.toJSON()
	think := j["outputThinkingTokens"].(int64)
	text := j["outputTextTokens"].(int64)
	tool := j["outputToolTokens"].(int64)
	if think+text+tool != 100 {
		t.Fatalf("pro-rated sum drifted: think=%d + text=%d + tool=%d = %d, want 100",
			think, text, tool, think+text+tool)
	}
	if think != 22 || text != 35 || tool != 43 {
		t.Errorf("pro-rate math: got %d/%d/%d, want 22/35/43", think, text, tool)
	}
	// Raw chars must also round-trip on the wire so the UI tooltip
	// can show "N thinking chars, M text chars, K tool chars".
	if j["outputThinkingChars"].(uint64) != 7 ||
		j["outputTextChars"].(uint64) != 11 ||
		j["outputToolChars"].(uint64) != 13 {
		t.Errorf("raw char counts wrong on the wire")
	}
}

func TestToJSON_ProRateNoCharsIsZero(t *testing.T) {
	// New session (or session that pre-dates #38): outputTokens is non-zero
	// but chars are all zero. Split fields should just be zero — do not
	// panic on divide-by-zero.
	s := newSession("agent-pr0", "sid-pr0", "/tmp", "")
	s.OutputTokens = 500
	j := s.toJSON()
	if j["outputThinkingTokens"].(int64) != 0 ||
		j["outputTextTokens"].(int64) != 0 ||
		j["outputToolTokens"].(int64) != 0 {
		t.Errorf("expected all zero when char counters are empty")
	}
}

// TestTranscriptWatcher_AccumulatesCharCounters wires the whole path end
// to end: a live watcher, a JSONL file with typed content, and the
// per-block-type counters incrementing on the Session.
func TestTranscriptWatcher_AccumulatesCharCounters(t *testing.T) {
	s := newTestSession(t, "agent-tc", "sid-tc")
	tpath := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(tpath, nil, 0o644); err != nil {
		t.Fatalf("create: %v", err)
	}
	s.mu.Lock()
	s.TranscriptPath = tpath
	s.mu.Unlock()
	startTranscriptWatcher(s.ctx, s)

	line := `{"message":{"role":"assistant","model":"claude-opus-4-7","content":[` +
		`{"type":"thinking","thinking":"aaaa"},` +
		`{"type":"text","text":"bb"},` +
		`{"type":"tool_use","name":"X","input":{"k":"v"}}` +
		`],"usage":{"input_tokens":1,"output_tokens":9}}}` + "\n"
	if err := os.WriteFile(tpath, []byte(line), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := s.OutputTextChars
		s.mu.Unlock()
		if got > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	toolBytes, _ := json.Marshal(map[string]any{"k": "v"})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.OutputThinkingChars != 4 || s.OutputTextChars != 2 ||
		s.OutputToolChars != uint64(len(toolBytes)) {
		t.Errorf("char counters: think=%d text=%d tool=%d (want 4/2/%d)",
			s.OutputThinkingChars, s.OutputTextChars, s.OutputToolChars, len(toolBytes))
	}
}

// #38 Smoke test standing in for the manual "start the server, curl
// /api/agents, grep for thinking/text/tool" step from the DoD. Confirms
// the three new *Tokens fields, the three *Chars fields, and the
// existing outputTokens all appear together in a single serialised
// snapshot with the expected numeric relationships.
func TestSessionToJSON_ExposesSplitFieldsForAPI(t *testing.T) {
	s := newSession("agent-wire", "sid-wire", "/tmp", "")
	s.OutputTokens = 200
	s.OutputThinkingChars = 100
	s.OutputTextChars = 50
	s.OutputToolChars = 50
	// Round-trip through JSON to prove the map keys survive marshalling
	// as the exact identifiers the frontend / any external client will
	// look for. Anything that renames or drops a key breaks this test.
	blob, err := json.Marshal(s.toJSON())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(blob, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{
		"outputTokens",
		"outputThinkingTokens", "outputTextTokens", "outputToolTokens",
		"outputThinkingChars", "outputTextChars", "outputToolChars",
	} {
		if _, ok := wire[k]; !ok {
			t.Errorf("expected key %q on the wire", k)
		}
	}
	// Numeric relationship: the three split-tokens sum to output_tokens.
	// json.Unmarshal decodes numbers to float64 by default; that's the
	// same coercion the JS client will see.
	sum := wire["outputThinkingTokens"].(float64) +
		wire["outputTextTokens"].(float64) +
		wire["outputToolTokens"].(float64)
	if sum != float64(s.OutputTokens) {
		t.Errorf("wire sum drift: %v != %d", sum, s.OutputTokens)
	}
}

// #38 API-level smoke stand-in for the manual "curl /api/agents |
// grep -iE 'thinking|text|tool'" step in the DoD. Runs the same
// handleAgents GET path a browser would hit, then prints the split
// fields for visual confirmation when -v is passed.
func TestSmoke_ApiAgentsReturnsSplitTokenFields(t *testing.T) {
	s := newTestSession(t, "agent-smoke", "sid-smoke")
	s.mu.Lock()
	s.OutputTokens = 500
	s.OutputThinkingChars = 300
	s.OutputTextChars = 150
	s.OutputToolChars = 50
	s.mu.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	rec := httptest.NewRecorder()
	testServer().handleAgents(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var mine map[string]any
	for _, a := range list {
		if a["id"] == s.ID {
			mine = a
			break
		}
	}
	if mine == nil {
		t.Fatalf("session not in list")
	}
	for _, k := range []string{
		"outputThinkingTokens", "outputTextTokens", "outputToolTokens",
		"outputThinkingChars", "outputTextChars", "outputToolChars",
	} {
		if _, ok := mine[k]; !ok {
			t.Errorf("missing %s on the wire", k)
		}
	}
	pretty, _ := json.MarshalIndent(map[string]any{
		"outputTokens":         mine["outputTokens"],
		"outputThinkingTokens": mine["outputThinkingTokens"],
		"outputTextTokens":     mine["outputTextTokens"],
		"outputToolTokens":     mine["outputToolTokens"],
		"outputThinkingChars":  mine["outputThinkingChars"],
		"outputTextChars":      mine["outputTextChars"],
		"outputToolChars":      mine["outputToolChars"],
	}, "", "  ")
	t.Logf("smoke — /api/agents wire fields for #38:\n%s", pretty)
	// Also print outside of t.Logf so -v isn't strictly required for
	// human-readable verification when this test is run standalone.
	fmt.Println("smoke — /api/agents wire fields for #38:")
	fmt.Println(string(pretty))
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
