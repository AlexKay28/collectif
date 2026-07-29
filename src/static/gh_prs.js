// ─── GitHub PR viewer — slice C of issue #44 ────────────────────
// Self-contained module: fetches /api/gh/prs*, renders list + detail,
// parses unified diff, hand-rolls markdown. No framework. No CDN libs.
// Toggle logic is local to this file (slice B owns its own toggle for
// the issues view; consolidation is post-merge).

(function () {
  "use strict";

  // ─── Fixture mode ────────────────────────────────
  // Flip to `true` for offline UI dev without slice A's backend.
  // MUST be `false` at commit time.
  const FIXTURE_MODE = false;

  const FIXTURE_PRS_INDEX = {
    total: 3,
    prs: [
      {
        number: 57, title: "Add /healthz + CI workflow",
        state: "open", merged: false, draft: false,
        headRef: "feature/healthz", baseRef: "main",
        headSha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        baseSha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        labels: ["ci"], assignees: [],
        author: "AlexKay28", commentCount: 3,
        createdAt: nDaysAgo(2), updatedAt: nDaysAgo(1),
        htmlUrl: "https://github.com/AlexKay28/collectif/pull/57",
      },
      {
        number: 55, title: "Refactor hook parser to strip ANSI aggressively",
        state: "closed", merged: true, draft: false,
        headRef: "refactor/hooks", baseRef: "main",
        headSha: "1111111111111111111111111111111111111111",
        baseSha: "2222222222222222222222222222222222222222",
        labels: ["refactor", "priority: medium"], assignees: [],
        author: "AlexKay28", commentCount: 7,
        createdAt: nDaysAgo(9), updatedAt: nDaysAgo(6),
        htmlUrl: "https://github.com/AlexKay28/collectif/pull/55",
      },
      {
        number: 54, title: "Draft: experimental streaming compaction",
        state: "open", merged: false, draft: true,
        headRef: "wip/compaction", baseRef: "main",
        headSha: "3333333333333333333333333333333333333333",
        baseSha: "4444444444444444444444444444444444444444",
        labels: ["experimental"], assignees: [],
        author: "someone-else", commentCount: 0,
        createdAt: nDaysAgo(4), updatedAt: nDaysAgo(4),
        htmlUrl: "https://github.com/AlexKay28/collectif/pull/54",
      },
      {
        number: 50, title: "Bump xterm to 5.5 and drop compat shim",
        state: "closed", merged: false, draft: false,
        headRef: "chore/xterm-5.5", baseRef: "main",
        headSha: "5555555555555555555555555555555555555555",
        baseSha: "6666666666666666666666666666666666666666",
        labels: ["chore"], assignees: [],
        author: "AlexKay28", commentCount: 1,
        createdAt: nDaysAgo(20), updatedAt: nDaysAgo(15),
        htmlUrl: "https://github.com/AlexKay28/collectif/pull/50",
      },
    ],
  };
  const FIXTURE_PR_DETAILS = {
    57: {
      number: 57, title: "Add /healthz + CI workflow",
      state: "open", merged: false, draft: false,
      body: "## Summary\n\nAdds a `/healthz` endpoint plus a minimal GitHub Actions CI workflow.\n\n- returns `200 OK` with `ok\\n` body\n- workflow runs `go build ./...` and `go test ./...`\n\n## Checklist\n\n- [x] endpoint returns 200\n- [x] workflow file green on first run\n- [ ] docs updated\n\nSee [example](https://example.com) for the deploy target.\n\nInline `code` looks like this.\n\n```go\nfunc healthz(w http.ResponseWriter, r *http.Request) {\n    w.Write([]byte(\"ok\\n\"))\n}\n```",
      html_url: "https://github.com/AlexKay28/collectif/pull/57",
      user: { login: "AlexKay28" },
      created_at: nDaysAgo(2), updated_at: nDaysAgo(1),
      head: { ref: "feature/healthz", sha: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
      base: { ref: "main", sha: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" },
      mergeable: true, mergeable_state: "clean",
      commits: 2, additions: 47, deletions: 3, changed_files: 3,
      comments_data: [
        { id: 1, user: { login: "Alex" }, body: "lgtm — one nit inline.", created_at: nDaysAgo(1), updated_at: nDaysAgo(1) },
      ],
      reviews_data: [
        { id: 10, user: { login: "Alex" }, state: "APPROVED", body: "Approving after nits", submitted_at: nHoursAgo(20) },
      ],
      review_comments_data: [
        { id: 20, user: { login: "Alex" }, body: "Prefer `io.WriteString` over `w.Write([]byte(…))`.", path: "src/health.go", line: 12, created_at: nHoursAgo(19), updated_at: nHoursAgo(19) },
      ],
    },
  };
  const FIXTURE_DIFF = "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\nnew file mode 100644\nindex 0000000..1234567\n--- /dev/null\n+++ b/.github/workflows/ci.yml\n@@ -0,0 +1,18 @@\n+name: CI\n+\n+on:\n+  push:\n+    branches: [ main ]\n+  pull_request:\n+\n+jobs:\n+  build:\n+    runs-on: ubuntu-latest\n+    steps:\n+      - uses: actions/checkout@v4\n+      - uses: actions/setup-go@v5\n+        with:\n+          go-version: '1.22'\n+      - run: go build ./...\n+      - run: go test ./...\n+\ndiff --git a/src/health.go b/src/health.go\nnew file mode 100644\nindex 0000000..abcdef1\n--- /dev/null\n+++ b/src/health.go\n@@ -0,0 +1,25 @@\n+package main\n+\n+import (\n+\t\"net/http\"\n+)\n+\n+// Liveness endpoint. Returns 200 unless the server is shutting down.\n+// Keep this cheap — no DB access, no lock acquisition.\n+func healthz(w http.ResponseWriter, r *http.Request) {\n+\tif shuttingDown() {\n+\t\thttp.Error(w, \"shutting down\", http.StatusServiceUnavailable)\n+\t\treturn\n+\t}\n+\tw.Header().Set(\"Content-Type\", \"text/plain\")\n+\tw.Write([]byte(\"ok\\n\"))\n+}\n+\n+var shutdownFlag atomic.Bool\n+func shuttingDown() bool { return shutdownFlag.Load() }\n+func markShuttingDown() { shutdownFlag.Store(true) }\n+\n+// Wire in main() via mux.HandleFunc(\"/healthz\", healthz).\n+// The route is intentionally NOT behind the auth middleware so probes\n+// from k8s / load balancers don't need a token.\ndiff --git a/src/main.go b/src/main.go\nindex 1234567..7654321 100644\n--- a/src/main.go\n+++ b/src/main.go\n@@ -42,7 +42,10 @@ func main() {\n \tmux := http.NewServeMux()\n \tmux.HandleFunc(\"/api/agents\", agentsHandler)\n \tmux.HandleFunc(\"/ws/dashboard\", wsHandler)\n-\thttp.ListenAndServe(\":\"+port, mux)\n+\tmux.HandleFunc(\"/healthz\", healthz)\n+\tsrv := &http.Server{Addr: \":\" + port, Handler: mux}\n+\tsetupSignalHandler(srv)\n+\tsrv.ListenAndServe()\n }\n";

  function nDaysAgo(n) { return new Date(Date.now() - n * 86400e3).toISOString(); }
  function nHoursAgo(n) { return new Date(Date.now() - n * 3600e3).toISOString(); }

  // ─── State ────────────────────────────────────────
  const state = {
    view: "list",          // "list" | "detail"
    prNumber: null,        // current detail PR
    detailTab: "conversation",  // "conversation" | "commits" | "files"
    diffText: null,        // last-fetched diff string
    diffError: null,
    diffLoading: false,
    expandedFiles: new Set(), // file paths open in the diff view
    detailData: null,      // last-fetched PR detail JSON
    detailError: null,
    list: null,            // last-fetched index JSON
    listError: null,
    filter: {
      state: "open",       // "open" | "closed" | "merged" | "all"
      sort: "updated-desc",
      q: "",
    },
    status: null,          // /api/gh/status JSON
    syncing: false,
  };

  // ─── Small helpers ─────────────────────────────────
  function esc(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }
  function relTime(iso) {
    if (!iso) return "";
    const t = new Date(iso).getTime();
    if (!t) return "";
    const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
    if (s < 60) return s + "s ago";
    if (s < 3600) return Math.floor(s / 60) + "m ago";
    if (s < 86400) return Math.floor(s / 3600) + "h ago";
    if (s < 30 * 86400) return Math.floor(s / 86400) + "d ago";
    if (s < 365 * 86400) return Math.floor(s / (30 * 86400)) + "mo ago";
    return Math.floor(s / (365 * 86400)) + "y ago";
  }

  // ─── Data layer ────────────────────────────────────
  async function apiGet(path) {
    if (FIXTURE_MODE) return fixtureFor(path);
    const res = await fetch(path);
    if (!res.ok) {
      const body = await res.text().catch(() => "");
      const err = new Error("HTTP " + res.status + (body ? ": " + body : ""));
      err.status = res.status;
      throw err;
    }
    // /diff is text; everything else is JSON.
    if (/\/diff$/.test(path)) return await res.text();
    return await res.json();
  }
  function fixtureFor(path) {
    if (path === "/api/gh/status") {
      return {
        repo: { owner: "AlexKay28", name: "collectif" },
        defaultBranch: "main",
        lastSyncAt: nHoursAgo(0.1),
        syncing: false,
        pendingCount: 0,
      };
    }
    if (path.startsWith("/api/gh/prs?") || path === "/api/gh/prs") return FIXTURE_PRS_INDEX;
    const m = path.match(/^\/api\/gh\/prs\/(\d+)(\/diff)?$/);
    if (m) {
      const n = Number(m[1]);
      if (m[2]) return FIXTURE_DIFF;
      const detail = FIXTURE_PR_DETAILS[n];
      if (!detail) { const e = new Error("HTTP 404: not cached"); e.status = 404; throw e; }
      return detail;
    }
    const e = new Error("fixture missing: " + path); e.status = 404; throw e;
  }

  async function loadStatus() {
    try { state.status = await apiGet("/api/gh/status"); }
    catch (err) { state.status = null; /* non-fatal */ }
  }
  async function loadList() {
    state.listError = null;
    try {
      const qs = new URLSearchParams();
      // The API accepts state=open|closed|all only. "merged" is a client-side
      // filter over the closed set (merged is a subset of closed on GitHub).
      const s = state.filter.state;
      qs.set("state", s === "merged" ? "closed" : (s === "all" ? "all" : s));
      if (state.filter.q) qs.set("q", state.filter.q);
      state.list = await apiGet("/api/gh/prs?" + qs.toString());
    } catch (err) {
      state.list = null;
      state.listError = err;
    }
  }
  async function loadDetail(n) {
    state.detailData = null;
    state.detailError = null;
    try { state.detailData = await apiGet("/api/gh/prs/" + n); }
    catch (err) { state.detailError = err; }
  }
  async function loadDiff(n) {
    state.diffLoading = true;
    state.diffError = null;
    state.diffText = null;
    renderDetail(); // show loading state
    try {
      state.diffText = await apiGet("/api/gh/prs/" + n + "/diff");
    } catch (err) {
      state.diffError = err;
    } finally {
      state.diffLoading = false;
      renderDetail();
    }
  }
  async function triggerSync() {
    if (FIXTURE_MODE) { toastMsg("info", "Fixture mode — sync stubbed"); return; }
    state.syncing = true;
    renderSyncBar();
    try {
      const res = await fetch("/api/gh/sync", { method: "POST" });
      if (!res.ok) throw new Error("HTTP " + res.status + ": " + (await res.text().catch(() => "")));
      toastMsg("info", "Sync started");
      pollUntilIdle();
    } catch (err) {
      state.syncing = false;
      toastMsg("error", "Sync failed: " + err.message);
      renderSyncBar();
    }
  }
  function pollUntilIdle() {
    let ticks = 0;
    const t = setInterval(async () => {
      ticks++;
      await loadStatus();
      state.syncing = !!(state.status && state.status.syncing);
      renderSyncBar();
      if (!state.syncing) {
        clearInterval(t);
        // Refresh the list after sync completes.
        await loadList();
        if (state.view === "list") renderList();
        toastMsg("success", "Sync complete");
      }
      if (ticks > 300) clearInterval(t); // 5-min hard cap
    }, 1000);
  }
  function toastMsg(level, msg) {
    if (window.toast && window.toast[level]) { window.toast[level](msg); return; }
    if (typeof toast !== "undefined" && toast[level]) { toast[level](msg); return; }
    console[level === "error" ? "error" : "log"](msg);
  }

  // ─── Minimal markdown renderer ────────────────────
  // Supports: headings, paragraphs, links, inline code, fenced code blocks,
  // unordered + ordered lists, task checkboxes, bold, italic. Not CommonMark;
  // just enough to render GitHub issue/PR bodies legibly.
  function renderMarkdown(md) {
    if (!md) return '<div class="ghp-empty-body">No description provided.</div>';
    const src = String(md).replace(/\r\n/g, "\n");
    const lines = src.split("\n");
    const out = [];
    let i = 0;

    function inline(t) {
      // First, protect inline `code` spans by tokenising them.
      const codes = [];
      t = t.replace(/`([^`]+)`/g, (_, c) => {
        codes.push(c); return " C" + (codes.length - 1) + " ";
      });
      t = esc(t);
      // Markdown links [text](url)
      t = t.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g,
        (_, txt, url) => '<a href="' + esc(url) + '" target="_blank" rel="noopener">' + txt + '</a>');
      // Autolink bare URLs.
      t = t.replace(/(^|[\s(])(https?:\/\/[^\s)]+)/g,
        (m, lead, url) => lead + '<a href="' + esc(url) + '" target="_blank" rel="noopener">' + esc(url) + '</a>');
      // Bold **x** and italic *x* / _x_
      t = t.replace(/\*\*([^*\n]+)\*\*/g, "<strong>$1</strong>");
      t = t.replace(/(^|[^*])\*([^*\n]+)\*(?!\*)/g, "$1<em>$2</em>");
      t = t.replace(/(^|[^_])_([^_\n]+)_(?!_)/g, "$1<em>$2</em>");
      // Restore code spans.
      t = t.replace(/ C(\d+) /g, (_, n) => "<code>" + esc(codes[Number(n)]) + "</code>");
      return t;
    }

    while (i < lines.length) {
      const line = lines[i];
      // Fenced code block
      const fence = line.match(/^```(\w*)\s*$/);
      if (fence) {
        const buf = [];
        i++;
        while (i < lines.length && !/^```\s*$/.test(lines[i])) { buf.push(lines[i]); i++; }
        if (i < lines.length) i++; // skip closing fence
        out.push('<pre><code>' + esc(buf.join("\n")) + '</code></pre>');
        continue;
      }
      // ATX heading
      const h = line.match(/^(#{1,6})\s+(.+?)\s*#*\s*$/);
      if (h) { out.push("<h" + h[1].length + ">" + inline(h[2]) + "</h" + h[1].length + ">"); i++; continue; }
      // Blank line
      if (/^\s*$/.test(line)) { i++; continue; }
      // Unordered list (- or *)
      if (/^\s*[-*]\s+/.test(line)) {
        const items = [];
        while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
          const m = lines[i].match(/^\s*[-*]\s+(.*)$/);
          let txt = m[1];
          // Task checkbox [ ] / [x]
          const cb = txt.match(/^\[([ xX])\]\s+(.*)$/);
          if (cb) {
            const checked = /[xX]/.test(cb[1]) ? ' checked' : '';
            items.push('<li><input type="checkbox" disabled' + checked + '>' + inline(cb[2]) + '</li>');
          } else {
            items.push("<li>" + inline(txt) + "</li>");
          }
          i++;
        }
        out.push("<ul>" + items.join("") + "</ul>");
        continue;
      }
      // Ordered list
      if (/^\s*\d+\.\s+/.test(line)) {
        const items = [];
        while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
          const m = lines[i].match(/^\s*\d+\.\s+(.*)$/);
          items.push("<li>" + inline(m[1]) + "</li>");
          i++;
        }
        out.push("<ol>" + items.join("") + "</ol>");
        continue;
      }
      // Paragraph — accumulate until blank line or block boundary
      const buf = [];
      while (i < lines.length && !/^\s*$/.test(lines[i]) && !/^```/.test(lines[i]) && !/^#{1,6}\s/.test(lines[i])
             && !/^\s*[-*]\s+/.test(lines[i]) && !/^\s*\d+\.\s+/.test(lines[i])) {
        buf.push(lines[i]); i++;
      }
      out.push("<p>" + inline(buf.join(" ")) + "</p>");
    }
    return out.join("\n");
  }

  // ─── Unified-diff parser ──────────────────────────
  // Returns [{ path, oldPath, additions, deletions, isNew, isDeleted, hunks: [{header, lines: [{type,text}]}] }, ...]
  function parseDiff(text) {
    if (!text) return [];
    const files = [];
    const lines = text.split("\n");
    let cur = null;
    let hunk = null;
    let i = 0;
    while (i < lines.length) {
      const line = lines[i];
      if (line.startsWith("diff --git ")) {
        if (cur) files.push(cur);
        cur = { path: "", oldPath: "", additions: 0, deletions: 0, isNew: false, isDeleted: false, hunks: [], preamble: [line] };
        const m = line.match(/^diff --git a\/(.+?) b\/(.+)$/);
        if (m) { cur.oldPath = m[1]; cur.path = m[2]; }
        hunk = null;
        i++;
        continue;
      }
      if (!cur) { i++; continue; }
      if (line.startsWith("new file mode")) { cur.isNew = true; cur.preamble.push(line); i++; continue; }
      if (line.startsWith("deleted file mode")) { cur.isDeleted = true; cur.preamble.push(line); i++; continue; }
      if (line.startsWith("index ") || line.startsWith("similarity ") || line.startsWith("rename ") || line.startsWith("copy ")
          || line.startsWith("Binary ")) {
        cur.preamble.push(line); i++; continue;
      }
      if (line.startsWith("--- ")) {
        const m = line.match(/^--- (?:a\/)?(.+)$/);
        if (m && m[1] !== "/dev/null") cur.oldPath = m[1];
        cur.preamble.push(line); i++; continue;
      }
      if (line.startsWith("+++ ")) {
        const m = line.match(/^\+\+\+ (?:b\/)?(.+)$/);
        if (m && m[1] !== "/dev/null") cur.path = cur.path || m[1];
        cur.preamble.push(line); i++; continue;
      }
      if (line.startsWith("@@")) {
        hunk = { header: line, lines: [] };
        cur.hunks.push(hunk);
        i++;
        continue;
      }
      if (!hunk) { i++; continue; }
      if (line.startsWith("+")) { hunk.lines.push({ type: "add", text: line.slice(1) }); cur.additions++; }
      else if (line.startsWith("-")) { hunk.lines.push({ type: "del", text: line.slice(1) }); cur.deletions++; }
      else if (line.startsWith("\\ ")) { hunk.lines.push({ type: "meta", text: line }); } // "\ No newline at end of file"
      else { hunk.lines.push({ type: "ctx", text: line.replace(/^ /, "") }); }
      i++;
    }
    if (cur) files.push(cur);
    return files;
  }

  function renderHunks(file) {
    const parts = [];
    for (const h of file.hunks) {
      parts.push('<div class="dline hunk"><span class="sign"></span><span class="code">' + esc(h.header) + '</span></div>');
      for (const l of h.lines) {
        if (l.type === "meta") { parts.push('<div class="dline meta">' + esc(l.text) + '</div>'); continue; }
        const sign = l.type === "add" ? "+" : (l.type === "del" ? "-" : " ");
        parts.push(
          '<div class="dline ' + l.type + '">' +
            '<span class="sign">' + sign + '</span>' +
            '<span class="code">' + esc(l.text) + '</span>' +
          '</div>'
        );
      }
    }
    return '<div class="ghp-hunks">' + parts.join("") + '</div>';
  }

  // ─── DOM lookups ──────────────────────────────────
  function root() { return document.getElementById("gh-prs-view"); }

  // ─── Rendering: list ─────────────────────────────
  function renderSyncBar() {
    const el = document.getElementById("ghp-syncbar");
    if (!el) return;
    const s = state.status;
    const repo = s && s.repo ? (s.repo.owner + "/" + s.repo.name) : "(no repo)";
    const lastSync = s && s.lastSyncAt && s.lastSyncAt !== "0001-01-01T00:00:00Z"
      ? "Last sync: " + relTime(s.lastSyncAt)
      : "Never synced";
    const statusText = state.syncing ? "syncing…" : lastSync;
    const statusCls = state.syncing ? "syncing" : "";
    el.innerHTML =
      '<span class="title">PRs</span>' +
      '<span class="repo">' + esc(repo) + '</span>' +
      '<span class="spacer"></span>' +
      '<span class="status ' + statusCls + '" id="ghp-sync-status">' + esc(statusText) + '</span>' +
      '<button class="sync" id="ghp-sync-btn"' + (state.syncing ? ' disabled' : '') + '>↻ Sync</button>';
    const btn = document.getElementById("ghp-sync-btn");
    if (btn) btn.onclick = triggerSync;
  }

  function counts() {
    // Best-effort counts; the /api/gh/prs response only ships the current
    // filtered slice, so we lean on `total` for whatever state is loaded
    // and leave the other pills without numbers on first load.
    const c = { open: null, closed: null, merged: null };
    if (state.list && Array.isArray(state.list.prs)) {
      const s = state.filter.state;
      if (s === "open") c.open = state.list.total;
      else if (s === "closed") c.closed = state.list.total;
      else if (s === "merged") c.merged = state.list.prs.filter(p => p.merged).length;
      else if (s === "all") {
        c.open = state.list.prs.filter(p => p.state === "open").length;
        c.closed = state.list.prs.filter(p => p.state === "closed" && !p.merged).length;
        c.merged = state.list.prs.filter(p => p.merged).length;
      }
    }
    return c;
  }

  function sortPRs(prs) {
    const sort = state.filter.sort;
    const arr = prs.slice();
    if (sort === "updated-desc") arr.sort((a, b) => (b.updatedAt || "").localeCompare(a.updatedAt || ""));
    else if (sort === "created-desc") arr.sort((a, b) => (b.createdAt || "").localeCompare(a.createdAt || ""));
    else if (sort === "created-asc") arr.sort((a, b) => (a.createdAt || "").localeCompare(b.createdAt || ""));
    else if (sort === "comments-desc") arr.sort((a, b) => (b.commentCount || 0) - (a.commentCount || 0));
    return arr;
  }

  function filterPRs(prs) {
    const q = state.filter.q.trim().toLowerCase();
    let out = prs;
    if (state.filter.state === "merged") out = out.filter(p => p.merged);
    if (state.filter.state === "closed") out = out.filter(p => !p.merged);
    if (q) out = out.filter(p =>
      (p.title || "").toLowerCase().includes(q) ||
      String(p.number || "").includes(q));
    return out;
  }

  function renderList() {
    const r = root();
    if (!r) return;
    r.innerHTML =
      '<div class="ghp-syncbar" id="ghp-syncbar"></div>' +
      '<div class="ghp-controls">' +
        '<div class="ghp-pills" id="ghp-pills"></div>' +
        '<span class="spacer"></span>' +
        '<select id="ghp-sort" title="Sort">' +
          '<option value="updated-desc">Recently updated</option>' +
          '<option value="created-desc">Newest</option>' +
          '<option value="created-asc">Oldest</option>' +
          '<option value="comments-desc">Most commented</option>' +
        '</select>' +
        '<input type="search" class="search" id="ghp-search" placeholder="Search PRs…" value="' + esc(state.filter.q) + '">' +
      '</div>' +
      '<div id="ghp-list-container"></div>';

    renderSyncBar();
    renderPills();
    const sortEl = document.getElementById("ghp-sort");
    if (sortEl) { sortEl.value = state.filter.sort; sortEl.onchange = () => { state.filter.sort = sortEl.value; renderListBody(); }; }
    const searchEl = document.getElementById("ghp-search");
    if (searchEl) {
      let timer = 0;
      searchEl.oninput = () => {
        clearTimeout(timer);
        timer = setTimeout(() => { state.filter.q = searchEl.value; renderListBody(); }, 150);
      };
    }
    renderListBody();
  }

  function renderPills() {
    const el = document.getElementById("ghp-pills");
    if (!el) return;
    const c = counts();
    const pill = (name, label) => {
      const cnt = c[name];
      const cntHtml = cnt != null ? '<span class="count">' + cnt + '</span>' : '';
      return '<button data-state="' + name + '"' + (state.filter.state === name ? ' class="active"' : '') + '>' +
        esc(label) + cntHtml + '</button>';
    };
    el.innerHTML = pill("open", "Open") + pill("closed", "Closed") + pill("merged", "Merged");
    el.querySelectorAll("button").forEach(b => {
      b.onclick = async () => {
        state.filter.state = b.dataset.state;
        renderListBody('<div class="ghp-loading">Loading…</div>');
        await loadList();
        renderPills();
        renderListBody();
      };
    });
  }

  function renderListBody(overrideHTML) {
    const el = document.getElementById("ghp-list-container");
    if (!el) return;
    if (overrideHTML) { el.innerHTML = overrideHTML; return; }
    if (state.listError) {
      const s = state.listError.status;
      if (s === 500 || s === 404) {
        el.innerHTML = '<div class="ghp-empty"><div class="big">☁</div><div>No cache yet — click <b>↻ Sync</b> to fetch from GitHub.</div></div>';
      } else {
        el.innerHTML = '<div class="ghp-empty"><div class="big">⚠</div><div>Couldn\'t load PRs: ' + esc(state.listError.message) + '</div></div>';
      }
      return;
    }
    if (!state.list || !Array.isArray(state.list.prs)) {
      el.innerHTML = '<div class="ghp-empty"><div class="big">☁</div><div>No cache yet — click <b>↻ Sync</b> to fetch from GitHub.</div></div>';
      return;
    }
    const prs = sortPRs(filterPRs(state.list.prs));
    if (prs.length === 0) {
      el.innerHTML = '<div class="ghp-empty"><div class="big">∅</div><div>No PRs match this filter.</div></div>';
      return;
    }
    const rows = prs.map(renderRow).join("");
    el.innerHTML = '<div class="ghp-list">' + rows + '</div>';
    el.querySelectorAll(".ghp-row").forEach(row => {
      row.onclick = () => openDetail(Number(row.dataset.n));
    });
  }

  function renderRow(pr) {
    const icoCls = pr.merged ? "merged" : (pr.state === "closed" ? "closed" : "");
    const ico = pr.merged ? "⬢" : (pr.state === "closed" ? "◉" : "◍");
    const labels = (pr.labels || []).map(l => '<span class="label">' + esc(l) + '</span>').join("");
    const commits = "commits";
    return '' +
      '<div class="ghp-row" data-n="' + pr.number + '">' +
        '<div class="ico ' + icoCls + '" title="' + esc(pr.merged ? "merged" : pr.state) + '">' + ico + '</div>' +
        '<div class="body">' +
          '<div class="title-line">' +
            '<span class="num">#' + pr.number + '</span>' +
            '<span>' + esc(pr.title || "(no title)") + '</span>' +
            (pr.draft ? '<span class="badge draft">Draft</span>' : '') +
            (labels ? '<span class="labels">' + labels + '</span>' : '') +
          '</div>' +
          '<div class="meta">' +
            esc(pr.author || "unknown") + ' wants to merge into ' +
            '<span class="mono">' + esc(pr.baseRef || "?") + '</span> ' +
            '<span class="arrow">←</span> ' +
            '<span class="mono">' + esc(pr.headRef || "?") + '</span>' +
            ' · opened ' + esc(relTime(pr.createdAt)) +
            ' · ' + (pr.commentCount || 0) + ' comment' + (pr.commentCount === 1 ? '' : 's') +
          '</div>' +
        '</div>' +
        '<div class="badges">' +
          (pr.commentCount ? '<span class="badge commentcount">💬 ' + pr.commentCount + '</span>' : '') +
        '</div>' +
      '</div>';
  }

  // ─── Rendering: detail ──────────────────────────
  async function openDetail(n) {
    state.view = "detail";
    state.prNumber = n;
    state.detailTab = "conversation";
    state.diffText = null; state.diffError = null; state.expandedFiles = new Set();
    renderDetailShell("<div class=\"ghp-loading\">Loading PR #" + n + "…</div>");
    await loadDetail(n);
    renderDetail();
  }

  function backToList() {
    state.view = "list";
    state.prNumber = null;
    renderList();
  }

  function renderDetailShell(bodyHTML) {
    const r = root();
    if (!r) return;
    r.innerHTML =
      '<button class="ghp-detail-back" id="ghp-back">← PRs</button>' +
      '<div id="ghp-detail-body">' + bodyHTML + '</div>';
    const back = document.getElementById("ghp-back");
    if (back) back.onclick = backToList;
  }

  function detailState(pr) {
    if (pr.merged || pr.merged_at) return { cls: "merged", label: "Merged" };
    if (pr.state === "closed") return { cls: "closed", label: "Closed" };
    if (pr.draft) return { cls: "draft", label: "Draft" };
    return { cls: "open", label: "Open" };
  }

  function renderDetail() {
    if (state.detailError) {
      const s = state.detailError.status;
      const msg = s === 404
        ? "Not cached yet — run a sync from the list view."
        : "Couldn't load PR: " + state.detailError.message;
      renderDetailShell('<div class="ghp-empty"><div class="big">⚠</div><div>' + esc(msg) + '</div></div>');
      return;
    }
    const pr = state.detailData;
    if (!pr) return; // still loading

    renderDetailShell(''); // reset with just the back button
    const container = document.getElementById("ghp-detail-body");
    if (!container) return;

    const st = detailState(pr);
    const commentsCount = (pr.comments_data || []).length + (pr.review_comments_data || []).length + (pr.reviews_data || []).filter(r => (r.body || "").trim()).length;
    const commitsCount = typeof pr.commits === "number" ? pr.commits : 0;
    const filesCount = typeof pr.changed_files === "number" ? pr.changed_files : 0;

    const headHTML =
      '<div class="ghp-detail-head">' +
        '<div class="title-row">' +
          '<div class="title"><span class="num">#' + pr.number + '</span>' + esc(pr.title || "(no title)") + '</div>' +
        '</div>' +
        '<div class="meta">' +
          '<span class="ghp-state ' + st.cls + '"><span class="dot"></span>' + st.label + '</span>' +
          '<span class="ghp-branches">' +
            '<span class="ref">' + esc((pr.user && pr.user.login) || "unknown") + ':' + esc((pr.head && pr.head.ref) || "?") + '</span> ' +
            '→ <span class="ref">' + esc((pr.base && pr.base.ref) || "?") + '</span>' +
          '</span>' +
          '<span>opened by <b>' + esc((pr.user && pr.user.login) || "unknown") + '</b> · ' + esc(relTime(pr.created_at)) + '</span>' +
          (typeof pr.additions === "number"
             ? '<span class="ghp-branches"><span class="ref" style="color:var(--running)">+' + pr.additions + '</span> <span class="ref" style="color:var(--error)">−' + pr.deletions + '</span></span>'
             : '') +
        '</div>' +
      '</div>';

    const tabsHTML =
      '<div class="ghp-tabs">' +
        '<button data-tab="conversation"' + (state.detailTab === "conversation" ? ' class="active"' : '') + '>Conversation<span class="count">' + commentsCount + '</span></button>' +
        '<button data-tab="commits"' + (state.detailTab === "commits" ? ' class="active"' : '') + '>Commits<span class="count">' + commitsCount + '</span></button>' +
        '<button data-tab="files"' + (state.detailTab === "files" ? ' class="active"' : '') + '>Files changed<span class="count">' + filesCount + '</span></button>' +
      '</div>';

    let paneHTML = "";
    if (state.detailTab === "conversation") paneHTML = renderConversationPane(pr);
    else if (state.detailTab === "commits") paneHTML = renderCommitsPane(pr);
    else if (state.detailTab === "files") paneHTML = renderFilesPane(pr);

    const actionsHTML =
      '<div class="ghp-actions">' +
        '<a class="btn" href="' + esc(pr.html_url || "#") + '" target="_blank" rel="noopener">Open on GitHub ↗</a>' +
        '<span class="spacer"></span>' +
        '<button disabled title="coming in slice D">Review</button>' +
        '<button disabled title="online only — use GitHub">Merge</button>' +
      '</div>';

    container.innerHTML = headHTML + tabsHTML + '<div id="ghp-tab-pane">' + paneHTML + '</div>' + actionsHTML;

    // Wire tab buttons
    container.querySelectorAll(".ghp-tabs button").forEach(btn => {
      btn.onclick = () => switchTab(btn.dataset.tab);
    });

    // Kick diff load lazily on first Files-tab open
    if (state.detailTab === "files" && state.diffText == null && !state.diffLoading && !state.diffError) {
      loadDiff(pr.number);
    }
    // Wire file collapse toggles
    wireFileToggles();
  }

  function switchTab(tab) {
    if (tab === state.detailTab) return;
    state.detailTab = tab;
    renderDetail();
  }

  function renderConversationPane(pr) {
    const body =
      '<div class="ghp-body">' +
        '<div class="ghp-md">' + renderMarkdown(pr.body || "") + '</div>' +
      '</div>';

    // Merge comment_data + reviews_data + review_comments_data into one
    // chronological stream so the reviewer reads what actually happened.
    const items = [];
    for (const c of (pr.comments_data || [])) items.push({
      kind: "comment", who: c.user && c.user.login, when: c.created_at, body: c.body,
    });
    for (const r of (pr.reviews_data || [])) {
      const st = String(r.state || "").toUpperCase();
      const badge = st === "APPROVED" ? "review-approved" :
                    st === "CHANGES_REQUESTED" ? "review-changes" :
                    "review-comment";
      const label = st === "APPROVED" ? "approved" :
                    st === "CHANGES_REQUESTED" ? "requested changes" :
                    "reviewed";
      items.push({
        kind: "review", who: r.user && r.user.login, when: r.submitted_at || r.created_at,
        body: r.body, badgeCls: badge, badgeText: label,
      });
    }
    for (const rc of (pr.review_comments_data || [])) items.push({
      kind: "line", who: rc.user && rc.user.login, when: rc.created_at,
      body: rc.body, path: rc.path, line: rc.line || rc.original_line,
      badgeCls: "review-line", badgeText: "line comment",
    });

    items.sort((a, b) => (a.when || "").localeCompare(b.when || ""));

    const commentsHTML = items.map(it => {
      const badge = it.badgeText
        ? '<span class="badge ' + esc(it.badgeCls || "") + '">' + esc(it.badgeText) + '</span>'
        : '';
      const pathBlock = it.path
        ? '<div class="path">' + esc(it.path) + (it.line ? ' <span class="line">:' + it.line + '</span>' : '') + '</div>'
        : '';
      return '' +
        '<div class="ghp-comment">' +
          '<div class="head">' +
            '<span class="who">' + esc(it.who || "unknown") + '</span>' +
            '<span>commented ' + esc(relTime(it.when)) + '</span>' +
            badge +
          '</div>' +
          pathBlock +
          ((it.body || "").trim()
            ? '<div class="body"><div class="ghp-md">' + renderMarkdown(it.body || "") + '</div></div>'
            : '') +
        '</div>';
    }).join("");

    return body + (commentsHTML ? '<div class="ghp-comments">' + commentsHTML + '</div>' : "");
  }

  function renderCommitsPane(pr) {
    // The cached PR JSON doesn't ship commits inline. Fall back to the
    // per-file diff summary if we have it; otherwise show a hint.
    // GitHub's /pulls/{n} response includes `commits` (count) but not
    // the array — that's /pulls/{n}/commits. Slice A doesn't cache it,
    // so we render a helpful stub with links.
    const html =
      '<div class="ghp-commits">' +
        '<div class="ghp-commit">' +
          '<div class="sha">—</div>' +
          '<div class="msg">' +
            (typeof pr.commits === "number" ? pr.commits + ' commit' + (pr.commits === 1 ? '' : 's') + ' — ' : '') +
            'per-commit metadata isn\'t cached in slice A. ' +
            'Open the PR on GitHub for the full list.' +
          '</div>' +
          '<div class="when">' +
            '<a href="' + esc(pr.html_url || "#") + '/commits" target="_blank" rel="noopener" style="color:var(--accent)">view on GitHub ↗</a>' +
          '</div>' +
        '</div>' +
      '</div>';
    return html;
  }

  function renderFilesPane(pr) {
    if (state.diffError) {
      const s = state.diffError.status;
      const msg = s === 503
        ? "Backend couldn't fetch the diff. Reconnect and retry, or wait for the next sync."
        : "Failed to load diff: " + state.diffError.message;
      return '<div class="ghp-diff-error">' +
        '<div class="msg">' + esc(msg) + '</div>' +
        '<button id="ghp-diff-retry">Retry</button>' +
      '</div>';
    }
    if (state.diffLoading || state.diffText == null) {
      return '<div class="ghp-diff-loading">Loading diff…</div>';
    }
    const files = parseDiff(state.diffText);
    if (files.length === 0) {
      return '<div class="ghp-empty" style="border-radius:8px;border:1px solid var(--border)"><div class="big">∅</div><div>No changes.</div></div>';
    }
    // Only auto-expand if there are ≤3 files, to keep large diffs manageable.
    if (state.expandedFiles.size === 0 && files.length <= 3) {
      for (const f of files) state.expandedFiles.add(f.path);
    }
    const html = files.map(f => {
      const expanded = state.expandedFiles.has(f.path);
      return '' +
        '<div class="ghp-file' + (expanded ? ' expanded' : '') + '" data-path="' + esc(f.path) + '">' +
          '<div class="fhead">' +
            '<span class="chev">▶</span>' +
            '<span class="path" title="' + esc(f.path) + '">' + esc(f.path) + '</span>' +
            '<span class="stats"><span class="add">+' + f.additions + '</span><span class="del">−' + f.deletions + '</span></span>' +
          '</div>' +
          '<div class="fbody">' + (expanded ? renderHunks(f) : "") + '</div>' +
        '</div>';
    }).join("");
    return '<div class="ghp-files">' + html + '</div>';
  }

  function wireFileToggles() {
    const pane = document.getElementById("ghp-tab-pane");
    if (!pane) return;
    const retry = document.getElementById("ghp-diff-retry");
    if (retry) retry.onclick = () => loadDiff(state.prNumber);
    pane.querySelectorAll(".ghp-file .fhead").forEach(h => {
      h.onclick = () => {
        const wrap = h.parentElement;
        const path = wrap.dataset.path;
        if (state.expandedFiles.has(path)) {
          state.expandedFiles.delete(path);
          wrap.classList.remove("expanded");
          const body = wrap.querySelector(".fbody");
          if (body) body.innerHTML = "";
        } else {
          state.expandedFiles.add(path);
          wrap.classList.add("expanded");
          // Lazy render the hunks now, on first open, so huge files don't
          // hit the DOM until asked.
          const body = wrap.querySelector(".fbody");
          if (body && !body.innerHTML.trim()) {
            const files = parseDiff(state.diffText || "");
            const f = files.find(x => x.path === path);
            if (f) body.innerHTML = renderHunks(f);
          }
        }
      };
    });
  }

  // ─── Nav toggle ──────────────────────────────────
  // Show/hide the PR view against the sibling #dashboard. Slice B owns its
  // own toggle for #gh-issues-view; we don't share code (post-merge cleanup).
  function showPRs() {
    const view = root();
    if (!view) return;
    const dashboard = document.getElementById("dashboard");
    const issuesView = document.getElementById("gh-issues-view"); // slice B, may not exist yet
    if (dashboard) dashboard.style.display = "none";
    if (issuesView) issuesView.style.display = "none";
    view.style.display = "";
    updateNavActive("gh-prs");
    if (state.view === "list") {
      renderList();
      if (state.status == null) loadStatus().then(renderSyncBar);
      if (state.list == null && state.listError == null) loadList().then(() => { renderPills(); renderListBody(); });
    } else {
      renderDetail();
    }
  }
  function hidePRs() {
    const view = root();
    if (view) view.style.display = "none";
  }
  function updateNavActive(which) {
    document.querySelectorAll("header .nav-btn").forEach(b => {
      b.classList.toggle("active", b.dataset.view === which);
    });
  }

  // Expose a small API so sibling slice B and the app boot can flip views
  // without reaching into private state.
  window.collectifGhPrs = {
    show: showPRs,
    hide: hidePRs,
    isVisible: () => { const r = root(); return r && r.style.display !== "none"; },
  };

  // ─── Boot ────────────────────────────────────────
  function boot() {
    // Inject the "PRs" button into the header, once, at load time.
    const header = document.querySelector("header");
    if (header && !header.querySelector('.nav-btn[data-view="gh-prs"]')) {
      const btn = document.createElement("button");
      btn.className = "nav-btn";
      btn.dataset.view = "gh-prs";
      btn.textContent = "PRs";
      // Sit the button right after the last .stat (before the spacer).
      const spacer = header.querySelector(".spacer");
      if (spacer) header.insertBefore(btn, spacer);
      else header.appendChild(btn);
      btn.onclick = showPRs;
    }
    // Clicking the "collectif" logo returns to dashboard — reuse the existing
    // handler in app.js by simply hiding our view when the dashboard becomes
    // visible again. We hook the h1 click as a low-priority listener.
    const h1 = document.querySelector("header h1");
    if (h1) {
      h1.addEventListener("click", () => {
        const view = root();
        if (view) view.style.display = "none";
        updateNavActive(null);
      });
    }
    // Initial state: our view is hidden.
    const view = root();
    if (view) view.style.display = "none";
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
})();
