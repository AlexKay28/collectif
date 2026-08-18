package main

import (
	"net/http"
	"strings"
	"testing"
)

// #56 — markdown export. The round trip is the test that matters: an
// export nobody can read back is an export that has quietly dropped
// something, and the failure is invisible until the day you need the
// record. parseExportedMarkdown below is the inverse, and it lives here
// rather than in the product because nothing imports notebooks — its only
// job is to prove the format is unambiguous.

func TestExportMarkdown_RoundTripsCellsAndOutputs(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	st, err := createNotebook("Health endpoint", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}

	want := []Cell{
		{ID: "c1", Type: CellMarkdown, Source: "## Why\n\nThe probe was lying.", State: CellIdle},
		{ID: "c2", Type: CellPrompt, Source: "add a health-check endpoint\nand a test for it",
			State: CellOK, Meta: CellMeta{Provenance: ProvenanceMirrored}},
		{ID: "c3", Type: CellShell, Source: "go test ./src", State: CellError},
	}
	for _, c := range want {
		if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: c}); err != nil {
			t.Fatalf("insert %s: %v", c.ID, err)
		}
	}
	outs := map[string][]Output{
		"c2": {
			{Type: OutputText, Text: "Added /healthz.\n\nIt returns 200 while the PTY is alive."},
			{Type: OutputToolResult, Text: "ok  \tgithub.com/AlexKay28/collectif/src\t0.412s"},
		},
		"c3": {{Type: OutputError, Text: "FAIL\tgithub.com/AlexKay28/collectif/src\t0.9s"}},
	}
	for cellID, list := range outs {
		for _, o := range list {
			if _, err := st.Append(evOutputAppended, outputAppendedPayload{CellID: cellID, Output: o}); err != nil {
				t.Fatalf("output on %s: %v", cellID, err)
			}
		}
	}

	rec := nbRequest(t, srv, http.MethodGet, "/api/nb/"+st.slug+"/export", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("export: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
		t.Errorf("Content-Type = %q, want text/markdown", ct)
	}
	md := rec.Body.String()
	if !strings.HasPrefix(md, "# Health endpoint\n") {
		t.Errorf("export does not open with the title:\n%s", firstLines(md, 3))
	}

	got := parseExportedMarkdown(t, md)
	if len(got.Cells) != len(want) {
		t.Fatalf("round trip returned %d cells, want %d:\n%s", len(got.Cells), len(want), md)
	}
	for i, w := range want {
		g := got.Cells[i]
		if g.Type != w.Type {
			t.Errorf("cell %d type = %q, want %q", i+1, g.Type, w.Type)
		}
		if g.Source != w.Source {
			t.Errorf("cell %d source = %q, want %q", i+1, g.Source, w.Source)
		}
		if g.State != w.State {
			t.Errorf("cell %d state = %q, want %q", i+1, g.State, w.State)
		}
		if g.Meta.Provenance != w.Meta.Provenance {
			t.Errorf("cell %d provenance = %q, want %q", i+1, g.Meta.Provenance, w.Meta.Provenance)
		}
		for j, wo := range outs[w.ID] {
			if j >= len(g.Outputs) {
				t.Errorf("cell %d lost output %d (%s)", i+1, j, wo.Type)
				continue
			}
			if g.Outputs[j].Type != wo.Type {
				t.Errorf("cell %d output %d type = %q, want %q", i+1, j, g.Outputs[j].Type, wo.Type)
			}
			if g.Outputs[j].Text != wo.Text {
				t.Errorf("cell %d output %d text = %q, want %q", i+1, j, g.Outputs[j].Text, wo.Text)
			}
		}
	}
}

// A tool result that printed a markdown fence of its own closes a
// three-backtick fence early, and everything after it renders as prose.
// Real transcripts are full of them — every agent that writes markdown.
func TestExportMarkdown_FenceSurvivesBackticksInOutput(t *testing.T) {
	withTempNotebooks(t)
	st, err := createNotebook("Fences", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	body := "here is a fence:\n```go\nfunc main() {}\n```\nand text after it"
	if _, err := st.Append(evCellInserted, cellInsertedPayload{
		Cell: Cell{ID: "c1", Type: CellShell, Source: "cat README.md", State: CellOK},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := st.Append(evOutputAppended, outputAppendedPayload{
		CellID: "c1", Output: Output{Type: OutputToolResult, Text: body},
	}); err != nil {
		t.Fatalf("output: %v", err)
	}

	got := parseExportedMarkdown(t, exportMarkdown(st.Doc()))
	if len(got.Cells) != 1 || len(got.Cells[0].Outputs) != 1 {
		t.Fatalf("round trip = %+v", got.Cells)
	}
	if got.Cells[0].Outputs[0].Text != body {
		t.Errorf("output text = %q, want %q", got.Cells[0].Outputs[0].Text, body)
	}
}

// D11: never claim fidelity we lack. An exported codex session that shows
// no turns has to say that is why, or the reader concludes the agent did
// nothing.
func TestExportMarkdown_SaysWhatTheSessionCouldNotShow(t *testing.T) {
	withTempNotebooks(t)
	st, err := createNotebook("Codex run", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	if _, err := st.Append(evMetaSet, metaSetPayload{
		Meta: &NotebookMeta{CLI: "codex", SessionID: "sess-1"},
	}); err != nil {
		t.Fatalf("meta: %v", err)
	}
	md := exportMarkdown(st.Doc())
	if !strings.Contains(md, "codex") || !strings.Contains(strings.ToLower(md), "not shown") {
		t.Errorf("export of a session with no turn projection does not say so:\n%s", md)
	}
}

func TestNotebookAPI_ExportRejectsNonGET(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	st, err := createNotebook("Health endpoint", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	rec := nbRequest(t, srv, http.MethodPost, "/api/nb/"+st.slug+"/export", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST export = %d, want 405", rec.Code)
	}
}

// ─── The inverse ────────────────────────────────────────────────────────

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// parseExportedMarkdown reads an export back into a document. It knows
// only what the format promises to preserve: the cells, and the text of
// the outputs written verbatim. Tool calls, approvals and injections are
// summary lines by design — markdown has no faithful form for a structured
// argument list, and the notebook's log stays the record.
func parseExportedMarkdown(t *testing.T, md string) *Notebook {
	t.Helper()
	nb := &Notebook{}
	var buf []string

	flush := func() {
		text := strings.Trim(strings.Join(buf, "\n"), "\n")
		buf = nil
		if text == "" || len(nb.Cells) == 0 {
			return
		}
		cell := &nb.Cells[len(nb.Cells)-1]
		if n := len(cell.Outputs); n > 0 && cell.Outputs[n-1].Text == "" {
			cell.Outputs[n-1].Text = undecorate(text)
			return
		}
		if cell.Source == "" {
			switch cell.Type {
			case CellPrompt:
				cell.Source = unquote(text)
			case CellMarkdown:
				cell.Source = text
			default:
				cell.Source = unfence(text)
			}
		}
	}

	for _, line := range strings.Split(md, "\n") {
		switch {
		case strings.HasPrefix(line, "<!-- collectif:cell "):
			flush()
			f := markerFields(line)
			nb.Cells = append(nb.Cells, Cell{
				Type:  CellType(f["type"]),
				State: CellState(f["state"]),
				Meta:  CellMeta{Provenance: f["provenance"]},
			})
		case strings.HasPrefix(line, "<!-- collectif:output "):
			flush()
			if len(nb.Cells) == 0 {
				t.Fatalf("output marker before any cell: %q", line)
			}
			cell := &nb.Cells[len(nb.Cells)-1]
			cell.Outputs = append(cell.Outputs, Output{Type: OutputType(markerFields(line)["type"])})
		case strings.HasPrefix(line, "<!-- collectif:"):
			flush() // the notebook header; its fields are not round-tripped
		default:
			buf = append(buf, line)
		}
	}
	flush()
	return nb
}

// markerFields reads `<!-- collectif:cell 2 type=prompt state=ok -->` into
// {"type": "prompt", "state": "ok"}. Positional words are ignored.
func markerFields(line string) map[string]string {
	line = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(line), "<!--"), "-->")
	out := map[string]string{}
	for _, tok := range strings.Fields(line) {
		if k, v, ok := strings.Cut(tok, "="); ok {
			out[k] = v
		}
	}
	return out
}

func unquote(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimPrefix(l, ">"), " ")
	}
	return strings.Join(lines, "\n")
}

// unfence removes a fenced block's delimiters. The opening fence may carry
// a language and may be longer than three backticks, so the closer is read
// off the opener rather than assumed.
func unfence(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "```") {
		return text
	}
	closer := lines[0]
	for len(closer) > 0 && closer[len(closer)-1] != '`' {
		closer = closer[:len(closer)-1]
	}
	if strings.TrimSpace(lines[len(lines)-1]) != closer {
		return text
	}
	return strings.Join(lines[1:len(lines)-1], "\n")
}

// undecorate strips the <details> wrapper a long output gets before
// unfencing what is inside it.
func undecorate(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) > 2 && strings.HasPrefix(lines[0], "<details>") {
		end := len(lines)
		for end > 0 && strings.TrimSpace(lines[end-1]) != "</details>" {
			end--
		}
		if end > 0 {
			inner := lines[1 : end-1]
			for len(inner) > 0 && (strings.TrimSpace(inner[0]) == "" || strings.HasPrefix(inner[0], "<summary>")) {
				inner = inner[1:]
			}
			text = strings.Trim(strings.Join(inner, "\n"), "\n")
		}
	}
	return unfence(text)
}
