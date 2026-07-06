// Bootstrap: pull the token from the URL (once), stash it in sessionStorage,
// wrap fetch to attach the Authorization header, and expose wsURL() for the
// WebSocket connections. Loaded BEFORE app.js so the fetch wrapper is already
// in place when the app makes its first request.
(function () {
  const url = new URL(location.href);
  const fromURL = url.searchParams.get("token");
  if (fromURL) {
    sessionStorage.setItem("collectif.token", fromURL);
    url.searchParams.delete("token");
    history.replaceState(null, "", url.pathname + (url.search ? url.search : "") + url.hash);
  }
  const tok = sessionStorage.getItem("collectif.token");
  if (!tok) {
    document.body.innerHTML =
      '<div style="max-width:360px;margin:15vh auto;padding:24px;background:#161b22;border:1px solid #30363d;border-radius:8px;color:#e6edf3;font:14px/1.5 -apple-system,sans-serif">' +
        '<h2 style="margin:0 0 12px;font-size:16px">collectif auth</h2>' +
        '<div style="color:#7d8590;font-size:12.5px;margin-bottom:12px">Paste the auth token printed on the server console.</div>' +
        '<input id="auth-tok" type="text" style="width:100%;padding:8px 10px;font:13px ui-monospace,monospace;background:#0d1117;border:1px solid #30363d;border-radius:6px;color:#e6edf3" placeholder="token" autofocus />' +
        '<button id="auth-save" style="margin-top:12px;width:100%;padding:8px 12px;background:#238636;border:1px solid #2ea043;color:#fff;border-radius:6px;font:600 13px -apple-system,sans-serif;cursor:pointer">Save &amp; reload</button>' +
      '</div>';
    const save = () => {
      const v = document.getElementById("auth-tok").value.trim();
      if (!v) return;
      sessionStorage.setItem("collectif.token", v);
      location.reload();
    };
    document.getElementById("auth-save").onclick = save;
    document.getElementById("auth-tok").addEventListener("keydown", (e) => { if (e.key === "Enter") save(); });
    window.AGENTCTL_NO_TOKEN = true;
    return;
  }
  window.AGENTCTL_TOKEN = tok;
  const origFetch = window.fetch.bind(window);
  window.fetch = function (input, init) {
    init = init || {};
    const headers = new Headers(init.headers || (typeof input !== "string" && input.headers) || {});
    if (!headers.has("Authorization")) headers.set("Authorization", "Bearer " + tok);
    init.headers = headers;
    return origFetch(input, init).then((res) => {
      if (res.status === 401) {
        sessionStorage.removeItem("collectif.token");
        location.reload();
      }
      return res;
    });
  };
  window.wsURL = function (path) {
    const proto = location.protocol === "https:" ? "wss://" : "ws://";
    return proto + location.host + path + (path.indexOf("?") >= 0 ? "&" : "?") + "token=" + encodeURIComponent(tok);
  };
})();
