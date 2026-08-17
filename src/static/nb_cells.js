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

let root;
let pendingDD = false;

export function mountCells(el) {
  root = el;
  onChange(render);
  document.addEventListener("keydown", onKeyDown);
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

  root.innerHTML =
    (nb.cells || []).map(renderCell).join("") +
    `<div class="nb-add-row">
       <button data-add="markdown">+ Markdown</button>
       <button data-add="shell">+ Shell</button>
       <button data-add="prompt" title="Runs once the agent loop lands (M2)">+ Prompt</button>
     </div>`;

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

function renderCell(cell) {
  const selected = cell.id === state.selected;
  const editing = selected && state.mode === "edit";
  const live = state.live.get(cell.id);
  const outputs = renderOutputs(cell, live);

  const body = editing
    ? `<textarea class="nb-src" data-cell-id="${escapeHTML(cell.id)}" spellcheck="false"
         rows="${Math.max(2, String(cell.source || "").split("\n").length)}">${escapeHTML(cell.source || "")}</textarea>`
    : cell.type === "markdown"
      ? `<div class="nb-md">${renderMarkdown(cell.source) || '<p class="nb-hint">Empty markdown cell — press Enter to edit.</p>'}</div>`
      : `<pre class="nb-src-view">${escapeHTML(cell.source || "") || '<span class="nb-hint">Empty — press Enter to edit.</span>'}</pre>`;

  // #50: prompt cells run through the agent loop now.
  const runnable = cell.type === "shell" || cell.type === "prompt";
  const running = cell.state === "running";

  return `
  <div class="nb-cell ${selected ? "sel" : ""} ${editing ? "editing" : ""} state-${escapeHTML(cell.state || "idle")}"
       data-cell="${escapeHTML(cell.id)}">
    <div class="nb-gutter">
      <span class="nb-type">${escapeHTML(cell.type)}</span>
      ${stateChip(cell)}
    </div>
    <div class="nb-body">
      ${body}
      ${outputs}
    </div>
    <div class="nb-actions">
      ${running
        ? `<button data-interrupt="${escapeHTML(cell.id)}" title="Interrupt">■</button>`
        : runnable
          ? `<button data-run="${escapeHTML(cell.id)}" title="Run (Shift+Enter)">▶</button>`
          : ``}
      <button data-del="${escapeHTML(cell.id)}" title="Delete (dd)">✕</button>
    </div>
  </div>`;
}

function stateChip(cell) {
  const s = cell.state || "idle";
  if (s === "idle") return "";
  const label = {
    running: "running",
    ok: okLabel(cell),
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
  const inTok = (u.inputTokens || 0) + (u.cacheReadTokens || 0) + (u.cacheCreationTokens || 0);
  if (inTok || u.outputTokens) parts.push(`${fmtTokens(inTok)}→${fmtTokens(u.outputTokens || 0)}`);
  return parts.length ? parts.join(" · ") : "ok";
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
  root.querySelectorAll("[data-del]").forEach((b) =>
    b.addEventListener("click", (e) => {
      e.stopPropagation();
      deleteCell(b.dataset.del);
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

async function runCell(id) {
  const cell = findCell(id);
  if (!cell) return;
  try {
    await nbAPI.runCell(state.id, id);
  } catch (err) {
    // A prompt or file cell answers 501 until M2. Say so plainly rather
    // than failing silently.
    showError(err);
  }
}

// ─── Keyboard ─────────────────────────────────────────────────────────

function onEditorKeyDown(e) {
  const ta = e.currentTarget;
  if (e.key === "Escape") {
    e.preventDefault();
    commit(ta).then(() => {
      state.mode = "command";
      ta.blur();
      render();
    });
    return;
  }
  if (e.key === "Enter" && (e.shiftKey || e.ctrlKey || e.metaKey)) {
    e.preventDefault();
    const id = ta.dataset.cellId;
    commit(ta).then(() => {
      state.mode = "command";
      runIfRunnable(id);
      if (e.shiftKey) selectNext();
      render();
    });
  }
}

function onKeyDown(e) {
  if (!state.notebook) return;
  if (state.mode === "edit") return;
  const tag = document.activeElement?.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;

  // Jupyter binds the unmodified keys only. Without this, Cmd/Ctrl+A
  // inserts a cell instead of selecting all, Cmd/Ctrl+D arms dd, and
  // Cmd+J/Cmd+K are swallowed as navigation.
  if (e.altKey || e.metaKey || (e.ctrlKey && e.key !== "Enter")) return;

  const cells = state.notebook.cells || [];
  const i = selectedIndex();

  const handled = () => {
    e.preventDefault();
    render();
  };

  if (e.key !== "d") pendingDD = false;

  switch (e.key) {
    case "Enter":
      if (e.shiftKey || e.ctrlKey || e.metaKey) {
        if (state.selected) runIfRunnable(state.selected);
        if (e.shiftKey) selectNext();
        return handled();
      }
      if (state.selected) {
        state.mode = "edit";
        return handled();
      }
      return;
    case "j":
    case "ArrowDown":
      if (i >= 0 && i < cells.length - 1) state.selected = cells[i + 1].id;
      return handled();
    case "k":
    case "ArrowUp":
      if (i > 0) state.selected = cells[i - 1].id;
      return handled();
    case "a":
      insertCell("shell", state.selected ? { beforeCellId: state.selected } : {});
      return handled();
    case "b":
      insertCell("shell", state.selected ? { afterCellId: state.selected } : {});
      return handled();
    case "m":
      if (state.selected) setType(state.selected, "markdown");
      return handled();
    case "y":
      if (state.selected) setType(state.selected, "shell");
      return handled();
    case "d":
      if (pendingDD) {
        pendingDD = false;
        if (state.selected) deleteCell(state.selected);
        return handled();
      }
      pendingDD = true;
      return;
    case "i":
      if (state.selected) nbAPI.interruptCell(state.id, state.selected).catch(() => {});
      return handled();
    default:
      return;
  }
}

// setType is a delete-and-reinsert: cell type is immutable in the schema
// because an event that changed it would leave outputs from a different
// kind of run attached.
async function setType(id, type) {
  const cells = state.notebook.cells || [];
  const i = cells.findIndex((c) => c.id === id);
  if (i < 0 || cells[i].type === type) return;
  const source = cells[i].source;
  try {
    // Insert in front of the cell being replaced, so the replacement lands
    // exactly where the original was. Anchoring to the previous cell sent
    // the first cell of a notebook to the bottom — movement the user never
    // asked for, and not undoable.
    const res = await nbAPI.addCell(state.id, { type, source, beforeCellId: id });
    await nbAPI.deleteCell(state.id, id);
    state.selected = res.cellId;
  } catch (err) {
    showError(err);
  }
}

function runIfRunnable(id) {
  const cell = findCell(id);
  if (cell && cell.type !== "markdown") runCell(id);
}

function selectNext() {
  const cells = state.notebook.cells || [];
  const i = selectedIndex();
  if (i >= 0 && i < cells.length - 1) state.selected = cells[i + 1].id;
}

export function showError(err) {
  const bar = document.getElementById("nb-error");
  if (!bar) return;
  bar.textContent = String(err?.message || err);
  bar.style.display = "";
  clearTimeout(showError._t);
  showError._t = setTimeout(() => (bar.style.display = "none"), 6000);
}
