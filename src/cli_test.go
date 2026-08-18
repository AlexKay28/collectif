package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"sync/atomic"
	"testing"
)

// TestGetAdapterDefaultsToClaude — an empty CLI name resolves to the
// default adapter so pre-#46 clients (and Session values loaded from
// on-disk state that predates the field) keep working unchanged.
func TestGetAdapterDefaultsToClaude(t *testing.T) {
	a := getAdapter("")
	if a == nil {
		t.Fatalf("getAdapter(\"\") returned nil; expected default claude adapter")
	}
	if a.Name() != "claude" {
		t.Errorf("getAdapter(\"\").Name() = %q, want %q", a.Name(), "claude")
	}
}

// TestGetAdapterUnknown — unknown names return nil. Callers rely on the
// nil sentinel to decide between "spawn something" and "reject the
// request as 400"; see handleAgents in api.go.
func TestGetAdapterUnknown(t *testing.T) {
	if a := getAdapter("nonesuch"); a != nil {
		t.Errorf("getAdapter(\"nonesuch\") = %v, want nil", a)
	}
}

// TestClaudeAdapterCapabilitiesAllTrue — Claude Code is the reference
// implementation; every capability bit is expected to be on. This test
// is intentionally strict so future accidental disables surface loudly.
func TestClaudeAdapterCapabilitiesAllTrue(t *testing.T) {
	a := getAdapter("claude")
	if a == nil {
		t.Fatalf("no claude adapter registered")
	}
	c := a.Capabilities()
	if !c.Hooks || !c.StructuredTranscript || !c.ToolCallEvents ||
		!c.SubagentFiles || !c.PreCompact || !c.SessionIDPinning {
		t.Errorf("claude Capabilities want all true, got %+v", c)
	}
}

// TestClaudeAdapterParseTranscriptLine — one usage-bearing line with a
// content array of each block type. The event shape must match what the
// underlying extractUsageAndChars + extractModel helpers return; those
// have their own dedicated coverage in transcript_test.go, so this is a
// thin delegation test.
func TestClaudeAdapterParseTranscriptLine(t *testing.T) {
	a := getAdapter("claude")
	if a == nil {
		t.Fatalf("no claude adapter")
	}
	line := []byte(`{"message":{"role":"assistant","model":"claude-opus-4-7","content":[` +
		`{"type":"thinking","thinking":"hmm"},` +
		`{"type":"text","text":"hi"}` +
		`],"usage":{"input_tokens":11,"output_tokens":22,"cache_read_input_tokens":3,"cache_creation_input_tokens":5}}}` + "\n")

	ev, err := a.ParseTranscriptLine(line)
	if err != nil {
		t.Fatalf("ParseTranscriptLine: %v", err)
	}
	if !ev.HasUsage {
		t.Fatalf("HasUsage=false; expected true for a usage-bearing line")
	}
	if ev.Model != "claude-opus-4-7" {
		t.Errorf("Model=%q, want claude-opus-4-7", ev.Model)
	}
	if ev.InputTokens != 11 || ev.OutputTokens != 22 ||
		ev.CacheReadTokens != 3 || ev.CacheCreationTokens != 5 {
		t.Errorf("tokens: got in=%d out=%d cr=%d cc=%d, want 11/22/3/5",
			ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheCreationTokens)
	}
	if ev.ThinkingChars != uint64(len("hmm")) {
		t.Errorf("ThinkingChars=%d, want %d", ev.ThinkingChars, len("hmm"))
	}
	if ev.TextChars != uint64(len("hi")) {
		t.Errorf("TextChars=%d, want %d", ev.TextChars, len("hi"))
	}
}

// TestClaudeAdapterParseTranscriptLineNoUsage — a line without a usage
// block must return HasUsage=false and a nil error. The watcher relies
// on this to skip non-assistant lines without spamming logs.
func TestClaudeAdapterParseTranscriptLineNoUsage(t *testing.T) {
	a := getAdapter("claude")
	ev, err := a.ParseTranscriptLine([]byte(`{"type":"user","message":{"role":"user","content":"hi"}}` + "\n"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ev.HasUsage {
		t.Errorf("HasUsage=true on a non-usage line")
	}
}

// TestClaudeAdapterModelContextLimit — sanity check that the adapter
// resolves ids against its own catalog (claudeModels in adapter_claude.go).
// The per-model values are asserted in harness_test.go; this test just
// confirms the routing and the empty-model fallback.
//
// #48: this previously asserted 200000 for a 1M-window model, encoding the
// stale table it was written against.
func TestClaudeAdapterModelContextLimit(t *testing.T) {
	a := getAdapter("claude")
	if got, want := a.ModelContextLimit("claude-opus-4-7-20260115"), 1_000_000; got != want {
		t.Errorf("ModelContextLimit(opus)=%d, want %d", got, want)
	}
	if got, want := a.ModelContextLimit(""), defaultContextLimit; got != want {
		t.Errorf("ModelContextLimit(\"\")=%d, want %d", got, want)
	}
}

// TestClaudeAdapterTranscriptPath — well-known path computation. We
// mainly care that (a) it computes some non-empty path when session +
// cwd are provided and (b) the empty-session-id case returns false.
func TestClaudeAdapterTranscriptPath(t *testing.T) {
	a := getAdapter("claude")
	if _, ok := a.TranscriptPath("", "/tmp/xyz"); ok {
		t.Errorf("expected ok=false for empty sessionID")
	}
	p, ok := a.TranscriptPath("sid-1", "/tmp/xyz")
	if !ok || p == "" {
		t.Errorf("expected a non-empty path, got %q ok=%v", p, ok)
	}
}

// ─── #46 Phase 3: GET /api/cli ─────────────────────────────────────

// TestHandleCLIList_ReturnsAllAdaptersDefaultFirst — the endpoint must
// return every registered adapter, with the default ("claude") first
// and the rest alphabetically. Also asserts the JSON payload shape
// (name/version/isDefault/capabilities) so a rename regression trips
// this test immediately.
func TestHandleCLIList_ReturnsAllAdaptersDefaultFirst(t *testing.T) {
	resetVersionCache() // start from a clean cache so behaviour is deterministic
	req := httptest.NewRequest(http.MethodGet, "/api/cli", nil)
	rec := httptest.NewRecorder()
	handleCLIList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got []cliListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("json: %v; body: %s", err, rec.Body.String())
	}
	if len(got) < 3 {
		t.Fatalf("expected at least 3 adapters, got %d: %+v", len(got), got)
	}
	// Default first.
	if got[0].Name != defaultAdapterName {
		t.Errorf("first entry name = %q, want %q", got[0].Name, defaultAdapterName)
	}
	if !got[0].IsDefault {
		t.Errorf("first entry isDefault = false, want true")
	}
	// Every registered adapter should appear exactly once.
	byName := map[string]cliListResponse{}
	for _, e := range got {
		if _, dup := byName[e.Name]; dup {
			t.Errorf("adapter %q returned twice", e.Name)
		}
		byName[e.Name] = e
	}
	for _, want := range []string{"claude", "codex", "opencode"} {
		e, ok := byName[want]
		if !ok {
			t.Errorf("adapter %q missing from response", want)
			continue
		}
		if want == defaultAdapterName && !e.IsDefault {
			t.Errorf("%q isDefault=false", want)
		}
		if want != defaultAdapterName && e.IsDefault {
			t.Errorf("%q isDefault=true; only %q may be default", want, defaultAdapterName)
		}
	}
	// Non-default entries must be alphabetically ordered.
	prev := ""
	for _, e := range got {
		if e.Name == defaultAdapterName {
			continue
		}
		if prev != "" && e.Name < prev {
			t.Errorf("non-default entries out of order: %q < %q", e.Name, prev)
		}
		prev = e.Name
	}
}

// TestHandleCLIList_CapabilitiesShape — sanity check that Claude's
// capabilities are all-true on the wire and each field name matches
// what the frontend keys on. Guards against an accidental camelCase
// -> snake_case rename slipping through.
func TestHandleCLIList_CapabilitiesShape(t *testing.T) {
	resetVersionCache()
	req := httptest.NewRequest(http.MethodGet, "/api/cli", nil)
	rec := httptest.NewRecorder()
	handleCLIList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Decode as a loose map so we see the exact JSON keys.
	var loose []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &loose); err != nil {
		t.Fatalf("json: %v", err)
	}
	var claude map[string]any
	for _, e := range loose {
		if n, _ := e["name"].(string); n == "claude" {
			claude = e
			break
		}
	}
	if claude == nil {
		t.Fatalf("claude adapter missing from response")
	}
	caps, ok := claude["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities not an object: %v", claude["capabilities"])
	}
	// transcriptContent joined the wire in #56: the session view has to be
	// able to say *why* a selected session has no document, and "collectif
	// cannot read this CLI's transcript format" is a different answer from
	// "no turn has been written yet". Without it the panel could only shrug.
	for _, k := range []string{"hooks", "structuredTranscript", "toolCallEvents",
		"subagentFiles", "preCompact", "sessionIdPinning", "transcriptContent"} {
		v, present := caps[k]
		if !present {
			t.Errorf("capabilities.%s missing", k)
			continue
		}
		b, isBool := v.(bool)
		if !isBool {
			t.Errorf("capabilities.%s not a bool: %v (%T)", k, v, v)
		}
		if !b {
			t.Errorf("capabilities.%s = false; claude is expected to be all-true", k)
		}
	}
}

// TestHandleCLIList_WrongMethod — non-GET must 405. The Router()
// integration test below covers auth; this exercises the handler's
// own method gate directly.
func TestHandleCLIList_WrongMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/cli", nil)
	rec := httptest.NewRecorder()
	handleCLIList(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rec.Code)
	}
}

// countingAdapter is a test-only CLIAdapter that counts Version()
// invocations so we can assert the 60s per-adapter cache actually
// short-circuits repeat calls. It's not registered in adapters — we
// swap it in explicitly for the cache test below.
type countingAdapter struct {
	name  string
	calls atomic.Int64
}

func (c *countingAdapter) Name() string { return c.name }
func (c *countingAdapter) Version() (string, error) {
	c.calls.Add(1)
	return "9.9.9", nil
}
func (c *countingAdapter) Capabilities() Capabilities                     { return Capabilities{} }
func (c *countingAdapter) Spawn(SpawnRequest) (*exec.Cmd, func(), error)  { return nil, func() {}, nil }
func (c *countingAdapter) TranscriptPath(string, string) (string, bool)   { return "", false }
func (c *countingAdapter) ParseTranscriptLine([]byte) (TranscriptEvent, error) {
	return TranscriptEvent{}, nil
}
func (c *countingAdapter) ProjectTranscriptLine([]byte) ([]TranscriptPart, error) {
	return nil, nil
}
func (c *countingAdapter) ModelContextLimit(string) int { return defaultContextLimit }

// TestHandleCLIList_VersionCache — cachedVersion() must short-circuit
// on repeat calls within versionCacheTTL. We drive the cache directly
// (rather than through /api/cli) so the assertion isolates the cache
// semantics from the endpoint's sort/serialization concerns.
func TestHandleCLIList_VersionCache(t *testing.T) {
	resetVersionCache()
	a := &countingAdapter{name: "counting-test-adapter"}
	// First hit: cache miss → Version() called.
	if v := cachedVersion(a); v != "9.9.9" {
		t.Errorf("first cachedVersion = %q, want %q", v, "9.9.9")
	}
	// Second and third: cache hits → Version() must not be called again.
	_ = cachedVersion(a)
	_ = cachedVersion(a)
	if got := a.calls.Load(); got != 1 {
		t.Errorf("Version() call count = %d after 3 cachedVersion calls, want 1", got)
	}
}

// TestHandleCLIList_RouterIntegration — end-to-end substitute for the
// live-smoke curl in the Phase 3 DoD. Stands up the real *Server
// router (auth middleware + all), hits /api/cli with the shared-secret
// token, and asserts a well-formed adapter list comes back. This
// exercises the same handler chain a browser hits.
func TestHandleCLIList_RouterIntegration(t *testing.T) {
	resetVersionCache()
	srv := NewServer("127.0.0.1", "0", "smoke-test-token", nil)
	ts := httptest.NewServer(srv.Router())
	defer ts.Close()

	// Unauthed request must 401 (the endpoint is behind /api/* auth).
	if resp, err := http.Get(ts.URL + "/api/cli"); err != nil {
		t.Fatalf("unauth GET: %v", err)
	} else {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauth /api/cli: got %d, want 401", resp.StatusCode)
		}
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/cli", nil)
	req.Header.Set("Authorization", "Bearer smoke-test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/cli: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/cli status: %d", resp.StatusCode)
	}
	var list []cliListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(list) < 3 {
		t.Errorf("expected >= 3 adapters via router, got %d", len(list))
	}
	if list[0].Name != defaultAdapterName || !list[0].IsDefault {
		t.Errorf("first entry: %+v; expected default %q", list[0], defaultAdapterName)
	}
}
