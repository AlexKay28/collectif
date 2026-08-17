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

const qs = new URLSearchParams(location.search);
export const TOKEN = qs.get("token") || "";

export const state = {
  id: null,
  notebook: null,
  version: 0,
  selected: null, // cell id
  mode: "command", // "command" | "edit"
  live: new Map(), // cellId -> streaming text for the current run
  ws: null,
  backoff: 1000,
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
  create: (title, root) => api("POST", "/api/nb", { title, root }),
  get: (id) => api("GET", `/api/nb/${id}`),
  remove: (id) => api("DELETE", `/api/nb/${id}`),
  setMeta: (id, patch) => api("PATCH", `/api/nb/${id}`, patch),
  addCell: (id, cell) => api("POST", `/api/nb/${id}/cells`, cell),
  editCell: (id, cid, patch) => api("PATCH", `/api/nb/${id}/cells/${cid}`, patch),
  deleteCell: (id, cid) => api("DELETE", `/api/nb/${id}/cells/${cid}`),
  moveCell: (id, cid, beforeCellId) =>
    api("POST", `/api/nb/${id}/cells/${cid}/move`, { beforeCellId }),
  runCell: (id, cid) => api("POST", `/api/nb/${id}/cells/${cid}/run`),
  interruptCell: (id, cid) => api("POST", `/api/nb/${id}/cells/${cid}/interrupt`),
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
      let at = nb.cells.length;
      if (p.afterCellId) {
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
