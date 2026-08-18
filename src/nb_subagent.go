package main

// nb_subagent.go — following the children a session delegates to.
// #55a, per ADR 0002's roadmap correction.
//
// A parent turn that delegates shows an Agent call and its result with
// nothing in between. What is missing is usually the majority of what the
// agent did — a subagent runs for minutes and makes dozens of tool calls,
// and the parent's transcript records none of it.
//
// The child's conversation is in its own file, and the parent's tool
// result names it. That link is exact (`toolUseResult.agentId`, verified
// against all 478 delegations on this machine, none missing), so nothing
// here guesses which turn a child belongs to.
//
// This is a directory follower rather than a file watcher, because the
// files do not exist when the delegation starts and there may be several.
// The shape is deliberately the same as the transcript watcher's — poll,
// remember an offset, read forward — because that one has survived
// partial writes, restarts and a growing file, and a second design would
// have to learn the same lessons again.

import (
	"bufio"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// subagentPollEvery matches the transcript watcher's cadence. A subagent's
// output is not more urgent than its parent's.
var subagentPollEvery = 250 * time.Millisecond

// subagentLineCap bounds one file's read so a runaway child cannot hold
// the poll loop. The offset advances regardless, so the rest arrives on
// the next tick rather than being lost.
const subagentLineCap = 2000

// watchSubagents follows every child transcript belonging to a session and
// feeds them to the projector. Returns a stop function, safe to call more
// than once — teardown paths are not perfectly sequenced.
//
// Returns a no-op stopper when the adapter cannot locate subagent files at
// all, which is every adapter but Claude Code today.
func watchSubagents(p *sessionProjector, adapter CLIAdapter, parentTranscript string) func() {
	sa, ok := adapter.(subagentLocator)
	if !ok || p == nil || parentTranscript == "" {
		return func() {}
	}
	// One probe with a known-good id tells us whether this adapter can
	// build paths at all, without needing a real child.
	if _, ok := sa.SubagentTranscriptPath(parentTranscript, "probe"); !ok {
		return func() {}
	}
	dir := subagentDirFor(sa, parentTranscript)
	if dir == "" {
		return func() {}
	}

	w := &subagentWatcher{p: p, dir: dir, offsets: map[string]int64{}, done: make(chan struct{})}
	go w.run()
	return w.stop
}

// subagentLocator is the optional half of CLIAdapter that knows where a
// CLI keeps its children. Optional rather than part of the interface: only
// Claude Code has the convention, and forcing every adapter to answer a
// question it cannot would produce three stubs and no information.
type subagentLocator interface {
	SubagentTranscriptPath(parentTranscript, agentID string) (string, bool)
}

// subagentDirFor recovers the directory from a sample path, so the
// convention stays owned by the adapter rather than duplicated here.
func subagentDirFor(sa subagentLocator, parentTranscript string) string {
	sample, ok := sa.SubagentTranscriptPath(parentTranscript, "probe")
	if !ok {
		return ""
	}
	return filepath.Dir(sample)
}

type subagentWatcher struct {
	p   *sessionProjector
	dir string

	mu      sync.Mutex
	offsets map[string]int64

	once sync.Once
	done chan struct{}
}

func (w *subagentWatcher) stop() { w.once.Do(func() { close(w.done) }) }

func (w *subagentWatcher) run() {
	t := time.NewTicker(subagentPollEvery)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
		}
		w.sweep()
	}
}

// sweep reads whatever is new in every child transcript. A missing
// directory is the common case, not an error: most sessions never
// delegate.
func (w *subagentWatcher) sweep() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := agentIDFromFilename(e.Name())
		if !ok {
			continue
		}
		w.readForward(filepath.Join(w.dir, e.Name()), id)
	}
}

// agentIDFromFilename recognises `agent-<id>.jsonl` and nothing else.
// Claude Code writes several sidecars beside each child — `.meta.json`,
// `.forked-skill.json`, `.forked-skill.marker.json` — and none of them is
// a transcript. Matching loosely would project a config file as
// conversation.
func agentIDFromFilename(name string) (string, bool) {
	if !strings.HasPrefix(name, "agent-") || !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
	if !agentIDRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// readForward reads from this file's remembered offset to the end. A
// partial trailing line is left for the next tick by not advancing past
// it, which is how the parent watcher handles a file being written under
// it.
func (w *subagentWatcher) readForward(path, agentID string) {
	w.mu.Lock()
	offset := w.offsets[path]
	w.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return
	}

	var parts []TranscriptPart
	var read int64
	br := bufio.NewReader(f)
	for i := 0; i < subagentLineCap; i++ {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && line[len(line)-1] != '\n' {
			break // torn write; leave it for the next tick
		}
		read += int64(len(line))
		if len(strings.TrimSpace(string(line))) > 0 {
			got, perr := claudeProjector.ProjectTranscriptLine(line)
			if perr == nil {
				parts = append(parts, got...)
			}
		}
		if err != nil {
			break
		}
	}
	if read == 0 {
		return
	}

	w.mu.Lock()
	w.offsets[path] = offset + read
	w.mu.Unlock()

	if len(parts) > 0 {
		w.p.IngestSubagent(agentID, parts)
	}
}

// claudeProjector parses child transcripts. Only Claude Code has the
// subagent convention today, and the watcher only starts for an adapter
// that can locate the files, so this is not a hidden assumption — it is
// the same adapter that answered the location question.
var claudeProjector = &claudeAdapter{}

// startSubagentWatch attaches a follower to a live session. Called once
// the transcript watcher has a path, since the children live beside it.
func startSubagentWatch(s *Session, p *sessionProjector, transcriptPath string) func() {
	adapter := s.adapter()
	if adapter == nil {
		return func() {}
	}
	stop := watchSubagents(p, adapter, transcriptPath)
	if _, ok := adapter.(subagentLocator); ok {
		log.Printf("[%s] subagent watch: %s", s.ID, subagentDirFor(adapter.(subagentLocator), transcriptPath))
	}
	return stop
}
