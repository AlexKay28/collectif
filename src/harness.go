package main

import (
	"strings"
	"time"
)

// harness.go — #42 Harness feedback telemetry.
//
// This file consolidates two sub-features from the umbrella issue #42:
//
//   #42.1  Context window pressure gauge
//   #42.7  Session health score + loop detection
//
// Both are read-only observations of the Claude Code harness — nothing
// here modifies session behaviour. Everything is derived from data we
// already collect (usage stats from the transcript, tool events from
// hooks) plus a small amount of extra state on Session.

// ─── #42.1 Context window pressure ──────────────────────────────────────

// Anthropic model context windows in tokens. The prefix match runs against
// the "model" field on assistant messages, e.g. "claude-opus-4-7-20260115".
var contextLimits = []struct {
	Prefix string
	Limit  int
}{
	{"claude-opus-4", 200000},
	{"claude-sonnet-4", 200000},
	{"claude-haiku-4", 200000},
	{"claude-opus-3", 200000},
	{"claude-sonnet-3", 200000},
	{"claude-haiku-3", 200000},
}

const defaultContextLimit = 200000

// contextLimitFor returns the context window size in tokens for the given
// model string. Falls back to 200k if the model is unknown so the UI
// still renders a sensible pressure gauge.
func contextLimitFor(model string) int {
	if model == "" {
		return defaultContextLimit
	}
	for _, m := range contextLimits {
		if strings.HasPrefix(model, m.Prefix) {
			return m.Limit
		}
	}
	return defaultContextLimit
}

// contextUsedPct computes 0.0..1.0 for the LATEST turn's context size
// against the model's limit. The total for a turn is uncached input +
// cache-creation + cache-read — that's the full context the model saw.
// Routes the model→limit lookup through the session's CLIAdapter so each
// CLI carries its own model catalog (#46).
func contextUsedPct(s *Session) float64 {
	limit := defaultContextLimit
	if a := s.adapter(); a != nil {
		limit = a.ModelContextLimit(s.Model)
	}
	return contextUsedPctLocked(s.LastContextTokens, limit)
}

// contextUsedPctLocked is the lock-free core, taking already-snapshotted
// values so callers that already hold s.mu (e.g. toJSON) don't have to
// re-lock. Same semantics as contextUsedPct.
func contextUsedPctLocked(total int64, limit int) float64 {
	if total == 0 || limit == 0 {
		return 0
	}
	pct := float64(total) / float64(limit)
	if pct > 1 {
		pct = 1
	}
	return pct
}

// ─── #42.1 Threshold-crossing broadcasts ────────────────────────────────

// contextWarnFired tracks whether each session has already been warned
// at the 70% / 90% thresholds so we don't spam the dashboard every turn.
type contextThresholds struct {
	warned70, warned90 bool
}

var contextWarnState = map[string]*contextThresholds{}
var contextWarnMu = struct {
	m map[string]*contextThresholds
}{m: contextWarnState}

// maybeBroadcastContextPressure fires a WS event when a session crosses
// 70% or 90% context usage — mirrors the cost_warning shape from #35.
// Called from the transcript watcher after each successful usage read.
func maybeBroadcastContextPressure(s *Session) {
	pct := contextUsedPct(s)
	if pct < 0.7 {
		return
	}
	st, ok := contextWarnState[s.ID]
	if !ok {
		st = &contextThresholds{}
		contextWarnState[s.ID] = st
	}
	crossed := ""
	if pct >= 0.9 && !st.warned90 {
		st.warned90 = true
		crossed = "critical"
	} else if pct >= 0.7 && !st.warned70 {
		st.warned70 = true
		crossed = "warn"
	}
	if crossed == "" {
		return
	}
	limit := defaultContextLimit
	if a := s.adapter(); a != nil {
		limit = a.ModelContextLimit(s.Model)
	}
	broadcastDashboard(map[string]any{
		"type":   "context_pressure",
		"id":     s.ID,
		"level":  crossed,
		"pct":    pct,
		"tokens": s.LastContextTokens,
		"limit":  limit,
	})
}

// resetContextWarn clears the "already warned" flags when a compaction is
// detected — after context drops, we want to warn again if it climbs back.
// (Compaction detection is #42.2, deferred; this hook stays wired for it.)
func resetContextWarn(sessionID string) {
	delete(contextWarnState, sessionID)
}

// ─── #42.7 Session health score ─────────────────────────────────────────

// A rolling window of the last N tool calls per session, kept small on
// purpose — we only need to detect loops, not remember history.
const (
	recentToolCap    = 20 // per-session ring
	recentFailureCap = 20
	loopThreshold    = 5              // same (tool, first_arg) N times in a row = loop
	failureWindow    = 10 * time.Minute
	stallWindow      = 5 * time.Minute
)

// ToolCallRecord is a compact fingerprint of a tool invocation — just
// enough to detect a loop. `Fingerprint` normalises the first argument
// so a Read of the same file counts as a repeat even if the JSON key
// order differs.
type ToolCallRecord struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	At          time.Time `json:"at"`
}

// FailureRecord tracks a PostToolUseFailure timestamp so we can count
// recent failures in the last N minutes.
type FailureRecord struct {
	Tool string    `json:"tool"`
	At   time.Time `json:"at"`
}

// firstArgOf returns a stable identifier for the "first meaningful arg"
// of a tool call. For Read/Edit/Write it's file_path, for Bash it's the
// command, for Grep it's pattern. Anything else falls back to a JSON of
// the whole input truncated to 80 chars.
func firstArgOf(tool string, input map[string]any) string {
	if input == nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := input[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	switch tool {
	case "Read", "Edit", "MultiEdit", "Write", "NotebookEdit":
		return pick("file_path", "path", "notebook_path")
	case "Bash":
		return pick("command")
	case "Grep":
		return pick("pattern")
	case "Glob":
		return pick("pattern")
	case "WebFetch":
		return pick("url")
	case "WebSearch":
		return pick("query")
	}
	// Fallback: shove the whole input into a stable string. Not perfect
	// but good enough for loop detection when we don't know the schema.
	var b strings.Builder
	for k, v := range input {
		b.WriteString(k)
		b.WriteByte('=')
		if s, ok := v.(string); ok {
			b.WriteString(s)
		}
		b.WriteByte(';')
		if b.Len() > 80 {
			break
		}
	}
	return b.String()
}

// appendToolCall pushes a new record onto the recent-tool ring, dropping
// the oldest entry when the cap is hit. Called from PreToolUse in hooks.go.
func (s *Session) appendToolCall(tool string, input map[string]any) {
	rec := ToolCallRecord{
		Name:        tool,
		Fingerprint: firstArgOf(tool, input),
		At:          time.Now(),
	}
	s.mu.Lock()
	s.RecentToolCalls = append(s.RecentToolCalls, rec)
	if len(s.RecentToolCalls) > recentToolCap {
		s.RecentToolCalls = s.RecentToolCalls[len(s.RecentToolCalls)-recentToolCap:]
	}
	s.mu.Unlock()
}

// appendFailure records a PostToolUseFailure timestamp. Called from hooks.go.
func (s *Session) appendFailure(tool string) {
	rec := FailureRecord{Tool: tool, At: time.Now()}
	s.mu.Lock()
	s.RecentFailures = append(s.RecentFailures, rec)
	if len(s.RecentFailures) > recentFailureCap {
		s.RecentFailures = s.RecentFailures[len(s.RecentFailures)-recentFailureCap:]
	}
	s.mu.Unlock()
}

// detectLoop returns true if the most recent N tool calls all share the
// same (name, fingerprint) — a strong signal that Claude is stuck.
func detectLoop(calls []ToolCallRecord) (bool, string) {
	if len(calls) < loopThreshold {
		return false, ""
	}
	last := calls[len(calls)-1]
	if last.Fingerprint == "" {
		// No fingerprint = can't reliably detect a loop.
		return false, ""
	}
	for i := len(calls) - loopThreshold; i < len(calls); i++ {
		if calls[i].Name != last.Name || calls[i].Fingerprint != last.Fingerprint {
			return false, ""
		}
	}
	arg := last.Fingerprint
	if len(arg) > 40 {
		arg = arg[:37] + "…"
	}
	return true, "looping on " + last.Name + " " + arg
}

// countRecentFailures returns how many failures happened in the last window.
func countRecentFailures(failures []FailureRecord, window time.Duration) int {
	cut := time.Now().Add(-window)
	n := 0
	for _, f := range failures {
		if f.At.After(cut) {
			n++
		}
	}
	return n
}

// computeHealthLocked is the lock-free core of the health calculation.
// The caller must have already snapshotted the fields it reads from
// Session (this lets callers that already hold s.mu — e.g. toJSON —
// avoid a re-lock deadlock). Penalty budget:
//   -20  loop detected (same tool + first-arg ≥5 times in a row)
//   -20  >3 PostToolUseFailure in last 10 min
//   -10  context pressure >90%
//   -10  running but no activity in >5 min (stall)
//   -10  paused_over_budget
// Reason returned is the FIRST penalty that fires — biggest driver first.
func computeHealthLocked(
	calls []ToolCallRecord,
	fails []FailureRecord,
	updatedAt time.Time,
	status string,
	lastContext int64,
	model string,
) (int, string) {
	score := 100
	reason := "healthy"

	if loop, why := detectLoop(calls); loop {
		score -= 20
		if reason == "healthy" {
			reason = why
		}
	}

	if failN := countRecentFailures(fails, failureWindow); failN > 3 {
		score -= 20
		if reason == "healthy" {
			reason = "3+ tool failures in the last 10 min"
		}
	}

	if lastContext > 0 && float64(lastContext)/float64(contextLimitFor(model)) > 0.9 {
		score -= 10
		if reason == "healthy" {
			reason = "context >90%, compaction imminent"
		}
	}

	if status == "running" && !updatedAt.IsZero() && time.Since(updatedAt) > stallWindow {
		score -= 10
		if reason == "healthy" {
			reason = "running but no activity in >5 min"
		}
	}

	if status == statusPausedOverBudget {
		score -= 10
		if reason == "healthy" {
			reason = "paused over budget"
		}
	}

	if score < 0 {
		score = 0
	}
	return score, reason
}

// computeHealth is the convenience wrapper for callers that don't
// already hold s.mu. Snapshots the required fields, releases, computes.
func computeHealth(s *Session) (int, string) {
	s.mu.Lock()
	calls := append([]ToolCallRecord(nil), s.RecentToolCalls...)
	fails := append([]FailureRecord(nil), s.RecentFailures...)
	updatedAt := s.UpdatedAt
	status := s.Status
	lastContext := s.LastContextTokens
	model := s.Model
	s.mu.Unlock()
	return computeHealthLocked(calls, fails, updatedAt, status, lastContext, model)
}
