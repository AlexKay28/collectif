package main

import (
	"bufio"
	"context"
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"
)

// startTranscriptWatcher polls the JSONL transcript file for the session and
// sums assistant-message token usage into the session totals. The schema is
// defensively parsed: we look for a `usage` object at the top level, under
// `message`, or under `response`, and accept whatever integer fields it holds.
func startTranscriptWatcher(ctx context.Context, s *Session) {
	s.mu.Lock()
	if s.watching || s.TranscriptPath == "" {
		s.mu.Unlock()
		return
	}
	s.watching = true
	path := s.TranscriptPath
	s.mu.Unlock()

	// shouldExit returns true when the session is gone or terminal — used to
	// stop polling for a transcript that will never grow again.
	shouldExit := func() bool {
		if getSession(s.ID) == nil {
			return true
		}
		s.mu.Lock()
		st := s.Status
		s.mu.Unlock()
		return st == "stopped" || st == "error"
	}

	clearWatching := func() {
		s.mu.Lock()
		s.watching = false
		s.mu.Unlock()
	}

	go func() {
		defer clearWatching()

		// Poll until the transcript file appears (Claude may take a moment to
		// create it). Bail out early if the session dies before it exists.
		var f *os.File
		openTicker := time.NewTicker(500 * time.Millisecond)
		for f == nil {
			var err error
			f, err = os.Open(path)
			if err == nil {
				break
			}
			select {
			case <-ctx.Done():
				openTicker.Stop()
				return
			case <-openTicker.C:
			}
			if shouldExit() {
				openTicker.Stop()
				return
			}
		}
		openTicker.Stop()
		defer f.Close()

		var partial []byte
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if shouldExit() {
				return
			}

			s.mu.Lock()
			offset := s.transcriptOffset
			s.mu.Unlock()

			if _, err := f.Seek(offset, 0); err != nil {
				continue
			}
			br := bufio.NewReader(f)
			var addedIn, addedOut, addedCR, addedCC int64
			var addedMsgs int
			var read int64
			// #42.1 harness telemetry — set by the LAST usage-bearing line
			// we see this tick. Written through to Session below.
			var lastCtx int64
			var lastModel string
			for {
				line, err := br.ReadBytes('\n')
				read += int64(len(line))
				if len(line) > 0 && line[len(line)-1] != '\n' {
					// Partial trailing line — remember for next round.
					partial = append(partial[:0], line...)
					read -= int64(len(line))
					break
				}
				partial = partial[:0]
				if len(strings.TrimSpace(string(line))) > 0 {
					if in, out, cr, cc, ok := extractUsage(line); ok {
						addedIn += in
						addedOut += out
						addedCR += cr
						addedCC += cc
						addedMsgs++
						// #42.1 track LAST turn's total context + model,
						// separate from cumulative counters. A turn's
						// context is uncached + cache-read + cache-create.
						lastCtx = in + cr + cc
						if m := extractModel(line); m != "" {
							lastModel = m
						}
					}
				}
				if err != nil {
					break
				}
			}

			if read > 0 {
				s.mu.Lock()
				s.transcriptOffset += read
				s.InputTokens += addedIn
				s.OutputTokens += addedOut
				s.CacheReadTokens += addedCR
				s.CacheCreationTokens += addedCC
				s.MessageCount += addedMsgs
				// #42.1 write through the last-turn snapshot. Only replace
				// if we actually observed a new value this tick so a tick
				// with no usage lines doesn't zero out the field.
				if lastCtx > 0 {
					s.LastContextTokens = lastCtx
				}
				if lastModel != "" {
					s.Model = lastModel
				}
				s.mu.Unlock()
				if addedMsgs > 0 {
					// Broadcast context warning after unlocking so the
					// downstream serializer isn't waiting on us.
					maybeBroadcastContextPressure(s)
					s.touch()
				}
			}
		}
	}()
	log.Printf("[%s] transcript watch: %s", s.ID, path)
}

// extractUsage walks a decoded JSON line and returns tokens if a `usage`
// object is present anywhere in a small set of well-known locations.
func extractUsage(line []byte) (in, out, cr, cc int64, ok bool) {
	var v map[string]any
	if err := json.Unmarshal(line, &v); err != nil {
		return 0, 0, 0, 0, false
	}
	if u := findUsage(v); u != nil {
		return getI64(u, "input_tokens"),
			getI64(u, "output_tokens"),
			getI64(u, "cache_read_input_tokens"),
			getI64(u, "cache_creation_input_tokens"),
			true
	}
	return 0, 0, 0, 0, false
}

// extractModel pulls the model id from a transcript line. Anthropic puts
// it at message.model on assistant turns; older/newer shapes vary so we
// probe a small set of well-known locations. #42.1.
func extractModel(line []byte) string {
	var v map[string]any
	if err := json.Unmarshal(line, &v); err != nil {
		return ""
	}
	if s, ok := v["model"].(string); ok && s != "" {
		return s
	}
	if m, ok := v["message"].(map[string]any); ok {
		if s, ok := m["model"].(string); ok && s != "" {
			return s
		}
	}
	if r, ok := v["response"].(map[string]any); ok {
		if s, ok := r["model"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func findUsage(v map[string]any) map[string]any {
	if u, ok := v["usage"].(map[string]any); ok {
		return u
	}
	// message.usage
	if m, ok := v["message"].(map[string]any); ok {
		if u, ok := m["usage"].(map[string]any); ok {
			return u
		}
	}
	// response.usage
	if r, ok := v["response"].(map[string]any); ok {
		if u, ok := r["usage"].(map[string]any); ok {
			return u
		}
	}
	return nil
}

func getI64(m map[string]any, k string) int64 {
	switch x := m[k].(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
}
