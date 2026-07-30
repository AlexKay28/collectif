package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// writeFile is a test helper that fails loudly if disk setup goes wrong —
// nothing else in the suite is worth running if the fixture can't be built.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestProjectAgentsDir_FindsClosestAncestorWithFiles(t *testing.T) {
	root := t.TempDir()
	// Ancestor has an agents dir with a .md file
	ancestorAgents := filepath.Join(root, ".claude", "agents")
	writeFile(t, filepath.Join(ancestorAgents, "helper.md"), "---\nname: helper\n---\nbody")

	// cwd is deep inside the ancestor
	cwd := filepath.Join(root, "sub", "deep")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	got, err := projectAgentsDir(cwd)
	if err != nil {
		t.Fatalf("projectAgentsDir: %v", err)
	}
	wantSuffix := filepath.Join(".claude", "agents")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("got %q, want suffix %q", got, wantSuffix)
	}
	// It should resolve to the ancestor's dir, not the cwd's non-existent one.
	if resolved, err := filepath.EvalSymlinks(ancestorAgents); err == nil {
		if got != resolved {
			t.Fatalf("got %q, want %q (ancestor with files)", got, resolved)
		}
	}
}

func TestProjectAgentsDir_SkipsEmptyAncestorAndPrefersFilledOne(t *testing.T) {
	root := t.TempDir()
	// Nearer ancestor: empty agents dir
	emptyDir := filepath.Join(root, "near", ".claude", "agents")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	// Farther ancestor: has an agent file
	farAgents := filepath.Join(root, ".claude", "agents")
	writeFile(t, filepath.Join(farAgents, "pm.md"), "---\nname: pm\n---\nbody")

	cwd := filepath.Join(root, "near", "sub")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	got, err := projectAgentsDir(cwd)
	if err != nil {
		t.Fatalf("projectAgentsDir: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(farAgents); got != resolved {
		t.Fatalf("got %q, want %q (should prefer ancestor with .md files)", got, resolved)
	}
}

func TestProjectAgentsDir_FallsBackToFirstEmptyWhenNoneHaveFiles(t *testing.T) {
	root := t.TempDir()
	emptyDir := filepath.Join(root, ".claude", "agents")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cwd := filepath.Join(root, "child")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}
	got, err := projectAgentsDir(cwd)
	if err != nil {
		t.Fatalf("projectAgentsDir: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(emptyDir); got != resolved {
		t.Fatalf("got %q, want %q (first empty)", got, resolved)
	}
}

func TestProjectAgentsDir_IgnoresUnderscorePrefixedFiles(t *testing.T) {
	root := t.TempDir()
	agents := filepath.Join(root, ".claude", "agents")
	// Only _hidden.md — dirHasAgentFiles must skip it.
	writeFile(t, filepath.Join(agents, "_hidden.md"), "---\nname: hidden\n---\nbody")

	got, err := projectAgentsDir(root)
	if err != nil {
		t.Fatalf("projectAgentsDir: %v", err)
	}
	// Since no ancestor has a *visible* .md file, we fall back to the first
	// existing empty dir. That's still `agents` here, resolved through symlinks.
	if resolved, _ := filepath.EvalSymlinks(agents); got != resolved {
		t.Fatalf("got %q, want %q", got, resolved)
	}
}

func TestSubagentPath_RejectsInvalidNames(t *testing.T) {
	cwd := t.TempDir()
	bad := []string{"", "-leading-dash", "has spaces", "with/slash", "..", strings.Repeat("a", 65)}
	for _, name := range bad {
		if _, err := subagentPath(cwd, name, "project"); err == nil {
			t.Errorf("expected error for name %q, got nil", name)
		}
	}
}

func TestSubagentPath_AcceptsValidNames(t *testing.T) {
	cwd := t.TempDir()
	good := []string{"a", "PM", "MDEV-ios", "SDEV", "worker_1", "a" + strings.Repeat("b", 63)}
	for _, name := range good {
		if _, err := subagentPath(cwd, name, "project"); err != nil {
			t.Errorf("expected ok for name %q, got %v", name, err)
		}
	}
}

func TestSubagentPath_ScopeDefaultsToProject(t *testing.T) {
	cwd := t.TempDir()
	got, err := subagentPath(cwd, "helper", "" /* unrecognized → project */)
	if err != nil {
		t.Fatalf("subagentPath: %v", err)
	}
	if !strings.Contains(got, filepath.Join(".claude", "agents")) {
		t.Fatalf("got %q, expected project path under .claude/agents", got)
	}
}

func TestParseSubagentFile_WithFrontmatter(t *testing.T) {
	raw := []byte("---\nname: pm\ndescription: product manager\nmodel: opus\ntools:\n  - Read\n  - Grep\nsubagents:\n  - dev\n  - qa\n---\nHello, I am the PM agent.\n")
	sf, err := parseSubagentFile(raw, "pm.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sf.Name != "pm" || sf.Description != "product manager" || sf.Model != "opus" {
		t.Errorf("frontmatter fields wrong: %+v", sf)
	}
	if !reflect.DeepEqual(sf.Tools, []string{"Read", "Grep"}) {
		t.Errorf("tools: got %v", sf.Tools)
	}
	if !reflect.DeepEqual(sf.Subagents, []string{"dev", "qa"}) {
		t.Errorf("subagents: got %v", sf.Subagents)
	}
	if sf.Prompt != "Hello, I am the PM agent." {
		t.Errorf("prompt: got %q", sf.Prompt)
	}
}

func TestParseSubagentFile_NoFrontmatterUsesFilenameAsName(t *testing.T) {
	raw := []byte("just a body\nno frontmatter here\n")
	sf, err := parseSubagentFile(raw, "solo.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sf.Name != "solo" {
		t.Errorf("name: got %q, want %q", sf.Name, "solo")
	}
	if sf.Prompt != "just a body\nno frontmatter here" {
		t.Errorf("prompt: got %q", sf.Prompt)
	}
}

func TestParseSubagentFile_MissingClosingFenceTreatedAsBodyOnly(t *testing.T) {
	raw := []byte("---\nname: broken\nno closing fence\n")
	sf, err := parseSubagentFile(raw, "broken.md")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Name comes from filename since we couldn't parse frontmatter.
	if sf.Name != "broken" {
		t.Errorf("name: got %q", sf.Name)
	}
	// Whole thing (trimmed) ends up in prompt.
	if !strings.Contains(sf.Prompt, "no closing fence") {
		t.Errorf("prompt should retain body; got %q", sf.Prompt)
	}
}

func TestSerializeSubagentFile_RoundTrip(t *testing.T) {
	orig := &SubagentFile{
		Name:        "dev",
		Description: "senior dev",
		Model:       "sonnet",
		Tools:       []string{"Read", "Edit"},
		Subagents:   []string{"qa"},
		Prompt:      "You are a senior dev.\nWrite good code.",
	}
	out, err := serializeSubagentFile(orig)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !strings.HasPrefix(string(out), "---\n") {
		t.Errorf("expected leading fence, got %q", string(out[:10]))
	}

	back, err := parseSubagentFile(out, "dev.md")
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if back.Name != orig.Name || back.Description != orig.Description || back.Model != orig.Model {
		t.Errorf("meta mismatch after round-trip: %+v", back)
	}
	if !reflect.DeepEqual(back.Tools, orig.Tools) || !reflect.DeepEqual(back.Subagents, orig.Subagents) {
		t.Errorf("slices mismatch: tools=%v subs=%v", back.Tools, back.Subagents)
	}
	if back.Prompt != orig.Prompt {
		t.Errorf("prompt mismatch: got %q want %q", back.Prompt, orig.Prompt)
	}
}

// TestListSubagentsFor_ProjectOverridesUser exercises the scope precedence:
// when the same name exists in both project and user scope, project wins.
// We can only fake the user scope by pointing HOME at a temp dir.
func TestListSubagentsFor_ProjectOverridesUser(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	// User-scope agent "shared"
	userDir := filepath.Join(tmpHome, ".claude", "agents")
	writeFile(t, filepath.Join(userDir, "shared.md"), "---\nname: shared\ndescription: user version\n---\nuser body")
	// User-only agent
	writeFile(t, filepath.Join(userDir, "user-only.md"), "---\nname: user-only\n---\n")

	// Project cwd with project-scope "shared"
	proj := t.TempDir()
	projAgents := filepath.Join(proj, ".claude", "agents")
	writeFile(t, filepath.Join(projAgents, "shared.md"), "---\nname: shared\ndescription: project version\n---\nproject body")
	writeFile(t, filepath.Join(projAgents, "proj-only.md"), "---\nname: proj-only\n---\n")

	got, err := listSubagentsFor(proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	byName := map[string]*SubagentFile{}
	for _, sf := range got {
		byName[sf.Name] = sf
	}
	shared, ok := byName["shared"]
	if !ok {
		t.Fatalf("expected 'shared' in result")
	}
	if shared.Scope != "project" {
		t.Errorf("scope of shared: got %q, want project (project must override user)", shared.Scope)
	}
	if shared.Description != "project version" {
		t.Errorf("description: got %q, want %q", shared.Description, "project version")
	}
	if _, ok := byName["user-only"]; !ok {
		t.Errorf("user-only should be present (no shadowing)")
	}
	if _, ok := byName["proj-only"]; !ok {
		t.Errorf("proj-only should be present")
	}

	// Verify sort: project scope first, then user, alphabetical within.
	scopes := make([]string, len(got))
	for i, sf := range got {
		scopes[i] = sf.Scope
	}
	if !sort.SliceIsSorted(scopes, func(i, j int) bool {
		if scopes[i] != scopes[j] {
			return scopes[i] == "project"
		}
		return false
	}) {
		t.Errorf("expected project entries before user entries, got scopes %v", scopes)
	}
}

func TestListSubagentsFor_SkipsUnderscoreAndNonMd(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	proj := t.TempDir()
	projAgents := filepath.Join(proj, ".claude", "agents")
	writeFile(t, filepath.Join(projAgents, "keep.md"), "---\nname: keep\n---\n")
	writeFile(t, filepath.Join(projAgents, "_skip.md"), "---\nname: skip\n---\n")
	writeFile(t, filepath.Join(projAgents, "readme.txt"), "not markdown")

	got, err := listSubagentsFor(proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].Name != "keep" {
		t.Errorf("expected only 'keep', got %+v", got)
	}
}
