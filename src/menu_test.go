package main

import (
	"strings"
	"testing"
)

func TestDetectMenu_ClaudePermission(t *testing.T) {
	input := []byte("Do you want to proceed?\n\n\x1b[32m❯ 1. Yes\x1b[0m\n  2. Yes, and don't ask again this session for Bash\n  3. No, and tell Claude what to do differently (esc)\n")
	got := detectMenu(input)
	if len(got) != 3 {
		t.Fatalf("expected 3 options, got %d: %+v", len(got), got)
	}
	want := []MenuOption{
		{Key: "1", Label: "Yes", Highlight: true},
		{Key: "2", Label: "Yes, and don't ask again this session for Bash"},
		{Key: "3", Label: "No, and tell Claude what to do differently (esc)"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("opt %d: got %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestDetectMenu_None(t *testing.T) {
	input := []byte("just some prose\nwith no menu\nblah blah\n")
	if got := detectMenu(input); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestDetectMenu_SingleItemNotAMenu(t *testing.T) {
	input := []byte("Step 1. Do the thing\nAnd then keep going.\n")
	if got := detectMenu(input); got != nil {
		t.Errorf("single 1. is not a menu, got %+v", got)
	}
}

func TestDetectMenu_CarriageReturnRewrite(t *testing.T) {
	// Ink often overwrites with \r; we should see the *final* content.
	input := []byte("old text\r  1. Yes\n  2. No\n")
	got := detectMenu(input)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d: %+v", len(got), got)
	}
	if got[0].Label != "Yes" || got[1].Label != "No" {
		t.Errorf("labels wrong: %+v", got)
	}
}

func TestDetectMenu_ParenStyle(t *testing.T) {
	input := []byte("Choose one:\n  1) First option\n  2) Second option\n  3) Third\n")
	got := detectMenu(input)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d: %+v", len(got), got)
	}
}

func TestDetectMenu_UsesLatestMenu(t *testing.T) {
	// An older menu earlier in the buffer shouldn't shadow the latest one.
	input := []byte("earlier prompt:\n  1. Old A\n  2. Old B\n\nresult was A\n\nnow choose again:\n  1. New X\n  2. New Y\n  3. New Z\n")
	got := detectMenu(input)
	if len(got) != 3 || got[0].Label != "New X" {
		t.Errorf("expected latest menu, got %+v", got)
	}
}

// #57 — the menu detector has been silently blind to Claude Code's own
// dialogs.
//
// `menuLineRe` required whitespace after the separator (`1.` then `\s+`).
// An ink TUI does not pad with spaces; it positions the cursor, so the
// gap between "1." and the label is a cursor-forward escape which
// stripAnsi removes. What reaches the matcher is "1.Yes, I trust this
// folder", and the regex rejected it.
//
// The consequence was invisible for a long time because the dashboard's
// approve/deny buttons run off `Pending`, which comes from hooks. Nothing
// read MenuOptions until #57 wired it to the send gate — at which point a
// gate that never fires is worse than no gate, because it looks like
// protection.
func TestDetectMenu_MatchesInkRenderingWithoutPaddingSpaces(t *testing.T) {
	// Cursor-forward escapes where a terminal would show spaces, exactly
	// as Claude Code's trust prompt renders.
	screen := "Quick safety check: Is this a project you created or one you trust?\r\n" +
		"\x1b[2C1.\x1b[1CYes, I trust this folder\r\n" +
		"\x1b[2C2.\x1b[1CNo, exit\r\n" +
		"Enter to confirm  Esc to cancel\r\n"

	opts := detectMenu([]byte(screen))
	if len(opts) != 2 {
		t.Fatalf("got %d options, want 2 — the gate this feeds would never fire: %+v", len(opts), opts)
	}
	if opts[0].Key != "1" || !strings.Contains(opts[0].Label, "I trust this folder") {
		t.Errorf("option 1 = %+v", opts[0])
	}
	if opts[1].Key != "2" || !strings.Contains(opts[1].Label, "No, exit") {
		t.Errorf("option 2 = %+v", opts[1])
	}
}

// The auto-mode dialog, which swallowed two live prompts during #47 P1.
func TestDetectMenu_MatchesTheAutoModeDialog(t *testing.T) {
	screen := "Set up auto mode for your environment?\r\n" +
		"\x1b[2C1.\x1b[1CSet it up\r\n" +
		"\x1b[2C2.\x1b[1CNot now\r\n" +
		"\x1b[2C3.\x1b[1CDon't show again\r\n" +
		"Enter to confirm  Esc to cancel\r\n"

	if opts := detectMenu([]byte(screen)); len(opts) != 3 {
		t.Fatalf("got %d options, want 3: %+v", len(opts), opts)
	}
}

// Loosening the separator must not start matching prose. Two consecutive
// numbered items are still required, and a decimal is not a menu.
func TestDetectMenu_StillIgnoresProse(t *testing.T) {
	for _, screen := range []string{
		"the run took 1.5 seconds and used 2.3 GB\r\n",
		"see section 1.introduction for details\r\n",
		"1.only one item here\r\n",
		"version 1.2.3 released\r\n",
	} {
		if opts := detectMenu([]byte(screen)); opts != nil {
			t.Errorf("%q was read as a menu: %+v", screen, opts)
		}
	}
}

// A PTY emits CRLF. Before #57 the trailing \r of every line was treated
// as an in-place rewrite marker, so every line was truncated to nothing
// and detectMenu returned nil for all real terminal output. The unit
// tests all used bare \n and so never saw it.
func TestDetectMenu_WorksOnCRLFAsAPTYActuallyEmitsIt(t *testing.T) {
	screen := "Do you want to proceed?\r\n\r\n  1. Yes\r\n  2. No\r\n"
	opts := detectMenu([]byte(screen))
	if len(opts) != 2 {
		t.Fatalf("got %d options from CRLF output, want 2: %+v", len(opts), opts)
	}
	if opts[0].Label != "Yes" || opts[1].Label != "No" {
		t.Errorf("labels came through as %q / %q", opts[0].Label, opts[1].Label)
	}
}

// In-place rewriting still has to win: a spinner that redraws a line must
// be read at its final state, not its first.
func TestDetectMenu_StillPrefersTheRewrittenLine(t *testing.T) {
	screen := "pick one\r\n  1. stale\r  1. fresh\r\n  2. second\r\n"
	opts := detectMenu([]byte(screen))
	if len(opts) != 2 || opts[0].Label != "fresh" {
		t.Fatalf("in-place rewrite was lost: %+v", opts)
	}
}

// Captured from a live Claude Code session: lines end "\r\r\n" and there
// are no spaces anywhere — an ink TUI positions the cursor between words
// rather than padding, and stripAnsi removes those escapes. This is what
// detectMenu is actually handed, and until #57 it matched none of it.
func TestDetectMenu_MatchesWhatALiveSessionActuallyEmits(t *testing.T) {
	screen := "Quicksafetycheck:Isthisaprojectyoucreatedoroneyoutrust?\r\r\n" +
		"\r\r\n" +
		"1.Yes,Itrustthisfolder\r\r\n" +
		"2.No,exit\r\r\n" +
		"\r\r\n" +
		"EntertoconfirmEsctocancel\r\r\n"

	opts := detectMenu([]byte(screen))
	if len(opts) != 2 {
		t.Fatalf("got %d options from a real render, want 2: %+v", len(opts), opts)
	}
	if opts[0].Label != "Yes,Itrustthisfolder" {
		t.Errorf("option 1 label = %q", opts[0].Label)
	}
	if opts[1].Label != "No,exit" {
		t.Errorf("option 2 label = %q", opts[1].Label)
	}
}

// A menu is only on screen while it is at the bottom of it. Scrollback
// keeps the trust prompt long after it has been answered, and reporting
// that as a live dialog would block sending on a perfectly healthy
// session — permanently, since nothing scrolls an idle terminal.
//
// Measured from a live session: the answered trust menu sat 2,966 bytes
// from the end, behind a full redrawn screen.
func TestDetectMenu_IgnoresAMenuBuriedInScrollback(t *testing.T) {
	screen := "1.Yes,Itrustthisfolder\r\r\n2.No,exit\r\r\n" +
		strings.Repeat("the session carried on and redrew its whole screen\r\r\n", 60)

	if opts := detectMenu([]byte(screen)); opts != nil {
		t.Errorf("a dismissed menu was reported as on screen: %+v", opts)
	}
}

// And a live one, whose footer is all that follows it, still counts.
func TestDetectMenu_StillSeesAMenuAtTheBottomOfTheScreen(t *testing.T) {
	screen := strings.Repeat("earlier output that has scrolled by\r\r\n", 40) +
		"1.Yes,Itrustthisfolder\r\r\n2.No,exit\r\r\n\r\r\nEntertoconfirmEsctocancel\r\r\n"

	if opts := detectMenu([]byte(screen)); len(opts) != 2 {
		t.Fatalf("a live dialog was missed: %+v", opts)
	}
}

// Claude Code redraws its whole screen as a single \r-separated line, so
// a screen's worth of output collapses to its last few characters.
// Measuring the collapsed length made a dismissed dialog look like it was
// still at the bottom of the screen — the freshness check has to measure
// what was rendered, not what survived the collapse.
func TestDetectMenu_FreshnessMeasuresRenderedOutputNotCollapsedLines(t *testing.T) {
	redraw := strings.Repeat("spinner frame\rstatus line\rmore chatter\r", 120)
	screen := "1.Yes,Itrustthisfolder\r\r\n2.No,exit\r\r\n" + redraw + "? for shortcuts"

	if opts := detectMenu([]byte(screen)); opts != nil {
		t.Errorf("a dialog buried under a full screen redraw was reported as live: %+v", opts)
	}
}
