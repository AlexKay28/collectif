package main

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Strips both CSI and OSC ANSI escape sequences. Non-exhaustive but covers
// what Claude Code / ink actually emit in menus.
var ansiRe = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[a-zA-Z]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[\(\)][A-Za-z0-9]|[=>NnPMHDEc78])`)

func stripAnsi(s string) string {
	return ansiRe.ReplaceAllString(s, "")
}

// Menu-line regex: optional highlight marker, then a digit, ., or ), then the label.
// Handles "❯ 1. Yes", "▶ 1) Yes", "  1. No", "> 1. Cancel".
var menuLineRe = regexp.MustCompile(`^\s*([❯▶*>❱▷►])?\s*(\d+)[\.\)]\s+(.+?)\s*$`)

// detectMenu scans the tail of the PTY output for a numbered menu. Returns
// the options in order 1..N, or nil if no menu is present. Requires at least
// two consecutive numbered items to guard against random "1. foo" in prose.
func detectMenu(buf []byte) []MenuOption {
	if len(buf) == 0 {
		return nil
	}
	// Only scan the last ~8KB — a menu is always at the bottom of the screen.
	tail := buf
	if len(tail) > 8192 {
		tail = tail[len(tail)-8192:]
	}
	stripped := stripAnsi(string(tail))
	// Some TUIs use \r to overwrite lines in place; keep only what's after the
	// last \r in each line so we see the final rendered state.
	rlines := strings.Split(stripped, "\n")
	lines := make([]string, 0, len(rlines))
	for _, l := range rlines {
		if i := strings.LastIndex(l, "\r"); i >= 0 {
			l = l[i+1:]
		}
		lines = append(lines, l)
	}

	// Walk back from the end to find the most recent "1." line. Then greedily
	// collect 2., 3., … while allowing blank lines between items.
	lastOne := -1
	for i := len(lines) - 1; i >= 0; i-- {
		m := menuLineRe.FindStringSubmatch(lines[i])
		if m != nil && m[2] == "1" {
			lastOne = i
			break
		}
	}
	if lastOne < 0 {
		return nil
	}
	var options []MenuOption
	expected := 1
	for i := lastOne; i < len(lines) && expected <= 12; i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			// Allow one blank inside the menu; more than one → probably done.
			if len(options) > 0 && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
				break
			}
			continue
		}
		m := menuLineRe.FindStringSubmatch(line)
		if m == nil {
			break
		}
		num, _ := strconv.Atoi(m[2])
		if num != expected {
			break
		}
		options = append(options, MenuOption{
			Key:       m[2],
			Label:     strings.TrimSpace(m[3]),
			Highlight: m[1] != "",
		})
		expected++
	}
	if len(options) < 2 {
		return nil
	}
	return options
}

func menusEqual(a, b []MenuOption) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key != b[i].Key || a[i].Label != b[i].Label || a[i].Highlight != b[i].Highlight {
			return false
		}
	}
	return true
}

// startMenuDetector polls the PTY ring buffer every 250ms, updating the
// session's MenuOptions when the visible menu changes. Cheap enough per
// session; the ring snapshot copy is bounded to 8KB in detectMenu.
func startMenuDetector(ctx context.Context, s *Session) {
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				opts := detectMenu(s.snapshotRing())
				if !menusEqual(opts, s.getMenuOptions()) {
					s.setMenuOptions(opts)
					s.touch()
				}
			}
		}
	}()
}
