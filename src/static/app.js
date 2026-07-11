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
// shows ✓/✕ in the .cwd-hint sibling. Does NOT block form submission — a
// stale/absent endpoint just leaves the hint empty.
let cwdInput = null;
let cwdHint = null;
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

// ─── Boot orchestration ─────────────────────────
// Wire every DOM handler and kick off live-updates. Skipped when the auth
// screen is showing (that state replaces document.body, so any getElementById
// lookup would return null).
function boot() {
  cwdInput = document.getElementById("cwd-input");
  cwdHint = document.getElementById("cwd-hint");
  cwdInput.addEventListener("input", () => {
    if (cwdCheckTimer) clearTimeout(cwdCheckTimer);
    const path = cwdInput.value.trim();
    cwdCheckTimer = setTimeout(() => validateCwd(path), 300);
  });

  document.getElementById("new-btn").onclick = () => {
    cwdInput.value = "";
    document.getElementById("prompt-input").value = "";
    const capIn = document.getElementById("cap-input");
    if (capIn) capIn.value = "";
    cwdHint.textContent = "";
    cwdHint.className = "cwd-hint";
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
    if (!cwd) { toast.error("cwd is required"); return; }
    const res = await fetch("/api/agents", {
      method: "POST",
      headers: {"Content-Type":"application/json"},
      body: JSON.stringify({cwd, prompt, cost_cap_usd}),
    });
    if (!res.ok) { toast.error("Spawn failed: " + await res.text()); return; }
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
