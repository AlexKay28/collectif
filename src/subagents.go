package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SubagentFile represents one .claude/agents/<name>.md file. Claude Code
// reads name/description/tools/model from the YAML frontmatter; the
// `subagents` field is our own extension for encoding delegation edges
// (Claude Code silently ignores unknown frontmatter fields). Scope
// distinguishes project (<cwd>/.claude/agents) from user (~/.claude/agents)
// and is set at read time — not persisted in the file itself.
type SubagentFile struct {
	Name        string   `json:"name" yaml:"name"`
	Description string   `json:"description" yaml:"description,omitempty"`
	Model       string   `json:"model" yaml:"model,omitempty"`
	Tools       []string `json:"tools" yaml:"tools,omitempty"`
	Subagents   []string `json:"subagents" yaml:"subagents,omitempty"`
	Prompt      string   `json:"prompt" yaml:"-"`
	Scope       string   `json:"scope,omitempty" yaml:"-"`
}

var subagentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// agentsDir returns the resolved absolute path to <cwd>/.claude/agents and
// verifies it is contained within the session's cwd (defensive against
// symlink escapes). Does not require the directory to exist.
func agentsDir(cwd string) (string, error) {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	realCwd, err := filepath.EvalSymlinks(abs)
	if err != nil {
		realCwd = abs
	}
	dir := filepath.Join(realCwd, ".claude", "agents")
	rel, err := filepath.Rel(realCwd, dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("agents dir escapes cwd")
	}
	return dir, nil
}

// userAgentsDir returns ~/.claude/agents (may not exist). Returns empty
// string if the user's home directory can't be resolved.
func userAgentsDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "agents")
}

// subagentPath returns the absolute path for a subagent file at the given
// scope ("project" = <cwd>/.claude/agents, "user" = ~/.claude/agents).
// Defaults to project scope.
func subagentPath(cwd, name, scope string) (string, error) {
	if !subagentNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid subagent name (allowed: a-z 0-9 -, up to 64 chars)")
	}
	if scope == "user" {
		dir := userAgentsDir()
		if dir == "" {
			return "", fmt.Errorf("cannot resolve user home directory")
		}
		return filepath.Join(dir, name+".md"), nil
	}
	dir, err := agentsDir(cwd)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".md"), nil
}

// parseSubagentFile splits the raw markdown into frontmatter and body, then
// unmarshals the frontmatter. Files without a frontmatter block are treated
// as body-only.
func parseSubagentFile(raw []byte, filename string) (*SubagentFile, error) {
	sf := &SubagentFile{Name: strings.TrimSuffix(filename, ".md")}
	content := string(raw)
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		sf.Prompt = strings.TrimSpace(content)
		return sf, nil
	}
	// find closing '---'
	rest := content[4:]
	idx := strings.Index(rest, "\n---")
	if idx < 0 {
		sf.Prompt = strings.TrimSpace(content)
		return sf, nil
	}
	fm := rest[:idx]
	body := rest[idx+4:]
	body = strings.TrimPrefix(body, "\r")
	body = strings.TrimPrefix(body, "\n")
	if err := yaml.Unmarshal([]byte(fm), sf); err != nil {
		return nil, err
	}
	if sf.Name == "" {
		sf.Name = strings.TrimSuffix(filename, ".md")
	}
	sf.Prompt = strings.TrimSpace(body)
	return sf, nil
}

// serializeSubagentFile writes frontmatter + prompt body in the canonical
// Claude Code format so the file remains directly usable by claude.
func serializeSubagentFile(sf *SubagentFile) ([]byte, error) {
	fm, err := yaml.Marshal(sf)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n")
	b.WriteString(sf.Prompt)
	b.WriteString("\n")
	return []byte(b.String()), nil
}

// listSubagentsFor reads both project (<cwd>/.claude/agents) and user
// (~/.claude/agents) scopes. On name collision the project entry wins,
// matching Claude Code's precedence rule.
func listSubagentsFor(cwd string) ([]*SubagentFile, error) {
	byName := map[string]*SubagentFile{}

	if dir := userAgentsDir(); dir != "" {
		for _, sf := range readAgentsDir(dir) {
			sf.Scope = "user"
			byName[sf.Name] = sf
		}
	}
	if dir, err := agentsDir(cwd); err == nil {
		for _, sf := range readAgentsDir(dir) {
			sf.Scope = "project"
			byName[sf.Name] = sf
		}
	}

	out := make([]*SubagentFile, 0, len(byName))
	for _, sf := range byName {
		out = append(out, sf)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope == "project"
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func readAgentsDir(dir string) []*SubagentFile {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := []*SubagentFile{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sf, err := parseSubagentFile(raw, e.Name())
		if err != nil {
			continue
		}
		out = append(out, sf)
	}
	return out
}

// ─── HTTP handlers ─────────────────────────────────────────────────

func handleSubagentsList(w http.ResponseWriter, r *http.Request, s *Session) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	list, err := listSubagentsFor(s.Cwd)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func handleSubagentByName(w http.ResponseWriter, r *http.Request, s *Session, name string) {
	scope := r.URL.Query().Get("scope")
	if scope != "user" && scope != "project" {
		scope = "project"
	}
	path, err := subagentPath(s.Cwd, name, scope)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		raw, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		sf, err := parseSubagentFile(raw, filepath.Base(path))
		if err != nil {
			http.Error(w, "parse: "+err.Error(), http.StatusInternalServerError)
			return
		}
		sf.Scope = scope
		writeJSON(w, http.StatusOK, sf)

	case http.MethodPut:
		var sf SubagentFile
		if !decodeBody(w, r, &sf) {
			return
		}
		// Enforce name from the URL. Frontmatter name will match.
		sf.Name = name
		sf.Scope = scope
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out, err := serializeSubagentFile(&sf)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := os.WriteFile(path, out, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, &sf)

	case http.MethodDelete:
		if err := os.Remove(path); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
