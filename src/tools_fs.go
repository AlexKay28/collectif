package main

// tools_fs.go — the built-in filesystem tools. #50 (M2 slice B), extended
// by #52 (M3) with the two that can change something.
//
// M2 gave the agent eyes and nothing else: read, glob, grep. M3 adds write
// and edit, which is the point at which being wrong stops being free — so
// they arrive behind the permission engine in policy.go and behind the
// approval widget nb_approval.go already had.
//
// Two rules apply to every tool here.
//
// Containment is not a policy rule. Each path-taking tool resolves symlinks
// and verifies the result is under the notebook root before doing any I/O.
// Policy (M3) can loosen what you may *do*; it can never loosen *where*.
//
// A failure is a result, not an error. A missing file or a bad pattern
// comes back as text with isError set, because the model is the one who has
// to react to it — returning a Go error would end the turn and deny it the
// chance.

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// toolOutputBudget bounds what one tool call can put into the context
// window. Generous enough for a real file, small enough that one call
// cannot spend the whole window.
const toolOutputBudget = 24 * 1024

// maxGrepMatches keeps a broad pattern from filling the context with hits.
const maxGrepMatches = 200

// maxWriteBytes bounds what one call may put on disk. A model that
// hallucinates a loop into a `content` argument should cost a rejected tool
// call, not the disk.
const maxWriteBytes = 8 * 1024 * 1024

func builtinTools() []Tool {
	return []Tool{&readTool{}, &globTool{}, &grepTool{}, &writeTool{}, &editTool{}, &bashTool{}}
}

// previewer is a tool that can describe what a call *would* do without
// doing it.
//
// Two callers need this and neither can be served by running the tool
// first. The policy gate has to show a human the proposed diff before they
// approve it — after is too late — and the runner shows the same diff
// afterwards as the record of what happened. Computing it once and using it
// for both is also what keeps the two from disagreeing.
type previewer interface {
	Preview(in map[string]any, root string) string
}

// argBool reads a flag the same defensive way argString reads a string. A
// transport without strict schema support can still deliver "true".
func argBool(in map[string]any, key string) bool {
	switch v := in[key].(type) {
	case bool:
		return v
	case string:
		return v == "true"
	}
	return false
}

// argString/argInt read the model's arguments defensively. Strict schemas
// make malformed input unlikely, not impossible — a transport without
// strict support (M4) can still deliver anything.
func argString(in map[string]any, key string) string {
	if v, ok := in[key].(string); ok {
		return v
	}
	return ""
}

func argInt(in map[string]any, key string) int {
	switch v := in[key].(type) {
	case float64: // JSON numbers decode as float64
		return int(v)
	case int:
		return v
	}
	return 0
}

// ─── read ───────────────────────────────────────────────────────────────

type readTool struct{}

func (t *readTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "read",
		Description: "Read a file from the notebook's working directory. " +
			"Use this when you need the actual contents of a file rather than a guess about them. " +
			"Paths are relative to the working directory; reading outside it is refused.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the working directory.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "First line to return, 1-based. Omit to start at the beginning.",
				},
				"limit": map[string]any{
					"type":        "integer",
					"description": "How many lines to return. Omit to read to the end.",
				},
			},
			"required":             []string{"path"},
			"additionalProperties": false,
		},
	}
}

func (t *readTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	rel := argString(in, "path")
	if rel == "" {
		return "read: a path is required.", true, nil
	}
	abs, err := containedPath(root, rel)
	if err != nil {
		return fmt.Sprintf("read %s: %v", rel, err), true, nil
	}
	body, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Sprintf("read %s: %v", rel, err), true, nil
	}

	offset, limit := argInt(in, "offset"), argInt(in, "limit")
	text := string(body)
	if offset > 0 || limit > 0 {
		lines := strings.Split(text, "\n")
		start := 0
		if offset > 0 {
			start = offset - 1
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if limit > 0 && start+limit < end {
			end = start + limit
		}
		text = strings.Join(lines[start:end], "\n")
	}
	return elide(text, toolOutputBudget), false, nil
}

// ─── glob ───────────────────────────────────────────────────────────────

type globTool struct{}

func (t *globTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "glob",
		Description: "List files in the working directory matching a glob pattern, e.g. `**/*.go`. " +
			"Use this to find out what exists before reading. Returns paths relative to the working directory.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Glob pattern. `**` matches across directories.",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

func (t *globTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	pattern := argString(in, "pattern")
	if pattern == "" {
		return "glob: a pattern is required.", true, nil
	}
	matches, err := walkMatch(root, pattern, "")
	if err != nil {
		return fmt.Sprintf("glob %s: %v", pattern, err), true, nil
	}
	if len(matches) == 0 {
		return fmt.Sprintf("No files match %s.", pattern), false, nil
	}
	return elide(strings.Join(matches, "\n"), toolOutputBudget), false, nil
}

// ─── grep ───────────────────────────────────────────────────────────────

type grepTool struct{}

func (t *grepTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "grep",
		Description: "Search file contents in the working directory with a regular expression. " +
			"Use this to locate where something is defined or used before reading whole files. " +
			"Returns matching lines as path:line:text.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{
					"type":        "string",
					"description": "Go/RE2 regular expression to search for.",
				},
				"path": map[string]any{
					"type":        "string",
					"description": "Subdirectory to search, relative to the working directory. Omit to search all of it.",
				},
				"glob": map[string]any{
					"type":        "string",
					"description": "Only search files matching this glob, e.g. `**/*.go`.",
				},
			},
			"required":             []string{"pattern"},
			"additionalProperties": false,
		},
	}
}

func (t *grepTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	pattern := argString(in, "pattern")
	if pattern == "" {
		return "grep: a pattern is required.", true, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("grep: invalid pattern %q: %v", pattern, err), true, nil
	}

	// Walk and report against the *resolved* root. containedPath resolves
	// symlinks, so mixing a resolved walk with an unresolved root made
	// filepath.Rel return ../../real/... under a symlinked root: the glob
	// filter then matched nothing and the paths printed were ones the model
	// could not feed back into read.
	resolvedRoot, err := containedPath(root, ".")
	if err != nil {
		return fmt.Sprintf("grep: %v", err), true, nil
	}
	base := resolvedRoot
	if sub := argString(in, "path"); sub != "" {
		abs, err := containedPath(root, sub)
		if err != nil {
			return fmt.Sprintf("grep %s: %v", sub, err), true, nil
		}
		base = abs
	}
	globPat := argString(in, "glob")

	var hits []string
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		if len(hits) >= maxGrepMatches {
			return fs.SkipAll
		}
		// Re-check containment per file: WalkDir follows the tree from base,
		// and base itself was checked, but a symlinked file inside it has
		// not been.
		if _, err := containedPath(root, path); err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(resolvedRoot, path)
		if relErr != nil {
			return nil
		}
		if globPat != "" && !matchGlob(globPat, rel) {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for line := 1; sc.Scan(); line++ {
			if re.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d:%s", rel, line, strings.TrimSpace(sc.Text())))
				if len(hits) >= maxGrepMatches {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Sprintf("grep: %v", err), true, nil
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No matches for %s.", pattern), false, nil
	}
	out := strings.Join(hits, "\n")
	if len(hits) >= maxGrepMatches {
		out += fmt.Sprintf("\n… stopped at %d matches; narrow the pattern …", maxGrepMatches)
	}
	return elide(out, toolOutputBudget), false, nil
}

// ─── write ──────────────────────────────────────────────────────────────

type writeTool struct{}

func (t *writeTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "write",
		Description: "Create a file, or replace one entirely, inside the notebook's working directory. " +
			"Use this for a new file or a rewrite; to change part of an existing file use `edit` instead, " +
			"which shows a smaller diff and cannot lose the rest of the file. " +
			"Writing outside the working directory is refused.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the working directory. Parent directories are created.",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "The complete new contents of the file.",
				},
			},
			"required":             []string{"path", "content"},
			"additionalProperties": false,
		},
	}
}

func (t *writeTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	rel := argString(in, "path")
	if rel == "" {
		return "write: a path is required.", true, nil
	}
	// An absent content key is not an empty one. A missing field truncating
	// a file is an accident nobody notices until the file is needed, so the
	// two are kept distinguishable even though the schema requires it.
	raw, ok := in["content"]
	if !ok {
		return "write: a content field is required. To empty a file, pass an explicit empty string.", true, nil
	}
	content, ok := raw.(string)
	if !ok {
		return "write: content must be a string.", true, nil
	}
	if len(content) > maxWriteBytes {
		return fmt.Sprintf("write %s: content is %d bytes, over the %d-byte limit for one call.",
			rel, len(content), maxWriteBytes), true, nil
	}

	// Containment before any I/O, and before the file is opened rather than
	// after: opening with O_TRUNC and *then* refusing would destroy the
	// target it was refusing to touch.
	abs, err := containedPath(root, rel)
	if err != nil {
		return fmt.Sprintf("write %s: %v", rel, err), true, nil
	}
	existed := true
	if _, statErr := os.Stat(abs); statErr != nil {
		existed = false
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Sprintf("write %s: %v", rel, err), true, nil
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("write %s: %v", rel, err), true, nil
	}

	verb := "Created"
	if existed {
		verb = "Replaced"
	}
	return fmt.Sprintf("%s %s (%d lines, %d bytes).", verb, rel, countLines(content), len(content)), false, nil
}

func (t *writeTool) Preview(in map[string]any, root string) string {
	rel := argString(in, "path")
	content, ok := in["content"].(string)
	if rel == "" || !ok {
		return ""
	}
	abs, err := containedPath(root, rel)
	if err != nil {
		return ""
	}
	old, _ := os.ReadFile(abs) // a missing file previews as a creation
	return unifiedDiff(rel, string(old), content)
}

// ─── edit ───────────────────────────────────────────────────────────────

type editTool struct{}

func (t *editTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "edit",
		Description: "Replace an exact string in a file inside the notebook's working directory. " +
			"Use this to change part of a file you have read: it is refused unless old_string appears exactly once, " +
			"so it cannot silently edit the wrong place. Include enough surrounding text to make the match unique.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the working directory.",
				},
				"old_string": map[string]any{
					"type":        "string",
					"description": "The exact text to replace, including whitespace and indentation.",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "What to put in its place.",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "Replace every occurrence instead of requiring exactly one.",
				},
			},
			"required":             []string{"path", "old_string", "new_string"},
			"additionalProperties": false,
		},
	}
}

func (t *editTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	abs, body, next, msg := editPlan(in, root)
	if msg != "" {
		return msg, true, nil
	}
	if err := os.WriteFile(abs, []byte(next), 0o644); err != nil {
		return fmt.Sprintf("edit %s: %v", argString(in, "path"), err), true, nil
	}
	n := strings.Count(body, argString(in, "old_string"))
	if !argBool(in, "replace_all") {
		n = 1
	}
	return fmt.Sprintf("Edited %s (%d replacement%s).", argString(in, "path"), n, plural(n)), false, nil
}

func (t *editTool) Preview(in map[string]any, root string) string {
	_, body, next, msg := editPlan(in, root)
	if msg != "" {
		return ""
	}
	return unifiedDiff(argString(in, "path"), body, next)
}

// editPlan resolves and validates an edit without performing it, so Run and
// Preview cannot disagree about what the call means. msg is non-empty when
// the call is refused, and is the text the model gets.
//
// Everything it refuses, it refuses *before* opening the file for writing:
// an edit that fails must leave the file exactly as it was, because the
// model's next move is to re-read it and a half-applied edit would send it
// somewhere neither of us intended.
func editPlan(in map[string]any, root string) (abs, body, next, msg string) {
	rel := argString(in, "path")
	if rel == "" {
		return "", "", "", "edit: a path is required."
	}
	old := argString(in, "old_string")
	if old == "" {
		return "", "", "", "edit: old_string is required and cannot be empty. To replace a whole file, use write."
	}
	replacement := argString(in, "new_string")
	if replacement == old {
		return "", "", "", "edit: old_string and new_string are identical, so there is nothing to change."
	}

	abs, err := containedPath(root, rel)
	if err != nil {
		return "", "", "", fmt.Sprintf("edit %s: %v", rel, err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return "", "", "", fmt.Sprintf("edit %s: %v", rel, err)
	}
	body = string(raw)

	n := strings.Count(body, old)
	switch {
	case n == 0:
		return "", "", "", fmt.Sprintf("edit %s: %q does not appear in the file. Read it first — "+
			"whitespace and indentation are part of the match.", rel, elide(old, 200))
	case n > 1 && !argBool(in, "replace_all"):
		// Taking the first match would put the edit somewhere the model did
		// not look at, and the failure surfaces long after the run.
		return "", "", "", fmt.Sprintf("edit %s: %q appears %d times. Include more surrounding text to "+
			"identify which one, or pass replace_all.", rel, elide(old, 200), n)
	}

	if argBool(in, "replace_all") {
		next = strings.ReplaceAll(body, old, replacement)
	} else {
		next = strings.Replace(body, old, replacement, 1)
	}
	return abs, body, next, ""
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

// ─── Matching ───────────────────────────────────────────────────────────

// walkMatch collects paths under root matching pattern, relative to root.
// Absolute or escaping patterns match nothing rather than reaching out:
// the tool's whole surface is the working directory.
func walkMatch(root, pattern, sub string) ([]string, error) {
	resolvedRoot, err := containedPath(root, ".")
	if err != nil {
		return nil, err
	}
	base := resolvedRoot
	if sub != "" {
		abs, err := containedPath(root, sub)
		if err != nil {
			return nil, err
		}
		base = abs
	}
	var out []string
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr
		}
		if _, err := containedPath(root, path); err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(resolvedRoot, path)
		if relErr != nil {
			return nil
		}
		if matchGlob(pattern, rel) {
			out = append(out, rel)
		}
		return nil
	})
	return out, err
}

// matchGlob supports `**` across directory separators, which
// filepath.Match does not. Everything else is delegated to it.
func matchGlob(pattern, name string) bool {
	if pattern == "" {
		return true
	}
	// A pattern that is absolute or climbs out can never match a path we
	// express relative to the root.
	if filepath.IsAbs(pattern) || strings.HasPrefix(pattern, "..") {
		return false
	}
	if !strings.Contains(pattern, "**") {
		if ok, _ := filepath.Match(pattern, name); ok {
			return true
		}
		// A bare pattern like *.go should also match nested files, which is
		// what people expect from a repo search.
		if ok, _ := filepath.Match(pattern, filepath.Base(name)); ok && !strings.Contains(pattern, "/") {
			return true
		}
		return false
	}
	re, err := regexp.Compile(globToRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

func globToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				// `**/` may match zero directories, so `**/*.go` finds
				// top-level files too.
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return b.String()
}
