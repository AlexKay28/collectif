// nb_cells.js — cell rendering, editing, and the keyboard model.
// #49 (M1 slice 3).
//
// The keyboard model is Jupyter's, adopted verbatim rather than invented:
// Esc/Enter switch between command and edit mode, a/b insert above/below,
// dd deletes, Shift+Enter runs and advances, Ctrl+Enter runs in place. It
// is muscle memory for the people this is for, and an original scheme would
// be a worse version of something they already know.

import { state, nbAPI, onChange } from "./nb.js";
import { renderMarkdown, renderOutputs, escapeHTML } from "./nb_render.js";

const CELL_TYPES = ["markdown", "shell", "prompt", "file"];
export { CELL_TYPES };

let root;

// hasFocus decides whether an unmodified keypress is a notebook command.
// On /notebook.html the notebook is the page and the answer is always yes.
// Embedded in the dashboard it is one panel among several, and Jupyter's
// command mode binds bare letters: without this, `dd` while reading the
// activity feed would delete a cell in a document you were not looking at.
let hasFocus = () => true;

export function mountCells(el, opts = {}) {
  root = el;
  if (opts.hasFocus) hasFocus = opts.hasFocus;
  onChange(render);
  document.addEventListener("keydown", onKeyDown);

  const help = document.getElementById("nb-help");
  // Click the backdrop to dismiss, but not the sheet itself — a stray
  // click while reading the map should not close it.
  help?.addEventListener("click", (e) => {
    if (e.target === help) toggleHelp(false);
  });
  document.getElementById("nb-keys")?.addEventListener("click", () => toggleHelp(true));

  render();
}

// ─── Rendering ────────────────────────────────────────────────────────

function render() {
  if (!root) return;
  const nb = state.notebook;
  if (!nb) {
    root.innerHTML = `<div class="nb-empty">Select or create a notebook to begin.</div>`;
    return;
  }
  // Preserve focus and caret across a re-render: output arrives while you
  // are typing, and losing the caret mid-word makes the editor unusable.
  const active = document.activeElement;
  const focusedCell = active?.dataset?.cellId;
  const selStart = active?.selectionStart;
  const selEnd = active?.selectionEnd;

  // In a mirrored notebook the prompt is the point — it is how you talk to
  // the running agent — so it leads and says where it goes. In a detached
  // one it is one of three equals.
  const live = !!(nb.meta && nb.meta.sessionId);
  root.innerHTML =
    (nb.cells || []).map((c, i) => renderCell(c, i + 1)).join("") +
    (live
      ? // ADR 0002 §7 Q2: a note written here cannot reach the agent's
        // context — the CLI owns that, and there is no wire to widen it.
        // The two buttons therefore say where their text goes, because a
        // "+ Note" beside a "+ Ask" reads as a quieter way to say the same
        // thing, and a margin note the agent never saw is a bad surprise.
        `<div class="nb-add-row live">
           <button data-add="prompt" class="primary" title="Send this to the agent (⇧Enter)">+ Ask the agent</button>
           <button data-add="markdown" title="A note in the margin. The CLI owns its own context — nothing you write here reaches it.">+ Note</button>
           <button data-add="shell">+ Shell</button>
           <span class="nb-add-note">notes stay here; only prompts reach the agent</span>
         </div>`
      : `<div class="nb-add-row">
           <button data-add="markdown">+ Markdown</button>
           <button data-add="shell">+ Shell</button>
           <button data-add="prompt">+ Prompt</button>
         </div>`);

  if (focusedCell) {
    const ta = root.querySelector(`textarea[data-cell-id="${cssEscape(focusedCell)}"]`);
    if (ta) {
      ta.focus();
      if (selStart != null) ta.setSelectionRange(selStart, selEnd);
    }
  }
  wire();
}

function cssEscape(s) {
  return String(s).replace(/["\\]/g, "\\$&");
}

// revealCell selects a cell and brings it into view. A search result that
// opens the right notebook and leaves you at the top of a three-hundred-cell
// document has not answered the question — the point of the result is the
// turn, not the file (#58).
//
// The brief mark afterwards is neutral rather than coloured: in this
// notebook colour means state, and a cell you were sent to is not a state.
export function revealCell(id) {
  state.mode = "command";
  state.selected = id;
  render();
  const el = root?.querySelector(`[data-cell="${cssEscape(id)}"]`);
  if (!el) return;
  const still = window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
  el.scrollIntoView({ block: "center", behavior: still ? "auto" : "smooth" });
  el.classList.add("found");
  setTimeout(() => el.classList.remove("found"), 1600);
}

// A cell is a margin note plus a body. Which typeface the source gets is
// not decoration: markdown and prompt sources are language and set in a
// serif, shell sources and every output are code and set in mono. The page
// then reads as a document with a terminal in it.
function renderCell(cell, index) {
  const selected = cell.id === state.selected;
  // ADR 0002 D9. A mirrored cell was produced by a CLI whose context lives
  // inside a process we do not own, so it cannot be edited and re-run —
  // only re-asked. Showing an Edit button on one would be an interactive
  // lie, which is the exact failure mode the ADR set out to stop.
  const mirrored = cell.meta && cell.meta.provenance === "mirrored";
  const compact = cell.meta && cell.meta.provenance === "compact";
  const editing = selected && state.mode === "edit" && !mirrored && !compact;
  const prose = cell.type === "markdown" || cell.type === "prompt";
  const live = state.live.get(cell.id);
  const hidden = state.hiddenOutputs.has(cell.id);
  const tall = state.tallOutputs.has(cell.id);
  const outputs = hidden
    ? (cell.outputs || []).length || live
      ? `<div class="out collapsed" data-show="${escapeHTML(cell.id)}">output hidden — press o to show</div>`
      : ""
    : renderOutputs(cell, live);
  const failed = state.cellErrors.get(cell.id);

  const rows = Math.max(2, String(cell.source || "").split("\n").length);
  const body = editing
    ? `<textarea class="nb-src ${prose ? "prose" : ""}" data-cell-id="${escapeHTML(cell.id)}"
         spellcheck="false" rows="${rows}">${escapeHTML(cell.source || "")}</textarea>`
    : cell.type === "markdown"
      ? `<div class="nb-md">${renderMarkdown(cell.source) || '<p class="nb-hint">Empty — press Enter to edit.</p>'}</div>`
      : `<pre class="nb-src-view ${prose ? "prose" : ""}">${escapeHTML(cell.source || "") ||
            (mirrored
              ? '<span class="nb-hint">This turn was already under way when collectif attached.</span>'
              : '<span class="nb-hint">Empty — press Enter to edit.</span>')}</pre>`;

  const runnable = !mirrored && !compact && (cell.type === "shell" || cell.type === "prompt");
  const running = cell.state === "running";

  return `
  <div class="nb-cell ${selected ? "sel" : ""} state-${escapeHTML(cell.state || "idle")}${mirrored ? " mirrored" : ""}${compact ? " compact" : ""}"
       data-cell="${escapeHTML(cell.id)}">
    <div class="nb-gutter">
      <span class="nb-idx">${index}</span>
      <span class="nb-type">${escapeHTML(compact ? "compacted" : cell.type)}</span>
      ${stateChip(cell)}
      ${modelChip(cell)}
      ${cacheChip(cell)}
    </div>
    <div class="nb-body ${tall ? "tall" : ""}">
      ${body}
      ${outputs}
      ${failed ? `<div class="cell-error"><span>${escapeHTML(failed)}</span><button class="x" data-dismiss="${escapeHTML(cell.id)}" title="Dismiss">✕</button></div>` : ""}
    </div>
    <div class="nb-actions">
      ${running && !mirrored
        ? `<button data-interrupt="${escapeHTML(cell.id)}" title="Stop (i)">■</button>`
        : runnable
          ? `<button data-run="${escapeHTML(cell.id)}" title="Run (⇧Enter)">▶</button>`
          : ``}
      ${mirrored && cell.type === "prompt" && cell.source
        ? `<button data-reask="${escapeHTML(cell.id)}" title="Ask this again (the agent's context cannot be rewound)">↻</button>`
        : ``}
      ${mirrored || compact ? `` : `<button data-del="${escapeHTML(cell.id)}" title="Delete (dd)">✕</button>`}
    </div>
  </div>`;
}

function stateChip(cell) {
  const s = cell.state || "idle";
  if (s === "idle") return "";
  const label = {
    running: "running",
    ok: okLabel(cell),
    idle: "",
    error: "error",
    interrupted: "interrupted",
    stale: "stale",
    queued: "queued",
  }[s] || s;
  return `<span class="nb-chip chip-${escapeHTML(s)}">${escapeHTML(label)}</span>`;
}

// okLabel shows what the run cost as well as how long it took. Tokens come
// from the provider's own report (#50), not from a scraped transcript.
function okLabel(cell) {
  const parts = [];
  if (cell.duration) parts.push(fmtDuration(cell.duration));
  const u = cell.usage || {};
  // input_tokens is only the *uncached remainder*, so the prompt is the sum
  // of all three. Reading input_tokens alone makes a working cache look
  // like a shrinking prompt.
  const inTok = (u.inputTokens || 0) + (u.cacheReadTokens || 0) + (u.cacheCreationTokens || 0);
  if (inTok || u.outputTokens) parts.push(`${fmtTokens(inTok)}→${fmtTokens(u.outputTokens || 0)}`);
  return parts.length ? parts.join(" · ") : "ok";
}

// modelChip shows a cell's own model when it has one (#53). Without it a
// per-cell override is invisible: two cells side by side, one answered by
// a frontier model and one by whatever is running on this laptop, and
// nothing in the document saying which was which.
function modelChip(cell) {
  const m = (cell.meta || {}).model;
  if (!m) return "";
  const effort = (cell.meta || {}).effort;
  const label = effort ? `${m} · ${effort}` : m;
  return `<span class="nb-chip chip-model" title="${escapeHTML(
    "This cell overrides the notebook's model, and runs on whichever transport serves it.",
  )}">${escapeHTML(label)}</span>`;
}

// cacheChip is the number M2.5 exists to produce (#51). It is rendered
// separately, and rendered as a warning at zero, because a re-run showing
// no cache reads is the canary for a projection bug — not a pricing
// curiosity to notice later in a bill.
function cacheChip(cell) {
  const u = cell.usage || {};
  const total = (u.inputTokens || 0) + (u.cacheReadTokens || 0) + (u.cacheCreationTokens || 0);
  if (!total) return "";
  const read = u.cacheReadTokens || 0;
  // #53. On a transport with no cached-token counter — Ollama, llama.cpp,
  // vLLM — that warning would fire on every cell forever, reporting a miss
  // that never happened and sending the reader after a bug that is not
  // there. So the absence is stated instead.
  //
  // cacheMode is derived per cell on the server, because a cell can name
  // its own model and so its own transport. It arrives with the fold, so a
  // cell inserted over the websocket since then falls back to the
  // notebook's transport — right for every cell that has not overridden
  // its model, and re-reading the fold settles the rest.
  const mode = cell.cacheMode || state.notebook?.provider?.capabilities?.cache;
  if (!read && mode === "none") {
    return `<span class="nb-chip chip-cache-na" title="${escapeHTML(
      "This model's endpoint does not report cache use, so there is no figure to show — not a cache miss.",
    )}">cache n/a</span>`;
  }
  const pct = Math.round((read / total) * 100);
  const cold = pct === 0;
  const title = cold
    ? "Nothing was served from cache. Expected on a first run; on a re-run of the same cell it means the prefix is not matching."
    : `${pct}% of the prompt was served from cache`;
  return `<span class="nb-chip chip-cache${cold ? " chip-cache-cold" : ""}" title="${escapeHTML(title)}">${pct}% cached</span>`;
}

function fmtTokens(n) {
  if (n < 1000) return String(n);
  if (n < 1e6) return `${(n / 1000).toFixed(n < 10000 ? 1 : 0)}k`;
  return `${(n / 1e6).toFixed(1)}M`;
}

// Go marshals time.Duration as an integer count of nanoseconds.
function fmtDuration(ns) {
  const ms = ns / 1e6;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m${Math.round((ms % 60000) / 1000)}s`;
}

// ─── Wiring ───────────────────────────────────────────────────────────

function wire() {
  root.querySelectorAll("[data-cell]").forEach((el) => {
    el.addEventListener("mousedown", () => {
      // Re-rendering replaces innerHTML, which destroys the textarea the
      // browser is about to place the caret in — so clicking inside the
      // cell you are editing would reset the caret and break selection.
      if (el.dataset.cell === state.selected) return;
      state.selected = el.dataset.cell;
      render();
    });
  });
  root.querySelectorAll("[data-run]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      runCell(b.dataset.run);
    }),
  );
  root.querySelectorAll("[data-interrupt]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      nbAPI.interruptCell(state.id, b.dataset.interrupt).catch(showError);
    }),
  );
  root.querySelectorAll("[data-reask]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      reask(b.dataset.reask);
    }),
  );
  root.querySelectorAll("[data-del]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      deleteCell(b.dataset.del);
    }),
  );
  root.querySelectorAll("[data-show]").forEach((el) =>
    el.addEventListener("click", () => {
      state.hiddenOutputs.delete(el.dataset.show);
      render();
    }),
  );
  root.querySelectorAll("[data-answer]").forEach((b) =>
    b.addEventListener("click", async (e) => {
      e.stopPropagation();
      const cell = b.closest("[data-cell]");
      b.disabled = true;
      try {
        await nbAPI.answerApproval(state.id, cell.dataset.cell, b.dataset.answer, b.dataset.approvalId);
      } catch (err) {
        b.disabled = false;
        // The agent may have timed out, or been answered from the
        // terminal. Either way the reason belongs on the cell, not in a
        // toast that scrolls away above a long document.
        state.cellErrors.set(cell.dataset.cell, String(err && err.message ? err.message : err));
        render();
      }
    }),
  );
  root.querySelectorAll("[data-dismiss]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      state.cellErrors.delete(b.dataset.dismiss);
      render();
    }),
  );
  root.querySelectorAll("[data-add]").forEach((b) =>
    b.addEventListener("click", () => insertCell(b.dataset.add, {})),
  );

  const ta = root.querySelector("textarea.nb-src");
  if (ta) {
    ta.addEventListener("blur", () => commit(ta));
    ta.addEventListener("keydown", (e) => e.stopPropagation()); // typing is not a shortcut
    ta.addEventListener("keydown", onEditorKeyDown);
  }
}

async function commit(ta) {
  const id = ta.dataset.cellId;
  const cell = findCell(id);
  if (!cell || cell.source === ta.value) return;
  cell.source = ta.value; // optimistic: the event will confirm it
  try {
    await nbAPI.editCell(state.id, id, { source: ta.value });
  } catch (err) {
    showError(err);
  }
}

function findCell(id) {
  return (state.notebook?.cells || []).find((c) => c.id === id);
}

function selectedIndex() {
  return (state.notebook?.cells || []).findIndex((c) => c.id === state.selected);
}

// pos is {beforeCellId} or {afterCellId}. "Above" cannot be expressed with
// afterCellId alone — empty means append, so inserting above the first cell
// used to drop the new cell at the bottom of the notebook.
async function insertCell(type, pos = {}) {
  try {
    const res = await nbAPI.addCell(state.id, {
      type,
      source: "",
      afterCellId: pos.afterCellId || "",
      beforeCellId: pos.beforeCellId || "",
    });
    state.selected = res.cellId;
    state.mode = "edit";
  } catch (err) {
    showError(err);
  }
}

async function deleteCell(id) {
  const cells = state.notebook?.cells || [];
  const i = cells.findIndex((c) => c.id === id);
  // Remember it for z. Position is recorded as "before the cell that
  // followed", which survives other edits better than an index.
  if (i >= 0) {
    state.trash.push({ cell: { ...cells[i] }, before: cells[i + 1]?.id || "" });
    if (state.trash.length > 20) state.trash.shift();
  }
  // Resolve the neighbour *before* awaiting. The server broadcasts inside
  // Append, before it writes the HTTP response, so the cell_deleted event
  // usually lands first and splices this very array — reading it afterwards
  // would skip a cell, nondeterministically.
  const nextID = (cells[i + 1] || cells[i - 1] || {}).id || null;
  try {
    await nbAPI.deleteCell(state.id, id);
    state.selected = nextID;
  } catch (err) {
    showError(err);
  }
}

// Re-run degrades to re-ask on a mirrored cell (ADR 0002 D9). There is no
// wire on which to tell a running CLI "forget turn 3, here is a different
// turn 3", so the honest verb is to put the same words at the bottom as a
// new prompt — which is what you would do by hand anyway.
async function reask(id) {
  const cell = findCell(id);
  if (!cell) return;
  try {
    const res = await nbAPI.addCell(state.id, { type: "prompt", source: cell.source });
    state.selected = res.cellId;
    state.mode = "edit";
    render();
  } catch (err) {
    showError(err);
  }
}

async function runCell(id) {
  const cell = findCell(id);
  if (!cell) return;
  state.cellErrors.delete(id);
  try {
    await nbAPI.runCell(state.id, id);
  } catch (err) {
    // Show it on the cell, not as a toast at the top of the page. The
    // request is rejected before any event is written, so without this
    // the cell does not change at all and pressing run looks like a
    // no-op — which is exactly how it read the first time someone tried
    // a prompt cell with no provider configured.
    state.cellErrors.set(id, String(err && err.message ? err.message : err));
    render();
  }
}

// selectNext advances to the cell below, appending one at the end — this
// is what makes ⇧↩ walk a notebook and leave you somewhere to type.
function selectNext() {
  const cells = state.notebook?.cells || [];
  const i = selectedIndex();
  if (i >= 0 && i < cells.length - 1) {
    state.selected = cells[i + 1].id;
    return;
  }
  insertCell("shell", {});
}

// Markdown has nothing to run — in Jupyter, "running" it is rendering it,
// which here happens the moment you leave the editor. Calling run on one
// would be a 400 the user did not ask for.
function runIfRunnable(id) {
  const cell = findCell(id);
  if (!cell) return;
  if (cell.type === "shell" || cell.type === "prompt") runCell(id);
}

async function setType(id, type) {
  const cell = findCell(id);
  if (!cell || cell.type === type) return;
  cell.type = type; // optimistic; the event confirms it
  render();
  try {
    await nbAPI.setCellType(state.id, id, type);
  } catch (err) {
    showError(err);
  }
}

// ─── Keyboard ─────────────────────────────────────────────────────────
//
// Jupyter's shortcut set, taken from the documented tables rather than
// from memory, and mapped onto this notebook's cell types. The mapping
// rules:
//
//   y → shell    Jupyter's "code" cell; ours is the one that executes.
//   m → markdown unchanged.
//   p → prompt   Jupyter has no equivalent, and its `r` (raw) has no
//                meaning here, so a new letter rather than a stolen one.
//   f → file     same reasoning; `f` is find/replace in Jupyter, which we
//                do not have yet — noted as a conflict to revisit.
//
// Deliberately absent: 1-6 (headings — markdown already does that),
// l (line numbers — no editor gutter), 0,0 (restart kernel — no kernel),
// and everything that acts on *multiple* selected cells (⇧j/⇧k/⌘a).
// Multi-select is a real gap rather than an oversight: half of Jupyter's
// command mode is plural, and faking it single-cell would be worse than
// leaving it out.

// Chords like `d d` and `i i` need a pending first key that expires, or a
// `d` typed a minute ago would arm a delete.
let pendingKey = null;
let pendingAt = 0;
const CHORD_MS = 1200;

function chord(key) {
  const now = Date.now();
  if (pendingKey === key && now - pendingAt < CHORD_MS) {
    pendingKey = null;
    return true;
  }
  pendingKey = key;
  pendingAt = now;
  return false;
}

function onEditorKeyDown(e) {
  const ta = e.currentTarget;
  const id = ta.dataset.cellId;

  // ⌃m and Esc both leave for command mode.
  if (e.key === "Escape" || (e.key === "m" && e.ctrlKey)) {
    e.preventDefault();
    commit(ta).then(() => { state.mode = "command"; ta.blur(); render(); });
    return;
  }
  if (e.key === "Enter" && (e.shiftKey || e.ctrlKey || e.metaKey || e.altKey)) {
    e.preventDefault();
    const alt = e.altKey, shift = e.shiftKey;
    commit(ta).then(() => {
      state.mode = "command";
      runIfRunnable(id);
      if (alt) insertCell("shell", { afterCellId: id });
      else if (shift) selectNext();
      render();
    });
    return;
  }
  // ⌃⇧- splits the cell at the cursor.
  if (e.key === "_" && e.ctrlKey && e.shiftKey) {
    e.preventDefault();
    splitCell(ta);
    return;
  }
  // Tab indents rather than moving focus out of the editor, and ⌘]/⌘[
  // do the same explicitly.
  if (e.key === "Tab" && !e.ctrlKey && !e.metaKey) {
    e.preventDefault();
    indentSelection(ta, e.shiftKey ? -1 : 1);
    return;
  }
  if ((e.metaKey || e.ctrlKey) && (e.key === "]" || e.key === "[")) {
    e.preventDefault();
    indentSelection(ta, e.key === "]" ? 1 : -1);
    return;
  }
  if ((e.metaKey || e.ctrlKey) && e.key === "s") {
    e.preventDefault();
    announceSaved();
  }
}

function indentSelection(ta, dir) {
  const { selectionStart: a, selectionEnd: b, value } = ta;
  const startLine = value.lastIndexOf("\n", a - 1) + 1;
  const block = value.slice(startLine, b);
  const next = dir > 0
    ? block.replace(/^/gm, "  ")
    : block.replace(/^ {1,2}/gm, "");
  ta.value = value.slice(0, startLine) + next + value.slice(b);
  ta.setSelectionRange(startLine, startLine + next.length);
}

async function splitCell(ta) {
  const id = ta.dataset.cellId;
  const cell = findCell(id);
  if (!cell) return;
  const at = ta.selectionStart;
  const head = ta.value.slice(0, at);
  const tail = ta.value.slice(at);
  try {
    await nbAPI.editCell(state.id, id, { source: head });
    const res = await nbAPI.addCell(state.id, { type: cell.type, source: tail, afterCellId: id });
    state.selected = res.cellId;
    state.mode = "edit";
  } catch (err) { showError(err); }
}

function onKeyDown(e) {
  if (!state.notebook) return;
  if (!hasFocus()) return;

  // The help overlay closes on Esc or q, matching Jupyter's pager.
  if (helpOpen()) {
    if (e.key === "Escape" || e.key === "q") { e.preventDefault(); toggleHelp(false); }
    return;
  }
  if (state.mode === "edit") return;
  const tag = document.activeElement?.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;

  // Jupyter binds the unmodified keys in command mode. Without this,
  // ⌘/Ctrl+A selects a cell instead of selecting all, and ⌘K/⌘J are
  // swallowed as navigation.
  const plain = !e.altKey && !e.metaKey && !e.ctrlKey;
  const cells = state.notebook.cells || [];
  const i = selectedIndex();
  const id = state.selected;
  const done = () => { e.preventDefault(); render(); };

  // Run verbs carry modifiers, so they are handled before the plain-key
  // guard rejects them.
  if (e.key === "Enter") {
    if (e.shiftKey) { if (id) runIfRunnable(id); selectNext(); return done(); }
    if (e.ctrlKey || e.metaKey) { if (id) runIfRunnable(id); return done(); }
    if (e.altKey) { if (id) runIfRunnable(id); insertCell("shell", { afterCellId: id }); return done(); }
    if (id) { state.mode = "edit"; return done(); }
    return;
  }
  if ((e.metaKey || e.ctrlKey) && e.key === "s") { e.preventDefault(); announceSaved(); return; }
  if (!plain) return;

  if (e.key !== "d" && e.key !== "i") pendingKey = null;

  // Mirrored cells are read-only. Refusing here, with a reason, beats
  // letting the key fire and surfacing a server error for something the
  // UI already knew was impossible.
  const cur = id ? findCell(id) : null;
  const readOnly = cur && cur.meta &&
    (cur.meta.provenance === "mirrored" || cur.meta.provenance === "compact");
  if (readOnly && "dxmypfM".includes(e.key)) {
    e.preventDefault();
    showNote("This cell is a record of what the agent did — press ↻ to ask it again.");
    return;
  }

  switch (e.key) {
    // ── selection ──
    case "j": case "ArrowDown":
      if (i >= 0 && i < cells.length - 1) state.selected = cells[i + 1].id;
      return done();
    case "k": case "ArrowUp":
      if (i > 0) state.selected = cells[i - 1].id;
      return done();

    // ── structure ──
    case "a": insertCell("shell", id ? { beforeCellId: id } : {}); return done();
    case "b": insertCell("shell", id ? { afterCellId: id } : {}); return done();
    case "d": if (chord("d") && id) deleteCell(id); return done();
    case "z": undoDelete(); return done();
    case "x": if (id) { copyCell(id); deleteCell(id); } return done();
    case "c": if (id) copyCell(id); return done();
    case "v": pasteCell(e.shiftKey ? "before" : "after"); return done();
    case "M": if (id) mergeBelow(id); return done(); // shift+m

    // ── cell type ──
    case "m": if (id) setType(id, "markdown"); return done();
    case "y": if (id) setType(id, "shell"); return done();
    case "p": if (id) setType(id, "prompt"); return done();
    case "f": if (id) setType(id, "file"); return done();

    // ── output ──
    case "o": if (id) { toggleSet(state.hiddenOutputs, id); } return done();
    case "O": if (id) { toggleSet(state.tallOutputs, id); } return done(); // shift+o

    // ── kernel-ish ──
    case "i":
      if (chord("i") && id) nbAPI.interruptCell(state.id, id).catch(() => {});
      return done();

    // ── help ──
    case "h": toggleHelp(true); return done();
    case "s": announceSaved(); return done();
    default:
      return;
  }
}

function toggleSet(set, id) { set.has(id) ? set.delete(id) : set.add(id); }

function copyCell(id) {
  const c = findCell(id);
  if (c) state.clipboard = { type: c.type, source: c.source };
}

async function pasteCell(where) {
  if (!state.clipboard) return;
  const pos = state.selected
    ? (where === "before" ? { beforeCellId: state.selected } : { afterCellId: state.selected })
    : {};
  try {
    const res = await nbAPI.addCell(state.id, {
      type: state.clipboard.type, source: state.clipboard.source, ...pos,
    });
    state.selected = res.cellId;
  } catch (err) { showError(err); }
}

// z restores the most recently deleted cell. The document is an
// append-only log, so this is a re-insert rather than a rewind — the
// deletion stays in the history, which is the honest thing for a log.
async function undoDelete() {
  const last = state.trash.pop();
  if (!last) return;
  try {
    const res = await nbAPI.addCell(state.id, {
      type: last.cell.type, source: last.cell.source,
      ...(last.before ? { beforeCellId: last.before } : {}),
    });
    state.selected = res.cellId;
  } catch (err) { showError(err); }
}

async function mergeBelow(id) {
  const cells = state.notebook.cells || [];
  const i = cells.findIndex((c) => c.id === id);
  if (i < 0 || i === cells.length - 1) return;
  const below = cells[i + 1];
  try {
    await nbAPI.editCell(state.id, id, {
      source: `${cells[i].source}\n${below.source}`,
    });
    await nbAPI.deleteCell(state.id, below.id);
    state.selected = id;
  } catch (err) { showError(err); }
}

function announceSaved() {
  // Every change is already a line in the log — there is nothing to save
  // and nothing to lose. Saying so is better than binding the key to
  // nothing and letting it feel broken.
  showNote("Saved continuously — every edit is already in the notebook's log.");
}

function helpOpen() {
  return document.getElementById("nb-help")?.classList.contains("open");
}

export function toggleHelp(open) {
  const el = document.getElementById("nb-help");
  if (el) el.classList.toggle("open", open);
}

// The bar's resting state is `display: none` in the stylesheet, so showing
// it means setting a value — clearing the inline style just falls back to
// the rule that hides it, which is why nothing this file reported had ever
// actually appeared on screen.
export function showNote(text) {
  const bar = document.getElementById("nb-error");
  if (!bar) return;
  bar.textContent = text;
  bar.classList.add("note");
  bar.style.display = "block";
  clearTimeout(showError._t);
  showError._t = setTimeout(() => { bar.style.display = "none"; bar.classList.remove("note"); }, 3500);
}

export function showError(err) {
  const bar = document.getElementById("nb-error");
  if (!bar) return;
  bar.textContent = String(err?.message || err);
  bar.classList.remove("note");
  bar.style.display = "block";
  clearTimeout(showError._t);
  showError._t = setTimeout(() => (bar.style.display = "none"), 6000);
}
