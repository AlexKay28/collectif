package main

// nb_api.go — HTTP surface for notebooks. #49 (M1 slice 2), ADR 0001 §5.
//
// Mutations go over HTTP and the stream goes over WS, matching the split
// server.go already uses: an HTTP mutation is curl-able, testable, and has
// somewhere to put an error, while a stream is not.
//
// Every handler here is a thin translation from request to log event. The
// document is never mutated directly — notebookStore.Append is the only
// path, so anything that changes a notebook is on disk by construction.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

func registerNotebookRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/nb", handleNotebooks)
	mux.HandleFunc("/api/nb/", handleNotebookByID)
	mux.HandleFunc("/ws/notebook/", handleNotebookWS)
}

// handleNotebooks — GET lists, POST creates.
func handleNotebooks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := listNotebooks()
		if err != nil {
			http.Error(w, "list notebooks: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, list)

	case http.MethodPost:
		var req struct {
			Title string `json:"title"`
			Root  string `json:"root"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Title) == "" {
			req.Title = "Untitled notebook"
		}
		// The root is the containment boundary every tool in M3 will be
		// checked against, so it is validated at creation rather than
		// first use.
		if !filepath.IsAbs(req.Root) {
			http.Error(w, "root must be an absolute path", http.StatusBadRequest)
			return
		}
		if st, err := os.Stat(req.Root); err != nil || !st.IsDir() {
			http.Error(w, "root is not a directory", http.StatusBadRequest)
			return
		}
		nbStore, err := createNotebook(req.Title, req.Root)
		if err != nil {
			http.Error(w, "create notebook: "+err.Error(), http.StatusInternalServerError)
			return
		}
		doc := nbStore.Doc()
		writeJSON(w, http.StatusOK, map[string]any{
			"id": doc.ID, "title": doc.Title, "root": doc.Root,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleNotebookByID routes /api/nb/<id> and its subpaths.
func handleNotebookByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/nb/")
	if rest == "" {
		http.Error(w, "notebook id required", http.StatusBadRequest)
		return
	}
	id, subpath := rest, ""
	if i := strings.Index(rest, "/"); i >= 0 {
		id, subpath = rest[:i], rest[i+1:]
	}
	// Validate before touching the filesystem: the id becomes a filename.
	if !validNotebookSlug(id) {
		http.Error(w, "invalid notebook id", http.StatusBadRequest)
		return
	}

	st, err := acquireNotebook(id)
	if errors.Is(err, errNotebookNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "open notebook: "+err.Error(), http.StatusInternalServerError)
		return
	}

	switch {
	case subpath == "":
		handleNotebookRoot(w, r, st)
	case subpath == "cells":
		handleCellInsert(w, r, st)
	case strings.HasPrefix(subpath, "cells/"):
		rest := strings.TrimPrefix(subpath, "cells/")
		cellID, action := rest, ""
		if i := strings.Index(rest, "/"); i >= 0 {
			cellID, action = rest[:i], rest[i+1:]
		}
		if cellID == "" {
			http.Error(w, "cell id required", http.StatusBadRequest)
			return
		}
		switch action {
		case "":
			handleCellEditOrDelete(w, r, st, cellID)
		case "move":
			handleCellMove(w, r, st, cellID)
		case "run":
			handleCellRun(w, r, st, cellID)
		case "interrupt":
			handleCellInterrupt(w, r, st, cellID)
		case "approve":
			handleCellApprove(w, r, st, cellID)
		default:
			http.Error(w, "unknown subpath", http.StatusNotFound)
		}
	default:
		http.Error(w, "unknown subpath", http.StatusNotFound)
	}
}

func handleNotebookRoot(w http.ResponseWriter, r *http.Request, st *notebookStore) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, st.Doc())

	case http.MethodPatch:
		var req struct {
			Title *string       `json:"title"`
			Meta  *NotebookMeta `json:"meta"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Title == nil && req.Meta == nil {
			http.Error(w, "nothing to update", http.StatusBadRequest)
			return
		}
		if _, err := st.Append(evMetaSet, metaSetPayload{Title: req.Title, Meta: req.Meta}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st.Doc())

	case http.MethodDelete:
		slug := st.slug
		dir := nbDirFn()
		if err := releaseNotebook(slug); err != nil {
			http.Error(w, "close notebook: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// The log is the notebook; the snapshot and the search index are
		// caches of it. Remove all three, and treat a missing cache as
		// already done. An index left behind would keep offering search
		// results that open a document that no longer exists (#58).
		if err := os.Remove(filepath.Join(dir, slug+".jsonl")); err != nil && !os.IsNotExist(err) {
			http.Error(w, "delete notebook: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_ = os.Remove(filepath.Join(dir, slug+".snap.json"))
		_ = os.Remove(searchIndexPath(dir, slug))
		forgetSearchIndex(dir, slug)
		w.WriteHeader(http.StatusNoContent)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// validCellType keeps unmodelled cell types out of the log. A type we don't
// render is worse than a rejected request: it would fold into a document
// the UI cannot draw.
func validCellType(t CellType) bool {
	switch t {
	case CellMarkdown, CellPrompt, CellShell, CellFile:
		return true
	}
	return false
}

func handleCellInsert(w http.ResponseWriter, r *http.Request, st *notebookStore) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Type         CellType `json:"type"`
		Source       string   `json:"source"`
		Meta         CellMeta `json:"meta"`
		AfterCellID  string   `json:"afterCellId"`
		BeforeCellID string   `json:"beforeCellId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !validCellType(req.Type) {
		http.Error(w, "unknown cell type "+string(req.Type), http.StatusBadRequest)
		return
	}
	cell := Cell{
		ID:     uuid.NewString(),
		Type:   req.Type,
		Source: req.Source,
		Meta:   req.Meta,
		State:  CellIdle,
	}
	if _, err := st.Append(evCellInserted, cellInsertedPayload{
		Cell: cell, AfterCellID: req.AfterCellID, BeforeCellID: req.BeforeCellID,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cellId": cell.ID})
}

func handleCellEditOrDelete(w http.ResponseWriter, r *http.Request, st *notebookStore, cellID string) {
	switch r.Method {
	case http.MethodPatch:
		var req struct {
			Source *string   `json:"source"`
			Meta   *CellMeta `json:"meta"`
			Type   *CellType `json:"type"`
		}
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Source == nil && req.Meta == nil && req.Type == nil {
			http.Error(w, "nothing to update", http.StatusBadRequest)
			return
		}
		// Same gate as insert: a type the fold cannot render must never
		// reach the log, where it would be permanent.
		if req.Type != nil && !validCellType(*req.Type) {
			http.Error(w, "unknown cell type "+string(*req.Type), http.StatusBadRequest)
			return
		}
		if !cellExists(st, cellID) {
			http.Error(w, "cell not found", http.StatusNotFound)
			return
		}
		if _, err := st.Append(evCellEdited, cellEditedPayload{CellID: cellID, Source: req.Source, Meta: req.Meta, Type: req.Type}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cellId": cellID})

	case http.MethodDelete:
		if !cellExists(st, cellID) {
			http.Error(w, "cell not found", http.StatusNotFound)
			return
		}
		if _, err := st.Append(evCellDeleted, cellDeletedPayload{CellID: cellID}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"cellId": cellID})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleCellMove(w http.ResponseWriter, r *http.Request, st *notebookStore, cellID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BeforeCellID string `json:"beforeCellId"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !cellExists(st, cellID) {
		http.Error(w, "cell not found", http.StatusNotFound)
		return
	}
	if _, err := st.Append(evCellMoved, cellMovedPayload{CellID: cellID, BeforeCellID: req.BeforeCellID}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, st.Doc())
}

// cellExists checks the folded document so a request naming a cell that
// isn't there gets a 404 instead of appending an event that folds to a
// no-op — a log full of edits to deleted cells is a log that lies.
func cellExists(st *notebookStore, cellID string) bool {
	doc := st.Doc()
	return indexOfCell(doc, cellID) >= 0
}
