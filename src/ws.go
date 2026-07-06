package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// Cap inbound WS frames at 1 MB. PTY input is at most a paste — a hostile
// client shouldn't be able to stream unbounded data straight into a shell.
const wsReadLimit = 1 << 20

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
	c.SetReadLimit(wsReadLimit)
	sub := s.addSub(c)
	defer s.removeSub(sub)

	// Flush scrollback so the new client sees prior output — go through the
	// per-sub queue so it's serialized with future broadcasts.
	if snap := s.snapshotRing(); len(snap) > 0 {
		sub.send(snap)
	}

	// Read pump — nothing to write back; input goes straight to the PTY.
	// A dead-client detection ping keeps the read loop honest.
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.SetPongHandler(func(string) error {
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
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
	c.SetReadLimit(wsReadLimit)
	sub := addDashSub(c)
	defer removeDashSub(sub)

	// Send initial snapshot via the per-sub queue so ordering is preserved.
	if b, err := json.Marshal(map[string]any{"type": "snapshot", "agents": allSessionsJSON()}); err == nil {
		sub.send(b)
	}

	// Keep the conn open; ignore inbound (dashboard is read-only for now).
	_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.SetPongHandler(func(string) error {
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}
