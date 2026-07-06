// ─── Header stats ───────────────────────────────
function renderStats() {
  const arr = Array.from(agents.values());
  let active = 0, waiting = 0, pending = 0;
  let tokIn = 0, tokOut = 0;
  for (const a of arr) {
    if (a.status === "running") active++;
    else if (a.status === "waiting_input") waiting++;
    if (a.pending) pending++;
    tokIn += a.inputTokens || 0;
    tokOut += a.outputTokens || 0;
  }
  document.getElementById("stat-total").textContent = arr.length;
  document.getElementById("stat-active").textContent = active;
  document.getElementById("stat-waiting").textContent = waiting;
  document.getElementById("stat-tok").textContent = fmtNum(tokIn + tokOut);
  const pw = document.getElementById("stat-pending-wrap");
  document.getElementById("stat-pending").textContent = pending;
  pw.style.display = pending > 0 ? "" : "none";
  // Also flash the browser tab title when something needs input.
  document.title = pending > 0 ? "(" + pending + "!) collectif" : "collectif";
}

// ─── Dashboard section renderers ─────────────────
// Live feed dedupe fingerprints — surviving across renderFeed() calls.
let feedSeen = new Set();      // "agentId|t|event|tool" fingerprints

// Compute a lightweight per-render summary once and reuse across sections
// that need the aggregate view (subtitle, hero-strip, tokens, tool-usage).
function computeSummary() {
  const arr = Array.from(agents.values());
  const now = Date.now();
  let active = 0, waiting = 0, idle = 0, err = 0, stopped = 0, recentlyActive = 0;
  let totalIn = 0, totalOut = 0, totalCR = 0, totalCC = 0, totalMsgs = 0;
  const pending = [];
  const toolAgg = {};
  for (const a of arr) {
    if (a.status === "running") active++;
    else if (a.status === "waiting_input") waiting++;
    else if (a.status === "idle") idle++;
    else if (a.status === "error") err++;
    else if (a.status === "stopped") stopped++;
    const upd = a.updatedAt ? new Date(a.updatedAt).getTime() : 0;
    if (upd && (now - upd) < 30_000) recentlyActive++;
    totalIn += a.inputTokens || 0;
    totalOut += a.outputTokens || 0;
    totalCR += a.cacheReadTokens || 0;
    totalCC += a.cacheCreationTokens || 0;
    totalMsgs += a.messageCount || 0;
    if (a.pending) pending.push(a);
    for (const [t, c] of Object.entries(a.toolCounts || {})) toolAgg[t] = (toolAgg[t] || 0) + c;
  }
  const totalCost = arr.reduce((s, a) => s + estimateCost(a), 0);
  return { arr, active, waiting, idle, err, stopped, recentlyActive,
           totalIn, totalOut, totalCR, totalCC, totalMsgs, totalCost, pending, toolAgg };
}

// Subtitle + hero-strip live in the "pending" section conceptually (they
// summarise the fleet), so we render them there — kept as its own function
// so a change in pending count / hero counts doesn't churn tokens/feed HTML.
function renderPending() {
  const s = computeSummary();
  document.getElementById("dash-subtitle").textContent =
    s.arr.length === 0
      ? "No agents yet. Spawn one with + New Agent."
      : s.arr.length + " agent" + (s.arr.length === 1 ? "" : "s") + " · " + s.recentlyActive + " touched in last 30s · " + s.totalMsgs + " assistant messages so far";

  document.getElementById("dash-hero").innerHTML = (
    tileHero("Agents", s.arr.length, s.arr.length === 0 ? "" : "living sessions", "🧑‍💻") +
    tileHero("Active now", s.active, s.active === s.recentlyActive ? "working" : s.recentlyActive + " touched < 30s", "⚡", "accent") +
    tileHero("Waiting for you", s.waiting, s.waiting > 0 ? "needs a decision" : "no prompts", "⏳", s.waiting > 0 ? "wait" : "") +
    tileHero("Silent", s.idle + s.stopped, s.idle + " idle · " + s.stopped + " stopped", "🌙") +
    (s.err > 0 ? tileHero("Errors", s.err, "recent failures", "🔥", "err") : "")
  );
}

function renderTokens() {
  const s = computeSummary();
  document.getElementById("dash-tokens").innerHTML = (
    tileTok("Input", s.totalIn, "prompt tokens") +
    tileTok("Output", s.totalOut, "generated tokens") +
    tileTok("Cache read", s.totalCR, "cache hits") +
    tileTok("Cache write", s.totalCC, "cache created") +
    tileTokCost("Estimated cost", fmtCost(s.totalCost), "Sonnet 4.6 rates (approx)")
  );
}

function renderTokensByAgent() {
  const arr = Array.from(agents.values());
  // Ceiling steps up through a 1‑2.5‑5 sequence (1000, 2500, 5000, 10000, ...).
  const peakTok = Math.max(0, ...arr.map(a => (a.inputTokens || 0) + (a.outputTokens || 0)));
  const maxTok = barScaleCeil(peakTok);
  document.getElementById("dash-bars").innerHTML = arr.length === 0
    ? '<div style="color: var(--muted); font-size: 12px">no agents</div>'
    : arr.slice().sort((a, b) => ((b.inputTokens || 0) + (b.outputTokens || 0)) - ((a.inputTokens || 0) + (a.outputTokens || 0)))
        .map(a => {
          const tok = (a.inputTokens || 0) + (a.outputTokens || 0);
          const pct = tok <= 0 ? 0 : Math.max(1, Math.round((tok / maxTok) * 100));
          return (
            '<div class="row" data-id="' + esc(a.id) + '">' +
              '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
              '<div class="track"><div class="fill" style="width:' + pct + '%"></div><span class="lbl">' + esc(agentName(a)) + '</span></div>' +
              '<div class="num">' + fmtNum(tok) + '</div>' +
            '</div>'
          );
        }).join('');
  document.querySelectorAll("#dash-bars .row").forEach(r => {
    r.onclick = () => selectAgent(r.dataset.id);
  });
}

function renderFeed() {
  const arr = Array.from(agents.values());
  // Synthetic "action" rows first (permission / ask / menu), then the
  // chronological event stream below.
  const events = [];
  for (const a of arr) {
    for (const e of (a.activity || [])) {
      events.push({ ...e, agent: a });
    }
  }
  events.sort((a, b) => (b.t || "").localeCompare(a.t || ""));
  const shown = events.slice(0, 60);
  document.getElementById("dash-feed-count").textContent = events.length;

  const actionRows = [];
  for (const a of arr) {
    if (a.pending)      actionRows.push(renderActionPending(a));
    if (a.askQuestion && a.askQuestion.questions && a.askQuestion.questions.length)
                        actionRows.push(renderActionAsk(a));
    if (a.menuOptions && a.menuOptions.length)
                        actionRows.push(renderActionMenu(a));
  }
  const eventRowsHTML = shown.map(e => {
    const fp = e.agent.id + "|" + e.t + "|" + e.event + "|" + (e.tool || "");
    const isNew = !feedSeen.has(fp);
    feedSeen.add(fp);
    const lvl = e.level || "info";
    let what = '<span class="ev">' + esc(e.event) + '</span>';
    if (e.tool) what += ' <span class="tool">' + esc(e.tool) + '</span>';
    if (e.detail) what += ' <span class="det">— ' + esc(e.detail) + '</span>';
    return (
      '<div class="row' + (isNew ? ' new' : '') + '" data-id="' + esc(e.agent.id) + '">' +
        '<div class="t">' + humanTime(e.t) + '</div>' +
        '<div class="who"><div class="avatar"><img src="' + avatarURL(e.agent.id) + '" alt=""></div><span class="n">' + esc(agentName(e.agent)) + '</span></div>' +
        '<div class="what ' + esc(lvl) + '">' + what + '</div>' +
      '</div>'
    );
  }).join('');

  document.getElementById("dash-feed").innerHTML = (actionRows.length === 0 && shown.length === 0)
    ? '<div style="padding:14px; color: var(--muted); font-family: -apple-system, sans-serif">No events yet.</div>'
    : (actionRows.join('') + eventRowsHTML);
  if (feedSeen.size > 500) feedSeen = new Set(Array.from(feedSeen).slice(-300));

  // Wire click handlers.
  document.querySelectorAll("#dash-feed .row").forEach(r => {
    r.onclick = (ev) => {
      if (ev.target.closest(".btns-inline")) return;
      selectAgent(r.dataset.id);
    };
  });
  document.querySelectorAll("#dash-feed .action.pending .btns button[data-act]").forEach(btn => {
    btn.onclick = async (ev) => {
      ev.stopPropagation();
      const id = btn.dataset.id, act = btn.dataset.act;
      btn.disabled = true;
      try {
        const res = await fetch("/api/agents/" + id + "/" + act, { method: "POST" });
        if (!res.ok) toast.error(act + " failed: " + (await res.text()));
      } finally { setTimeout(() => { btn.disabled = false; }, 400); }
    };
  });
  document.querySelectorAll("#dash-feed .action.menu .btns button[data-key]").forEach(btn => {
    btn.onclick = async (ev) => {
      ev.stopPropagation();
      const id = btn.closest(".action").dataset.id;
      btn.disabled = true;
      try { await sendToAgent(id, btn.dataset.key + "\r"); }
      finally { setTimeout(() => { btn.disabled = false; }, 400); }
    };
  });
  document.querySelectorAll("#dash-feed .action.ask .btns button[data-idx]").forEach(btn => {
    btn.onclick = async (ev) => {
      ev.stopPropagation();
      const row = btn.closest(".action");
      const id = row.dataset.id;
      const a = agents.get(id);
      const idx = parseInt(btn.dataset.idx, 10);
      const qIdx = parseInt(btn.dataset.q, 10);
      const qi = a && a.askQuestion && a.askQuestion.questions[qIdx];
      // Go to top of list, walk down, then confirm (or toggle for multi).
      let keys = "\x1b[A".repeat(20) + "\x1b[B".repeat(idx);
      keys += (qi && qi.multiSelect) ? " " : "\r";
      btn.disabled = true;
      try { await sendToAgent(id, keys); }
      finally { setTimeout(() => { btn.disabled = false; }, 400); }
    };
  });
  document.querySelectorAll("#dash-feed .action.ask .navs button[data-nav]").forEach(btn => {
    btn.onclick = async (ev) => {
      ev.stopPropagation();
      const id = btn.closest(".action").dataset.id;
      const nav = btn.dataset.nav;
      const key = nav === "up" ? "\x1b[A" : nav === "down" ? "\x1b[B" : nav === "enter" ? "\r" : "\x1b";
      await sendToAgent(id, key);
    };
  });
  document.querySelectorAll("#dash-feed .action").forEach(r => {
    r.onclick = (ev) => {
      if (ev.target.closest("button")) return;
      selectAgent(r.dataset.id);
    };
  });
}

function renderToolUsage() {
  const { toolAgg } = computeSummary();
  const toolEntries = Object.entries(toolAgg).sort((a, b) => b[1] - a[1]);
  document.getElementById("dash-tools").innerHTML = toolEntries.length === 0
    ? '<div style="color: var(--muted); font-size: 12px">no tool calls yet</div>'
    : toolEntries.map(([t, c]) => '<span class="chip">' + esc(t) + '<strong>' + c + '</strong></span>').join('');
}

function renderTrends() {
  drawSparkline("chart-active", trend.map(s => s.active), { yMin: 0 });
  document.getElementById("chart-active-val").textContent = trend.length ? trend[trend.length - 1].active : "0";
  drawSparkline("chart-tokens", trend.map(s => s.tokens), { yMin: 0 });
  document.getElementById("chart-tokens-val").textContent = fmtNum(trend.length ? trend[trend.length - 1].tokens : 0);
}

// Fire-and-forget PTY input to a specific agent (not necessarily the selected one).
async function sendToAgent(id, data) {
  const res = await fetch("/api/agents/" + id + "/input", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ data })
  });
  if (!res.ok) toast.error("Send failed: " + (await res.text()));
}

// ─── Compact action rows (inline in the feed) ──
// One synthetic row per active interaction. Same 3-column grid as regular
// feed rows so they visually blend. The body holds a compact prompt, an
// optional single-line tool preview, and the buttons.
function actionShellHead(a, kind, klass) {
  return (
    '<div class="action ' + klass + '" data-id="' + esc(a.id) + '">' +
      '<div class="t">' + humanTime((a.pending && a.pending.at) || (a.askQuestion && a.askQuestion.at) || a.updatedAt) + '</div>' +
      '<div class="who"><div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div><span class="n">' + esc(agentName(a)) + '</span></div>'
  );
}
function renderActionPending(a) {
  const p = a.pending || {};
  const toolLine = p.tool
    ? '<div class="tool-prev"><span class="tn">' + esc(p.tool) + '</span>' + (p.toolInput && p.toolInput.command ? ' · ' + esc(truncateStr(p.toolInput.command, 90)) : (p.toolInput && p.toolInput.file_path ? ' · ' + esc(p.toolInput.file_path) : '')) + '</div>'
    : '';
  return (
    actionShellHead(a, "pending", "pending") +
      '<div class="body">' +
        '<div class="kind">Permission</div>' +
        (p.message ? '<div class="msg">' + esc(p.message) + '</div>' : '') +
        toolLine +
        '<div class="btns">' +
          '<button class="approve" data-act="approve" data-id="' + esc(a.id) + '">✓ Yes</button>' +
          '<button class="deny"    data-act="deny"    data-id="' + esc(a.id) + '">✕ No</button>' +
        '</div>' +
      '</div>' +
    '</div>'
  );
}
function renderActionAsk(a) {
  const items = a.askQuestion.questions;
  const bodyHTML = items.map((qi, qIdx) => {
    const opts = (qi.options || []).map((o, i) => (
      '<button class="opt" data-q="' + qIdx + '" data-idx="' + i + '" title="' + esc(o.description || "") + '">' +
        '<span class="idx">' + (i + 1) + '</span>' + esc(o.label) +
      '</button>'
    )).join('');
    return (
      '<div class="msg">' + esc(qi.question) + '</div>' +
      '<div class="btns">' + opts + '</div>' +
      (qi.multiSelect ? '<div class="tool-prev">multi-select — click options, then press ↵</div>' : '')
    );
  }).join('');
  return (
    actionShellHead(a, "ask", "ask") +
      '<div class="body">' +
        '<div class="kind">Claude is asking</div>' +
        bodyHTML +
        '<div class="navs">' +
          '<button data-nav="up">↑</button>' +
          '<button data-nav="down">↓</button>' +
          '<button data-nav="enter">↵</button>' +
          '<button data-nav="esc">Esc</button>' +
        '</div>' +
      '</div>' +
    '</div>'
  );
}
function renderActionMenu(a) {
  const opts = (a.menuOptions || []).map(o => (
    '<button class="opt' + (o.highlight ? ' hi' : '') + '" data-key="' + esc(o.key) + '">' +
      '<span class="idx">' + esc(o.key) + '</span>' + esc(o.label) +
    '</button>'
  )).join('');
  return (
    actionShellHead(a, "menu", "menu") +
      '<div class="body">' +
        '<div class="kind">Menu</div>' +
        '<div class="btns">' + opts + '</div>' +
      '</div>' +
    '</div>'
  );
}
function truncateStr(s, n) {
  if (!s || s.length <= n) return s || "";
  return s.slice(0, n) + "…";
}

function tileHero(lab, val, sub, spark, mod) {
  return '<span class="hero ' + (mod || '') + '"><span class="lab">' + esc(lab) + '</span><span class="val">' + esc(String(val)) + '</span>' + (sub ? '<span class="sub">· ' + esc(sub) + '</span>' : '') + '</span>';
}
function tileTok(lab, val, sub) {
  return '<div class="tok"><div class="lab">' + esc(lab) + '</div><div class="val">' + fmtNum(val) + '</div><div class="sub">' + esc(sub) + '</div></div>';
}
function tileTokCost(lab, val, sub) {
  return '<div class="tok cost"><div class="lab">' + esc(lab) + '</div><div class="val">' + esc(val) + '</div><div class="sub">' + esc(sub) + '</div></div>';
}

// ─── Sidebar (with user-defined order + drag) ───
// Persist manual order in localStorage. New agents append; missing ids in
// storage default to their spawn time.
const ORDER_KEY = "collectif.sidebarOrder";
function loadOrder() {
  try { return JSON.parse(localStorage.getItem(ORDER_KEY) || "[]"); } catch (_) { return []; }
}
function saveOrder(arr) { localStorage.setItem(ORDER_KEY, JSON.stringify(arr)); }
function sortedAgents() {
  const arr = Array.from(agents.values());
  const order = loadOrder();
  const idx = new Map(order.map((id, i) => [id, i]));
  return arr.sort((a, b) => {
    const ai = idx.has(a.id) ? idx.get(a.id) : 1e9;
    const bi = idx.has(b.id) ? idx.get(b.id) : 1e9;
    if (ai !== bi) return ai - bi;
    return a.createdAt.localeCompare(b.createdAt);
  });
}

function renderSidebar() {
  const arr = sortedAgents();
  if (arr.length === 0) {
    sidebar.innerHTML = '<div class="empty-sidebar">No agents yet.<br>Click <strong>+ Add Agent</strong> above to spawn one.</div>';
    return;
  }
  sidebar.innerHTML = "";
  for (const a of arr) {
    const c = document.createElement("div");
    c.className = "agent-card " + (a.status || "idle")
      + (selectedId === a.id ? " selected" : "")
      + (a.pending ? " pending" : "");
    c.dataset.id = a.id;
    c.draggable = true;
    const task = a.currentTask || a.prompt || "";
    const activityText = a.lastActivity || (a.lastTool ? "✓ " + a.lastTool : "");
    const tokTotal = (a.inputTokens || 0) + (a.outputTokens || 0);
    c.innerHTML = (
      '<button class="kill-btn" draggable="false" title="Kill agent">×</button>' +
      '<div class="card-head">' +
        '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="card-body">' +
          '<div class="card-name"><span class="name">' + esc(agentName(a)) + '</span>' +
          (a.pending ? '<span class="pending-badge">Action</span>' : '') +
          '<span class="age">' + humanAge(a.createdAt) + '</span></div>' +
          '<div class="card-cwd" title="' + esc(a.cwd) + '">' + esc(cwdBase(a.cwd)) + '</div>' +
          '<div class="card-status-row"><span class="status-pill ' + esc(a.status || "idle") + '"><span class="dot"></span>' + esc((a.status || "idle").replace("_", " ")) + '</span></div>' +
          (task ? '<div class="card-activity" title="' + esc(task) + '">▸ ' + esc(task) + '</div>' : '') +
          (activityText ? '<div class="card-activity">' + esc(activityText) + '</div>' : '') +
          (tokTotal > 0 ? '<div class="card-token">' + fmtNum(a.inputTokens || 0) + ' in · ' + fmtNum(a.outputTokens || 0) + ' out</div>' : '') +
        '</div>' +
      '</div>'
    );
    // Card click → select (but only if the click didn't originate on kill).
    c.addEventListener("click", (ev) => {
      if (ev.target.closest(".kill-btn")) return;
      selectAgent(a.id);
    });
    const killBtn = c.querySelector(".kill-btn");
    // Belt-and-braces: block mousedown/pointerdown too so no drag can start
    // from the button, no matter how the browser interprets the gesture.
    killBtn.addEventListener("mousedown", (ev) => ev.stopPropagation());
    killBtn.addEventListener("pointerdown", (ev) => ev.stopPropagation());
    killBtn.addEventListener("dragstart", (ev) => { ev.preventDefault(); ev.stopPropagation(); });
    armConfirmButton(killBtn, {
      armedLabel: "Click again to kill",
      onConfirm: async () => {
        try {
          const res = await fetch("/api/agents/" + a.id, { method: "DELETE" });
          if (!res.ok) toast.error("Kill failed: " + res.status + " " + (await res.text()));
        } catch (err) {
          toast.error("Kill failed: " + err.message);
        }
      },
    });

    // Drag & drop reordering — only when dragstart originates on the card
    // itself, not on interactive children (kill button already blocks).
    c.addEventListener("dragstart", (ev) => {
      if (ev.target.closest(".kill-btn")) { ev.preventDefault(); return; }
      ev.dataTransfer.effectAllowed = "move";
      ev.dataTransfer.setData("text/plain", a.id);
      c.classList.add("dragging");
    });
    c.addEventListener("dragend", () => {
      c.classList.remove("dragging");
      document.querySelectorAll(".agent-card").forEach(x => x.classList.remove("drop-before", "drop-after"));
    });
    c.addEventListener("dragover", (ev) => {
      ev.preventDefault();
      ev.dataTransfer.dropEffect = "move";
      const rect = c.getBoundingClientRect();
      const before = (ev.clientY - rect.top) < rect.height / 2;
      c.classList.toggle("drop-before", before);
      c.classList.toggle("drop-after", !before);
    });
    c.addEventListener("dragleave", () => {
      c.classList.remove("drop-before", "drop-after");
    });
    c.addEventListener("drop", (ev) => {
      ev.preventDefault();
      const draggedId = ev.dataTransfer.getData("text/plain");
      if (!draggedId || draggedId === a.id) return;
      const rect = c.getBoundingClientRect();
      const before = (ev.clientY - rect.top) < rect.height / 2;
      reorderAgents(draggedId, a.id, before);
    });

    sidebar.appendChild(c);
  }
}

function reorderAgents(movedId, pivotId, before) {
  const currentOrder = sortedAgents().map(a => a.id);
  const from = currentOrder.indexOf(movedId);
  if (from < 0) return;
  currentOrder.splice(from, 1);
  let to = currentOrder.indexOf(pivotId);
  if (to < 0) return;
  if (!before) to += 1;
  currentOrder.splice(to, 0, movedId);
  saveOrder(currentOrder);
  scheduleRender("sidebar");
}

// ─── Selection + embedded terminal panel ───────
// The dashboard always stays mounted. Selecting an agent just updates the
// LEFT term-panel (header + optional approval bar + xterm), unselecting
// (clicking the logo) shows the placeholder again.
function selectAgent(id) {
  if (selectedId === id) return;
  selectedId = id;
  renderSidebar();
  renderTermPanel(true);
  renderSendPanel();
}
