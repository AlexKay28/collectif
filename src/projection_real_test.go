package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// #47 P0 — the spike's actual question.
//
// The fixture tests in projection_test.go prove the parser does what I
// designed it to do. They cannot prove the design was right, because I
// wrote both sides. This one runs the projection over a real Claude Code
// transcript on this machine and asserts the *shape of the result* is a
// document a person would read: mostly turns, not mostly machinery.
//
// It skips when there is no transcript to read. That makes it a canary
// rather than a gate — when Claude Code changes its format, this is what
// notices.
func TestProjectClaude_AgainstARealTranscript(t *testing.T) {
	path := newestTranscript(t)
	if path == "" {
		t.Skip("no Claude Code transcript on this machine — nothing to project")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("open %s: %v", path, err)
	}
	defer f.Close()

	a := &claudeAdapter{}
	counts := map[PartKind]int{}
	var lines, projecting, sidechain int
	var longestUser string

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		lines++
		parts, err := a.ProjectTranscriptLine(sc.Bytes())
		if err != nil {
			t.Fatalf("line %d returned an error: %v", lines, err)
		}
		if len(parts) > 0 {
			projecting++
		}
		for _, p := range parts {
			counts[p.Kind]++
			if p.Sidechain {
				sidechain++
			}
			if p.Kind == PartUserText && len(p.Text) > len(longestUser) {
				longestUser = p.Text
			}
			if p.Kind == PartToolCall && p.ToolName == "" {
				t.Errorf("line %d: tool call with no name", lines)
			}
			if p.Kind == PartToolResult && p.ToolUseID == "" {
				t.Errorf("line %d: tool result with no call to pair with", lines)
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Skipf("read %s: %v", path, err)
	}

	kinds := make([]string, 0, len(counts))
	for k, n := range counts {
		kinds = append(kinds, string(k)+"="+itoa(n))
	}
	sort.Strings(kinds)
	t.Logf("%s\n  %d lines → %d projecting, %s, sidechain parts=%d",
		filepath.Base(path), lines, projecting, strings.Join(kinds, " "), sidechain)

	if counts[PartAssistantText] == 0 {
		t.Error("no assistant prose projected from a real session — the notebook would be empty of answers")
	}
	if counts[PartToolCall] == 0 {
		t.Error("no tool calls projected — the notebook would show conclusions with no work behind them")
	}
	// Every call needs its result, or the document shows work starting and
	// never finishing. Sidechain calls are answered in their own file.
	if r, c := counts[PartToolResult], counts[PartToolCall]; r < c/2 {
		t.Errorf("%d tool results for %d calls — results are being dropped", r, c)
	}
	// The filter that matters. A real session has a handful of typed
	// prompts and thousands of injected user-role lines; if the ratio is
	// anywhere near even, the isMeta/sourceToolUseID filter is not working
	// and the document is unreadable.
	if u := counts[PartUserText]; u == 0 {
		t.Error("no typed prompts projected — the filter is too aggressive and ate the human")
	} else if u > counts[PartAssistantText] {
		t.Errorf("%d user parts vs %d assistant parts — injected lines are leaking through as prompts", u, counts[PartAssistantText])
	}
	if longestUser != "" && len(longestUser) > 20000 {
		t.Errorf("longest 'typed prompt' is %d bytes — that is an injected document, not a prompt", len(longestUser))
	}

	// Not an assertion, a finding. Across every transcript on this machine
	// (50 files, 7453 thinking blocks) not one carries thinking *text* —
	// Claude Code persists the signature and discards the summary. So
	// PartThinking is unreachable from a transcript, and a session's
	// notebook cannot show reasoning no matter how well we parse. Logged
	// rather than asserted because the day that changes we want to notice,
	// not fail.
	if counts[PartThinking] > 0 {
		t.Logf("thinking text is now present in transcripts (%d parts) — the ADR 0002 capability table should be updated",
			counts[PartThinking])
	}
}

// Subagent conversations are not in the main transcript at all — they live
// in `<session>/subagents/agent-*.jsonl`, which is why isSidechain never
// fires above. The same parser reads them unchanged, which is the whole of
// M6's input problem solved by accident.
func TestProjectClaude_SubagentFilesUseTheSameSchema(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	files, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*", "subagents", "*.jsonl"))
	if len(files) == 0 {
		t.Skip("no subagent transcripts on this machine")
	}

	a := &claudeAdapter{}
	counts := map[PartKind]int{}
	var sidechain, checked int
	for _, path := range files {
		if checked >= 20 {
			break
		}
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		checked++
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			parts, err := a.ProjectTranscriptLine(sc.Bytes())
			if err != nil {
				t.Fatalf("%s: %v", filepath.Base(path), err)
			}
			for _, p := range parts {
				counts[p.Kind]++
				if p.Sidechain {
					sidechain++
				}
			}
		}
		f.Close()
	}
	t.Logf("%d subagent files → assistant=%d tool_call=%d tool_result=%d, sidechain-flagged=%d",
		checked, counts[PartAssistantText], counts[PartToolCall], counts[PartToolResult], sidechain)

	if counts[PartAssistantText] == 0 && counts[PartToolCall] == 0 {
		t.Error("subagent transcripts projected nothing — M6 has no input")
	}
	total := 0
	for _, n := range counts { // every kind, including the task prompt the parent sent
		total += n
	}
	if sidechain != total {
		t.Errorf("%d of %d subagent parts were flagged as sidechain — unflagged ones would interleave "+
			"a subagent's conversation into its parent's document", sidechain, total)
	}
}

func newestTranscript(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	best, bestSize := "", int64(0)
	for _, m := range matches {
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if fi.Size() > bestSize {
			best, bestSize = m, fi.Size()
		}
	}
	return best
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// #55a — the correlation, checked against real files rather than my
// reading of them. For every agentId a transcript reports, the child
// transcript the adapter points at has to actually be there.
func TestProjectClaude_EveryReportedSubagentHasAFileWhereWeLookForIt(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	transcripts, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	if len(transcripts) == 0 {
		t.Skip("no Claude Code transcripts on this machine")
	}

	a := &claudeAdapter{}
	var reported, located, missing int
	var firstMissing string
	for _, tp := range transcripts {
		f, err := os.Open(tp)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			parts, err := a.ProjectTranscriptLine(sc.Bytes())
			if err != nil {
				t.Fatalf("%s: %v", filepath.Base(tp), err)
			}
			for _, p := range parts {
				if p.AgentID == "" {
					continue
				}
				reported++
				path, ok := a.SubagentTranscriptPath(tp, p.AgentID)
				if !ok {
					t.Errorf("agent id %q from %s produced no path", p.AgentID, filepath.Base(tp))
					continue
				}
				if _, err := os.Stat(path); err == nil {
					located++
				} else {
					missing++
					if firstMissing == "" {
						firstMissing = path
					}
				}
			}
		}
		f.Close()
	}
	t.Logf("%d subagents reported, %d transcripts located, %d missing", reported, located, missing)

	if reported == 0 {
		t.Skip("no subagents in any transcript here")
	}
	// Some will legitimately be absent — a child that never wrote a line,
	// or a session whose files were cleaned up. A majority missing would
	// mean the path convention is wrong.
	if located*2 < reported {
		t.Errorf("only %d of %d subagent transcripts were found (first miss: %s) — the path convention is wrong",
			located, reported, firstMissing)
	}
}
