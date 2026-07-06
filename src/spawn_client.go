package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// runSpawnClient implements `collectif spawn` — a tiny CLI a running agent
// invokes to spawn a child agent under itself. It reads COLLECTIF_URL,
// COLLECTIF_TOKEN, and COLLECTIF_AGENT_ID from the environment (injected by
// pty.go when the parent was spawned).
//
//	collectif spawn --cwd /some/dir --prompt "do the thing"
//
// Prints the new agent id on stdout, exits 0 on success.
func runSpawnClient(args []string) {
	fs := flag.NewFlagSet("spawn", flag.ExitOnError)
	cwd := fs.String("cwd", "", "working directory for the child agent (required)")
	prompt := fs.String("prompt", "", "initial prompt for the child")
	parentOverride := fs.String("parent", "", "override parent agent id (default: $COLLECTIF_AGENT_ID)")
	fs.Parse(args)

	url := os.Getenv("COLLECTIF_URL")
	token := os.Getenv("COLLECTIF_TOKEN")
	if url == "" || token == "" {
		fmt.Fprintln(os.Stderr, "collectif spawn: COLLECTIF_URL and COLLECTIF_TOKEN must be set (available in every collectif-spawned session)")
		os.Exit(2)
	}
	if *cwd == "" {
		fmt.Fprintln(os.Stderr, "collectif spawn: --cwd is required")
		os.Exit(2)
	}
	abs, err := filepath.Abs(*cwd)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collectif spawn:", err)
		os.Exit(2)
	}
	parent := *parentOverride
	if parent == "" {
		parent = os.Getenv("COLLECTIF_AGENT_ID")
	}

	body, _ := json.Marshal(map[string]string{
		"cwd":           abs,
		"prompt":        *prompt,
		"parentAgentID": parent,
	})
	req, err := http.NewRequest("POST", url+"/api/agents", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "collectif spawn:", err)
		os.Exit(1)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collectif spawn:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "collectif spawn: %s\n%s\n", resp.Status, string(respBody))
		os.Exit(1)
	}
	var out struct{ AgentID string `json:"agentID"` }
	if err := json.Unmarshal(respBody, &out); err != nil {
		fmt.Fprintln(os.Stderr, "collectif spawn: bad response:", err)
		os.Exit(1)
	}
	fmt.Println(out.AgentID)
}
