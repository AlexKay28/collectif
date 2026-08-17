package main

// nb_run.go — cell execution. #49 (M1 slice 3), ADR 0001 §4.4.
//
// M1 runs shell cells only. Prompt and file cells are declared and refused
// with 501 until M2 lands the agent loop, which is more honest than
// pretending they are inert.
//
// The shape here is the one the agent loop will reuse: a run is started by
// an HTTP call that returns immediately, streams transient deltas to
// watchers, and finalises as exactly one persisted output plus a
// run_finished. Nothing about a run is recoverable from the deltas — they
// are a live view, and the log is the record.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

// maxCellOutput bounds what one run can accumulate in memory and in the
// log. A runaway command should cost a truncated cell, not the process.
const maxCellOutput = 256 * 1024

// nbShell is the interpreter shell cells run under. /bin/sh keeps the
// dependency honest; a cell that needs bash can say so itself.
var nbShell = []string{"/bin/sh", "-c"}

// nbRun is an in-flight execution. It lives only while the command runs;
// the log holds everything durable about it.
type nbRun struct {
	runID  string
	cancel context.CancelFunc

	mu          sync.Mutex
	interrupted bool
}

func (r *nbRun) markInterrupted() {
	r.mu.Lock()
	r.interrupted = true
	r.mu.Unlock()
	r.cancel()
}

func (r *nbRun) wasInterrupted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupted
}

// ─── HTTP ───────────────────────────────────────────────────────────────

func handleCellRun(w http.ResponseWriter, r *http.Request, st *notebookStore, cellID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc := st.Doc()
	i := indexOfCell(doc, cellID)
	if i < 0 {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}
	cell := doc.Cells[i]

	switch cell.Type {
	case CellMarkdown:
		http.Error(w, "markdown cells are not executed", http.StatusBadRequest)
		return
	case CellFile:
		// A file cell is context, not an action — it contributes its
		// contents to every projection and has nothing of its own to run.
		http.Error(w, "file cells are read during projection, not run", http.StatusBadRequest)
		return
	case CellPrompt:
		// ADR 0002 D10: two backends, one verb. A notebook that mirrors a
		// CLI session sends its prompts to that CLI; a detached notebook
		// runs them on our own provider. The provider check applies only
		// to the second — a session notebook on a machine with no API key
		// is perfectly usable, because the agent is the CLI.
		if doc.Meta.SessionID == "" && activeProvider == nil {
			http.Error(w,
				"no model provider is configured — set one up before running prompt cells",
				http.StatusServiceUnavailable)
			return
		}
		if doc.Meta.SessionID != "" {
			// Checked before a run is begun so a dead session is a clean
			// 503 rather than a cell that starts and never finishes.
			if err := checkSessionDrivable(doc.Meta.SessionID); err != nil {
				code := http.StatusServiceUnavailable
				if errors.Is(err, errAgentWaiting) {
					code = http.StatusConflict
				}
				http.Error(w, err.Error(), code)
				return
			}
		}
	case CellShell:
		// fall through
	default:
		http.Error(w, "unknown cell type", http.StatusBadRequest)
		return
	}

	run, ctx, err := st.beginRun(cellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if _, err := st.Append(evRunStarted, runStartedPayload{CellID: cellID, RunID: run.runID}); err != nil {
		st.endRun(cellID, run.runID)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Everything below this cell was produced from a projection that
	// included its old output, so those results no longer reflect the
	// notebook (ADR §4.2). Marking them is advisory — we never re-run them,
	// because unlike a spreadsheet an agent turn costs money and touches
	// disks. The fold has understood this event since M1; nothing had ever
	// emitted one, so staleness silently never happened.
	st.invalidateBelow(cellID)

	// Execution is asynchronous: the call starts a run, it does not wait
	// for one. A cell that takes ten minutes must not hold an HTTP request
	// open for ten minutes.
	switch {
	case cell.Type == CellPrompt && doc.Meta.SessionID != "":
		go sendPromptCell(st, cellID, cell.Source, doc.Meta.SessionID, run)
	case cell.Type == CellPrompt:
		go runPromptCell(ctx, st, cellID, run, activeProvider)
	default:
		go runShellCell(ctx, st, cellID, cell.Source, doc.Root, run)
	}

	writeJSON(w, http.StatusOK, map[string]any{"cellId": cellID, "runId": run.runID})
}

func handleCellInterrupt(w http.ResponseWriter, r *http.Request, st *notebookStore, cellID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	doc := st.Doc()
	i := indexOfCell(doc, cellID)

	// A mirrored turn has no local run to cancel — the work is happening
	// inside the CLI, and the run entry here is only a placeholder that the
	// send path releases. So the session is checked *first*: cancelling the
	// placeholder would report success and stop nothing, which is worse
	// than a button that does not exist.
	//
	// Stopping the agent is Escape. The transcript then reports the
	// interruption and the projector settles the cell — we do not settle it
	// ourselves, because we do not know whether the agent obeyed.
	if i >= 0 && doc.Meta.SessionID != "" && doc.Cells[i].Type == CellPrompt {
		if doc.Cells[i].State != CellRunning {
			http.Error(w, "cell is not running", http.StatusConflict)
			return
		}
		if err := interruptSession(doc.Meta.SessionID); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cellId": cellID, "sessionInterrupted": true})
		return
	}

	st.runsMu.Lock()
	run, ok := st.runs[cellID]
	st.runsMu.Unlock()
	if !ok {
		http.Error(w, "cell is not running", http.StatusConflict)
		return
	}
	run.markInterrupted()
	writeJSON(w, http.StatusOK, map[string]any{"cellId": cellID, "runId": run.runID})
}

// ─── Run registry ───────────────────────────────────────────────────────

// beginRun claims a cell for execution. One run per cell: a second run
// while the first is live would race two writers onto one cell's outputs.
func (st *notebookStore) beginRun(cellID string) (*nbRun, context.Context, error) {
	st.runsMu.Lock()
	defer st.runsMu.Unlock()
	if st.runs == nil {
		st.runs = map[string]*nbRun{}
	}
	if _, ok := st.runs[cellID]; ok {
		return nil, nil, errors.New("cell is already running")
	}
	// The context is created with the claim, so an interrupt arriving
	// before the goroutine starts still reaches it.
	ctx, cancel := context.WithCancel(context.Background())
	run := &nbRun{runID: uuid.NewString(), cancel: cancel}
	st.runs[cellID] = run
	return run, ctx, nil
}

// endRun releases the claim, but only if it still belongs to this run — a
// late finisher must not unclaim a run that has already been replaced.
func (st *notebookStore) endRun(cellID, runID string) {
	st.runsMu.Lock()
	if cur, ok := st.runs[cellID]; ok && cur.runID == runID {
		delete(st.runs, cellID)
	}
	st.runsMu.Unlock()
}

// interruptAllRuns stops every in-flight run. Used on shutdown so a
// notebook doesn't leave orphaned process groups behind.
func (st *notebookStore) interruptAllRuns() {
	st.runsMu.Lock()
	runs := make([]*nbRun, 0, len(st.runs))
	for _, r := range st.runs {
		runs = append(runs, r)
	}
	st.runsMu.Unlock()
	for _, r := range runs {
		r.markInterrupted()
	}
}

// ─── Execution ──────────────────────────────────────────────────────────

func runShellCell(ctx context.Context, st *notebookStore, cellID, source, root string, run *nbRun) {
	defer st.endRun(cellID, run.runID)
	defer run.cancel() // release the context regardless of how we exit

	out := &deltaWriter{st: st, cellID: cellID, runID: run.runID}

	cmd := exec.Command(nbShell[0], append(nbShell[1:], source)...)
	cmd.Dir = root
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group, so a kill reaches the whole tree. A shell cell
	// that backgrounds work would otherwise leave it running after an
	// interrupt — the same reason main.go kills session process groups.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	status := CellOK
	if err := cmd.Start(); err != nil {
		out.write("collectif: " + err.Error() + "\n")
		st.finishRun(cellID, run.runID, out.text(), CellError)
		return
	}

	done := make(chan struct{})
	go func() {
		select {
		case <-done:
			// Finished normally. Check done first and return without
			// looking at ctx: runShellCell cancels the context on the way
			// out, so both cases are ready at once on the happy path and a
			// random pick would kill a pid Wait() has already reaped —
			// which, once pids recycle, is someone else's process group.
			return
		case <-ctx.Done():
			select {
			case <-done: // raced us to the exit; nothing to kill
				return
			default:
			}
			killProcessGroup(cmd)
		}
	}()

	waitErr := cmd.Wait()
	close(done)

	switch {
	case run.wasInterrupted():
		status = CellInterrupted
	case waitErr != nil:
		status = CellError
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			out.write(fmt.Sprintf("\ncollectif: exited with status %d\n", exitErr.ExitCode()))
		} else {
			out.write("\ncollectif: " + waitErr.Error() + "\n")
		}
	}
	st.finishRun(cellID, run.runID, out.text(), status)
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		return
	}
	_ = cmd.Process.Kill()
}

// finishRun writes the two events that make a run durable: one finalised
// output, then the terminal state. Deltas were live-only; this is the
// record.
// logNotebookErr keeps the log-append failure message identical wherever a
// run reports one.
func logNotebookErr(st *notebookStore, cellID, what string, err error) {
	log.Printf("notebook %s: %s for cell %s: %v", st.slug, what, cellID, err)
}

func (st *notebookStore) finishRun(cellID, runID, text string, status CellState) {
	if text != "" {
		if _, err := st.Append(evOutputAppended, outputAppendedPayload{
			CellID: cellID, RunID: runID,
			Output: Output{Type: OutputText, Text: text},
		}); err != nil {
			log.Printf("notebook %s: append output for cell %s: %v", st.slug, cellID, err)
		}
	}
	// Clear the live buffer between the two events, never before or after
	// both. A fold taken in this window has the finalised output and no
	// live copy, which renders correctly; clearing later would leave a
	// window where a client could hold two copies of the same output.
	st.clearLive(cellID, runID)

	if _, err := st.Append(evRunFinished, runFinishedPayload{
		CellID: cellID, RunID: runID, Status: status,
	}); err != nil {
		log.Printf("notebook %s: finish run for cell %s: %v", st.slug, cellID, err)
	}
}

// ─── Output plumbing ────────────────────────────────────────────────────

// deltaWriter fans process output to watchers as it arrives while the store
// accumulates the copy that becomes the finalised output. Stdout and stderr
// share one writer so interleaving is preserved the way a terminal would
// show it, which means Write is called concurrently — the store's own lock
// is what makes that safe.
//
// The accumulation lives on the store rather than here so a client that
// connects mid-run can be handed it in the opening fold. Deltas are not
// persisted, so without that a refreshed page would show a running cell
// with nothing in it.
type deltaWriter struct {
	st     *notebookStore
	cellID string
	runID  string
}

func (d *deltaWriter) Write(p []byte) (int, error) {
	d.write(string(p))
	return len(p), nil
}

func (d *deltaWriter) write(s string) {
	// Stop broadcasting when the store stops accepting: past the cap the
	// text is going nowhere, and pushing it anyway floods every subscriber.
	if d.st.appendLive(d.cellID, d.runID, s) {
		d.st.broadcastDelta(d.cellID, d.runID, s)
	}
}

func (d *deltaWriter) text() string {
	return d.st.liveText(d.cellID, d.runID)
}

// sendPromptCell hands a prompt to the CLI that owns this notebook's
// session (#47 P1). It is deliberately not a "run": nothing is executed
// here, and the cell stays running until the projector sees the turn come
// back through the transcript and settles it. That asymmetry is the honest
// one — we are not the agent, we are the surface.
func sendPromptCell(st *notebookStore, cellID, source, sessionID string, run *nbRun) {
	// Adoption only means anything on a CLI whose turns come back to us.
	// On one we cannot project, nothing is ever echoed, so waiting to be
	// adopted would mean every delivered prompt eventually reporting "this
	// may not have arrived" — D11's error pointed the other way (#47 P2).
	projects := st.Doc().Fidelity != nil && st.Doc().Fidelity.Turns
	if projects {
		// Registered before sending: the transcript can be read back
		// within milliseconds, and registering afterwards would lose the
		// race and duplicate the prompt.
		if p := sessionProjectorFor(sessionID); p != nil {
			p.AwaitAdoption(cellID, source)
		}
	}
	if err := sendToSession(sessionID, source); err != nil {
		st.Append(evOutputAppended, outputAppendedPayload{ //nolint:errcheck
			CellID: cellID, RunID: run.runID,
			Output: Output{Type: OutputError, Text: err.Error()},
		})
		st.finishRunWithUsage(cellID, run.runID, CellError, Usage{})
		return
	}
	if !projects {
		// Delivered, and that is all we will ever know. Settling it as ok
		// with a note is the honest end: the prompt reached the agent, the
		// answer is somewhere we cannot see, and an empty cell left running
		// would read as an agent ignoring you.
		cli := st.Doc().Meta.CLI
		if cli == "" {
			cli = "this CLI"
		}
		st.Append(evOutputAppended, outputAppendedPayload{ //nolint:errcheck
			CellID: cellID, RunID: run.runID,
			Output: Output{
				Type: OutputText,
				Text: "Sent. collectif cannot read " + cli + "'s transcript, so the reply will not " +
					"appear here — watch the terminal for it.",
			},
		})
		st.finishRunWithUsage(cellID, run.runID, CellOK, Usage{})
		return
	}

	// No terminal event here on purpose. The turn is the CLI's now, and it
	// ends when the transcript says it ends.
	st.endRun(cellID, run.runID)
}
