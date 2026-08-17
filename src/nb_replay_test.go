package main

import (
	"bufio"
	"os"
	"testing"
)

// #47 P0 — the demonstration harness.
//
// Every other test here asserts. This one *shows*: it replays a real
// transcript through the projector into a real notebook so the resulting
// document can be opened in the browser and judged as a document, which is
// the only way to find out whether the shape chosen in slice B is one a
// person can read. Assertions cannot answer that question.
//
// Gated on an environment variable because it writes to the live notebook
// directory:
//
//	COLLECTIF_REPLAY=~/.claude/projects/<proj>/<session>.jsonl \
//	  go test ./src -run TestDev_ReplayTranscript -v
func TestDev_ReplayTranscriptIntoANotebook(t *testing.T) {
	path := os.Getenv("COLLECTIF_REPLAY")
	if path == "" {
		t.Skip("set COLLECTIF_REPLAY=<transcript.jsonl> to replay one into a notebook")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	root, _ := os.Getwd()
	// COLLECTIF_REPLAY_CLI lets the harness pretend the transcript came
	// from another CLI, which is how the degraded surfaces get looked at
	// without installing codex.
	cli := os.Getenv("COLLECTIF_REPLAY_CLI")
	if cli == "" {
		cli = "claude"
	}
	caps := Capabilities{TranscriptContent: true}
	if a := adapters[cli]; a != nil {
		caps = a.Capabilities()
	}
	st, err := openSessionNotebook("replay-"+os.Getenv("COLLECTIF_REPLAY_ID"), cli, root, caps)
	if err != nil {
		t.Fatalf("open notebook: %v", err)
	}
	p := newSessionProjector(st)
	defer p.Close()

	a := &claudeAdapter{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var lines int
	for sc.Scan() {
		lines++
		parts, err := a.ProjectTranscriptLine(sc.Bytes())
		if err != nil {
			t.Fatalf("line %d: %v", lines, err)
		}
		if len(parts) > 0 {
			p.Ingest(parts)
		}
	}
	p.Close()

	doc := st.Doc()
	var outs int
	for _, c := range doc.Cells {
		outs += len(c.Outputs)
	}
	t.Logf("replayed %d lines → notebook %q: %d cells, %d outputs", lines, st.slug, len(doc.Cells), outs)
	t.Logf("open: /notebook.html?token=<token>#nb=%s", st.slug)
}
