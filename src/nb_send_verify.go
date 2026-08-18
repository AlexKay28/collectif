package main

// nb_send_verify.go — did the prompt actually land in the input box?
// #57.
//
// The gate in checkSessionDrivable refuses to send while a dialog we can
// *see* is up. It will never see all of them: detection is regex work on
// ANSI bytes, startup dialogs fire no hook, and the next one Claude Code
// adds will be invisible until somebody notices.
//
// So this is the layer that does not depend on recognising anything. A
// CLI echoes what you type into its input box, so a prompt that reached
// the box appears in the PTY output within moments. One that did not went
// somewhere else — a dialog, a paused process, a CLI that has already
// exited. The check tests the property we actually care about instead of
// pattern-matching the causes, which is what makes it work for a dialog
// nobody has seen yet.
//
// It is a check on an assumption, not a gate, and it fails open. A CLI
// that does not echo, or whose output we are not reading, produces no
// evidence either way — and reporting every send as lost would be worse
// than the problem.

import (
	"strings"
	"time"
)

var (
	// echoWindow is how long a prompt has to appear on screen. Generous:
	// a terminal redraw is immediate, but a busy machine under -race is
	// not, and a false report is more damaging than a slow one.
	echoWindow = 2500 * time.Millisecond
	echoPoll   = 100 * time.Millisecond
)

// echoProbeRunes is how much of the prompt must be seen. Short, because a
// terminal wraps and re-flows: demanding the whole prompt would fail on
// any input longer than the window is wide.
const echoProbeRunes = 12

// verifyPromptEcho watches the session's screen for the prompt it was just
// sent. Reports false only when there is positive evidence of a problem:
// output arrived, and the prompt was not in it.
func verifyPromptEcho(s *Session, text string) bool {
	probe := echoProbe(text)
	if probe == "" {
		return true // nothing distinctive enough to look for
	}
	before := len(s.snapshotRing())
	deadline := time.Now().Add(echoWindow)
	for {
		screen := s.snapshotRing()
		if strings.Contains(normaliseScreen(string(screen)), probe) {
			return true
		}
		if time.Now().After(deadline) {
			// No output at all since the send means we have no evidence,
			// not evidence of absence: a CLI that does not echo must not be
			// reported as having swallowed the prompt.
			return len(screen) == before
		}
		time.Sleep(echoPoll)
	}
}

// echoProbe reduces a prompt to the fragment we look for: its first few
// significant characters, normalised the same way the screen is.
func echoProbe(text string) string {
	n := normaliseScreen(text)
	if len([]rune(n)) < echoProbeRunes {
		return "" // too short to be distinctive; assume it landed
	}
	return string([]rune(n)[:echoProbeRunes])
}

// normaliseScreen makes terminal output comparable to source text. A
// terminal wraps mid-phrase, redraws in place, and interleaves escape
// sequences and cursor moves, so both sides are stripped of ANSI and of
// whitespace entirely — what survives is the sequence of visible
// characters, which is the only thing the two have in common.
func normaliseScreen(s string) string {
	stripped := stripAnsi(s)
	var b strings.Builder
	b.Grow(len(stripped))
	for _, r := range stripped {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}
