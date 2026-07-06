package main

import (
	"context"
	"crypto/rand"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

var authToken string

func main() {
	if len(os.Args) > 1 && os.Args[1] == "spawn" {
		runSpawnClient(os.Args[2:])
		return
	}
	setupLogging()
	port := flag.String("port", "7317", "TCP port to bind on 127.0.0.1")
	bind := flag.String("bind", "127.0.0.1", "bind address")
	tokenFlag := flag.String("token", "", "shared-secret auth token (random if empty)")
	flag.Parse()

	if *tokenFlag != "" {
		authToken = *tokenFlag
	} else {
		authToken = randomToken(24)
	}

	hookBind = *bind
	hookPort = *port

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/agents", handleAgents)
	mux.HandleFunc("/api/agents/", handleAgentByID)
	mux.HandleFunc("/api/cwd/check", handleCwdCheck)
	mux.HandleFunc("/api/hooks", handleHook)
	mux.HandleFunc("/ws/session/", handleSessionWS)
	mux.HandleFunc("/ws/dashboard", handleDashboardWS)

	startPendingSweeper()

	addr := *bind + ":" + *port
	log.Printf("collectif listening on http://%s", addr)
	log.Printf("Auth token: %s", authToken)
	log.Printf("Open http://%s:%s/?token=%s", *bind, *port, authToken)

	srv := &http.Server{Addr: addr, Handler: withAuth(mux)}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		log.Printf("shutdown: terminating sessions")
		shutdownAllSessions(3 * time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func randomToken(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		log.Fatalf("rand: %v", err)
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

// withAuth gates every request except /api/hooks on the shared-secret token.
// /api/hooks is authenticated separately by its per-session ?ht= UUID so a
// leaked shared secret cannot forge hook events. Static assets (HTML, CSS,
// JS) are public — they contain no secrets and browsers can't attach the
// token to <link>/<script> subresource requests. The token gate matters on
// /api/* and /ws/*, which is where real data lives.
func withAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api/hooks" || !isProtectedPath(p) {
			h.ServeHTTP(w, r)
			return
		}
		if !checkToken(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func isProtectedPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/")
}

func checkToken(r *http.Request) bool {
	if authToken == "" {
		return false
	}
	if q := r.URL.Query().Get("token"); q != "" && q == authToken {
		return true
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") && strings.TrimPrefix(h, "Bearer ") == authToken {
		return true
	}
	return false
}

func shutdownAllSessions(grace time.Duration) {
	registryMu.RLock()
	sessions := make([]*Session, 0, len(registry))
	for _, s := range registry {
		sessions = append(sessions, s)
	}
	registryMu.RUnlock()

	for _, s := range sessions {
		if s.Cmd == nil || s.Cmd.Process == nil {
			continue
		}
		if pgid, err := syscall.Getpgid(s.Cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			_ = s.Cmd.Process.Signal(syscall.SIGTERM)
		}
	}

	deadline := time.Now().Add(grace)
	for {
		alive := 0
		for _, s := range sessions {
			if s.Cmd != nil && s.Cmd.Process != nil && s.Cmd.ProcessState == nil {
				if err := s.Cmd.Process.Signal(syscall.Signal(0)); err == nil {
					alive++
				}
			}
		}
		if alive == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	for _, s := range sessions {
		if s.Cmd == nil || s.Cmd.Process == nil {
			continue
		}
		if err := s.Cmd.Process.Signal(syscall.Signal(0)); err != nil {
			continue
		}
		if pgid, err := syscall.Getpgid(s.Cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = s.Cmd.Process.Kill()
		}
	}
}
