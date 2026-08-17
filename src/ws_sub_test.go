package main

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A deliberate read of the WebSocket plumbing, after two serious bugs were
// found in it in consecutive phases (#49 review: no pings; #50: send on a
// closed channel). These tests pin the properties that file has to hold.

// A client that stops reading — a suspended laptop, a paused tab, a hung
// proxy — fills the TCP window and blocks WriteMessage forever. With no
// write deadline the pump goroutine is pinned, and its queue with it (up to
// 8 MB for a session), while send() keeps dropping so the server looks
// perfectly healthy. The connection has to time out and die instead.
func TestWSSub_ClientThatStopsReadingIsEventuallyDropped(t *testing.T) {
	prev := wsWriteTimeout
	wsWriteTimeout = 150 * time.Millisecond
	t.Cleanup(func() { wsWriteTimeout = prev })

	f := newNBFixture(t)
	conn, closeWS := f.dialWSRaw(t)
	defer closeWS()
	defer conn.Close()
	// Deliberately never read from conn.

	// Push enough that the socket buffers fill and a write has to block.
	big := strings.Repeat("x", 256*1024)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		f.st.broadcastDelta("c1", "r1", big)

		f.st.subMu.Lock()
		n := len(f.st.subs)
		var stopped bool
		for sub := range f.st.subs {
			select {
			case <-sub.closed:
				stopped = true
			default:
			}
		}
		f.st.subMu.Unlock()

		if stopped || n == 0 {
			return // the stalled client was dropped, which is the point
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("a client that never reads was never dropped — its pump goroutine and queue are pinned for the process lifetime")
}

// waitForSubscriber blocks until the handler goroutine has registered the
// connection. Dialing returns as soon as the handshake completes, which is
// before addSub runs — reading the map straight after a dial is a race in
// the test, not in the code.
func (f *nbFixture) waitForSubscriber(t *testing.T, timeout time.Duration) *wsSub {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f.st.subMu.Lock()
		var found *wsSub
		for s := range f.st.subs {
			found = s
		}
		f.st.subMu.Unlock()
		if found != nil {
			return found
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no subscriber registered within the timeout")
	return nil
}

// stop() is reached from both the reader side (handler defer) and the
// writer side (pump, on a write error), so it has to tolerate being called
// from several goroutines at once.
func TestWSSub_StopIsIdempotentUnderConcurrency(t *testing.T) {
	f := newNBFixture(t)
	conn, closeWS := f.dialWSRaw(t)
	defer closeWS()
	defer conn.Close()

	sub := f.waitForSubscriber(t, 5*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); sub.stop() }()
	}
	wg.Wait()

	if sub.send([]byte("after stop")) {
		t.Error("send accepted a message after stop")
	}
}

// Every handler must release its subscriber, or a page that is opened and
// closed repeatedly leaks a goroutine each time.
func TestWSSub_DisconnectReleasesTheSubscriber(t *testing.T) {
	f := newNBFixture(t)
	hs := httptest.NewServer(f.srv.Router())
	defer hs.Close()
	url := "ws" + strings.TrimPrefix(hs.URL, "http") + "/ws/notebook/" + f.st.slug + "?token=test-token"

	for i := 0; i < 10; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		conn.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.st.subMu.Lock()
		n := len(f.st.subs)
		f.st.subMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.st.subMu.Lock()
	n := len(f.st.subs)
	f.st.subMu.Unlock()
	t.Fatalf("%d subscribers still registered after every client disconnected", n)
}
