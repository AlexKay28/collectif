package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #50 M2 slice A. Context projection is the mechanic the whole cell model
// rests on (ADR 0001 §4.2): to run cell i, fold cells [0, i) into a message
// list rather than accumulating one. These tests pin what each cell type
// contributes and that nothing downstream of the cell leaks in.

func projFixture(t *testing.T) (*Notebook, string) {
	t.Helper()
	root := t.TempDir()
	return &Notebook{Root: root}, root
}

func addProjCell(nb *Notebook, c Cell) {
	if c.State == "" {
		c.State = CellIdle
	}
	nb.Cells = append(nb.Cells, c)
}

func joinText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(string(m.Role))
		b.WriteString(": ")
		for _, c := range m.Content {
			b.WriteString(c.Text)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestProject_OnlyIncludesCellsAboveTheTarget(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{ID: "c0", Type: CellMarkdown, Source: "above"})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "the question"})
	addProjCell(nb, Cell{ID: "c2", Type: CellMarkdown, Source: "below"})

	msgs, err := projectContext(nb, 1)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	got := joinText(msgs)
	if !strings.Contains(got, "above") {
		t.Errorf("projection missing the cell above:\n%s", got)
	}
	if !strings.Contains(got, "the question") {
		t.Errorf("projection missing the target cell's own source:\n%s", got)
	}
	if strings.Contains(got, "below") {
		t.Errorf("projection leaked a cell below the target:\n%s", got)
	}
}

// Authored prose is instruction in an agent notebook, so it becomes a user
// message rather than being dropped.
func TestProject_MarkdownBecomesAUserMessage(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{ID: "c0", Type: CellMarkdown, Source: "# Context\nUse tabs."})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "go"})

	msgs, _ := projectContext(nb, 1)
	if msgs[0].Role != RoleUser {
		t.Errorf("markdown projected as %q, want %q", msgs[0].Role, RoleUser)
	}
	if !strings.Contains(msgs[0].Content[0].Text, "Use tabs.") {
		t.Errorf("markdown text missing: %+v", msgs[0])
	}
}

// A prompt cell contributes both halves of the exchange it produced, or the
// model has no idea what it already answered.
func TestProject_PromptContributesItsQuestionAndAnswer(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{
		ID: "c0", Type: CellPrompt, Source: "what is 2+2?", State: CellOK,
		Outputs: []Output{{Type: OutputText, Text: "four"}},
	})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "and 3+3?"})

	msgs, _ := projectContext(nb, 1)
	if len(msgs) < 3 {
		t.Fatalf("got %d messages, want question + answer + new question:\n%s", len(msgs), joinText(msgs))
	}
	if msgs[0].Role != RoleUser || !strings.Contains(msgs[0].Content[0].Text, "2+2") {
		t.Errorf("first message = %+v, want the earlier question", msgs[0])
	}
	if msgs[1].Role != RoleAssistant || !strings.Contains(msgs[1].Content[0].Text, "four") {
		t.Errorf("second message = %+v, want the earlier answer", msgs[1])
	}
}

// A prompt cell that never ran has no answer to contribute — projecting an
// empty assistant turn would tell the model it had already replied.
func TestProject_UnrunPromptContributesNoAssistantTurn(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{ID: "c0", Type: CellPrompt, Source: "never ran"})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "target"})

	msgs, _ := projectContext(nb, 1)
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			t.Errorf("projection invented an assistant turn:\n%s", joinText(msgs))
		}
	}
}

func TestProject_ShellContributesCommandAndOutput(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{
		ID: "c0", Type: CellShell, Source: "go test ./...", State: CellOK,
		Outputs: []Output{{Type: OutputText, Text: "ok  collectif  1.2s\n"}},
	})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "did it pass?"})

	got := joinText(mustProject(t, nb, 1))
	if !strings.Contains(got, "go test ./...") {
		t.Errorf("projection missing the command:\n%s", got)
	}
	if !strings.Contains(got, "ok  collectif") {
		t.Errorf("projection missing the command output:\n%s", got)
	}
}

// file cells are how you pin context deliberately. They re-read at
// projection time, so a notebook re-run after an edit sees the new file.
func TestProject_FileCellReReadsFromDiskEachTime(t *testing.T) {
	nb, root := projFixture(t)
	path := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(path, []byte("first version"), 0o644); err != nil {
		t.Fatal(err)
	}
	addProjCell(nb, Cell{ID: "c0", Type: CellFile, Source: "notes.txt"})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "summarise"})

	if got := joinText(mustProject(t, nb, 1)); !strings.Contains(got, "first version") {
		t.Fatalf("projection missing file contents:\n%s", got)
	}
	if err := os.WriteFile(path, []byte("second version"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := joinText(mustProject(t, nb, 1))
	if !strings.Contains(got, "second version") {
		t.Errorf("file cell did not re-read on projection:\n%s", got)
	}
	if strings.Contains(got, "first version") {
		t.Errorf("projection served a stale copy of the file:\n%s", got)
	}
}

// A file cell must not become a way to read outside the notebook root.
func TestProject_FileCellCannotEscapeTheNotebookRoot(t *testing.T) {
	nb, _ := projFixture(t)
	addProjCell(nb, Cell{ID: "c0", Type: CellFile, Source: "../../etc/passwd"})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "read it"})

	msgs, err := projectContext(nb, 1)
	if err == nil {
		if got := joinText(msgs); strings.Contains(got, "root:") {
			t.Fatal("a file cell read outside the notebook root")
		}
	}
	// Either an error or a contained miss is acceptable; serving the file
	// is not.
}

// Long command output is elided on the way to the model while the notebook
// keeps the whole thing — the document is the record, the projection is a
// budget.
func TestProject_TruncatesLongOutputButKeepsHeadAndTail(t *testing.T) {
	nb, _ := projFixture(t)
	long := strings.Repeat("NOISE\n", 20000) // well past the cap
	addProjCell(nb, Cell{
		ID: "c0", Type: CellShell, Source: "noisy", State: CellOK,
		Outputs: []Output{{Type: OutputText, Text: "HEAD-MARKER\n" + long + "TAIL-MARKER\n"}},
	})
	addProjCell(nb, Cell{ID: "c1", Type: CellPrompt, Source: "what happened?"})

	got := joinText(mustProject(t, nb, 1))
	if len(got) > 4*projectionCellBudget {
		t.Errorf("projection is %d bytes, want it bounded near %d", len(got), projectionCellBudget)
	}
	if !strings.Contains(got, "HEAD-MARKER") {
		t.Error("truncation dropped the head of the output")
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Error("truncation dropped the tail of the output — the end is usually the error")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncation was silent; the model should be told it is reading an excerpt")
	}
}

func mustProject(t *testing.T, nb *Notebook, i int) []Message {
	t.Helper()
	msgs, err := projectContext(nb, i)
	if err != nil {
		t.Fatalf("projectContext: %v", err)
	}
	return msgs
}
