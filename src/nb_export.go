package main

// nb_export.go — a notebook as markdown. #56 (M7), per ADR 0002.
//
// Why markdown and not another JSON view: a projected session is the record
// of what an agent did, and every place that record is wanted — a PR
// description, an issue comment, a commit message — renders markdown and
// none of them can render a notebook. The log stays the archive; this is
// the form you can paste.
//
// Two rules shape the format.
//
// Structure is carried in HTML comments. GitHub drops them when it renders,
// so they cost the reader nothing, and they are what makes the document
// readable *back* — which is the only way to know the export did not
// quietly lose a cell. An export nobody can parse fails silently, and by
// the time anyone notices, the thing it was a record of is gone.
//
// What markdown cannot represent is summarised rather than faked. A tool
// call is a name over a JSON argument tree; there is no faithful markdown
// for that, so it becomes one line and the format says so rather than
// pretending the round trip is total. D11 applies to our own artifacts too.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// nbExportLongOutput mirrors the thresholds in nb_render.js: past them an
// output is collapsed behind a <details> instead of being inlined. A single
// tool result is routinely thousands of lines, and inline it buries the
// answer that follows — the same reason the renderer folds it.
const (
	nbExportLongChars = 600
	nbExportLongLines = 12
)

func handleNotebookExport(w http.ResponseWriter, r *http.Request, st *notebookStore) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc := st.Doc()
	// Doc() folds the log and has no idea what the file is called; the slug
	// is the notebook's name everywhere else, so the export carries it too.
	doc.ID = st.slug
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	// inline, not attachment: this is meant to be read and copied, and a
	// browser that downloads it instead has put a file between the user and
	// the paste they came for.
	w.Header().Set("Content-Disposition", `inline; filename="`+st.slug+`.md"`)
	_, _ = w.Write([]byte(exportMarkdown(doc)))
}

func exportMarkdown(nb *Notebook) string {
	var b strings.Builder
	title := nb.Title
	if title == "" {
		title = "Untitled notebook"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)

	fields := []string{}
	if nb.ID != "" {
		fields = append(fields, "id="+nb.ID)
	}
	fields = append(fields, fmt.Sprintf("cells=%d", len(nb.Cells)))
	if nb.Meta.CLI != "" {
		fields = append(fields, "cli="+nb.Meta.CLI)
	}
	if nb.Meta.SessionID != "" {
		fields = append(fields, "session="+nb.Meta.SessionID)
	}
	fields = append(fields, "exported="+time.Now().UTC().Format(time.RFC3339))
	b.WriteString(nbMarker("notebook", fields...))
	b.WriteString("\n\n")

	// One visible line of provenance. Without it an exported document is a
	// transcript with no author: a reader three months later cannot tell
	// whether a person wrote these cells or an agent was watched writing
	// them, and the two mean different things about how much to trust them.
	sub := []string{}
	if nb.Root != "" {
		sub = append(sub, "`"+nb.Root+"`")
	}
	if nb.Meta.SessionID != "" {
		cli := nb.Meta.CLI
		if cli == "" {
			cli = "a CLI"
		}
		sub = append(sub, "projected from a live "+cli+" session")
	}
	if len(sub) > 0 {
		b.WriteString("*" + strings.Join(sub, " · ") + "*\n\n")
	}
	if note := fidelityNote(nb.Fidelity); note != "" {
		b.WriteString(note + "\n\n")
	}

	for i := range nb.Cells {
		writeCell(&b, &nb.Cells[i], i+1)
	}
	return b.String()
}

// fidelityNote is the export's half of D11. A codex session exported with
// no turns in it reads as an agent that did nothing; the note is the
// difference between "nothing happened" and "we could not see it".
func fidelityNote(f *NotebookFidelity) string {
	if f == nil {
		return ""
	}
	var missing []string
	if !f.Turns {
		missing = append(missing, "its turns are not shown here — collectif cannot read "+f.CLI+"'s transcript format")
	}
	if !f.Approvals {
		missing = append(missing, "permission requests do not appear here")
	}
	if !f.Usage {
		missing = append(missing, "token counts are unavailable")
	}
	if !f.Subagents {
		missing = append(missing, "work it delegated to subagents is not nested here")
	}
	if len(missing) == 0 {
		return ""
	}
	return "> **Partial view of this " + f.CLI + " session.** " + strings.Join(missing, "; ") + "."
}

func writeCell(b *strings.Builder, c *Cell, index int) {
	fields := []string{"type=" + string(c.Type)}
	if c.Meta.Provenance != "" {
		fields = append(fields, "provenance="+c.Meta.Provenance)
	}
	state := c.State
	if state == "" {
		state = CellIdle
	}
	fields = append(fields, "state="+string(state))
	// Duration and cost ride in the marker rather than on the page. They
	// belong to the record — someone will want to know what a turn cost —
	// but a PR description is read for the narrative, and a per-cell
	// timing line under every paragraph is noise in that context.
	if c.Duration > 0 {
		fields = append(fields, "duration="+c.Duration.Round(time.Millisecond).String())
	}
	if in := c.Usage.InputTokens + c.Usage.CacheReadTokens + c.Usage.CacheCreationTokens; in > 0 || c.Usage.OutputTokens > 0 {
		fields = append(fields, fmt.Sprintf("tokens=%d/%d", in, c.Usage.OutputTokens))
	}
	b.WriteString(nbMarker(fmt.Sprintf("cell %d", index), fields...))
	b.WriteString("\n\n")

	if src := strings.TrimRight(c.Source, "\n"); src != "" {
		switch c.Type {
		case CellMarkdown:
			// Already markdown. Anything else here would be re-encoding a
			// document into itself.
			b.WriteString(src)
		case CellPrompt:
			// A prompt is an instruction someone gave, not the document's
			// own voice — the same distinction the notebook draws by
			// setting prompts as a quotation rather than as body text.
			b.WriteString(blockquote(src))
		default:
			b.WriteString(fencedBlock(src, langFor(c.Type)))
		}
		b.WriteString("\n\n")
	}
	writeOutputs(b, c)
}

func writeOutputs(b *strings.Builder, c *Cell) {
	// An approval is two log entries — the question, then the verdict —
	// paired by id. The reader wants one thing, so they are folded here
	// exactly as nb_render.js folds them for the screen.
	verdicts := map[string]string{}
	for _, o := range c.Outputs {
		if o.Type != OutputApproval {
			continue
		}
		if res, _ := o.Data["resolution"].(string); res != "" {
			if id, _ := o.Data["approvalId"].(string); id != "" {
				verdicts[id] = res
			}
		}
	}
	byAgent := map[string][]Output{}
	for _, o := range c.Outputs {
		if id, _ := o.Data["agentId"].(string); id != "" {
			byAgent[id] = append(byAgent[id], o)
		}
	}

	var injections []Output
	flushInjections := func() {
		if len(injections) > 0 {
			writeInjections(b, injections)
			injections = nil
		}
	}
	drawn := map[string]bool{}
	for _, o := range c.Outputs {
		if id, _ := o.Data["agentId"].(string); id != "" {
			flushInjections()
			if !drawn[id] {
				drawn[id] = true
				writeSubagent(b, byAgent[id])
			}
			continue
		}
		if o.Type == OutputInjection {
			injections = append(injections, o)
			continue
		}
		flushInjections()
		if o.Type == OutputApproval {
			if res, _ := o.Data["resolution"].(string); res != "" {
				continue // folded into its question above
			}
			id, _ := o.Data["approvalId"].(string)
			writeApproval(b, o, verdicts[id])
			continue
		}
		writeOutput(b, o, true)
	}
	flushInjections()
}

// writeOutput renders one block. marked is false for a child's outputs
// inside a subagent's <details>: a structural marker in there would read as
// a sibling of the parent's own outputs on the way back in, which is the
// one thing the round trip exists to catch.
func writeOutput(b *strings.Builder, o Output, marked bool) {
	mark := func(fields ...string) {
		if marked {
			b.WriteString(nbMarker("output", fields...) + "\n\n")
		}
	}
	switch o.Type {
	case OutputText:
		// The agent's prose is prose. Fencing it would turn the one part of
		// a session that reads as writing into a code listing.
		if strings.TrimSpace(o.Text) == "" {
			return
		}
		mark("type=text")
		b.WriteString(strings.TrimRight(o.Text, "\n") + "\n\n")

	case OutputToolCall:
		name, _ := o.Data["name"].(string)
		if name == "" {
			name = o.Text
		}
		if name == "" {
			name = "tool"
		}
		mark("type=tool_call")
		line := "**→ " + name + "**"
		if args := toolArgSummary(o.Data["input"]); args != "" {
			line += " `" + strings.ReplaceAll(oneLine(args), "`", "'") + "`"
		}
		b.WriteString(line + "\n\n")

	default:
		if strings.TrimSpace(o.Text) == "" {
			return
		}
		mark("type=" + string(o.Type))
		lang := ""
		if o.Type == OutputDiff {
			lang = "diff"
		}
		block := fencedBlock(strings.TrimRight(o.Text, "\n"), lang)
		if len(o.Text) > nbExportLongChars || strings.Count(o.Text, "\n") >= nbExportLongLines {
			head := oneLine(strings.SplitN(o.Text, "\n", 2)[0])
			if len(head) > 110 {
				head = head[:110]
			}
			if head == "" {
				head = string(o.Type)
			}
			// The blank lines inside <details> are load-bearing: GitHub only
			// renders markdown inside an HTML block when it is separated
			// from the tags by one.
			block = "<details>\n<summary>" + escapeHTMLText(head) + "</summary>\n\n" + block + "\n</details>"
		}
		b.WriteString(block + "\n\n")
	}
}

func writeApproval(b *strings.Builder, o Output, verdict string) {
	b.WriteString(nbMarker("output", "type=approval") + "\n\n")
	parts := []string{}
	if tool, _ := o.Data["tool"].(string); tool != "" {
		parts = append(parts, "**"+tool+"**")
	}
	if q := oneLine(o.Text); q != "" {
		parts = append(parts, q)
	}
	if args := toolArgSummary(o.Data["input"]); args != "" {
		parts = append(parts, "`"+strings.ReplaceAll(oneLine(args), "`", "'")+"`")
	}
	if verdict == "" {
		verdict = "unanswered"
	}
	b.WriteString("> **Approval** — " + strings.Join(parts, " ") + " — **" + verdict + "**\n\n")
}

// writeInjections folds a run of injections into one line, as the renderer
// does. Recording them individually is right for the log — an injection you
// cannot see is often the whole explanation for a surprising turn — but
// thirty lines of "system reminder, 4 KB" in a PR description would bury
// the turn they belong to.
func writeInjections(b *strings.Builder, outs []Output) {
	var total float64
	for _, o := range outs {
		if n, ok := o.Data["size"].(float64); ok {
			total += n
		}
	}
	b.WriteString(nbMarker("output", "type=injection", fmt.Sprintf("count=%d", len(outs))) + "\n\n")
	fmt.Fprintf(b, "*%d context injection%s, %s the model read that nobody typed*\n\n",
		len(outs), plural(len(outs)), fmtExportBytes(int64(total)))
}

// writeSubagent nests a delegated child's work under the turn that spawned
// it (#55a), collapsed for the same reason the renderer collapses it: the
// parent's narrative is what is being read, and the child is there for when
// the answer is surprising.
func writeSubagent(b *strings.Builder, outs []Output) {
	kind := "subagent"
	for _, o := range outs {
		if t, _ := o.Data["agentType"].(string); t != "" {
			kind = t
			break
		}
	}
	var inner strings.Builder
	for _, o := range outs {
		writeOutput(&inner, o, false)
	}
	b.WriteString(nbMarker("output", "type=subagent", "agent="+kind) + "\n\n")
	b.WriteString("<details>\n<summary>delegated to " + escapeHTMLText(kind) + "</summary>\n\n" +
		strings.TrimRight(inner.String(), "\n") + "\n</details>\n\n")
}

// ─── Formatting ─────────────────────────────────────────────────────────

func nbMarker(kind string, fields ...string) string {
	if len(fields) == 0 {
		return "<!-- collectif:" + kind + " -->"
	}
	return "<!-- collectif:" + kind + " " + strings.Join(fields, " ") + " -->"
}

// fencedBlock wraps text in a fence long enough to survive its contents. An
// agent that printed a markdown fence — which is most of them, most days —
// closes a three-backtick fence early, and the rest of the export renders
// as prose from there down.
func fencedBlock(text, lang string) string {
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	n := longest + 1
	if n < 3 {
		n = 3
	}
	fence := strings.Repeat("`", n)
	return fence + lang + "\n" + text + "\n" + fence
}

func blockquote(text string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if l == "" {
			lines[i] = ">"
			continue
		}
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}

func langFor(t CellType) string {
	if t == CellShell {
		return "sh"
	}
	return ""
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// escapeHTMLText guards the one place export writes raw HTML. A summary is
// the agent's own first line of output, so it can contain anything.
func escapeHTMLText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// toolArgSummary picks the one argument worth showing, matching
// nb_render.js's toolArgs: a Bash call is its command, a Read is its path.
// Falling back to the whole JSON blob is right for a tool we do not know
// and wrong for the handful that account for most calls.
func toolArgSummary(input any) string {
	if input == nil {
		return ""
	}
	m, ok := input.(map[string]any)
	if !ok {
		return fmt.Sprint(input)
	}
	for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url", "description"} {
		if v, ok := m[key].(string); ok && v != "" {
			return v
		}
	}
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func fmtExportBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}
