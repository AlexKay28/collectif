package main

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// #48 M0. These tests pin the two defects the ADR surfaced:
//   1. the model -> context-window table was stale, so current 1M-window
//      models were treated as 200k and the pressure gauge read ~5x high;
//   2. contextWarnState was an unsynchronised package-level map written
//      from one transcript-watcher goroutine per session.

func TestModelContextLimit_CurrentModels(t *testing.T) {
	a := getAdapter("claude")
	if a == nil {
		t.Fatal("claude adapter not registered")
	}

	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-5", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-fable-5", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4-7", 1_000_000},
		{"claude-opus-4-6", 1_000_000},
		{"claude-sonnet-4-6", 1_000_000},
		{"claude-haiku-4-5", 200_000},
	}
	for _, c := range cases {
		if got := a.ModelContextLimit(c.model); got != c.want {
			t.Errorf("ModelContextLimit(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// Dated snapshots must resolve to the same window as their alias — the API
// accepts both spellings and a session can report either.
func TestModelContextLimit_DatedSnapshotResolves(t *testing.T) {
	a := getAdapter("claude")
	if got := a.ModelContextLimit("claude-haiku-4-5-20251001"); got != 200_000 {
		t.Errorf("dated haiku snapshot = %d, want 200000", got)
	}
	if got := a.ModelContextLimit("claude-opus-4-7-20260115"); got != 1_000_000 {
		t.Errorf("dated opus snapshot = %d, want 1000000", got)
	}
}

// An unknown model is more likely an older, smaller-window one, so the
// fallback stays conservative: warning early beats staying silent.
func TestModelContextLimit_UnknownFallsBackConservatively(t *testing.T) {
	a := getAdapter("claude")
	if got := a.ModelContextLimit("some-unreleased-model"); got != defaultContextLimit {
		t.Errorf("unknown model = %d, want %d", got, defaultContextLimit)
	}
	if got := a.ModelContextLimit(""); got != defaultContextLimit {
		t.Errorf("empty model = %d, want %d", got, defaultContextLimit)
	}
}

// The bug in the ADR: 200k tokens on a 1M-window model is 20% used, and
// must not trip the 70% warning.
func TestContextPressure_NoFalseAlarmOnLargeWindowModel(t *testing.T) {
	t.Cleanup(func() { resetContextWarn("m0-nofalse") })

	s := newSession("m0-nofalse", "sid", "/tmp", "")
	s.Model = "claude-opus-5"
	s.LastContextTokens = 200_000

	if pct := contextUsedPct(s); pct < 0.19 || pct > 0.21 {
		t.Fatalf("contextUsedPct = %.3f, want ~0.20", pct)
	}
	if level := maybeBroadcastContextPressure(s); level != "" {
		t.Fatalf("crossed %q at 20%% usage, want no broadcast", level)
	}
}

func TestContextPressure_CrossesEachThresholdOnce(t *testing.T) {
	t.Cleanup(func() { resetContextWarn("m0-thresholds") })

	s := newSession("m0-thresholds", "sid", "/tmp", "")
	s.Model = "claude-opus-5"

	s.LastContextTokens = 750_000 // 75%
	if level := maybeBroadcastContextPressure(s); level != "warn" {
		t.Fatalf("at 75%% got %q, want \"warn\"", level)
	}
	if level := maybeBroadcastContextPressure(s); level != "" {
		t.Fatalf("re-broadcast at 75%%: got %q, want \"\"", level)
	}

	s.LastContextTokens = 950_000 // 95%
	if level := maybeBroadcastContextPressure(s); level != "critical" {
		t.Fatalf("at 95%% got %q, want \"critical\"", level)
	}
	if level := maybeBroadcastContextPressure(s); level != "" {
		t.Fatalf("re-broadcast at 95%%: got %q, want \"\"", level)
	}
}

// One transcript watcher goroutine per session all write this map. Run
// under -race: concurrent map writes are a fatal runtime throw, not a
// recoverable panic.
func TestContextWarnState_ConcurrentSessions(t *testing.T) {
	const goroutines = 8
	const perGoroutine = 50

	sessions := make([]*Session, 0, goroutines)
	for i := 0; i < goroutines; i++ {
		id := fmt.Sprintf("m0-race-%d", i)
		s := newSession(id, "sid", "/tmp", "")
		s.Model = "claude-opus-5"
		s.LastContextTokens = 800_000 // 80% — above the warn threshold
		sessions = append(sessions, s)
		t.Cleanup(func() { resetContextWarn(id) })
	}

	var wg sync.WaitGroup
	for _, s := range sessions {
		wg.Add(1)
		go func(s *Session) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				maybeBroadcastContextPressure(s)
				resetContextWarn(s.ID)
			}
		}(s)
	}
	wg.Wait()
}

// computeHealthLocked must not reach back into a package-level table for
// the limit — it takes an already-resolved one, so a 1M-window session at
// 20% is not docked for imminent compaction.
func TestComputeHealth_UsesResolvedLimit(t *testing.T) {
	score, reason := computeHealthLocked(nil, nil, time.Time{}, "running", 200_000, 1_000_000)
	if score != 100 {
		t.Errorf("score = %d, want 100 (20%% of a 1M window is not pressure); reason=%q", score, reason)
	}

	score, reason = computeHealthLocked(nil, nil, time.Time{}, "running", 950_000, 1_000_000)
	if score != 90 {
		t.Errorf("score = %d, want 90 at 95%% context; reason=%q", score, reason)
	}
	if reason != "context >90%, compaction imminent" {
		t.Errorf("reason = %q, want the context-pressure reason", reason)
	}
}
