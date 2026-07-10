// draw.js — #39 in-browser sketch pad. Opens as a modal, saves the canvas
// as image/png, and hands the blob off to attach.js which uploads it into
// the current session's attachment queue.
//
// Deliberately minimal: pen only (with size + colour), eraser, undo (per
// stroke), and clear. No fancy tools, no layers, no filters. If you need
// heavier editing, sketch here to explain and add a real image later.

const DRAW_W = 800;
const DRAW_H = 600;

let drawCanvas = null;
let drawCtx = null;
let drawing = false;
let currentStroke = null; // {points:[{x,y}], color, size, mode}
let strokes = [];         // committed strokes, used for undo
let drawColor = "#e6edf3";
let drawSize = 4;
let eraserOn = false;

function openDrawingPad() {
  const modal = document.getElementById("draw-modal");
  if (!modal) return;
  modal.classList.add("show");
  if (!drawCanvas) initDrawing();
  clearCanvas(); // fresh every open
  strokes = [];
}
function closeDrawingPad() {
  document.getElementById("draw-modal").classList.remove("show");
}

function initDrawing() {
  drawCanvas = document.getElementById("draw-canvas");
  drawCanvas.width = DRAW_W;
  drawCanvas.height = DRAW_H;
  drawCtx = drawCanvas.getContext("2d");
  drawCtx.lineCap = "round";
  drawCtx.lineJoin = "round";

  drawCanvas.addEventListener("pointerdown", onDrawStart);
  drawCanvas.addEventListener("pointermove", onDrawMove);
  drawCanvas.addEventListener("pointerup", onDrawEnd);
  drawCanvas.addEventListener("pointercancel", onDrawEnd);
  drawCanvas.addEventListener("pointerleave", onDrawEnd);

  document.getElementById("draw-size").addEventListener("input", (ev) => {
    drawSize = parseInt(ev.target.value, 10) || 4;
    document.getElementById("draw-size-val").textContent = drawSize + "px";
  });
  for (const btn of document.querySelectorAll(".draw-color")) {
    btn.addEventListener("click", () => {
      drawColor = btn.dataset.color;
      eraserOn = false;
      syncToolbar();
    });
  }
  document.getElementById("draw-eraser").addEventListener("click", () => {
    eraserOn = !eraserOn;
    syncToolbar();
  });
  document.getElementById("draw-undo").addEventListener("click", undoStroke);
  document.getElementById("draw-clear").addEventListener("click", () => { clearCanvas(); strokes = []; });
  document.getElementById("draw-cancel").addEventListener("click", closeDrawingPad);
  document.getElementById("draw-save").addEventListener("click", saveDrawing);
  syncToolbar();
}

function syncToolbar() {
  for (const btn of document.querySelectorAll(".draw-color")) {
    btn.classList.toggle("active", !eraserOn && btn.dataset.color === drawColor);
  }
  document.getElementById("draw-eraser").classList.toggle("active", eraserOn);
}

function pointerXY(ev) {
  const rect = drawCanvas.getBoundingClientRect();
  const scaleX = DRAW_W / rect.width;
  const scaleY = DRAW_H / rect.height;
  return { x: (ev.clientX - rect.left) * scaleX, y: (ev.clientY - rect.top) * scaleY };
}

function onDrawStart(ev) {
  drawing = true;
  drawCanvas.setPointerCapture(ev.pointerId);
  const p = pointerXY(ev);
  currentStroke = {
    points: [p],
    color: eraserOn ? "#0d1117" : drawColor,   // matches modal background
    size: eraserOn ? drawSize * 3 : drawSize,
    mode: eraserOn ? "destination-out" : "source-over",
  };
  ev.preventDefault();
}
function onDrawMove(ev) {
  if (!drawing || !currentStroke) return;
  const p = pointerXY(ev);
  currentStroke.points.push(p);
  drawStrokeSegment(currentStroke, currentStroke.points.length - 2);
}
function onDrawEnd(ev) {
  if (!drawing) return;
  drawing = false;
  if (currentStroke && currentStroke.points.length > 1) {
    strokes.push(currentStroke);
  } else if (currentStroke && currentStroke.points.length === 1) {
    // Single tap → draw a dot.
    const p = currentStroke.points[0];
    drawCtx.globalCompositeOperation = currentStroke.mode;
    drawCtx.fillStyle = currentStroke.color;
    drawCtx.beginPath();
    drawCtx.arc(p.x, p.y, currentStroke.size / 2, 0, Math.PI * 2);
    drawCtx.fill();
    drawCtx.globalCompositeOperation = "source-over";
    strokes.push(currentStroke);
  }
  currentStroke = null;
}

function drawStrokeSegment(stroke, fromIdx) {
  if (fromIdx < 0 || fromIdx + 1 >= stroke.points.length) return;
  const a = stroke.points[fromIdx];
  const b = stroke.points[fromIdx + 1];
  drawCtx.globalCompositeOperation = stroke.mode;
  drawCtx.strokeStyle = stroke.color;
  drawCtx.lineWidth = stroke.size;
  drawCtx.beginPath();
  drawCtx.moveTo(a.x, a.y);
  drawCtx.lineTo(b.x, b.y);
  drawCtx.stroke();
  drawCtx.globalCompositeOperation = "source-over";
}

function clearCanvas() {
  drawCtx.fillStyle = "#0d1117";
  drawCtx.fillRect(0, 0, DRAW_W, DRAW_H);
}

function redrawAll() {
  clearCanvas();
  for (const s of strokes) {
    for (let i = 0; i < s.points.length - 1; i++) drawStrokeSegment(s, i);
  }
}

function undoStroke() {
  if (strokes.length === 0) return;
  strokes.pop();
  redrawAll();
}

async function saveDrawing() {
  if (!drawCanvas) return;
  const blob = await new Promise(res => drawCanvas.toBlob(res, "image/png"));
  if (!blob) { toast.error("Canvas empty"); return; }
  await window.collectifAttach.uploadFromBlob(blob, "sketch-" + Date.now() + ".png");
  closeDrawingPad();
  toast.success("Sketch attached — hit Send to deliver it");
}
