package main

// PR-ready detection (issue #37).
//
// Two independent paths converge on markReviewReady():
//
//   A) Tool-use signal (primary) — fired from hooks.go on a PostToolUse
//      carrying `Bash` with a `gh pr create ...` command that exited 0.
//      We parse the last https://github.com/.../pull/N URL from stdout.
//
//   B) Git-state polling (fallback) — a package-level goroutine started
//      from main() ticks every 30s. For sessions that are idle/stopped
//      and have no PR URL yet, we check whether the current branch was
//      pushed and matches HEAD; if so, `gh pr view` reveals the URL.
//
// Both paths run with 5s timeouts and swallow errors — if gh isn't
// installed, path B silently does nothing.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// prURLRe matches https://github.com/<owner>/<repo>/pull/<n> — captures the
// full URL. We take the LAST match in a stdout blob because `gh pr create`
// prints its URL as the final line but may echo prefixes/warnings above.
var prURLRe = regexp.MustCompile(`https?://github\.com/[^\s"']+/pull/\d+`)

// extractPRURL returns the last GitHub pull URL found in s, or "".
func extractPRURL(s string) string {
	matches := prURLRe.FindAllString(s, -1)
	if len(matches) == 0 {
		return ""
	}
	url := matches[len(matches)-1]
	// Trim trailing punctuation that terminal output sometimes carries.
	url = strings.TrimRight(url, ".,);]")
	return url
}

// handleBashPostToolUse is called from hooks.go on PostToolUse events with
// tool_name == "Bash". It inspects the tool_input command + tool_response
// (exit_code, stdout) and, on `gh pr create` with exit 0, marks the session
// as review_ready.
func handleBashPostToolUse(s *Session, input map[string]any, response map[string]any) {
	if s == nil || input == nil {
		return
	}
	cmd, _ := input["command"].(string)
	if cmd == "" || !strings.Contains(cmd, "gh pr create") {
		return
	}
	// Extract exit code and stdout defensively — schema varies by version.
	exit := 0
	if v, ok := response["exit_code"]; ok {
		switch n := v.(type) {
		case float64:
			exit = int(n)
		case int:
			exit = n
		}
	}
	if exit != 0 {
		return
	}
	var stdout string
	if v, ok := response["stdout"].(string); ok {
		stdout = v
	}
	// Some hook payloads may collapse output into a single "output" field.
	if stdout == "" {
		if v, ok := response["output"].(string); ok {
			stdout = v
		}
	}
	url := extractPRURL(stdout)
	if url == "" {
		return
	}
	markReviewReady(s, url)
}

// markReviewReady sets the PR URL, flips status to review_ready, and kicks
// off an async fetch of the PR title. It's safe to call multiple times;
// the second call is a no-op if the URL matches.
func markReviewReady(s *Session, url string) {
	if s == nil || url == "" {
		return
	}
	s.mu.Lock()
	if s.PRURL == url && s.Status == "review_ready" {
		s.mu.Unlock()
		return
	}
	s.PRURL = url
	s.mu.Unlock()

	s.appendActivity(ActivityEntry{Event: "PRReady", Detail: url, Level: "info"})
	s.setStatus("review_ready", "PR opened")

	// Fire-and-forget title fetch.
	go func() {
		title := fetchPRTitle(s.Cwd, url)
		if title == "" {
			return
		}
		s.mu.Lock()
		s.PRTitle = title
		s.mu.Unlock()
		s.touch()
	}()
}

// fetchPRTitle runs `gh pr view <url> --json title -q .title` from cwd with
// a 5s timeout. Returns "" on any error.
func fetchPRTitle(cwd, url string) string {
	if cwd == "" || url == "" {
		return ""
	}
	if _, err := os.Stat(cwd); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "pr", "view", url, "--json", "title", "-q", ".title").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// checkForPR runs `gh pr view --json url,title` from the session cwd; if a
// PR exists for the current branch, calls markReviewReady.
func checkForPR(s *Session) {
	if s == nil || s.Cwd == "" {
		return
	}
	if _, err := os.Stat(s.Cwd); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", "--json", "url,title")
	cmd.Dir = s.Cwd
	out, err := cmd.Output()
	if err != nil {
		return
	}
	var view struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(out, &view); err != nil {
		return
	}
	if view.URL == "" {
		return
	}
	markReviewReady(s, view.URL)
	if view.Title != "" {
		s.mu.Lock()
		s.PRTitle = view.Title
		s.mu.Unlock()
		s.touch()
	}
}

// startPRPoller ticks every 30 seconds and, for eligible sessions, runs the
// git-state fallback detection. "Eligible" = status idle/stopped, no PR URL
// yet, cwd exists.
func startPRPoller() {
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
				pollOneSession(s)
			}
		}
	}()
}

// pollOneSession is the per-session body of the git-state fallback path.
// Wrapped in a recover so a panic (e.g. cwd deleted mid-check) can't take
// down the whole poller.
func pollOneSession(s *Session) {
	defer func() { _ = recover() }()

	s.mu.Lock()
	status := s.Status
	prURL := s.PRURL
	cwd := s.Cwd
	s.mu.Unlock()

	if prURL != "" {
		return
	}
	if status != "idle" && status != "stopped" {
		return
	}
	if cwd == "" {
		return
	}
	if _, err := os.Stat(cwd); err != nil {
		return
	}

	// New commits vs origin? If we're already up to date with remote HEAD
	// there's nothing to push.
	if !hasCommitsAheadOfRemote(cwd) {
		return
	}

	branch := currentBranch(cwd)
	if branch == "" || branch == "HEAD" {
		return
	}
	// Remote branch present and matches local HEAD?
	if !remoteHasBranchMatchingHead(cwd, branch) {
		return
	}
	checkForPR(s)
}

func hasCommitsAheadOfRemote(cwd string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "log", "--oneline", "-1", "--format=%H", "origin/HEAD..HEAD")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

func currentBranch(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func remoteHasBranchMatchingHead(cwd, branch string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// ls-remote prints "<sha>\trefs/heads/<branch>" if present.
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "origin", branch)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return false
	}
	remoteSHA := strings.Fields(line)[0]

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	cmd2 := exec.CommandContext(ctx2, "git", "-C", cwd, "rev-parse", "HEAD")
	out2, err := cmd2.Output()
	if err != nil {
		return false
	}
	localSHA := strings.TrimSpace(string(out2))
	return localSHA != "" && remoteSHA == localSHA
}
