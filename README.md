# collectif

A single Go binary that runs coding-agent CLI sessions in PTYs and streams them to a browser dashboard.
Monitor multiple agents, answer their prompts, and watch token spend — from one page.

It still runs your agents in PTYs. What changed is that you no longer have
to *read* one: opening a session shows its **notebook** — the prompts you
sent and the work the agent did in reply, projected from its own transcript
into cells you can scroll, fold and export. The raw terminal is one click
away for the things a projection cannot show. See
[ADR 0002](docs/adr/0002-notebook-is-the-agent-surface.md).

collectif runs *your* agent rather than one of its own, so your existing
subscription keeps paying for it. `claude` is projected in full today;
`codex` and `opencode` spawn and are driven, and say plainly which parts of
their sessions collectif cannot yet read.

## Requirements

- **Go 1.21+** — `go version`
- At least one coding-agent CLI on `$PATH`: **Claude Code**
  ([install guide](https://docs.claude.com/en/docs/claude-code/quickstart)),
  `codex`, or `opencode`.

## Getting started

```bash
git clone https://github.com/AlexKay28/collectif.git
cd collectif
go build -o collectif ./src
./collectif                 # binds 127.0.0.1:7317 by default
```

On startup the server prints an auth token and the URL to use:

```
INFO  collectif listening on http://127.0.0.1:7317
INFO  Auth token: <token>
INFO  Open http://127.0.0.1:7317/?token=<token>
```

Open that URL and click **+ Add Agent** to spawn your first session. Select
it in the sidebar and its notebook opens; the **Terminal** tab beside the
session header is the raw PTY when you want it.

## Options

```bash
./collectif -port 8080                 # custom port
./collectif -bind 0.0.0.0              # listen on all interfaces (see below)
./collectif -token my-shared-secret    # fixed token instead of random
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
collectif/
  run.sh, README.md
  src/
    main.go            server bootstrap, auth, graceful shutdown
    session.go         Session state + registry
    pty.go             spawn a CLI in a PTY
    cli.go             CLIAdapter — what each CLI can and cannot report
    hooks.go           /api/hooks — status derivation
    menu.go            detects numbered TUI menus in PTY output
    transcript.go      token counting from the CLI's JSONL transcripts
    projection.go      one transcript line → notebook-shaped parts
    nb_session.go      those parts folded into a session's document
    nb_store.go        the append-only notebook log and its snapshot
    nb_export.go       a notebook as markdown, for a PR description
    api.go, ws.go      HTTP + WebSocket handlers
    log.go             colorized log output
    static/index.html  dashboard — the front door
    static/nb_*.{js,css}  the notebook: cells, rendering, the embedded view
```

## Documents

Every session collectif can project gets a notebook under
`.collectif/notebooks/<slug>.jsonl` — an append-only log, folded into a
document on read. It outlives the process: the session ends, its record
stays. `GET /api/nb/<id>/export` renders one as markdown, which is the form
a PR description can hold.

Nothing deletes them. The notebook page's header says how much disk they
are using so that stays a decision rather than a surprise.
