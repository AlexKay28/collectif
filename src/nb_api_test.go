package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// #49 M1 slice 2. Transport over the document layer: a shared registry so
// two clients fold one document, HTTP for mutations, WS for the stream.

// withTempNotebooks points the notebook directory at a temp dir for one
// test and clears the registry afterwards so package-level state doesn't
// leak between tests.
func withTempNotebooks(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := nbDirFn
	nbDirFn = func() string { return dir }
	t.Cleanup(func() {
		closeAllNotebooks()
		nbDirFn = prev
	})
	return dir
}

// ─── Registry ───────────────────────────────────────────────────────────

func TestNotebookRegistry_SharesOneStorePerSlug(t *testing.T) {
	withTempNotebooks(t)

	created, err := createNotebook("Shared", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	slug := created.slug

	a, err := acquireNotebook(slug)
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	b, err := acquireNotebook(slug)
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if a != b {
		t.Fatal("two acquires returned different stores — one log must have one handle")
	}

	// A write through one handle is visible through the other, because
	// there is only one.
	if _, err := a.Append(evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellMarkdown}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if got := len(b.Doc().Cells); got != 1 {
		t.Errorf("cells via second handle = %d, want 1", got)
	}
}

func TestNotebookRegistry_ReleaseThenAcquireReopens(t *testing.T) {
	withTempNotebooks(t)

	st, err := createNotebook("Reopen me", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	slug := st.slug
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellShell, Source: "ls"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := releaseNotebook(slug); err != nil {
		t.Fatalf("release: %v", err)
	}

	again, err := acquireNotebook(slug)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if got := len(again.Doc().Cells); got != 1 {
		t.Errorf("cells after reopen = %d, want 1", got)
	}
	if _, err := again.Append(evCellDeleted, cellDeletedPayload{CellID: "c1"}); err != nil {
		t.Errorf("reopened store is not writable: %v", err)
	}
}

// The slug becomes a filename. Anything that could walk out of the
// notebooks directory has to be refused before it reaches the filesystem.
func TestValidNotebookSlug_RejectsAnythingThatCouldEscape(t *testing.T) {
	bad := []string{
		"", ".", "..", "../etc", "a/b", `a\b`, "/abs", "with space",
		"..%2f..%2fetc", "nul\x00byte", strings.Repeat("x", 200),
	}
	for _, s := range bad {
		if validNotebookSlug(s) {
			t.Errorf("validNotebookSlug(%q) = true, want false", s)
		}
	}
	good := []string{"notes", "notes-1", "my_notebook", "a1", "2026-08-16-gauge"}
	for _, s := range good {
		if !validNotebookSlug(s) {
			t.Errorf("validNotebookSlug(%q) = false, want true", s)
		}
	}
}

func TestAcquireNotebook_RejectsTraversalBeforeTouchingDisk(t *testing.T) {
	withTempNotebooks(t)
	if _, err := acquireNotebook("../../etc/passwd"); err == nil {
		t.Fatal("expected an error for a traversing slug")
	}
}

// ─── HTTP ───────────────────────────────────────────────────────────────

func nbRequest(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path+"?token=test-token", rdr)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
}

func TestNotebookAPI_CreateThenGet(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	root := t.TempDir()

	rec := nbRequest(t, srv, http.MethodPost, "/api/nb", map[string]any{"title": "Gauge dig", "root": root})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &created)
	if created.ID == "" {
		t.Fatal("create returned no id")
	}

	rec = nbRequest(t, srv, http.MethodGet, "/api/nb/"+created.ID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	var nb Notebook
	decodeJSON(t, rec, &nb)
	if nb.Title != "Gauge dig" {
		t.Errorf("Title = %q", nb.Title)
	}
	if nb.Root != root {
		t.Errorf("Root = %q, want %q", nb.Root, root)
	}
}

func TestNotebookAPI_CreateRejectsRootThatIsNotADirectory(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()

	rec := nbRequest(t, srv, http.MethodPost, "/api/nb", map[string]any{"title": "Bad", "root": "/nonexistent/xyz-collectif"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a root that isn't a directory, got %d", rec.Code)
	}
	rec = nbRequest(t, srv, http.MethodPost, "/api/nb", map[string]any{"title": "Bad", "root": "relative/path"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a relative root, got %d", rec.Code)
	}
}

func TestNotebookAPI_GetUnknownIs404(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	rec := nbRequest(t, srv, http.MethodGet, "/api/nb/never-existed", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d %s", rec.Code, rec.Body.String())
	}
}

// A traversing id must never return notebook content. Two mechanisms get
// us there and both are acceptable: http.ServeMux normalises a literal
// "/api/nb/.." before routing (a redirect that stays on this server), and
// validNotebookSlug rejects encoded attempts that survive to the handler.
// The property under test is the outcome — no traversal yields a document,
// and no redirect points anywhere but back into this server.
func TestNotebookAPI_TraversingIDIsRejected(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	for _, id := range []string{"..", "../..", "..%2f..%2fetc%2fpasswd", "a%2Fb", "%2e%2e"} {
		rec := nbRequest(t, srv, http.MethodGet, "/api/nb/"+id, nil)
		if rec.Code == http.StatusOK {
			t.Errorf("id %q returned 200 — a traversing id must never serve content: %s", id, rec.Body.String())
			continue
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			if !strings.HasPrefix(loc, "/") || strings.Contains(loc, "..") {
				t.Errorf("id %q redirected off-server or unresolved: %q", id, loc)
			}
		}
	}
}

func TestNotebookAPI_CellLifecycle(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()

	rec := nbRequest(t, srv, http.MethodPost, "/api/nb", map[string]any{"title": "Cells", "root": t.TempDir()})
	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &created)
	base := "/api/nb/" + created.ID

	newCell := func(typ, src string) string {
		t.Helper()
		r := nbRequest(t, srv, http.MethodPost, base+"/cells", map[string]any{"type": typ, "source": src})
		if r.Code != http.StatusOK {
			t.Fatalf("insert cell: %d %s", r.Code, r.Body.String())
		}
		var out struct {
			CellID string `json:"cellId"`
		}
		decodeJSON(t, r, &out)
		if out.CellID == "" {
			t.Fatal("insert returned no cellId")
		}
		return out.CellID
	}

	c1 := newCell("markdown", "# One")
	c2 := newCell("shell", "echo two")

	// Edit.
	if r := nbRequest(t, srv, http.MethodPatch, base+"/cells/"+c1, map[string]any{"source": "# Edited"}); r.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", r.Code, r.Body.String())
	}
	// Move c2 in front of c1.
	if r := nbRequest(t, srv, http.MethodPost, base+"/cells/"+c2+"/move", map[string]any{"beforeCellId": c1}); r.Code != http.StatusOK {
		t.Fatalf("move: %d %s", r.Code, r.Body.String())
	}

	rec = nbRequest(t, srv, http.MethodGet, base, nil)
	var nb Notebook
	decodeJSON(t, rec, &nb)
	if got, want := cellIDs(&nb), []string{c2, c1}; !equalStrings(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if got := mustCell(t, &nb, c1).Source; got != "# Edited" {
		t.Errorf("source = %q, want %q", got, "# Edited")
	}

	// Delete.
	if r := nbRequest(t, srv, http.MethodDelete, base+"/cells/"+c2, nil); r.Code != http.StatusOK && r.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.Code, r.Body.String())
	}
	rec = nbRequest(t, srv, http.MethodGet, base, nil)
	nb = Notebook{}
	decodeJSON(t, rec, &nb)
	if got, want := cellIDs(&nb), []string{c1}; !equalStrings(got, want) {
		t.Errorf("after delete = %v, want %v", got, want)
	}
}

func TestNotebookAPI_CellInsertRejectsUnknownType(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	rec := nbRequest(t, srv, http.MethodPost, "/api/nb", map[string]any{"title": "Types", "root": t.TempDir()})
	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rec, &created)

	r := nbRequest(t, srv, http.MethodPost, "/api/nb/"+created.ID+"/cells", map[string]any{"type": "teleport", "source": "x"})
	if r.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unknown cell type, got %d %s", r.Code, r.Body.String())
	}
}

func TestNotebookAPI_ListsNotebooks(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	root := t.TempDir()

	for _, title := range []string{"First", "Second"} {
		if r := nbRequest(t, srv, http.MethodPost, "/api/nb", map[string]any{"title": title, "root": root}); r.Code != http.StatusOK {
			t.Fatalf("create %s: %d", title, r.Code)
		}
	}

	rec := nbRequest(t, srv, http.MethodGet, "/api/nb", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	decodeJSON(t, rec, &list)
	if len(list) != 2 {
		t.Fatalf("listed %d notebooks, want 2: %s", len(list), rec.Body.String())
	}
}

func TestNotebookAPI_RequiresAuth(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	req := httptest.NewRequest(http.MethodGet, "/api/nb", nil) // no token
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a token, got %d", rec.Code)
	}
}

// ─── WebSocket ──────────────────────────────────────────────────────────

func TestNotebookWS_SendsFoldThenTailsLiveEvents(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()

	st, err := createNotebook("Live", t.TempDir())
	if err != nil {
		t.Fatalf("createNotebook: %v", err)
	}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c1", Type: CellMarkdown, Source: "before"}}); err != nil {
		t.Fatalf("seed append: %v", err)
	}

	hs := httptest.NewServer(srv.Router())
	defer hs.Close()

	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/notebook/" + st.slug + "?token=test-token"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// 1. The folded document arrives first, carrying prior state.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var fold struct {
		Type     string    `json:"type"`
		Notebook *Notebook `json:"notebook"`
	}
	if err := conn.ReadJSON(&fold); err != nil {
		t.Fatalf("read fold: %v", err)
	}
	if fold.Type != "fold" {
		t.Fatalf("first message type = %q, want \"fold\"", fold.Type)
	}
	if fold.Notebook == nil || len(fold.Notebook.Cells) != 1 {
		t.Fatalf("fold did not carry prior state: %+v", fold.Notebook)
	}

	// 2. A later append arrives as a live event.
	if _, err := st.Append(evCellInserted, cellInsertedPayload{Cell: Cell{ID: "c2", Type: CellShell, Source: "after"}}); err != nil {
		t.Fatalf("live append: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var live struct {
		Type  string `json:"type"`
		Event *Event `json:"event"`
	}
	if err := conn.ReadJSON(&live); err != nil {
		t.Fatalf("read live event: %v", err)
	}
	if live.Type != "event" {
		t.Fatalf("second message type = %q, want \"event\"", live.Type)
	}
	if live.Event == nil || live.Event.Type != evCellInserted {
		t.Fatalf("live event = %+v, want a cell_inserted", live.Event)
	}
}

func TestNotebookWS_UnknownNotebookIs404(t *testing.T) {
	withTempNotebooks(t)
	srv := testServer()
	hs := httptest.NewServer(srv.Router())
	defer hs.Close()

	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/notebook/never-existed?token=test-token"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		t.Fatal("expected the dial to fail for an unknown notebook")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v, want 404", resp)
	}
}

// Changing a cell's type is a command-mode verb in Jupyter (y for code, m
// for markdown) and one of the ones people reach for constantly — you type
// a paragraph into the wrong cell and fix it with one key. Without a
// backend for it the keys are decoration, so the log has to be able to
// express the change.
func TestNotebookAPI_CellTypeCanBeChanged(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "not actually a command")

	rec := nbRequest(t, f.srv, "PATCH", f.base+"/cells/"+cell, map[string]any{"type": "markdown"})
	if rec.Code != 200 {
		t.Fatalf("PATCH type = %d: %s", rec.Code, rec.Body.String())
	}

	doc := f.st.Doc()
	i := indexOfCell(doc, cell)
	if i < 0 {
		t.Fatalf("cell vanished")
	}
	if doc.Cells[i].Type != CellMarkdown {
		t.Errorf("type = %q, want markdown", doc.Cells[i].Type)
	}
	if doc.Cells[i].Source != "not actually a command" {
		t.Errorf("retyping a cell rewrote its source to %q", doc.Cells[i].Source)
	}
}

// A type the fold does not model must be refused at the door, exactly as
// it is on insert — otherwise the log carries a cell nothing can render.
func TestNotebookAPI_CellTypeChangeRejectsUnknownType(t *testing.T) {
	f := newNBFixture(t)
	cell := f.addCell(t, "shell", "echo hi")

	rec := nbRequest(t, f.srv, "PATCH", f.base+"/cells/"+cell, map[string]any{"type": "kernel"})
	if rec.Code != 400 {
		t.Errorf("PATCH with an unknown type = %d, want 400", rec.Code)
	}
}
