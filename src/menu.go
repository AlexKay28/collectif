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

// Menu-line regex: optional highlight marker, then a digit, . or ), then
// the label. Handles "❯ 1. Yes", "▶ 1) Yes", "  1. No", "> 1. Cancel".
//
// The space after the separator is optional, and that is not cosmetic
// (#57). An ink TUI does not pad with spaces — it positions the cursor —
// so the gap between "1." and its label is a cursor-forward escape, which
// stripAnsi removes before this ever runs. Requiring `\s+` therefore
// rejected Claude Code's own trust and auto-mode dialogs, and the detector
// had been silently blind to them for as long as it has existed. Nobody
// noticed because nothing read MenuOptions until the send gate did.
//
// The label must start with something that is not a digit or a further
// separator, which keeps "1.5 seconds" and "version 1.2.3" out. The
// two-consecutive-items rule below does the rest of the work.
var menuLineRe = regexp.MustCompile(`^\s*([❯▶*>❱▷►])?\s*(\d+)[\.\)]\s*([^\s\d.)][^\r\n]*?)\s*$`)

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
	// Some TUIs use \r to overwrite a line in place; keep only what follows
	// the last one so we see the final rendered state.
	//
	// The trailing \r of a CRLF is not that, and treating it as one was a
	// bug that made this function return nothing at all on real terminal
	// output (#57). A PTY line arrives as "1. Yes\r\n"; splitting on \n
	// leaves "1. Yes\r", and taking everything after the last \r left the
	// empty string. Every line. The detector has never matched a live
	// dialog, which is why MenuOptions was always null and nobody noticed
	// until the send gate started depending on it.
	rlines := strings.Split(stripped, "\n")
	lines := make([]string, 0, len(rlines))
	// rendered[i] is how much output line i actually took, *before* the
	// in-place rewrites are collapsed away. It is what the freshness check
	// below measures, and the distinction matters more than it sounds:
	// Claude Code redraws its whole screen as one enormous \r-separated
	// line, which collapses to its final few characters. Measuring the
	// collapsed length made a screen's worth of redraw look like thirty
	// bytes, and a dismissed dialog stayed "on screen" forever.
	rendered := make([]int, 0, len(rlines))
	for _, l := range rlines {
		rendered = append(rendered, len(l)+1)
		// Trim *every* trailing CR, not one. Captured from a live session,
		// Claude Code's lines end "\r\r\n" — the PTY's ONLCR translation on
		// top of the CR the TUI already emitted. Removing a single one left
		// another at the end, which the rewrite rule below then read as an
		// in-place redraw and truncated the line to nothing.
		l = strings.TrimRight(l, "\r")
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

	// A menu that was *drawn* is not necessarily a menu that is *up*.
	// Scrollback keeps the trust prompt long after it has been answered,
	// and a detector that cannot tell the difference would report a dialog
	// forever — which, wired to the send gate, blocks a healthy session
	// permanently. That is a worse bug than the one the gate exists for.
	//
	// A menu awaiting an answer sits at the bottom of the screen with only
	// its footer after it. One that has been dismissed has a whole redrawn
	// screen after it. Measured on a live session: a live trust prompt's
	// footer is a few hundred bytes; the same menu once answered sat 2,966
	// bytes from the end, behind a full banner and input box. The window
	// below has an order of magnitude of margin on the first and a third of
	// the distance on the second.
	//
	// This is a heuristic and it is the second line of defence, not the
	// first — verifyPromptEcho is what actually protects a send, because it
	// tests whether the text landed rather than guessing what is on screen.
	after := 0
	for i := lastOne; i < len(rendered); i++ {
		after += rendered[i]
	}
	if after > menuFreshTailBytes {
		return nil
	}
	return options
}

// menuFreshTailBytes is how much rendered output may follow a menu before
// we stop believing it is on screen. See detectMenu for how it was chosen.
const menuFreshTailBytes = 1200

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
		var lastSerial uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				serial := s.getRingSerial()
				if serial == lastSerial {
					continue
				}
				lastSerial = serial
				opts := detectMenu(s.snapshotRing())
				if !menusEqual(opts, s.getMenuOptions()) {
					s.setMenuOptions(opts)
					s.touch()
				}
			}
		}
	}()
}
