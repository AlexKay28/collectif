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

  // Approvals are recorded append-only: the question when the agent asks,
  // the verdict when it is answered, paired by id. The reader wants one
  // thing, so the pair is folded here rather than in the log — the log
  // keeps both, because "asked at T1, denied at T2" is the audit trail.
  const verdicts = new Map();
  for (const o of outs) {
    const d = o.data || {};
    if (o.type === "approval" && d.resolution) verdicts.set(d.approvalId, d.resolution);
  }
  // Consecutive injections collapse into one disclosure. A turn can carry
  // thirty of them, and thirty separate lines would be worse than hiding
  // them entirely — the reason they are recorded is that they are findable
  // when something surprising happened, not that they are worth reading.
  // A subagent's turns are tagged with the child that produced them.
  // Gathered into one nested block per child and drawn where the child
  // first appears — a delegating turn otherwise shows an Agent call and a
  // result with nothing between them, and what is missing is usually most
  // of what happened.
  const agents = new Map();
  for (const o of outs) {
    const id = (o.data || {}).agentId;
    if (!id) continue;
    if (!agents.has(id)) agents.set(id, []);
    agents.get(id).push(o);
  }

  const parts = [];
  const drawn = new Set();
  let run = [];
  const flush = () => {
    if (run.length) { parts.push(renderInjections(run)); run = []; }
  };
  for (const o of outs) {
    const agentId = (o.data || {}).agentId;
    if (agentId) {
      flush();
      if (!drawn.has(agentId)) {
        drawn.add(agentId);
        parts.push(renderSubagent(agentId, agents.get(agentId)));
      }
      continue;
    }
    if (o.type === "injection") { run.push(o); continue; }
    flush();
    if (o.type === "approval") {
      const d = o.data || {};
      if (d.resolution) continue; // folded into its question above
      parts.push(renderApproval(o, verdicts.get(d.approvalId)));
      continue;
    }
    parts.push(renderOutput(o));
  }
  flush();
  return parts.join("");
}

// Context the harness put in front of the model that nobody typed: skill
// bodies, hook output, system reminders. Closed, it is one quiet line —
// the turn did not "begin with your prompt" and the document should not
// imply it did. Open, it is the list, with sizes, because the size is
// usually the surprising part.
function renderInjections(outs) {
  const total = outs.reduce((n, o) => n + ((o.data || {}).size || 0), 0);
  const rows = outs.map((o) => {
    const d = o.data || {};
    return `<div class="inj-row">
              <span class="inj-label">${escapeHTML(String(d.label || "context"))}</span>
              <span class="inj-size">${fmtBytes(d.size || 0)}</span>
              <div class="inj-body">${escapeHTML(o.text || "")}</div>
            </div>`;
  }).join("");
  const n = outs.length;
  return `<details class="out inj">
            <summary>${n} context injection${n === 1 ? "" : "s"}
              <span class="inj-total">${fmtBytes(total)} the model read that nobody typed</span>
            </summary>
            ${rows}
          </details>`;
}

// One delegated child: collapsed to a line naming what it was and how
// much it did, expandable to its whole conversation. Collapsed by default
// because the parent's own narrative is the thing being read — the child
// is there for when the answer is surprising.
function renderSubagent(agentId, outs) {
  const kind = (outs.find((o) => (o.data || {}).agentType) || { data: {} }).data.agentType || "subagent";
  const calls = outs.filter((o) => o.type === "tool_call").length;
  const said = outs.filter((o) => o.type === "text").length;
  const bits = [];
  if (calls) bits.push(`${calls} tool call${calls === 1 ? "" : "s"}`);
  if (said) bits.push(`${said} message${said === 1 ? "" : "s"}`);

  return `<details class="out subagent">
            <summary><span class="sa-kind">${escapeHTML(kind)}</span>
              <span class="sa-count">${escapeHTML(bits.join(", ") || "no output yet")}</span>
            </summary>
            <div class="sa-body">${outs.map(renderOutput).join("")}</div>
          </details>`;
}

function fmtBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10240 ? 1 : 0)} KB`;
  return `${(n / 1048576).toFixed(1)} MB`;
}

// An approval is the one block in a document where the reader has to
// decide something, so it is the one block that gets a border. Everything
// else here is a record; this is a question, and until it is answered the
// agent is stopped — including for any prompt you try to send, which will
// otherwise be read as the answer.
function renderApproval(o, verdict) {
  const d = o.data || {};
  const id = escapeHTML(String(d.approvalId || ""));
  const tool = d.tool ? `<span class="tool">${escapeHTML(String(d.tool))}</span>` : "";
  const args = toolArgs(d.input);

  if (verdict) {
    const label = { approved: "approved", denied: "denied", expired: "expired, unanswered" }[verdict] || verdict;
    return `<div class="out approval done ${escapeHTML(verdict)}">
              <span class="mark"></span>
              <div class="q">${tool}${escapeHTML(o.text || "")}
                ${args ? `<span class="args">${escapeHTML(args)}</span>` : ""}</div>
              <span class="verdict">${escapeHTML(label)}</span>
            </div>`;
  }
  return `<div class="out approval open" data-approval="${id}">
            <span class="mark"></span>
            <div class="q">${tool}${escapeHTML(o.text || "")}
              ${args ? `<span class="args">${escapeHTML(args)}</span>` : ""}</div>
            <div class="acts">
              <button data-answer="deny" data-approval-id="${id}">Deny</button>
              <button data-answer="approve" data-approval-id="${id}" class="ok">Approve</button>
            </div>
          </div>`;
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
      const d = o.data || {};
      // The name lives in data for a projected session and in text for a
      // native run; both shapes reach this renderer.
      const name = d.name || o.text || "tool";
      return `<div class="out tool"><span class="tool-name">→ ${escapeHTML(name)}</span>` +
             (toolArgs(d.input) ? `<span class="tool-args">${escapeHTML(toolArgs(d.input))}</span>` : "") +
             `</div>`;
    }
    case "tool_result": {
      const bad = o.data && o.data.isError;
      const text = o.text || "";
      // A tool result is routinely thousands of lines. Inline, it buries
      // the answer that follows it; dropped, the document stops being a
      // record. Collapsed with its first line showing is the compromise
      // a terminal cannot offer at all.
      if (text.length > 600 || text.split("\n").length > 12) {
        const head = text.split("\n")[0].slice(0, 120);
        const lines = text.split("\n").length;
        return `<details class="out toolres long${bad ? " err" : ""}">` +
               `<summary>${escapeHTML(head)}<span class="more"> ${lines} lines</span></summary>` +
               `<pre>${escapeHTML(text)}</pre></details>`;
      }
      return `<pre class="out toolres${bad ? " err" : ""}">${escapeHTML(text)}</pre>`;
    }
    // thinking / tool_call / tool_result / image / subagent / approval are
    // declared in the schema but not produced until M2+. Render their text
    // rather than dropping them, so a notebook from a newer build is
    // readable here instead of blank.
    default:
      return `<pre class="out">${escapeHTML(o.text || "")}</pre>`;
  }
}

// toolArgs picks the one argument worth showing on the call line. A Bash
// call is its command; a Read is its path. Falling back to the whole JSON
// blob is right for tools we do not recognise and wrong for the four that
// account for most calls, which is why the salient keys are named.
function toolArgs(input) {
  if (input == null) return "";
  if (typeof input !== "object") return String(input);
  for (const key of ["command", "file_path", "path", "pattern", "query", "url", "description"]) {
    if (typeof input[key] === "string" && input[key]) return input[key];
  }
  const json = JSON.stringify(input);
  return json === "{}" ? "" : json;
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
