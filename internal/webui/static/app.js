"use strict";

// ---- helpers ----------------------------------------------------------

const $ = (id) => document.getElementById(id);

// h builds DOM nodes. Text is always set via text nodes, never as HTML, so
// paths, names and server messages can't inject markup.
function h(tag, props, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

async function api(method, path, body) {
  const opts = { method, headers: {}, credentials: "same-origin" };
  if (method !== "GET") {
    opts.headers["X-Courier-Request"] = "1";
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body || {});
  }
  let res;
  try {
    res = await fetch(path, opts);
  } catch {
    throw new Error("immich-courier isn't responding. Is it still running?");
  }
  if (res.status === 401) {
    throw new Error("This settings page has expired. Open Settings again from the immich-courier tray icon.");
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `Request failed (${res.status})`);
  return data;
}

let toastTimer;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => (t.hidden = true), 4000);
}

function showError(el, err) {
  el.textContent = err ? err.message || String(err) : "";
  el.hidden = !err;
}

function busy(btn, on, label) {
  if (on) {
    btn.dataset.label = btn.textContent;
    btn.textContent = label || "Working…";
    btn.disabled = true;
  } else {
    btn.textContent = btn.dataset.label || btn.textContent;
    btn.disabled = false;
  }
}

function host(url) {
  try { return new URL(url).host; } catch { return url; }
}

function ago(iso) {
  if (!iso) return "";
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return new Date(iso).toLocaleDateString();
}

function basename(p) {
  const parts = p.split(/[\\/]/).filter(Boolean);
  return parts.length ? parts[parts.length - 1] : p;
}

// ---- state & rendering -----------------------------------------------

let state = null;

async function refresh() {
  try {
    state = await api("GET", "/api/state");
    render();
  } catch (e) {
    $("status-pill").textContent = "Not connected";
    $("status-pill").className = "pill warn";
    $("banner").textContent = e.message;
    $("banner").hidden = false;
  }
}

function accountState(a) {
  if (!a.hasKey) return ["warn", "Not logged in"];
  const s = a.status;
  if (!a.inUse || !s) return ["muted", "Not used by any folder yet"];
  if (s.authFailed) return ["warn", "Login expired, log in again"];
  if (state.paused) return ["muted", "Paused"];
  if (!s.online) return ["warn", "Server unreachable, files wait until it's back"];
  if (s.uploading) return ["busy", `Uploading ${basename(s.uploading)}`];
  if (s.queued > 0) return ["busy", `${s.queued} waiting`];
  return ["ok", "Up to date"];
}

function overall() {
  if (!state.accounts.length) return ["warn", "Not set up"];
  if (state.problem && state.folders.length) return ["warn", "Needs attention"];
  if (!state.folders.length) return ["warn", "No folders yet"];
  const live = state.accounts.filter((a) => a.status);
  if (live.some((a) => a.status.authFailed || !a.hasKey)) return ["warn", "Needs attention"];
  if (state.paused) return ["", "Paused"];
  const waiting = live.reduce((n, a) => n + a.status.queued, 0);
  if (live.length && live.every((a) => !a.status.online)) return ["warn", "Server unreachable"];
  if (live.some((a) => a.status.uploading)) return ["busy", "Uploading"];
  if (waiting) return ["busy", `${waiting} waiting`];
  return ["ok", "Up to date"];
}

function render() {
  const [cls, text] = overall();
  const pill = $("status-pill");
  pill.textContent = text;
  pill.className = "pill " + cls;

  const pause = $("btn-pause");
  pause.hidden = !state.folders.length;
  pause.textContent = state.paused ? "Resume uploads" : "Pause uploads";
  $("btn-rescan").hidden = !state.folders.length;

  const banner = $("banner");
  const needsLogin = state.accounts.filter((a) => !a.hasKey || (a.status && a.status.authFailed));
  if (needsLogin.length) {
    banner.textContent = `Log in again for: ${needsLogin.map((a) => a.name).join(", ")}. Until then, files for ${needsLogin.length > 1 ? "these accounts" : "this account"} wait in the queue.`;
    banner.hidden = false;
  } else {
    banner.hidden = true;
  }

  renderAccounts();
  renderFolders();
  $("footer").textContent = `immich-courier ${state.version} · config: ${state.config}`;
}

function renderAccounts() {
  const box = $("accounts");
  box.replaceChildren();
  if (!state.accounts.length) {
    box.append(
      h("div", { class: "empty" },
        h("strong", {}, "Connect your Immich server"),
        "Log in once; the app then uploads in the background.",
        h("div", {}, h("button", { class: "btn primary", type: "button", onclick: () => openAccountDialog() }, "Add account")))
    );
    return;
  }
  for (const a of state.accounts) {
    const [cls, text] = accountState(a);
    const s = a.status;
    const stats = [];
    stats.push(`${a.folders} folder${a.folders === 1 ? "" : "s"}`);
    if (s && s.uploadedThisSession) stats.push(`${s.uploadedThisSession} uploaded since start`);
    if (s && s.lastUpload) stats.push(`last ${ago(s.lastUpload)}`);
    const needLogin = !a.hasKey || (s && s.authFailed);
    box.append(
      h("article", { class: "card" },
        h("div", { class: "card-head" },
          h("span", { class: "card-name" }, a.name),
          h("span", { class: "state " + cls }, text)),
        h("div", { class: "card-server" }, host(a.server)),
        s && s.lastError && !s.online ? h("div", { class: "stats", title: s.lastError }, "Last error: " + s.lastError) : null,
        h("div", { class: "stats" }, stats.join(" · ")),
        h("div", { class: "card-actions" },
          h("button", {
            class: needLogin ? "btn primary small" : "btn ghost small", type: "button",
            onclick: () => openAccountDialog(a),
          }, needLogin ? "Log in again" : "Change login"),
          h("button", {
            class: "btn ghost small danger", type: "button",
            disabled: a.inUse,
            title: a.inUse ? "Remove its folders first" : null,
            onclick: () => removeAccount(a),
          }, "Remove")))
    );
  }
}

function renderFolders() {
  const box = $("folders");
  box.replaceChildren();
  $("btn-add-folder").disabled = !state.accounts.length;
  if (!state.folders.length) {
    box.append(
      h("div", { class: "empty" },
        h("strong", {}, state.accounts.length ? "Pick a folder to watch" : "Add an account first"),
        state.accounts.length ? "New photos and videos in it will be uploaded automatically." : "Then choose which folders upload to it.",
        state.accounts.length ? h("div", {}, h("button", { class: "btn primary", type: "button", onclick: openFolderDialog }, "Add folder")) : null)
    );
    return;
  }
  for (const f of state.folders) {
    box.append(
      h("div", { class: "row" },
        h("div", { class: "row-main" },
          h("div", { class: "row-path mono" }, f.path),
          h("div", { class: "row-sub" }, "Uploads to ", h("strong", {}, f.account))),
        h("button", { class: "btn ghost small danger", type: "button", onclick: () => removeFolder(f) }, "Stop watching"))
    );
  }
}

// ---- actions ------------------------------------------------------------

async function removeAccount(a) {
  if (!confirm(`Remove the account "${a.name}" from this app?\n\nNothing is deleted in Immich. The app's API key stays listed under Account settings → API keys in Immich; you can delete it there.`)) return;
  try {
    await api("POST", "/api/accounts/remove", { name: a.name });
    toast(`Removed ${a.name}`);
    refresh();
  } catch (e) { toast(e.message); }
}

async function removeFolder(f) {
  if (!confirm(`Stop watching ${f.path}?\n\nFiles already uploaded stay in Immich.`)) return;
  try {
    await api("POST", "/api/folders/remove", { path: f.path });
    toast("Stopped watching " + basename(f.path));
    refresh();
  } catch (e) { toast(e.message); }
}

$("btn-pause").addEventListener("click", async () => {
  try {
    await api("POST", "/api/pause", { paused: !state.paused });
    refresh();
  } catch (e) { toast(e.message); }
});
$("btn-rescan").addEventListener("click", async () => {
  try {
    await api("POST", "/api/rescan");
    toast("Checking folders…");
    setTimeout(refresh, 1500);
  } catch (e) { toast(e.message); }
});

// ---- account dialog -----------------------------------------------------

let probed = null; // ServerInfo for the current dialog

function openAccountDialog(existing) {
  probed = null;
  $("form-account").reset();
  $("acct-title").textContent = existing ? `Log in again: ${existing.name}` : "Add an account";
  $("in-server").value = existing ? existing.server : "";
  $("in-server").disabled = false;
  $("in-name").value = existing ? existing.name : (state && state.accounts.length ? "" : "personal");
  $("in-name").readOnly = !!existing;
  $("step-server").hidden = false;
  $("step-login").hidden = true;
  $("btn-continue").hidden = false;
  showError($("acct-error"), null);
  $("dlg-account").showModal();
  if (existing) probeServer();
  else $("in-server").focus();
}

async function probeServer() {
  const btn = $("btn-continue");
  showError($("acct-error"), null);
  busy(btn, true, "Checking…");
  try {
    probed = await api("POST", "/api/probe", { server: $("in-server").value });
    $("in-server").value = probed.server;
    $("server-info").textContent = `Immich ${probed.version || ""} at ${host(probed.server)}`;
    $("method-oidc").hidden = !probed.oauth;
    $("method-password").hidden = !probed.passwordLogin;
    $("method-password").open = !probed.oauth && probed.passwordLogin;
    $("method-apikey").open = !probed.oauth && !probed.passwordLogin;
    $("apikey-link").href = probed.server + "/user-settings?isOpen=api-keys";
    $("step-server").hidden = true;
    $("step-login").hidden = false;
    btn.hidden = true;
    if (!$("in-name").value) $("in-name").value = host(probed.server).split(".")[0].replace(/[^A-Za-z0-9 _.-]/g, "") || "personal";
    ($("in-name").readOnly ? (probed.oauth ? $("btn-oidc") : $("in-email")) : $("in-name")).focus();
  } catch (e) {
    showError($("acct-error"), e);
  } finally {
    busy(btn, false);
  }
}

$("btn-add-account").addEventListener("click", () => openAccountDialog());
$("btn-continue").addEventListener("click", probeServer);
$("in-server").addEventListener("keydown", (e) => {
  if (e.key === "Enter") { e.preventDefault(); probeServer(); }
});

function loginBody(extra) {
  return Object.assign({ name: $("in-name").value.trim(), server: probed.server }, extra);
}

async function loggedIn(res) {
  $("dlg-account").close();
  toast(`Logged in as ${res.name || res.email}. ${state && state.folders.length ? "" : "Now pick a folder to watch."}`);
  await refresh();
  if (!state.folders.length) openFolderDialog();
}

$("btn-oidc").addEventListener("click", async (e) => {
  showError($("acct-error"), null);
  busy(e.target, true, "Opening login…");
  try {
    const res = await api("POST", "/api/login/oidc", loginBody());
    sessionStorage.setItem("ic-oidc", "1");
    window.location.href = res.url; // comes back to /#login
  } catch (err) {
    showError($("acct-error"), err);
    busy(e.target, false);
  }
});

$("btn-password").addEventListener("click", async (e) => {
  showError($("acct-error"), null);
  busy(e.target, true, "Logging in…");
  try {
    const res = await api("POST", "/api/login/password", loginBody({ email: $("in-email").value, password: $("in-password").value }));
    $("in-password").value = "";
    loggedIn(res);
  } catch (err) {
    showError($("acct-error"), err);
  } finally {
    busy(e.target, false);
  }
});

$("btn-apikey").addEventListener("click", async (e) => {
  showError($("acct-error"), null);
  busy(e.target, true, "Checking key…");
  try {
    const res = await api("POST", "/api/login/apikey", loginBody({ apiKey: $("in-apikey").value }));
    $("in-apikey").value = "";
    loggedIn(res);
  } catch (err) {
    showError($("acct-error"), err);
  } finally {
    busy(e.target, false);
  }
});

// Back from the identity provider: wait for the app to finish the login.
async function finishOIDC() {
  if (location.hash !== "#login" && !sessionStorage.getItem("ic-oidc")) return;
  history.replaceState(null, "", "/");
  sessionStorage.removeItem("ic-oidc");
  toast("Finishing login…");
  for (let i = 0; i < 60; i++) {
    const r = await api("GET", "/api/login/oidc").catch(() => ({ state: "error", error: "lost connection" }));
    if (r.state === "done") { await loggedIn(r); return; }
    if (r.state === "error") {
      toast("Login failed: " + r.error);
      return;
    }
    if (r.state === "none") return;
    await new Promise((res) => setTimeout(res, 1000));
  }
  toast("Login is taking too long; try again.");
}

// ---- folder dialog ------------------------------------------------------

let cwd = "";

async function browse(path) {
  showError($("folder-error"), null);
  let d;
  try {
    d = await api("GET", "/api/dirs?path=" + encodeURIComponent(path || ""));
  } catch (e) {
    showError($("folder-error"), e);
    return;
  }
  cwd = d.path;
  $("cur-path").textContent = d.path;
  $("btn-up").disabled = !d.parent;
  $("btn-up").onclick = () => browse(d.parent);
  const roots = $("roots");
  roots.replaceChildren(...(d.roots || []).map((r) =>
    h("button", { class: "btn ghost small", type: "button", onclick: () => browse(r) }, r.replace(/\\$/, ""))));
  const list = $("dir-list");
  list.replaceChildren();
  if (d.error) list.append(h("li", { class: "note" }, d.error));
  else if (!d.dirs.length) list.append(h("li", { class: "note" }, "No subfolders. You can watch this folder or create one below."));
  for (const name of d.dirs) {
    const sep = d.path.endsWith("/") || d.path.endsWith("\\") ? "" : (d.path.includes("\\") ? "\\" : "/");
    const li = h("li", { tabindex: "0", role: "option" }, name);
    const go = () => browse(d.path + sep + name);
    li.addEventListener("click", go);
    li.addEventListener("keydown", (e) => { if (e.key === "Enter") go(); });
    list.append(li);
  }
  list.scrollTop = 0;
}

function openFolderDialog() {
  const sel = $("sel-account");
  sel.replaceChildren(...state.accounts.map((a) => h("option", { value: a.name }, `${a.name} (${host(a.server)})`)));
  showError($("folder-error"), null);
  $("in-newdir").value = "";
  $("dlg-folder").showModal();
  browse(cwd || state.home);
}

$("btn-add-folder").addEventListener("click", openFolderDialog);

$("btn-mkdir").addEventListener("click", async () => {
  const name = $("in-newdir").value.trim();
  if (!name) return;
  try {
    const r = await api("POST", "/api/dirs/create", { parent: cwd, name });
    $("in-newdir").value = "";
    browse(r.path);
  } catch (e) { showError($("folder-error"), e); }
});

$("btn-watch").addEventListener("click", async (e) => {
  showError($("folder-error"), null);
  busy(e.target, true, "Adding…");
  try {
    await api("POST", "/api/folders/add", { path: cwd, account: $("sel-account").value });
    $("dlg-folder").close();
    toast("Watching " + basename(cwd));
    refresh();
  } catch (err) {
    showError($("folder-error"), err);
  } finally {
    busy(e.target, false);
  }
});

// ---- start ----------------------------------------------------------------

refresh().then(finishOIDC);
setInterval(() => { if (document.visibilityState === "visible") refresh(); }, 3000);
document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") refresh(); });
