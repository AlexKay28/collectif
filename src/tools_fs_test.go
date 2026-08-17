package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #50 M2 slice B. The read-only tools. They are read-only on purpose: M2 is
// the phase where the loop can be wrong without being destructive, and
// write access waits for the permission engine in M3.
//
// Containment is the property under test more than the reading is. Every
// path-taking tool resolves symlinks and verifies the result is under the
// notebook root *before* doing any I/O — policy can loosen what you may do,
// it can never loosen where.

func toolRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("README.md", "# Project\nA notebook harness.\n")
	write("src/main.go", "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n")
	write("src/util.go", "package main\n\n// TODO: tidy this up\nfunc helper() {}\n")
	write("docs/notes.txt", "nothing to see\n")
	return root
}

func runTool(t *testing.T, tool Tool, root string, in map[string]any) (string, bool) {
	t.Helper()
	out, isErr, err := tool.Run(context.Background(), in, root)
	if err != nil {
		t.Fatalf("%s returned a hard error (%v) — tool failures belong in the result so the model can adapt", tool.Spec().Name, err)
	}
	return out, isErr
}

// ─── Containment ────────────────────────────────────────────────────────

func TestTools_RefuseToEscapeTheNotebookRoot(t *testing.T) {
	root := toolRoot(t)
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("SENSITIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink planted inside the root pointing out of it.
	if err := os.Symlink(outside, filepath.Join(root, "link-out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	for _, path := range []string{
		"../secret.txt", "../../etc/passwd", outside, "link-out",
		"src/../../escape", "./../../out",
	} {
		out, isErr := runTool(t, &readTool{}, root, map[string]any{"path": path})
		if !isErr {
			t.Errorf("read(%q) succeeded — containment breached", path)
		}
		if strings.Contains(out, "SENSITIVE") {
			t.Fatalf("read(%q) leaked content from outside the root", path)
		}
	}
}

func TestTools_GrepAndGlobStayInsideTheRoot(t *testing.T) {
	root := toolRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "hit.txt"), []byte("NEEDLE"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := runTool(t, &grepTool{}, root, map[string]any{"pattern": "NEEDLE", "path": outside})
	if strings.Contains(out, "NEEDLE") {
		t.Error("grep searched outside the notebook root")
	}
	out, _ = runTool(t, &globTool{}, root, map[string]any{"pattern": filepath.Join(outside, "*.txt")})
	if strings.Contains(out, "hit.txt") {
		t.Error("glob matched outside the notebook root")
	}
}

// ─── read ───────────────────────────────────────────────────────────────

func TestReadTool_ReturnsFileContents(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &readTool{}, root, map[string]any{"path": "README.md"})
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if !strings.Contains(out, "A notebook harness.") {
		t.Errorf("read returned %q", out)
	}
}

func TestReadTool_MissingFileIsAToolErrorNotACrash(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &readTool{}, root, map[string]any{"path": "nope.txt"})
	if !isErr {
		t.Error("reading a missing file should report an error the model can act on")
	}
	if out == "" {
		t.Error("the error result should say what went wrong")
	}
}

func TestReadTool_HonoursALineRange(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &readTool{}, root, map[string]any{
		"path": "src/main.go", "offset": float64(3), "limit": float64(1),
	})
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if !strings.Contains(out, "func main()") {
		t.Errorf("range read returned %q, want line 3", out)
	}
	if strings.Contains(out, "package main") {
		t.Errorf("range read included line 1: %q", out)
	}
}

func TestReadTool_TruncatesEnormousFiles(t *testing.T) {
	root := toolRoot(t)
	big := strings.Repeat("filler line\n", 200000)
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := runTool(t, &readTool{}, root, map[string]any{"path": "big.txt"})
	if isErr {
		t.Fatalf("read failed: %s", out)
	}
	if len(out) > 4*toolOutputBudget {
		t.Errorf("read returned %d bytes, want it bounded near %d", len(out), toolOutputBudget)
	}
	if !strings.Contains(out, "truncated") {
		t.Error("truncation should be stated, not silent")
	}
}

// ─── glob ───────────────────────────────────────────────────────────────

func TestGlobTool_MatchesRelativePaths(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &globTool{}, root, map[string]any{"pattern": "**/*.go"})
	if isErr {
		t.Fatalf("glob failed: %s", out)
	}
	for _, want := range []string{"src/main.go", "src/util.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("glob output %q missing %s", out, want)
		}
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("glob matched a non-Go file: %q", out)
	}
	if strings.Contains(out, root) {
		t.Errorf("glob leaked absolute paths: %q", out)
	}
}

func TestGlobTool_NoMatchesIsNotAnError(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &globTool{}, root, map[string]any{"pattern": "**/*.rs"})
	if isErr {
		t.Errorf("an empty result is an answer, not a failure: %q", out)
	}
	if out == "" {
		t.Error("an empty result should still say so")
	}
}

// ─── grep ───────────────────────────────────────────────────────────────

func TestGrepTool_ReportsFileAndLine(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &grepTool{}, root, map[string]any{"pattern": "TODO"})
	if isErr {
		t.Fatalf("grep failed: %s", out)
	}
	if !strings.Contains(out, "src/util.go") {
		t.Errorf("grep output %q missing the file", out)
	}
	if !strings.Contains(out, "tidy this up") {
		t.Errorf("grep output %q missing the matching line", out)
	}
	if !strings.Contains(out, ":3:") {
		t.Errorf("grep output %q missing the line number", out)
	}
}

func TestGrepTool_InvalidRegexIsAToolError(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &grepTool{}, root, map[string]any{"pattern": "([unclosed"})
	if !isErr {
		t.Error("an invalid pattern should come back as a tool error the model can correct")
	}
	if !strings.Contains(strings.ToLower(out), "pattern") {
		t.Errorf("error %q should name the problem", out)
	}
}

func TestGrepTool_CanScopeToAGlob(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &grepTool{}, root, map[string]any{"pattern": "package", "glob": "**/*.go"})
	if isErr {
		t.Fatalf("grep failed: %s", out)
	}
	if strings.Contains(out, "README.md") {
		t.Errorf("glob scope ignored: %q", out)
	}
}

// ─── Schemas ────────────────────────────────────────────────────────────

// Strict schemas are what make tool arguments guaranteed to validate, so
// the loop never hand-parses a malformed input.
func TestToolSchemas_AreStrict(t *testing.T) {
	for _, tool := range builtinTools() {
		spec := tool.Spec()
		if spec.Description == "" {
			t.Errorf("%s has no description — the model chooses tools by it", spec.Name)
		}
		if spec.InputSchema == nil {
			t.Fatalf("%s has no input schema", spec.Name)
		}
		if got, ok := spec.InputSchema["additionalProperties"]; !ok || got != false {
			t.Errorf("%s schema does not set additionalProperties:false", spec.Name)
		}
		props, ok := spec.InputSchema["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Fatalf("%s schema has no properties", spec.Name)
		}
		for name, raw := range props {
			p, _ := raw.(map[string]any)
			if p["description"] == nil || p["description"] == "" {
				t.Errorf("%s.%s has no description", spec.Name, name)
			}
		}
	}
}
