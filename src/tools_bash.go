package main

// tools_bash.go — the `bash` tool. #52 (M3), ADR 0001 §4.5.
//
// There is deliberately almost nothing here. The execution is nb_run.go's
// execShell — the same interpreter, the same own-process-group start, the
// same kill that reaches a backgrounded child — because M1 already got that
// right for shell cells and a second implementation would drift from it.
// What this file adds is the tool's contract with the model: a schema, a
// timeout, a bounded result, and the streaming seam that lets the output be
// watched while it happens.
//
// Note what is *not* here: containment. A command is not a path, and there
// is no honest way to prove that `make -C ../..` stays under the root
// short of a sandbox we do not have. So bash is contained by its working
// directory only, and its real gate is the permission engine — which is why
// its shipped default is ask rather than allow.

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// bashDefaultTimeout is what a call gets when it does not ask. Long enough
// for a build, short enough that a hung command does not hold a notebook
// run open until someone notices.
const bashDefaultTimeout = 2 * time.Minute

// bashMaxTimeout caps what a call may ask for. The model is not the right
// party to decide that ten minutes of waiting is acceptable.
const bashMaxTimeout = 10 * time.Minute

// streamingTool is a Tool whose output is worth watching arrive rather than
// reading at the end. dispatchTool hands one the cell's live writer, so a
// long command fills the cell as it runs instead of after — which is the
// difference the notebook exists to make over a terminal that has already
// scrolled.
type streamingTool interface {
	RunStream(ctx context.Context, in map[string]any, root string, sink io.Writer) (string, bool, error)
}

type bashTool struct{}

func (t *bashTool) Spec() ToolSpec {
	return ToolSpec{
		Name: "bash",
		Description: "Run a shell command in the notebook's working directory. " +
			"Use this for things the other tools cannot do — building, running tests, git, package managers. " +
			"Prefer `read`, `glob` and `grep` for looking at files: they are contained and this is not. " +
			"Output is combined stdout and stderr; a non-zero exit is reported with its status.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command line to run, as /bin/sh would read it.",
				},
				"timeout": map[string]any{
					"type":        "integer",
					"description": "Seconds to wait before killing it. Defaults to 120, capped at 600.",
				},
			},
			"required":             []string{"command"},
			"additionalProperties": false,
		},
	}
}

func (t *bashTool) Run(ctx context.Context, in map[string]any, root string) (string, bool, error) {
	return t.RunStream(ctx, in, root, nil)
}

func (t *bashTool) RunStream(ctx context.Context, in map[string]any, root string, sink io.Writer) (string, bool, error) {
	command := argString(in, "command")
	if strings.TrimSpace(command) == "" {
		return "bash: a command is required.", true, nil
	}

	timeout := bashDefaultTimeout
	if secs := argInt(in, "timeout"); secs > 0 {
		timeout = time.Duration(secs) * time.Second
		if timeout > bashMaxTimeout {
			timeout = bashMaxTimeout
		}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The buffer is the tool's own: the sink is the cell's live stream and
	// already carries the model's text from this turn, so reading the
	// result back off it would hand the model its own prose as command
	// output.
	buf := &teeBuffer{sink: sink}
	status := execShell(runCtx, buf, command, root, nil)

	// Whose deadline fired matters. The run's own context being cancelled
	// is an interrupt, and saying "timed out" about it would send the model
	// looking for a slow command that was never slow.
	switch {
	case ctx.Err() != nil:
		return elide(buf.String(), toolOutputBudget) +
			"\ncollectif: the run was interrupted before this command finished.", true, nil
	case runCtx.Err() != nil:
		return elide(buf.String(), toolOutputBudget) +
			fmt.Sprintf("\ncollectif: timed out after %s and was killed.", timeout), true, nil
	}

	out := elide(buf.String(), toolOutputBudget)
	if status != CellOK {
		return out, true, nil
	}
	if strings.TrimSpace(out) == "" {
		// An empty result reads as a tool that did nothing. It is the
		// normal outcome of a successful command, and saying so costs one
		// line and saves a retry.
		return "(no output; the command succeeded)", false, nil
	}
	return out, false, nil
}

// bashCaptureLimit bounds what one command may hold in memory. The result
// is elided to toolOutputBudget on the way to the model anyway, so this is
// not about the context window — it is about `cat /dev/urandom` costing a
// truncated tool result rather than the process. The store's own live
// buffer is capped the same way for the same reason (maxCellOutput).
const bashCaptureLimit = 256 * 1024

// teeBuffer accumulates what the tool returns to the model while passing
// the same bytes to the live view.
//
// Writes arrive from two goroutines — execShell points both stdout and
// stderr at it so interleaving is preserved the way a terminal would show
// it — so it holds its own lock rather than relying on the sink's.
//
// Past the cap it keeps the head and a rolling tail rather than truncating
// forwards. Both ends matter and for different reasons: the head says what
// the command started doing, and the tail is where the error is.
type teeBuffer struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped int
	sink    io.Writer
}

func (t *teeBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.absorb(p)
	t.mu.Unlock()
	if t.sink != nil {
		t.sink.Write(p) //nolint:errcheck // a lost delta is a live-view glitch, not a failed command
	}
	return len(p), nil
}

func (t *teeBuffer) absorb(p []byte) {
	half := bashCaptureLimit / 2
	if room := half - len(t.head); room > 0 {
		if room > len(p) {
			room = len(p)
		}
		t.head = append(t.head, p[:room]...)
		p = p[room:]
	}
	if len(p) == 0 {
		return
	}
	t.tail = append(t.tail, p...)
	if over := len(t.tail) - half; over > 0 {
		t.tail = append(t.tail[:0], t.tail[over:]...)
		t.dropped += over
	}
}

func (t *teeBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropped == 0 {
		return string(t.head) + string(t.tail)
	}
	// Said out loud, for the same reason elide says it: silence would let
	// the model reason confidently about an excerpt it believes is whole.
	return fmt.Sprintf("%s\n… %d bytes of output dropped …\n%s", t.head, t.dropped, t.tail)
}
