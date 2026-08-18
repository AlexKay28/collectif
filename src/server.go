package main

// Server owns the runtime-level config that used to live as package-level
// vars (hookBind, hookPort, authToken, staticFS, auth-exempt predicate).
// Handlers that need any of these are methods on *Server; the rest stay as
// package-level funcs since they only touch the session registry.
//
// Motivation (#34): explicit dependencies, easier tests, room to run two
// instances in one process for multi-tenant work later.

import (
	"crypto/subtle"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
)

type Server struct {
	Bind      string
	Port      string
	AuthToken string
	StaticFS  fs.FS

	// IsProtected returns true when a request path needs the shared-secret
	// token. Extracted onto Server so tests can inject a different policy
	// and multi-tenant setups can vary the boundary.
	IsProtected func(path string) bool
}

func NewServer(bind, port, authToken string, staticFS fs.FS) *Server {
	return &Server{
		Bind:        bind,
		Port:        port,
		AuthToken:   authToken,
		StaticFS:    staticFS,
		IsProtected: defaultIsProtected,
	}
}

// defaultIsProtected mirrors the original hardcoded predicate at main.go:186.
func defaultIsProtected(p string) bool {
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/ws/") || p == "/metrics"
}

// Router returns the fully wired mux with auth gating applied.
func (srv *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(srv.StaticFS)))
	mux.HandleFunc("/api/agents", srv.handleAgents)
	mux.HandleFunc("/api/agents/", handleAgentByID)
	mux.HandleFunc("/api/cwd/check", handleCwdCheck)
	mux.HandleFunc("/api/config", handleConfig)
	// #46 Phase 3: expose the CLIAdapter registry so the frontend can
	// populate the spawn picker + decide which per-session panels to
	// degrade for adapters that don't support them.
	mux.HandleFunc("/api/cli", handleCLIList)
	mux.HandleFunc("/api/hooks", handleHook)
	mux.HandleFunc("/ws/session/", handleSessionWS)
	mux.HandleFunc("/ws/dashboard", handleDashboardWS)
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/metrics", handleMetrics)
	// #44 local GitHub issue/PR mirror. Read-only endpoints under /api/gh/*;
	// the syncer shells out to the host `gh` CLI.
	registerGHRoutes(mux)
	// #49 notebooks — /api/nb/* for mutations, /ws/notebook/<id> for the
	// event stream. Both inherit the auth gate below.
	registerNotebookRoutes(mux)
	// #58 search across those notebooks. Read-only, and read from an index
	// beside the logs rather than from the registry, so a query cannot pin
	// every notebook on disk open.
	registerSearchRoutes(mux)
	return srv.withAuth(mux)
}

// withAuth gates every request except /api/hooks on the shared-secret token.
// /api/hooks is authenticated separately by its per-session ?ht= UUID so a
// leaked shared secret cannot forge hook events.
func (srv *Server) withAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/api/hooks" || !srv.IsProtected(p) {
			h.ServeHTTP(w, r)
			return
		}
		if !srv.checkToken(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (srv *Server) checkToken(r *http.Request) bool {
	if srv.AuthToken == "" {
		return false
	}
	if q := r.URL.Query().Get("token"); q != "" && sameToken(q, srv.AuthToken) {
		return true
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") && sameToken(strings.TrimPrefix(h, "Bearer "), srv.AuthToken) {
		return true
	}
	return false
}

// sameToken compares two secrets in constant time so a network attacker
// can't leak the token one character at a time via response-timing analysis.
func sameToken(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// hookURL always targets 127.0.0.1: Claude Code runs on the same host as
// collectif, so the hook callback is a loopback call regardless of what
// address the server is bound to. Bind may be 0.0.0.0 for LAN access, which
// is not a routable destination for outgoing requests.
func (srv *Server) hookURL(hookToken string) string {
	return fmt.Sprintf("http://127.0.0.1:%s/api/hooks?ht=%s", srv.Port, hookToken)
}
