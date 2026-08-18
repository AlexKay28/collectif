package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// #52 M3. The bash tool.
//
// It runs under the same machinery as a shell cell rather than a second
// implementation of it — same interpreter, same own-process-group start,
// same streaming writer. The reason is not tidiness: the process-group kill
// in nb_run.go exists because a command that backgrounds work leaves it
// running after an interrupt, and a second copy of that runner would
// eventually be one copy that remembered and one that did not.

func TestBashTool_ReturnsStdoutAndStderr(t *testing.T) {
	root := t.TempDir()
	out, isErr := runTool(t, &bashTool{}, root, map[string]any{
		"command": "echo to-stdout; echo to-stderr 1>&2",
	})
	if isErr {
		t.Fatalf("bash failed: %s", out)
	}
	if !strings.Contains(out, "to-stdout") || !strings.Contains(out, "to-stderr") {
		t.Errorf("output = %q, want both streams", out)
	}
}

func TestBashTool_RunsInTheNotebookRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := runTool(t, &bashTool{}, root, map[string]any{"command": "ls"})
	if isErr {
		t.Fatalf("bash failed: %s", out)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Errorf("output = %q, want the working directory to be the notebook root", out)
	}
}

// A non-zero exit is a result the model has to see and react to, not a
// failed run — the same rule the read tools follow.
func TestBashTool_NonZeroExitIsAToolErrorNotAHardError(t *testing.T) {
	out, isErr := runTool(t, &bashTool{}, t.TempDir(), map[string]any{"command": "exit 3"})
	if !isErr {
		t.Error("a non-zero exit should be reported as a tool error")
	}
	if !strings.Contains(out, "3") {
		t.Errorf("output = %q, want the exit status named", out)
	}
}

func TestBashTool_RefusesAnEmptyCommand(t *testing.T) {
	if _, isErr := runTool(t, &bashTool{}, t.TempDir(), map[string]any{}); !isErr {
		t.Error("bash with no command should be a tool error")
	}
}

// The timeout is what keeps one tool call from holding a notebook run open
// forever. It has to kill the process, not just stop waiting for it.
func TestBashTool_TimesOutAndSaysSo(t *testing.T) {
	start := time.Now()
	out, isErr := runTool(t, &bashTool{}, t.TempDir(), map[string]any{
		"command": "sleep 30", "timeout": float64(1),
	})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("bash took %s to time out", elapsed)
	}
	if !isErr {
		t.Error("a timed-out command should report an error")
	}
	if !strings.Contains(strings.ToLower(out), "timed out") {
		t.Errorf("output = %q, want the timeout named — otherwise it reads as a command that produced nothing", out)
	}
}

// The whole process group is killed, not just the shell. A command that
// backgrounds work would otherwise leave it running with nothing left to
// reap it — the failure nb_run.go's Setpgid comment records.
func TestBashTool_KillsTheWholeProcessGroupOnTimeout(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "survivor.txt")
	// The child outlives the shell's own exit unless the group is killed;
	// if it does, it writes the marker.
	cmd := "(sleep 3; echo alive > " + marker + ") & wait"

	if _, isErr := runTool(t, &bashTool{}, root, map[string]any{"command": cmd, "timeout": float64(1)}); !isErr {
		t.Fatal("the command should have timed out")
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a backgrounded child survived the timeout — the process group was not killed")
	}
}

// Cancelling the run's context stops the command. This is what an interrupt
// on the cell reaches through to.
func TestBashTool_ContextCancellationStopsTheCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&bashTool{}).Run(ctx, map[string]any{"command": "sleep 30"}, t.TempDir()) //nolint:errcheck
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelling the context did not stop the command")
	}
}

// Output has to be watchable while it happens. A ten-minute build that
// shows nothing until it finishes is the terminal experience the notebook
// exists to replace.
func TestBashTool_StreamsOutputWhileItRuns(t *testing.T) {
	sink := &recordingSink{}
	tool := &bashTool{}

	done := make(chan struct{})
	go func() {
		defer close(done)
		tool.RunStream(context.Background(), //nolint:errcheck
			map[string]any{"command": "echo first; sleep 1; echo second"}, t.TempDir(), sink)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.text(), "first") && !strings.Contains(sink.text(), "second") {
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	<-done
	t.Errorf("nothing was streamed before the command finished; sink saw %q", sink.text())
}

// A command's output is not bounded by anything the model said, so the
// buffer holding it has to be. Both ends are kept: the head says what the
// command started doing and the tail is where the error is.
func TestTeeBuffer_BoundsWhatItHoldsAndKeepsBothEnds(t *testing.T) {
	sink := &recordingSink{}
	buf := &teeBuffer{sink: sink}

	buf.Write([]byte("FIRST-MARKER\n")) //nolint:errcheck
	for i := 0; i < 40; i++ {
		buf.Write([]byte(strings.Repeat("x", 32*1024))) //nolint:errcheck
	}
	buf.Write([]byte("\nLAST-MARKER\n")) //nolint:errcheck

	got := buf.String()
	if len(got) > bashCaptureLimit+200 {
		t.Errorf("buffer held %d bytes, want it bounded near %d", len(got), bashCaptureLimit)
	}
	if !strings.Contains(got, "FIRST-MARKER") {
		t.Error("the head was dropped — nothing says what the command started doing")
	}
	if !strings.Contains(got, "LAST-MARKER") {
		t.Error("the tail was dropped — that is where the error would have been")
	}
	if !strings.Contains(got, "dropped") {
		t.Error("truncation was silent; the model would reason about an excerpt it believes is whole")
	}
	// The live view is never truncated here — the store's own cap decides
	// that, and this buffer must not quietly starve it.
	if !strings.Contains(sink.text(), "LAST-MARKER") {
		t.Error("the sink did not receive everything the command wrote")
	}
}

type recordingSink struct {
	mu sync.Mutex
	b  strings.Builder
}

func (r *recordingSink) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.Write(p)
}

func (r *recordingSink) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.b.String()
}
