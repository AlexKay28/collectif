package main

import (
	"fmt"
	"strings"
	"testing"
)

// #52 M3. The unified diff behind the `diff` output type.
//
// ADR 0001 §4.1 calls a rendered diff "the single highest-value thing a
// notebook shows that a terminal cannot", so the format is not decorative:
// nb_render.js's renderDiff classifies lines by their first character, and
// a diff that does not use the standard prefixes renders as undifferentiated
// context.

func TestUnifiedDiff_IdenticalContentProducesNothing(t *testing.T) {
	if got := unifiedDiff("a.txt", "one\ntwo\n", "one\ntwo\n"); got != "" {
		t.Errorf("unifiedDiff of identical content = %q, want empty — a no-op edit has no diff to show", got)
	}
}

func TestUnifiedDiff_HasFileHeadersAndAHunkHeader(t *testing.T) {
	got := unifiedDiff("src/main.go", "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	for _, want := range []string{"--- a/src/main.go", "+++ b/src/main.go", "@@ ", "-two", "+TWO", " one", " three"} {
		if !strings.Contains(got, want) {
			t.Errorf("diff missing %q:\n%s", want, got)
		}
	}
}

func TestUnifiedDiff_ANewFileIsAllAdditions(t *testing.T) {
	got := unifiedDiff("new.txt", "", "alpha\nbeta\n")
	if !strings.Contains(got, "+alpha") || !strings.Contains(got, "+beta") {
		t.Errorf("diff of a new file should be all additions:\n%s", got)
	}
	if strings.Contains(got, "\n-") {
		t.Errorf("diff of a new file should delete nothing:\n%s", got)
	}
}

// A one-line change in a long file must produce a hunk, not the file. This
// is what keeps a diff readable and what keeps it out of the model's
// context window at full size.
func TestUnifiedDiff_LocalisesAChangeInALongFile(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	old := b.String()
	next := strings.Replace(old, "line 250\n", "line 250 changed\n", 1)

	got := unifiedDiff("long.txt", old, next)
	if n := strings.Count(got, "\n"); n > 12 {
		t.Errorf("a one-line change produced %d diff lines, want a localised hunk:\n%s", n, got)
	}
	if !strings.Contains(got, "+line 250 changed") || !strings.Contains(got, "-line 250") {
		t.Errorf("the change itself is missing:\n%s", got)
	}
	// Line numbers are the only way a reader can find the hunk in the file.
	if !strings.Contains(got, "@@ -248") {
		t.Errorf("hunk header should start three lines of context before the change:\n%s", got)
	}
}

func TestUnifiedDiff_SeparateChangesGetSeparateHunks(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	old := b.String()
	next := strings.Replace(old, "line 10\n", "TEN\n", 1)
	next = strings.Replace(next, "line 80\n", "EIGHTY\n", 1)

	got := unifiedDiff("two.txt", old, next)
	if n := strings.Count(got, "@@ "); n != 2 {
		t.Errorf("got %d hunks, want 2 — far-apart changes must not be joined by 70 lines of context:\n%s", n, got)
	}
}

// A file with no trailing newline is common enough (and easy enough to
// corrupt) that the diff must not silently invent one.
func TestUnifiedDiff_MarksAMissingTrailingNewline(t *testing.T) {
	got := unifiedDiff("x.txt", "one\ntwo", "one\ntwo\n")
	if !strings.Contains(got, "\\ No newline at end of file") {
		t.Errorf("adding a trailing newline should be visible in the diff:\n%s", got)
	}
}
