// notify.js — #36 browser notifications for status transitions.
//
// Fires a native Notification when a session enters a status that needs
// attention (waiting_input, review_ready) or has finished (stopped, error,
// paused_over_budget). Rate-limited to one notif per (session, status) pair
// so bouncing statuses don't spam.
//
// Permission model: we do NOT prompt on page load. We prompt lazily on the
// first notable transition, and only if the user hasn't already denied.
// Preference is stored in localStorage so we don't ask again if they said
// yes but the browser's permission got reset.
//
// Quiet mode: a per-agent toggle in localStorage suppresses notifications
// for that one agent. Useful for a demo session you're intentionally
// ignoring.

(function () {
  const NOTABLE = new Set([
    "waiting_input",
    "review_ready",       // #37
    "stopped",
    "error",
    "paused_over_budget", // #35
  ]);

  const LS_ENABLED = "collectif_notify_enabled";     // "yes" | "no" | (unset)
  const LS_QUIET_PREFIX = "collectif_notify_quiet:"; // + agent id

  // Track last-seen status per agent so we only fire on TRANSITIONS,
  // not on repeated snapshots of the same status.
  const lastStatus = new Map();

  function isSupported() {
    return typeof window !== "undefined" && "Notification" in window;
  }

  function isEnabled() {
    return localStorage.getItem(LS_ENABLED) !== "no";
  }

  function isQuiet(id) {
    return localStorage.getItem(LS_QUIET_PREFIX + id) === "yes";
  }

  function setQuiet(id, quiet) {
    if (quiet) localStorage.setItem(LS_QUIET_PREFIX + id, "yes");
    else       localStorage.removeItem(LS_QUIET_PREFIX + id);
  }

  async function ensurePermission() {
    if (!isSupported()) return false;
    if (Notification.permission === "granted") return true;
    if (Notification.permission === "denied") return false;
    // "default" — we can ask.
    try {
      const p = await Notification.requestPermission();
      if (p === "denied") localStorage.setItem(LS_ENABLED, "no");
      return p === "granted";
    } catch (_) {
      return false;
    }
  }

  function humanStatus(s) {
    return String(s || "").replace(/_/g, " ");
  }

  // Emits a notification for an agent snapshot if the status transition is
  // notable and the user hasn't opted out.
  async function maybeNotify(agent) {
    if (!agent || !agent.id) return;
    if (!isSupported() || !isEnabled()) return;
    if (isQuiet(agent.id)) return;

    const prev = lastStatus.get(agent.id);
    const cur = agent.status || "";
    if (prev === cur) return;
    lastStatus.set(agent.id, cur);
    if (!NOTABLE.has(cur)) return;

    if (!(await ensurePermission())) return;

    const name = (typeof agentName === "function") ? agentName(agent) : (agent.id.slice(0, 8));
    const cwdBaseFn = typeof cwdBase === "function" ? cwdBase : (p => p || "");
    const cwd = cwdBaseFn(agent.cwd);
    const task = agent.currentTask || agent.prompt || "";
    const body = [humanStatus(cur), cwd, task && "▸ " + task].filter(Boolean).join(" · ");

    let title;
    switch (cur) {
      case "waiting_input":      title = "⏸  " + name + " needs input"; break;
      case "review_ready":       title = "✅ " + name + " — PR ready for review"; break;
      case "stopped":            title = "○ " + name + " finished"; break;
      case "error":              title = "✗ " + name + " errored"; break;
      case "paused_over_budget": title = "💰 " + name + " paused (over budget)"; break;
      default:                   title = name + ": " + humanStatus(cur);
    }

    let n;
    try {
      n = new Notification(title, {
        body,
        tag: "collectif-" + agent.id,     // dedupe: replaces prior notif for same agent
        renotify: true,
        icon: (typeof avatarURL === "function") ? avatarURL(agent.id) : undefined,
      });
    } catch (_) {
      return;
    }
    n.onclick = () => {
      window.focus();
      // review_ready → open PR if we have one; otherwise focus the agent tile.
      if (cur === "review_ready" && agent.prURL) {
        window.open(agent.prURL, "_blank");
      } else if (typeof selectAgent === "function") {
        selectAgent(agent.id);
      }
      n.close();
    };
  }

  // Seed lastStatus without firing — used on initial snapshot so that
  // agents already in review_ready when we connect don't ping us.
  function seed(agent) {
    if (!agent || !agent.id) return;
    lastStatus.set(agent.id, agent.status || "");
  }

  // Global handle for other modules to call after upsert events.
  window.collectifNotify = {
    maybeNotify,
    seed,
    isEnabled,
    setEnabled(yes) { localStorage.setItem(LS_ENABLED, yes ? "yes" : "no"); },
    isQuiet,
    setQuiet,
    isSupported,
    permission() { return isSupported() ? Notification.permission : "unsupported"; },
  };
})();
