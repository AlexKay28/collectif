package main

// Tests for the /api/gh/* read-only mirror.
//
// The gh CLI is never called in these tests — either we stub ghExecFn or we
// seed the cache directly on disk and hit the handler through httptest. Any
// test that reached out over the network would fail the "must run offline"
// contract in issue #44 slice A.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Repo URL parsing
// ---------------------------------------------------------------------------

func TestParseGitHubRemote(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    ghRepo
		wantErr bool
	}{
		{"ssh with .git", "git@github.com:AlexKay28/collectif.git", ghRepo{"AlexKay28", "collectif"}, false},
		{"ssh without .git", "git@github.com:AlexKay28/collectif", ghRepo{"AlexKay28", "collectif"}, false},
		{"https with .git", "https://github.com/AlexKay28/collectif.git", ghRepo{"AlexKay28", "collectif"}, false},
		{"https without .git", "https://github.com/AlexKay28/collectif", ghRepo{"AlexKay28", "collectif"}, false},
		{"https with trailing slash", "https://github.com/AlexKay28/collectif/", ghRepo{"AlexKay28", "collectif"}, false},
		{"http (upgraded via regex)", "http://github.com/AlexKay28/collectif", ghRepo{"AlexKay28", "collectif"}, false},
		{"empty", "", ghRepo{}, true},
		{"whitespace", "   ", ghRepo{}, true},
		{"gitlab ssh", "git@gitlab.com:owner/repo.git", ghRepo{}, true},
		{"gitea https", "https://gitea.example.com/o/r.git", ghRepo{}, true},
		{"local path", "/tmp/repo.git", ghRepo{}, true},
		{"nonsense", "notaurl", ghRepo{}, true},
		{"ssh missing repo", "git@github.com:AlexKay28", ghRepo{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseGitHubRemote(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Cache round-trip
// ---------------------------------------------------------------------------

// testRepo is the fake owner/name every seedCache test uses. Kept as a
// package-level so the "fallback cwd" path in serviceForRequest can find
// this repo without shelling out to git — see stubGHServerCwd.
var testRepo = ghRepo{Owner: "AlexKay28", Name: "collectif"}

// stubGHServerCwd overrides ghServerCwdFn + resolveOriginRepo for a single
// test so serviceForRequest("") returns the seeded testRepo without calling
// real git. The returned cleanup restores the previous stubs.
//
// This replaces the old pattern of pre-populating a global service handle
// with a hand-built cache; the registry model wants us to go through
// services.forCwd so we exercise the real code path.
func stubGHServerCwd(cacheRoot string) func() {
	prevCwd := ghServerCwdFn
	prevRepoRoot := ghRepoRootFn
	prevResolve := ghResolveOriginFn
	ghServerCwdFn = func() string { return cacheRoot }
	ghRepoRootFn = func(cwd string) string { return cacheRoot }
	ghResolveOriginFn = func(cwd string) (ghRepo, error) { return testRepo, nil }
	return func() {
		ghServerCwdFn = prevCwd
		ghRepoRootFn = prevRepoRoot
		ghResolveOriginFn = prevResolve
	}
}

// seedCache writes minimal fixtures into a fresh cache root and warms the
// registry so serviceForRequest("") returns a ghService pointing at it.
// Returns the created cache dir so the test can inspect files if needed.
func seedCache(t *testing.T, issues []ghIssueIndexEntry, prs []ghPRIndexEntry) string {
	t.Helper()
	dir := t.TempDir()

	cache, err := newGHCache(dir, testRepo)
	if err != nil {
		t.Fatalf("newGHCache: %v", err)
	}
	if err := cache.ensureDirs(); err != nil {
		t.Fatalf("ensureDirs: %v", err)
	}
	if err := writeIndex(cache.issueIndexPath(), issues); err != nil {
		t.Fatalf("write issues idx: %v", err)
	}
	if err := writeIndex(cache.prIndexPath(), prs); err != nil {
		t.Fatalf("write prs idx: %v", err)
	}
	// One full issue body so the /issues/{n} test has something to read.
	for _, e := range issues {
		body, _ := json.Marshal(map[string]any{
			"number":        e.Number,
			"title":         e.Title,
			"state":         e.State,
			"body":          "fixture body for #" + fmt.Sprintf("%d", e.Number),
			"comments_data": []any{},
			"updated_at":    e.UpdatedAt,
		})
		if err := writeAtomic(cache.issuePath(e.Number), body); err != nil {
			t.Fatalf("write issue %d: %v", e.Number, err)
		}
	}
	for _, e := range prs {
		body, _ := json.Marshal(map[string]any{
			"number":     e.Number,
			"title":      e.Title,
			"state":      e.State,
			"head":       map[string]any{"ref": e.HeadRef, "sha": e.HeadSHA},
			"base":       map[string]any{"ref": e.BaseRef, "sha": e.BaseSHA},
			"updated_at": e.UpdatedAt,
		})
		if err := writeAtomic(cache.prPath(e.Number), body); err != nil {
			t.Fatalf("write pr %d: %v", e.Number, err)
		}
	}

	// Reset the registry, stub the "server cwd" hooks, and pre-warm the
	// registry with our fixture cache so the handlers pick it up without
	// hitting the (stubbed) resolver a second time.
	resetGHServiceForTest()
	restore := stubGHServerCwd(dir)
	shard := testRepo.Owner + "__" + testRepo.Name
	services.services[shard] = &ghService{
		cache:   cache,
		syncer:  newGHSyncer(cache, testRepo),
		rootDir: dir,
	}
	t.Cleanup(func() {
		restore()
		resetGHServiceForTest()
	})
	return dir
}

// seedTestSession pokes a minimal Session into the process registry so
// serviceForRequest can look it up via ?agent=<id>. The returned cleanup
// removes the entry so tests don't leak into each other.
func seedTestSession(id, cwd string) func() {
	s := &Session{ID: id, Cwd: cwd}
	registryMu.Lock()
	registry[id] = s
	registryMu.Unlock()
	return func() {
		registryMu.Lock()
		delete(registry, id)
		registryMu.Unlock()
	}
}

func TestCacheRoundTrip_IssueByNumber(t *testing.T) {
	seedCache(t, []ghIssueIndexEntry{
		{Number: 42, Title: "Test issue", State: "open", UpdatedAt: "2026-07-01T00:00:00Z"},
	}, nil)

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/issues/42")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if n, _ := got["number"].(float64); int(n) != 42 {
		t.Fatalf("number = %v want 42", got["number"])
	}
	if got["title"] != "Test issue" {
		t.Fatalf("title = %v", got["title"])
	}
}

func TestIssueByNumber_404WhenMissing(t *testing.T) {
	seedCache(t, nil, nil)
	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/issues/999")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d, want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Filter tests — state + q
// ---------------------------------------------------------------------------

func TestIssuesListFiltering(t *testing.T) {
	seedCache(t, []ghIssueIndexEntry{
		{Number: 1, Title: "Fix crash on startup", State: "open", Labels: []string{"bug"}},
		{Number: 2, Title: "Refactor auth", State: "open", Labels: []string{"enhancement"}, Assignees: []string{"AlexKay28"}},
		{Number: 3, Title: "Old completed task", State: "closed", Labels: []string{"bug"}},
		{Number: 4, Title: "AUTH improvements", State: "closed"},
	}, nil)

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	get := func(qs string) (map[string]any, int) {
		resp, err := http.Get(srv.URL + "/api/gh/issues?" + qs)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close()
		var v map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&v)
		return v, resp.StatusCode
	}

	// state=open
	v, code := get("state=open")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if int(v["total"].(float64)) != 2 {
		t.Fatalf("state=open total = %v want 2", v["total"])
	}

	// state=closed
	v, _ = get("state=closed")
	if int(v["total"].(float64)) != 2 {
		t.Fatalf("state=closed total = %v want 2", v["total"])
	}

	// state=all (default when omitted)
	v, _ = get("")
	if int(v["total"].(float64)) != 4 {
		t.Fatalf("state=all total = %v want 4", v["total"])
	}

	// q=auth — case-insensitive match against title, should hit #2 and #4
	v, _ = get("q=auth")
	if int(v["total"].(float64)) != 2 {
		t.Fatalf("q=auth total = %v want 2", v["total"])
	}

	// q=auth + state=open — just #2
	v, _ = get("state=open&q=auth")
	if int(v["total"].(float64)) != 1 {
		t.Fatalf("q=auth&state=open total = %v want 1", v["total"])
	}

	// label filter
	v, _ = get("label=bug")
	if int(v["total"].(float64)) != 2 {
		t.Fatalf("label=bug total = %v want 2", v["total"])
	}

	// assignee filter
	v, _ = get("assignee=AlexKay28")
	if int(v["total"].(float64)) != 1 {
		t.Fatalf("assignee=AlexKay28 total = %v want 1", v["total"])
	}

	// no matches
	v, _ = get("q=zzz-does-not-exist")
	if int(v["total"].(float64)) != 0 {
		t.Fatalf("no matches total = %v want 0", v["total"])
	}
}

// ---------------------------------------------------------------------------
// Status endpoint on empty cache
// ---------------------------------------------------------------------------

func TestStatusOnEmptyCache(t *testing.T) {
	seedCache(t, nil, nil)
	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/status")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	repo, _ := v["repo"].(map[string]any)
	if repo["owner"] != "AlexKay28" {
		t.Fatalf("owner = %v", repo["owner"])
	}
	if repo["name"] != "collectif" {
		t.Fatalf("name = %v", repo["name"])
	}
	if v["syncing"] != false {
		t.Fatalf("syncing = %v want false", v["syncing"])
	}
	if int(v["pendingCount"].(float64)) != 0 {
		t.Fatalf("pendingCount = %v want 0", v["pendingCount"])
	}
	// No lastSyncAt yet — the field should be present but zero-valued.
	if v["lastSyncAt"] == nil {
		t.Fatalf("lastSyncAt should be present (may be zero string)")
	}
}

// ---------------------------------------------------------------------------
// Sync with stubbed gh — proves the write pipeline end-to-end
// ---------------------------------------------------------------------------

func TestSyncWithStubbedGH(t *testing.T) {
	// Redirect the registry to a temp cache, but keep the real syncer so
	// runSync exercises the fetcher paths against our ghExecFn stub.
	dir := t.TempDir()
	resetGHServiceForTest()
	restore := stubGHServerCwd(dir)
	t.Cleanup(func() { restore(); resetGHServiceForTest() })
	cache, err := newGHCache(dir, testRepo)
	if err != nil {
		t.Fatalf("newGHCache: %v", err)
	}
	shard := testRepo.Owner + "__" + testRepo.Name
	services.services[shard] = &ghService{
		cache:   cache,
		syncer:  newGHSyncer(cache, testRepo),
		rootDir: dir,
	}
	svc := services.services[shard]

	// Fixture responses. Order matters: the pipeline calls issues list,
	// then per-issue comments, then pulls list, then per-PR reviews +
	// comments, then repo meta. Anything unexpected is a bug.
	prev := ghExecFn
	t.Cleanup(func() { ghExecFn = prev })
	ghExecFn = func(_ context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "issues?state=all") && strings.Contains(joined, "page=1"):
			// One real issue + one PR (has pull_request field, must be skipped).
			return []byte(`[
				{"number": 43, "title": "Right side panel", "state": "open", "user": {"login": "AlexKay28"}, "labels": [{"name": "enhancement"}], "comments": 0, "created_at": "2026-07-17T12:59:11Z", "updated_at": "2026-07-17T12:59:11Z", "html_url": "https://github.com/AlexKay28/collectif/issues/43"},
				{"number": 57, "title": "Some PR", "state": "open", "pull_request": {"url": "..."}, "user": {"login": "AlexKay28"}, "labels": [], "comments": 0, "created_at": "2026-07-20T00:00:00Z", "updated_at": "2026-07-20T00:00:00Z"}
			]`), nil
		case strings.Contains(joined, "issues?state=all") && strings.Contains(joined, "page=2"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "issues/43/comments"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "issues/57/comments"):
			return []byte(`[{"id": 1, "body": "lgtm", "user": {"login": "AlexKay28"}}]`), nil
		case strings.Contains(joined, "pulls?state=all") && strings.Contains(joined, "page=1"):
			return []byte(`[
				{"number": 57, "title": "Some PR", "state": "open", "draft": false, "merged_at": null, "user": {"login": "AlexKay28"}, "labels": [], "head": {"ref": "feature", "sha": "aaa"}, "base": {"ref": "main", "sha": "bbb"}, "created_at": "2026-07-20T00:00:00Z", "updated_at": "2026-07-20T00:00:00Z", "html_url": "https://github.com/AlexKay28/collectif/pull/57"}
			]`), nil
		case strings.Contains(joined, "pulls?state=all") && strings.Contains(joined, "page=2"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "pulls/57/reviews"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "pulls/57/comments"):
			return []byte(`[]`), nil
		case strings.Contains(joined, "repos/AlexKay28/collectif") && !strings.Contains(joined, "/"):
			return []byte(`{"default_branch": "main"}`), nil
		case joined == "api repos/AlexKay28/collectif":
			return []byte(`{"default_branch": "main"}`), nil
		}
		return nil, fmt.Errorf("unexpected gh call: %v", args)
	}

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Kick a synchronous sync.
	resp, err := http.Post(srv.URL+"/api/gh/sync?wait=1", "application/json", nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("sync status %d: %s", resp.StatusCode, string(body))
	}
	resp.Body.Close()

	// Issues index should have exactly one entry (the PR was filtered out).
	if _, err := os.Stat(svc.cache.issuePath(43)); err != nil {
		t.Fatalf("issue 43 not written: %v", err)
	}
	if _, err := os.Stat(svc.cache.issuePath(57)); err == nil {
		t.Fatalf("PR 57 was written to issues/ (should have been filtered out)")
	}
	// PR index should have PR 57.
	if _, err := os.Stat(svc.cache.prPath(57)); err != nil {
		t.Fatalf("pr 57 not written: %v", err)
	}
	// Repo meta lastSyncAt should be recent.
	var meta ghRepoMeta
	found, err := readJSON(svc.cache.repoMetaPath(), &meta)
	if err != nil || !found {
		t.Fatalf("repo meta: found=%v err=%v", found, err)
	}
	if time.Since(meta.LastSyncAt) > time.Minute {
		t.Fatalf("lastSyncAt not recent: %v", meta.LastSyncAt)
	}
	if meta.DefaultBranch != "main" {
		t.Fatalf("defaultBranch = %q want main", meta.DefaultBranch)
	}

	// /api/gh/issues should return the single non-PR issue.
	resp, err = http.Get(srv.URL + "/api/gh/issues")
	if err != nil {
		t.Fatalf("issues: %v", err)
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	resp.Body.Close()
	if int(v["total"].(float64)) != 1 {
		t.Fatalf("issues total = %v want 1", v["total"])
	}
}

// ---------------------------------------------------------------------------
// Coalescing — second concurrent sync returns started=false
// ---------------------------------------------------------------------------

func TestSyncCoalesces(t *testing.T) {
	dir := t.TempDir()
	resetGHServiceForTest()
	// Stub cwd/resolver so serviceForRequest("") returns our fixture repo
	// without shelling to git. Note the name here is "y", not "collectif" —
	// the resolver stub always returns the same shard.
	prevCwd := ghServerCwdFn
	prevRepoRoot := ghRepoRootFn
	prevResolve := ghResolveOriginFn
	repo := ghRepo{Owner: "x", Name: "y"}
	ghServerCwdFn = func() string { return dir }
	ghRepoRootFn = func(cwd string) string { return dir }
	ghResolveOriginFn = func(cwd string) (ghRepo, error) { return repo, nil }
	t.Cleanup(func() {
		ghServerCwdFn = prevCwd
		ghRepoRootFn = prevRepoRoot
		ghResolveOriginFn = prevResolve
		resetGHServiceForTest()
	})
	cache, err := newGHCache(dir, repo)
	if err != nil {
		t.Fatalf("newGHCache: %v", err)
	}
	shard := repo.Owner + "__" + repo.Name
	services.services[shard] = &ghService{
		cache:   cache,
		syncer:  newGHSyncer(cache, repo),
		rootDir: dir,
	}
	svc := services.services[shard]

	// Manually claim the sync lock so a subsequent POST /api/gh/sync sees
	// the "already syncing" branch. We finish it before the test ends so
	// no goroutine leak.
	if !svc.syncer.tryStart() {
		t.Fatalf("test setup: sync unexpectedly already running")
	}
	defer svc.syncer.finish(nil)

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/gh/sync", "application/json", nil)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	defer resp.Body.Close()
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	if v["started"] != false {
		t.Fatalf("started = %v want false", v["started"])
	}
}

// ---------------------------------------------------------------------------
// Diff endpoint — cached fresh path
// ---------------------------------------------------------------------------

func TestPRDiffServesCachedFile(t *testing.T) {
	seedCache(t, nil, []ghPRIndexEntry{
		{Number: 57, Title: "PR", State: "open", HeadRef: "feature", BaseRef: "main", HeadSHA: "aaa", BaseSHA: "bbb", UpdatedAt: "2020-01-01T00:00:00Z"},
	})

	// Write a diff file with a mod time AFTER the PR's updated_at.
	shard := testRepo.Owner + "__" + testRepo.Name
	diffPath := services.services[shard].cache.diffPath(57)
	if err := os.MkdirAll(filepath.Dir(diffPath), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(diffPath, []byte("diff --git a/foo b/foo\n"), 0o600); err != nil {
		t.Fatalf("write diff: %v", err)
	}

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/prs/57/diff")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "diff --git") {
		t.Fatalf("body = %q", string(body))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestPRDiff404WhenPRMissing(t *testing.T) {
	seedCache(t, nil, nil)
	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/prs/999/diff")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d want 404", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// concatenated arrays decoder (used by --paginate output)
// ---------------------------------------------------------------------------

func TestDecodeConcatenatedArrays(t *testing.T) {
	// Two pages of comments concatenated (as gh api --paginate emits).
	body := []byte(`[{"id":1}][{"id":2},{"id":3}]`)
	got, err := decodeConcatenatedArrays(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d items want 3", len(got))
	}

	// Single non-paginated array.
	got, err = decodeConcatenatedArrays([]byte(`[]`))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d items want 0", len(got))
	}
}

// ---------------------------------------------------------------------------
// #44 per-agent scoping — ?agent=<id> path
// ---------------------------------------------------------------------------

// TestServiceForRequestAgent seeds a fake session pointing at a "different"
// project cwd and confirms /api/gh/status?agent=<id> resolves to that repo
// (not the server's fallback repo). The resolver stub returns a distinct
// (owner, name) for the agent's cwd so we can prove the wiring end-to-end.
func TestServiceForRequestAgent(t *testing.T) {
	serverDir := t.TempDir()
	agentDir := t.TempDir()

	prevCwd := ghServerCwdFn
	prevRepoRoot := ghRepoRootFn
	prevResolve := ghResolveOriginFn
	ghServerCwdFn = func() string { return serverDir }
	ghRepoRootFn = func(cwd string) string { return cwd }
	ghResolveOriginFn = func(cwd string) (ghRepo, error) {
		if cwd == agentDir {
			return ghRepo{Owner: "agentowner", Name: "agentrepo"}, nil
		}
		return ghRepo{Owner: "serverowner", Name: "serverrepo"}, nil
	}
	resetGHServiceForTest()
	t.Cleanup(func() {
		ghServerCwdFn = prevCwd
		ghRepoRootFn = prevRepoRoot
		ghResolveOriginFn = prevResolve
		resetGHServiceForTest()
	})

	cleanup := seedTestSession("agent-xyz", agentDir)
	defer cleanup()

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/status?agent=agent-xyz")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, string(body))
	}
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
	repo, _ := v["repo"].(map[string]any)
	if repo["owner"] != "agentowner" || repo["name"] != "agentrepo" {
		t.Fatalf("repo = %+v, want owner=agentowner name=agentrepo", repo)
	}

	// Sanity: the cache was sharded under the AGENT's repo key, and it
	// landed beneath the SERVER's cache root — not the agent's cwd.
	wantShard := filepath.Join(serverDir, ".collectif", "cache", "gh", "agentowner__agentrepo")
	if _, err := os.Stat(wantShard); err != nil {
		t.Fatalf("expected sharded cache at %s: %v", wantShard, err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, ".collectif")); err == nil {
		t.Fatalf("cache leaked into agent cwd — should stay server-anchored")
	}
}

// TestServiceForRequestAgentNotFound proves the 404 branch fires when the
// query param names a session id the registry doesn't know about. The body
// is the {"error": "..."} JSON the frontend expects.
func TestServiceForRequestAgentNotFound(t *testing.T) {
	dir := t.TempDir()
	prevCwd := ghServerCwdFn
	prevRepoRoot := ghRepoRootFn
	prevResolve := ghResolveOriginFn
	ghServerCwdFn = func() string { return dir }
	ghRepoRootFn = func(cwd string) string { return dir }
	ghResolveOriginFn = func(cwd string) (ghRepo, error) { return testRepo, nil }
	resetGHServiceForTest()
	t.Cleanup(func() {
		ghServerCwdFn = prevCwd
		ghRepoRootFn = prevRepoRoot
		ghResolveOriginFn = prevResolve
		resetGHServiceForTest()
	})

	mux := http.NewServeMux()
	registerGHRoutes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/gh/status?agent=nonexistent")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d want 404", resp.StatusCode)
	}
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	msg, _ := v["error"].(string)
	if !strings.Contains(msg, "nonexistent") {
		t.Fatalf("error = %q, want it to mention the missing id", msg)
	}
}
