package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// #51 M2.5 — the gate itself.
//
// Everything else in this phase is structure that can be proven offline:
// deterministic rendering, breakpoint placement, the lookback stride, the
// metric. None of it answers the question the gate exists to ask, which is
// whether the cache actually *lands* against the real API. That needs a
// key, so this test skips loudly rather than pretending.
//
// Run it with credentials present:
//
//	ANTHROPIC_API_KEY=... go test ./src -run TestLive_CachePays -v
//
// It costs a handful of real tokens. The pass condition is the one written
// into #51: re-running the last cell of a ten-cell notebook reports
// non-zero cache reads and a materially cheaper prompt than the first run.
func TestLive_CachePaysOnASecondRun(t *testing.T) {
	if !anthropicCredentialsPresent() {
		t.Skip("no Anthropic credentials — this is the one M2.5 check that cannot run offline; " +
			"set ANTHROPIC_API_KEY and re-run to close the gate")
	}

	provider := newAnthropicProvider()

	// A prefix big enough to be worth caching. The minimum cacheable prefix
	// is 512 tokens on claude-opus-5 and 1024 on most others; below that
	// caching silently does nothing at all, which would make a correct
	// implementation look broken.
	filler := strings.Repeat(
		"This is context from an earlier cell in the notebook. It exists to make the prefix long enough to cache. ",
		60)

	var msgs []Message
	for i := 0; i < 9; i++ {
		msgs = append(msgs, userText(filler))
	}
	msgs = append(msgs, userText("Reply with exactly the word: ok"))

	req := Request{
		Model:                anthropicDefaultModel,
		System:               "You are running inside a collectif notebook. Answer tersely.",
		Messages:             msgs,
		Tools:                toolSpecs(),
		MaxTokens:            64,
		StablePrefixMessages: len(msgs),
	}

	runOnce := func(label string) Usage {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		stream, err := provider.Stream(ctx, req)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer stream.Close()
		for {
			// Distinguish a finished stream from a failed one. Treating a
			// 401 or a 429 as EOF would return zero usage and land on the
			// "prefix is not matching" failure below — the one check that
			// cannot run offline, pointing at the wrong bug.
			if _, err := stream.Next(); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				t.Fatalf("%s: stream failed (this is an API or credential problem, not a cache one): %v", label, err)
			}
		}
		res := stream.Result()
		u := res.Usage
		t.Logf("%s: prompt=%d (uncached=%d, cache_read=%d, cache_write=%d) output=%d hit=%.0f%%",
			label, promptTokens(u), u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens,
			u.OutputTokens, cacheHitRatio(u)*100)
		return u
	}

	first := runOnce("cold run")
	if promptTokens(first) == 0 {
		t.Fatal("no prompt tokens reported — the request never reached the model")
	}
	if first.CacheCreationTokens == 0 {
		t.Errorf("cold run wrote nothing to cache (%+v) — the breakpoints are not taking effect", first)
	}

	// A cache entry only becomes readable once the first response has begun
	// streaming, which it has by now.
	second := runOnce("warm run")

	if second.CacheReadTokens == 0 {
		t.Fatalf("warm run read nothing from cache (%+v) — the projected prefix is not matching between runs, "+
			"which is the failure this gate exists to catch", second)
	}
	if ratio := cacheHitRatio(second); ratio < 0.5 {
		t.Errorf("warm run served only %.0f%% of the prompt from cache, want most of it", ratio*100)
	}
	if second.InputTokens >= first.InputTokens {
		t.Errorf("uncached prompt did not shrink (%d then %d) — the warm run paid full price",
			first.InputTokens, second.InputTokens)
	}
}

// A guard against the gate quietly never running: if credentials are
// present, the live test must not be skipped by some other accident.
func TestLive_GateIsRunnableWhenCredentialsExist(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		t.Skip("no key in the environment")
	}
	if !anthropicCredentialsPresent() {
		t.Error("a key is set but credential detection does not see it — the gate would skip silently")
	}
}
