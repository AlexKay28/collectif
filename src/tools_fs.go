package main

// tools_fs.go — the read-only built-in tools. #50 (M2 slice B).
//
// M2 gives the agent eyes and nothing else: read, glob, grep. Write access
// waits for the permission engine in M3, so this is the phase where the
// loop can be wrong without being destructive.
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

func builtinTools() []Tool {
	return []Tool{&readTool{}, &globTool{}, &grepTool{}}
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

	base := root
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
		rel, relErr := filepath.Rel(root, path)
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

// ─── Matching ───────────────────────────────────────────────────────────

// walkMatch collects paths under root matching pattern, relative to root.
// Absolute or escaping patterns match nothing rather than reaching out:
// the tool's whole surface is the working directory.
func walkMatch(root, pattern, sub string) ([]string, error) {
	base := root
	if sub != "" {
		abs, err := containedPath(root, sub)
		if err != nil {
			return nil, err
		}
		base = abs
	}
	var out []string
	err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr
		}
		if _, err := containedPath(root, path); err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
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
