package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #52 M3. Containment for the tools that can *change* something.
//
// The read tools' suite asserts that a path outside the root cannot be
// read. These assert the stronger property the write tools need: a path
// outside the root cannot be created either. Reading through a bad path
// leaks; writing through one destroys, and the two do not fail in the same
// places — a read resolves an existing file, a write resolves a file that
// does not exist yet.

// containedPath resolved symlinks with EvalSymlinks on the *whole* path,
// which fails outright when the last component does not exist. The failure
// was swallowed and the unresolved lexical path used instead, so a symlink
// planted inside the root escaped for any path naming a file that was not
// there yet — which is every path a write tool creates.
func TestContainedPath_ANewFileUnderASymlinkedDirectoryCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := containedPath(root, "escape/newfile.txt")
	if err == nil {
		t.Fatalf("containedPath returned %q for a path that lands in %s — containment breached", got, outside)
	}
}

// The same hole one level deeper: the symlink is not the parent, it is an
// ancestor. Resolving only the parent would still let this through.
func TestContainedPath_ANewFileBelowASymlinkedAncestorCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got, err := containedPath(root, "escape/deep/newfile.txt"); err == nil {
		t.Fatalf("containedPath returned %q — a symlinked ancestor escaped containment", got)
	}
}

// A path that does not exist and does not escape must still resolve, or
// every write to a new file would be refused.
func TestContainedPath_ANewFileInsideTheRootIsAllowed(t *testing.T) {
	root := t.TempDir()
	got, err := containedPath(root, "sub/dir/newfile.txt")
	if err != nil {
		t.Fatalf("containedPath refused a new file inside the root: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolved, "sub/dir/newfile.txt"); got != want {
		t.Errorf("containedPath = %q, want %q", got, want)
	}
}

// ─── write ──────────────────────────────────────────────────────────────

func TestWriteTool_CreatesAFileAndItsParentDirectories(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &writeTool{}, root, map[string]any{
		"path": "pkg/sub/new.go", "content": "package sub\n",
	})
	if isErr {
		t.Fatalf("write failed: %s", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "pkg/sub/new.go"))
	if err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if string(got) != "package sub\n" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteTool_OverwritesAnExistingFile(t *testing.T) {
	root := toolRoot(t)
	if out, isErr := runTool(t, &writeTool{}, root, map[string]any{
		"path": "README.md", "content": "# Replaced\n",
	}); isErr {
		t.Fatalf("write failed: %s", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# Replaced\n" {
		t.Errorf("content = %q, want the new content", got)
	}
}

func TestWriteTool_RefusesAPathWithoutContentOrContentWithoutAPath(t *testing.T) {
	root := toolRoot(t)
	if _, isErr := runTool(t, &writeTool{}, root, map[string]any{"content": "x"}); !isErr {
		t.Error("write with no path should be a tool error the model can act on")
	}
	// An absent content field is not the same as an empty one: truncating a
	// file because a key was missing is the kind of accident that is only
	// discovered later.
	if _, isErr := runTool(t, &writeTool{}, root, map[string]any{"path": "README.md"}); !isErr {
		t.Error("write with no content field should be refused, not treated as truncate")
	}
	if got, err := os.ReadFile(filepath.Join(root, "README.md")); err != nil || len(got) == 0 {
		t.Errorf("README.md was truncated by a refused write: %v %q", err, got)
	}
}

// The empty string is a legitimate content value once it is said out loud.
func TestWriteTool_AnExplicitEmptyStringTruncates(t *testing.T) {
	root := toolRoot(t)
	if out, isErr := runTool(t, &writeTool{}, root, map[string]any{
		"path": "docs/notes.txt", "content": "",
	}); isErr {
		t.Fatalf("write of an empty string failed: %s", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "docs/notes.txt"))
	if err != nil || len(got) != 0 {
		t.Errorf("file = %q (%v), want empty", got, err)
	}
}

func TestWriteTool_PreviewsTheDiffWithoutWriting(t *testing.T) {
	root := toolRoot(t)
	in := map[string]any{"path": "README.md", "content": "# Project\nChanged.\n"}

	diff := (&writeTool{}).Preview(in, root)
	if !strings.Contains(diff, "+Changed.") || !strings.Contains(diff, "-A notebook harness.") {
		t.Errorf("preview diff does not describe the change:\n%s", diff)
	}
	got, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "Changed.") {
		t.Fatal("Preview wrote to the file — the point of a preview is that it happens before the approval")
	}
}

func TestWriteTool_PreviewOfANoOpWriteIsEmpty(t *testing.T) {
	root := toolRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := (&writeTool{}).Preview(map[string]any{"path": "README.md", "content": string(body)}, root); diff != "" {
		t.Errorf("a write that changes nothing previewed a diff:\n%s", diff)
	}
}

// ─── edit ───────────────────────────────────────────────────────────────

func TestEditTool_ReplacesAnExactMatch(t *testing.T) {
	root := toolRoot(t)
	out, isErr := runTool(t, &editTool{}, root, map[string]any{
		"path": "src/util.go", "old_string": "// TODO: tidy this up", "new_string": "// Tidied.",
	})
	if isErr {
		t.Fatalf("edit failed: %s", out)
	}
	got, err := os.ReadFile(filepath.Join(root, "src/util.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "// Tidied.") || strings.Contains(string(got), "TODO") {
		t.Errorf("file = %q", got)
	}
}

func TestEditTool_AMissingOldStringIsAToolErrorAndChangesNothing(t *testing.T) {
	root := toolRoot(t)
	before, err := os.ReadFile(filepath.Join(root, "src/util.go"))
	if err != nil {
		t.Fatal(err)
	}
	out, isErr := runTool(t, &editTool{}, root, map[string]any{
		"path": "src/util.go", "old_string": "not in the file", "new_string": "x",
	})
	if !isErr {
		t.Error("an edit whose old_string is absent must report an error the model can react to")
	}
	if !strings.Contains(out, "not in the file") {
		t.Errorf("the error should quote what was not found: %q", out)
	}
	after, err := os.ReadFile(filepath.Join(root, "src/util.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("a failed edit modified the file")
	}
}

// An old_string matching twice is ambiguous. Silently taking the first is
// how an edit lands in the wrong place and is noticed three commits later.
func TestEditTool_AnAmbiguousMatchIsRefusedUnlessReplaceAll(t *testing.T) {
	root := toolRoot(t)
	if err := os.WriteFile(filepath.Join(root, "dup.txt"), []byte("a\nsame\nb\nsame\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := runTool(t, &editTool{}, root, map[string]any{
		"path": "dup.txt", "old_string": "same", "new_string": "other",
	})
	if !isErr {
		t.Fatal("an ambiguous edit should be refused")
	}
	if !strings.Contains(out, "2") {
		t.Errorf("the error should say how many times it matched: %q", out)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "dup.txt")); string(got) != "a\nsame\nb\nsame\n" {
		t.Errorf("a refused edit changed the file: %q", got)
	}

	if out, isErr := runTool(t, &editTool{}, root, map[string]any{
		"path": "dup.txt", "old_string": "same", "new_string": "other", "replace_all": true,
	}); isErr {
		t.Fatalf("replace_all edit failed: %s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "dup.txt")); string(got) != "a\nother\nb\nother\n" {
		t.Errorf("replace_all = %q", got)
	}
}

func TestEditTool_RefusesDegenerateArguments(t *testing.T) {
	root := toolRoot(t)
	for _, in := range []map[string]any{
		{"path": "README.md", "old_string": "", "new_string": "x"},
		{"path": "README.md", "old_string": "# Project", "new_string": "# Project"},
		{"old_string": "a", "new_string": "b"},
	} {
		if _, isErr := runTool(t, &editTool{}, root, in); !isErr {
			t.Errorf("edit(%v) should be refused", in)
		}
	}
}

func TestEditTool_PreviewsTheDiffWithoutWriting(t *testing.T) {
	root := toolRoot(t)
	in := map[string]any{"path": "src/util.go", "old_string": "// TODO: tidy this up", "new_string": "// Tidied."}

	diff := (&editTool{}).Preview(in, root)
	if !strings.Contains(diff, "+// Tidied.") {
		t.Errorf("preview diff does not describe the change:\n%s", diff)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "src/util.go")); strings.Contains(string(got), "Tidied") {
		t.Fatal("Preview wrote to the file")
	}
}

// ─── Containment, adversarially ─────────────────────────────────────────

// The same suite the read tools have, on the tools that can destroy rather
// than leak. A read through a bad path exposes a file; a write through one
// replaces it, and the two do not fail in the same places.
func TestWriteTools_RefuseToEscapeTheNotebookRoot(t *testing.T) {
	root := toolRoot(t)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outside, []byte("SENSITIVE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "link-dir")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link-file")); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"../escaped.txt",
		"../../etc/collectif-should-not-be-here",
		outside,
		filepath.Join(outsideDir, "new-file.txt"),
		"link-file",
		// The one the old containment missed: a *new* file under a
		// symlinked directory. EvalSymlinks could not resolve it, the
		// error was swallowed, and the lexical path was waved through.
		"link-dir/new-file.txt",
		"link-dir/nested/deeper.txt",
		"src/../../escape.txt",
		"./../../out.txt",
		"src/./../../../out.txt",
		// A unicode name changes nothing about where the path lands.
		"../café-escape.txt",
	} {
		if out, isErr := runTool(t, &writeTool{}, root, map[string]any{"path": path, "content": "PWNED"}); !isErr {
			t.Errorf("write(%q) succeeded — containment breached (%s)", path, out)
		}
		if out, isErr := runTool(t, &editTool{}, root, map[string]any{
			"path": path, "old_string": "SENSITIVE", "new_string": "PWNED",
		}); !isErr {
			t.Errorf("edit(%q) succeeded — containment breached (%s)", path, out)
		}
	}

	if got, err := os.ReadFile(outside); err != nil || string(got) != "SENSITIVE" {
		t.Fatalf("the file outside the root was modified: %q (%v)", got, err)
	}
	entries, err := os.ReadDir(outsideDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("files were created outside the root: %d entries", len(entries))
	}
}

// Containment is checked before the file is opened, not after — otherwise
// a write to an out-of-bounds path truncates it and only then reports the
// refusal, which is the worst of both.
func TestWriteTool_ARefusedWriteDoesNotTruncateItsTarget(t *testing.T) {
	root := toolRoot(t)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "keep.txt")
	if err := os.WriteFile(outside, []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, isErr := runTool(t, &writeTool{}, root, map[string]any{"path": outside, "content": ""}); !isErr {
		t.Fatal("write outside the root should be refused")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "KEEP" {
		t.Errorf("the target was truncated by a refused write: %q (%v)", got, err)
	}
}
