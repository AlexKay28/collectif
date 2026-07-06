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

	go func() {
		var partial []byte
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}

			s.mu.Lock()
			offset := s.transcriptOffset
			s.mu.Unlock()

			if _, err := f.Seek(offset, 0); err != nil {
				_ = f.Close()
				continue
			}
			br := bufio.NewReader(f)
			var addedIn, addedOut, addedCR, addedCC int64
			var addedMsgs int
			var read int64
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
					}
				}
				if err != nil {
					break
				}
			}
			_ = f.Close()

			if read > 0 {
				s.mu.Lock()
				s.transcriptOffset += read
				s.InputTokens += addedIn
				s.OutputTokens += addedOut
				s.CacheReadTokens += addedCR
				s.CacheCreationTokens += addedCC
				s.MessageCount += addedMsgs
				s.mu.Unlock()
				if addedMsgs > 0 {
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
