package main

import (
	"log"
	"sync"
	"syscall"
	"time"
)

// #35 cost cap — server-side enforcement.
//
// Rates mirror `estimateCost` in src/static/core.js:
//   Input        $3    / M tokens
//   Output       $15   / M tokens
//   Cache read   $0.30 / M tokens
//   Cache write  $3.75 / M tokens
//
// We don't currently distinguish per-model rates on the server — Session
// doesn't carry the model — so we mirror the client's single-rate estimate.
// Opus rates are the safe over-estimate should we ever want to switch.
const (
	costRateInputPerM       = 3.0
	costRateOutputPerM      = 15.0
	costRateCacheReadPerM   = 0.30
	costRateCacheCreatePerM = 3.75
)

// Session status string used when SIGSTOPped over budget.
const statusPausedOverBudget = "paused_over_budget"

// EstimatedCostUSD returns the current estimated cost of s in USD.
// Mirrors client-side estimateCost() so warning-thresholds align.
func (s *Session) EstimatedCostUSD() float64 {
	s.mu.Lock()
	in := s.InputTokens
	out := s.OutputTokens
	cr := s.CacheReadTokens
	cc := s.CacheCreationTokens
	s.mu.Unlock()
	return (float64(in)*costRateInputPerM +
		float64(out)*costRateOutputPerM +
		float64(cr)*costRateCacheReadPerM +
		float64(cc)*costRateCacheCreatePerM) / 1_000_000.0
}

// One warning per session — after the first cost_warning fires we don't
// spam the dashboard on every hook event.
var costWarnFired sync.Map // key: session id → struct{}

// enforceCostCaps runs at the tail of handleHook after all state updates.
// Emits a cost_warning event when the session crosses 80% of its cap,
// and pauses (SIGSTOP) when it exceeds 100%.
func enforceCostCaps(s *Session) {
	s.mu.Lock()
	capUSD := s.CostCapUSD
	status := s.Status
	s.mu.Unlock()
	if capUSD <= 0 {
		return
	}
	cost := s.EstimatedCostUSD()

	if cost >= capUSD && status != statusPausedOverBudget {
		pauseOverBudget(s, cost, capUSD)
		return
	}

	if cost >= 0.8*capUSD {
		if _, already := costWarnFired.LoadOrStore(s.ID, struct{}{}); already {
			return
		}
		broadcastDashboardRaw(map[string]any{
			"type": "cost_warning",
			"id":   s.ID,
			"pct":  cost / capUSD,
			"cost": cost,
			"cap":  capUSD,
		})
	}
}

// pauseOverBudget SIGSTOPs the session's process group and marks it paused.
func pauseOverBudget(s *Session, cost, capUSD float64) {
	c := s.cmd()
	if c != nil && c.Process != nil {
		pid := c.Process.Pid
		if pgid, err := syscall.Getpgid(pid); err == nil {
			if err := syscall.Kill(-pgid, syscall.SIGSTOP); err != nil {
				log.Printf("[%s] costcap: SIGSTOP pgid %d: %v", s.ID, pgid, err)
			}
		} else {
			_ = c.Process.Signal(syscall.SIGSTOP)
		}
	}
	s.appendActivity(ActivityEntry{
		Event:  "CostCapExceeded",
		Detail: "paused over budget",
		Level:  "warn",
	})
	s.setStatus(statusPausedOverBudget, "over budget — paused")
	broadcastDashboardRaw(map[string]any{
		"type": "cost_warning",
		"id":   s.ID,
		"pct":  cost / capUSD,
		"cost": cost,
		"cap":  capUSD,
	})
}

// resumeFromPause SIGCONTs the session and clears its per-session cap.
func resumeFromPause(s *Session) {
	c := s.cmd()
	if c != nil && c.Process != nil {
		pid := c.Process.Pid
		if pgid, err := syscall.Getpgid(pid); err == nil {
			if err := syscall.Kill(-pgid, syscall.SIGCONT); err != nil {
				log.Printf("[%s] costcap: SIGCONT pgid %d: %v", s.ID, pgid, err)
			}
		} else {
			_ = c.Process.Signal(syscall.SIGCONT)
		}
	}
	s.mu.Lock()
	s.CostCapUSD = 0
	s.mu.Unlock()
	costWarnFired.Delete(s.ID)
	s.appendActivity(ActivityEntry{
		Event:  "CostCapResumed",
		Detail: "resumed by user (cap cleared)",
		Level:  "info",
	})
	s.setStatus("running", "resumed")
}

// ─── Hourly window tracking ──────────────────────────────────────────
//
// We keep per-session snapshots of the total estimated cost at the last
// sample point, plus a rolling 60-minute list of (t, delta) increments.
// Every 30s the poller samples every session, sums the delta since the
// last sample, and appends to the window. handleAgents POST consults
// hourlyCostTotal() before spawning.
var (
	hourlyMu     sync.Mutex
	lastSample   = map[string]float64{} // sid → last observed EstimatedCostUSD
	hourlyBucket []hourlySample
)

type hourlySample struct {
	At   time.Time
	Cost float64
}

func hourlyCostTotal() float64 {
	hourlyMu.Lock()
	defer hourlyMu.Unlock()
	cutoff := time.Now().Add(-1 * time.Hour)
	sum := 0.0
	pruned := hourlyBucket[:0]
	for _, s := range hourlyBucket {
		if s.At.After(cutoff) {
			pruned = append(pruned, s)
			sum += s.Cost
		}
	}
	hourlyBucket = pruned
	return sum
}

// sampleHourly walks every session, computes the delta since we last
// looked, and records it. Called every 30s from a goroutine kicked off
// in main().
func sampleHourly() {
	registryMu.RLock()
	sessions := make([]*Session, 0, len(registry))
	for _, s := range registry {
		sessions = append(sessions, s)
	}
	registryMu.RUnlock()

	now := time.Now()
	hourlyMu.Lock()
	for _, s := range sessions {
		cur := s.EstimatedCostUSD()
		prev := lastSample[s.ID]
		if cur > prev {
			hourlyBucket = append(hourlyBucket, hourlySample{At: now, Cost: cur - prev})
		}
		lastSample[s.ID] = cur
	}
	// Drop removed sessions from lastSample so the map doesn't grow unbounded.
	live := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		live[s.ID] = true
	}
	for id := range lastSample {
		if !live[id] {
			delete(lastSample, id)
		}
	}
	hourlyMu.Unlock()
}

// countSessionsThisHour returns how many distinct sessions logged spend
// in the current hourly window, and how many crossed their per-session
// cap. Used by the dashboard budget strip readout.
func countSessionsThisHour() (nSessions, nOverCap int) {
	registryMu.RLock()
	sessions := make([]*Session, 0, len(registry))
	for _, s := range registry {
		sessions = append(sessions, s)
	}
	registryMu.RUnlock()
	for _, s := range sessions {
		cost := s.EstimatedCostUSD()
		if cost > 0 {
			nSessions++
		}
		s.mu.Lock()
		capUSD := s.CostCapUSD
		s.mu.Unlock()
		if capUSD > 0 && cost >= capUSD {
			nOverCap++
		}
	}
	return nSessions, nOverCap
}

// startHourlyCostBroadcaster ticks every 30s: samples, then broadcasts
// the current hourly_cost event to dashboard subscribers.
func startHourlyCostBroadcaster() {
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			sampleHourly()
			total := hourlyCostTotal()
			n, over := countSessionsThisHour()
			broadcastDashboardRaw(map[string]any{
				"type":     "hourly_cost",
				"cost":     total,
				"sessions": n,
				"overCap":  over,
			})
		}
	}()
}
