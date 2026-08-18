package main

// nb_session.go — a running CLI session, rendered as a notebook.
// #47 P0 slice B, per ADR 0002.
//
// Slice A turned a transcript line into parts. This turns a stream of parts
// into a document, and the shape it chooses is the argument of the whole
// ADR: a prompt is a *cell*, and everything the agent did in response is
// that cell's *output*. A terminal cannot make that distinction — it has
// only one column, and your words and the machine's scroll past in it
// together. A notebook keeps authored source and produced output apart,
// which is what makes the result readable a week later.
//
// The projector is not the CLI's peer, it is its stenographer. It never
// decides anything: no cell exists that the transcript did not report, no
// state is set that the transcript did not justify.
//
// Idempotence is the requirement that shapes everything else here. This
// runs against a file another process is appending to, across restarts
// where we re-read from the top, so "append what I just saw" is wrong by
// default. Every part carries the CLI's own line id; a line we have
// already folded is skipped, and the set of seen ids is rebuilt from the
// document rather than held only in memory — because the memory is what
// the restart lost.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Provenance values for CellMeta. Empty means authored: you typed it, you
// own it, every verb applies. The rest are ADR 0002 D9 — cells the notebook
// is showing rather than cells it is running.
const (
	// ProvenanceMirrored marks a cell projected from a CLI session. It is
	// read-only: the context that produced it lives inside a process we do
	// not own, so it cannot be edited and re-run, only re-asked.
	ProvenanceMirrored = "mirrored"
	// ProvenanceCompact marks the summary a CLI wrote when it compacted its
	// own context — the only surviving record of everything above it.
	ProvenanceCompact = "compact"
)

// sessionProjector folds transcript parts into one notebook's log.
//
// It is single-writer by construction (one transcript watcher per session)
// but holds a mutex anyway: Close races with the watcher's final Ingest on
// every teardown path, and a duplicated terminal event is a document that
// says a turn finished twice.
type sessionProjector struct {
	st *notebookStore

	mu      sync.Mutex
	seen    map[string]bool // CLI line ids already folded
	current string          // the cell outputs are landing on
	// currentParent is the open cell's parent line. Two prompts sharing a
	// parent means the first was abandoned, and settling it "ok" would
	// claim a question was answered when nothing ran.
	currentParent string
	// pendingParent hands the parent link to openCell without widening its
	// signature for the one caller that has one.
	pendingParent string
	// adoptCell/adoptText are the cell awaiting its own reflection: a
	// prompt you authored here and sent to the CLI, which is about to come
	// back to us through the transcript.
	adoptCell string
	adoptText string
	// adoptGen invalidates a pending give-up timer when the cell it was
	// watching is adopted or replaced.
	adoptGen uint64
	// approvalKey/approvalID track the question the agent is currently
	// asking, so a hook that fires twice records it once and the answer
	// can be paired with it.
	approvalKey string
	approvalID  string
	// subagentCell maps a child agent to the turn that spawned it, and
	// heldSubagent parks a child's work until that link is known — the two
	// arrive in either order depending on whether the delegation was
	// synchronous (#55a).
	subagentCell map[string]subagentLink
	heldSubagent map[string][]TranscriptPart
	closed       bool
}

type subagentLink struct {
	cellID    string
	agentType string
}

func newSessionProjector(st *notebookStore) *sessionProjector {
	p := &sessionProjector{st: st, seen: map[string]bool{}}
	p.reseedFromDocument()
	return p
}

// reseedFromDocument rebuilds the seen-set and the open cell from the
// notebook itself. This is what makes a restart safe: the log already knows
// everything this projector was told before it died, so the truth is read
// back out of the log rather than trusted to survive in memory.
func (p *sessionProjector) reseedFromDocument() {
	doc := p.st.Doc()
	for i := range doc.Cells {
		c := &doc.Cells[i]
		if c.Meta.SourceUUID != "" {
			p.seen[c.Meta.SourceUUID] = true
		}
		for _, o := range c.Outputs {
			if id, ok := o.Data["sourceUuid"].(string); ok && id != "" {
				p.seen[id] = true
			}
		}
		// A cell left running is the turn that was in flight when we
		// stopped; output resumes on it rather than opening a duplicate.
		if c.Meta.Provenance == ProvenanceMirrored && c.State == CellRunning {
			p.current, p.currentParent = c.ID, c.Meta.ParentUUID
		}
	}
}

// Ingest folds one transcript line's parts into the document. Parts from a
// single line are handled together because the line, not the part, is the
// unit the CLI assigns an id to.
func (p *sessionProjector) Ingest(parts []TranscriptPart) {
	// Children whose parent link arrived during this pass. Released after
	// the lock is dropped: IngestSubagent takes p.mu itself, and appending
	// to the store under our own lock is exactly the pattern the store's
	// own comment warns against.
	var release []string

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	for _, part := range parts {
		// Subagent turns belong in their own document (M6 nests them).
		// Interleaved into the parent they read as the main agent
		// contradicting itself mid-turn.
		if part.Sidechain {
			continue
		}
		// A part with no id cannot be recognised on the next restart. We
		// drop it rather than duplicate it on every reconnect: losing one
		// line is a gap, duplicating it is a document that lies.
		if part.UUID == "" || p.seen[part.UUID] {
			continue
		}
		if part.Kind == PartToolResult && part.AgentID != "" && p.current != "" {
			if p.linkSubagentLocked(part.AgentID, part.AgentType, p.current) {
				release = append(release, part.AgentID)
			}
		}
		if p.apply(part) {
			p.seen[part.UUID] = true
		}
	}
	p.mu.Unlock()

	for _, agentID := range release {
		p.mu.Lock()
		held := p.heldSubagent[agentID]
		delete(p.heldSubagent, agentID)
		p.mu.Unlock()
		if len(held) > 0 {
			p.IngestSubagent(agentID, held)
		}
	}
}

// apply folds one part, reporting whether it was recorded. A part we could
// not write must not be marked seen, or a transient failure becomes a
// permanent hole.
func (p *sessionProjector) apply(part TranscriptPart) bool {
	switch part.Kind {
	case PartUserText:
		// A new prompt ends the previous turn. The CLI does not announce
		// that a turn is over; the next prompt is the announcement.
		//
		// Unless it is the *same* turn re-sent: a shared parent means the
		// open cell was abandoned before it produced anything, which is an
		// interruption rather than a success.
		end := CellOK
		if part.ParentUUID != "" && part.ParentUUID == p.currentParent {
			end = CellInterrupted
		}
		p.settleCurrent(end)
		// A prompt we sent ourselves already has a cell. Attaching to it
		// rather than inserting is what keeps the document from showing
		// everything you type twice.
		if p.adopt(part) {
			return true
		}
		p.pendingParent = part.ParentUUID
		ok := p.openCell(CellPrompt, part.Text, ProvenanceMirrored, part.UUID, part.Model, CellRunning)
		p.pendingParent = ""
		p.currentParent = part.ParentUUID
		return ok

	case PartCompactSummary:
		p.settleCurrent(CellOK)
		ok := p.openCell(CellMarkdown, part.Text, ProvenanceCompact, part.UUID, "", CellOK)
		// A summary is not a turn, so nothing hangs under it.
		p.current = ""
		return ok

	case PartInterrupted:
		// A state change on the turn that was running, not a turn. Whatever
		// the agent produced before being stopped is kept — that is the
		// difference between interrupted and failed.
		p.settleCurrent(CellInterrupted)
		return true

	case PartInjection:
		// Context the model read that nobody typed. It belongs to the turn
		// it entered, does not change that turn's state, and is recorded as
		// label and size rather than body — the transcript on disk is the
		// archive, and duplicating every injection would make a notebook
		// cost the size of everything ever put in front of the model.
		return p.appendOutput(part, Output{
			Type: OutputInjection,
			Text: part.Text,
			Data: map[string]any{
				"sourceUuid": part.UUID,
				"label":      part.Label,
				"size":       part.Size,
			},
		})

	case PartAssistantText:
		return p.appendOutput(part, Output{
			Type: OutputText, Text: part.Text,
			Data: map[string]any{"sourceUuid": part.UUID},
		})

	case PartThinking:
		return p.appendOutput(part, Output{
			Type: OutputThinking, Text: part.Text,
			Data: map[string]any{"sourceUuid": part.UUID},
		})

	case PartToolCall:
		data := map[string]any{
			"sourceUuid": part.UUID,
			"name":       part.ToolName,
			"toolUseId":  part.ToolUseID,
		}
		// The input is the CLI's JSON. It is carried through as decoded
		// values so the renderer can show a command as a command rather
		// than as an escaped string.
		if len(part.ToolInput) > 0 {
			var in any
			if json.Unmarshal(part.ToolInput, &in) == nil {
				data["input"] = in
			}
		}
		return p.appendOutput(part, Output{Type: OutputToolCall, Data: data})

	case PartToolResult:
		out := Output{
			Type: OutputToolResult, Text: part.Text,
			Data: map[string]any{"sourceUuid": part.UUID, "toolUseId": part.ToolUseID},
		}
		if part.AgentID != "" {
			out.Data["spawnedAgentId"] = part.AgentID
		}
		if part.IsError {
			// The model treats a failed tool result as ordinary input and
			// reacts to it, so this is output that failed, not a failed
			// cell. The cell's own state is not touched.
			out.Data["isError"] = true
		}
		return p.appendOutput(part, out)
	}
	return false
}

// openCell inserts a cell at the end and makes it the one output lands on.
func (p *sessionProjector) openCell(typ CellType, source, provenance, srcUUID, model string, state CellState) bool {
	cell := Cell{
		ID:     uuid.NewString(),
		Type:   typ,
		Source: source,
		State:  state,
		Meta: CellMeta{
			Provenance: provenance,
			SourceUUID: srcUUID,
			ParentUUID: p.pendingParent,
			Model:      model,
		},
	}
	if _, err := p.st.Append(evCellInserted, cellInsertedPayload{Cell: cell}); err != nil {
		return false
	}
	p.current = cell.ID
	return true
}

// appendOutput lands a produced block on the open cell, opening an honest
// placeholder first if there isn't one.
func (p *sessionProjector) appendOutput(part TranscriptPart, out Output) bool {
	if p.current == "" {
		// Attaching to a session already in flight is the normal case, not
		// an edge one — you open the browser ten minutes in. The output has
		// to land somewhere, and that somewhere must not claim to be a
		// prompt the user typed, so the source stays empty and the UI can
		// say what it is.
		if !p.openCell(CellPrompt, "", ProvenanceMirrored, "", part.Model, CellRunning) {
			return false
		}
	}
	_, err := p.st.Append(evOutputAppended, outputAppendedPayload{
		CellID: p.current,
		Output: out,
	})
	return err == nil
}

// settleCurrent gives the open cell a terminal state. Without it every
// finished turn keeps its spinner and the document reads as permanently in
// flight — which is exactly as informative as a terminal that never
// returns to a prompt.
func (p *sessionProjector) settleCurrent(state CellState) {
	if p.current == "" {
		return
	}
	p.st.Append(evRunFinished, runFinishedPayload{ //nolint:errcheck // a lost terminal event is not worth failing the ingest
		CellID: p.current,
		Status: state,
	})
	p.current, p.currentParent = "", ""
}

// Close settles the turn that was in flight when the session ended. It is
// idempotent because teardown paths are not perfectly sequenced and a turn
// that finished twice is worse than one that finished late.
func (p *sessionProjector) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	p.settleCurrent(CellOK)
}

// ─── Opening a session's notebook ───────────────────────────────────────

// sessionNotebookSlug maps a session id to a stable notebook id.
//
// Stability is the whole point: createNotebook uniquifies its slug so two
// notebooks called "Notes" can coexist, which is right for documents you
// author and exactly wrong here — a restart would open notebook number two
// beside the session's real one and the history would appear to vanish.
//
// Session ids come from elsewhere and are not guaranteed to be slug-shaped,
// so anything outside the allowed alphabet is folded to a dash and the
// result is truncated to fit. Two ids that differ only in stripped
// characters would collide, which is why the hash suffix is unconditional.
func sessionNotebookSlug(sessionID string) string {
	const prefix = "session-"
	sum := sha256.Sum256([]byte(sessionID))
	suffix := "-" + hex.EncodeToString(sum[:4])

	var b strings.Builder
	for _, r := range strings.ToLower(sessionID) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 32 {
			break
		}
	}
	body := strings.Trim(b.String(), "-_")
	if body == "" {
		body = "x"
	}
	return prefix + body + suffix
}

// openSessionNotebook returns the notebook for a session, creating it on
// first use. The document outlives the process that produced it: a session
// ends, its notebook stays, which is the difference between this and the
// ring buffer it replaces.
func openSessionNotebook(sessionID, cli, cwd string, caps Capabilities) (*notebookStore, error) {
	slug := sessionNotebookSlug(sessionID)
	if st, err := acquireNotebook(slug); err == nil {
		return st, nil
	} else if !errors.Is(err, errNotebookNotFound) {
		return nil, err
	}

	title := cli + " · " + sessionID
	st, err := openNamedNotebook(slug, title, cwd)
	if err != nil {
		return nil, err
	}
	// The link back to the session, and the switch that sends prompt cells
	// to the CLI instead of to our own provider.
	if m := st.Doc().Meta; m.SessionID != sessionID || m.CLI != cli {
		meta := m
		meta.SessionID, meta.CLI = sessionID, cli
		if _, err := st.Append(evMetaSet, metaSetPayload{Meta: &meta}); err != nil {
			return nil, err
		}
	}

	// The gap an adapter cannot fill is stated in the document's fidelity
	// block, which is derived on read (#47 P2). P0 wrote a markdown cell
	// here instead; that put a claim about the *build* permanently into a
	// *document*, where it would still be asserting "codex turns are not
	// shown" long after someone wrote the parser.
	_ = caps
	return st, nil
}

// openSessionProjector gives a session its notebook and the projector that
// fills it, memoised on the session. Returns nil when the notebook cannot
// be opened: a session must keep running and keep reporting usage even if
// its document is unavailable, because the document is a view and the
// session is the work.
func openSessionProjector(s *Session) *sessionProjector {
	s.mu.Lock()
	if s.projector != nil {
		p := s.projector
		s.mu.Unlock()
		return p
	}
	id, cwd := s.ID, s.Cwd
	s.mu.Unlock()

	adapter := s.adapter()
	if adapter == nil {
		return nil
	}
	st, err := openSessionNotebook(id, adapter.Name(), cwd, adapter.Capabilities())
	if err != nil {
		log.Printf("[%s] notebook unavailable: %v", id, err)
		return nil
	}
	p := newSessionProjector(st)

	s.mu.Lock()
	defer s.mu.Unlock()
	// Another goroutine may have won the race while we were on disk. Use
	// its projector, not ours, or two of them fold into one log.
	if s.projector != nil {
		return s.projector
	}
	s.nb, s.projector = st, p
	return p
}

// notebookSlugOf is the nil-safe accessor toJSON needs. Callers hold s.mu.
func notebookSlugOf(st *notebookStore) string {
	if st == nil {
		return ""
	}
	return st.slug
}

// ─── Driving the session ────────────────────────────────────────────────

// sendToSession writes text to a session's PTY as if it had been typed.
// #47 P1, per ADR 0002 D1'.
//
// This is the notebook's other half. P0 made a session readable; this is
// what makes the document the place you work rather than a viewer beside
// the place you work. The CLI has no API — the PTY is the API — so a
// prompt cell is submitted the same way a person submits one, with a
// carriage return. Without that return the text sits in the CLI's input
// box and nothing happens, which looks exactly like a hung agent.
func sendToSession(sessionID, text string) error {
	s := getSession(sessionID)
	if s == nil {
		return fmt.Errorf("session %s is no longer running — its notebook is a record now, not a control surface", sessionID)
	}
	pt := s.pty()
	if pt == nil {
		return fmt.Errorf("session %s has no terminal attached yet", sessionID)
	}
	if _, err := pt.Write([]byte(text + "\r")); err != nil {
		return fmt.Errorf("writing to session %s: %w", sessionID, err)
	}
	return nil
}

// interruptSession stops whatever the agent is doing. Escape is what a
// person presses, and it is all the CLI offers.
func interruptSession(sessionID string) error {
	s := getSession(sessionID)
	if s == nil {
		return fmt.Errorf("session %s is no longer running", sessionID)
	}
	pt := s.pty()
	if pt == nil {
		return fmt.Errorf("session %s has no terminal attached", sessionID)
	}
	_, err := pt.Write([]byte("\x1b"))
	return err
}

// AwaitAdoption tells the projector that the cell it is about to see
// mirrored back already exists.
//
// Without it the document shows every prompt twice: once as the cell you
// wrote and once as the cell the transcript reported. Matching on the text
// is crude but it is the only link there is — the CLI does not echo an id
// back, and it has no idea the prompt came from us rather than from a
// keyboard.
func (p *sessionProjector) AwaitAdoption(cellID, source string) {
	p.AwaitAdoptionFor(cellID, source, adoptionTimeout)
}

// adoptionTimeout bounds how long a sent prompt may go unmirrored before
// we admit it may never have arrived. Generous: the CLI writes the user
// turn to its transcript almost immediately, but the watcher polls at
// 500ms and a busy machine is slower than a quiet one.
const adoptionTimeout = 20 * time.Second

// AwaitAdoptionFor is AwaitAdoption with an explicit deadline.
//
// The deadline exists because the modal gate is best-effort. Detecting
// that a CLI is showing a dialog is regex archaeology on ANSI bytes
// (menu.go), and it does not catch every one — the "Set up auto mode"
// dialog it missed is why this timeout was written. A prompt that lands in
// a dialog instead of the input box leaves its cell running forever, which
// looks exactly like an agent thinking hard. Saying "this may not have
// arrived, go and look" is the honest end state.
func (p *sessionProjector) AwaitAdoptionFor(cellID, source string, within time.Duration) {
	p.mu.Lock()
	p.adoptCell, p.adoptText = cellID, source
	p.adoptGen++
	gen := p.adoptGen
	p.mu.Unlock()

	time.AfterFunc(within, func() { p.giveUpOnAdoption(cellID, gen) })
}

// giveUpOnAdoption fires when a sent prompt was never mirrored back. The
// generation check makes it a no-op if the cell was adopted, superseded,
// or another prompt was sent in the meantime.
func (p *sessionProjector) giveUpOnAdoption(cellID string, gen uint64) {
	p.mu.Lock()
	if p.closed || p.adoptGen != gen || p.adoptCell != cellID {
		p.mu.Unlock()
		return
	}
	p.adoptCell, p.adoptText = "", ""
	p.mu.Unlock()

	p.st.Append(evOutputAppended, outputAppendedPayload{ //nolint:errcheck
		CellID: cellID,
		Output: Output{
			Type: OutputError,
			Text: "The agent never echoed this prompt back, so it may not have reached it — " +
				"a CLI showing a dialog swallows whatever is typed. Open the terminal to check.",
		},
	})
	p.st.Append(evRunFinished, runFinishedPayload{ //nolint:errcheck
		CellID: cellID, Status: CellError,
	})
}

// CancelAdoption drops a pending adoption without settling its cell — the
// caller has already decided what the cell says (#57).
func (p *sessionProjector) CancelAdoption(cellID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.adoptCell == cellID {
		p.adoptCell, p.adoptText = "", ""
		p.adoptGen++
	}
}

// adopt attaches an incoming mirrored prompt to the cell the user already
// authored, returning whether it did. One shot: a later identical prompt
// is a new turn, not a second chance.
func (p *sessionProjector) adopt(part TranscriptPart) bool {
	if p.adoptCell == "" {
		return false
	}
	cellID, want := p.adoptCell, p.adoptText
	p.adoptCell, p.adoptText = "", ""
	p.adoptGen++ // any pending give-up timer for this cell is now stale

	if strings.TrimSpace(part.Text) != strings.TrimSpace(want) {
		// Not ours. The cell we sent is not going to be answered — the
		// send failed, or you typed into the terminal instead — so settle
		// it rather than leaving it spinning until the page is reloaded.
		p.st.Append(evRunFinished, runFinishedPayload{ //nolint:errcheck
			CellID: cellID, Status: CellInterrupted,
		})
		return false
	}

	// Upgrade in place: it is a mirrored cell now, because the CLI owns the
	// turn from here on and the read-only rules have to apply to it.
	meta := CellMeta{
		Provenance: ProvenanceMirrored,
		SourceUUID: part.UUID,
		ParentUUID: part.ParentUUID,
		Model:      part.Model,
	}
	if _, err := p.st.Append(evCellEdited, cellEditedPayload{CellID: cellID, Meta: &meta}); err != nil {
		return false
	}
	p.current, p.currentParent = cellID, part.ParentUUID
	return true
}

// sessionProjectorFor finds the projector driving a session's notebook, or
// nil if the session has none — no transcript yet, or an adapter that
// cannot project.
func sessionProjectorFor(sessionID string) *sessionProjector {
	s := getSession(sessionID)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projector
}

// checkSessionDrivable reports whether a prompt could be sent right now.
// Separate from sendToSession so the HTTP layer can refuse before a run is
// begun: a cell that starts and never finishes is worse than a 503.
func checkSessionDrivable(sessionID string) error {
	s := getSession(sessionID)
	if s == nil {
		return fmt.Errorf("session %s is no longer running — this notebook is a record of it, not a control surface", sessionID)
	}
	if s.pty() == nil {
		return fmt.Errorf("session %s has no terminal attached yet", sessionID)
	}
	// A CLI showing a dialog reads whatever arrives as an answer to it.
	// Sending a prompt now is not merely useless — a prompt beginning with
	// "1" would select the first option of a permission request.
	if s.hasPending() {
		return errAgentWaiting
	}
	// #57. The menu detector has been reporting numbered dialogs on every
	// tick since long before the notebook existed, and nothing read it.
	// This is the auto-mode dialog that swallowed two live prompts: it
	// fires no hook, so Pending stays empty, but it was on screen and
	// detected the whole time.
	if opts := s.getMenuOptions(); len(opts) > 0 {
		return fmt.Errorf("%w — it is showing: %s", errAgentWaiting, menuSummary(opts))
	}
	return nil
}

// menuSummary names what is on screen. A refusal that does not say what is
// in the way is indistinguishable from the agent simply being busy, and
// leaves the reader with nothing to do about it.
func menuSummary(opts []MenuOption) string {
	labels := make([]string, 0, len(opts))
	for i, o := range opts {
		if i == 3 {
			labels = append(labels, "…")
			break
		}
		labels = append(labels, o.Key+". "+o.Label)
	}
	return strings.Join(labels, " / ")
}

// errAgentWaiting is a distinct error because the HTTP layer answers it
// with 409 rather than 503: nothing is broken, the agent is just mid-
// question and the answer has to be given deliberately.
var errAgentWaiting = errors.New(
	"the agent is waiting on an answer — respond to it before sending a new prompt, " +
		"or it will read the prompt as the answer")

// ─── Subagents (#55a) ───────────────────────────────────────────────────
//
// A parent turn that delegates shows an Agent call and its result with
// nothing in between, and that gap is usually the majority of what the
// agent did. The child's conversation lives in its own transcript, and the
// parent's tool result names it — so the attachment is by the link the
// format gives us, never by "whichever turn happened to be open".
//
// Ordering runs both ways and neither is the exception. A background
// launch reports its child id immediately and the work arrives over the
// following minutes; a synchronous Agent call writes the child's whole
// transcript first and names it only in the result. So child work that
// arrives before its link is held, and released when the link shows up.

// maxHeldSubagentParts bounds work held for a child that nothing has
// claimed yet. A child whose result never arrives — a crashed parent, a
// transcript we started reading mid-session — would otherwise accumulate
// for as long as the session runs. Dropping the oldest is right: the tail
// of a subagent's conversation is the part that concludes something.
const maxHeldSubagentParts = 200

// linkSubagentLocked records that a child belongs to a turn. Caller holds
// p.mu. Reports whether this call established the link, so the caller
// knows to release whatever was held for that child once it can.
func (p *sessionProjector) linkSubagentLocked(agentID, agentType, cellID string) bool {
	if p.subagentCell == nil {
		p.subagentCell = map[string]subagentLink{}
	}
	if _, ok := p.subagentCell[agentID]; ok {
		return false // already linked; a second result changes nothing
	}
	p.subagentCell[agentID] = subagentLink{cellID: cellID, agentType: agentType}
	return true
}

// IngestSubagent folds one child transcript's parts into its parent's cell.
func (p *sessionProjector) IngestSubagent(agentID string, parts []TranscriptPart) {
	p.mu.Lock()
	if p.closed || agentID == "" {
		p.mu.Unlock()
		return
	}
	link, linked := p.subagentCell[agentID]
	if !linked {
		// Nothing claims this child yet. Hold it rather than guessing a
		// parent, and rather than dropping work we will be able to place
		// in a moment.
		if p.heldSubagent == nil {
			p.heldSubagent = map[string][]TranscriptPart{}
		}
		kept := append(p.heldSubagent[agentID], parts...)
		if len(kept) > maxHeldSubagentParts {
			kept = kept[len(kept)-maxHeldSubagentParts:]
		}
		p.heldSubagent[agentID] = kept
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	for _, part := range parts {
		if part.UUID == "" {
			continue // unidentifiable, and so undedupable across restarts
		}
		p.mu.Lock()
		if p.seen[part.UUID] {
			p.mu.Unlock()
			continue
		}
		p.mu.Unlock()

		out, ok := subagentOutput(part, agentID, link.agentType)
		if !ok {
			continue
		}
		if _, err := p.st.Append(evOutputAppended, outputAppendedPayload{
			CellID: link.cellID, Output: out,
		}); err != nil {
			continue
		}
		p.mu.Lock()
		p.seen[part.UUID] = true
		p.mu.Unlock()
	}
}

// subagentOutput renders one child part with the same vocabulary the
// parent's own work uses — a tool call is a tool call whoever made it —
// tagged so the renderer can nest it under the child that produced it.
func subagentOutput(part TranscriptPart, agentID, agentType string) (Output, bool) {
	data := map[string]any{
		"sourceUuid": part.UUID,
		"agentId":    agentID,
	}
	if agentType != "" {
		data["agentType"] = agentType
	}
	switch part.Kind {
	case PartAssistantText, PartUserText:
		// A child's "user" turn is the task it was given, not a person
		// typing, so both render as the child's own words.
		if strings.TrimSpace(part.Text) == "" {
			return Output{}, false
		}
		return Output{Type: OutputText, Text: part.Text, Data: data}, true
	case PartThinking:
		return Output{Type: OutputThinking, Text: part.Text, Data: data}, true
	case PartToolCall:
		data["name"] = part.ToolName
		data["toolUseId"] = part.ToolUseID
		if len(part.ToolInput) > 0 {
			var in any
			if json.Unmarshal(part.ToolInput, &in) == nil {
				data["input"] = in
			}
		}
		return Output{Type: OutputToolCall, Data: data}, true
	case PartToolResult:
		data["toolUseId"] = part.ToolUseID
		if part.IsError {
			data["isError"] = true
		}
		return Output{Type: OutputToolResult, Text: part.Text, Data: data}, true
	}
	// Injections and interruptions inside a child are noise at this depth.
	return Output{}, false
}
