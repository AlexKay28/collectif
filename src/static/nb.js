// nb.js — notebook store, transport, and the client-side fold.
// #49 (M1 slice 3).
//
// The server owns the document; this file keeps a local copy in step. Two
// message kinds arrive on the socket:
//
//   fold   the whole document, plus the log position it was taken at
//   event  one log entry, carrying the position it was applied at
//   delta  a fragment of a running cell's output (live only, never logged)
//
// Applying events locally rather than refetching keeps typing responsive
// and lets output stream. The safety net is the version number: anything at
// or below the folded position is already included, and anything this build
// doesn't understand triggers a refetch instead of a guess.

// The token reaches us two ways and both are needed. On /notebook.html it
// is in the query string. Inside the dashboard it is not: auth.js takes it
// out of the URL and stashes it, so reading only location.search gave the
// embedded notebook an empty token — its fetches survived on auth.js's
// Authorization header, but every WebSocket URL carries the token in the
// query and those failed the auth gate with nothing on screen to say why.
const qs = new URLSearchParams(location.search);
export const TOKEN =
  qs.get("token") || sessionStorage.getItem("collectif.token") || "";

export const state = {
  id: null,
  notebook: null,
  version: 0,
  selected: null, // cell id
  mode: "command", // "command" | "edit"
  live: new Map(), // cellId -> streaming text for the current run
  // cellId -> a failure that belongs to that cell. Kept until the user
  // acts, unlike the banner at the top of the page: a toast for a cell
  // you are not looking at is how you press run and see nothing happen.
  cellErrors: new Map(),
  // Jupyter's command-mode verbs need somewhere to keep their state.
  clipboard: null,        // a cut/copied cell, pasted with v / shift+v
  trash: [],              // deleted cells, restored newest-first with z
  hiddenOutputs: new Set(), // cells whose output is collapsed (o)
  tallOutputs: new Set(),   // cells whose output scroll is unlocked (shift+o)
  ws: null,
  backoff: 1000,
  // PTY sessions from the original dashboard. They are a different kind of
  // thing to a notebook — a running process rather than a document — so
  // they get their own list rather than being flattened together.
  sessions: [],
  dashWS: null,
  dashBackoff: 1000,
};

const listeners = new Set();
export function onChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
function emit() {
  for (const fn of listeners) fn();
}

// ─── HTTP ─────────────────────────────────────────────────────────────

function withToken(path) {
  return path + (path.includes("?") ? "&" : "?") + "token=" + encodeURIComponent(TOKEN);
}

export async function api(method, path, body) {
  const res = await fetch(withToken(path), {
    method,
    headers: body ? { "content-type": "application/json" } : {},
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    const text = (await res.text()).trim();
    throw new Error(text || res.statusText);
  }
  if (res.status === 204) return null;
  const ct = res.headers.get("content-type") || "";
  return ct.includes("json") ? res.json() : res.text();
}

export const nbAPI = {
  list: () => api("GET", "/api/nb"),
  sessions: () => api("GET", "/api/agents"),
  create: (title, root) => api("POST", "/api/nb", { title, root }),
  get: (id) => api("GET", `/api/nb/${id}`),
  remove: (id) => api("DELETE", `/api/nb/${id}`),
  setMeta: (id, patch) => api("PATCH", `/api/nb/${id}`, patch),
  addCell: (id, cell) => api("POST", `/api/nb/${id}/cells`, cell),
  editCell: (id, cid, patch) => api("PATCH", `/api/nb/${id}/cells/${cid}`, patch),
  setCellType: (id, cid, type) => api("PATCH", `/api/nb/${id}/cells/${cid}`, { type }),
  deleteCell: (id, cid) => api("DELETE", `/api/nb/${id}/cells/${cid}`),
  moveCell: (id, cid, beforeCellId) =>
    api("POST", `/api/nb/${id}/cells/${cid}/move`, { beforeCellId }),
  runCell: (id, cid) => api("POST", `/api/nb/${id}/cells/${cid}/run`),
  interruptCell: (id, cid) => api("POST", `/api/nb/${id}/cells/${cid}/interrupt`),
  answerApproval: (id, cid, answer) =>
    api("POST", `/api/nb/${id}/cells/${cid}/approve`, { answer }),
};

// ─── Fold ─────────────────────────────────────────────────────────────

function cellIndex(nb, id) {
  return nb.cells ? nb.cells.findIndex((c) => c.id === id) : -1;
}

// applyEvent mirrors applyEvent in nb_doc.go for the types the UI needs.
// Returns false when the type is unrecognised, which the caller answers by
// refetching rather than by rendering a document that has silently drifted.
function applyEvent(nb, ev) {
  const p = ev.payload || {};
  switch (ev.type) {
    case "notebook_created":
      nb.title = p.title;
      nb.root = p.root;
      return true;
    case "meta_set":
      if (p.title !== undefined && p.title !== null) nb.title = p.title;
      if (p.meta) nb.meta = p.meta;
      return true;
    case "cell_inserted": {
      const cell = p.cell;
      if (!cell.state) cell.state = "idle";
      // Mirrors applyEvent in nb_doc.go: beforeCellId wins, then
      // afterCellId, then append. The two must agree or the client drifts
      // from the server until the next fold.
      let at = nb.cells.length;
      if (p.beforeCellId) {
        const i = cellIndex(nb, p.beforeCellId);
        if (i >= 0) at = i;
      } else if (p.afterCellId) {
        const i = cellIndex(nb, p.afterCellId);
        if (i >= 0) at = i + 1;
      }
      nb.cells.splice(at, 0, cell);
      return true;
    }
    case "cell_edited": {
      const i = cellIndex(nb, p.cellId);
      if (i < 0) return true;
      if (p.source !== undefined && p.source !== null) nb.cells[i].source = p.source;
      if (p.meta) nb.cells[i].meta = p.meta;
      return true;
    }
    case "cell_moved": {
      const from = cellIndex(nb, p.cellId);
      if (from < 0) return true;
      const [cell] = nb.cells.splice(from, 1);
      let to = nb.cells.length;
      if (p.beforeCellId) {
        const i = cellIndex(nb, p.beforeCellId);
        if (i >= 0) to = i;
      }
      nb.cells.splice(to, 0, cell);
      return true;
    }
    case "cell_deleted": {
      const i = cellIndex(nb, p.cellId);
      if (i >= 0) nb.cells.splice(i, 1);
      return true;
    }
    case "run_started": {
      const i = cellIndex(nb, p.cellId);
      if (i < 0) return true;
      nb.cells[i].outputs = [];
      nb.cells[i].runId = p.runId;
      nb.cells[i].state = "running";
      state.live.set(p.cellId, "");
      return true;
    }
    case "output_appended": {
      const i = cellIndex(nb, p.cellId);
      if (i < 0) return true;
      (nb.cells[i].outputs = nb.cells[i].outputs || []).push(p.output);
      return true;
    }
    case "run_finished": {
      const i = cellIndex(nb, p.cellId);
      if (i < 0) return true;
      nb.cells[i].state = p.status || "ok";
      // The finalised output has arrived; drop the streaming copy so the
      // cell shows the record rather than a live view of it.
      state.live.delete(p.cellId);
      return true;
    }
    case "cells_invalidated":
      for (const id of p.cellIds || []) {
        const i = cellIndex(nb, id);
        if (i >= 0 && (nb.cells[i].state === "ok" || nb.cells[i].state === "error")) {
          nb.cells[i].state = "stale";
        }
      }
      return true;
    default:
      return false; // written by a newer build — resync instead of guessing
  }
}

// ─── Transport ────────────────────────────────────────────────────────

export async function openNotebook(id) {
  state.id = id;
  state.notebook = await nbAPI.get(id);
  state.version = state.notebook.version || 0;
  state.live.clear();
  if (!state.selected && state.notebook.cells?.length) {
    state.selected = state.notebook.cells[0].id;
  }
  emit();
  connect();
}

async function resync() {
  if (!state.id) return;
  state.notebook = await nbAPI.get(state.id);
  state.version = state.notebook.version || 0;
  emit();
}

function connect() {
  if (state.ws) {
    state.ws.onclose = null;
    state.ws.close();
  }
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const url = `${proto}//${location.host}/ws/notebook/${state.id}?token=${encodeURIComponent(TOKEN)}`;
  const ws = new WebSocket(url);
  state.ws = ws;

  ws.onopen = () => {
    state.backoff = 1000;
  };

  ws.onmessage = (e) => {
    let msg;
    try {
      msg = JSON.parse(e.data);
    } catch {
      return;
    }
    if (msg.type === "fold") {
      state.notebook = msg.notebook;
      state.version = msg.version ?? msg.notebook.version ?? 0;
      // Live buffers for cells that are mid-run. Without these a refresh
      // during a long command shows a running cell with nothing in it,
      // because deltas are never written to the log.
      state.live.clear();
      for (const [cellId, live] of Object.entries(msg.live || {})) {
        state.live.set(cellId, live.text || "");
      }
      emit();
      return;
    }
    if (msg.type === "event") {
      // Anything at or below the folded position is already applied. This
      // is what makes subscribe-before-fold safe on the server side.
      if (msg.seq !== undefined && msg.seq <= state.version) return;
      if (!state.notebook) return;
      if (!applyEvent(state.notebook, msg.event)) {
        resync();
        return;
      }
      state.version = msg.seq ?? state.version + 1;
      emit();
      return;
    }
    if (msg.type === "delta") {
      const prev = state.live.get(msg.cellId) || "";
      state.live.set(msg.cellId, prev + msg.text);
      emit();
    }
  };

  ws.onclose = () => {
    state.ws = null;
    // Reconnecting is a re-fold, so a dropped socket costs a round trip
    // and nothing else.
    setTimeout(() => {
      if (state.id) connect();
    }, state.backoff);
    state.backoff = Math.min(state.backoff * 2, 15000);
  };
}


// ─── PTY sessions ─────────────────────────────────────────────────────

// The dashboard's own stream, consumed read-only. The notebook never
// drives a session — spawning a session and the terminal itself belong to
// the dashboard, which stays the front door (ADR 0002 D8'). This is a view.
//
// Only /notebook.html calls this. Inside the dashboard the page already
// holds one /ws/dashboard socket, and a second one from the same tab would
// double every broadcast for a list this embed does not draw.
export async function watchSessions() {
  try {
    state.sessions = (await nbAPI.sessions()) || [];
    emit();
  } catch {
    // A dashboard that is unreachable should not stop the notebook
    // working; the section simply stays empty.
  }
  connectDashboard();
}

function connectDashboard() {
  if (state.dashWS) {
    state.dashWS.onclose = null;
    state.dashWS.close();
  }
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  const ws = new WebSocket(`${proto}//${location.host}/ws/dashboard?token=${encodeURIComponent(TOKEN)}`);
  state.dashWS = ws;

  ws.onopen = () => { state.dashBackoff = 1000; };
  ws.onmessage = (e) => {
    let msg;
    try { msg = JSON.parse(e.data); } catch { return; }
    if (msg.type === "snapshot") {
      state.sessions = msg.agents || [];
    } else if (msg.type === "upsert") {
      const i = state.sessions.findIndex((a) => a.id === msg.agent.id);
      if (i >= 0) state.sessions[i] = msg.agent;
      else state.sessions.push(msg.agent);
    } else if (msg.type === "remove") {
      state.sessions = state.sessions.filter((a) => a.id !== msg.id);
    } else {
      return; // context_pressure, cost_warning and friends are not ours
    }
    emit();
  };
  ws.onclose = () => {
    state.dashWS = null;
    setTimeout(connectDashboard, state.dashBackoff);
    state.dashBackoff = Math.min(state.dashBackoff * 2, 15000);
  };
}
