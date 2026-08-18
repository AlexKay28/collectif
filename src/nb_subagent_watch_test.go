package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #55a — following a child's transcript.
//
// The parent's watcher polls one file at a known path. A subagent's is
// harder in two ways: the file does not exist when the delegation starts,
// and there may be several. So this is a directory follower — poll for
// files matching the convention, remember an offset per file, and project
// what is new.

func subagentDir(t *testing.T, parentTranscript string) string {
	t.Helper()
	base := parentTranscript[:len(parentTranscript)-len(filepath.Ext(parentTranscript))]
	dir := filepath.Join(base, "subagents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return dir
}

func TestSubagentWatcher_FollowsAChildThatAppearsLater(t *testing.T) {
	withTempNotebooks(t)
	root := t.TempDir()
	st, err := createNotebook("Parent", root)
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	p := newSessionProjector(st)

	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := subagentDir(t, parent)

	// A turn that delegates, linked before the child's file exists — the
	// background case, which is 406 of the 478 delegations on this machine.
	p.Ingest([]TranscriptPart{part(PartUserText, "review it", "l1")})
	p.Ingest([]TranscriptPart{{
		Kind: PartToolResult, ToolUseID: "t1", UUID: "l2",
		AgentID: "kid1", AgentType: "code-reviewer",
	}})

	stop := watchSubagents(p, &claudeAdapter{}, parent)
	t.Cleanup(stop)

	// The child starts writing a minute later, as far as we are concerned.
	writeLines(t, filepath.Join(dir, "agent-kid1.jsonl"),
		`{"type":"assistant","uuid":"k1","isSidechain":true,"timestamp":"2026-08-18T10:00:00.000Z",
		  "message":{"role":"assistant","content":[{"type":"text","text":"found three things"}]}}`)

	waitForSubagentOutputs(t, st, "kid1", 1, 5*time.Second)

	// And keeps writing: the follower has to resume from its offset rather
	// than re-reading and duplicating.
	writeLines(t, filepath.Join(dir, "agent-kid1.jsonl"),
		`{"type":"assistant","uuid":"k2","isSidechain":true,"timestamp":"2026-08-18T10:00:05.000Z",
		  "message":{"role":"assistant","content":[{"type":"text","text":"and a fourth"}]}}`)

	outs := waitForSubagentOutputs(t, st, "kid1", 2, 5*time.Second)
	if outs[0].Text != "found three things" || outs[1].Text != "and a fourth" {
		t.Errorf("child turns out of order or duplicated: %+v", outs)
	}
}

func TestSubagentWatcher_FollowsSeveralChildrenAtOnce(t *testing.T) {
	withTempNotebooks(t)
	st, err := createNotebook("Fanout", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	p := newSessionProjector(st)

	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := subagentDir(t, parent)

	p.Ingest([]TranscriptPart{part(PartUserText, "fan out", "l1")})
	for i, id := range []string{"kidA", "kidB", "kidC"} {
		p.Ingest([]TranscriptPart{{
			Kind: PartToolResult, ToolUseID: "t", UUID: string(rune('a'+i)) + "-res", AgentID: id,
		}})
	}

	stop := watchSubagents(p, &claudeAdapter{}, parent)
	t.Cleanup(stop)

	for i, id := range []string{"kidA", "kidB", "kidC"} {
		writeLines(t, filepath.Join(dir, "agent-"+id+".jsonl"),
			`{"type":"assistant","uuid":"`+id+`-1","isSidechain":true,
			  "message":{"role":"assistant","content":[{"type":"text","text":"from `+id+`"}]}}`)
		_ = i
	}
	for _, id := range []string{"kidA", "kidB", "kidC"} {
		outs := waitForSubagentOutputs(t, st, id, 1, 5*time.Second)
		if outs[0].Text != "from "+id {
			t.Errorf("%s got %q", id, outs[0].Text)
		}
	}
}

// A file whose name is not the convention is not a subagent transcript,
// and reading arbitrary files out of that directory would be a surprise.
func TestSubagentWatcher_IgnoresFilesThatAreNotChildTranscripts(t *testing.T) {
	withTempNotebooks(t)
	st, err := createNotebook("Sidecars", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	p := newSessionProjector(st)

	parent := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(parent, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := subagentDir(t, parent)

	p.Ingest([]TranscriptPart{part(PartUserText, "go", "l1")})
	p.Ingest([]TranscriptPart{{Kind: PartToolResult, ToolUseID: "t1", UUID: "l2", AgentID: "kid9"}})

	// Claude Code writes these beside every child transcript.
	writeLines(t, filepath.Join(dir, "agent-kid9.meta.json"), `{"agentType":"general-purpose"}`)
	writeLines(t, filepath.Join(dir, "agent-kid9.forked-skill.json"), `{"skillName":"x"}`)
	writeLines(t, filepath.Join(dir, "notes.txt"), `not json at all`)

	stop := watchSubagents(p, &claudeAdapter{}, parent)
	t.Cleanup(stop)

	time.Sleep(700 * time.Millisecond)
	for _, o := range st.Doc().Cells[0].Outputs {
		if o.Data != nil && o.Data["agentId"] != nil {
			t.Errorf("a sidecar file was projected as a subagent turn: %+v", o)
		}
	}
}

// No subagents directory is the common case — most sessions never
// delegate. It must not be an error, and must not spin.
func TestSubagentWatcher_ToleratesNoSubagentsDirectory(t *testing.T) {
	withTempNotebooks(t)
	st, err := createNotebook("Lonely", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	p := newSessionProjector(st)
	parent := filepath.Join(t.TempDir(), "sess.jsonl")

	stop := watchSubagents(p, &claudeAdapter{}, parent)
	time.Sleep(300 * time.Millisecond)
	stop()
	stop() // teardown paths are not perfectly sequenced
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(collapseWS(l) + "\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func waitForSubagentOutputs(t *testing.T, st *notebookStore, agentID string, want int, timeout time.Duration) []Output {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		var got []Output
		for _, c := range st.Doc().Cells {
			for _, o := range c.Outputs {
				if o.Data != nil && o.Data["agentId"] == agentID {
					got = append(got, o)
				}
			}
		}
		if len(got) >= want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s produced %d outputs, want %d", agentID, len(got), want)
		}
		time.Sleep(40 * time.Millisecond)
	}
}
