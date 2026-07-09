const agents = new Map();
const sidebar = document.getElementById("sidebar-list");
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
const SECTIONS = ["stats", "sidebar", "pending", "trends", "tokens", "tokensByAgent", "feed", "toolUsage", "term", "budget"];
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
  if (now.has("pending"))       renderPending();
  if (now.has("trends"))        renderTrends();
  if (now.has("tokens"))        renderTokens();
  if (now.has("tokensByAgent")) renderTokensByAgent();
  if (now.has("feed"))          renderFeed();
  if (now.has("toolUsage"))     renderToolUsage();
  if (now.has("term"))          renderTermPanel(false);
  if (now.has("budget"))        renderBudgetStrip();
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
