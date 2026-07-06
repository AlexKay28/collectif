package main

import "testing"

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
