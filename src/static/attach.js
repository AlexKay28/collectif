// attach.js — #39 image attachment queue for the terminal composer.
//
// Three ways to attach:
//   • paste (Ctrl+V on the dashboard while a session is selected)
//   • drag & drop onto the terminal area
//   • the 📎 file-picker button
// Queued attachments show as chips above the composer. On Send, the server
// composes "@<path1> @<path2> <text>\r" and writes it to the PTY. Each chip
// tracks a delivery status:
//   queued (blue)   → uploaded but not sent
//   sent   (amber)  → sent, waiting for Claude to read it
//   seen   (green)  → PostToolUse observed the path — delivered
//   stale  (red)    → 15 s passed, no read → suggest re-attach

// Per-session queue: sessionId → array of chip objects.
const attachQueues = new Map();
function queueFor(id) {
  if (!attachQueues.has(id)) attachQueues.set(id, []);
  return attachQueues.get(id);
}

function readAsDataURL(blob) {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onload = () => resolve(r.result);
    r.onerror = () => reject(r.error);
    r.readAsDataURL(blob);
  });
}

async function uploadAttachment(file, sessionId) {
  if (!sessionId) { toast.error("Select an agent first"); return null; }
  const form = new FormData();
  form.append("file", file, file.name || "attachment.png");
  const res = await fetch("/api/agents/" + sessionId + "/attach", {
    method: "POST",
    body: form,
  });
  if (!res.ok) {
    const txt = await res.text();
    toast.error("Attach failed: " + txt);
    return null;
  }
  const meta = await res.json();
  const thumb = await readAsDataURL(file);
  const chip = { ...meta, sessionId, status: "queued", thumb };
  queueFor(sessionId).push(chip);
  scheduleRender("term");
  return chip;
}

function removeChip(sessionId, path) {
  const q = queueFor(sessionId);
  const idx = q.findIndex(c => c.path === path);
  if (idx >= 0) q.splice(idx, 1);
  scheduleRender("term");
}

// Called from the dashboard WS event dispatch when the server reports
// delivery status changes.
function markAttachmentStatus(sessionId, paths, status) {
  const q = queueFor(sessionId);
  let touched = false;
  for (const c of q) {
    if (paths.includes(c.path)) {
      c.status = status;
      touched = true;
    }
  }
  if (touched) scheduleRender("term");
}

async function sendComposedMessage(text) {
  if (!selectedId) return;
  const q = queueFor(selectedId);
  const paths = q.filter(c => c.status === "queued").map(c => c.path);
  if (paths.length === 0 && !text.trim()) return;
  const res = await fetch("/api/agents/" + selectedId + "/send", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ text, paths }),
  });
  if (!res.ok) {
    toast.error("Send failed: " + await res.text());
    return;
  }
  // Optimistic local status bump — server will also broadcast attachment_sent
  // which will land the same result via the WS dispatch.
  for (const c of q) if (paths.includes(c.path)) c.status = "sent";
  scheduleRender("term");
  const box = document.getElementById("compose-input");
  if (box) box.value = "";
}

// Render the chip strip + composer inside the terminal panel head. Called
// by scheduleRender("term").
function renderAttachmentStrip() {
  // #39 The composer sits OUTSIDE the terminal panel now so the xterm stays
  // a pure Claude CLI window. compose-panel is the outer wrapper we toggle
  // for visibility; compose-strip inside holds the chips + input row.
  const panel = document.getElementById("compose-panel");
  const container = document.getElementById("compose-strip");
  if (!container) return;
  if (!selectedId) {
    if (panel) panel.style.display = "none";
    return;
  }
  if (panel) panel.style.display = "block";
  const q = queueFor(selectedId);
  const chips = container.querySelector(".chips");
  chips.innerHTML = "";
  for (const c of q) {
    const el = document.createElement("div");
    el.className = "chip status-" + c.status;
    el.title = c.name + " (" + fmtSize(c.sizeBytes) + ") — " + c.status;
    el.innerHTML =
      '<img class="thumb" src="' + esc(c.thumb) + '" alt="">' +
      '<span class="name">' + esc(shortName(c.name)) + '</span>' +
      '<span class="dot ' + c.status + '" aria-hidden="true"></span>' +
      '<button class="rm" title="Remove">×</button>';
    el.querySelector(".rm").addEventListener("click", (ev) => {
      ev.stopPropagation();
      removeChip(selectedId, c.path);
    });
    chips.appendChild(el);
  }
}

function shortName(n) { return n.length > 20 ? n.slice(0, 17) + "…" : n; }
function fmtSize(b) {
  if (b < 1024) return b + " B";
  if (b < 1024*1024) return (b/1024).toFixed(0) + " KB";
  return (b/1024/1024).toFixed(1) + " MB";
}

// Wire up paste, drag-drop, and picker to the currently selected session.
function bootAttach() {
  const dropZone = document.getElementById("term-panel-root") || document.getElementById("term-body");

  // Paste — anywhere in the app while an agent is selected.
  document.addEventListener("paste", async (ev) => {
    if (!selectedId) return;
    const items = ev.clipboardData && ev.clipboardData.items;
    if (!items) return;
    let uploaded = 0;
    for (const it of items) {
      if (it.kind !== "file") continue;
      const file = it.getAsFile();
      if (!file || !file.type.startsWith("image/")) continue;
      if (!file.name) Object.defineProperty(file, "name", { value: "paste-" + Date.now() + ".png" });
      await uploadAttachment(file, selectedId);
      uploaded++;
    }
    if (uploaded > 0) {
      ev.preventDefault();
      toast.success("Attached " + uploaded + (uploaded === 1 ? " image" : " images"));
    }
  });

  // Drag & drop — highlight while dragging over, upload on drop.
  if (dropZone) {
    let dragDepth = 0;
    dropZone.addEventListener("dragenter", (ev) => {
      if (!selectedId) return;
      if (!ev.dataTransfer || !Array.from(ev.dataTransfer.types).includes("Files")) return;
      dragDepth++;
      ev.preventDefault();
      dropZone.classList.add("drop-active");
    });
    dropZone.addEventListener("dragover", (ev) => {
      if (!selectedId) return;
      if (!ev.dataTransfer || !Array.from(ev.dataTransfer.types).includes("Files")) return;
      ev.preventDefault();
      ev.dataTransfer.dropEffect = "copy";
    });
    dropZone.addEventListener("dragleave", (ev) => {
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) dropZone.classList.remove("drop-active");
    });
    dropZone.addEventListener("drop", async (ev) => {
      dragDepth = 0;
      dropZone.classList.remove("drop-active");
      if (!selectedId) return;
      if (!ev.dataTransfer || !ev.dataTransfer.files || !ev.dataTransfer.files.length) return;
      ev.preventDefault();
      let uploaded = 0;
      for (const f of ev.dataTransfer.files) {
        if (!f.type.startsWith("image/")) continue;
        await uploadAttachment(f, selectedId);
        uploaded++;
      }
      if (uploaded > 0) toast.success("Attached " + uploaded + (uploaded === 1 ? " image" : " images"));
    });
  }

  // File-picker + drawing-pad buttons.
  const pickBtn = document.getElementById("compose-pick");
  const pickInput = document.getElementById("compose-file");
  if (pickBtn && pickInput) {
    pickBtn.addEventListener("click", () => pickInput.click());
    pickInput.addEventListener("change", async () => {
      for (const f of pickInput.files) {
        if (!f.type.startsWith("image/")) continue;
        await uploadAttachment(f, selectedId);
      }
      pickInput.value = "";
    });
  }
  const drawBtn = document.getElementById("compose-draw");
  if (drawBtn) drawBtn.addEventListener("click", () => openDrawingPad());

  // Send.
  const sendBtn = document.getElementById("compose-send");
  const input = document.getElementById("compose-input");
  if (sendBtn && input) {
    sendBtn.addEventListener("click", () => sendComposedMessage(input.value));
    input.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" && !ev.shiftKey) {
        ev.preventDefault();
        sendComposedMessage(input.value);
      }
    });
  }
}

// Expose for dashboard WS dispatch.
window.collectifAttach = {
  markAttachmentStatus,
  uploadFromBlob: async (blob, name) => {
    if (!selectedId) { toast.error("Select an agent first"); return; }
    const file = new File([blob], name || "drawing-" + Date.now() + ".png", { type: blob.type || "image/png" });
    return uploadAttachment(file, selectedId);
  },
};
