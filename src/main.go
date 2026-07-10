package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"flag"
	"io/fs"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

//go:embed static
var staticFS embed.FS

var authToken string

func main() {
	setupLogging()
	port := flag.String("port", "7317", "TCP port to bind on 127.0.0.1")
	bind := flag.String("bind", "127.0.0.1", "bind address")
	tokenFlag := flag.String("token", "", "shared-secret auth token (random if empty)")
	flag.Parse()

	if *tokenFlag != "" {
		authToken = *tokenFlag
		_ = saveTokenFile(authToken)
	} else if t, err := loadTokenFile(); err == nil && t != "" {
		authToken = t
	} else {
		authToken = randomToken(24)
		if err := saveTokenFile(authToken); err != nil {
			log.Printf("warn: could not persist token: %v", err)
		}
	}

	hookBind = *bind
	hookPort = *port

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embed: %v", err)
	}

	// #35 load config once at boot; publishes into the atomic pointer.
	initConfig()

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/agents", handleAgents)
	mux.HandleFunc("/api/agents/", handleAgentByID)
	mux.HandleFunc("/api/cwd/check", handleCwdCheck)
	mux.HandleFunc("/api/config", handleConfig) // #35
	mux.HandleFunc("/api/hooks", handleHook)
	mux.HandleFunc("/ws/session/", handleSessionWS)
	mux.HandleFunc("/ws/dashboard", handleDashboardWS)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/metrics", handleMetrics)

	startPendingSweeper()
	startHourlyCostBroadcaster()   // #35
	startPRPoller()                // #37 PR-ready fallback detection
	startAttachmentStaleWatcher()  // #39 flag undelivered attachments

	addr := *bind + ":" + *port
	log.Printf("collectif listening on http://%s", addr)
	if !isLoopbackBind(*bind) {
		log.Printf("WARNING: bind address %q is not a loopback address — any client that can reach this host on the network can access collectif if they obtain the auth token", *bind)
		log.Printf("WARNING: the auth token is printed below in plaintext; anyone who sees these logs (including via a shared terminal, log file, or process listing) can authenticate")
	}
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

// tokenFilePath returns the location of the persisted auth token — mode 0600,
// under $XDG_CONFIG_HOME/collectif/token (or ~/.config/collectif/token as
// the XDG fallback). Delete this file to rotate the token on next start.
func tokenFilePath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "collectif", "token"), nil
}

func loadTokenFile() (string, error) {
	p, err := tokenFilePath()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func saveTokenFile(t string) error {
	p, err := tokenFilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(t+"\n"), 0o600)
}

// isLoopbackBind reports whether bind refers to the local loopback interface.
// We treat empty string as loopback-safe (Go's http server treats it the same
// as "0.0.0.0", but our default is "127.0.0.1" so empty shouldn't occur; be
// conservative and don't warn). Anything else that isn't 127.x/localhost/::1
// gets a warning at startup.
func isLoopbackBind(bind string) bool {
	if bind == "" || bind == "localhost" || bind == "::1" || bind == "[::1]" {
		return true
	}
	return strings.HasPrefix(bind, "127.")
}

func randomToken(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	max := big.NewInt(int64(len(alphabet)))
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			log.Fatalf("rand: %v", err)
		}
		out[i] = alphabet[idx.Int64()]
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
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/") || p == "/metrics"
}

func checkToken(r *http.Request) bool {
	if authToken == "" {
		return false
	}
	if q := r.URL.Query().Get("token"); q != "" && sameToken(q, authToken) {
		return true
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") && sameToken(strings.TrimPrefix(h, "Bearer "), authToken) {
		return true
	}
	return false
}

// sameToken compares two secrets in constant time so a network attacker
// can't leak the token one character at a time via response-timing analysis.
// crypto/subtle.ConstantTimeCompare requires equal-length inputs, so we
// short-circuit on length mismatch first (which is public information).
func sameToken(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func shutdownAllSessions(grace time.Duration) {
	registryMu.RLock()
	sessions := make([]*Session, 0, len(registry))
	for _, s := range registry {
		sessions = append(sessions, s)
	}
	registryMu.RUnlock()

	for _, s := range sessions {
		c := s.cmd()
		if c == nil || c.Process == nil {
			continue
		}
		if pgid, err := syscall.Getpgid(c.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		} else {
			_ = c.Process.Signal(syscall.SIGTERM)
		}
	}

	deadline := time.Now().Add(grace)
	for {
		alive := 0
		for _, s := range sessions {
			c := s.cmd()
			if c != nil && c.Process != nil && c.ProcessState == nil {
				if err := c.Process.Signal(syscall.Signal(0)); err == nil {
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
		c := s.cmd()
		if c == nil || c.Process == nil {
			continue
		}
		if err := c.Process.Signal(syscall.Signal(0)); err != nil {
			continue
		}
		if pgid, err := syscall.Getpgid(c.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		} else {
			_ = c.Process.Kill()
		}
	}
}
