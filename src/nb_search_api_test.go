package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #58 — the query surface named in the issue:
//
//	GET /api/search?q=...&kind=prompt|output|tool&cli=claude&since=...
//
// It returns enough to render a result row without opening the notebook:
// which notebook, which cell, its state, the turn's prompt, and where in
// the cell the match sits.

func searchRequest(t *testing.T, url string) (*httptest.ResponseRecorder, searchResults) {
	t.Helper()
	rec := httptest.NewRecorder()
	handleSearch(rec, httptest.NewRequest(http.MethodGet, url, nil))
	var res searchResults
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode %s: %v (%s)", url, err, rec.Body.String())
		}
	}
	return rec, res
}

func seedSearchAPINotebooks(t *testing.T) {
	t.Helper()
	seedNotebook(t, "api-claude", NotebookMeta{CLI: "claude", SessionID: "sess-1"}, []Cell{{
		ID: "cell-a", Type: CellPrompt, State: CellOK,
		Source: "rewrite the quokka projector",
		Outputs: []Output{
			{Type: OutputText, Text: "the quokka projector now folds parts into cells"},
			{Type: OutputToolCall, Data: map[string]any{
				"name": "Bash", "input": map[string]any{"command": "go test ./src -run Quokka"},
			}},
		},
	}})
	seedNotebook(t, "api-codex", NotebookMeta{CLI: "codex", SessionID: "sess-2"}, []Cell{{
		ID: "cell-b", Type: CellPrompt, State: CellError, Source: "ask the quokka about codex",
	}})
}

func TestHandleSearch_ReturnsEnoughToRenderARow(t *testing.T) {
	withTempNotebooks(t)
	seedSearchAPINotebooks(t)

	rec, res := searchRequest(t, "/api/search?q=quokka")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(res.Groups) != 2 {
		t.Fatalf("results are grouped by notebook; got %d groups", len(res.Groups))
	}
	var claude *searchGroup
	for i := range res.Groups {
		if res.Groups[i].Notebook == "api-claude" {
			claude = &res.Groups[i]
		}
	}
	if claude == nil {
		t.Fatal("api-claude is missing from the results")
	}
	if claude.CLI != "claude" || claude.SessionID != "sess-1" {
		t.Errorf("group lost its session identity: %+v", claude)
	}
	var promptHit *searchHit
	for i := range claude.Hits {
		if claude.Hits[i].Kind == searchKindPrompt {
			promptHit = &claude.Hits[i]
		}
	}
	if promptHit == nil {
		t.Fatal("no prompt hit")
	}
	if promptHit.CellID != "cell-a" || promptHit.CellIndex != 1 {
		t.Errorf("hit does not locate the cell: %+v", promptHit)
	}
	if promptHit.State != string(CellOK) {
		t.Errorf("hit state = %q, want ok — a result row shows how the turn ended", promptHit.State)
	}
	if promptHit.Snippet == "" {
		t.Error("hit carries no snippet, so the row has nothing to show")
	}
	if promptHit.OutputIndex != -1 {
		t.Errorf("a match in the cell's own source must say so; outputIndex = %d", promptHit.OutputIndex)
	}
}

func TestHandleSearch_FiltersByKindAndCLI(t *testing.T) {
	withTempNotebooks(t)
	seedSearchAPINotebooks(t)

	_, res := searchRequest(t, "/api/search?q=quokka&kind=output")
	if res.Count != 1 {
		t.Fatalf("kind=output returned %d hits, want 1", res.Count)
	}
	if res.Groups[0].Hits[0].Kind != searchKindOutput {
		t.Errorf("kind=output returned a %s hit", res.Groups[0].Hits[0].Kind)
	}

	// Repeated and comma-separated both work, because a UI with three
	// checkboxes will send one of them and it should not have to guess.
	// Two rows: one turn in each notebook, the claude one matching on both
	// its prompt and its reply.
	for _, path := range []string{
		"/api/search?q=quokka&kind=prompt&kind=output",
		"/api/search?q=quokka&kind=prompt,output",
	} {
		_, res = searchRequest(t, path)
		if res.Count != 2 {
			t.Errorf("%s returned %d rows, want 2", path, res.Count)
		}
	}

	_, res = searchRequest(t, "/api/search?q=quokka&cli=codex")
	if res.Count != 1 || res.Groups[0].Notebook != "api-codex" {
		t.Errorf("cli=codex returned %d hits in %d groups", res.Count, len(res.Groups))
	}
}

func TestHandleSearch_AcceptsSinceAsDurationOrTimestamp(t *testing.T) {
	withTempNotebooks(t)
	seedSearchAPINotebooks(t)

	// Everything just written is inside the window either way.
	for _, since := range []string{"24h", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)} {
		_, res := searchRequest(t, "/api/search?q=quokka&since="+since)
		if res.Count == 0 {
			t.Errorf("since=%s hid results that are minutes old", since)
		}
	}
	// A window that closed before anything happened returns nothing rather
	// than everything: a filter nobody applied is worse than no filter.
	_, res := searchRequest(t, "/api/search?q=quokka&since="+time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	if res.Count != 0 {
		t.Errorf("since=<future> returned %d hits", res.Count)
	}
}

func TestHandleSearch_RejectsWhatItCannotAnswer(t *testing.T) {
	withTempNotebooks(t)

	if rec, _ := searchRequest(t, "/api/search"); rec.Code != http.StatusBadRequest {
		t.Errorf("no query returned %d, want 400", rec.Code)
	}
	if rec, _ := searchRequest(t, "/api/search?q=x&kind=nonsense"); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown kind returned %d, want 400 — silently ignoring it answers a different question", rec.Code)
	}
	if rec, _ := searchRequest(t, "/api/search?q=x&since=lunchtime"); rec.Code != http.StatusBadRequest {
		t.Errorf("unparseable since returned %d, want 400", rec.Code)
	}
	rec := httptest.NewRecorder()
	handleSearch(rec, httptest.NewRequest(http.MethodPost, "/api/search?q=x", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST returned %d, want 405", rec.Code)
	}
}

// The index is derived from the log, so deleting the notebook has to delete
// it too — otherwise search keeps offering rows that open a document that
// is no longer there.
func TestDeletingANotebookRemovesItsSearchIndex(t *testing.T) {
	dir := withTempNotebooks(t)
	seedNotebook(t, "doomed", NotebookMeta{}, []Cell{
		{ID: "c1", Type: CellPrompt, State: CellOK, Source: "a question about wombats"},
	})
	if res := mustSearch(t, searchQuery{Text: "wombats"}); res.Count != 1 {
		t.Fatalf("setup: found %d", res.Count)
	}

	rec := httptest.NewRecorder()
	handleNotebookByID(rec, httptest.NewRequest(http.MethodDelete, "/api/nb/doomed", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "doomed"+searchIndexSuffix)); !os.IsNotExist(err) {
		t.Errorf("the index outlived its log: %v", err)
	}
	if res := mustSearch(t, searchQuery{Text: "wombats"}); res.Count != 0 {
		t.Errorf("a deleted notebook is still returning %d hits", res.Count)
	}
}
