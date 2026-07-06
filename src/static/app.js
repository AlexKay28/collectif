if (window.AGENTCTL_NO_TOKEN) { /* Auth screen is showing; skip app boot. */ } else {
const agents = new Map();
const sidebar = document.getElementById("sidebar");
let selectedId = null;

// Terminal state (single embedded terminal in the detail pane).
let term = null, termWS = null, fitAddon = null, termAgentId = null;

// ─── Helpers ────────────────────────────────────
function avatarURL(id) {
  return "https://api.dicebear.com/9.x/personas/svg?seed=" + encodeURIComponent(id) + "&backgroundColor=1c2128,161b22,30363d";
}
// ~60 × ~60 = ~3600 codenames, plus a 3-char hex suffix derived from a djb2
// hash of the agent id. Suffix disambiguates on the ~2% birthday-collision
// case at 20 agents, while keeping the visible name short enough to read.
const CODENAME_ADJ = [
  "swift","calm","brave","quick","clever","stoic","zesty","spry","keen","bright",
  "daring","fierce","wise","bold","mellow","nimble","glad","lively","dandy","witty",
  "sunny","frosty","misty","dusky","amber","cobalt","copper","silver","golden","ivory",
  "jade","opal","onyx","ruby","coral","stormy","breezy","cloudy","gentle","hearty",
  "jolly","merry","peppy","plucky","proud","quiet","rowdy","royal","rustic","snappy",
  "spicy","stellar","sturdy","tidy","upbeat","vivid","warm","wily","zealous","zippy",
];
const CODENAME_NOUN = [
  "otter","hawk","fox","badger","robin","lynx","heron","panda","goose","stag",
  "koala","raven","sable","tapir","sparrow","yak","ibex","marmot","gecko","newt",
  "beaver","bison","cheetah","cougar","crane","dolphin","eagle","falcon","finch","gerbil",
  "hare","hedgehog","ibis","jackal","kestrel","lemur","llama","magpie","mole","narwhal",
  "orca","osprey","panther","pelican","quail","raccoon","ram","salmon","seal","shrew",
  "swan","tanager","toucan","turtle","viper","walrus","weasel","wolf","wombat","zebra",
];
// djb2 (Bernstein) hash — deterministic, no crypto needed, spreads uniformly.
function hashID(s) {
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = (((h << 5) + h) + s.charCodeAt(i)) >>> 0;
  return h;
}
function agentName(a) {
  const h = hashID(a.id);
  const adj = CODENAME_ADJ[h % CODENAME_ADJ.length];
  const noun = CODENAME_NOUN[Math.floor(h / CODENAME_ADJ.length) % CODENAME_NOUN.length];
  // 3-char hex suffix from a different rotation of the hash so it doesn't
  // just mirror adj/noun choice — makes collisions visually obvious.
  const suffix = (((h >>> 12) ^ (h >>> 20)) & 0xfff).toString(16).padStart(3, "0");
  return adj + "-" + noun + "-" + suffix;
}
function humanAge(iso) {
  if (!iso) return "";
  const t = new Date(iso).getTime();
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 60) return s + "s";
  if (s < 3600) return Math.floor(s / 60) + "m";
  if (s < 86400) return Math.floor(s / 3600) + "h";
  return Math.floor(s / 86400) + "d";
}
function humanTime(iso) {
  if (!iso) return "";
  return new Date(iso).toTimeString().slice(0, 8);
}
function cwdBase(cwd) {
  if (!cwd) return "";
  const p = cwd.replace(/\/+$/, "").split("/");
  return p[p.length - 1] || cwd;
}
function esc(s) { return (s || "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;").replace(/'/g, "&#39;"); }
function fmtNum(n) { return (n || 0).toLocaleString(); }
// Smallest 1/2.5/5 × 10^k value >= n, with a floor of 1000.
function barScaleCeil(n) {
  const MIN = 1000;
  if (!(n > MIN)) return MIN;
  const mants = [1, 2.5, 5];
  const exp = Math.floor(Math.log10(n));
  for (let e = exp; e <= exp + 1; e++) {
    for (const m of mants) {
      const v = m * Math.pow(10, e);
      if (v >= n) return v;
    }
  }
  return Math.pow(10, exp + 2);
}
function topTools(counts, n) {
  return Object.entries(counts || {}).sort((a, b) => b[1] - a[1]).slice(0, n);
}

// ─── Toasts ─────────────────────────────────────
// Replacement for window.alert. Non-blocking, stacks bottom-right, auto-fades.
// toast.error / toast.info / toast.success — each holds 5s before dismissal.
const toast = (function () {
  let container = null;
  function ensure() {
    if (container) return container;
    container = document.createElement("div");
    container.id = "toast-container";
    document.body.appendChild(container);
    return container;
  }
  function show(msg, level) {
    const el = document.createElement("div");
    el.className = "toast " + level;
    el.textContent = msg;
    ensure().appendChild(el);
    // Force a reflow so the transition kicks in (Chrome/Safari batch style writes).
    requestAnimationFrame(() => el.classList.add("in"));
    const dismiss = () => {
      el.classList.remove("in");
      el.classList.add("out");
      setTimeout(() => el.remove(), 220);
    };
    setTimeout(dismiss, 5000);
    el.addEventListener("click", dismiss);
  }
  return {
    error:   (m) => show(String(m), "error"),
    info:    (m) => show(String(m), "info"),
    success: (m) => show(String(m), "success"),
  };
})();

// ─── Inline "click again to confirm" for destructive buttons ────
// Used by kill buttons in place of confirm(). First click swaps the button
// into a warning state and starts a 3s timer; second click within the window
// invokes onConfirm. If the timer elapses the button reverts.
function armConfirmButton(btn, opts) {
  const armedLabel = opts.armedLabel;
  const idleLabel = btn.textContent;
  const onConfirm = opts.onConfirm;
  const state = { armed: false, timer: 0 };
  btn.addEventListener("click", (ev) => {
    ev.stopPropagation();
    ev.preventDefault();
    if (state.armed) {
      clearTimeout(state.timer);
      state.armed = false;
      btn.classList.remove("confirming");
      btn.textContent = idleLabel;
      onConfirm();
      return;
    }
    state.armed = true;
    btn.classList.add("confirming");
    btn.textContent = armedLabel;
    state.timer = setTimeout(() => {
      state.armed = false;
      btn.classList.remove("confirming");
      btn.textContent = idleLabel;
    }, 3000);
  });
}

// ─── Dashboard render batcher ───────────────────
// Every WS message / trend tick used to call renderDashboard() directly, which
// rebuilt every section synchronously. Split into per-section renderers and
// coalesce dirty sections into a single rAF so bursts of WS messages produce
// exactly one paint. `scheduleRender('all')` invalidates every section.
const SECTIONS = ["stats", "sidebar", "dag", "pending", "trends", "tokens", "tokensByAgent", "feed", "toolUsage", "term"];
const dirty = new Set();
let rafPending = false;
function scheduleRender(section) {
  if (section === "all") { for (const s of SECTIONS) dirty.add(s); }
  else dirty.add(section);
  if (rafPending) return;
  rafPending = true;
  requestAnimationFrame(flushRender);
}
function flushRender() {
  rafPending = false;
  // Snapshot + clear so any dirty flags raised during rendering flow into the
  // next frame instead of being dropped.
  const now = new Set(dirty);
  dirty.clear();
  if (now.has("stats"))         renderStats();
  if (now.has("sidebar"))       renderSidebar();
  if (now.has("dag"))           renderDAG();
  if (now.has("pending"))       renderPending();
  if (now.has("trends"))        renderTrends();
  if (now.has("tokens"))        renderTokens();
  if (now.has("tokensByAgent")) renderTokensByAgent();
  if (now.has("feed"))          renderFeed();
  if (now.has("toolUsage"))     renderToolUsage();
  if (now.has("term"))          renderTermPanel(false);
}

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

// ─── Cost estimate (rough Sonnet 4.6 pricing) ───
// Input $3/M · Output $15/M · Cache read $0.30/M · Cache write $3.75/M
function estimateCost(a) {
  return ((a.inputTokens || 0) * 3
        + (a.outputTokens || 0) * 15
        + (a.cacheReadTokens || 0) * 0.30
        + (a.cacheCreationTokens || 0) * 3.75) / 1_000_000;
}
function fmtCost(usd) {
  if (usd < 0.01) return "$" + usd.toFixed(4);
  if (usd < 1) return "$" + usd.toFixed(3);
  return "$" + usd.toFixed(2);
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

// ─── Agent DAG (overview) ───────────────────────
// Layout: recursive Reingold-Tilford-lite. Each subtree computes its total
// width from its leaves; parents sit centered over their children. Multiple
// roots (independent user-spawned agents) are laid out side-by-side.
// Nodes are absolutely-positioned HTML for reuse of the existing CSS;
// edges are an SVG overlay in the same coordinate space.
const DAG_NODE_W = 200;
const DAG_NODE_H = 56;
const DAG_X_GAP  = 22;
const DAG_Y_GAP  = 42;

function buildDAGForest() {
  const arr = Array.from(agents.values());
  const byId = new Map(arr.map(a => [a.id, a]));
  const children = new Map();
  for (const a of arr) {
    const p = a.parentId && byId.has(a.parentId) ? a.parentId : null;
    if (!children.has(p)) children.set(p, []);
    children.get(p).push(a);
  }
  for (const [, list] of children) {
    list.sort((x, y) => (x.createdAt || "").localeCompare(y.createdAt || ""));
  }
  const roots = children.get(null) || [];
  return { roots, children };
}
function layoutSubtree(node, children, x, y, positions) {
  const kids = children.get(node.id) || [];
  if (kids.length === 0) {
    positions.set(node.id, { x, y });
    return DAG_NODE_W;
  }
  const childY = y + DAG_NODE_H + DAG_Y_GAP;
  let childX = x;
  const widths = [];
  for (const k of kids) {
    const w = layoutSubtree(k, children, childX, childY, positions);
    widths.push(w);
    childX += w + DAG_X_GAP;
  }
  const totalW = widths.reduce((s, w) => s + w, 0) + DAG_X_GAP * (kids.length - 1);
  positions.set(node.id, { x: x + totalW / 2 - DAG_NODE_W / 2, y });
  return Math.max(DAG_NODE_W, totalW);
}
function renderDAG() {
  const wrap = document.getElementById("dash-dag-wrap");
  const host = document.getElementById("dash-dag");
  const arr = Array.from(agents.values());
  document.getElementById("dag-count").textContent = arr.length;
  if (arr.length === 0) { wrap.style.display = "none"; return; }
  wrap.style.display = "";

  const { roots, children } = buildDAGForest();
  const positions = new Map();
  let cursorX = 0;
  let maxDepth = 0;
  const measureDepth = (n, d) => {
    maxDepth = Math.max(maxDepth, d);
    for (const k of children.get(n.id) || []) measureDepth(k, d + 1);
  };
  for (const r of roots) {
    const w = layoutSubtree(r, children, cursorX, 0, positions);
    cursorX += w + DAG_X_GAP * 2;
    measureDepth(r, 0);
  }
  const totalW = Math.max(DAG_NODE_W, cursorX - DAG_X_GAP * 2);
  const totalH = (maxDepth + 1) * DAG_NODE_H + maxDepth * DAG_Y_GAP;

  const edges = [];
  for (const a of arr) {
    if (!a.parentId) continue;
    const p = positions.get(a.parentId);
    const c = positions.get(a.id);
    if (!p || !c) continue;
    const x1 = p.x + DAG_NODE_W / 2, y1 = p.y + DAG_NODE_H;
    const x2 = c.x + DAG_NODE_W / 2, y2 = c.y;
    const mid = (y1 + y2) / 2;
    edges.push('<path class="edge" d="M ' + x1 + ',' + y1 + ' C ' + x1 + ',' + mid + ' ' + x2 + ',' + mid + ' ' + x2 + ',' + y2 + '" marker-end="url(#dag-arrow)"/>');
  }

  const nodes = arr.map(a => {
    const pos = positions.get(a.id);
    if (!pos) return "";
    const status = a.status || "idle";
    const activity = a.lastActivity || (a.lastTool ? "✓ " + a.lastTool : "");
    const cls = "dag-node " + status + (a.pending ? " pending" : "");
    return (
      '<div class="' + cls + '" data-id="' + esc(a.id) + '" style="left:' + pos.x + 'px;top:' + pos.y + 'px;width:' + DAG_NODE_W + 'px;height:' + DAG_NODE_H + 'px">' +
        '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="info">' +
          '<div class="name">' + esc(agentName(a)) + '</div>' +
          '<div class="sub">' + esc(activity || cwdBase(a.cwd)) + '</div>' +
        '</div>' +
      '</div>'
    );
  }).join('');

  host.innerHTML =
    '<div class="canvas" style="width:' + totalW + 'px;height:' + totalH + 'px">' +
      '<svg width="' + totalW + '" height="' + totalH + '">' +
        '<defs>' +
          '<marker id="dag-arrow" markerWidth="10" markerHeight="10" refX="9" refY="3" orient="auto" markerUnits="strokeWidth">' +
            '<path class="arrow" d="M0,0 L0,6 L9,3 z"/>' +
          '</marker>' +
        '</defs>' +
        edges.join('') +
      '</svg>' +
      nodes +
    '</div>';

  host.querySelectorAll(".dag-node").forEach(n => {
    n.onclick = () => selectAgent(n.dataset.id);
  });
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

// ─── Trend sampling + sparkline drawing ─────────
// Sample every 5s, keep last 120 points (10 minutes). Total tokens = sum of
// (input + output) across all agents; active = agents with status=running.
const trend = [];
function sampleTrend() {
  let active = 0, tokens = 0;
  for (const a of agents.values()) {
    if (a.status === "running") active++;
    tokens += (a.inputTokens || 0) + (a.outputTokens || 0);
  }
  trend.push({ t: Date.now(), active, tokens });
  if (trend.length > 120) trend.shift();
}
function drawSparkline(id, values, opts) {
  opts = opts || {};
  const svg = document.getElementById(id);
  if (!svg) return;
  const W = 300, H = 64, PAD = 4;
  if (!values || values.length < 2) {
    svg.innerHTML = '<text class="axis" x="' + (W/2) + '" y="' + (H/2) + '" text-anchor="middle" dominant-baseline="middle">not enough data yet</text>';
    return;
  }
  const yMin = opts.yMin !== undefined ? opts.yMin : Math.min(...values);
  let yMax = Math.max(...values);
  if (yMax === yMin) yMax = yMin + 1;
  const stepX = (W - 2 * PAD) / (values.length - 1);
  const toY = v => H - PAD - ((v - yMin) / (yMax - yMin)) * (H - 2 * PAD);
  const pts = values.map((v, i) => (PAD + i * stepX).toFixed(1) + "," + toY(v).toFixed(1));
  const line = "M " + pts.join(" L ");
  const area = line + " L " + (PAD + (values.length - 1) * stepX).toFixed(1) + "," + (H - PAD) + " L " + PAD + "," + (H - PAD) + " Z";
  svg.innerHTML =
    '<path class="area" d="' + area + '"/>' +
    '<path class="line" d="' + line + '"/>' +
    '<text class="axis" x="' + PAD + '" y="12">' + fmtNum(yMax) + '</text>' +
    '<text class="axis" x="' + PAD + '" y="' + (H - 4) + '">' + fmtNum(yMin) + '</text>';
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
    sidebar.innerHTML = '<div class="empty-sidebar">No agents yet.<br>Click <strong>+ New Agent</strong> to spawn one.</div>';
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
    let childCount = 0;
    for (const other of agents.values()) if (other.parentId === a.id) childCount++;
    c.innerHTML = (
      '<button class="kill-btn" draggable="false" title="Kill agent">×</button>' +
      '<div class="card-head">' +
        '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="card-body">' +
          '<div class="card-name"><span class="name">' + esc(agentName(a)) + '</span>' +
          (a.pending ? '<span class="pending-badge">Action</span>' : '') +
          (childCount > 0 ? '<span class="child-badge" title="Has children">↳ ' + childCount + '</span>' : '') +
          (a.parentId ? '<span class="parent-badge" title="Has a parent agent">↑</span>' : '') +
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

function renderTermPanel(mountTerminal) {
  const a = selectedId ? agents.get(selectedId) : null;
  const empty = document.getElementById("term-empty");
  const head = document.getElementById("term-head");
  const body = document.getElementById("term-body");
  if (!a) {
    empty.style.display = "flex";
    head.style.display = "none";
    body.style.display = "none";
    teardownTerminal();
    return;
  }
  empty.style.display = "none";
  head.style.display = "flex";
  body.style.display = "block";

  document.getElementById("d-avatar").src = avatarURL(a.id);
  document.getElementById("d-name").textContent = agentName(a);
  document.getElementById("d-cwd").textContent = a.cwd;
  const status = a.status || "idle";
  document.getElementById("d-status").innerHTML = '<span class="status-pill ' + esc(status) + '"><span class="dot"></span>' + esc(status.replace("_", " ")) + '</span>';
  const task = a.currentTask || a.prompt || "";
  document.getElementById("d-task").textContent = task ? "▸ " + task : "";
  renderFamilyLine(a);
  renderSubagentsStrip(a);

  if (mountTerminal) mountTerminalFor(a.id);
  // If we didn't remount, the terminal is still bound to the previous agent.
  // Fit again in case the panel resized.
  requestAnimationFrame(() => { fitTerminalNow(); });
}

// Family strip in the terminal head: ↑ parent · ↳ children. Only visible
// when the selected agent has a parent or a child; both are clickable to
// drill into the related agent.
function renderFamilyLine(a) {
  const el = document.getElementById("d-family");
  if (!el) return;
  const parent = a.parentId ? agents.get(a.parentId) : null;
  const children = [];
  for (const other of agents.values()) {
    if (other.parentId === a.id) children.push(other);
  }
  if (!parent && children.length === 0) { el.style.display = "none"; el.innerHTML = ""; return; }
  el.style.display = "";
  const chip = (rel, other) =>
    '<button class="family-chip" data-id="' + esc(other.id) + '" title="' + esc(other.cwd || "") + '">' +
      rel + ' <span class="mini-avatar"><img src="' + avatarURL(other.id) + '" alt=""></span> ' +
      esc(agentName(other)) +
    '</button>';
  const parts = [];
  if (parent) parts.push(chip("↑", parent));
  for (const c of children) parts.push(chip("↳", c));
  el.innerHTML = parts.join(" ");
  el.querySelectorAll(".family-chip").forEach(btn => {
    btn.onclick = () => selectAgent(btn.dataset.id);
  });
}

// Sub-agents info panel in the terminal head. Read-only: one row per direct
// child showing status + task + tokens + last activity. Click a row to drill
// into that child. Any control over these children happens naturally in
// this parent's own conversation — not from a dashboard button strip.
function renderSubagentsStrip(a) {
  const el = document.getElementById("d-subagents");
  if (!el) return;
  const children = [];
  for (const other of agents.values()) {
    if (other.parentId === a.id) children.push(other);
  }
  if (children.length === 0) { el.style.display = "none"; el.innerHTML = ""; return; }
  el.style.display = "";
  const rows = children.map(c => {
    const status = c.status || "idle";
    const task = c.currentTask || c.prompt || "";
    const activity = c.lastActivity || (c.lastTool ? "✓ " + c.lastTool : "");
    const desc = task ? '<span class="sa-task">▸ ' + esc(task) + '</span>' + (activity ? " · " + esc(activity) : "")
                     : (activity ? esc(activity) : esc(cwdBase(c.cwd)));
    const tok = (c.inputTokens || 0) + (c.outputTokens || 0);
    return (
      '<div class="subagent-row ' + status + '" data-id="' + esc(c.id) + '">' +
        '<span class="sa-avatar"><img src="' + avatarURL(c.id) + '" alt=""></span>' +
        '<div class="sa-info">' +
          '<span class="sa-name">' + esc(agentName(c)) + '</span>' +
          '<span style="color:var(--muted); font-size: 10.5px"> · ' + esc(status.replace("_", " ")) + '</span>' +
          '<div class="sa-desc">' + desc + '</div>' +
        '</div>' +
        '<div class="sa-tok">' + (tok > 0 ? fmtNum(tok) + " tok" : "") + '</div>' +
      '</div>'
    );
  }).join('');
  el.innerHTML = '<div class="sa-lab">Sub-agents (' + children.length + ')</div>' + rows;
  el.querySelectorAll(".subagent-row").forEach(r => {
    r.onclick = () => selectAgent(r.dataset.id);
  });
}

// ─── Embedded terminal ──────────────────────────
let termResizeObserver = null;
let termResizeTimer = null;
let lastCols = 0, lastRows = 0;
let termGen = 0;
let termBackoff = 1000;
const TERM_BACKOFF_MAX = 15000;

function teardownTerminal() {
  termGen++;
  window.removeEventListener("resize", onTermResize);
  if (termResizeObserver) { termResizeObserver.disconnect(); termResizeObserver = null; }
  if (termResizeTimer) { clearTimeout(termResizeTimer); termResizeTimer = null; }
  if (termWS) { try { termWS.close(); } catch(e){} termWS = null; }
  if (term) { term.dispose(); term = null; }
  termAgentId = null;
  lastCols = 0; lastRows = 0;
  document.getElementById("term-container").innerHTML = "";
}
// Fit + notify PTY. Debounced so a stream of resize events fires exactly one
// round-trip. Without POST /resize, Claude keeps drawing at the initial size
// and the display gets garbled as the container changes.
function fitTerminalNow() {
  if (!fitAddon || !term) return;
  try { fitAddon.fit(); } catch (_) { return; }
  const cols = term.cols, rows = term.rows;
  if (cols === lastCols && rows === lastRows) return;
  lastCols = cols; lastRows = rows;
  if (!termAgentId) return;
  fetch("/api/agents/" + termAgentId + "/resize", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ cols, rows })
  }).catch(() => { /* best-effort; visual fit already applied */ });
}
function onTermResize() {
  if (termResizeTimer) clearTimeout(termResizeTimer);
  termResizeTimer = setTimeout(() => { termResizeTimer = null; fitTerminalNow(); }, 80);
}
function connectTerminalWS(id, gen) {
  // Only (re)connect if we're still mounted for the same agent + generation.
  if (termAgentId !== id || !term || gen !== termGen) return;
  // The server flushes the ring buffer as the first frames; after that first
  // write, scroll to the bottom so the user lands at the current prompt.
  let justConnected = true;
  const ws = new WebSocket(wsURL("/ws/session/" + id));
  termWS = ws;
  ws.binaryType = "arraybuffer";
  ws.onopen = () => { termBackoff = 1000; };
  ws.onmessage = (ev) => {
    if (gen !== termGen || !term) return;
    if (typeof ev.data === "string") term.write(ev.data);
    else term.write(new Uint8Array(ev.data));
    if (justConnected) {
      justConnected = false;
      requestAnimationFrame(() => { if (term && gen === termGen) term.scrollToBottom(); });
    }
  };
  ws.onclose = () => {
    if (gen !== termGen || termAgentId !== id || !term) return;
    term.write("\r\n[disconnected, reconnecting…]\r\n");
    setTimeout(() => connectTerminalWS(id, gen), termBackoff);
    termBackoff = Math.min(termBackoff * 2, TERM_BACKOFF_MAX);
  };
  ws.onerror = () => { try { ws.close(); } catch (_) {} };
}
function mountTerminalFor(id) {
  if (termAgentId === id && term) return;
  teardownTerminal();
  termAgentId = id;
  const container = document.getElementById("term-container");
  term = new Terminal({ cursorBlink: true, fontFamily: "ui-monospace, Menlo, monospace", fontSize: 13, theme: { background: "#000000" } });
  fitAddon = new FitAddon.FitAddon();
  term.loadAddon(fitAddon);
  term.open(container);
  term.scrollToBottom();
  requestAnimationFrame(() => fitTerminalNow());
  window.addEventListener("resize", onTermResize);
  // Watch the container itself so column/panel resizes (not just window
  // resizes) also refit. Fires on any layout change that affects our box.
  termResizeObserver = new ResizeObserver(onTermResize);
  termResizeObserver.observe(container);
  const body = document.getElementById("term-body");
  if (body) termResizeObserver.observe(body);

  termBackoff = 1000;
  const gen = ++termGen;
  connectTerminalWS(id, gen);
  term.onData((d) => { if (termWS && termWS.readyState === 1) termWS.send(d); });
}


// ─── Auto-detected menu panel (right column) ────
// Server scans the selected agent's PTY output for numbered menus and
// publishes them as `menuOptions`. Each entry becomes a click-to-send button.
// Clicking sends "<digit>\r" to the PTY, which any TUI select accepts.
async function sendToPTY(data) {
  if (!selectedId || !data) return false;
  const res = await fetch("/api/agents/" + selectedId + "/input", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ data })
  });
  if (!res.ok) { toast.error("Send failed: " + (await res.text())); return false; }
  return true;
}
// All interactive UIs (permission, ask, menu) now render inline as synthetic
// rows at the top of the feed (renderFeed). No separate panels remain, so
// renderSendPanel is a no-op kept only because a few call sites still invoke
// it during selection/refresh.
function renderSendPanel() { /* intentionally empty */ }

// Kill button in the terminal panel head — inline "click again" state instead
// of a browser confirm(). Wired once at load.
armConfirmButton(document.getElementById("d-kill"), {
  armedLabel: "Click again to kill",
  onConfirm: async () => {
    if (!selectedId) return;
    try {
      const res = await fetch("/api/agents/" + selectedId, { method: "DELETE" });
      if (!res.ok) toast.error("Kill failed: " + res.status + " " + (await res.text()));
    } catch (err) { toast.error("Kill failed: " + err.message); }
  },
});

// ─── New-agent modal ────────────────────────────
// As-you-type cwd validation. Debounced 300 ms; hits GET /api/cwd/check and
// shows ✓/✕ in the .cwd-hint sibling. Does NOT block form submission — a
// stale/absent endpoint just leaves the hint empty.
const cwdInput = document.getElementById("cwd-input");
const cwdHint = document.getElementById("cwd-hint");
let cwdCheckTimer = 0;
let cwdCheckSeq = 0;
let cwdCheckEndpointMissing = false;
async function validateCwd(path) {
  if (cwdCheckEndpointMissing) return; // endpoint isn't there — stay silent.
  const seq = ++cwdCheckSeq;
  if (!path) { cwdHint.textContent = ""; cwdHint.className = "cwd-hint"; return; }
  try {
    const res = await fetch("/api/cwd/check?path=" + encodeURIComponent(path));
    if (seq !== cwdCheckSeq) return; // a newer keystroke has since fired.
    if (res.status === 404) {
      // Might be missing dir OR missing endpoint — inspect the body to
      // decide, since the response for the endpoint-itself is plain text.
      let body = null;
      try { body = await res.json(); } catch (_) { /* not JSON */ }
      if (body && typeof body.ok === "boolean") {
        cwdHint.textContent = "✕ " + (body.error || "not a directory");
        cwdHint.className = "cwd-hint err";
      } else {
        cwdCheckEndpointMissing = true;
        cwdHint.textContent = "";
        cwdHint.className = "cwd-hint";
      }
      return;
    }
    let body = null;
    try { body = await res.json(); } catch (_) { /* ignore parse errs */ }
    if (res.ok && body && body.ok) {
      cwdHint.textContent = "✓ OK";
      cwdHint.className = "cwd-hint ok";
    } else {
      const msg = (body && body.error) || res.statusText || "invalid path";
      cwdHint.textContent = "✕ " + msg;
      cwdHint.className = "cwd-hint err";
    }
  } catch (_) {
    // Network/parse error — silently hide (don't block form submission).
    if (seq !== cwdCheckSeq) return;
    cwdHint.textContent = "";
    cwdHint.className = "cwd-hint";
  }
}
cwdInput.addEventListener("input", () => {
  if (cwdCheckTimer) clearTimeout(cwdCheckTimer);
  const path = cwdInput.value.trim();
  cwdCheckTimer = setTimeout(() => validateCwd(path), 300);
});

// Spawn modal: parentAgentID is set when the user opens the modal via the
// terminal head's "+ Child" button. Cleared for the top-bar "+ New Agent".
let modalParentAgentID = "";
function openSpawnModal(parentAgent) {
  modalParentAgentID = parentAgent ? parentAgent.id : "";
  cwdInput.value = parentAgent && parentAgent.cwd ? parentAgent.cwd : "";
  document.getElementById("prompt-input").value = "";
  cwdHint.textContent = "";
  cwdHint.className = "cwd-hint";
  document.getElementById("modal-title").textContent =
    parentAgent ? "Spawn a child agent" : "Spawn a new agent";
  const hint = document.getElementById("modal-parent-hint");
  if (parentAgent) {
    hint.textContent = "Parent: " + agentName(parentAgent) + " (" + parentAgent.id.slice(0, 8) + ")";
    hint.style.display = "";
  } else {
    hint.style.display = "none";
    hint.textContent = "";
  }
  document.getElementById("modal").classList.add("show");
  cwdInput.focus();
}
document.getElementById("new-btn").onclick = () => openSpawnModal(null);
document.getElementById("d-child").onclick = () => {
  const parent = selectedId ? agents.get(selectedId) : null;
  if (!parent) { toast.error("Select an agent to spawn a child of it"); return; }
  openSpawnModal(parent);
};
document.getElementById("cancel-btn").onclick = () => document.getElementById("modal").classList.remove("show");
document.getElementById("create-btn").onclick = async () => {
  const cwd = cwdInput.value.trim();
  const prompt = document.getElementById("prompt-input").value.trim();
  if (!cwd) { toast.error("cwd is required"); return; }
  const body = { cwd, prompt };
  if (modalParentAgentID) body.parentAgentID = modalParentAgentID;
  const res = await fetch("/api/agents", { method: "POST", headers: {"Content-Type":"application/json"}, body: JSON.stringify(body) });
  if (!res.ok) { toast.error("Spawn failed: " + await res.text()); return; }
  document.getElementById("modal").classList.remove("show");
  modalParentAgentID = "";
};

// Return to dashboard: click the header logo.
document.querySelector("header h1").onclick = () => {
  selectedId = null;
  teardownTerminal();
  scheduleRender("all");
};

// ─── Live updates ───────────────────────────────
// Reconnecting dashboard WS with exponential backoff (1s → 15s). On reconnect
// the server sends a fresh `snapshot`, which the existing handler applies —
// so no extra resync logic is needed.
let dashWS = null;
let dashBackoff = 1000;
const DASH_BACKOFF_MAX = 15000;
function setConnIndicator(reconnecting) {
  const el = document.getElementById("conn-indicator");
  if (el) el.style.display = reconnecting ? "" : "none";
}
function connectDashboardWS() {
  dashWS = new WebSocket(wsURL("/ws/dashboard"));
  dashWS.onopen = () => {
    dashBackoff = 1000;
    setConnIndicator(false);
  };
  dashWS.onmessage = (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.type === "snapshot") {
      agents.clear();
      for (const a of msg.agents) agents.set(a.id, a);
    } else if (msg.type === "upsert") {
      agents.set(msg.agent.id, msg.agent);
    } else if (msg.type === "remove") {
      agents.delete(msg.id);
      if (selectedId === msg.id) {
        selectedId = null;
        teardownTerminal();
      }
    }
    scheduleRender("all");
  };
  const scheduleReconnect = () => {
    setConnIndicator(true);
    setTimeout(connectDashboardWS, dashBackoff);
    dashBackoff = Math.min(dashBackoff * 2, DASH_BACKOFF_MAX);
  };
  dashWS.onclose = scheduleReconnect;
  dashWS.onerror = () => { try { dashWS.close(); } catch (_) {} };
}
connectDashboardWS();

// Trend sampling every 5s. Sampling ticks continue running even while an
// agent is selected so the dashboard is up-to-date when you return.
setInterval(() => {
  sampleTrend();
  scheduleRender("all");
}, 5000);

// Initial render (dashboard view — no selection) plus a first sample so the
// sparklines have at least one point on load.
sampleTrend();
scheduleRender("all");
} // AGENTCTL_NO_TOKEN guard
