// nb_search.js — search across every notebook, from the sidebar. #58.
//
// The sidebar is where you choose what to read, and a search result is the
// same kind of choice — so results take the sidebar over while a query is
// live rather than opening a page of their own. Clearing the field puts the
// notebook list back exactly as it was.
//
// The three filters are the reason this exists. A transcript is one column
// of undifferentiated text; a notebook keeps what you asked apart from what
// the agent said apart from what it ran (ADR 0002 §3), so the filters are
// named for those acts — asked / replied / ran — rather than for the field
// names underneath them.

import { state, api, openNotebook } from "./nb.js";
import { escapeHTML } from "./nb_render.js";
import { revealCell, showError } from "./nb_cells.js";

// Long enough that a fast typist makes one request per word rather than one
// per keystroke; short enough that it still feels like it is keeping up.
const DEBOUNCE_MS = 180;

const KINDS = [
  ["prompt", "asked", "what a human typed"],
  ["output", "replied", "what the agent said"],
  ["tool", "ran", "tool calls, their results, and requests to run them"],
];

let els = {};
let kinds = new Set();
let timer = null;
let seq = 0;

export function mountSearch() {
  els = {
    input: document.getElementById("nb-q"),
    filters: document.getElementById("nb-q-kinds"),
    results: document.getElementById("nb-results"),
    browse: document.getElementById("nb-browse"),
  };
  if (!els.input) return;

  els.filters.innerHTML = KINDS.map(
    ([k, label, why]) =>
      `<button type="button" data-kind="${k}" title="${escapeHTML(why)}">${label}</button>`,
  ).join("");
  els.filters.querySelectorAll("[data-kind]").forEach((b) =>
    b.addEventListener("click", () => {
      const k = b.dataset.kind;
      if (kinds.has(k)) kinds.delete(k);
      else kinds.add(k);
      b.classList.toggle("on", kinds.has(k));
      run();
    }),
  );

  els.input.addEventListener("input", schedule);
  els.input.addEventListener("keydown", (e) => {
    // Esc gets you out of a search the same way it gets you out of a cell.
    if (e.key === "Escape") {
      e.preventDefault();
      clear();
      els.input.blur();
    }
  });

  // `/` is the one key this adds to the notebook's Jupyter vocabulary. It
  // is free — Jupyter binds nothing to it in command mode — and it is what
  // every other document reader on the machine uses for find.
  document.addEventListener("keydown", (e) => {
    if (e.key !== "/" || e.altKey || e.ctrlKey || e.metaKey) return;
    const tag = document.activeElement?.tagName;
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
    if (state.mode === "edit") return;
    e.preventDefault();
    els.input.focus();
    els.input.select();
  });
}

function schedule() {
  clearTimeout(timer);
  timer = setTimeout(run, DEBOUNCE_MS);
}

function clear() {
  els.input.value = "";
  run();
}

async function run() {
  const q = els.input.value.trim();
  if (!q) {
    show(false);
    els.results.innerHTML = "";
    return;
  }
  let path = `/api/search?q=${encodeURIComponent(q)}`;
  for (const k of kinds) path += `&kind=${k}`;

  // Responses can land out of order — a short query is answered from a warm
  // index while a longer one is still folding a log. Only the newest one
  // may draw, or the results flick back to a previous query's answer.
  const mine = ++seq;
  show(true);
  els.results.innerHTML = `<div class="sr-note">searching…</div>`;
  try {
    const res = await api("GET", path);
    if (mine !== seq) return;
    render(res);
  } catch (err) {
    if (mine !== seq) return;
    els.results.innerHTML = `<div class="sr-note">${escapeHTML(String(err?.message || err))}</div>`;
  }
}

function show(searching) {
  els.results.hidden = !searching;
  els.browse.hidden = searching;
}

function render(res) {
  if (!res || !res.count) {
    els.results.innerHTML = `<div class="sr-note">No matches. Every term has to appear in the same block.</div>`;
    return;
  }
  const head =
    `<div class="sr-head">${res.count} ${res.count === 1 ? "match" : "matches"} in ` +
    `${res.groups.length} notebook${res.groups.length === 1 ? "" : "s"}` +
    (res.truncated ? ` <span class="sr-more">of ${res.total}</span>` : "") +
    `</div>`;

  els.results.innerHTML =
    head +
    res.groups
      .map(
        (g) => `
      <div class="sr-group">
        <div class="sr-nb" title="${escapeHTML(g.root || "")}">
          <span class="sr-nb-title">${escapeHTML(g.title || g.notebook)}</span>
          ${g.cli ? `<span class="sr-cli">${escapeHTML(g.cli)}</span>` : ""}
          <span class="sr-count">${g.total}</span>
        </div>
        ${g.hits.map((h) => hitRow(g, h)).join("")}
      </div>`,
      )
      .join("");

  els.results.querySelectorAll("[data-cell]").forEach((el) =>
    el.addEventListener("click", async () => {
      try {
        if (el.dataset.nb !== state.id) {
          state.selected = null;
          await openNotebook(el.dataset.nb);
        }
        revealCell(el.dataset.cell);
      } catch (err) {
        showError(err);
      }
    }),
  );
}

const KIND_LABEL = {
  prompt: "asked",
  output: "replied",
  tool: "ran",
  note: "note",
  injection: "injected",
};

// A row has to answer "is this the one?" without opening anything: what act
// it was, how that turn ended, the matched text, and — when the match is
// not the prompt itself — the question that led to it. The prompt line is
// what makes a hit inside a subagent legible at all (#55a): on its own,
// "some agent said this" is the transcript problem again.
function hitRow(g, h) {
  const state_ = h.state || "idle";
  const agent = h.agentId ? `<span class="sr-agent">subagent</span>` : "";
  const tool = h.tool ? `<span class="sr-tool">${escapeHTML(h.tool)}</span>` : "";
  const context =
    h.kind !== "prompt" && h.prompt
      ? `<div class="sr-in">${escapeHTML(h.prompt)}</div>`
      : "";
  return `
    <div class="sr-hit state-${escapeHTML(state_)}" data-nb="${escapeHTML(g.notebook)}"
         data-cell="${escapeHTML(h.cellId)}" title="cell ${h.cellIndex}">
      <div class="sr-meta">
        <span class="sr-dot"></span>
        <span class="sr-kind">${escapeHTML(KIND_LABEL[h.kind] || h.kind)}</span>
        ${tool}${agent}
      </div>
      <div class="sr-snip">${escapeHTML(h.snippet || "")}</div>
      ${context}
    </div>`;
}
