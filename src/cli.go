package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// runCLI dispatches to the correct sub-command implementation. Called from
// main() when the first arg is not a flag. Prints usage and exits when no
// arg or an unknown one is given.
func runCLI(args []string) {
	if len(args) == 0 {
		printUsage(os.Stdout)
		os.Exit(0)
	}
	switch args[0] {
	case "spawn":
		runSpawnClient(args[1:])
	case "whoami":
		runWhoamiClient(args[1:])
	case "children":
		runChildrenClient(args[1:])
	case "status":
		runStatusClient(args[1:])
	case "tail":
		runTailClient(args[1:])
	case "send":
		runSendClient(args[1:])
	case "kill":
		runKillClient(args[1:])
	case "-h", "--help", "help":
		printUsage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "collectif: unknown command %q\n\n", args[0])
		printUsage(os.Stderr)
		os.Exit(2)
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `collectif — manage Claude Code sub-agents from inside your session

Discovery:
  collectif whoami                       your id + parent id (from env)
  collectif children                     list your direct children

Read:
  collectif status <id>                  JSON snapshot of one agent
  collectif tail   <id> [--bytes N]      recent terminal output (default 8K)

Control:
  collectif send   <id> <text>           write text to that agent's PTY
  collectif kill   <id> [--yes]          terminate an agent

Create:
  collectif spawn --cwd DIR [--prompt "..."] [--parent ID]

Environment (auto-injected when spawned by collectif):
  COLLECTIF_URL, COLLECTIF_TOKEN, COLLECTIF_AGENT_ID, COLLECTIF_PARENT_ID
`)
}

func mustEnv() (string, string) {
	u := os.Getenv("COLLECTIF_URL")
	tok := os.Getenv("COLLECTIF_TOKEN")
	if u == "" || tok == "" {
		fmt.Fprintln(os.Stderr, "collectif: COLLECTIF_URL and COLLECTIF_TOKEN must be set (available in every collectif-spawned session)")
		os.Exit(2)
	}
	return u, tok
}

func apiGET(path string, out any) (int, []byte) {
	u, tok := mustEnv()
	req, _ := http.NewRequest("GET", u+path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collectif:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		_ = json.Unmarshal(body, out)
	}
	return resp.StatusCode, body
}

func apiJSON(method, path string, body any) (int, []byte) {
	u, tok := mustEnv()
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u+path, buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "collectif:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, rb
}

func runWhoamiClient(_ []string) {
	fmt.Printf("agent id:  %s\n", os.Getenv("COLLECTIF_AGENT_ID"))
	if p := os.Getenv("COLLECTIF_PARENT_ID"); p != "" {
		fmt.Printf("parent id: %s\n", p)
	} else {
		fmt.Println("parent id: (none — this agent is a root)")
	}
	fmt.Printf("url:       %s\n", os.Getenv("COLLECTIF_URL"))
}

func runChildrenClient(_ []string) {
	self := os.Getenv("COLLECTIF_AGENT_ID")
	if self == "" {
		fmt.Fprintln(os.Stderr, "collectif: COLLECTIF_AGENT_ID not set")
		os.Exit(2)
	}
	var all []map[string]any
	code, body := apiGET("/api/agents", &all)
	if code >= 300 {
		fmt.Fprintf(os.Stderr, "collectif children: %d %s\n", code, string(body))
		os.Exit(1)
	}
	fmt.Printf("%-10s  %-20s  %-14s  %-8s  %s\n", "ID", "NAME", "STATUS", "TOKENS", "TASK")
	found := 0
	for _, a := range all {
		if getStr(a, "parentId") != self {
			continue
		}
		found++
		task := getStr(a, "currentTask")
		if task == "" {
			task = getStr(a, "prompt")
		}
		fmt.Printf("%-10s  %-20s  %-14s  %-8s  %s\n",
			shortID(getStr(a, "id")),
			cliCodename(getStr(a, "id")),
			getStr(a, "status"),
			fmtI(a["inputTokens"])+"+"+fmtI(a["outputTokens"]),
			truncateCLI(task, 40))
	}
	if found == 0 {
		fmt.Println("(no children)")
	}
}

func runStatusClient(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: collectif status <id>")
		os.Exit(2)
	}
	code, body := apiGET("/api/agents/"+args[0], nil)
	if code >= 300 {
		fmt.Fprintf(os.Stderr, "collectif status: %d %s\n", code, string(body))
		os.Exit(1)
	}
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, body, "", "  ")
	os.Stdout.Write(pretty.Bytes())
	fmt.Println()
}

func runTailClient(args []string) {
	bytesN := 8192
	positional := []string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "--bytes" && i+1 < len(args) {
			fmt.Sscanf(args[i+1], "%d", &bytesN)
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--bytes=") {
			fmt.Sscanf(strings.TrimPrefix(args[i], "--bytes="), "%d", &bytesN)
			continue
		}
		positional = append(positional, args[i])
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "usage: collectif tail <id> [--bytes N]")
		os.Exit(2)
	}
	q := url.Values{}
	q.Set("bytes", fmt.Sprint(bytesN))
	code, body := apiGET("/api/agents/"+positional[0]+"/output?"+q.Encode(), nil)
	if code >= 300 {
		fmt.Fprintf(os.Stderr, "collectif tail: %d %s\n", code, string(body))
		os.Exit(1)
	}
	os.Stdout.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
}

func runSendClient(args []string) {
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: collectif send <id> <text>...")
		os.Exit(2)
	}
	id := args[0]
	text := strings.Join(args[1:], " ")
	if !strings.HasSuffix(text, "\n") && !strings.HasSuffix(text, "\r") {
		text += "\r"
	}
	code, body := apiJSON("POST", "/api/agents/"+id+"/input", map[string]string{"data": text})
	if code >= 300 {
		fmt.Fprintf(os.Stderr, "collectif send: %d %s\n", code, string(body))
		os.Exit(1)
	}
}

func runKillClient(args []string) {
	yes := false
	positional := []string{}
	for _, a := range args {
		if a == "--yes" || a == "-y" {
			yes = true
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "usage: collectif kill <id> [--yes]")
		os.Exit(2)
	}
	id := positional[0]
	if !yes {
		fmt.Fprintf(os.Stderr, "about to kill %s — type YES to confirm: ", id)
		var reply string
		fmt.Scanln(&reply)
		if strings.TrimSpace(reply) != "YES" {
			fmt.Fprintln(os.Stderr, "aborted")
			os.Exit(1)
		}
	}
	code, body := apiJSON("DELETE", "/api/agents/"+id, nil)
	if code >= 300 {
		fmt.Fprintf(os.Stderr, "collectif kill: %d %s\n", code, string(body))
		os.Exit(1)
	}
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// cliCodename mirrors the frontend's hash-of-id → adjective-noun so IDs in
// terminal output match what the dashboard shows.
func cliCodename(id string) string {
	adjs := []string{"swift", "calm", "brave", "quick", "clever", "stoic", "zesty", "spry", "keen", "bright", "daring", "fierce", "wise", "bold", "mellow", "nimble", "glad", "lively", "dandy", "witty"}
	nouns := []string{"otter", "hawk", "fox", "badger", "robin", "lynx", "heron", "panda", "goose", "stag", "koala", "raven", "sable", "tapir", "sparrow", "yak", "ibex", "marmot", "gecko", "newt"}
	var h uint32
	for _, c := range id {
		h = h*31 + uint32(c)
	}
	return adjs[int(h)%len(adjs)] + "-" + nouns[int(h>>8)%len(nouns)]
}

func fmtI(v any) string {
	switch x := v.(type) {
	case float64:
		return fmt.Sprintf("%d", int64(x))
	case int64:
		return fmt.Sprintf("%d", x)
	}
	return "0"
}

func truncateCLI(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
