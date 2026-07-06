# COLLECTIF — agentctl

A single Go binary that runs Claude Code sessions in PTYs and streams them to a browser dashboard.
Monitor multiple agents, answer their prompts, and watch token spend — from one page.

## Requirements

- **Go 1.21+** — `go version`
- **Claude Code CLI** on `$PATH` — [install guide](https://docs.claude.com/en/docs/claude-code/quickstart)

## Getting started

```bash
git clone https://github.com/AlexKay28/Collectif.git
cd Collectif/agentctl
go build -o agentctl .
./agentctl                  # binds 127.0.0.1:7317 by default
```

On startup the server prints an auth token and the URL to use:

```
INFO  agentctl listening on http://127.0.0.1:7317
INFO  Auth token: <token>
INFO  Open http://127.0.0.1:7317/?token=<token>
```

Open that URL and click **+ New Agent** to spawn your first Claude Code session.

## Options

```bash
./agentctl -port 8080                 # custom port
./agentctl -bind 0.0.0.0              # listen on all interfaces (see below)
./agentctl -token my-shared-secret    # fixed token instead of random
```

**Remote access:** binding to `0.0.0.0` exposes the dashboard on your network. Anyone with the token gets full PTY control of your agents. Prefer an SSH tunnel:

```bash
ssh -L 7317:127.0.0.1:7317 you@server
# then open http://127.0.0.1:7317/?token=... on your laptop
```

## Or use the wrapper

```bash
./run.sh                    # from repo root — auto-rebuilds if source changed
./run.sh -port 8080
```

## Layout

```
agentctl/
  main.go              server bootstrap, auth, graceful shutdown
  session.go           Session state + registry
  pty.go               spawn `claude` in a PTY
  hooks.go             /api/hooks — status derivation
  menu.go              detects numbered TUI menus in PTY output
  transcript.go        token counting from Claude's JSONL transcripts
  api.go, ws.go        HTTP + WebSocket handlers
  log.go               colorized log output
  static/index.html    dashboard (single embedded file)
```
