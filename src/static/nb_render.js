// nb_render.js — markdown and cell-output rendering. #49 (M1 slice 3).
//
// A deliberately small markdown subset: headings, emphasis, code, links,
// lists, rules, paragraphs. The alternative was a CDN library, which the
// Artifact-style CSP and the repo's no-build-step stance both rule out, and
// a notebook needs far less than CommonMark to be readable.
//
// Everything is escaped before any markup is produced, so a cell's source
// can never introduce HTML.

export function escapeHTML(s) {
  return String(s)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;");
}

function inline(s) {
  return (
    escapeHTML(s)
      // code spans first: their contents must not be re-marked up
      .replace(/`([^`]+)`/g, (_, c) => `<code>${c}</code>`)
      .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
      .replace(/(^|\W)\*([^*]+)\*/g, "$1<em>$2</em>")
      .replace(/\[([^\]]+)\]\((https?:[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>')
  );
}

export function renderMarkdown(src) {
  const lines = String(src || "").split("\n");
  const out = [];
  let para = [];
  let list = null; // "ul" | "ol"
  let fence = null;

  const flushPara = () => {
    if (para.length) {
      out.push(`<p>${inline(para.join(" "))}</p>`);
      para = [];
    }
  };
  const flushList = () => {
    if (list) {
      out.push(`</${list}>`);
      list = null;
    }
  };

  for (const raw of lines) {
    const line = raw.replace(/\s+$/, "");

    if (fence !== null) {
      if (/^```/.test(line)) {
        out.push(`<pre class="code"><code>${escapeHTML(fence.join("\n"))}</code></pre>`);
        fence = null;
      } else {
        fence.push(raw);
      }
      continue;
    }
    if (/^```/.test(line)) {
      flushPara();
      flushList();
      fence = [];
      continue;
    }

    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      flushPara();
      flushList();
      const level = h[1].length;
      out.push(`<h${level}>${inline(h[2])}</h${level}>`);
      continue;
    }

    if (/^(-{3,}|\*{3,})$/.test(line)) {
      flushPara();
      flushList();
      out.push("<hr>");
      continue;
    }

    const ul = /^\s*[-*]\s+(.*)$/.exec(line);
    const ol = /^\s*\d+\.\s+(.*)$/.exec(line);
    if (ul || ol) {
      flushPara();
      const want = ul ? "ul" : "ol";
      if (list !== want) {
        flushList();
        out.push(`<${want}>`);
        list = want;
      }
      out.push(`<li>${inline((ul || ol)[1])}</li>`);
      continue;
    }

    if (line.trim() === "") {
      flushPara();
      flushList();
      continue;
    }
    para.push(line);
  }
  if (fence !== null) {
    out.push(`<pre class="code"><code>${escapeHTML(fence.join("\n"))}</code></pre>`);
  }
  flushPara();
  flushList();
  return out.join("\n");
}

// ─── Outputs ──────────────────────────────────────────────────────────

// renderOutputs draws a cell's finalised outputs, or the live stream while
// a run is in flight. Streaming text wins for a running cell because the
// finalised copy does not exist yet.
export function renderOutputs(cell, liveText) {
  if (liveText !== undefined && cell.state === "running") {
    return `<pre class="out stream">${escapeHTML(liveText)}<span class="caret"></span></pre>`;
  }
  const outs = cell.outputs || [];
  if (!outs.length) return "";
  return outs.map(renderOutput).join("");
}

function renderOutput(o) {
  switch (o.type) {
    case "error":
      return `<pre class="out err">${escapeHTML(o.text || "")}</pre>`;
    case "diff":
      return renderDiff(o.text || "");
    case "thinking":
      // Reasoning is a summary, not the raw chain of thought — the API
      // never returns that. Collapsed by default: it is context for when an
      // answer surprises you, not the answer.
      return `<details class="out think"><summary>reasoning</summary><pre>${escapeHTML(o.text || "")}</pre></details>`;
    case "tool_call": {
      const input = o.data && o.data.input ? JSON.stringify(o.data.input) : "";
      return `<div class="out tool"><span class="tool-name">→ ${escapeHTML(o.text || "")}</span>` +
             (input ? `<span class="tool-args">${escapeHTML(input)}</span>` : "") + `</div>`;
    }
    case "tool_result": {
      const bad = o.data && o.data.isError;
      return `<pre class="out toolres${bad ? " err" : ""}">${escapeHTML(o.text || "")}</pre>`;
    }
    // thinking / tool_call / tool_result / image / subagent / approval are
    // declared in the schema but not produced until M2+. Render their text
    // rather than dropping them, so a notebook from a newer build is
    // readable here instead of blank.
    default:
      return `<pre class="out">${escapeHTML(o.text || "")}</pre>`;
  }
}

// renderDiff colourises a unified diff. The single highest-value thing a
// notebook can show that a terminal cannot — see ADR 0001 §4.1.
function renderDiff(text) {
  const rows = text.split("\n").map((line) => {
    let cls = "d-ctx";
    if (/^\+\+\+|^---/.test(line)) cls = "d-file";
    else if (line.startsWith("+")) cls = "d-add";
    else if (line.startsWith("-")) cls = "d-del";
    else if (line.startsWith("@@")) cls = "d-hunk";
    return `<span class="${cls}">${escapeHTML(line)}</span>`;
  });
  return `<pre class="out diff">${rows.join("\n")}</pre>`;
}
