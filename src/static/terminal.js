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
    updateTeamVisibility();
    return;
  }
  empty.style.display = "none";
  head.style.display = "flex";
  body.style.display = "block";

  document.getElementById("d-avatar").src = avatarURL(a.id);
  document.getElementById("d-name").textContent = agentName(a);
  document.getElementById("d-cwd").textContent = a.cwd;
  const status = a.status || "idle";
  document.getElementById("d-status").innerHTML = '<span class="status-pill ' + esc(status) + '"><span class="dot"></span>' + esc(status.replace(/_/g, " ")) + '</span>';
  const task = a.currentTask || a.prompt || "";
  document.getElementById("d-task").textContent = task ? "▸ " + task : "";

  // #35 Resume-anyway button: shown next to Kill when paused_over_budget.
  const resumeBtn = document.getElementById("d-resume");
  if (resumeBtn) {
    resumeBtn.style.display = status === "paused_over_budget" ? "" : "none";
  }

  if (mountTerminal) mountTerminalFor(a.id);
  requestAnimationFrame(() => { fitTerminalNow(); });
  updateTeamVisibility();
}

// Cross-context clipboard helpers. Prefer navigator.clipboard when we're
// in a secure context (HTTPS / localhost); fall back to the legacy
// document.execCommand("copy") which works on plain HTTP but requires a
// short-lived textarea to hold the payload.
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch (_) { /* fall through to execCommand */ }
  const ta = document.createElement("textarea");
  ta.value = text;
  ta.style.position = "fixed";
  ta.style.top = "0";
  ta.style.left = "-9999px";
  ta.setAttribute("readonly", "");
  document.body.appendChild(ta);
  ta.focus();
  ta.select();
  let ok = false;
  try { ok = document.execCommand("copy"); } catch (_) { ok = false; }
  document.body.removeChild(ta);
  return ok;
}
async function pasteFromClipboard() {
  try {
    if (navigator.clipboard && window.isSecureContext && navigator.clipboard.readText) {
      return await navigator.clipboard.readText();
    }
  } catch (_) { /* falls through */ }
  return "";
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
    if (typeof ev.data === "string") { term.write(ev.data); appendToOutputBuffer(ev.data); }
    else {
      const bytes = new Uint8Array(ev.data);
      term.write(bytes);
      try { appendToOutputBuffer(new TextDecoder("utf-8", { fatal: false }).decode(bytes)); } catch (_) {}
    }
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
  term = new Terminal({
    cursorBlink: true,
    fontFamily: "ui-monospace, Menlo, monospace",
    fontSize: 13,
    theme: { background: "#000000" },
    rightClickSelectsWord: true,
  });
  // Selection → clipboard behavior mirrors what Alacritty / iTerm /
  // gnome-terminal do:
  //  1. Finish a drag-select → auto-copy to clipboard immediately.
  //  2. Ctrl+C (or Cmd+C on Mac) with a selection → also copy AND swallow
  //     the key so it doesn't SIGINT. No selection → key passes through
  //     to the PTY as normal (SIGINT for a running process).
  //  3. Ctrl+Shift+C also copies (traditional terminal convention).
  //  4. Ctrl+Shift+V / Cmd+V pastes clipboard into the PTY.
  let lastSelection = "";
  if (term.onSelectionChange) {
    term.onSelectionChange(() => {
      const sel = term.getSelection ? term.getSelection() : "";
      if (sel && sel !== lastSelection) {
        lastSelection = sel;
        copyToClipboard(sel).then(ok => {
          if (ok) toast.success("Copied " + sel.length + " chars");
        });
      } else if (!sel) {
        lastSelection = "";
      }
    });
  }
  term.attachCustomKeyEventHandler((ev) => {
    if (ev.type !== "keydown") return true;
    const isMac = navigator.platform.toUpperCase().indexOf("MAC") >= 0;
    const isC = ev.key === "c" || ev.key === "C";
    const isV = ev.key === "v" || ev.key === "V";
    const shift = ev.shiftKey;
    const ctrlOrCmd = ev.ctrlKey || (isMac && ev.metaKey);
    // Copy: try Ctrl+C / Cmd+C with a selection first; falls through to
    // SIGINT if no selection is detected. Ctrl+Shift+C ALWAYS opens the
    // output modal — the reliable fallback for browsers where xterm's
    // selection doesn't populate (see issue #18).
    if (ctrlOrCmd && isC && shift && !ev.altKey) {
      openOutputModal();
      return false;
    }
    if (ctrlOrCmd && isC && !ev.altKey) {
      const sel = (term.getSelection && term.getSelection()) || lastSelection ||
                  (window.getSelection && String(window.getSelection())) || "";
      if (sel) {
        copyToClipboard(sel).then(ok => {
          if (ok) toast.success("Copied " + sel.length + " chars");
          else    toast.error("Clipboard write blocked by browser");
        });
        return false; // swallow the key so plain Ctrl+C doesn't SIGINT
      }
      // No selection → let it through as SIGINT (default xterm behavior).
      return true;
    }
    // Paste: Ctrl+Shift+V / Cmd+V.
    if (ctrlOrCmd && isV && (shift || isMac) && !ev.altKey) {
      pasteFromClipboard().then(text => {
        if (text && termWS && termWS.readyState === 1) termWS.send(text);
        else if (!text) toast.info("Clipboard empty or access denied");
      });
      return false;
    }
    return true;
  });
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

// Copy button — opens a modal with the recent terminal buffer as plain
// HTML text. Native browser selection + Ctrl+C work there, and a
// "Copy all" button uses the same execCommand fallback for the whole
// buffer. This bypasses xterm's finicky selection layer entirely.
const OUTPUT_BUFFER_MAX = 65536;
let termOutputBuffer = "";
// Reliable copy path shared by the "Copy" button and the Ctrl+Shift+C
// key binding — see issue #18. Some browsers don't populate xterm's
// selection reliably; this modal always works.
function openOutputModal() {
  const clean = stripAnsiClient(termOutputBuffer).replace(/^\s+/, "");
  const body = document.getElementById("output-body");
  const modal = document.getElementById("output-modal");
  if (!body || !modal) return;
  body.textContent = clean || "(no output yet)";
  modal.classList.add("show");
  // Pre-select everything so Ctrl+C copies without an extra drag.
  const range = document.createRange();
  range.selectNodeContents(body);
  const winSel = window.getSelection && window.getSelection();
  if (winSel) { winSel.removeAllRanges(); winSel.addRange(range); }
}
function appendToOutputBuffer(chunk) {
  termOutputBuffer += chunk;
  if (termOutputBuffer.length > OUTPUT_BUFFER_MAX * 2) {
    termOutputBuffer = termOutputBuffer.slice(-OUTPUT_BUFFER_MAX);
  }
}
function stripAnsiClient(s) {
  return String(s)
    .replace(/\x1b\][^\x07\x1b]*(\x07|\x1b\\)/g, "")   // OSC
    .replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, "")            // CSI
    .replace(/\x1b[\(\)][A-Za-z0-9]/g, "")             // charset selects
    .replace(/\x1b[=>NnPMHDEc78]/g, "")                // misc
    .replace(/\r(?!\n)/g, "\n");                       // lone CR → newline
}

// Wire up the terminal-panel buttons and modal at boot time. Kept in a
// function so the auth screen (where the DOM nodes don't exist) can skip
// this wiring by simply not calling bootTerminal().
function bootTerminal() {
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

  document.getElementById("d-copy").onclick = () => openOutputModal();

  // #35 Resume-anyway: POST /api/agents/{id}/resume; SIGCONTs the process
  // and clears the per-session cap so it can continue.
  const resumeBtn = document.getElementById("d-resume");
  if (resumeBtn) {
    resumeBtn.onclick = async () => {
      if (!selectedId) return;
      resumeBtn.disabled = true;
      try {
        const res = await fetch("/api/agents/" + selectedId + "/resume", { method: "POST" });
        if (!res.ok) toast.error("Resume failed: " + res.status + " " + (await res.text()));
        else toast.success("Resumed");
      } catch (err) {
        toast.error("Resume failed: " + err.message);
      } finally {
        setTimeout(() => { resumeBtn.disabled = false; }, 400);
      }
    };
  }
  document.getElementById("output-close").onclick = () => document.getElementById("output-modal").classList.remove("show");
  // Esc closes the output modal from anywhere (matches the sa-modal UX).
  document.addEventListener("keydown", (ev) => {
    if (ev.key !== "Escape") return;
    const m = document.getElementById("output-modal");
    if (m && m.classList.contains("show")) m.classList.remove("show");
  });
  document.getElementById("output-copy-all").onclick = async () => {
    const text = document.getElementById("output-body").textContent;
    if (!text) { toast.error("Nothing to copy"); return; }
    const ok = await copyToClipboard(text);
    if (ok) toast.success("Copied " + text.length + " chars");
    else    toast.error("Clipboard write failed — select text in the box and Ctrl+C manually");
  };
}
