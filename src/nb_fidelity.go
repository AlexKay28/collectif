package main

// nb_fidelity.go — what this notebook can actually show you.
// #47 P2, per ADR 0002 D11.
//
// D11 says projection fidelity is per-adapter and visibly degraded, never
// faked. P0 and P1 built the fidelity; this is the "visibly" half, and it
// turned out to be the harder one.
//
// The obvious version — a boolean and a note — is wrong twice over.
//
// One boolean cannot describe a session. A codex session cannot show you
// its turns, but it can still be sent prompts, and its permission requests
// still reach the notebook, because it has hooks. Collapsing that to
// "degraded" tells you nothing about what you can actually do here, which
// is the only question a banner is for.
//
// And the statement does not belong in the log. What a build can do is a
// property of the code, not of the document: a note written into a
// notebook in August is still asserting "codex turns are not shown" in
// December, after someone has written the parser. So this is derived on
// every read and stored nowhere. P0's explanatory markdown cell is gone;
// it was expedient and it was in a user's document, permanently, making a
// claim about a version of collectif that had already moved on.

// NotebookFidelity is the honest answer to "what works in this notebook".
// Nil on a detached notebook, which has no CLI and so no question to
// answer — its prompt cells run on our own provider and every surface
// works by construction.
type NotebookFidelity struct {
	CLI string `json:"cli"`

	// Turns is whether the agent's conversation is projected into cells.
	// False means the document stays empty of the agent's work no matter
	// how long it runs, and the terminal is the only place to watch it.
	Turns bool `json:"turns"`

	// Approvals is whether permission requests appear in the document and
	// can be answered here. Requires hooks; independent of Turns.
	Approvals bool `json:"approvals"`

	// Send is whether prompt cells reach the agent. True for every CLI:
	// the PTY is the input, and every CLI has one.
	Send bool `json:"send"`

	// Usage is whether token counts and cost are read from the CLI's own
	// transcript rather than guessed.
	Usage bool `json:"usage"`

	// Subagents is whether delegated work is nested into the turn that
	// spawned it (#55a). Without it an agent that delegated heavily reads
	// as one that did nothing between its tool call and its result, which
	// is a far more misleading absence than a missing token count.
	Subagents bool `json:"subagents"`

	// MCP is whether a call that went out to one of the user's own MCP
	// servers is shown as one (#54). False does not mean the session made
	// no MCP calls — it means we cannot tell which of its calls were, so
	// every one of them reads as built-in. That is the more dangerous
	// absence of the two: a reader who cannot see the distinction assumes
	// there was none.
	MCP bool `json:"mcp"`
}

// fidelityOf maps an adapter's capabilities onto the surfaces a person
// actually uses. A nil adapter claims nothing but Send, because a terminal
// is the one thing we can be sure of.
func fidelityOf(a CLIAdapter) NotebookFidelity {
	if a == nil {
		return NotebookFidelity{Send: true}
	}
	caps := a.Capabilities()
	_, locates := a.(subagentLocator)
	_, namesMCP := a.(mcpNamer)
	return NotebookFidelity{
		CLI:       a.Name(),
		Turns:     caps.TranscriptContent,
		Approvals: caps.Hooks,
		Send:      true,
		Usage:     caps.StructuredTranscript,
		// Both halves are required: knowing where the children are, and
		// being able to read them once found.
		Subagents: caps.SubagentFiles && caps.TranscriptContent && locates,
		// Likewise both halves: knowing the CLI's namespacing convention
		// is worth nothing on a CLI whose tool calls are not projected at
		// all. #54's whole finding is that MCP needs no capability of its
		// own beyond being able to read the tool call it rode in on.
		MCP: caps.TranscriptContent && namesMCP,
	}
}

// fidelityFor resolves the fidelity of a notebook's session, or nil if it
// is detached. Looked up through the adapter registry rather than through
// the live session, so a notebook whose session has ended still says what
// it was able to record.
func fidelityFor(doc *Notebook) *NotebookFidelity {
	if doc == nil || doc.Meta.SessionID == "" {
		return nil
	}
	name := doc.Meta.CLI
	if name == "" {
		name = defaultAdapterName
	}
	f := fidelityOf(adapters[name])
	if f.CLI == "" {
		f.CLI = name
	}
	return &f
}
