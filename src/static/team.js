// ─── Team designer: subagent CRUD + inline tree panel ──
// Right-column panel showing subagent configs from .claude/agents/*.md as a
// compact indented tree — root row = the selected session, subagents laid
// out below with left padding proportional to depth. Fits any width; no
// horizontal scroll. Edges come from each file's `subagents:` frontmatter.
let teamSubagents = [];   // last-loaded array of SubagentFile
let teamEditingName = ""; // "" when creating, else the name being edited
let teamCurrentSelection = null; // last agent id we loaded team for

async function refreshTeamForCurrent() {
  const a = selectedId ? agents.get(selectedId) : null;
  if (!a) return;
  document.getElementById("team-cwd").textContent = a.cwd + "/.claude/agents/";
  await refreshTeam();
}
function updateTeamVisibility() {
  const a = selectedId ? agents.get(selectedId) : null;
  const team = document.getElementById("dash-team-panel");
  const metrics = document.getElementById("dash-right-metrics");
  const main = document.querySelector(".dash-main");
  if (!team || !metrics) return;
  if (main) main.classList.toggle("no-agent", !a);
  if (a) {
    team.style.display = "flex";
    metrics.style.display = "none";
    if (teamCurrentSelection !== a.id) {
      teamCurrentSelection = a.id;
      refreshTeamForCurrent();
    }
    startTeamPolling();
  } else {
    team.style.display = "none";
    metrics.style.display = "";
    teamCurrentSelection = null;
    teamSubagents = [];
    stopTeamPolling();
  }
}

// Poll the team endpoint every 4s so external changes (e.g. `/agents` in
// the parent claude, or an editor writing to .claude/agents/) show up
// without the user hitting Refresh. Only runs while the panel is visible.
let teamPollTimer = null;
function startTeamPolling() {
  if (teamPollTimer) return;
  teamPollTimer = setInterval(() => {
    if (!selectedId) { stopTeamPolling(); return; }
    // Skip the poll while an edit modal is open — otherwise a mid-edit
    // refresh could re-render the form under the user.
    if (document.getElementById("sa-modal").classList.contains("show")) return;
    refreshTeam();
  }, 4000);
}
function stopTeamPolling() {
  if (teamPollTimer) { clearInterval(teamPollTimer); teamPollTimer = null; }
}
async function refreshTeam() {
  if (!selectedId) return;
  try {
    const res = await fetch("/api/agents/" + selectedId + "/subagents");
    if (!res.ok) { toast.error("Load failed: " + (await res.text())); return; }
    teamSubagents = await res.json();
  } catch (err) { toast.error("Load failed: " + err.message); return; }
  renderTeamCanvas();
}
function renderTeamCanvas() {
  const canvas = document.getElementById("team-canvas");
  const empty = document.getElementById("team-empty");
  const a = selectedId ? agents.get(selectedId) : null;
  if (!a) return;

  if (teamSubagents.length === 0) {
    canvas.style.display = "none";
    empty.style.display = "flex";
    return;
  }
  canvas.style.display = "block";
  empty.style.display = "none";

  // Build parent → children map.
  const byName = new Map(teamSubagents.map(sf => [sf.name, sf]));
  const childrenOf = new Map();
  const hasParent = new Set();
  for (const sf of teamSubagents) {
    for (const child of (sf.subagents || [])) {
      if (!byName.has(child)) continue;
      if (!childrenOf.has(sf.name)) childrenOf.set(sf.name, []);
      childrenOf.get(sf.name).push(child);
      hasParent.add(child);
    }
  }
  const roots = teamSubagents.filter(sf => !hasParent.has(sf.name)).map(sf => sf.name);
  if (roots.length === 0 && teamSubagents.length > 0) roots.push(teamSubagents[0].name);

  // Flatten to an ordered list with depth + last-child flag for tree lines.
  const rows = [];
  const visited = new Set();
  function walk(name, depth, isLast) {
    if (visited.has(name)) return;
    visited.add(name);
    const sf = byName.get(name);
    if (!sf) return;
    rows.push({ sf, depth, isLast });
    const kids = (childrenOf.get(name) || []).filter(k => !visited.has(k));
    kids.forEach((k, i) => walk(k, depth + 1, i === kids.length - 1));
  }
  roots.forEach(r => walk(r, 1, roots.indexOf(r) === roots.length - 1));
  for (const sf of teamSubagents) {
    if (!visited.has(sf.name)) walk(sf.name, 1, false);
  }

  const rootHTML = (
    '<div class="team-root">' +
      '<div class="lab">Current session</div>' +
      '<div class="head">' +
        '<div class="avatar"><img src="' + avatarURL(a.id) + '" alt=""></div>' +
        '<div class="name">' + esc(agentName(a)) + '</div>' +
      '</div>' +
    '</div>'
  );
  const treeHTML = '<div class="team-tree">' + rows.map(({ sf, depth, isLast }) => {
    const modelBadge = sf.model ? '<span class="badge model">' + esc(sf.model) + '</span>' : "";
    const scopeBadge = sf.scope ? '<span class="badge scope ' + esc(sf.scope) + '" title="' + (sf.scope === "user" ? "~/.claude/agents/ (shared across all your projects)" : "project-scoped .claude/agents/") + '">' + esc(sf.scope) + '</span>' : "";
    const indent = depth * 14 + 8;
    const hasDesc = !!(sf.description || "").trim();
    const chevron = hasDesc
      ? '<button class="tree-chevron" data-act="expand" title="Show description">▸</button>'
      : '<span class="tree-chevron" style="visibility:hidden">▸</span>';
    return (
      '<div class="tree-row' + (isLast ? " last" : "") + '" data-name="' + esc(sf.name) + '" data-scope="' + esc(sf.scope || "project") + '" style="padding-left:' + indent + 'px;--d:' + depth + '">' +
        chevron +
        '<div class="tree-info"><span class="name">' + esc(sf.name) + '</span></div>' +
        '<div class="tree-badges">' + modelBadge + scopeBadge + '</div>' +
        (hasDesc ? '<div class="tree-desc">' + esc(sf.description) + '</div>' : '') +
      '</div>'
    );
  }).join("") + '</div>';

  canvas.innerHTML = rootHTML + treeHTML;
  canvas.querySelectorAll(".tree-row[data-name]").forEach(r => {
    r.onclick = (ev) => {
      if (ev.target.closest("[data-act='expand']")) {
        ev.stopPropagation();
        r.classList.toggle("expanded");
        return;
      }
      openSubagentModal(r.dataset.name, r.dataset.scope);
    };
  });
}

let teamEditingScope = "project";
function openSubagentModal(name, scope) {
  teamEditingName = name || "";
  teamEditingScope = scope || "project";
  const sf = name ? teamSubagents.find(x => x.name === name) : null;
  if (sf && sf.scope) teamEditingScope = sf.scope;
  document.getElementById("sa-title").textContent = name ? "Edit " + name : "New subagent";
  document.getElementById("sa-name").value = sf ? sf.name : "";
  document.getElementById("sa-name").disabled = !!name;
  document.getElementById("sa-desc").value = sf ? (sf.description || "") : "";
  document.getElementById("sa-model").value = sf ? (sf.model || "") : "";
  const scopeSel = document.getElementById("sa-scope");
  if (scopeSel) { scopeSel.value = teamEditingScope; scopeSel.disabled = !!name; }
  document.getElementById("sa-tools").value = sf ? (sf.tools || []).join(", ") : "";
  document.getElementById("sa-prompt").value = sf ? (sf.prompt || "") : "";
  // Delegation checkboxes: every other subagent except self.
  const delegates = document.getElementById("sa-delegates");
  const others = teamSubagents.filter(x => x.name !== (sf ? sf.name : ""));
  if (others.length === 0) {
    delegates.innerHTML = '<div class="empty">Add more subagents to draw delegation edges.</div>';
  } else {
    const chosen = new Set(sf ? (sf.subagents || []) : []);
    delegates.innerHTML = others.map(o => (
      '<label><input type="checkbox" value="' + esc(o.name) + '"' + (chosen.has(o.name) ? " checked" : "") + '>' + esc(o.name) + '</label>'
    )).join('');
  }
  document.getElementById("sa-delete").style.display = name ? "" : "none";
  document.getElementById("sa-modal").classList.add("show");
  if (!name) document.getElementById("sa-name").focus();
}
function closeSubagentModal() {
  document.getElementById("sa-modal").classList.remove("show");
  teamEditingName = "";
}
async function saveSubagent() {
  const name = document.getElementById("sa-name").value.trim();
  if (!/^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$/.test(name)) {
    toast.error("Name must match a–z 0–9 - (max 64 chars)");
    return;
  }
  // For new subagents, honour the scope dropdown; for edits, teamEditingScope
  // was captured from the clicked node so we overwrite the correct file.
  const scopeSel = document.getElementById("sa-scope");
  if (!teamEditingName && scopeSel) teamEditingScope = scopeSel.value || "project";
  const body = {
    name,
    description: document.getElementById("sa-desc").value.trim(),
    model: document.getElementById("sa-model").value.trim(),
    tools: document.getElementById("sa-tools").value.split(",").map(s => s.trim()).filter(Boolean),
    subagents: Array.from(document.querySelectorAll("#sa-delegates input:checked")).map(cb => cb.value),
    prompt: document.getElementById("sa-prompt").value,
  };
  try {
    const scope = teamEditingScope || "project";
    const res = await fetch("/api/agents/" + selectedId + "/subagents/" + encodeURIComponent(name) + "?scope=" + encodeURIComponent(scope), {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) { toast.error("Save failed: " + (await res.text())); return; }
    toast.success("Saved " + name + " (" + scope + ")");
  } catch (err) { toast.error("Save failed: " + err.message); return; }
  closeSubagentModal();
  refreshTeam();
}
async function deleteSubagent() {
  if (!teamEditingName) return;
  if (!confirm("Delete subagent " + teamEditingName + " (" + teamEditingScope + ")?")) return;
  const res = await fetch("/api/agents/" + selectedId + "/subagents/" + encodeURIComponent(teamEditingName) + "?scope=" + encodeURIComponent(teamEditingScope), { method: "DELETE" });
  if (!res.ok) { toast.error("Delete failed: " + (await res.text())); return; }
  toast.success("Deleted " + teamEditingName);
  closeSubagentModal();
  refreshTeam();
}

// Wire up the team-panel buttons at boot. Guarded via bootTeam() so the
// auth screen (where these DOM nodes don't exist) can skip the wiring.
function bootTeam() {
  document.getElementById("team-refresh").onclick = refreshTeamForCurrent;
  document.getElementById("team-new").onclick = () => openSubagentModal(null);
  document.getElementById("team-empty-new").onclick = () => openSubagentModal(null);
  document.getElementById("sa-cancel").onclick = closeSubagentModal;
  document.getElementById("sa-save").onclick = saveSubagent;
  document.getElementById("sa-delete").onclick = deleteSubagent;
}
