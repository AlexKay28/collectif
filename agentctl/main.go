package main

import (
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
)

//go:embed static
var staticFS embed.FS

func main() {
	setupLogging()
	port := flag.String("port", "7317", "TCP port to bind on 127.0.0.1")
	bind := flag.String("bind", "127.0.0.1", "bind address")
	flag.Parse()

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
	mux.HandleFunc("/api/hooks", handleHook)
	mux.HandleFunc("/ws/session/", handleSessionWS)
	mux.HandleFunc("/ws/dashboard", handleDashboardWS)

	addr := *bind + ":" + *port
	log.Printf("agentctl listening on http://%s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}
