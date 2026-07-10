package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// attach.go — #39 image attachments delivered to the CLI via @<path> refs.
//
// The flow: browser POSTs an image (multipart or JSON base64) to
//   POST /api/agents/{id}/attach
// server saves it under <cwd>/.collectif/attachments/ (or a temp dir if the
// cwd isn't writable) and returns the absolute path. Later, when the user
// hits Send, POST /api/agents/{id}/send composes "@<path> <text>\r" and
// writes it to the PTY.
//
// Delivery verification: when Claude's PostToolUse fires with a matching
// path in the tool input, we mark the attachment "seen" and broadcast a
// dashboard event so the chip turns green. If we don't see a read within
// staleAfter of the send, we broadcast stale — chip turns red.

const (
	attachMaxSize    = 5 << 20 // 5 MB
	staleAfter       = 15 * time.Second
	sanitiseFallback = "attachment"
)

// allowedImageMIMEs — anything else is refused up-front so the PTY doesn't
// receive references to files Claude Code won't accept as images.
var allowedImageMIMEs = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

type attachRecord struct {
	Path       string
	Name       string
	MIME       string
	Size       int64
	SessionID  string
	UploadedAt time.Time
	SentAt     time.Time // zero until the user hits Send
	Seen       bool
}

var (
	attachMu sync.Mutex
	attachments = map[string]*attachRecord{} // key: absolute path
)

// nameSanitiser strips anything that isn't safe inside a filename component.
var nameSanitiser = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func sanitiseFilename(raw string) string {
	base := filepath.Base(raw)
	base = nameSanitiser.ReplaceAllString(base, "_")
	base = strings.Trim(base, "._-")
	if base == "" {
		return sanitiseFallback
	}
	if len(base) > 80 {
		base = base[len(base)-80:]
	}
	return base
}

// attachmentsDir returns the directory where files land for a given session.
// Prefers <cwd>/.collectif/attachments, falls back to a per-session temp dir.
func attachmentsDir(s *Session) (string, error) {
	if s.Cwd != "" {
		candidate := filepath.Join(s.Cwd, ".collectif", "attachments")
		if err := os.MkdirAll(candidate, 0o700); err == nil {
			return candidate, nil
		}
	}
	tmp := filepath.Join(os.TempDir(), "collectif-"+s.ID, "attachments")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", err
	}
	return tmp, nil
}

// saveAttachment writes body to disk and registers the attachment.
func saveAttachment(s *Session, name, mime string, body []byte) (*attachRecord, error) {
	if len(body) == 0 {
		return nil, errors.New("empty file")
	}
	if int64(len(body)) > attachMaxSize {
		return nil, fmt.Errorf("file too large (%d > %d)", len(body), attachMaxSize)
	}
	if !allowedImageMIMEs[mime] {
		return nil, fmt.Errorf("mime type %q not allowed", mime)
	}
	dir, err := attachmentsDir(s)
	if err != nil {
		return nil, err
	}
	safe := sanitiseFilename(name)
	if safe == "" {
		safe = sanitiseFallback
	}
	stamp := time.Now().UnixNano()
	full := filepath.Join(dir, fmt.Sprintf("%d_%s", stamp, safe))
	if err := os.WriteFile(full, body, 0o600); err != nil {
		return nil, err
	}
	rec := &attachRecord{
		Path:       full,
		Name:       safe,
		MIME:       mime,
		Size:       int64(len(body)),
		SessionID:  s.ID,
		UploadedAt: time.Now(),
	}
	attachMu.Lock()
	attachments[full] = rec
	attachMu.Unlock()
	return rec, nil
}

// handleAttach — POST /api/agents/{id}/attach.
// Accepts multipart form ("file" field) OR JSON {"name","mime","data_b64"}.
func handleAttach(w http.ResponseWriter, r *http.Request, s *Session) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Guard against oversized uploads before we allocate any buffers.
	r.Body = http.MaxBytesReader(w, r.Body, attachMaxSize+64*1024) // small slack for form overhead

	var (
		name, mime string
		body       []byte
	)
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(attachMaxSize + 32*1024); err != nil {
			http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "field 'file' missing: "+err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		name = hdr.Filename
		mime = hdr.Header.Get("Content-Type")
		body, err = io.ReadAll(f)
		if err != nil {
			http.Error(w, "read file: "+err.Error(), http.StatusBadRequest)
			return
		}
	case strings.HasPrefix(ct, "application/json"):
		var in struct {
			Name    string `json:"name"`
			MIME    string `json:"mime"`
			DataB64 string `json:"data_b64"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			http.Error(w, "parse json: "+err.Error(), http.StatusBadRequest)
			return
		}
		var err error
		body, err = base64.StdEncoding.DecodeString(in.DataB64)
		if err != nil {
			http.Error(w, "decode base64: "+err.Error(), http.StatusBadRequest)
			return
		}
		name = in.Name
		mime = in.MIME
	default:
		http.Error(w, "unsupported content-type", http.StatusUnsupportedMediaType)
		return
	}

	if mime == "" {
		mime = "image/png" // pastes rarely carry a filename or MIME
	}
	rec, err := saveAttachment(s, name, mime, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      rec.Path,
		"name":      rec.Name,
		"mime":      rec.MIME,
		"sizeBytes": rec.Size,
	})
}

// handleAgentSend — POST /api/agents/{id}/send.
// Body: {"text": "...", "paths": ["/abs/path1", ...]}
// Composes "@<path1> @<path2> <text>\r" and writes to the PTY. Marks the
// attachments as sent (sentAt now) so the stale watcher can flag them.
func handleAgentSend(w http.ResponseWriter, r *http.Request, s *Session) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Text  string   `json:"text"`
		Paths []string `json:"paths"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	// Only accept paths that we minted for this session — no arbitrary
	// path injection just because the client typed one.
	var refs []string
	now := time.Now()
	attachMu.Lock()
	for _, p := range in.Paths {
		rec, ok := attachments[p]
		if !ok || rec.SessionID != s.ID {
			continue
		}
		rec.SentAt = now
		refs = append(refs, "@"+p)
	}
	attachMu.Unlock()

	msg := strings.Join(refs, " ")
	if in.Text != "" {
		if msg != "" {
			msg += " "
		}
		msg += in.Text
	}
	msg += "\r"

	pty := s.pty()
	if pty == nil {
		http.Error(w, "no PTY", http.StatusConflict)
		return
	}
	if _, err := pty.Write([]byte(msg)); err != nil {
		http.Error(w, "write PTY: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Broadcast so the chip strip moves into the "sent, awaiting seen" state.
	broadcastDashboard(map[string]any{
		"type":  "attachment_sent",
		"id":    s.ID,
		"paths": in.Paths,
	})
	w.WriteHeader(http.StatusNoContent)
}

// attachmentSeen is called from the PostToolUse hook path. It scans the
// tool input for anything that matches a pending attachment path and marks
// it seen. Also broadcasts to the dashboard so the chip turns green.
func attachmentSeen(s *Session, toolInput map[string]any) {
	if len(toolInput) == 0 {
		return
	}
	// Collect string values from the tool input — file_path, path,
	// command (Bash), etc. — and check each against pending attachments.
	candidates := extractStringPaths(toolInput)
	if len(candidates) == 0 {
		return
	}
	attachMu.Lock()
	var matched []string
	for _, cand := range candidates {
		for path, rec := range attachments {
			if rec.SessionID != s.ID || rec.Seen {
				continue
			}
			if strings.Contains(cand, path) {
				rec.Seen = true
				matched = append(matched, path)
			}
		}
	}
	attachMu.Unlock()
	if len(matched) == 0 {
		return
	}
	broadcastDashboard(map[string]any{
		"type":  "attachment_seen",
		"id":    s.ID,
		"paths": matched,
	})
}

func extractStringPaths(m map[string]any) []string {
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if len(t) > 0 && strings.Contains(t, "/") {
				out = append(out, t)
			}
		case map[string]any:
			for _, sub := range t {
				walk(sub)
			}
		case []any:
			for _, sub := range t {
				walk(sub)
			}
		}
	}
	walk(m)
	return out
}

// staleWatcher runs every 5 s, broadcasting attachment_stale for
// attachments that were sent >staleAfter ago and still not seen.
func startAttachmentStaleWatcher() {
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		staled := map[string]bool{}
		for range tick.C {
			now := time.Now()
			attachMu.Lock()
			bySession := map[string][]string{}
			for path, rec := range attachments {
				if rec.SentAt.IsZero() || rec.Seen || staled[path] {
					continue
				}
				if now.Sub(rec.SentAt) > staleAfter {
					staled[path] = true
					bySession[rec.SessionID] = append(bySession[rec.SessionID], path)
				}
			}
			attachMu.Unlock()
			for sid, paths := range bySession {
				broadcastDashboard(map[string]any{
					"type":  "attachment_stale",
					"id":    sid,
					"paths": paths,
				})
			}
		}
	}()
}

// cleanupAttachments removes on-disk files and clears map entries for a
// terminated session. Called from removeSession.
func cleanupAttachments(sessionID string) {
	attachMu.Lock()
	var toRemove []string
	for path, rec := range attachments {
		if rec.SessionID == sessionID {
			toRemove = append(toRemove, path)
			delete(attachments, path)
		}
	}
	attachMu.Unlock()
	for _, p := range toRemove {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("attach: remove %s: %v", p, err)
		}
	}
	// Also clean the temp fallback dir if it exists.
	tmp := filepath.Join(os.TempDir(), "collectif-"+sessionID)
	_ = os.RemoveAll(tmp)
}
