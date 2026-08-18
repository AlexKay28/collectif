package main

// nb_doc.go — the notebook document model and the fold that produces it.
// #49 (M1), per docs/adr/0001-notebook-harness.md §4.1 and §4.3.
//
// A notebook is not a struct we mutate and save. It is whatever folding its
// event log produces: the log is the source of truth, the document is a
// derived value, and the snapshot on disk is only a cache of that value.
//
// Two rules in here are compatibility commitments from M1 onward, because
// these files are user documents:
//
//   - The envelope ({v, type, id, at, payload}) is stable. New event types
//     add payloads; they do not reshape the envelope.
//   - An unknown event type is skipped by the fold and left in the log. A
//     notebook written by a newer build degrades on an older one; it never
//     corrupts. A malformed payload on a *known* type is a different thing
//     and is reported as an error — that is data damage, not version skew.

import (
	"encoding/json"
	"fmt"
	"time"
)

// nbSchemaVersion is the envelope version stamped on every event we write.
const nbSchemaVersion = 1

// ─── Cells ──────────────────────────────────────────────────────────────

type CellType string

const (
	CellMarkdown CellType = "markdown" // prose; never executed
	CellPrompt   CellType = "prompt"   // an instruction to the agent (M2)
	CellShell    CellType = "shell"    // a command; no model involved
	CellFile     CellType = "file"     // a pinned file, re-read on projection (M2)
)

type CellState string

const (
	CellIdle    CellState = "idle"
	CellQueued  CellState = "queued"
	CellRunning CellState = "running"
	CellOK      CellState = "ok"
	CellError   CellState = "error"
	// CellInterrupted is a run the user stopped. Distinct from error: the
	// cell didn't fail, it was cut short, and whatever it produced before
	// the kill is kept.
	CellInterrupted CellState = "interrupted"
	// CellStale marks a cell whose result no longer reflects the cells
	// above it. Advisory only — we never re-run it automatically, because
	// unlike a spreadsheet an agent turn costs money and touches disks.
	CellStale CellState = "stale"
)

type OutputType string

const (
	OutputText       OutputType = "text"
	OutputThinking   OutputType = "thinking"
	OutputToolCall   OutputType = "tool_call"
	OutputToolResult OutputType = "tool_result"
	OutputDiff       OutputType = "diff"
	OutputImage      OutputType = "image"
	OutputError      OutputType = "error"
	OutputSubagent   OutputType = "subagent"
	OutputApproval   OutputType = "approval"
	// OutputInjection is context the harness put into the model's window
	// that nobody typed (#47). Recorded as fact, label and size rather than
	// body — the CLI's own transcript is the archive.
	OutputInjection OutputType = "injection"
)

// Output is one rendered block produced by a run. Only text and error are
// emitted in M1; the rest are declared so the renderer and the schema agree
// on names before the phases that produce them land.
type Output struct {
	Type OutputType     `json:"type"`
	Text string         `json:"text,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

type CellMeta struct {
	Model     string   `json:"model,omitempty"`
	Effort    string   `json:"effort,omitempty"`
	Collapsed bool     `json:"collapsed,omitempty"`
	Tags      []string `json:"tags,omitempty"`

	// Provenance records who authored the cell (ADR 0002 D9). Empty means
	// you typed it and every verb applies. "mirrored" means a CLI session
	// produced it, and the cell is read-only — the context behind it lives
	// inside a process we do not own, so it can be re-asked but not edited
	// and re-run.
	Provenance string `json:"provenance,omitempty"`

	// SourceUUID is the CLI's own id for the transcript line this cell came
	// from. It is the idempotency key: the projector re-reads a growing
	// file across restarts, and this is how it recognises its own work.
	SourceUUID string `json:"sourceUuid,omitempty"`

	// ParentUUID is that line's parent. A transcript is a tree, and two
	// prompts sharing a parent means the first was abandoned — a fact the
	// projector needs back after a restart.
	ParentUUID string `json:"parentUuid,omitempty"`
}

// Cell separates Source (authored, the user's) from Outputs (produced,
// the machine's) — the distinction Jupyter got right and chat transcripts
// lose.
type Cell struct {
	ID       string        `json:"id"`
	Type     CellType      `json:"type"`
	Source   string        `json:"source"`
	Meta     CellMeta      `json:"meta,omitempty"`
	Outputs  []Output      `json:"outputs,omitempty"`
	State    CellState     `json:"state"`
	RunID    string        `json:"runId,omitempty"`
	Started  time.Time     `json:"started,omitempty"`
	Duration time.Duration `json:"duration,omitempty"`
	// Usage is what this cell's last run actually cost, reported by the
	// provider rather than inferred from a transcript (#50). Zero for cell
	// types that don't call a model.
	Usage Usage `json:"usage,omitempty"`

	// CacheMode is what the transport behind *this* cell's model can say
	// about prompt caching (#53). Derived on every read, never folded: a
	// cell whose model resolves to a transport with no cached-token
	// counter must render "not reported" rather than "0% cached", which
	// reads as a miss and then as a bug.
	CacheMode CacheMode `json:"cacheMode,omitempty"`
}

type NotebookMeta struct {
	Model     string  `json:"model,omitempty"`
	Effort    string  `json:"effort,omitempty"`
	BudgetUSD float64 `json:"budgetUsd,omitempty"`

	// CLI is the adapter that spawned the mirrored session. Recorded so a
	// notebook whose session has long ended can still say what it was able
	// to record — the *capabilities* are looked up live, but which CLI it
	// was is a fact about this document.
	CLI string `json:"cli,omitempty"`

	// SessionID names the CLI session this notebook mirrors (ADR 0002).
	// Empty means detached: the notebook is its own document and prompt
	// cells run on collectif's provider (D10). Non-empty means the agent
	// is the CLI, and a prompt cell is sent to it rather than executed
	// here — the one field that decides which of the two backends runs.
	SessionID string `json:"sessionId,omitempty"`
}

type Notebook struct {
	ID    string       `json:"id"`
	Title string       `json:"title"`
	Root  string       `json:"root"` // working directory; execution is contained here
	Meta  NotebookMeta `json:"meta"`
	Cells []Cell       `json:"cells"`
	// Fidelity is what this notebook can actually show, derived on every
	// read from the adapter registry (#47 P2). Never folded, never
	// written: what a build can do is a property of the code, not of the
	// document, and a claim frozen into a log goes stale silently.
	Fidelity *NotebookFidelity `json:"fidelity,omitempty"`

	// Provider is the transport this notebook's prompt cells run on,
	// derived on every read for the same reason Fidelity is (#53). Nil on
	// a mirrored session: there the CLI is the agent and already chose.
	Provider *NotebookProvider `json:"provider,omitempty"`

	// Version is the number of events folded in, including ones this build
	// did not understand. It is the log's position, not a schema version.
	Version int `json:"version"`
}

// ─── Events ─────────────────────────────────────────────────────────────

const (
	evNotebookCreated  = "notebook_created"
	evMetaSet          = "meta_set"
	evCellInserted     = "cell_inserted"
	evCellEdited       = "cell_edited"
	evCellMoved        = "cell_moved"
	evCellDeleted      = "cell_deleted"
	evRunStarted       = "run_started"
	evOutputAppended   = "output_appended"
	evRunFinished      = "run_finished"
	evCellsInvalidated = "cells_invalidated"
)

// Event is one entry in the log. Payload stays raw so an unknown type can
// be carried through a read without this build having to understand it.
type Event struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type notebookCreatedPayload struct {
	Title string `json:"title"`
	Root  string `json:"root"`
}

// metaSetPayload carries notebook-level settings. Pointers so "not
// supplied" stays distinct from "set to empty" — renaming a notebook and
// changing its default model are independent edits.
type metaSetPayload struct {
	Title *string       `json:"title,omitempty"`
	Meta  *NotebookMeta `json:"meta,omitempty"`
}

type cellInsertedPayload struct {
	Cell Cell `json:"cell"`
	// AfterCellID places the new cell directly after that cell. Empty
	// appends to the end; an id we cannot find also appends, so a racing
	// delete degrades to "at the end" rather than losing the cell.
	AfterCellID string `json:"afterCellId,omitempty"`
	// BeforeCellID places the new cell directly in front of that cell, and
	// takes precedence over AfterCellID. It exists because "after" alone
	// cannot express the top of the notebook: empty means append, so
	// inserting above the first cell used to drop the new cell at the
	// bottom instead.
	BeforeCellID string `json:"beforeCellId,omitempty"`
}

// cellEditedPayload uses pointers so "not supplied" and "set to empty" stay
// distinguishable — clearing a cell's source is a real edit.
type cellEditedPayload struct {
	CellID string    `json:"cellId"`
	Source *string   `json:"source,omitempty"`
	Meta   *CellMeta `json:"meta,omitempty"`
	// Type retypes the cell in place. Jupyter's y/m/r are command-mode
	// verbs, and people use them to fix a cell they started in the wrong
	// mode — deleting and re-adding would lose the cell's identity and
	// every output already recorded against it.
	Type *CellType `json:"type,omitempty"`
}

type cellMovedPayload struct {
	CellID string `json:"cellId"`
	// BeforeCellID is the cell to land in front of. Empty moves to the end.
	BeforeCellID string `json:"beforeCellId,omitempty"`
}

type cellDeletedPayload struct {
	CellID string `json:"cellId"`
}

type runStartedPayload struct {
	CellID string `json:"cellId"`
	RunID  string `json:"runId"`
}

type outputAppendedPayload struct {
	CellID string `json:"cellId"`
	RunID  string `json:"runId"`
	Output Output `json:"output"`
}

type runFinishedPayload struct {
	CellID string    `json:"cellId"`
	RunID  string    `json:"runId"`
	Status CellState `json:"status"`
	// Usage is carried on the terminal event so a cell's cost is part of
	// the log rather than a number only the live process knew (#50).
	Usage Usage `json:"usage,omitempty"`
}

type cellsInvalidatedPayload struct {
	CellIDs []string `json:"cellIds"`
}

// ─── Fold ───────────────────────────────────────────────────────────────

// foldEvents replays a log into a document.
func foldEvents(evs []Event) (*Notebook, error) {
	nb := &Notebook{}
	for _, e := range evs {
		if err := applyEvent(nb, e); err != nil {
			return nil, err
		}
	}
	return nb, nil
}

// applyEvent folds one event into nb. Unknown types advance the version and
// change nothing else; see the file header for why that is deliberate.
func applyEvent(nb *Notebook, e Event) error {
	// Version advances even on a type we skip, so it stays a faithful
	// count of the log — otherwise a snapshot taken by this build would
	// re-apply events a newer build had already folded.
	defer func() { nb.Version++ }()

	switch e.Type {
	case evNotebookCreated:
		var p notebookCreatedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		nb.Title = p.Title
		nb.Root = p.Root

	case evMetaSet:
		var p metaSetPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		if p.Title != nil {
			nb.Title = *p.Title
		}
		if p.Meta != nil {
			nb.Meta = *p.Meta
		}

	case evCellInserted:
		var p cellInsertedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		c := p.Cell
		if c.State == "" {
			c.State = CellIdle
		}
		at := len(nb.Cells)
		if p.BeforeCellID != "" {
			if i := indexOfCell(nb, p.BeforeCellID); i >= 0 {
				at = i
			}
		} else if p.AfterCellID != "" {
			if i := indexOfCell(nb, p.AfterCellID); i >= 0 {
				at = i + 1
			}
		}
		nb.Cells = append(nb.Cells, Cell{})
		copy(nb.Cells[at+1:], nb.Cells[at:])
		nb.Cells[at] = c

	case evCellEdited:
		var p cellEditedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		i := indexOfCell(nb, p.CellID)
		if i < 0 {
			return nil // edit of a deleted cell — nothing to do
		}
		if p.Source != nil {
			nb.Cells[i].Source = *p.Source
		}
		if p.Type != nil && validCellType(*p.Type) {
			nb.Cells[i].Type = *p.Type
		}
		if p.Meta != nil {
			nb.Cells[i].Meta = *p.Meta
		}
		// State is deliberately untouched. Whether an edit invalidates a
		// result is the runner's call, and it says so with cells_invalidated.

	case evCellMoved:
		var p cellMovedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		from := indexOfCell(nb, p.CellID)
		if from < 0 {
			return nil
		}
		c := nb.Cells[from]
		nb.Cells = append(nb.Cells[:from], nb.Cells[from+1:]...)
		to := len(nb.Cells)
		if p.BeforeCellID != "" {
			if i := indexOfCell(nb, p.BeforeCellID); i >= 0 {
				to = i
			}
		}
		nb.Cells = append(nb.Cells, Cell{})
		copy(nb.Cells[to+1:], nb.Cells[to:])
		nb.Cells[to] = c

	case evCellDeleted:
		var p cellDeletedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		if i := indexOfCell(nb, p.CellID); i >= 0 {
			nb.Cells = append(nb.Cells[:i], nb.Cells[i+1:]...)
		}

	case evRunStarted:
		var p runStartedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		i := indexOfCell(nb, p.CellID)
		if i < 0 {
			return nil
		}
		// A new run replaces the previous one's outputs. Appending would
		// stack every re-run of a cell on top of the last.
		nb.Cells[i].Outputs = nil
		nb.Cells[i].RunID = p.RunID
		nb.Cells[i].State = CellRunning
		nb.Cells[i].Started = e.At
		nb.Cells[i].Duration = 0
		nb.Cells[i].Usage = Usage{}

	case evOutputAppended:
		var p outputAppendedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		i := indexOfCell(nb, p.CellID)
		if i < 0 {
			return nil
		}
		// Ignore output from a superseded run — a slow write racing a
		// re-run must not land in the new run's output.
		if p.RunID != "" && nb.Cells[i].RunID != "" && p.RunID != nb.Cells[i].RunID {
			return nil
		}
		nb.Cells[i].Outputs = append(nb.Cells[i].Outputs, p.Output)

	case evRunFinished:
		var p runFinishedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		i := indexOfCell(nb, p.CellID)
		if i < 0 {
			return nil
		}
		if p.RunID != "" && nb.Cells[i].RunID != "" && p.RunID != nb.Cells[i].RunID {
			return nil
		}
		st := p.Status
		if st == "" {
			st = CellOK
		}
		nb.Cells[i].State = st
		nb.Cells[i].Usage = p.Usage
		// Duration comes from the envelope timestamps rather than being
		// reported by the writer, so it stays consistent with the log.
		if !nb.Cells[i].Started.IsZero() && !e.At.IsZero() {
			if d := e.At.Sub(nb.Cells[i].Started); d > 0 {
				nb.Cells[i].Duration = d
			}
		}

	case evCellsInvalidated:
		var p cellsInvalidatedPayload
		if err := decodePayload(e, &p); err != nil {
			return err
		}
		for _, id := range p.CellIDs {
			i := indexOfCell(nb, id)
			if i < 0 {
				continue
			}
			// Only a finished result can go stale. A cell that never ran
			// has nothing to invalidate, and clobbering a running cell
			// would lie about work still in flight.
			if nb.Cells[i].State == CellOK || nb.Cells[i].State == CellError {
				nb.Cells[i].State = CellStale
			}
		}

	default:
		// Version skew: a newer build wrote something we don't model. Skip
		// it and keep folding — the line stays in the log untouched.
	}
	return nil
}

func decodePayload(e Event, dst any) error {
	if len(e.Payload) == 0 {
		return fmt.Errorf("notebook event %q (%s): empty payload", e.ID, e.Type)
	}
	if err := json.Unmarshal(e.Payload, dst); err != nil {
		return fmt.Errorf("notebook event %q (%s): %w", e.ID, e.Type, err)
	}
	return nil
}

func indexOfCell(nb *Notebook, id string) int {
	for i := range nb.Cells {
		if nb.Cells[i].ID == id {
			return i
		}
	}
	return -1
}

// clone returns a deep-enough copy for handing to a caller that may read it
// while the store keeps folding: the cell and output slices are copied so a
// later append cannot mutate what the caller is holding.
func (nb *Notebook) clone() *Notebook {
	if nb == nil {
		return nil
	}
	out := *nb
	out.Cells = make([]Cell, len(nb.Cells))
	copy(out.Cells, nb.Cells)
	for i := range out.Cells {
		if n := len(nb.Cells[i].Outputs); n > 0 {
			out.Cells[i].Outputs = make([]Output, n)
			copy(out.Cells[i].Outputs, nb.Cells[i].Outputs)
		}
	}
	return &out
}
