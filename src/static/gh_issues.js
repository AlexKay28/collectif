// ─── #44 Slice B — GitHub Issues frontend ────────────
// Self-contained: fetch → state → list render → detail render → nav.
// Reads live backend at /api/gh/* (slice A). All UI reads go through the
// cache; the network is never on a render's critical path — worst case
// we show the "No cache yet" empty state.
//
// Testing: set FIXTURE_MODE = true to render from an in-file JSON
// dataset instead of hitting the backend. Leave FALSE at commit time.
(function () {
  "use strict";

  const FIXTURE_MODE = false;

  // ─── Fixture data ─────────────────────────────
  // Only consulted when FIXTURE_MODE === true. Hand-written to exercise:
  // open + closed mix, multiple labels, assignees, checkboxes in body,
  // fenced code, comments with markdown.
  const FIXTURES = (function () {
    const issues = [
      { number: 44, title: "Local GitHub-style issue & PR tracker",
        state: "open", labels: ["enhancement", "priority: high"],
        assignees: ["AlexKay28"], author: "AlexKay28", commentCount: 3,
        createdAt: "2026-07-19T10:14:22Z", updatedAt: "2026-07-24T09:00:00Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/44" },
      { number: 43, title: "Right side panel: tabbed context view",
        state: "open", labels: ["enhancement", "priority: medium"],
        assignees: [], author: "AlexKay28", commentCount: 0,
        createdAt: "2026-07-17T12:59:11Z", updatedAt: "2026-07-17T12:59:11Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/43" },
      { number: 42, title: "Harness feedback: expose Claude Code's internal telemetry to the dashboard",
        state: "open", labels: ["enhancement", "priority: medium"],
        assignees: ["AlexKay28"], author: "AlexKay28", commentCount: 3,
        createdAt: "2026-07-11T08:00:00Z", updatedAt: "2026-07-23T11:00:00Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/42" },
      { number: 41, title: "Multi-agent visual control: Conductor-inspired UI",
        state: "open", labels: ["enhancement", "priority: medium"],
        assignees: [], author: "AlexKay28", commentCount: 1,
        createdAt: "2026-07-10T14:20:00Z", updatedAt: "2026-07-10T14:20:00Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/41" },
      { number: 40, title: "Attachment composer for the terminal",
        state: "closed", labels: ["enhancement"],
        assignees: ["AlexKay28"], author: "AlexKay28", commentCount: 2,
        createdAt: "2026-07-01T09:00:00Z", updatedAt: "2026-07-20T15:00:00Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/40" },
      { number: 37, title: "PR-ready review queue on Overview",
        state: "closed", labels: ["enhancement", "priority: low"],
        assignees: [], author: "AlexKay28", commentCount: 0,
        createdAt: "2026-06-28T10:00:00Z", updatedAt: "2026-07-05T08:00:00Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/37" },
      { number: 32, title: "CI workflow missing `workflow` scope",
        state: "open", labels: ["bug"],
        assignees: [], author: "AlexKay28", commentCount: 0,
        createdAt: "2026-06-15T12:00:00Z", updatedAt: "2026-06-15T12:00:00Z",
        htmlUrl: "https://github.com/AlexKay28/collectif/issues/32" },
    ];
    const detail = {
      44: {
        number: 44,
        title: "Local GitHub-style issue & PR tracker",
        state: "open",
        body: "## Motivation\n\nI administer this project's work through **GitHub Issues** and PRs, but I'm frequently on a flaky connection where github.com stalls.\n\nGoals for phase 1:\n\n- [x] Cache issues + comments on disk\n- [x] `↻ Sync` button in the UI\n- [ ] Single-issue view with rendered markdown\n- [ ] PR viewer (see #45)\n\n```go\n// example: cache layout\ntype CachedIssue struct {\n    Number  int\n    Title   string\n}\n```\n\nSee also [the docs](https://example.com).",
        labels: [
          { name: "enhancement", color: "a2eeef" },
          { name: "priority: high", color: "f85149" },
        ],
        assignees: [{ login: "AlexKay28" }],
        user: { login: "AlexKay28", avatar_url: "https://github.com/AlexKay28.png" },
        comments: 3,
        created_at: "2026-07-19T10:14:22Z",
        updated_at: "2026-07-24T09:00:00Z",
        html_url: "https://github.com/AlexKay28/collectif/issues/44",
        comments_data: [
          { id: 1, user: { login: "AlexKay28" },
            body: "Kicking this off. Slice A (backend) will land first, then B + C in parallel.\n\n- Backend: `/api/gh/*` endpoints\n- Frontend: two nav buttons",
            created_at: "2026-07-20T10:00:00Z" },
          { id: 2, user: { login: "reviewer-bot" },
            body: "*Reminder:* keep the markdown renderer dependency-free — no CDN pulls.",
            created_at: "2026-07-21T11:30:00Z" },
          { id: 3, user: { login: "AlexKay28" },
            body: "Ack. Hand-rolled it.",
            created_at: "2026-07-22T14:00:00Z" },
        ],
      },
      42: {
        number: 42,
        title: "Harness feedback: expose Claude Code's internal telemetry to the dashboard",
        state: "open",
        body: "The dashboard should surface **context-window pressure** and **session health scores** live.\n\n### Sub-features\n\n1. Context pressure bar (>=70% warm, >=90% hot)\n2. Session-health score (0–100)\n3. `context_pressure` toast on threshold cross\n\n### Tasks\n\n- [x] #42.1 Context pressure strip\n- [x] #42.7 Health check strip\n- [ ] #42.9 Telemetry export API\n- [ ] Docs update",
        labels: [
          { name: "enhancement", color: "a2eeef" },
          { name: "priority: medium", color: "fef2c0" },
        ],
        assignees: [{ login: "AlexKay28" }],
        user: { login: "AlexKay28", avatar_url: "https://github.com/AlexKay28.png" },
        comments: 3,
        created_at: "2026-07-11T08:00:00Z",
        updated_at: "2026-07-23T11:00:00Z",
        html_url: "https://github.com/AlexKay28/collectif/issues/42",
        comments_data: [
          { id: 10, user: { login: "AlexKay28" },
            body: "Sub-feature #1 shipped in `6ab170b`.", created_at: "2026-07-13T08:00:00Z" },
          { id: 11, user: { login: "reviewer-bot" },
            body: "Nice. Any thoughts on how #42.9 will expose the data?", created_at: "2026-07-15T09:00:00Z" },
          { id: 12, user: { login: "AlexKay28" },
            body: "Probably a WS stream keyed by session id. Details in the PR.", created_at: "2026-07-16T10:00:00Z" },
        ],
      },
      40: {
        number: 40,
        title: "Attachment composer for the terminal",
        state: "closed",
        body: "Add drag-drop + paste + drawing-pad image attachments to the running agent's PTY.\n\nShipped in slice #40 across a couple of commits.",
        labels: [ { name: "enhancement", color: "a2eeef" } ],
        assignees: [{ login: "AlexKay28" }],
        user: { login: "AlexKay28", avatar_url: "https://github.com/AlexKay28.png" },
        comments: 2,
        created_at: "2026-07-01T09:00:00Z",
        updated_at: "2026-07-20T15:00:00Z",
        html_url: "https://github.com/AlexKay28/collectif/issues/40",
        comments_data: [
          { id: 20, user: { login: "AlexKay28" }, body: "Landed the composer.", created_at: "2026-07-19T15:00:00Z" },
          { id: 21, user: { login: "AlexKay28" }, body: "Closing — feature complete.", created_at: "2026-07-20T15:00:00Z" },
        ],
      },
    };
    return { issues, detail };
  })();

  // ─── State ────────────────────────────────────
  const state = {
    view: "overview",       // "overview" | "issues" | "issue"
    issues: [],             // list from /api/gh/issues (client-cached)
    total: 0,
    stateFilter: "open",    // "open" | "closed"
    labelFilter: "",        // "" = all
    assigneeFilter: "",     // "" = all
    sort: "newest",         // newest | oldest | most-commented | recently-updated
    query: "",              // text search (client-side over title + cached body)
    detailBodies: {},       // number -> {body, comments_data} — for text search
    currentIssue: null,     // full issue object once opened
    syncStatus: null,       // {repo, lastSyncAt, syncing, pendingCount}
    syncPollTimer: 0,
    loading: false,
    loadError: null,
    empty: false,           // set when backend has no cache yet
  };

  // ─── Fetch helpers ────────────────────────────
  async function apiIssues(params) {
    if (FIXTURE_MODE) {
      const q = params || {};
      let list = FIXTURES.issues.slice();
      if (q.state && q.state !== "all") list = list.filter(i => i.state === q.state);
      if (q.label)    list = list.filter(i => (i.labels || []).some(l => l.toLowerCase() === q.label.toLowerCase()));
      if (q.assignee) list = list.filter(i => (i.assignees || []).some(a => a.toLowerCase() === q.assignee.toLowerCase()));
      if (q.q)        list = list.filter(i => i.title.toLowerCase().includes(q.q.toLowerCase()));
      return { total: list.length, issues: list };
    }
    const usp = new URLSearchParams();
    if (params) for (const [k, v] of Object.entries(params)) if (v) usp.set(k, v);
    const suffix = usp.toString() ? ("?" + usp.toString()) : "";
    const res = await fetch("/api/gh/issues" + suffix);
    if (!res.ok) throw new Error("issues: " + res.status);
    return await res.json();
  }
  async function apiIssue(n) {
    if (FIXTURE_MODE) {
      const d = FIXTURES.detail[n];
      if (!d) throw new Error("not cached");
      return d;
    }
    const res = await fetch("/api/gh/issues/" + encodeURIComponent(n));
    if (!res.ok) throw new Error("issue " + n + ": " + res.status);
    return await res.json();
  }
  async function apiStatus() {
    if (FIXTURE_MODE) {
      return {
        repo: { owner: "AlexKay28", name: "collectif" },
        defaultBranch: "main",
        lastSyncAt: new Date(Date.now() - 4 * 60_000).toISOString(),
        syncing: false, pendingCount: 0,
      };
    }
    const res = await fetch("/api/gh/status");
    if (!res.ok) throw new Error("status: " + res.status);
    return await res.json();
  }
  async function apiSync() {
    if (FIXTURE_MODE) return { started: true };
    const res = await fetch("/api/gh/sync", { method: "POST" });
    if (!res.ok) throw new Error("sync: " + res.status);
    return await res.json();
  }

  // ─── Utilities ────────────────────────────────
  function esc(s) {
    return String(s == null ? "" : s)
      .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;").replace(/'/g, "&#39;");
  }
  function humanAge(iso) {
    if (!iso) return "";
    const t = new Date(iso).getTime();
    if (!t || t < 0) return "";
    const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
    if (s < 60) return s + "s";
    if (s < 3600) return Math.floor(s / 60) + "m";
    if (s < 86400) return Math.floor(s / 3600) + "h";
    return Math.floor(s / 86400) + "d";
  }
  // Zero time from the contract = "0001-01-01T00:00:00Z".
  function isZeroTime(iso) {
    return !iso || iso.startsWith("0001-");
  }
  // GitHub label chip colour: pick black-or-white text based on background luminance.
  function labelChipStyle(hex) {
    if (!hex) return "background:var(--panel-2);color:var(--text)";
    const h = hex.replace("#", "").padEnd(6, "0");
    const r = parseInt(h.slice(0, 2), 16), g = parseInt(h.slice(2, 4), 16), b = parseInt(h.slice(4, 6), 16);
    const lum = (0.299 * r + 0.587 * g + 0.114 * b) / 255;
    const fg = lum > 0.55 ? "#000" : "#fff";
    return "background:#" + h + ";color:" + fg;
  }
  // Older list endpoint returns labels as string[]; detail returns objects with color.
  // Normalise so the chip renderer only sees {name, color}.
  function normLabels(labels) {
    if (!labels) return [];
    return labels.map(l => (typeof l === "string") ? { name: l, color: "" } : { name: l.name, color: l.color });
  }
  function countTasks(body) {
    if (!body) return { done: 0, total: 0 };
    let done = 0, total = 0;
    const re = /^\s*[-*]\s+\[( |x|X)\]\s+/gm;
    let m;
    while ((m = re.exec(body)) !== null) {
      total++;
      if (m[1].toLowerCase() === "x") done++;
    }
    return { done, total };
  }

  // ─── Minimal markdown renderer ────────────────
  // Supported: headings (# … ######), paragraphs, links [t](u), inline code
  // `x`, fenced code blocks ```lang…```, unordered lists (-, *), ordered
  // lists (1.), task list checkboxes (- [ ], - [x]), bold **x** __x__,
  // italic *x* _x_, blockquotes (>), horizontal rule (---), autolinks.
  //
  // Deliberately not supported: nested lists (we render them flat), tables,
  // images, HTML pass-through, reference-style links. Anything unsupported
  // just shows as literal escaped text — safer than a partial render.
  function renderMarkdown(src) {
    if (!src) return "";
    const lines = String(src).replace(/\r\n?/g, "\n").split("\n");
    const out = [];
    let i = 0;

    function inline(s) {
      // Escape first, then re-inject the allowed inline constructs. Order
      // matters: code spans first (their contents are opaque), then links,
      // then bold, italic, autolinks.
      // We use a placeholder approach for code spans so that later regexes
      // don't chew into their contents.
      const codes = [];
      s = s.replace(/`([^`]+)`/g, function (_, c) {
        codes.push(c);
        return "CODE" + (codes.length - 1) + "";
      });
      s = esc(s);
      // Links [t](u) — u limited to http(s)/mailto/# to avoid javascript:.
      s = s.replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, function (_, t, u) {
        const safe = /^(https?:|mailto:|#|\/)/i.test(u) ? u : "#";
        return '<a href="' + esc(safe) + '" target="_blank" rel="noopener">' + t + "</a>";
      });
      // Bold + italic (bold first so ** doesn't get mistaken for two italics).
      s = s.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
      s = s.replace(/__([^_]+)__/g, "<strong>$1</strong>");
      s = s.replace(/\*([^*\n]+)\*/g, "<em>$1</em>");
      s = s.replace(/(^|[^\w])_([^_\n]+)_/g, "$1<em>$2</em>");
      // Bare-URL autolink — after link processing so it doesn't clobber [](…).
      s = s.replace(/(^|[\s(])((?:https?:\/\/)[^\s<)]+)/g, function (_, pre, u) {
        return pre + '<a href="' + u + '" target="_blank" rel="noopener">' + u + "</a>";
      });
      // Restore code spans.
      s = s.replace(/CODE(\d+)/g, function (_, n) {
        return "<code>" + esc(codes[Number(n)]) + "</code>";
      });
      return s;
    }

    while (i < lines.length) {
      const line = lines[i];

      // Fenced code block.
      const fence = line.match(/^```(\S*)\s*$/);
      if (fence) {
        i++;
        const buf = [];
        while (i < lines.length && !/^```\s*$/.test(lines[i])) {
          buf.push(lines[i]); i++;
        }
        if (i < lines.length) i++; // skip closing fence
        const cls = fence[1] ? ' class="lang-' + esc(fence[1]) + '"' : "";
        out.push("<pre><code" + cls + ">" + esc(buf.join("\n")) + "</code></pre>");
        continue;
      }

      // Horizontal rule.
      if (/^(?:-{3,}|\*{3,}|_{3,})\s*$/.test(line)) {
        out.push("<hr>"); i++; continue;
      }

      // Heading.
      const h = line.match(/^(#{1,6})\s+(.*)$/);
      if (h) {
        const level = h[1].length;
        out.push("<h" + level + ">" + inline(h[2]) + "</h" + level + ">");
        i++; continue;
      }

      // Blockquote (single-level).
      if (/^>\s?/.test(line)) {
        const buf = [];
        while (i < lines.length && /^>\s?/.test(lines[i])) {
          buf.push(lines[i].replace(/^>\s?/, "")); i++;
        }
        out.push("<blockquote>" + renderMarkdown(buf.join("\n")) + "</blockquote>");
        continue;
      }

      // Unordered list (with optional task-list checkbox).
      if (/^\s*[-*]\s+/.test(line)) {
        const items = [];
        while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
          const raw = lines[i].replace(/^\s*[-*]\s+/, "");
          const task = raw.match(/^\[( |x|X)\]\s+(.*)$/);
          if (task) {
            const checked = task[1].toLowerCase() === "x" ? " checked" : "";
            items.push('<li class="task"><input type="checkbox" disabled' + checked + ">" + inline(task[2]) + "</li>");
          } else {
            items.push("<li>" + inline(raw) + "</li>");
          }
          i++;
        }
        out.push("<ul>" + items.join("") + "</ul>");
        continue;
      }

      // Ordered list.
      if (/^\s*\d+\.\s+/.test(line)) {
        const items = [];
        while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
          const raw = lines[i].replace(/^\s*\d+\.\s+/, "");
          items.push("<li>" + inline(raw) + "</li>");
          i++;
        }
        out.push("<ol>" + items.join("") + "</ol>");
        continue;
      }

      // Blank line — paragraph break.
      if (/^\s*$/.test(line)) { i++; continue; }

      // Paragraph: gather consecutive non-blank lines that aren't list/heading/fence.
      const buf = [];
      while (i < lines.length && !/^\s*$/.test(lines[i])
             && !/^```/.test(lines[i]) && !/^#{1,6}\s+/.test(lines[i])
             && !/^\s*[-*]\s+/.test(lines[i]) && !/^\s*\d+\.\s+/.test(lines[i])
             && !/^>\s?/.test(lines[i])
             && !/^(?:-{3,}|\*{3,}|_{3,})\s*$/.test(lines[i])) {
        buf.push(lines[i]); i++;
      }
      if (buf.length) out.push("<p>" + inline(buf.join(" ")) + "</p>");
    }

    return out.join("\n");
  }

  // ─── View toggle ──────────────────────────────
  // Show/hide the three top-level views. Kept intentionally self-contained
  // per slice-B scope — slice C's PR view does its own thing. Consolidation
  // happens post-merge.
  function showView(v) {
    state.view = v;
    const dash = document.getElementById("dashboard");
    const issuesView = document.getElementById("gh-issues-view");
    const detailView = document.getElementById("gh-issue-detail");
    if (dash) dash.style.display        = (v === "overview") ? "" : "none";
    if (issuesView) issuesView.style.display = (v === "issues") ? "" : "none";
    if (detailView) detailView.style.display = (v === "issue")  ? "" : "none";
    // Update nav-btn active state (only touch our own buttons).
    for (const b of document.querySelectorAll("#top-nav .nav-btn")) {
      const target = b.getAttribute("data-view");
      // For the detail view treat "issues" as still-active.
      const isActive = (v === target) || (v === "issue" && target === "gh-issues");
      b.classList.toggle("active", isActive);
    }
  }

  // ─── List render ──────────────────────────────
  function renderShell() {
    const view = document.getElementById("gh-issues-view");
    if (!view) return;
    // Build once. On subsequent calls we only refresh the list + counts.
    if (view.dataset.built) { refreshList(); return; }
    view.dataset.built = "1";

    const repo = state.syncStatus && state.syncStatus.repo
      ? state.syncStatus.repo.owner + "/" + state.syncStatus.repo.name
      : "";
    view.innerHTML =
      '<div class="gh-shell-head">' +
        '<h2>Issues</h2>' +
        '<span class="repo" id="gh-repo">' + esc(repo) + '</span>' +
        '<span class="spacer"></span>' +
      '</div>' +
      '<div class="gh-sync-bar">' +
        '<button id="gh-sync-btn" title="Fetch latest from GitHub">↻ Sync</button>' +
        '<span class="spinner" id="gh-sync-spinner" style="display:none"></span>' +
        '<span class="status" id="gh-sync-status">Never synced</span>' +
        '<span class="spacer"></span>' +
      '</div>' +
      '<div class="gh-filters">' +
        '<span class="gh-state-pills">' +
          '<button data-state="open" class="on">Open <span class="n" id="gh-cnt-open">0</span></button>' +
          '<button data-state="closed">Closed <span class="n" id="gh-cnt-closed">0</span></button>' +
        '</span>' +
        '<select id="gh-label"><option value="">Label</option></select>' +
        '<select id="gh-assignee"><option value="">Assignee</option></select>' +
        '<select id="gh-sort">' +
          '<option value="newest">Newest</option>' +
          '<option value="oldest">Oldest</option>' +
          '<option value="most-commented">Most commented</option>' +
          '<option value="recently-updated">Recently updated</option>' +
        '</select>' +
        '<span class="spacer"></span>' +
        '<input type="text" id="gh-search" placeholder="Search issues…">' +
      '</div>' +
      '<div class="gh-list" id="gh-list"></div>';

    // Wire filter controls.
    for (const b of view.querySelectorAll(".gh-state-pills button")) {
      b.addEventListener("click", () => {
        state.stateFilter = b.getAttribute("data-state");
        for (const x of view.querySelectorAll(".gh-state-pills button")) x.classList.remove("on");
        b.classList.add("on");
        loadIssues();
      });
    }
    view.querySelector("#gh-label").addEventListener("change", (e) => {
      state.labelFilter = e.target.value; loadIssues();
    });
    view.querySelector("#gh-assignee").addEventListener("change", (e) => {
      state.assigneeFilter = e.target.value; loadIssues();
    });
    view.querySelector("#gh-sort").addEventListener("change", (e) => {
      state.sort = e.target.value; refreshList();
    });
    view.querySelector("#gh-search").addEventListener("input", (e) => {
      state.query = e.target.value; refreshList();
    });
    view.querySelector("#gh-sync-btn").addEventListener("click", onSyncClick);
  }

  function refreshCounts() {
    // Counts run against the current in-memory list. When state is "open" the
    // "closed" count is unknown from a single request — best effort: show what
    // we've seen. This is a minor fidelity gap, noted in the final report.
    let open = 0, closed = 0;
    for (const i of state.issues) {
      if (i.state === "open") open++;
      else if (i.state === "closed") closed++;
    }
    const openEl = document.getElementById("gh-cnt-open");
    const closedEl = document.getElementById("gh-cnt-closed");
    if (openEl)   openEl.textContent = String(state.stateFilter === "open" ? state.total : open);
    if (closedEl) closedEl.textContent = String(state.stateFilter === "closed" ? state.total : closed);
  }

  function refreshFilterOptions() {
    // Rebuild label + assignee dropdowns from what's in the current list.
    const labelSel = document.getElementById("gh-label");
    const assigneeSel = document.getElementById("gh-assignee");
    if (!labelSel || !assigneeSel) return;
    const labels = new Set(), assignees = new Set();
    for (const i of state.issues) {
      for (const l of (i.labels || [])) labels.add(typeof l === "string" ? l : l.name);
      for (const a of (i.assignees || [])) assignees.add(a);
    }
    const currLbl = state.labelFilter;
    const currAss = state.assigneeFilter;
    labelSel.innerHTML = '<option value="">Label</option>' +
      Array.from(labels).sort().map(l => '<option value="' + esc(l) + '"' + (l === currLbl ? " selected" : "") + ">" + esc(l) + "</option>").join("");
    assigneeSel.innerHTML = '<option value="">Assignee</option>' +
      Array.from(assignees).sort().map(a => '<option value="' + esc(a) + '"' + (a === currAss ? " selected" : "") + ">" + esc(a) + "</option>").join("");
  }

  function sortIssues(list) {
    const arr = list.slice();
    if (state.sort === "newest")            arr.sort((a, b) => (b.createdAt || "").localeCompare(a.createdAt || ""));
    else if (state.sort === "oldest")       arr.sort((a, b) => (a.createdAt || "").localeCompare(b.createdAt || ""));
    else if (state.sort === "most-commented") arr.sort((a, b) => (b.commentCount || 0) - (a.commentCount || 0));
    else if (state.sort === "recently-updated") arr.sort((a, b) => (b.updatedAt || "").localeCompare(a.updatedAt || ""));
    return arr;
  }

  function refreshList() {
    const listEl = document.getElementById("gh-list");
    if (!listEl) return;
    refreshFilterOptions();
    refreshCounts();

    if (state.loading && state.issues.length === 0) {
      listEl.innerHTML = '<div class="empty">Loading…</div>';
      return;
    }
    if (state.empty || (state.total === 0 && !state.query)) {
      listEl.innerHTML =
        '<div class="empty">No cache yet — click <b>↻ Sync</b> to fetch from GitHub.' +
        (state.loadError ? '<div class="hint">' + esc(state.loadError) + "</div>" : "") +
        "</div>";
      return;
    }

    // Client-side text search over cached title + body (body only present if
    // we've opened that issue during the session).
    const q = state.query.trim().toLowerCase();
    let filtered = state.issues;
    if (q) {
      filtered = filtered.filter(i => {
        if ((i.title || "").toLowerCase().includes(q)) return true;
        const body = state.detailBodies[i.number];
        if (body && body.body && body.body.toLowerCase().includes(q)) return true;
        return false;
      });
    }
    filtered = sortIssues(filtered);

    if (filtered.length === 0) {
      listEl.innerHTML = '<div class="empty">No matching issues.</div>';
      return;
    }

    listEl.innerHTML = filtered.map(rowHTML).join("");
    for (const el of listEl.querySelectorAll(".gh-row")) {
      el.addEventListener("click", () => openIssue(Number(el.getAttribute("data-n"))));
    }
  }

  function rowHTML(i) {
    const labels = normLabels(i.labels);
    const labelHTML = labels.map(l =>
      '<span class="gh-label" style="' + labelChipStyle(l.color) + '">' + esc(l.name) + "</span>"
    ).join(" ");
    // Task count is only known once we've opened the issue detail. Show it
    // opportunistically if we have it cached.
    let taskFrag = "";
    const cached = state.detailBodies[i.number];
    if (cached) {
      const t = countTasks(cached.body);
      if (t.total > 0) taskFrag = ' <span class="dot-sep">·</span> ' + t.done + " of " + t.total + " tasks done";
    }
    const commentFrag = (i.commentCount || 0) > 0
      ? ' <span class="dot-sep">·</span> ' + (i.commentCount) + " comment" + (i.commentCount === 1 ? "" : "s")
      : "";
    const icon = i.state === "closed" ? "◉" : "○";
    return (
      '<div class="gh-row ' + (i.state === "closed" ? "closed" : "") + '" data-n="' + i.number + '">' +
        '<div class="icon">' + icon + "</div>" +
        '<div class="body">' +
          '<div class="title-row">' +
            '<span class="num">#' + i.number + "</span>" +
            '<span class="title">' + esc(i.title || "") + "</span>" +
            (labelHTML ? " " + labelHTML : "") +
          "</div>" +
          '<div class="meta">' +
            "opened " + esc(humanAge(i.createdAt)) + " by " + esc(i.author || "unknown") +
            commentFrag +
            taskFrag +
          "</div>" +
        "</div>" +
      "</div>"
    );
  }

  // ─── Load + sync ──────────────────────────────
  async function loadIssues() {
    state.loading = true;
    state.loadError = null;
    // Only send state=all for open/closed pills — the API treats missing state
    // as "all" too, but explicit is easier to debug from the network tab.
    const params = {
      state: state.stateFilter || "all",
      label: state.labelFilter,
      assignee: state.assigneeFilter,
    };
    try {
      const data = await apiIssues(params);
      state.issues = data.issues || [];
      state.total = data.total || 0;
      state.empty = state.total === 0 && !state.labelFilter && !state.assigneeFilter && !state.query;
    } catch (e) {
      state.issues = []; state.total = 0; state.empty = true;
      state.loadError = String(e && e.message || e);
    }
    state.loading = false;
    refreshList();
  }

  async function refreshStatus() {
    try {
      const s = await apiStatus();
      state.syncStatus = s;
      renderStatusBar();
      // Keep polling while a sync is in flight; stop otherwise.
      if (s.syncing) startSyncPolling();
      else {
        stopSyncPolling();
        // On sync completion, refresh the list so freshly-cached issues appear.
        if (state.view === "issues" || state.view === "issue") loadIssues();
      }
    } catch (e) {
      // Non-fatal — status errors just leave the bar stale.
    }
  }
  function renderStatusBar() {
    const s = state.syncStatus;
    if (!s) return;
    const statusEl = document.getElementById("gh-sync-status");
    const spinnerEl = document.getElementById("gh-sync-spinner");
    const btn = document.getElementById("gh-sync-btn");
    const repoEl = document.getElementById("gh-repo");
    if (repoEl && s.repo) repoEl.textContent = s.repo.owner + "/" + s.repo.name;
    if (spinnerEl) spinnerEl.style.display = s.syncing ? "" : "none";
    if (btn) btn.disabled = !!s.syncing;
    if (statusEl) {
      if (s.syncing) statusEl.textContent = "Syncing…";
      else if (isZeroTime(s.lastSyncAt)) statusEl.textContent = "Never synced";
      else statusEl.textContent = "Last sync: " + humanAge(s.lastSyncAt) + " ago";
    }
  }
  function startSyncPolling() {
    if (state.syncPollTimer) return;
    state.syncPollTimer = setInterval(refreshStatus, 3000);
  }
  function stopSyncPolling() {
    if (state.syncPollTimer) {
      clearInterval(state.syncPollTimer);
      state.syncPollTimer = 0;
    }
  }
  async function onSyncClick() {
    const btn = document.getElementById("gh-sync-btn");
    if (btn) btn.disabled = true;
    try {
      await apiSync();
      // Optimistically flip to syncing so the spinner shows immediately.
      if (state.syncStatus) state.syncStatus.syncing = true;
      renderStatusBar();
      startSyncPolling();
    } catch (e) {
      if (btn) btn.disabled = false;
      // Non-fatal — surface via the status text.
      const statusEl = document.getElementById("gh-sync-status");
      if (statusEl) statusEl.textContent = "Sync failed: " + String(e && e.message || e);
    }
  }

  // ─── Detail view ──────────────────────────────
  async function openIssue(n) {
    showView("issue");
    const view = document.getElementById("gh-issue-detail");
    if (!view) return;
    view.innerHTML = '<button class="gh-back">← Issues</button><div class="empty" style="color:var(--muted);padding:20px 0">Loading #' + n + "…</div>";
    view.querySelector(".gh-back").addEventListener("click", () => { showView("issues"); refreshList(); });
    try {
      const iss = await apiIssue(n);
      state.currentIssue = iss;
      // Cache body + comments for list-view text search.
      state.detailBodies[n] = { body: iss.body || "", comments_data: iss.comments_data || [] };
      renderDetail(iss);
    } catch (e) {
      view.innerHTML =
        '<button class="gh-back">← Issues</button>' +
        '<div class="empty" style="color:var(--muted);padding:20px 0">Could not load issue #' + n + ": " + esc(String(e && e.message || e)) + "</div>";
      view.querySelector(".gh-back").addEventListener("click", () => { showView("issues"); refreshList(); });
    }
  }

  function renderDetail(iss) {
    const view = document.getElementById("gh-issue-detail");
    if (!view) return;
    const labels = normLabels(iss.labels);
    const labelChips = labels.map(l =>
      '<span class="gh-label" style="' + labelChipStyle(l.color) + '">' + esc(l.name) + "</span>"
    ).join(" ");
    const author = iss.user && iss.user.login ? iss.user.login : "unknown";
    const avatar = iss.user && iss.user.avatar_url ? iss.user.avatar_url : "";
    const commentsData = iss.comments_data || [];
    const stateCls = iss.state === "closed" ? "closed" : "open";
    const stateLabel = iss.state === "closed" ? "Closed" : "Open";

    const commentsHTML = commentsData.map(c => {
      const clogin = c.user && c.user.login ? c.user.login : "unknown";
      const cav = c.user && c.user.avatar_url ? c.user.avatar_url : "";
      return (
        '<div class="gh-comment">' +
          '<div class="head">' +
            (cav ? '<span class="avatar"><img src="' + esc(cav) + '" alt=""></span>' : "") +
            '<span class="who">' + esc(clogin) + "</span>" +
            '<span class="when">commented ' + esc(humanAge(c.created_at)) + " ago</span>" +
          "</div>" +
          '<div class="body gh-md">' + renderMarkdown(c.body || "") + "</div>" +
        "</div>"
      );
    }).join("");

    view.innerHTML =
      '<button class="gh-back">← Issues</button>' +
      '<div class="gh-issue-head">' +
        '<h1><span class="num">#' + iss.number + "</span> " + esc(iss.title || "") + "</h1>" +
        '<div class="sub">' +
          '<span class="gh-state-pill ' + stateCls + '"><span class="dot"></span>' + stateLabel + "</span>" +
          '<span>opened by <b>' + esc(author) + "</b> · " + esc(humanAge(iss.created_at)) + " ago" +
          (commentsData.length > 0 ? " · " + commentsData.length + " comment" + (commentsData.length === 1 ? "" : "s") : "") +
          "</span>" +
          (labelChips ? '<span class="gh-labels-inline">' + labelChips + "</span>" : "") +
        "</div>" +
      "</div>" +
      '<div class="gh-issue-body">' +
        '<div class="head">' +
          (avatar ? '<span class="avatar"><img src="' + esc(avatar) + '" alt=""></span>' : "") +
          '<span class="who">' + esc(author) + "</span>" +
          '<span class="when">opened ' + esc(humanAge(iss.created_at)) + " ago</span>" +
        "</div>" +
        '<div class="body gh-md">' + renderMarkdown(iss.body || "*No description provided.*") + "</div>" +
      "</div>" +
      (commentsData.length > 0
        ? '<div class="gh-comments-head">' + commentsData.length + " comment" + (commentsData.length === 1 ? "" : "s") + "</div>" + commentsHTML
        : "") +
      '<div class="gh-actions">' +
        '<button class="open-gh" id="gh-open-external">Open on GitHub ↗</button>' +
        '<button disabled title="coming in slice D">Comment</button>' +
        '<button disabled title="coming in slice D">' + (iss.state === "closed" ? "Reopen" : "Close") + "</button>" +
        '<button disabled title="coming in slice D">Assign</button>' +
        '<button disabled title="coming in slice D">Label</button>' +
      "</div>";

    view.querySelector(".gh-back").addEventListener("click", () => { showView("issues"); refreshList(); });
    const openBtn = view.querySelector("#gh-open-external");
    if (openBtn && iss.html_url) {
      openBtn.addEventListener("click", () => window.open(iss.html_url, "_blank", "noopener"));
    } else if (openBtn) {
      openBtn.disabled = true;
    }
  }

  // ─── Boot ─────────────────────────────────────
  function wireNav() {
    // We own only the Issues nav button. Slice C will add PRs; we tolerate
    // (or add if missing) an Overview button that just resets to overview.
    const nav = document.getElementById("top-nav");
    if (!nav) return;
    for (const b of nav.querySelectorAll(".nav-btn")) {
      const target = b.getAttribute("data-view");
      b.addEventListener("click", () => {
        if (target === "overview") {
          showView("overview");
        } else if (target === "gh-issues") {
          showView("issues");
          renderShell();
          // First load: fire status + issues in parallel.
          if (state.issues.length === 0) {
            refreshStatus();
            loadIssues();
          } else {
            refreshList();
            refreshStatus();
          }
        }
        // Other targets (gh-prs, etc.) — slice C handles.
      });
    }
  }

  // Wait for DOMContentLoaded because `defer` on our script tag means we
  // load after body-parsing, but boot()-style init in app.js runs first.
  function init() {
    // Only the auth-authenticated app has the nav element; skip when the
    // auth screen replaced body.
    if (window.AGENTCTL_NO_TOKEN) return;
    wireNav();
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }

  // Expose a couple of hooks for future slices / debugging.
  window.collectifGH = {
    showView, openIssue, refreshList, loadIssues, refreshStatus,
    _fixtureMode: FIXTURE_MODE,
  };
})();
