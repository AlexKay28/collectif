package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

// hooksReceived counts every hook POST that reaches handleHook, regardless of
// outcome. Exposed via /metrics as collectif_hooks_received_total. See #32.
var hooksReceived atomic.Uint64

// handleHealthz responds with a plain-text 200 for liveness probes. It is
// registered on the mux directly (not under /api/*) and is not covered by
// isProtectedPath, so it bypasses the shared-secret auth gate.
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleMetrics emits a Prometheus text-format exposition of the process-wide
// counters and gauges computed from the in-memory session registry. We do
// not depend on the Prometheus client library: the exposition format is
// stable and hand-formatting keeps the dependency footprint at zero.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	// Snapshot the registry under the read lock, then compute + write outside
	// the lock so a slow HTTP client can't block hook processing.
	registryMu.RLock()
	sessions := make([]*Session, 0, len(registry))
	for _, s := range registry {
		sessions = append(sessions, s)
	}
	registryMu.RUnlock()

	totalAgents := len(sessions)
	statusCounts := map[string]int{}
	var inputTokens, outputTokens int64
	var sessionSubs int
	for _, s := range sessions {
		s.mu.Lock()
		status := s.Status
		inputTokens += s.InputTokens
		outputTokens += s.OutputTokens
		s.mu.Unlock()
		if status == "" {
			status = "unknown"
		}
		statusCounts[status]++

		s.subMu.Lock()
		sessionSubs += len(s.subs)
		s.subMu.Unlock()
	}

	dashMu.Lock()
	dashCount := len(dashSubs)
	dashMu.Unlock()

	hooks := hooksReceived.Load()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	var b strings.Builder

	fmt.Fprintln(&b, "# HELP collectif_agents_total Total number of tracked agent sessions.")
	fmt.Fprintln(&b, "# TYPE collectif_agents_total gauge")
	fmt.Fprintf(&b, "collectif_agents_total %d\n", totalAgents)

	// Deterministic ordering keeps scrape diffs readable.
	statuses := make([]string, 0, len(statusCounts))
	for k := range statusCounts {
		statuses = append(statuses, k)
	}
	sort.Strings(statuses)
	fmt.Fprintln(&b, "# HELP collectif_agents_by_status Agent count partitioned by status.")
	fmt.Fprintln(&b, "# TYPE collectif_agents_by_status gauge")
	for _, st := range statuses {
		fmt.Fprintf(&b, "collectif_agents_by_status{status=%q} %d\n", st, statusCounts[st])
	}

	fmt.Fprintln(&b, "# HELP collectif_hooks_received_total Total number of hook POSTs received.")
	fmt.Fprintln(&b, "# TYPE collectif_hooks_received_total counter")
	fmt.Fprintf(&b, "collectif_hooks_received_total %d\n", hooks)

	fmt.Fprintln(&b, "# HELP collectif_tokens_input_total Sum of InputTokens across all sessions.")
	fmt.Fprintln(&b, "# TYPE collectif_tokens_input_total counter")
	fmt.Fprintf(&b, "collectif_tokens_input_total %d\n", inputTokens)

	fmt.Fprintln(&b, "# HELP collectif_tokens_output_total Sum of OutputTokens across all sessions.")
	fmt.Fprintln(&b, "# TYPE collectif_tokens_output_total counter")
	fmt.Fprintf(&b, "collectif_tokens_output_total %d\n", outputTokens)

	fmt.Fprintln(&b, "# HELP collectif_ws_conns_dashboard Currently connected dashboard websocket subscribers.")
	fmt.Fprintln(&b, "# TYPE collectif_ws_conns_dashboard gauge")
	fmt.Fprintf(&b, "collectif_ws_conns_dashboard %d\n", dashCount)

	fmt.Fprintln(&b, "# HELP collectif_ws_conns_session Sum of per-session websocket subscriber counts.")
	fmt.Fprintln(&b, "# TYPE collectif_ws_conns_session gauge")
	fmt.Fprintf(&b, "collectif_ws_conns_session %d\n", sessionSubs)

	_, _ = w.Write([]byte(b.String()))
}
