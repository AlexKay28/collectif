package main

// nb_project.go — context projection. #50 (M2), ADR 0001 §4.2.
//
// The hard problem with editable, out-of-order cells is that conversation
// context is normally *accumulated*, and you cannot un-say a message. That
// is Jupyter's hidden-state problem wearing a new costume: the kernel holds
// state that no longer matches the visible code, so deleting a cell doesn't
// delete its effects. marimo answered it by making the notebook a dataflow
// graph and deriving state instead of accumulating it.
//
// We apply the same move to conversation context: to run cell i, fold cells
// [0, i) into a message list, then append cell i's own source. Nothing is
// snapshotted. There is no kernel state to rewind, so editing cell 3 and
// re-running it is simply a different projection.
//
// The cost is that every run re-sends the prefix, which is affordable only
// because the prefix is stable enough to be cached — see M2.5 (#51). Two
// rules here exist for that reason and not for tidiness: rendering is
// deterministic (no timestamps, no map iteration), and oversized cell
// output is elided on the way out rather than left to blow the window.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// projectionCellBudget bounds what one cell may contribute. The notebook
// keeps the whole output; this is only what the model is shown. Head and
// tail are both kept because the beginning says what ran and the end is
// usually where the error is.
const projectionCellBudget = 8 * 1024

// projectContext folds cells [0, target) into a message list and appends
// the target cell's own source.
func projectContext(nb *Notebook, target int) ([]Message, error) {
	if nb == nil || target < 0 || target >= len(nb.Cells) {
		return nil, fmt.Errorf("projectContext: cell index %d out of range", target)
	}
	var msgs []Message
	for i := 0; i < target; i++ {
		m, err := projectCell(nb, nb.Cells[i])
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m...)
	}
	// The target contributes its question only; its own previous answer is
	// deliberately absent, because we are about to produce a new one.
	self := nb.Cells[target]
	if src := strings.TrimSpace(self.Source); src != "" {
		msgs = append(msgs, userText(src))
	}
	return msgs, nil
}

// projectCell is the per-type contribution table from ADR §4.2.
func projectCell(nb *Notebook, c Cell) ([]Message, error) {
	switch c.Type {
	case CellMarkdown:
		// Authored prose in an agent notebook is nearly always instruction,
		// so it is context rather than decoration.
		if src := strings.TrimSpace(c.Source); src != "" {
			return []Message{userText(src)}, nil
		}
		return nil, nil

	case CellPrompt:
		if strings.TrimSpace(c.Source) == "" {
			return nil, nil
		}
		// Trimmed exactly as the target's own source is below. A cell has
		// to render identically whether it is the one being run or context
		// above one, or a trailing newline from a textarea makes the same
		// cell byte-different between two runs and the cached prefix misses
		// — silently, at full price.
		out := []Message{userText(strings.TrimSpace(c.Source))}
		// Only a cell that actually produced an answer contributes one.
		// Projecting an empty assistant turn would tell the model it had
		// already replied when it had not.
		if blocks := assistantBlocks(c); len(blocks) > 0 {
			out = append(out, Message{Role: RoleAssistant, Content: blocks})
		}
		return out, nil

	case CellShell:
		if strings.TrimSpace(c.Source) == "" {
			return nil, nil
		}
		var b strings.Builder
		b.WriteString("$ ")
		b.WriteString(c.Source)
		if text := cellOutputText(c); text != "" {
			b.WriteString("\n")
			b.WriteString(elide(text, projectionCellBudget))
		}
		if c.State == CellError {
			b.WriteString("\n(command failed)")
		}
		return []Message{userText(b.String())}, nil

	case CellFile:
		path := strings.TrimSpace(c.Source)
		if path == "" {
			return nil, nil
		}
		// Re-read on every projection, so a notebook re-run after an edit
		// sees the new file rather than a copy frozen at authoring time.
		body, err := readContainedFile(nb.Root, path)
		if err != nil {
			// A missing or out-of-bounds file is context the model should
			// know about, not a reason to fail the run.
			return []Message{userText(fmt.Sprintf("File %s could not be read: %v", path, err))}, nil
		}
		return []Message{userText(fmt.Sprintf("File %s:\n%s", path, elide(body, projectionCellBudget)))}, nil
	}
	return nil, nil
}

// assistantBlocks reconstructs the model turn a prompt cell recorded.
// Thinking is not replayed: transports either reject foreign thinking
// blocks or silently drop them, and the summary we stored is not the thing
// the model would need anyway.
func assistantBlocks(c Cell) []ContentBlock {
	var out []ContentBlock
	for _, o := range c.Outputs {
		switch o.Type {
		case OutputText:
			if strings.TrimSpace(o.Text) != "" {
				out = append(out, ContentBlock{Type: BlockText, Text: elide(o.Text, projectionCellBudget)})
			}
		}
	}
	return out
}

func cellOutputText(c Cell) string {
	var b strings.Builder
	for _, o := range c.Outputs {
		if o.Type == OutputText || o.Type == OutputError {
			b.WriteString(o.Text)
		}
	}
	return b.String()
}

func userText(s string) Message {
	return Message{Role: RoleUser, Content: []ContentBlock{{Type: BlockText, Text: s}}}
}

// elide trims the middle of an oversized string, keeping both ends and
// saying so. Silence would let the model reason confidently about an
// excerpt it believes is the whole thing.
func elide(s string, budget int) string {
	if len(s) <= budget {
		return s
	}
	half := budget / 2
	// Cut on rune boundaries. Slicing by byte offset splits multi-byte
	// characters, and json.Marshal turns the fragments into U+FFFD before
	// the request goes out — so the model reads a mangled boundary.
	head := s[:runeBoundaryBefore(s, half)]
	tail := s[runeBoundaryAfter(s, len(s)-half):]
	return fmt.Sprintf("%s\n… %d bytes truncated …\n%s", head, len(s)-len(head)-len(tail), tail)
}

// runeBoundaryBefore returns the largest index <= i that starts a rune.
func runeBoundaryBefore(s string, i int) int {
	if i > len(s) {
		i = len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeBoundaryAfter returns the smallest index >= i that starts a rune.
func runeBoundaryAfter(s string, i int) int {
	if i < 0 {
		i = 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// readContainedFile reads a path relative to root, refusing anything that
// resolves outside it. Containment is checked here rather than left to the
// policy engine because a file cell must never become a way to read the
// filesystem at large.
func readContainedFile(root, rel string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("notebook has no root")
	}
	abs, err := containedPath(root, rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// containedPath resolves rel against root and verifies the result stays
// inside it, following symlinks so a link cannot step out.
func containedPath(root, rel string) (string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	target := rel
	if !filepath.IsAbs(target) {
		target = filepath.Join(rootAbs, rel)
	}
	target = filepath.Clean(target)
	target = resolveDeepestExisting(target)
	relToRoot, err := filepath.Rel(rootAbs, target)
	if err != nil {
		return "", fmt.Errorf("path %q is outside the notebook root", rel)
	}
	if relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the notebook root", rel)
	}
	return target, nil
}

// resolveDeepestExisting resolves the symlinks on the longest prefix of p
// that exists, and re-attaches the components that do not exist yet.
//
// The obvious version — EvalSymlinks(p), keep the old value on error — was
// what this used to be, and it was a containment hole rather than a
// fallback. EvalSymlinks is all-or-nothing: it fails on any path whose last
// component is missing, so for `root/link/newfile` it returned an error and
// the caller kept the *lexical* path, which is inside the root by
// inspection and outside it in fact. Every path a write tool is given names
// a file that does not exist yet, so the read tools' containment check did
// not transfer to them at all (#52).
func resolveDeepestExisting(p string) string {
	var missing string
	for probe := p; ; {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			return filepath.Join(resolved, missing)
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			// Walked to the filesystem root without finding anything that
			// exists. Nothing can be resolved, so the lexical path is all
			// there is — and it cannot be hiding a link, because there is
			// no link on it to hide.
			return p
		}
		missing = filepath.Join(filepath.Base(probe), missing)
		probe = parent
	}
}
