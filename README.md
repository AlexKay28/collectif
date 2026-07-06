# COLLECTIF — agentctl

A single Go binary that spawns Claude Code sessions in PTYs, streams their terminal output to a browser dashboard over WebSocket, and derives live status (`running` / `waiting_input` / `idle` / `error` / `stopped`) from Claude Code's own hooks pushed via HTTP — no output parsing.

## Build

```
cd agentctl
go build -o agentctl .
```

Produces a single static binary with the dashboard HTML/JS embedded via `go:embed`.

## Run

```
./agentctl                # binds 127.0.0.1:7317
./agentctl -port 8080     # custom port
```

Open http://127.0.0.1:7317 in a browser.

- Click **+ New Agent**, enter an absolute cwd (and an optional initial prompt), Spawn.
- Tiles color-code by status. Click a tile to open an xterm.js pane bound to the PTY. Type freely; keystrokes go straight to the Claude process.
- The **Kill** button in the terminal overlay tears the process down.

## Security posture (MVP scope)

- Binds `127.0.0.1` only. No auth, no TLS.
- No persistence: registry is in-memory, gone on restart.
- The generated hook settings live in `/tmp/agentctl-settings-*` and are removed when the session is deleted.

## How status is derived

Each spawn writes a temp `settings.json` that registers HTTP hooks for `SessionStart`, `PreToolUse`, `PostToolUse`, `PostToolUseFailure`, `Notification`, `Stop`, `SessionEnd` pointing at `/api/hooks` on this server. Claude is launched with `--session-id <uuid> --settings <file>` so the session ID is pinned at spawn — no correlation race between PTY spawn and the first hook event.

Status mapping (`hooks.go`):

| hook event                          | status          |
|-------------------------------------|-----------------|
| SessionStart, Pre/PostToolUse       | running         |
| Notification matcher=permission_prompt | waiting_input |
| Notification matcher=idle_prompt    | idle            |
| PostToolUseFailure                  | error           |
| Stop                                | idle            |
| SessionEnd, PTY EOF                 | stopped         |
| PTY exit non-zero                   | error           |

## File layout

```
agentctl/
  main.go              server bootstrap + routing
  session.go           Session struct, registry, ring buffer, WS pub/sub
  pty.go               spawn `claude` in a PTY, tee output to ring + subscribers
  hooks.go             POST /api/hooks -> status transitions
  ws.go                /ws/session/:id and /ws/dashboard
  api.go               POST/DELETE /api/agents
  settings_gen.go      per-spawn settings.json with HTTP hooks
  static/index.html    dashboard grid + xterm.js pane (loaded from CDN)
```
