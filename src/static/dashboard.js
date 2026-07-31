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
  // #38 Fleet-wide output split. Server already pro-rates per-agent; we
  // just sum the resulting per-block-type token counts so the Overview
  // tile can render a stacked bar without re-doing the math client-side.
  let totalOutThink = 0, totalOutText = 0, totalOutTool = 0;
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
    totalOutThink += a.outputThinkingTokens || 0;
    totalOutText  += a.outputTextTokens || 0;
    totalOutTool  += a.outputToolTokens || 0;
    if (a.pending) pending.push(a);
    for (const [t, c] of Object.entries(a.toolCounts || {})) toolAgg[t] = (toolAgg[t] || 0) + c;
  }
  const totalCost = arr.reduce((s, a) => s + estimateCost(a), 0);
  return { arr, active, waiting, idle, err, stopped, recentlyActive,
           totalIn, totalOut, totalCR, totalCC, totalMsgs, totalCost, pending, toolAgg,
           totalOutThink, totalOutText, totalOutTool };
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
  // #38 The Output tile now carries a stacked bar split into thinking /
  // text / tool_use. Fall back to the flat number when we have no char
  // signal yet (session pre-dates the upgrade, or hasn't produced any
  // typed content). Keep the lump-sum big number above the bar so the
  // familiar "N generated tokens" is still visible at a glance.
  document.getElementById("dash-tokens").innerHTML = (
    tileTok("Input", s.totalIn, "prompt tokens") +
    tileTokSplit("Output", s.totalOut, s.totalOutThink, s.totalOutText, s.totalOutTool) +
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
  // #38 The per-agent bar keeps its length (proportional to total in+out
  // tokens) but the OUTPUT slice within it is now sub-segmented into
  // thinking / text / tool_use using the pro-rated counts from the API.
  // Input tokens keep their existing "input" colour so the bar is still
  // dominated by whichever number is dominant in absolute terms.
  document.getElementById("dash-bars").innerHTML = arr.length === 0
    ? '<div style="color: var(--muted); font-size: 12px">no agents</div>'
    : arr.slice().sort((a, b) => ((b.inputTokens || 0) + (b.outputTokens || 0)) - ((a.inputTokens || 0) + (a.outputTokens || 0)))
        .map(a => {
          const inp = a.inputTokens || 0;
          const out = a.outputTokens || 0;
          const tok = inp + out;
          const pct = tok <= 0 ? 0 : Math.max(1, Math.round((tok / maxTok) * 100));
          // Sub-segment widths inside the row's filled portion. When
          // outputThinking/text/tool are all zero (no signal yet), we
          // fall back to a single output segment.
          const think = a.outputThinkingTokens || 0;
          const text  = a.outputTextTokens || 0;
          const tool  = a.outputToolTokens || 0;
          const segs = renderRowOutputSegments(tok, inp, out, think, text, tool);
          return (
            '<button type="button" class="row" aria-label="Open agent ' + esc(agentName(a)) + ' — ' + fmtNum(tok) + ' tokens" data-id="' + esc(a.id) + '">' +
              '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
              '<div class="track"><div class="fill" style="width:' + pct + '%">' + segs + '</div><span class="lbl">' + esc(agentName(a)) + '</span></div>' +
              '<div class="num">' + fmtNum(tok) + '</div>' +
            '</button>'
          );
        }).join('');
  document.querySelectorAll("#dash-bars .row").forEach(r => {
    r.onclick = () => selectAgent(r.dataset.id);
    r.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      ev.preventDefault();
      selectAgent(r.dataset.id);
    });
  });
}

// #38 Render the four coloured sub-segments (input, thinking, text, tool)
// that fill an agent's row bar. Percentages are normalised against the
// row's own token total so they always fill exactly 100% of the .fill
// container. Zero-width segments are omitted so the CSS border-radius
// on the outer element renders cleanly for the visible segments.
function renderRowOutputSegments(tok, inp, out, think, text, tool) {
  if (tok <= 0) return "";
  const parts = [];
  const pushSeg = (kind, val, title) => {
    if (val <= 0) return;
    const p = (val / tok) * 100;
    parts.push('<span class="seg ' + kind + '" style="width:' + p.toFixed(2) + '%" title="' + esc(title) + '"></span>');
  };
  pushSeg("in", inp, fmtNum(inp) + " input tokens");
  // If server gave us a per-block-type split, use it; otherwise render a
  // single "output" segment so pre-#38 sessions still look right.
  if (think + text + tool > 0) {
    pushSeg("think", think, fmtNum(think) + " thinking tokens (approx)");
    pushSeg("text",  text,  fmtNum(text) + " text tokens (approx)");
    pushSeg("tool",  tool,  fmtNum(tool) + " tool_use tokens (approx)");
  } else {
    pushSeg("out", out, fmtNum(out) + " output tokens");
  }
  return parts.join("");
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
      '<button type="button" class="row' + (isNew ? ' new' : '') + '" aria-label="Open ' + esc(agentName(e.agent)) + ' — ' + esc(e.event || "event") + (e.tool ? ' ' + esc(e.tool) : '') + '" data-id="' + esc(e.agent.id) + '">' +
        '<span class="t">' + humanTime(e.t) + '</span>' +
        '<span class="who"><span class="avatar"><img src="' + avatarURL(e.agent.id) + '" alt=""></span><span class="n">' + esc(agentName(e.agent)) + '</span></span>' +
        '<span class="what ' + esc(lvl) + '">' + what + '</span>' +
      '</button>'
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
    r.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      if (ev.target.closest(".btns-inline")) return;
      ev.preventDefault();
      selectAgent(r.dataset.id);
    });
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
    r.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      if (ev.target.closest("button")) return;
      ev.preventDefault();
      selectAgent(r.dataset.id);
    });
  });
}

function renderToolUsage() {
  const { toolAgg } = computeSummary();
  const toolEntries = Object.entries(toolAgg).sort((a, b) => b[1] - a[1]);
  document.getElementById("dash-tools").innerHTML = toolEntries.length === 0
    ? '<div style="color: var(--muted); font-size: 12px">no tool calls yet</div>'
    : toolEntries.map(([t, c]) => '<span class="chip">' + esc(t) + '<strong>' + c + '</strong></span>').join('');
}

// ─── #42.1 Context pressure strip ──────────────────
// Lists sessions currently over 70% of their model's context window, oldest
// first so you can see who's about to auto-compact. Hidden until at least
// one session crosses the threshold.
function renderContextPressure() {
  const arr = Array.from(agents.values())
    .filter(a => (a.contextUsedPct || 0) >= 0.7)
    .sort((x, y) => (y.contextUsedPct || 0) - (x.contextUsedPct || 0));
  const head = document.getElementById("dash-context-head");
  const container = document.getElementById("dash-context");
  const count = document.getElementById("dash-context-count");
  if (!container || !head) return;
  if (arr.length === 0) {
    head.style.display = "none";
    container.style.display = "none";
    container.innerHTML = "";
    return;
  }
  head.style.display = "";
  container.style.display = "";
  if (count) count.textContent = String(arr.length);
  container.innerHTML = arr.map(a => {
    const pct = Math.round((a.contextUsedPct || 0) * 100);
    const cls = pct >= 90 ? "hot" : "warm";
    return (
      '<div class="ctx-row ' + cls + '" role="button" tabindex="0" aria-label="Select agent ' + esc(agentName(a)) + ' (context ' + pct + '%)" data-id="' + esc(a.id) + '">' +
        '<div class="avatar-mini"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="body">' +
          '<div class="name">' + esc(agentName(a)) + '</div>' +
          '<div class="meta">' + fmtNum(a.lastContextTokens || 0) + ' / ' + fmtNum(a.contextLimit || 0) + ' tokens · ' + esc(a.model || "?") + '</div>' +
        '</div>' +
        '<div class="pct">' + pct + '%</div>' +
      '</div>'
    );
  }).join("");
  container.querySelectorAll(".ctx-row").forEach(el => {
    el.addEventListener("click", () => selectAgent(el.dataset.id));
    el.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      ev.preventDefault();
      selectAgent(el.dataset.id);
    });
  });
}

// ─── #42.7 Health check strip ──────────────────────
// Lists sessions whose health score has dropped below 70. Shows the
// primary reason (loop, failures, stall, context, over-budget) so the
// human can act. Hidden until at least one session is degraded.
function renderHealthCheck() {
  const arr = Array.from(agents.values())
    .filter(a => (a.healthScore == null ? 100 : a.healthScore) < 70)
    .sort((x, y) => (x.healthScore || 0) - (y.healthScore || 0));
  const head = document.getElementById("dash-health-head");
  const container = document.getElementById("dash-health");
  const count = document.getElementById("dash-health-count");
  if (!container || !head) return;
  if (arr.length === 0) {
    head.style.display = "none";
    container.style.display = "none";
    container.innerHTML = "";
    return;
  }
  head.style.display = "";
  container.style.display = "";
  if (count) count.textContent = String(arr.length);
  container.innerHTML = arr.map(a => {
    const score = a.healthScore == null ? 100 : a.healthScore;
    const cls = score < 50 ? "danger" : "warn";
    return (
      '<div class="health-row ' + cls + '" role="button" tabindex="0" aria-label="Select agent ' + esc(agentName(a)) + ' (health ' + score + ')" data-id="' + esc(a.id) + '">' +
        '<div class="avatar-mini"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="body">' +
          '<div class="name">' + esc(agentName(a)) + '</div>' +
          '<div class="meta">' + esc(a.healthReason || "degraded") + '</div>' +
        '</div>' +
        '<div class="score">' + score + '</div>' +
      '</div>'
    );
  }).join("");
  container.querySelectorAll(".health-row").forEach(el => {
    el.addEventListener("click", () => selectAgent(el.dataset.id));
    el.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      ev.preventDefault();
      selectAgent(el.dataset.id);
    });
  });
}

// ─── #37 PR-ready: Review queue ──────────────────
// Sessions in "review_ready" surface here for quick human triage. Two
// buttons per row: Open PR (external link) and Mark reviewed (server
// clears the flag, session moves to "stopped"). Oldest first so the
// human works through them round-robin.
function renderReviewQueue() {
  const arr = Array.from(agents.values()).filter(a => a.status === "review_ready");
  const head = document.getElementById("dash-review-head");
  const container = document.getElementById("dash-review");
  const count = document.getElementById("dash-review-count");
  if (!container || !head) return;
  if (arr.length === 0) {
    head.style.display = "none";
    container.style.display = "none";
    container.innerHTML = "";
    return;
  }
  head.style.display = "";
  container.style.display = "";
  count.textContent = arr.length;
  // Oldest first: sort by updatedAt ascending — that's when the PR
  // flag was flipped on. Falls back to createdAt for stability.
  arr.sort((a, b) => {
    const at = a.updatedAt || a.createdAt || "";
    const bt = b.updatedAt || b.createdAt || "";
    return at.localeCompare(bt);
  });
  container.innerHTML = arr.map(a => {
    const label = a.prTitle || a.prURL || "(PR opened)";
    const age = humanAge(a.updatedAt || a.createdAt);
    return (
      '<div class="review-row" role="button" tabindex="0" aria-label="Select agent ' + esc(agentName(a)) + ' (review ready)" data-id="' + esc(a.id) + '">' +
        '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="rq-body">' +
          '<div class="rq-name">' + esc(agentName(a)) + '<span class="rq-age">· ' + esc(age) + '</span></div>' +
          '<div class="rq-title" title="' + esc(a.prURL || "") + '">' + esc(label) + '</div>' +
        '</div>' +
        '<div class="rq-btns">' +
          '<button class="rq-open" type="button" aria-label="Open pull request in a new tab" data-url="' + esc(a.prURL || "") + '">Open PR</button>' +
          '<button class="rq-done" type="button" aria-label="Mark pull request reviewed" data-id="' + esc(a.id) + '">Mark reviewed</button>' +
        '</div>' +
      '</div>'
    );
  }).join('');
  // Wire click handlers.
  container.querySelectorAll(".review-row").forEach(r => {
    r.onclick = (ev) => {
      if (ev.target.closest("button")) return;
      selectAgent(r.dataset.id);
    };
    r.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      if (ev.target.closest("button")) return;
      ev.preventDefault();
      selectAgent(r.dataset.id);
    });
  });
  container.querySelectorAll(".rq-open").forEach(btn => {
    btn.onclick = (ev) => {
      ev.stopPropagation();
      const url = btn.dataset.url;
      if (url) window.open(url, "_blank");
    };
  });
  container.querySelectorAll(".rq-done").forEach(btn => {
    btn.onclick = async (ev) => {
      ev.stopPropagation();
      const id = btn.dataset.id;
      btn.disabled = true;
      try {
        const res = await fetch("/api/agents/" + id + "/reviewed", { method: "POST" });
        if (!res.ok) toast.error("Mark reviewed failed: " + (await res.text()));
      } catch (err) {
        toast.error("Mark reviewed failed: " + err.message);
      } finally {
        setTimeout(() => { btn.disabled = false; }, 400);
      }
    };
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

// ─── Compact action rows (inline in the feed) ──
// One synthetic row per active interaction. Same 3-column grid as regular
// feed rows so they visually blend. The body holds a compact prompt, an
// optional single-line tool preview, and the buttons.
function actionShellHead(a, kind, klass) {
  return (
    '<div class="action ' + klass + '" role="button" tabindex="0" aria-label="Open ' + esc(agentName(a)) + ' — ' + esc(klass) + '" data-id="' + esc(a.id) + '">' +
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

// #38 Output-tokens tile with a stacked horizontal bar splitting the
// lump-sum output_tokens into thinking / text / tool_use segments,
// plus a legend and an "approximate" tooltip. The big number stays at
// the top of the tile for continuity with the other tiles. If we have
// no char signal yet (no assistant turn observed since the upgrade),
// we render the plain flat number — no misleading zeros in the legend.
const TOK_APPROX_TITLE = "Approximate — pro-rated from the character split of Claude's response blocks. " +
  "Thinking is typically under-counted (dense reasoning packs more tokens per char) " +
  "and tool_use is typically over-counted (JSON keys are token-cheap).";
function tileTokSplit(lab, total, think, text, tool) {
  const sum = think + text + tool;
  if (total <= 0 || sum <= 0) {
    return tileTok(lab, total, "generated tokens");
  }
  const p = (n) => ((n / sum) * 100).toFixed(1) + "%";
  const bar =
    '<div class="tok-split-bar" title="' + esc(TOK_APPROX_TITLE) + '">' +
      '<span class="seg think" style="width:' + p(think) + '" title="Thinking · ' + fmtNum(think) + ' tokens (approx)"></span>' +
      '<span class="seg text"  style="width:' + p(text)  + '" title="Text · '     + fmtNum(text)  + ' tokens (approx)"></span>' +
      '<span class="seg tool"  style="width:' + p(tool)  + '" title="Tool use · ' + fmtNum(tool)  + ' tokens (approx)"></span>' +
    '</div>';
  const legend =
    '<div class="tok-split-legend">' +
      '<span><i class="sw think"></i>' + fmtNum(think) + ' thinking</span>' +
      '<span><i class="sw text"></i>'  + fmtNum(text)  + ' text</span>' +
      '<span><i class="sw tool"></i>'  + fmtNum(tool)  + ' tool</span>' +
      '<span class="approx" title="' + esc(TOK_APPROX_TITLE) + '">approx</span>' +
    '</div>';
  return (
    '<div class="tok split">' +
      '<div class="lab">' + esc(lab) + '</div>' +
      '<div class="val">' + fmtNum(total) + '</div>' +
      bar + legend +
    '</div>'
  );
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
      + (a.pending ? " pending" : "")
      + (warnedAgents.has(a.id) ? " cost-warned" : "")           // #35
      + (a.status === "review_ready" ? " status-review-ready" : ""); // #37
    c.dataset.id = a.id;
    c.draggable = true;
    // #45 F4 — expose the tile itself to assistive tech / clickcast: it's the
    // primary hit-target that opens the terminal. Kept as <div> because
    // <button> would fight drag-and-drop reordering and inherit default UA
    // styling that clashes with .agent-card. role+tabindex+aria-label +
    // keyboard handler below give it the same accessible surface.
    c.setAttribute("role", "button");
    c.setAttribute("tabindex", "0");
    c.setAttribute("aria-label", "Select agent " + agentName(a));
    if (selectedId === a.id) c.setAttribute("aria-current", "true");
    const task = a.currentTask || a.prompt || "";
    const activityText = a.lastActivity || (a.lastTool ? "✓ " + a.lastTool : "");
    const tokTotal = (a.inputTokens || 0) + (a.outputTokens || 0);
    // #36 Per-agent quiet toggle — 🔕 when notifications are enabled, 🔔 when
    // muted. Tooltips explain the action rather than the current state.
    const quiet = window.collectifNotify && window.collectifNotify.isQuiet(a.id);
    const quietIcon  = quiet ? "🔔" : "🔕";
    const quietTitle = quiet ? "Un-mute notifications for this agent" : "Mute notifications for this agent";
    // #42.1 context pressure bar under the token row. Green <60%,
    // amber 60-85%, red >85%. Only render when we have signal.
    //
    // #46 Phase 3: for adapters without StructuredTranscript we have no
    // reliable token accounting, so the pressure bar would be
    // misleadingly zero. Render "—" in that slot instead of a bar.
    const ctxPct = a.contextUsedPct || 0;
    let ctxCls = "ok";
    if (ctxPct >= 0.85) ctxCls = "hot";
    else if (ctxPct >= 0.6) ctxCls = "warm";
    const ctxBar = !adapterSupports(a, "structuredTranscript")
      ? '<div class="ctx-bar dashed" title="Context pressure unavailable — this CLI does not expose a structured transcript.">—</div>'
      : (ctxPct > 0
          ? '<div class="ctx-bar ' + ctxCls + '" title="Context ' + Math.round(ctxPct*100) + '% (' + fmtNum(a.lastContextTokens||0) + ' / ' + fmtNum(a.contextLimit||0) + ' tokens)"><div class="fill" style="width:' + (ctxPct*100).toFixed(1) + '%"></div></div>'
          : "");
    // #42.7 health warning pill — only shown when score drops below 70.
    const health = a.healthScore == null ? 100 : a.healthScore;
    const healthPill = health < 70
      ? '<span class="health-pill' + (health < 50 ? ' danger' : '') + '" title="' + esc(a.healthReason || "degraded") + '">⚠ ' + esc(a.healthReason || "degraded") + '</span>'
      : "";
    // #46 CLI chip: 2-letter form + per-CLI colour class. Comes right
    // after the codename so at-a-glance scanning of the sidebar reveals
    // which sessions belong to which CLI.
    const cliChip = renderCLIChip(a.cli);
    c.innerHTML = (
      '<button class="quiet-btn" type="button" draggable="false" aria-label="' + esc(quietTitle) + '" title="' + esc(quietTitle) + '">' + quietIcon + '</button>' +
      '<button class="kill-btn" type="button" draggable="false" aria-label="Kill agent ' + esc(agentName(a)) + '" title="Kill agent">×</button>' +
      '<div class="card-head">' +
        '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="card-body">' +
          '<div class="card-name"><span class="name">' + esc(agentName(a)) + '</span>' +
          cliChip +
          (a.pending ? '<span class="pending-badge">Action</span>' : '') +
          (a.status === "review_ready" ? '<span class="review-badge">PR</span>' : '') +
          '<span class="age">' + humanAge(a.createdAt) + '</span></div>' +
          '<div class="card-cwd" title="' + esc(a.cwd) + '">' + esc(cwdBase(a.cwd)) + '</div>' +
          '<div class="card-status-row"><span class="status-pill ' + esc(a.status || "idle") + '"><span class="dot"></span>' + esc((a.status || "idle").replace("_", " ")) + '</span>' + healthPill + '</div>' +
          (task ? '<div class="card-activity" title="' + esc(task) + '">▸ ' + esc(task) + '</div>' : '') +
          (activityText ? '<div class="card-activity">' + esc(activityText) + '</div>' : '') +
          (tokTotal > 0 ? '<div class="card-token">' + fmtNum(a.inputTokens || 0) + ' in · ' + fmtNum(a.outputTokens || 0) + ' out</div>' : '') +
          ctxBar +
        '</div>' +
      '</div>'
    );
    if (health < 50) c.classList.add("unhealthy");
    // Card click → select (but only if the click didn't originate on kill
    // or the quiet-toggle).
    c.addEventListener("click", (ev) => {
      if (ev.target.closest(".kill-btn")) return;
      if (ev.target.closest(".quiet-btn")) return;
      selectAgent(a.id);
    });
    // #45 F4 — Enter / Space activate the card the same way as a click,
    // since we exposed it as role="button" above.
    c.addEventListener("keydown", (ev) => {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      if (ev.target.closest(".kill-btn")) return;
      if (ev.target.closest(".quiet-btn")) return;
      ev.preventDefault();
      selectAgent(a.id);
    });
    // #36 Quiet-mode toggle.
    const quietBtn = c.querySelector(".quiet-btn");
    if (quietBtn && window.collectifNotify) {
      quietBtn.addEventListener("mousedown", (ev) => ev.stopPropagation());
      quietBtn.addEventListener("dragstart", (ev) => { ev.preventDefault(); ev.stopPropagation(); });
      quietBtn.addEventListener("click", (ev) => {
        ev.stopPropagation();
        const nowQuiet = !window.collectifNotify.isQuiet(a.id);
        window.collectifNotify.setQuiet(a.id, nowQuiet);
        toast.info(nowQuiet ? "Muted " + agentName(a) : "Unmuted " + agentName(a));
        scheduleRender("sidebar");
      });
    }
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
  // #44 gh-mirror listens for this to refresh Issues/PRs against the newly
  // selected agent's cwd.
  document.dispatchEvent(new CustomEvent("collectif-agent-selected", { detail: { id } }));
}

// ─── #35 Budget strip ──────────────────────────────────────
// The server sends `hourly_cost` events every 30s and a `cost_warning` event
// when a session crosses 80% (or 100%) of its per-session cap. The strip
// shows the rolling-hour total against the hourly cap from /api/config, and
// a small readout of session counts.
let currentHourlyCost = 0;
let currentHourlySessions = 0;
let currentHourlyOverCap = 0;
let currentHourlyCap = 0;          // from /api/config.cost_cap_hour_usd
let warnedAgents = new Set();      // ids that flashed recently

function handleHourlyCost(msg) {
  currentHourlyCost = msg.cost || 0;
  currentHourlySessions = msg.sessions || 0;
  currentHourlyOverCap = msg.overCap || 0;
  scheduleRender("budget");
}

function handleCostWarning(msg) {
  const a = agents.get(msg.id);
  const name = a ? agentName(a) : msg.id;
  const pct = Math.round((msg.pct || 0) * 100);
  toast.error(name + " at " + pct + "% of " + fmtCost(msg.cap || 0) + " cap");
  warnedAgents.add(msg.id);
  scheduleRender("sidebar");
  // Auto-clear the highlight after 30s so it doesn't linger forever.
  setTimeout(() => {
    warnedAgents.delete(msg.id);
    scheduleRender("sidebar");
  }, 30000);
}

async function refreshBudgetConfig() {
  try {
    const res = await fetch("/api/config");
    if (!res.ok) return;
    const c = await res.json();
    currentHourlyCap = c && c.cost_cap_hour_usd ? c.cost_cap_hour_usd : 0;
    scheduleRender("budget");
  } catch (_) { /* silent — best-effort */ }
}

function renderBudgetStrip() {
  const el = document.getElementById("dash-budget");
  if (!el) return;
  const cap = currentHourlyCap;
  const cost = currentHourlyCost;
  const pct = cap > 0 ? Math.min(100, Math.round((cost / cap) * 100)) : 0;
  const barCls = cap > 0
    ? (cost >= cap ? "over" : (cost >= 0.8 * cap ? "warn" : "ok"))
    : "ok";
  const capLabel = cap > 0 ? " / " + fmtCost(cap) : " (no hourly cap set)";
  el.innerHTML = (
    '<div class="budget-head">' +
      '<span class="budget-val">' + esc(fmtCost(cost)) + '</span>' +
      '<span class="budget-cap">' + esc(capLabel) + '</span>' +
    '</div>' +
    (cap > 0
      ? '<div class="budget-bar"><div class="budget-fill ' + barCls + '" style="width:' + pct + '%"></div></div>'
      : '') +
    '<div class="budget-sub">' + currentHourlySessions +
      ' session' + (currentHourlySessions === 1 ? '' : 's') +
      ' this hour · ' + currentHourlyOverCap + ' over cap</div>'
  );
}

// Fetch the hourly cap once at boot so the strip renders promptly. Scripts
// are placed at the end of body, so the DOM is already parsed by the time
// this runs — call directly, defer for a tick so auth.js has published fetch.
// Skip when the auth screen is showing (getElementById would be null anyway).
if (typeof window !== "undefined" && !window.AGENTCTL_NO_TOKEN) {
  setTimeout(() => { try { refreshBudgetConfig(); } catch (_) {} }, 0);
}
