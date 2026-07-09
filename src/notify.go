package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"
)

// notify.go — #36 outbound status-transition notifier.
//
// Fires a webhook POST when a session enters one of the "notable" statuses
// below. Rate-limited: at most one webhook per (session, status) pair, so
// bouncing between `running` and `waiting_input` doesn't spam. Retries once
// after 5 s on failure and then gives up — this is best-effort awareness,
// not a queue.
//
// Runs off the hot path in its own goroutine so setStatus never blocks on
// slow HTTP.

var notifyClient = &http.Client{Timeout: 10 * time.Second}

// notableStatuses are the transitions worth notifying about — either they
// require human attention or they mark a session as done.
var notableStatuses = map[string]bool{
	"waiting_input":       true,
	"review_ready":        true, // #37
	"stopped":             true,
	"error":               true,
	"paused_over_budget":  true, // #35
}

var (
	notifySeenMu sync.Mutex
	notifySeen   = make(map[string]bool) // key = sessionID + "|" + status
)

// notifyStatusTransition is called at the end of setStatus. Cheap on the
// caller side — dispatches the actual HTTP in a goroutine.
func notifyStatusTransition(s *Session, status, activity string) {
	if !notableStatuses[status] {
		return
	}
	key := s.ID + "|" + status
	notifySeenMu.Lock()
	if notifySeen[key] {
		notifySeenMu.Unlock()
		return
	}
	notifySeen[key] = true
	notifySeenMu.Unlock()

	url := GetConfig().NotifyWebhookURL
	if url == "" {
		return
	}
	go dispatchWebhook(url, s, status, activity)
}

// notifyReset lets a session forget its "already notified" cache for a given
// status when it leaves that state. E.g. once waiting_input clears back to
// running, the next waiting_input event should notify again.
func notifyReset(sessionID, prev string) {
	if prev == "" || !notableStatuses[prev] {
		return
	}
	notifySeenMu.Lock()
	delete(notifySeen, sessionID+"|"+prev)
	notifySeenMu.Unlock()
}

func dispatchWebhook(url string, s *Session, status, activity string) {
	cost := s.EstimatedCostUSD() // takes s.mu itself
	s.mu.Lock()
	payload := map[string]any{
		"type":     "status",
		"id":       s.ID,
		"agent":    s.ID, // codename is derived from ID hash on the receiver
		"status":   status,
		"activity": activity,
		"cwd":      s.Cwd,
		"cwd_base": filepath.Base(s.Cwd),
		"pr_url":   s.PRURL,
		"pr_title": s.PRTitle,
		"cost_usd": cost,
		"ts":       time.Now().Unix(),
	}
	s.mu.Unlock()

	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("notify: marshal: %v", err)
		return
	}

	if err := postOnce(url, body); err == nil {
		return
	}
	// Retry once after 5 s, then give up.
	time.Sleep(5 * time.Second)
	if err := postOnce(url, body); err != nil {
		log.Printf("notify: webhook %s failed twice: %v", url, err)
	}
}

func postOnce(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "collectif/1 (notify)")
	resp, err := notifyClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return &httpStatusErr{resp.StatusCode}
	}
	return nil
}

type httpStatusErr struct{ code int }

func (e *httpStatusErr) Error() string { return http.StatusText(e.code) }
