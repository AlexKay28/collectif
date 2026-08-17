package main

// nb_ws.go — the notebook event stream. #49 (M1 slice 2), ADR 0001 §4.3.
//
// A client connects, receives the folded document once, then tails live
// events. That is the whole protocol: reconnecting is a re-fold, so a
// dropped connection costs nothing but a round trip, and a subscriber that
// falls behind can be dropped rather than blocking the writer.
//
// Note what is deliberately absent: the client sends nothing. Mutations go
// over HTTP (nb_api.go), which keeps the socket one-directional and means a
// hostile client cannot drive the notebook through a channel with no error
// path. The read pump exists only to notice a dead peer.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

func handleNotebookWS(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/ws/notebook/")
	id := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		id = rest[:i]
	}
	if !validNotebookSlug(id) {
		http.Error(w, "invalid notebook id", http.StatusBadRequest)
		return
	}

	// Resolve before upgrading so an unknown notebook is a plain 404 the
	// dialer can read, rather than an immediately-closed socket.
	st, err := acquireNotebook(id)
	if errors.Is(err, errNotebookNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "open notebook: "+err.Error(), http.StatusInternalServerError)
		return
	}

	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(wsReadLimit)

	sub := st.addSub(c)
	defer st.removeSub(sub)

	// Subscribe first, then send the fold. The other order has a hole: an
	// event appended between folding and subscribing would reach no one,
	// and the client would sit on a document quietly missing a change.
	//
	// The cost of this order is that a racing event can be queued ahead of
	// the fold. That is why events carry a sequence number: the fold states
	// the position it was taken at, and the client applies only events with
	// a greater one. Ordering stops mattering.
	// The fold carries the live buffers too. Deltas are never persisted, so
	// a client joining mid-run would otherwise see a running cell with
	// nothing in it until the run finished — which is exactly what a page
	// refresh does.
	doc := st.Doc()
	if b, err := json.Marshal(map[string]any{
		"type":     "fold",
		"version":  doc.Version,
		"notebook": doc,
		"live":     st.liveSnapshot(),
	}); err == nil {
		sub.send(b)
	}

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
