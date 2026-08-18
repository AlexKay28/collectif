package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #52 M3. The permission gate, end to end through a detached notebook's
// run loop.
//
// The requirement that shaped this file: a policy `ask` emits the *same*
// OutputApproval the CLI's own questions already use, resolved by the same
// append-only pair, folded by the same renderer. Two approval widgets that
// looked different for the same decision would be a worse outcome than not
// building the second one, so these tests assert the shape of the record
// rather than only the effect on the filesystem.

// findApproval returns the open question on a cell, if there is one.
func findApproval(c Cell) (Output, bool) {
	resolved := map[string]bool{}
	for _, o := range c.Outputs {
		if o.Type == OutputApproval && o.Data["resolution"] != nil {
			resolved[str(o.Data["approvalId"])] = true
		}
	}
	for _, o := range c.Outputs {
		if o.Type == OutputApproval && o.Data["resolution"] == nil && !resolved[str(o.Data["approvalId"])] {
			return o, true
		}
	}
	return Output{}, false
}

func findResolution(c Cell) (Output, bool) {
	for _, o := range c.Outputs {
		if o.Type == OutputApproval && o.Data["resolution"] != nil {
			return o, true
		}
	}
	return Output{}, false
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// waitForApproval polls the document until the run blocks on a question.
func (f *nbFixture) waitForApproval(t *testing.T, cellID string, timeout time.Duration) Output {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		doc := f.st.Doc()
		if i := indexOfCell(doc, cellID); i >= 0 {
			if o, ok := findApproval(doc.Cells[i]); ok {
				return o
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no approval question appeared on cell %s within %s", cellID, timeout)
	return Output{}
}

// writeCall scripts a provider that asks for one write and then answers.
func writeCall(path, content string) []scriptedTurn {
	return []scriptedTurn{
		{toolName: "write", toolInput: map[string]any{"path": path, "content": content}},
		{text: "done"},
	}
}

// ─── allow ──────────────────────────────────────────────────────────────

func TestPolicyGate_AnAllowedCallRunsAndTheRuleIsRecorded(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Allow: []string{"write(**)"}})
	withTools(t, &writeTool{})
	withProvider(t, &fakeProvider{turns: writeCall("out.txt", "hello\n")})
	cell := f.addCell(t, "prompt", "write a file")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if c.State != CellOK {
		t.Fatalf("State = %q (outputs %+v)", c.State, c.Outputs)
	}
	got, err := os.ReadFile(filepath.Join(f.root, "out.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("the allowed write did not happen: %q (%v)", got, err)
	}
	// No question was asked, so there is no approval block. The decision is
	// still in the log — on the result it authorised, which is where it can
	// be read without an extra event per call.
	if _, ok := findApproval(c); ok {
		t.Error("an allowed call put an approval question in the document")
	}
	var sawRule bool
	for _, o := range c.Outputs {
		if o.Type == OutputToolResult && str(o.Data["policyRule"]) == "write(**)" {
			sawRule = true
		}
	}
	if !sawRule {
		t.Errorf("the rule that allowed the call was not recorded: %+v", c.Outputs)
	}
	// A write that succeeded shows what it changed. This is the output type
	// ADR 0001 §4.1 built the renderer for and nothing had ever produced.
	if !hasOutputOfType(c, OutputDiff) {
		t.Error("a successful write recorded no diff output")
	}
}

// ─── deny ───────────────────────────────────────────────────────────────

func TestPolicyGate_ADeniedCallNeverRunsAndTheModelIsTold(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Deny: []string{"write(**/.env)"}, Allow: []string{"write(**)"}})
	withTools(t, &writeTool{})
	fp := &fakeProvider{turns: writeCall(".env", "STOLEN=1\n")}
	withProvider(t, fp)
	cell := f.addCell(t, "prompt", "write the env file")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if _, err := os.Stat(filepath.Join(f.root, ".env")); err == nil {
		t.Fatal("a denied write happened anyway")
	}
	// A denial is a tool result, not a failed run: the model has to see it
	// and adapt, and ending the turn would deny it the chance.
	if c.State != CellOK {
		t.Errorf("State = %q, want the run to continue after a denial", c.State)
	}
	if len(fp.sent()) != 2 {
		t.Errorf("provider called %d times, want the loop to reach a second turn", len(fp.sent()))
	}
	// The document says what was refused and which rule refused it.
	res, ok := findResolution(c)
	if !ok {
		t.Fatalf("no approval record for the denial: %+v", c.Outputs)
	}
	if str(res.Data["resolution"]) != "denied" || str(res.Data["rule"]) != "write(**/.env)" {
		t.Errorf("denial record = %+v, want resolution denied via write(**/.env)", res.Data)
	}
}

// ─── ask ────────────────────────────────────────────────────────────────

func TestPolicyGate_AnAskBlocksTheRunAndProceedsWhenApproved(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Ask: []string{"write(**)"}})
	withTools(t, &writeTool{})
	withProvider(t, &fakeProvider{turns: writeCall("notes.md", "# Notes\n")})
	cell := f.addCell(t, "prompt", "write notes")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	q := f.waitForApproval(t, cell, 10*time.Second)

	// The same output type the CLI's questions use, with the same
	// approvalId field the renderer folds on.
	if q.Type != OutputApproval {
		t.Fatalf("question output type = %q, want %q", q.Type, OutputApproval)
	}
	if str(q.Data["approvalId"]) == "" {
		t.Fatal("the question carries no approvalId, so no resolution can ever be paired with it")
	}
	if str(q.Data["tool"]) != "write" {
		t.Errorf("question tool = %q", q.Data["tool"])
	}
	// The proposed diff is the whole reason this decision is better made in
	// a notebook than in a terminal.
	if diff := str(q.Data["diff"]); !strings.Contains(diff, "+# Notes") {
		t.Errorf("the question carries no proposed diff: %+v", q.Data)
	}
	// The fourth answer needs to say what it would write before it writes.
	if got := str(q.Data["alwaysRule"]); got != "write(**)" {
		t.Errorf("alwaysRule = %q, want the rule an always-allow would append", got)
	}
	// Nothing happened while the question is open.
	if _, err := os.Stat(filepath.Join(f.root, "notes.md")); err == nil {
		t.Fatal("the write happened before it was approved")
	}

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/approve",
		map[string]any{"answer": "approve", "approvalId": q.Data["approvalId"]})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}

	c := f.waitForState(t, cell, 10*time.Second)
	if got, err := os.ReadFile(filepath.Join(f.root, "notes.md")); err != nil || string(got) != "# Notes\n" {
		t.Fatalf("the approved write did not happen: %q (%v)", got, err)
	}
	res, ok := findResolution(c)
	if !ok || str(res.Data["resolution"]) != "approved" {
		t.Errorf("resolution = %+v, want approved", res.Data)
	}
	if str(res.Data["approvalId"]) != str(q.Data["approvalId"]) {
		t.Error("the resolution does not pair with its question")
	}
}

func TestPolicyGate_ADeniedAskDoesNotRunTheTool(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Ask: []string{"write(**)"}})
	withTools(t, &writeTool{})
	withProvider(t, &fakeProvider{turns: writeCall("nope.txt", "x")})
	cell := f.addCell(t, "prompt", "write it")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	q := f.waitForApproval(t, cell, 10*time.Second)
	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/approve",
		map[string]any{"answer": "deny", "approvalId": q.Data["approvalId"]})
	if rec.Code != http.StatusOK {
		t.Fatalf("deny: %d %s", rec.Code, rec.Body.String())
	}

	c := f.waitForState(t, cell, 10*time.Second)
	if _, err := os.Stat(filepath.Join(f.root, "nope.txt")); err == nil {
		t.Fatal("a denied call ran anyway")
	}
	res, _ := findResolution(c)
	if str(res.Data["resolution"]) != "denied" {
		t.Errorf("resolution = %+v, want denied", res.Data)
	}
}

// The fourth answer, and the only genuinely new piece of UI in this phase.
func TestPolicyGate_AlwaysAllowWritesTheRuleItShowed(t *testing.T) {
	f := newNBFixture(t)
	rulesFile := withTempPolicy(t, policyRules{Ask: []string{"write(**)"}})
	withTools(t, &writeTool{})
	withProvider(t, &fakeProvider{turns: writeCall("src/x.go", "package x\n")})
	cell := f.addCell(t, "prompt", "write it")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	q := f.waitForApproval(t, cell, 10*time.Second)
	shown := str(q.Data["alwaysRule"])
	if shown != "write(src/**)" {
		t.Fatalf("alwaysRule = %q, want write(src/**)", shown)
	}

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/approve",
		map[string]any{"answer": "always", "approvalId": q.Data["approvalId"]})
	if rec.Code != http.StatusOK {
		t.Fatalf("always: %d %s", rec.Code, rec.Body.String())
	}
	c := f.waitForState(t, cell, 10*time.Second)

	if got, err := os.ReadFile(filepath.Join(f.root, "src/x.go")); err != nil || string(got) != "package x\n" {
		t.Fatalf("the approved write did not happen: %q (%v)", got, err)
	}
	// The rule that was shown is the rule that was written. Anything else
	// makes the confirmation a lie.
	body, err := os.ReadFile(rulesFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), shown) {
		t.Errorf("the rules file does not contain %q:\n%s", shown, body)
	}
	res, _ := findResolution(c)
	if str(res.Data["rule"]) != shown {
		t.Errorf("the resolution does not record the rule it wrote: %+v", res.Data)
	}
}

// An unanswered question is recorded as expired, never as an approval.
// P1's honesty rule, inherited: record the decision, never infer one.
func TestPolicyGate_AnUnansweredAskExpiresAndIsNotAnApproval(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Ask: []string{"write(**)"}})
	withTools(t, &writeTool{})
	withProvider(t, &fakeProvider{turns: writeCall("timeout.txt", "x")})

	prev := policyApprovalTimeout
	policyApprovalTimeout = 250 * time.Millisecond
	t.Cleanup(func() { policyApprovalTimeout = prev })

	cell := f.addCell(t, "prompt", "write it")
	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if _, err := os.Stat(filepath.Join(f.root, "timeout.txt")); err == nil {
		t.Fatal("an expired question ran the tool")
	}
	res, ok := findResolution(c)
	if !ok {
		t.Fatalf("no resolution recorded for the expired question: %+v", c.Outputs)
	}
	if got := str(res.Data["resolution"]); got != "expired" {
		t.Errorf("resolution = %q, want expired — an unanswered question must never read as approved", got)
	}
}

// Answering a question nobody is waiting on is a 409, matching the session
// path: the question was real, it is simply no longer being asked.
func TestPolicyGate_AnsweringNothingIsAConflict(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "prompt", "idle")
	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/approve",
		map[string]any{"answer": "approve", "approvalId": "nope"})
	if rec.Code != http.StatusConflict {
		t.Errorf("got %d, want 409 when nothing is pending (body %q)", rec.Code, rec.Body.String())
	}
}

// A dispatch with no gate is a bug, and the only safe way to have a bug
// like that is loudly. Failing open would mean a future caller that forgot
// to pass one silently ran every tool unpoliced, and nothing would say so.
func TestDispatchTool_WithoutAGateRefuses(t *testing.T) {
	withTools(t, &writeTool{})
	root := t.TempDir()

	out := dispatchTool(context.Background(), nil,
		ToolCall{Name: "write", Input: map[string]any{"path": "x.txt", "content": "y"}}, root, nil)

	if !out.IsError {
		t.Error("a dispatch with no permission gate ran the tool")
	}
	if _, err := os.Stat(filepath.Join(root, "x.txt")); err == nil {
		t.Fatal("the tool ran anyway")
	}
}

// ─── Containment outranks policy ────────────────────────────────────────

// The load-bearing claim of the whole phase. `allow write(**)` is as loose
// as a rule can be, and it still cannot move the notebook root.
func TestPolicyGate_AnAllowRuleCannotWidenTheNotebookRoot(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Allow: []string{"write(**)", "write(*)", "bash(*)"}})
	withTools(t, &writeTool{})

	outside := filepath.Join(t.TempDir(), "owned.txt")
	withProvider(t, &fakeProvider{turns: writeCall(outside, "PWNED")})
	cell := f.addCell(t, "prompt", "escape")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	f.waitForState(t, cell, 10*time.Second)

	if _, err := os.Stat(outside); err == nil {
		t.Fatal("an allow rule let a write out of the notebook root")
	}
}

// ─── Per-notebook override, through the document ────────────────────────

// ADR 0001 §4.6 makes the rules overridable per notebook in Meta. The unit
// tests cover the merge; this covers that the notebook's own rules are the
// ones a run actually reads.
func TestPolicyGate_ANotebooksOwnRulesAreConsulted(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Ask: []string{"write(**)"}})
	withTools(t, &writeTool{})
	if _, err := f.st.Append(evMetaSet, metaSetPayload{Meta: &NotebookMeta{
		Permissions: &policyRules{Allow: []string{"write(**)"}},
	}}); err != nil {
		t.Fatal(err)
	}
	withProvider(t, &fakeProvider{turns: writeCall("own.txt", "ok\n")})
	cell := f.addCell(t, "prompt", "write it")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	c := f.waitForState(t, cell, 10*time.Second)

	if _, ok := findApproval(c); ok {
		t.Error("the notebook's own allow rule was ignored and the run asked anyway")
	}
	if got, err := os.ReadFile(filepath.Join(f.root, "own.txt")); err != nil || string(got) != "ok\n" {
		t.Fatalf("the write did not happen: %q (%v)", got, err)
	}
}

// A rule that does not parse matches nothing. That is the safe direction
// for allow and the dangerous one for deny: a typo in `deny bash(rm -rf *)`
// silently stops denying anything. So a notebook's rules are validated on
// the way in, where there is somewhere to put the error.
func TestPolicyGate_AMalformedRuleIsRejectedNotIgnored(t *testing.T) {
	f := newNBFixture(t)
	rec := nbRequest(t, f.srv, http.MethodPatch, f.base, map[string]any{
		"meta": map[string]any{"permissions": map[string]any{"deny": []string{"bash(rm -rf *"}}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for an unparseable rule (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bash(rm -rf *") {
		t.Errorf("the error should quote the rule it could not read: %q", rec.Body.String())
	}
	// A well-formed one still goes through.
	rec = nbRequest(t, f.srv, http.MethodPatch, f.base, map[string]any{
		"meta": map[string]any{"permissions": map[string]any{"deny": []string{"bash(rm -rf *)"}}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d for a valid rule: %s", rec.Code, rec.Body.String())
	}
}

// ─── bash ───────────────────────────────────────────────────────────────

// bash is the tool with no containment of its own — a command is not a
// path — so the gate is the only thing in front of it. Its question shows
// the command and carries no diff, because there is nothing to preview.
func TestPolicyGate_BashAsksWithItsCommandAndRunsWhenApproved(t *testing.T) {
	f := newNBFixture(t)
	withTempPolicy(t, policyRules{Ask: []string{"bash(*)"}})
	withTools(t, &bashTool{})
	withProvider(t, &fakeProvider{turns: []scriptedTurn{
		{toolName: "bash", toolInput: map[string]any{"command": "echo ran-it > made.txt"}},
		{text: "done"},
	}})
	cell := f.addCell(t, "prompt", "run it")

	nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/run", nil)
	q := f.waitForApproval(t, cell, 10*time.Second)

	input, _ := q.Data["input"].(map[string]any)
	if str(input["command"]) != "echo ran-it > made.txt" {
		t.Errorf("the question does not show the command: %+v", q.Data)
	}
	if q.Data["diff"] != nil {
		t.Errorf("bash previewed a diff it cannot know: %+v", q.Data["diff"])
	}
	if got := str(q.Data["alwaysRule"]); got != "bash(echo *)" {
		t.Errorf("alwaysRule = %q, want the program generalised", got)
	}
	if _, err := os.Stat(filepath.Join(f.root, "made.txt")); err == nil {
		t.Fatal("the command ran before it was approved")
	}

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/approve",
		map[string]any{"answer": "approve", "approvalId": q.Data["approvalId"]})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	f.waitForState(t, cell, 10*time.Second)
	if _, err := os.Stat(filepath.Join(f.root, "made.txt")); err != nil {
		t.Fatalf("the approved command did not run: %v", err)
	}
}

// ─── The other backend ──────────────────────────────────────────────────

// A notebook mirroring a CLI session must not grow a second permission
// gate. That CLI runs with its own permission model and never calls these
// tools; a policy question here would be about a decision we do not make.
func TestPolicyGate_ASessionNotebookAnswersThroughTheSessionPath(t *testing.T) {
	f := newNBFixture(t)
	sid := "no-such-session"
	if _, err := f.st.Append(evMetaSet, metaSetPayload{Meta: &NotebookMeta{SessionID: sid}}); err != nil {
		t.Fatal(err)
	}
	cell := f.addCell(t, "prompt", "hello")

	rec := nbRequest(t, f.srv, http.MethodPost, f.base+"/cells/"+cell+"/approve",
		map[string]any{"answer": "approve"})
	// Whatever it answers, it must not be the detached path's 409-on-no-
	// pending-policy-question: this notebook's questions come from the CLI.
	if rec.Code == http.StatusOK {
		t.Errorf("a session notebook approved through the policy path (body %q)", rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "session") {
		t.Errorf("error body %q should name the session as the thing that is gone", rec.Body.String())
	}
}
