package main

// GitHub read-only cache + HTTP API (issue #44 slice A).
//
// This file implements the local mirror of the GitHub issue/PR read side:
//
//   * Repo resolution from the local `git remote get-url origin` (SSH + HTTPS).
//   * A background syncer that shells out to `gh api ...` to populate a
//     JSON-per-entity cache under <repo-root>/.collectif/cache/gh/.
//   * HTTP handlers under /api/gh/* that read exclusively from that cache —
//     the network is never on the critical path for a render.
//   * A PR diff endpoint that prefers local `git diff base..head` and falls
//     back to fetching the PR ref via `git fetch origin pull/N/head:...`.
//
// Slice A is READ-ONLY. Comment/close/label mutations belong to slice D
// (offline write queue), and this file does not touch them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Repo resolution
// ---------------------------------------------------------------------------

// ghRepo identifies a GitHub repository as (owner, name).
type ghRepo struct {
	Owner string `json:"owner"`
	Name  string `json:"name"`
}

// ghRepoMeta is what we persist under repo.json — cache-wide metadata about
// which remote we last synced and when. Populated on every successful sync.
type ghRepoMeta struct {
	Owner         string    `json:"owner"`
	Repo          string    `json:"repo"`
	DefaultBranch string    `json:"defaultBranch"`
	LastSyncAt    time.Time `json:"lastSyncAt"`
}

// Two remote URL forms are common:
//
//	git@github.com:owner/repo.git
//	https://github.com/owner/repo(.git)?
//
// Anything else (gitlab, gitea, local paths, ssh://) is rejected — this
// slice is GitHub-only per the issue's "Cross-repo config: origin only" line.
var (
	ghSSHRe   = regexp.MustCompile(`^git@github\.com:([^/]+)/([^/]+?)(?:\.git)?$`)
	ghHTTPSRe = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+?)(?:\.git)?/?$`)
)

// parseGitHubRemote pulls (owner, name) out of a remote URL. Returns an
// error the caller can pass straight through to the user — the remote URL
// is typically visible to them via `git remote -v` so echoing it is safe.
func parseGitHubRemote(remote string) (ghRepo, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ghRepo{}, errors.New("empty remote URL")
	}
	if m := ghSSHRe.FindStringSubmatch(remote); m != nil {
		return ghRepo{Owner: m[1], Name: m[2]}, nil
	}
	if m := ghHTTPSRe.FindStringSubmatch(remote); m != nil {
		return ghRepo{Owner: m[1], Name: m[2]}, nil
	}
	return ghRepo{}, fmt.Errorf("remote %q is not a GitHub SSH or HTTPS URL", remote)
}

// resolveOriginRepo runs `git remote get-url origin` in cwd and returns the
// parsed (owner, name). Bubble up a helpful error if there is no origin.
func resolveOriginRepo(cwd string) (ghRepo, error) {
	if cwd == "" {
		cwd = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ghRepo{}, fmt.Errorf("git remote get-url origin failed: %w", err)
	}
	return parseGitHubRemote(strings.TrimSpace(string(out)))
}

// repoRoot resolves the top-level git working tree for cwd. Falls back to
// cwd itself if git is not available — the cache still works, just rooted
// wherever the server was started.
func repoRoot(cwd string) string {
	if cwd == "" {
		cwd = "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		abs, aerr := filepath.Abs(cwd)
		if aerr != nil {
			return cwd
		}
		return abs
	}
	return strings.TrimSpace(string(out))
}

// ---------------------------------------------------------------------------
// Cache layout
// ---------------------------------------------------------------------------

// ghCache owns the on-disk layout under <root>/.collectif/cache/gh/<shard>/.
// All paths are absolute. Two syncers must never touch the same root — the
// syncer mutex guards that; concurrent readers (HTTP handlers) share it.
//
// The <shard> segment (owner + "__" + name) scopes the cache to a specific
// GitHub repo. The <root> stays anchored to the server's own repo root so we
// don't scatter .collectif/ dirs into random project repos the user visits;
// the shard lets one server process mirror multiple repos side by side
// (each agent may have a different cwd/origin).
type ghCache struct {
	root string // <server-repo-root>/.collectif/cache/gh/<owner>__<name>
}

// shardKeyRe validates the sharded subdir segment. parseGitHubRemote already
// only accepts owner/name matching this shape, but we double-check here so a
// future caller that constructs a ghRepo directly can't smuggle path
// separators into the filesystem layout.
var shardKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// newGHCache builds a cache rooted at <rootAbove>/.collectif/cache/gh/<shard>.
// The shard is derived from repo.Owner/repo.Name. Returns an error if either
// component contains characters outside the safe filename set — this should
// never trip in practice because parseGitHubRemote screens for it, but the
// belt-and-braces check keeps the on-disk layout honest.
func newGHCache(rootAbove string, repo ghRepo) (*ghCache, error) {
	if !shardKeyRe.MatchString(repo.Owner) || !shardKeyRe.MatchString(repo.Name) {
		return nil, fmt.Errorf("unsafe repo shard: owner=%q name=%q", repo.Owner, repo.Name)
	}
	shard := repo.Owner + "__" + repo.Name
	return &ghCache{root: filepath.Join(rootAbove, ".collectif", "cache", "gh", shard)}, nil
}

func (c *ghCache) repoMetaPath() string   { return filepath.Join(c.root, "repo.json") }
func (c *ghCache) issuesDir() string      { return filepath.Join(c.root, "issues") }
func (c *ghCache) prsDir() string         { return filepath.Join(c.root, "prs") }
func (c *ghCache) diffsDir() string       { return filepath.Join(c.root, "pr-diffs") }
func (c *ghCache) issueIndexPath() string { return filepath.Join(c.issuesDir(), "index.json") }
func (c *ghCache) prIndexPath() string    { return filepath.Join(c.prsDir(), "index.json") }
func (c *ghCache) issuePath(n int) string {
	return filepath.Join(c.issuesDir(), strconv.Itoa(n)+".json")
}
func (c *ghCache) prPath(n int) string   { return filepath.Join(c.prsDir(), strconv.Itoa(n)+".json") }
func (c *ghCache) diffPath(n int) string { return filepath.Join(c.diffsDir(), strconv.Itoa(n)+".diff") }

// ensureDirs creates the four subdirectories the cache needs. 0o700 so a
// token-leak on a shared machine doesn't expose the mirrored issue bodies.
func (c *ghCache) ensureDirs() error {
	for _, d := range []string{c.root, c.issuesDir(), c.prsDir(), c.diffsDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// writeAtomic writes body to path via a sibling .tmp file and rename, so a
// reader never sees a half-written JSON blob.
func writeAtomic(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readJSON slurps and unmarshals a cache file. Returns (false, nil) if the
// file does not exist so callers can distinguish "empty cache" from I/O error.
func readJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// Index entries — the shape the frontend lists render from
// ---------------------------------------------------------------------------

// ghIssueIndexEntry is the reduced projection stored in issues/index.json.
// Frontends filter/sort on this without loading each per-issue JSON.
type ghIssueIndexEntry struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	State        string   `json:"state"` // "open" | "closed"
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Author       string   `json:"author"`
	CommentCount int      `json:"commentCount"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
	HTMLURL      string   `json:"htmlUrl"`
}

// ghPRIndexEntry mirrors ghIssueIndexEntry plus the PR-specific bits the
// PR list needs to render (branches, draft badge, merged-vs-closed).
type ghPRIndexEntry struct {
	Number       int      `json:"number"`
	Title        string   `json:"title"`
	State        string   `json:"state"` // "open" | "closed"
	Merged       bool     `json:"merged"`
	Draft        bool     `json:"draft"`
	HeadRef      string   `json:"headRef"`
	BaseRef      string   `json:"baseRef"`
	HeadSHA      string   `json:"headSha"`
	BaseSHA      string   `json:"baseSha"`
	Labels       []string `json:"labels"`
	Assignees    []string `json:"assignees"`
	Author       string   `json:"author"`
	CommentCount int      `json:"commentCount"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
	HTMLURL      string   `json:"htmlUrl"`
}

// ---------------------------------------------------------------------------
// Syncer
// ---------------------------------------------------------------------------

// ghSyncer coalesces sync requests. At most one sync per cache is in flight
// at any moment; a second concurrent request returns immediately with
// "already syncing" so the caller can decide whether to poll status.
type ghSyncer struct {
	cache *ghCache
	repo  ghRepo

	mu      sync.Mutex // guards syncing + lastErr
	syncing bool
	lastErr error
	lastAt  time.Time
}

func newGHSyncer(cache *ghCache, repo ghRepo) *ghSyncer {
	return &ghSyncer{cache: cache, repo: repo}
}

// tryStart claims the sync lock. Returns false if a sync is already running.
func (s *ghSyncer) tryStart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.syncing {
		return false
	}
	s.syncing = true
	return true
}

func (s *ghSyncer) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncing = false
	s.lastErr = err
	s.lastAt = time.Now()
}

func (s *ghSyncer) status() (bool, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncing, s.lastAt, s.lastErr
}

// runSync is the full sync pipeline. It is safe to call from a background
// goroutine (POST /api/gh/sync) or synchronously (?wait=1).
func (s *ghSyncer) runSync(ctx context.Context) error {
	if err := s.cache.ensureDirs(); err != nil {
		return err
	}
	fetched, err := ghListIssuesAll(ctx, s.repo)
	if err != nil {
		return fmt.Errorf("list issues: %w", err)
	}
	// GitHub's issues endpoint returns PRs too; keep them separate.
	var issueIdx []ghIssueIndexEntry
	for _, item := range fetched {
		if item.IsPullRequest() {
			continue
		}
		comments, err := ghListIssueComments(ctx, s.repo, item.Number)
		if err != nil {
			return fmt.Errorf("issue %d comments: %w", item.Number, err)
		}
		full := map[string]any{}
		if err := json.Unmarshal(item.Raw, &full); err != nil {
			return fmt.Errorf("issue %d unmarshal: %w", item.Number, err)
		}
		full["comments_data"] = comments
		body, err := json.MarshalIndent(full, "", "  ")
		if err != nil {
			return err
		}
		if err := writeAtomic(s.cache.issuePath(item.Number), body); err != nil {
			return err
		}
		issueIdx = append(issueIdx, item.toIssueIndex())
	}
	sort.Slice(issueIdx, func(i, j int) bool { return issueIdx[i].Number > issueIdx[j].Number })
	if err := writeIndex(s.cache.issueIndexPath(), issueIdx); err != nil {
		return err
	}

	// PRs — use the pulls endpoint for the richer head/base/mergeable fields.
	prs, err := ghListPullsAll(ctx, s.repo)
	if err != nil {
		return fmt.Errorf("list prs: %w", err)
	}
	var prIdx []ghPRIndexEntry
	for _, pr := range prs {
		reviews, err := ghListPRReviews(ctx, s.repo, pr.Number)
		if err != nil {
			return fmt.Errorf("pr %d reviews: %w", pr.Number, err)
		}
		threadComments, err := ghListPRReviewComments(ctx, s.repo, pr.Number)
		if err != nil {
			return fmt.Errorf("pr %d review comments: %w", pr.Number, err)
		}
		issueComments, err := ghListIssueComments(ctx, s.repo, pr.Number)
		if err != nil {
			return fmt.Errorf("pr %d issue comments: %w", pr.Number, err)
		}
		full := map[string]any{}
		if err := json.Unmarshal(pr.Raw, &full); err != nil {
			return fmt.Errorf("pr %d unmarshal: %w", pr.Number, err)
		}
		full["comments_data"] = issueComments
		full["reviews_data"] = reviews
		full["review_comments_data"] = threadComments
		body, err := json.MarshalIndent(full, "", "  ")
		if err != nil {
			return err
		}
		if err := writeAtomic(s.cache.prPath(pr.Number), body); err != nil {
			return err
		}
		prIdx = append(prIdx, pr.toPRIndex(len(issueComments)))
	}
	sort.Slice(prIdx, func(i, j int) bool { return prIdx[i].Number > prIdx[j].Number })
	if err := writeIndex(s.cache.prIndexPath(), prIdx); err != nil {
		return err
	}

	// Repo meta — best-effort default branch (empty if the call fails so a
	// stale-network sync doesn't nuke a good defaultBranch on subsequent read).
	defaultBranch := ghDefaultBranch(ctx, s.repo)
	meta := ghRepoMeta{
		Owner:         s.repo.Owner,
		Repo:          s.repo.Name,
		DefaultBranch: defaultBranch,
		LastSyncAt:    time.Now().UTC(),
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.cache.repoMetaPath(), body)
}

func writeIndex[T any](path string, entries []T) error {
	if entries == nil {
		entries = []T{}
	}
	body, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, body)
}

// ---------------------------------------------------------------------------
// gh api shell-outs
// ---------------------------------------------------------------------------

// ghExecTimeout bounds any single `gh` invocation. 60s is generous enough
// for a slow network / big-page fetch but not so long that a hung process
// stalls the whole sync for minutes.
const ghExecTimeout = 60 * time.Second

// ghExec runs `gh` with the given args and returns stdout. Empties stderr
// into the returned error on failure so the caller has something to log.
// Overridable via ghExecFn so tests can inject a stub.
var ghExecFn = func(ctx context.Context, args ...string) ([]byte, error) {
	subCtx, cancel := context.WithTimeout(ctx, ghExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(subCtx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %s", err, msg)
	}
	return stdout.Bytes(), nil
}

// ghRawItem is a paginated list item — carries both a decoded number
// (for filtering issues vs PRs) and the raw JSON (so we can persist the
// full unmodified shape without losing fields we haven't modelled).
type ghRawItem struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	State       string          `json:"state"`
	HTMLURL     string          `json:"html_url"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Comments    int             `json:"comments"`
	Labels      []ghLabel       `json:"labels"`
	Assignees   []ghUser        `json:"assignees"`
	User        ghUser          `json:"user"`
	PullRequest *ghPullRequest  `json:"pull_request"`
	Raw         json.RawMessage `json:"-"`
}

type ghLabel struct {
	Name string `json:"name"`
}
type ghUser struct {
	Login string `json:"login"`
}
type ghPullRequest struct {
	URL string `json:"url"`
}

func (r *ghRawItem) IsPullRequest() bool { return r.PullRequest != nil }

func (r *ghRawItem) toIssueIndex() ghIssueIndexEntry {
	labels := make([]string, 0, len(r.Labels))
	for _, l := range r.Labels {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(r.Assignees))
	for _, a := range r.Assignees {
		assignees = append(assignees, a.Login)
	}
	return ghIssueIndexEntry{
		Number:       r.Number,
		Title:        r.Title,
		State:        r.State,
		Labels:       labels,
		Assignees:    assignees,
		Author:       r.User.Login,
		CommentCount: r.Comments,
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
		HTMLURL:      r.HTMLURL,
	}
}

// ghPullItem models the /pulls response — richer than the /issues shape
// because it has head/base/draft/merged_at fields we index for the PR list.
type ghPullItem struct {
	Number    int             `json:"number"`
	Title     string          `json:"title"`
	State     string          `json:"state"`
	Draft     bool            `json:"draft"`
	MergedAt  *string         `json:"merged_at"`
	HTMLURL   string          `json:"html_url"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
	Labels    []ghLabel       `json:"labels"`
	Assignees []ghUser        `json:"assignees"`
	User      ghUser          `json:"user"`
	Head      ghPullBranch    `json:"head"`
	Base      ghPullBranch    `json:"base"`
	Raw       json.RawMessage `json:"-"`
}

type ghPullBranch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

func (p *ghPullItem) toPRIndex(commentCount int) ghPRIndexEntry {
	labels := make([]string, 0, len(p.Labels))
	for _, l := range p.Labels {
		labels = append(labels, l.Name)
	}
	assignees := make([]string, 0, len(p.Assignees))
	for _, a := range p.Assignees {
		assignees = append(assignees, a.Login)
	}
	return ghPRIndexEntry{
		Number:       p.Number,
		Title:        p.Title,
		State:        p.State,
		Merged:       p.MergedAt != nil && *p.MergedAt != "",
		Draft:        p.Draft,
		HeadRef:      p.Head.Ref,
		BaseRef:      p.Base.Ref,
		HeadSHA:      p.Head.SHA,
		BaseSHA:      p.Base.SHA,
		Labels:       labels,
		Assignees:    assignees,
		Author:       p.User.Login,
		CommentCount: commentCount,
		CreatedAt:    p.CreatedAt,
		UpdatedAt:    p.UpdatedAt,
		HTMLURL:      p.HTMLURL,
	}
}

// ghListIssuesAll paginates repos/OWNER/REPO/issues until the API returns an
// empty page. state=all so we surface closed items too — the frontend can
// filter by open/closed on the cached index.
func ghListIssuesAll(ctx context.Context, repo ghRepo) ([]ghRawItem, error) {
	var out []ghRawItem
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/issues?state=all&per_page=100&page=%d", repo.Owner, repo.Name, page)
		body, err := ghExecFn(ctx, "api", path)
		if err != nil {
			return nil, err
		}
		var raws []json.RawMessage
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, err
		}
		if len(raws) == 0 {
			break
		}
		for _, raw := range raws {
			var item ghRawItem
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			item.Raw = raw
			out = append(out, item)
		}
		if len(raws) < 100 {
			break
		}
	}
	return out, nil
}

// ghListPullsAll paginates the /pulls endpoint.
func ghListPullsAll(ctx context.Context, repo ghRepo) ([]ghPullItem, error) {
	var out []ghPullItem
	for page := 1; ; page++ {
		path := fmt.Sprintf("repos/%s/%s/pulls?state=all&per_page=100&page=%d", repo.Owner, repo.Name, page)
		body, err := ghExecFn(ctx, "api", path)
		if err != nil {
			return nil, err
		}
		var raws []json.RawMessage
		if err := json.Unmarshal(body, &raws); err != nil {
			return nil, err
		}
		if len(raws) == 0 {
			break
		}
		for _, raw := range raws {
			var item ghPullItem
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			item.Raw = raw
			out = append(out, item)
		}
		if len(raws) < 100 {
			break
		}
	}
	return out, nil
}

func ghListIssueComments(ctx context.Context, repo ghRepo, n int) ([]json.RawMessage, error) {
	path := fmt.Sprintf("repos/%s/%s/issues/%d/comments?per_page=100", repo.Owner, repo.Name, n)
	body, err := ghExecFn(ctx, "api", "--paginate", path)
	if err != nil {
		return nil, err
	}
	return decodeConcatenatedArrays(body)
}

func ghListPRReviews(ctx context.Context, repo ghRepo, n int) ([]json.RawMessage, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/reviews?per_page=100", repo.Owner, repo.Name, n)
	body, err := ghExecFn(ctx, "api", "--paginate", path)
	if err != nil {
		return nil, err
	}
	return decodeConcatenatedArrays(body)
}

func ghListPRReviewComments(ctx context.Context, repo ghRepo, n int) ([]json.RawMessage, error) {
	path := fmt.Sprintf("repos/%s/%s/pulls/%d/comments?per_page=100", repo.Owner, repo.Name, n)
	body, err := ghExecFn(ctx, "api", "--paginate", path)
	if err != nil {
		return nil, err
	}
	return decodeConcatenatedArrays(body)
}

// decodeConcatenatedArrays turns `gh api --paginate`'s output (which is
// concatenated JSON arrays: `[...][...][...]`) into a single flat slice.
// A single non-paginated response is a plain `[...]`, which the decoder
// also handles as one iteration of the loop.
func decodeConcatenatedArrays(body []byte) ([]json.RawMessage, error) {
	var out []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	for {
		var page []json.RawMessage
		if err := dec.Decode(&page); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		out = append(out, page...)
	}
}

func ghDefaultBranch(ctx context.Context, repo ghRepo) string {
	body, err := ghExecFn(ctx, "api", fmt.Sprintf("repos/%s/%s", repo.Owner, repo.Name))
	if err != nil {
		return ""
	}
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.DefaultBranch
}

// ---------------------------------------------------------------------------
// Server plumbing
// ---------------------------------------------------------------------------

// ghService is the per-repo handle. One is created per (owner, name) shard
// the first time an HTTP request resolves to that repo. Subsequent requests
// for the same repo reuse the entry from the registry so the syncer's mutex
// is process-wide rather than per-request.
//
// rootDir is the git top-level of the *agent's* cwd (used by handleGHPRDiff
// to run `git diff` and `git fetch` in the right working tree). It is not
// the same as the cache root, which is anchored to the *server's* repo — see
// the comment on newGHCache. Keeping them separate lets a single server
// mirror many agent repos without polluting each one with .collectif/ dirs.
type ghService struct {
	cache   *ghCache
	syncer  *ghSyncer
	rootDir string // git top-level of the resolved agent cwd
}

// ghServiceRegistry lazily hands out per-repo ghService values. The map is
// keyed by the same shard string used in the cache path (owner + "__" + name)
// so two cwds pointing at the same origin share one entry.
type ghServiceRegistry struct {
	mu       sync.Mutex
	services map[string]*ghService
}

var services = &ghServiceRegistry{services: map[string]*ghService{}}

// ghServerCwdFn lets tests override the working directory used for repo
// resolution when no ?agent / ?cwd query param is supplied. Production code
// returns "." (the server's own cwd).
var ghServerCwdFn = func() string { return "." }

// ghRepoRootFn and ghResolveOriginFn are the git-shell-out seams. Tests
// override them so seedCache doesn't need a real git repo on disk.
var (
	ghRepoRootFn      = repoRoot
	ghResolveOriginFn = resolveOriginRepo
)

// forCwd returns (creating if needed) the ghService for the repo whose
// origin resolves from cwd. cwd == "" means "use the server's cwd" — the
// original single-repo behaviour that keeps existing callers/tests working.
//
// The cache root stays anchored to the SERVER's repo root even when cwd
// points at some other project. That's intentional (see newGHCache): we
// don't want to spray .collectif/ dirs into arbitrary project checkouts
// the user browses. rootDir, on the other hand, is the git top-level of
// the resolved cwd so PR diffs run against refs that actually exist there.
func (reg *ghServiceRegistry) forCwd(cwd string) (*ghService, error) {
	if cwd == "" {
		cwd = ghServerCwdFn()
	}
	repo, err := ghResolveOriginFn(cwd)
	if err != nil {
		return nil, err
	}
	shard := repo.Owner + "__" + repo.Name

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if svc, ok := reg.services[shard]; ok {
		return svc, nil
	}

	cacheRootAbove := ghRepoRootFn(ghServerCwdFn()) // server-anchored — see doc above
	cache, err := newGHCache(cacheRootAbove, repo)
	if err != nil {
		return nil, err
	}
	if err := cache.ensureDirs(); err != nil {
		return nil, err
	}
	svc := &ghService{
		cache:   cache,
		syncer:  newGHSyncer(cache, repo),
		rootDir: ghRepoRootFn(cwd), // agent-anchored — used by git diff/fetch
	}
	reg.services[shard] = svc
	return svc, nil
}

// httpError bundles an HTTP status code with an error so per-request
// resolution can distinguish 404 (agent not found) from 500 (repo resolve
// failed). Handlers unwrap it when writing the response.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

// writeServiceError sends err as an HTTP response. It handles httpError
// (JSON body with the {"error": ...} shape the frontend expects) and falls
// back to http.Error for the generic 500 case so we don't break existing
// callers relying on plain-text error bodies.
func writeServiceError(w http.ResponseWriter, err error) {
	var he *httpError
	if errors.As(err, &he) {
		writeJSON(w, he.status, map[string]any{"error": he.msg})
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// serviceForRequest picks the right ghService for this HTTP call.
// Priority: ?agent=<id> (session lookup) > ?cwd=<path> > server cwd.
func serviceForRequest(r *http.Request) (*ghService, error) {
	q := r.URL.Query()
	if id := q.Get("agent"); id != "" {
		s := getSession(id)
		if s == nil {
			return nil, &httpError{status: http.StatusNotFound, msg: "agent not found: " + id}
		}
		return services.forCwd(s.Cwd)
	}
	if cwd := q.Get("cwd"); cwd != "" {
		return services.forCwd(cwd)
	}
	return services.forCwd("")
}

// resetGHServiceForTest wipes the process-wide registry so a test can point
// it at a fresh temp dir and stub gh. Test-only.
func resetGHServiceForTest() {
	services = &ghServiceRegistry{services: map[string]*ghService{}}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// handleGHStatus reports which repo we're mirroring, when we last synced,
// and whether a sync is currently in flight. pendingCount is always 0 in
// slice A — slice D adds the local write queue.
func handleGHStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc, err := serviceForRequest(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	syncing, _, _ := svc.syncer.status()
	var meta ghRepoMeta
	_, _ = readJSON(svc.cache.repoMetaPath(), &meta)

	// Prefer the syncer's live repo identity — the meta.json only exists
	// after the first successful sync.
	repo := svc.syncer.repo
	writeJSON(w, http.StatusOK, map[string]any{
		"repo": map[string]string{
			"owner": repo.Owner,
			"name":  repo.Name,
		},
		"defaultBranch": meta.DefaultBranch,
		"lastSyncAt":    meta.LastSyncAt,
		"syncing":       syncing,
		"pendingCount":  0,
	})
}

// handleGHSync kicks off a background sync (or blocks when ?wait=1 so
// tests can assert the resulting cache). Coalesces: if a sync is already
// running, returns 200 with started=false rather than starting a second.
func handleGHSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc, err := serviceForRequest(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	s := svc.syncer
	if !s.tryStart() {
		writeJSON(w, http.StatusOK, map[string]any{"started": false, "reason": "already syncing"})
		return
	}
	wait := r.URL.Query().Get("wait") == "1"
	if wait {
		err := s.runSync(r.Context())
		s.finish(err)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"started": true, "wait": true})
		return
	}
	go func() {
		err := s.runSync(context.Background())
		s.finish(err)
	}()
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

// handleGHIssues serves the filtered index. All filtering happens in-memory
// against the cached JSON — no network. Empty state slice means "all".
func handleGHIssues(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc, err := serviceForRequest(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var entries []ghIssueIndexEntry
	found, err := readJSON(svc.cache.issueIndexPath(), &entries)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		entries = []ghIssueIndexEntry{}
	}
	filtered := filterIssueEntries(entries, r.URL.Query())
	writeJSON(w, http.StatusOK, map[string]any{
		"issues": filtered,
		"total":  len(filtered),
	})
}

func handleGHIssueByNumber(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc, err := serviceForRequest(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	n, ok := parseTrailingNumber(r.URL.Path, "/api/gh/issues/")
	if !ok {
		http.Error(w, "invalid issue number", http.StatusBadRequest)
		return
	}
	serveJSONFile(w, svc.cache.issuePath(n))
}

func handleGHPRs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc, err := serviceForRequest(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	var entries []ghPRIndexEntry
	found, err := readJSON(svc.cache.prIndexPath(), &entries)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		entries = []ghPRIndexEntry{}
	}
	filtered := filterPREntries(entries, r.URL.Query())
	writeJSON(w, http.StatusOK, map[string]any{
		"prs":   filtered,
		"total": len(filtered),
	})
}

// handleGHPRSubpath multiplexes /api/gh/prs/{n} and /api/gh/prs/{n}/diff.
func handleGHPRSubpath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	svc, err := serviceForRequest(r)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/gh/prs/")
	if rest == "" {
		// Bare /api/gh/prs/ falls through to the list handler upstream.
		handleGHPRs(w, r)
		return
	}
	numStr := rest
	sub := ""
	if i := strings.Index(rest, "/"); i > 0 {
		numStr = rest[:i]
		sub = rest[i+1:]
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n <= 0 {
		http.Error(w, "invalid pr number", http.StatusBadRequest)
		return
	}
	switch sub {
	case "":
		serveJSONFile(w, svc.cache.prPath(n))
	case "diff":
		handleGHPRDiff(w, r, svc, n)
	default:
		http.Error(w, "unknown pr subpath", http.StatusNotFound)
	}
}

// handleGHPRDiff serves a unified diff for PR n. Prefers a cached diff file
// (fresh vs the cached PR's updated_at); otherwise computes with local git,
// fetching the PR ref on demand when the head SHA isn't present.
//
// svc is passed by handleGHPRSubpath so the caller only resolves the service
// once (and, crucially, we run git in svc.rootDir — the agent's git top-level
// — not the server's cwd, so PR refs from that repo actually exist).
func handleGHPRDiff(w http.ResponseWriter, r *http.Request, svc *ghService, n int) {
	cache := svc.cache
	// Load cached PR — we need base/head SHAs to compute the diff anyway.
	var pr map[string]any
	found, err := readJSON(cache.prPath(n), &pr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(w, "pr not cached", http.StatusNotFound)
		return
	}
	updatedAt, _ := pr["updated_at"].(string)
	diffPath := cache.diffPath(n)
	if fresh, err := diffFileFresh(diffPath, updatedAt); err == nil && fresh {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		f, err := os.Open(diffPath)
		if err == nil {
			defer f.Close()
			_, _ = io.Copy(w, f)
			return
		}
	}

	head := extractPRSHA(pr, "head")
	base := extractPRSHA(pr, "base")
	if head == "" || base == "" {
		http.Error(w, "pr missing head/base sha", http.StatusInternalServerError)
		return
	}

	diff, err := computePRDiff(r.Context(), svc.rootDir, n, base, head)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// Best-effort cache; a write failure still succeeds the response.
	_ = writeAtomic(diffPath, diff)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(diff)
}

// extractPRSHA pulls head.sha or base.sha out of the cached PR blob.
func extractPRSHA(pr map[string]any, key string) string {
	if v, ok := pr[key].(map[string]any); ok {
		if s, ok := v["sha"].(string); ok {
			return s
		}
	}
	return ""
}

// diffFileFresh returns true if the cached diff was written after the PR's
// updated_at timestamp. Any parse failure returns (false, nil) so we fall
// through to a live recompute rather than serving a possibly-stale blob.
func diffFileFresh(path, updatedAt string) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if updatedAt == "" {
		return true, nil
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return false, nil
	}
	return st.ModTime().After(t), nil
}

// computePRDiff runs `git diff base..head`. Missing either ref triggers a
// `git fetch origin pull/N/head:refs/collectif/pr-N` and one retry.
func computePRDiff(ctx context.Context, cwd string, n int, base, head string) ([]byte, error) {
	if cwd == "" {
		cwd = "."
	}
	out, err := gitDiff(ctx, cwd, base, head)
	if err == nil {
		return out, nil
	}
	// Try fetching the PR ref, then retry once.
	if fErr := gitFetchPRRef(ctx, cwd, n); fErr != nil {
		return nil, fmt.Errorf("git diff failed and fetch fallback failed: diff=%v fetch=%v", err, fErr)
	}
	out, err = gitDiff(ctx, cwd, base, head)
	if err != nil {
		return nil, fmt.Errorf("git diff still failed after fetch: %w", err)
	}
	return out, nil
}

func gitDiff(ctx context.Context, cwd, base, head string) ([]byte, error) {
	sub, cancel := context.WithTimeout(ctx, ghExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(sub, "git", "-C", cwd, "diff", base+".."+head)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git diff %s..%s: %w: %s", base, head, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func gitFetchPRRef(ctx context.Context, cwd string, n int) error {
	sub, cancel := context.WithTimeout(ctx, ghExecTimeout)
	defer cancel()
	spec := fmt.Sprintf("pull/%d/head:refs/collectif/pr-%d", n, n)
	cmd := exec.CommandContext(sub, "git", "-C", cwd, "fetch", "origin", spec)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch %s: %w: %s", spec, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// serveJSONFile streams the raw JSON body of a cache file. 404 if missing.
func serveJSONFile(w http.ResponseWriter, path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not cached", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// parseTrailingNumber extracts the number at the end of an /api/... path.
// Returns (0, false) if the segment is missing, non-numeric, or has extra
// path components (a caller wanting {n}/subpath should split first).
func parseTrailingNumber(p, prefix string) (int, bool) {
	rest := strings.TrimPrefix(p, prefix)
	if rest == "" || strings.Contains(rest, "/") {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// ---------------------------------------------------------------------------
// Filtering
// ---------------------------------------------------------------------------

// filterIssueEntries applies query-string filters to the cached index. All
// filters are AND-ed together. state=all is the default when unspecified.
func filterIssueEntries(entries []ghIssueIndexEntry, q map[string][]string) []ghIssueIndexEntry {
	state := firstOrDefault(q, "state", "all")
	label := firstOrDefault(q, "label", "")
	assignee := firstOrDefault(q, "assignee", "")
	text := strings.ToLower(firstOrDefault(q, "q", ""))

	out := make([]ghIssueIndexEntry, 0, len(entries))
	for _, e := range entries {
		if state != "all" && !strings.EqualFold(e.State, state) {
			continue
		}
		if label != "" && !containsFold(e.Labels, label) {
			continue
		}
		if assignee != "" && !containsFold(e.Assignees, assignee) {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(e.Title), text) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func filterPREntries(entries []ghPRIndexEntry, q map[string][]string) []ghPRIndexEntry {
	state := firstOrDefault(q, "state", "all")
	label := firstOrDefault(q, "label", "")
	assignee := firstOrDefault(q, "assignee", "")
	text := strings.ToLower(firstOrDefault(q, "q", ""))

	out := make([]ghPRIndexEntry, 0, len(entries))
	for _, e := range entries {
		if state != "all" && !strings.EqualFold(e.State, state) {
			continue
		}
		if label != "" && !containsFold(e.Labels, label) {
			continue
		}
		if assignee != "" && !containsFold(e.Assignees, assignee) {
			continue
		}
		if text != "" && !strings.Contains(strings.ToLower(e.Title), text) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func firstOrDefault(q map[string][]string, key, def string) string {
	if vs, ok := q[key]; ok && len(vs) > 0 && vs[0] != "" {
		return vs[0]
	}
	return def
}

func containsFold(hay []string, needle string) bool {
	for _, s := range hay {
		if strings.EqualFold(s, needle) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// registerGHRoutes wires the /api/gh/* handlers onto a mux. Called from main.
// Kept as a plain func so a future server.go extraction is a mechanical move.
func registerGHRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/gh/status", handleGHStatus)
	mux.HandleFunc("/api/gh/sync", handleGHSync)
	mux.HandleFunc("/api/gh/issues", handleGHIssues)
	mux.HandleFunc("/api/gh/issues/", handleGHIssueByNumber)
	mux.HandleFunc("/api/gh/prs", handleGHPRs)
	mux.HandleFunc("/api/gh/prs/", handleGHPRSubpath)
}
