package main

// nb_search_api.go — GET /api/search. #58.
//
// One read-only endpoint, shaped exactly as the issue specifies it. Like
// every handler in nb_api.go it is a thin translation: the parsing and the
// refusals live here, and everything that decides an answer lives in
// nb_search.go.
//
// It refuses what it cannot answer rather than approximating it. A `kind`
// this build does not model, or a `since` it cannot parse, is dropped
// silently by most search APIs — and a filter that was silently ignored
// answers a different question than the one that was asked, while looking
// exactly like an answer to it.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func registerSearchRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/search", handleSearch)
}

func handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()

	text := strings.TrimSpace(q.Get("q"))
	if text == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}

	// kind repeats or comma-separates. A UI with three checkboxes will send
	// one of those two and should not have to know which we prefer.
	var kinds []string
	for _, raw := range q["kind"] {
		for _, k := range strings.Split(raw, ",") {
			k = strings.TrimSpace(strings.ToLower(k))
			if k == "" {
				continue
			}
			if !validSearchKind(k) {
				http.Error(w, "unknown kind "+k, http.StatusBadRequest)
				return
			}
			kinds = append(kinds, k)
		}
	}

	var since time.Time
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		t, err := parseSince(s, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		since = t
	}

	limit := 0
	if s := strings.TrimSpace(q.Get("limit")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
			return
		}
		limit = n
	}

	res, err := searchNotebooks(searchQuery{
		Text:  text,
		Kinds: kinds,
		CLI:   strings.TrimSpace(q.Get("cli")),
		Since: since,
		Limit: limit,
	})
	if err != nil {
		http.Error(w, "search: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// parseSince accepts the two things a person actually types: a window ("2h",
// "72h") and an instant ("2026-08-18", or a full RFC 3339 timestamp). A bare
// date is read as midnight UTC, which is the reading that includes the whole
// of that day rather than half of it.
func parseSince(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			d = -d
		}
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errBadSince
}

var errBadSince = errors.New("since must be a duration like 24h, or a date like 2026-08-18")
