// All interactive UIs (permission, ask, menu) now render inline as synthetic
// rows at the top of the feed (renderFeed). No separate panels remain, so
// renderSendPanel is a no-op kept only because a few call sites still invoke
// it during selection/refresh.
function renderSendPanel() { /* intentionally empty */ }

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
// The notebook page has always linked here as `/?token=…#agent=<id>` — the
// D8' escape hatch, "show me the raw bytes behind this document" — and
// nothing on this side has ever read that fragment. The link opened the
// dashboard with nothing selected, which reads as the button being broken.
//
// Applied on the first snapshot because the agent map is empty before it,
// and the fragment is cleared afterwards so a later reload does not drag
// the user back to a session they have since navigated away from.
let agentDeepLinkPending = true;
function applyAgentDeepLink() {
  if (!agentDeepLinkPending) return;
  const m = /^#agent=([A-Za-z0-9_-]+)$/.exec(location.hash || "");
  if (!m) { agentDeepLinkPending = false; return; }
  if (!agents.has(m[1])) return; // the session may not be in this snapshot yet
  agentDeepLinkPending = false;
  selectAgent(m[1]);
  // The link asked for the terminal specifically. Guarded because the
  // session-view module may not have loaded; the notebook is the default
  // either way, which is a worse answer here but not a broken one.
  if (window.collectifSessionView) window.collectifSessionView.show("terminal");
  history.replaceState(null, "", location.pathname + location.search);
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
      // #36 Seed last-seen status on snapshot so we don't ping the user about
      // agents that were already in a notable state before we connected.
      if (window.collectifNotify) {
        for (const a of msg.agents) window.collectifNotify.seed(a);
      }
      applyAgentDeepLink();
    } else if (msg.type === "upsert") {
      agents.set(msg.agent.id, msg.agent);
      // #36 Fire a browser notification if this transition is notable and the
      // user has enabled notifications for this agent.
      if (window.collectifNotify) window.collectifNotify.maybeNotify(msg.agent);
    } else if (msg.type === "remove") {
      agents.delete(msg.id);
      if (selectedId === msg.id) {
        selectedId = null;
        teardownTerminal();
      }
    } else if (msg.type === "hourly_cost") {
      // #35 budget strip data.
      handleHourlyCost(msg);
    } else if (msg.type === "cost_warning") {
      // #35 toast + highlight the sidebar tile.
      handleCostWarning(msg);
    } else if (msg.type === "context_pressure") {
      // #42.1 toast when a session crosses 70% (warn) or 90% (critical).
      const a = agents.get(msg.id);
      const nm = a ? agentName(a) : msg.id.slice(0, 8);
      const pct = Math.round((msg.pct || 0) * 100);
      if (msg.level === "critical") toast.error(nm + " at " + pct + "% context — compaction imminent");
      else                          toast.info(nm + " at " + pct + "% context");
    } else if (msg.type === "attachment_sent" && window.collectifAttach) {
      window.collectifAttach.markAttachmentStatus(msg.id, msg.paths || [], "sent");
    } else if (msg.type === "attachment_seen" && window.collectifAttach) {
      window.collectifAttach.markAttachmentStatus(msg.id, msg.paths || [], "seen");
      if (msg.id === selectedId) toast.success("Attachment delivered ✓");
    } else if (msg.type === "attachment_stale" && window.collectifAttach) {
      window.collectifAttach.markAttachmentStatus(msg.id, msg.paths || [], "stale");
      if (msg.id === selectedId) toast.error("Attachment not read within 15s — retry?");
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

// ─── New-agent modal ────────────────────────────
// As-you-type cwd validation. Debounced 300 ms; hits GET /api/cwd/check and
// shows ✓/✕ in the .cwd-hint sibling. Also gates the Spawn button so the
// user can't submit a path we already know is invalid — see #45 finding 2.
// The gate is bypassed when the check endpoint itself is missing (old
// servers) so the server-side rejection stays the fallback.
let cwdInput = null;
let cwdHint = null;
let createBtn = null;
let cliInput = null;
let cwdValid = false;
let cwdCheckTimer = 0;
let cwdCheckSeq = 0;
let cwdCheckEndpointMissing = false;
function updateCreateBtn() {
  if (!createBtn || !cwdInput) return;
  const empty = !cwdInput.value.trim();
  // #46 Defensive: also refuse to spawn if the CLI selector somehow
  // ended up empty. Should never happen in practice (fetchCLIs
  // preselects and there's always at least "claude"), but guards
  // against a broken /api/cli response leaving the picker blank.
  const cliEmpty = !!cliInput && !cliInput.value;
  createBtn.disabled = empty || cliEmpty || (!cwdCheckEndpointMissing && !cwdValid);
}

// #46 Phase 3: CLI adapter cache. Fetched once at boot (idempotent — retried
// on modal open if the initial call failed) and shared with the rest of the
// UI via window.collectifCLIs so per-session panel renderers can look up
// capabilities by adapter name.
window.collectifCLIs = [];        // full /api/cli response, sorted default-first
window.collectifCLIByName = {};   // { "claude": {...}, "codex": {...}, ... }
const CLI_LAST_USED_KEY = "collectif.lastCLI";

async function fetchCLIs() {
  try {
    const res = await fetch("/api/cli");
    if (!res.ok) return null;
    const list = await res.json();
    if (!Array.isArray(list)) return null;
    window.collectifCLIs = list;
    const byName = {};
    for (const e of list) if (e && e.name) byName[e.name] = e;
    window.collectifCLIByName = byName;
    return list;
  } catch (_) {
    return null;
  }
}

// Populate the <select id="cli-input"> with one <option> per adapter. Called
// on modal open. If /api/cli hasn't been fetched yet (initial call failed or
// still in flight), fetches now and retries once. Preselects the last-used
// CLI from localStorage, falling back to the server-side default (marked with
// isDefault: true — always "claude" today).
async function populateCLIPicker() {
  if (!cliInput) return;
  let list = window.collectifCLIs;
  if (!list || list.length === 0) {
    list = await fetchCLIs();
  }
  if (!list || list.length === 0) {
    // Endpoint unreachable — degrade to a hidden "claude" default so
    // spawn still works. Old servers without /api/cli hit this path.
    cliInput.innerHTML = '<option value="claude" selected>claude</option>';
    updateCreateBtn();
    return;
  }
  // Determine preselection: last-used > default flag > first entry.
  let last = "";
  try { last = localStorage.getItem(CLI_LAST_USED_KEY) || ""; } catch (_) {}
  const has = list.some(e => e.name === last);
  if (!has) {
    const def = list.find(e => e.isDefault);
    last = def ? def.name : list[0].name;
  }
  cliInput.innerHTML = list.map(e => {
    const lab = e.version ? (e.name + " (" + e.version + ")") : e.name;
    const sel = e.name === last ? " selected" : "";
    return '<option value="' + esc(e.name) + '"' + sel + '>' + esc(lab) + '</option>';
  }).join("");
  updateCreateBtn();
}
async function validateCwd(path) {
  if (cwdCheckEndpointMissing) { updateCreateBtn(); return; }
  const seq = ++cwdCheckSeq;
  if (!path) { cwdHint.textContent = ""; cwdHint.className = "cwd-hint"; cwdValid = false; updateCreateBtn(); return; }
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
        cwdValid = false;
      } else {
        cwdCheckEndpointMissing = true;
        cwdHint.textContent = "";
        cwdHint.className = "cwd-hint";
      }
      updateCreateBtn();
      return;
    }
    let body = null;
    try { body = await res.json(); } catch (_) { /* ignore parse errs */ }
    if (res.ok && body && body.ok) {
      cwdHint.textContent = "✓ OK";
      cwdHint.className = "cwd-hint ok";
      cwdValid = true;
    } else {
      const msg = (body && body.error) || res.statusText || "invalid path";
      cwdHint.textContent = "✕ " + msg;
      cwdHint.className = "cwd-hint err";
      cwdValid = false;
    }
    updateCreateBtn();
  } catch (_) {
    // Network/parse error — silently hide (don't block form submission).
    if (seq !== cwdCheckSeq) return;
    cwdHint.textContent = "";
    cwdHint.className = "cwd-hint";
    cwdValid = false;
    updateCreateBtn();
  }
}

// ─── Boot orchestration ─────────────────────────
// Wire every DOM handler and kick off live-updates. Skipped when the auth
// screen is showing (that state replaces document.body, so any getElementById
// lookup would return null).
function boot() {
  cwdInput = document.getElementById("cwd-input");
  cwdHint = document.getElementById("cwd-hint");
  createBtn = document.getElementById("create-btn");
  cliInput = document.getElementById("cli-input");
  cwdInput.addEventListener("input", () => {
    if (cwdCheckTimer) clearTimeout(cwdCheckTimer);
    const path = cwdInput.value.trim();
    // Disable immediately on empty; the debounced validate will re-enable
    // once a non-empty path checks out. Preserves the previous validity
    // for non-empty typing so the button doesn't flicker per keystroke.
    if (!path) { cwdValid = false; updateCreateBtn(); }
    cwdCheckTimer = setTimeout(() => validateCwd(path), 300);
  });
  // #46 keep the Spawn button gate honest when the CLI selection changes
  // (empty selection blocks the button).
  if (cliInput) cliInput.addEventListener("change", updateCreateBtn);

  // #46 Prefetch the adapter list at boot so the modal opens without a
  // network wait. Non-blocking — populateCLIPicker retries on modal-open
  // if this initial fetch failed (offline start, endpoint 500, etc).
  fetchCLIs();

  document.getElementById("new-btn").onclick = () => {
    cwdInput.value = "";
    document.getElementById("prompt-input").value = "";
    const capIn = document.getElementById("cap-input");
    if (capIn) capIn.value = "";
    cwdHint.textContent = "";
    cwdHint.className = "cwd-hint";
    cwdValid = false;
    // #46 Populate the CLI picker each time the modal opens so version
    // changes surface without a page reload. Cache is in-memory so the
    // second open is essentially free.
    populateCLIPicker();
    updateCreateBtn();
    document.getElementById("modal").classList.add("show");
    cwdInput.focus();
  };
  document.getElementById("cancel-btn").onclick = () => document.getElementById("modal").classList.remove("show");
  document.getElementById("create-btn").onclick = async () => {
    const cwd = cwdInput.value.trim();
    const prompt = document.getElementById("prompt-input").value.trim();
    // #35 optional per-session cap.
    const capEl = document.getElementById("cap-input");
    const cost_cap_usd = capEl ? (parseFloat(capEl.value) || 0) : 0;
    // #46 CLI selection. Defaults to "claude" when the picker is missing
    // (e.g. old cached index.html served from a service worker) so the
    // request stays backward-compatible.
    const cli = cliInput ? (cliInput.value || "claude") : "claude";
    if (!cwd) { toast.error("cwd is required"); return; }
    const res = await fetch("/api/agents", {
      method: "POST",
      headers: {"Content-Type":"application/json"},
      body: JSON.stringify({cli, cwd, prompt, cost_cap_usd}),
    });
    if (!res.ok) { toast.error("Spawn failed: " + await res.text()); return; }
    // #46 Remember the last-used CLI so the next spawn preselects it.
    try { localStorage.setItem(CLI_LAST_USED_KEY, cli); } catch (_) {}
    document.getElementById("modal").classList.remove("show");
  };

  // Return to dashboard: click the header logo.
  document.querySelector("header h1").onclick = () => {
    selectedId = null;
    teardownTerminal();
    scheduleRender("all");
  };

  bootTerminal();
  bootTeam();
  bootAttach();

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
}

if (!window.AGENTCTL_NO_TOKEN) { boot(); }
