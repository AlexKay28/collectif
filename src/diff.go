package main

// diff.go — the unified diff behind the `diff` output type. #52 (M3).
//
// ADR 0001 §4.1 singles a rendered diff out as "the single highest-value
// thing a notebook shows that a terminal cannot", and nb_render.js has been
// able to colourise one since M1. Nothing produced one until the write
// tools landed, so this is the other half of a feature that was already
// half built.
//
// The output is git's format, not an approximation of it, because the
// renderer classifies lines by their first character and because a diff a
// person can paste into `git apply` is worth more than one they cannot.
//
// Shelling out to `git diff --no-index` was the alternative. It was
// rejected for the reason the rest of this package avoids second-hand
// knowledge: it needs git installed, it needs the file already on disk (so
// it cannot preview a write *before* it happens, which is exactly what the
// approval widget must show), and its exit codes conflate "no differences"
// with "could not run".

import (
	"fmt"
	"strings"
)

// diffContext is how many unchanged lines surround each change. Three is
// git's default and the number people's eyes are calibrated to.
const diffContext = 3

// diffLCSBudget caps the O(n·m) table. Beyond it the middle section is
// reported as a wholesale replacement rather than a minimal edit script:
// a worse diff is a fine outcome, a gigabyte of ints is not. The common
// prefix and suffix are trimmed first, so this is reached only when a file
// genuinely changed almost everywhere.
const diffLCSBudget = 4_000_000

// noEOLMarker is git's own wording. It is load-bearing rather than
// cosmetic: a diff that silently invents a trailing newline reports a
// change that did not happen and hides one that did.
const noEOLMarker = `\ No newline at end of file`

// diffLine carries the missing-final-newline flag alongside the text so
// that adding a trailing newline registers as a change. Comparing text
// alone made "one\ntwo" and "one\ntwo\n" identical, which is the one edit
// people make by accident and the one they most want to see.
type diffLine struct {
	text  string
	noEOL bool
}

type diffOp struct {
	op   byte // ' ' context, '-' removed, '+' added
	line diffLine
}

// unifiedDiff renders the change from old to next as a unified diff, or ""
// when there is none. path is used for both file headers; a rename is not
// something the write tools can do, so there is no second path to take.
func unifiedDiff(path, old, next string) string {
	a, b := splitDiffLines(old), splitDiffLines(next)
	ops := diffOps(a, b)

	hunks := groupHunks(ops)
	if len(hunks) == 0 {
		return ""
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n", path)
	fmt.Fprintf(&out, "+++ b/%s\n", path)
	for _, h := range hunks {
		out.WriteString(h.render(ops))
	}
	return out.String()
}

// splitDiffLines turns text into lines without the "" that trailing-newline
// splitting produces, recording instead that the last line had no newline.
func splitDiffLines(s string) []diffLine {
	if s == "" {
		return nil
	}
	noEOL := !strings.HasSuffix(s, "\n")
	if !noEOL {
		s = s[:len(s)-1]
	}
	parts := strings.Split(s, "\n")
	out := make([]diffLine, len(parts))
	for i, p := range parts {
		out[i] = diffLine{text: p}
	}
	out[len(out)-1].noEOL = noEOL
	return out
}

// diffOps produces the edit script. Common prefix and suffix are removed
// first: a typical edit changes a handful of lines in a large file, and
// without the trim the LCS table would be sized by the file rather than by
// the change.
func diffOps(a, b []diffLine) []diffOp {
	var ops []diffOp

	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}

	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ops = append(ops, diffMiddle(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

func diffMiddle(a, b []diffLine) []diffOp {
	switch {
	case len(a) == 0 && len(b) == 0:
		return nil
	case len(a) == 0 || len(b) == 0 || len(a)*len(b) > diffLCSBudget:
		ops := make([]diffOp, 0, len(a)+len(b))
		for _, l := range a {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range b {
			ops = append(ops, diffOp{'+', l})
		}
		return ops
	}

	// Longest common subsequence, bottom-up so the walk below reads
	// forwards and the deletions of a replaced block come out before its
	// additions — which is the order every diff reader expects.
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// ─── Hunks ──────────────────────────────────────────────────────────────

type diffHunk struct {
	from, to           int // op index range, half-open
	oldStart, oldCount int
	newStart, newCount int
}

// groupHunks collects each run of changes with diffContext lines either
// side, merging runs whose context would otherwise overlap. Without the
// merge, two edits four lines apart print the same four lines twice and
// claim they are separate places in the file.
func groupHunks(ops []diffOp) []diffHunk {
	var changed []int
	for i, o := range ops {
		if o.op != ' ' {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}

	// Running line numbers, so a hunk header can be computed from an op
	// index without rescanning.
	oldNo := make([]int, len(ops)+1)
	newNo := make([]int, len(ops)+1)
	for i, o := range ops {
		oldNo[i+1], newNo[i+1] = oldNo[i], newNo[i]
		if o.op != '+' {
			oldNo[i+1]++
		}
		if o.op != '-' {
			newNo[i+1]++
		}
	}

	var hunks []diffHunk
	start := max(0, changed[0]-diffContext)
	end := min(len(ops), changed[0]+diffContext+1)
	for _, idx := range changed[1:] {
		if idx-diffContext <= end {
			end = min(len(ops), idx+diffContext+1)
			continue
		}
		hunks = append(hunks, newHunk(start, end, oldNo, newNo))
		start, end = max(0, idx-diffContext), min(len(ops), idx+diffContext+1)
	}
	return append(hunks, newHunk(start, end, oldNo, newNo))
}

func newHunk(from, to int, oldNo, newNo []int) diffHunk {
	h := diffHunk{
		from: from, to: to,
		oldCount: oldNo[to] - oldNo[from],
		newCount: newNo[to] - newNo[from],
	}
	// git's convention: a side that contributes no lines is numbered from
	// the line before it, which for an empty file is line 0.
	h.oldStart, h.newStart = oldNo[from], newNo[from]
	if h.oldCount > 0 {
		h.oldStart++
	}
	if h.newCount > 0 {
		h.newStart++
	}
	return h
}

func (h diffHunk) render(ops []diffOp) string {
	var b strings.Builder
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", h.oldStart, h.oldCount, h.newStart, h.newCount)
	for _, o := range ops[h.from:h.to] {
		b.WriteByte(o.op)
		b.WriteString(o.line.text)
		b.WriteByte('\n')
		if o.line.noEOL {
			b.WriteString(noEOLMarker + "\n")
		}
	}
	return b.String()
}
