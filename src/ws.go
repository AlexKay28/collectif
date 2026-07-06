package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(r *http.Request) bool { return true }, // 127.0.0.1 only bind = fine
}

func handleSessionWS(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/ws/session/")
	s := getSession(id)
	if s == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	// Flush scrollback so the new client sees prior output.
	if snap := s.snapshotRing(); len(snap) > 0 {
		_ = c.WriteMessage(websocket.BinaryMessage, snap)
	}
	s.addSub(c)
	defer func() {
		s.removeSub(c)
		_ = c.Close()
	}()

	for {
		mt, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		if s.PTY == nil {
			continue
		}
		switch mt {
		case websocket.TextMessage, websocket.BinaryMessage:
			_, _ = s.PTY.Write(data)
		}
	}
}

func handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	addDashSub(c)
	defer func() {
		removeDashSub(c)
		_ = c.Close()
	}()

	// Send initial snapshot.
	snap := map[string]any{"type": "snapshot", "agents": allSessionsJSON()}
	if b, err := json.Marshal(snap); err == nil {
		_ = c.WriteMessage(websocket.TextMessage, b)
	}

	// Keep the conn open; ignore inbound (dashboard is read-only for now).
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}
