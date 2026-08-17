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

// startTranscriptWatcher polls the transcript file for the session and sums
// per-turn usage into the session totals. Line parsing is delegated to the
// session's CLIAdapter — see ParseTranscriptLine on the adapter for the
// per-CLI schema (#46 Phase 1).
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

		// A transcript exists, so there is something to render. Opening
		// here rather than at spawn keeps a session that never produces
		// one from leaving an empty document behind.
		projector := openSessionProjector(s)
		if projector != nil {
			defer projector.Close()
		}

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
			var addedThinkChars, addedTextChars, addedToolChars uint64
			var addedMsgs int
			var read int64
			// #42.1 harness telemetry — set by the LAST usage-bearing line
			// we see this tick. Written through to Session below.
			var lastCtx int64
			var lastModel string
			// #46 Route per-line parsing through the session's CLIAdapter.
			// Snapshotting once per tick is fine: the adapter is stateless
			// and s.CLI is immutable-after-publish.
			adapter := s.adapter()
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
				if len(strings.TrimSpace(string(line))) > 0 && adapter != nil {
					// Projection is additive: it must never interfere with
					// the usage accounting below, which predates it and
					// feeds the pressure gauge.
					if projector != nil {
						if parts, perr := adapter.ProjectTranscriptLine(line); perr == nil && len(parts) > 0 {
							projector.Ingest(parts)
						}
					}
					ev, perr := adapter.ParseTranscriptLine(line)
					if perr == nil && ev.HasUsage {
						addedIn += int64(ev.InputTokens)
						addedOut += int64(ev.OutputTokens)
						addedCR += int64(ev.CacheReadTokens)
						addedCC += int64(ev.CacheCreationTokens)
						addedThinkChars += ev.ThinkingChars
						addedTextChars += ev.TextChars
						addedToolChars += ev.ToolChars
						addedMsgs++
						// #42.1 track LAST turn's total context + model,
						// separate from cumulative counters. A turn's
						// context is uncached + cache-read + cache-create.
						lastCtx = int64(ev.InputTokens + ev.CacheReadTokens + ev.CacheCreationTokens)
						if ev.Model != "" {
							lastModel = ev.Model
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
				s.OutputThinkingChars += addedThinkChars
				s.OutputTextChars += addedTextChars
				s.OutputToolChars += addedToolChars
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
	in, out, cr, cc, _, _, _, ok = extractUsageAndChars(line)
	return
}

// extractUsageAndChars is extractUsage extended with a per-block-type
// character split of the assistant turn's `message.content[]`. #38.
//
// Correctness note: `usage` may live at three locations (top-level, under
// `message`, or under `response`), but the typed `content` array only
// lives under `message`. To avoid double-counting when a single JSONL
// line contains both a top-level `usage` AND a nested `message.usage`,
// we walk `message.content[]` at most once per line — keyed to whichever
// usage location we picked — and never fall back to scanning other
// branches for content.
func extractUsageAndChars(line []byte) (in, out, cr, cc int64, thinkCh, textCh, toolCh uint64, ok bool) {
	var v map[string]any
	if err := json.Unmarshal(line, &v); err != nil {
		return 0, 0, 0, 0, 0, 0, 0, false
	}
	u := findUsage(v)
	if u == nil {
		return 0, 0, 0, 0, 0, 0, 0, false
	}
	in = getI64(u, "input_tokens")
	out = getI64(u, "output_tokens")
	cr = getI64(u, "cache_read_input_tokens")
	cc = getI64(u, "cache_creation_input_tokens")
	// Chars only come from message.content[]. If we don't see one, we
	// still return the usage — chars just stay zero, matching the
	// "old-transcript / no content array" case in the issue.
	if m, mok := v["message"].(map[string]any); mok {
		thinkCh, textCh, toolCh = sumContentChars(m["content"])
	}
	ok = true
	return
}

// sumContentChars walks a message.content[] array and sums the character
// length of each block by type. Unknown block types contribute zero.
//   - "thinking": len(block.thinking)
//   - "text":     len(block.text)
//   - "tool_use": len(json(block.input))  — the input object is what the
//     model actually generated; JSON-encoding it approximates the token
//     surface. Falls back to zero if input is missing/unmarshalable.
func sumContentChars(raw any) (think, text, tool uint64) {
	arr, ok := raw.([]any)
	if !ok {
		return 0, 0, 0
	}
	for _, item := range arr {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		bt, _ := block["type"].(string)
		switch bt {
		case "thinking":
			if s, ok := block["thinking"].(string); ok {
				think += uint64(len(s))
			}
		case "text":
			if s, ok := block["text"].(string); ok {
				text += uint64(len(s))
			}
		case "tool_use":
			if input, ok := block["input"]; ok && input != nil {
				if b, err := json.Marshal(input); err == nil {
					tool += uint64(len(b))
				}
			}
		}
	}
	return
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
