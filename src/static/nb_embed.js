// nb_embed.js — the notebook, mounted where the terminal used to be.
// #56 (M7), per ADR 0002 D8'.
//
// The dashboard stays the front door. What changes is what "opening a
// session" shows you: its document, projected from its transcript, with the
// raw PTY one click away rather than being the only thing on offer. Two
// pages that linked to each other become one page with two views.
//
// This file is the seam between them, and the seam is deliberately thin.
// Everything below it — the store, the fold, the WS transport, the cell
// renderer — is the same code /notebook.html runs, unmodified. The only
// things that live here are the ones that genuinely differ between a page
// and a panel: which document is open (the dashboard's selection decides,
// not a sidebar), whether the keyboard belongs to us, and when the terminal
// is allowed to exist.
//
// It talks to the dashboard's classic scripts through window, the way
// gh_issues.js and attach.js already do. Modules load after them, so every
// call in that direction is guarded and every call in this direction fails
// open: if this file never loads, renderTermPanel mounts the terminal as it
// always did, and the dashboard is exactly the product it was yesterday.

import { state, openNotebook, onChange, TOKEN } from "./nb.js";
import { mountCells, showError } from "./nb_cells.js";
import { escapeHTML, fidelityHTML } from "./nb_render.js";

const panel = document.getElementById("nb-embed");
const cellsEl = document.getElementById("nb-cells");
const fidelityEl = document.getElementById("nb-fidelity");
const emptyEl = document.getElementById("nb-embed-empty");
const bodyEl = document.getElementById("term-body");
const switchEl = document.getElementById("session-view");
const exportEl = document.getElementById("nb-embed-export");
const pageEl = document.getElementById("nb-embed-page");

// All or nothing. The one case where these are legitimately absent is the
// auth screen, which replaces document.body wholesale before any of this
// runs — and null-guarding every line below instead would hide a genuine
// markup mistake behind code that looks like it is working.
if (panel && cellsEl && switchEl) boot();

function boot() {
  // The last explicit choice, not a per-agent one. Someone driving a CLI
  // from the terminal wants the terminal for every session they open, and
  // being put back into the document on each selection reads as the app
  // arguing with them. The default is still the notebook (D8').
  const VIEW_KEY = "collectif.sessionView";
  let view = read(VIEW_KEY) === "terminal" ? "terminal" : "notebook";
  let agentID = null;
  let wantNotebook = null; // the slug the current selection should be showing
  let placeholderFor = null; // the agent whose "no document" text is on screen
  let engaged = false;

  // Jupyter's command mode binds bare letters, so the notebook may only
  // claim the keyboard when the reader is demonstrably in it. Without this,
  // `dd` typed while reading the activity feed deletes a cell in a document
  // nobody was looking at.
  document.addEventListener("pointerdown", (e) => {
    engaged = panel.contains(e.target);
  });
  // The modal check is the second half of the same problem: every dialog
  // on this page is a `.show` wrapper around a `.box`, and while one is up
  // the keys belong to it.
  mountCells(cellsEl, {
    hasFocus: () =>
      view === "notebook" && engaged && !document.querySelector(".show > .box"),
  });

  switchEl.addEventListener("click", (e) => {
    const btn = e.target.closest("[data-view]");
    if (btn) show(btn.dataset.view);
  });

  // applyView is visibility only, and is deliberately separate from show():
  // renderTermPanel calls sync(), so a sync() that re-entered
  // renderTermPanel to mount the terminal would recurse forever.
  function applyView() {
    for (const b of switchEl.querySelectorAll("[data-view]")) {
      const on = b.dataset.view === view;
      b.classList.toggle("on", on);
      b.setAttribute("aria-selected", on ? "true" : "false");
    }
    panel.style.display = view === "notebook" ? "" : "none";
    if (bodyEl) bodyEl.style.display = view === "terminal" && agentID ? "block" : "none";
  }

  function show(which) {
    view = which === "terminal" ? "terminal" : "notebook";
    write(VIEW_KEY, view);
    applyView();
    // renderTermPanel does the mounting and the xterm fit. Going through it
    // is what makes switching to the terminal show *this* session rather
    // than whichever one was mounted when we last left the view.
    if (view === "terminal" && agentID && window.renderTermPanel) {
      window.renderTermPanel(true);
    }
  }

  // sync is called from renderTermPanel on every dashboard repaint, which
  // is the one place that already knows both who is selected and what their
  // latest snapshot says. A session's notebook slug appears some seconds
  // after the session does — the transcript has to be written first — so
  // this cannot be a one-shot on selection.
  function sync(agent) {
    if (agent.id !== agentID) {
      agentID = agent.id;
      wantNotebook = null;
      placeholderFor = null;
      // Blank the cells before the new document arrives. openNotebook is a
      // round trip, and until it lands state.notebook still holds the last
      // session's turns — leaving them on screen attributes one agent's
      // work to another for as long as the fetch takes.
      cellsEl.innerHTML = "";
      fidelityEl.style.display = "none";
      // A terminal left mounted for the session you just navigated away
      // from keeps its PTY socket open and shows the wrong scrollback the
      // moment you click Terminal. Only the notebook view can leave it
      // unmounted, so only the notebook view has to clean it up.
      if (view === "notebook" && window.teardownTerminal) window.teardownTerminal();
    }
    applyView(); // keeps the panel's visibility in step with the selection
    const slug = agent.notebook || "";
    if (slug && slug !== wantNotebook) {
      wantNotebook = slug;
      placeholderFor = null;
      state.selected = null;
      openNotebook(slug).catch(showError);
    }
    if (!slug) renderPlaceholder(agent);
    renderChrome();
  }

  function clear() {
    agentID = null;
    wantNotebook = null;
    placeholderFor = null;
  }

  // Why a selected session has no document. The two reasons want different
  // words: an adapter collectif cannot read will never produce one, and
  // saying "not yet" about that is a promise we do not keep.
  // sync runs on every dashboard repaint, so this rebuilds only when the
  // subject changes — rewriting innerHTML on each tick would re-attach the
  // button's handler a few times a second for as long as a session has no
  // document, which can be its whole life on an adapter that cannot
  // project.
  function renderPlaceholder(agent) {
    if (placeholderFor === agent.id) return;
    placeholderFor = agent.id;
    const cli = agent.cli || "claude";
    const info = window.collectifCLIByName && window.collectifCLIByName[cli];
    const canProject = !info || !info.capabilities || info.capabilities.transcriptContent;
    cellsEl.innerHTML = "";
    fidelityEl.style.display = "none";
    emptyEl.style.display = "";
    emptyEl.innerHTML = canProject
      ? `<p>No document yet — this session has not written a turn collectif can read.</p>
         <p class="why">It appears here as soon as the agent's transcript does. Until then the terminal has everything.</p>`
      : `<p>collectif cannot read <b>${escapeHTML(cli)}</b>'s transcript format, so this session has no document.</p>
         <p class="why">Nothing here is scraped from the screen to make up the difference (ADR 0002 D11). Its permission requests and your prompts still work; the conversation itself is in the terminal.</p>`;
    emptyEl.innerHTML += `<p><button type="button" data-view="terminal">Open the terminal</button></p>`;
    emptyEl.querySelector("[data-view]").addEventListener("click", () => show("terminal"));
  }

  function renderChrome() {
    const nb = state.notebook;
    const showing = !!(nb && wantNotebook);
    if (showing) emptyEl.style.display = "none";

    const html = showing ? fidelityHTML(nb.fidelity) : "";
    fidelityEl.style.display = html ? "" : "none";
    fidelityEl.innerHTML = html;

    const id = showing ? state.id : "";
    for (const [el, href] of [
      [exportEl, id ? `/api/nb/${encodeURIComponent(id)}/export?token=${encodeURIComponent(TOKEN)}` : ""],
      [pageEl, id ? `/notebook.html?token=${encodeURIComponent(TOKEN)}#${encodeURIComponent(id)}` : ""],
    ]) {
      if (!el) continue;
      el.style.display = href ? "" : "none";
      if (href) el.href = href;
    }
  }

  onChange(renderChrome);
  show(view);

  // The dashboard's half of the seam. current() is what lets
  // renderTermPanel decide whether to mount an xterm at all: under D8' a
  // selection no longer implies a PTY socket, and opening one for a session
  // whose terminal nobody looked at is work and bandwidth spent on a hidden
  // element.
  window.collectifSessionView = { current: () => view, show, sync, clear };
}

function read(key) {
  try { return sessionStorage.getItem(key) || ""; } catch (_) { return ""; }
}
function write(key, value) {
  try { sessionStorage.setItem(key, value); } catch (_) { /* private mode */ }
}
