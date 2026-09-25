/* ncgo admin console — vanilla fetch + DOM, no framework, no build step. */
"use strict";

(function () {
  const USERS_PAGE = 50;
  let usersOffset = 0;

  function showBanner(node) {
    const el = document.getElementById("banner");
    el.hidden = false;
    el.textContent = "";
    el.appendChild(node);
  }

  function authBanner(status) {
    const frag = document.createDocumentFragment();
    if (status === 401) {
      frag.append("You are not signed in. ");
      const a = document.createElement("a");
      a.href = "/index.php/login";
      a.textContent = "Sign in";
      frag.append(a, " to view the admin console.");
    } else {
      frag.append("The admin console is restricted to members of the admin group (HTTP " + status + ").");
    }
    showBanner(frag);
  }

  async function loadJSON(url) {
    const res = await fetch(url, { headers: { Accept: "application/json" } });
    if (res.status === 401 || res.status === 403) {
      authBanner(res.status);
      throw new Error("auth " + res.status);
    }
    if (!res.ok) throw new Error(url + ": HTTP " + res.status);
    return res.json();
  }

  function cell(row, text) {
    const td = document.createElement("td");
    td.textContent = text == null || text === "" ? "—" : String(text);
    row.appendChild(td);
  }

  function emptyRow(row, cols, msg) {
    const td = document.createElement("td");
    td.colSpan = cols;
    td.className = "empty";
    td.textContent = msg;
    row.appendChild(td);
  }

  function fmtBytes(bytes) {
    if (bytes < 1024) return bytes + " B";
    const units = ["KiB", "MiB", "GiB", "TiB"];
    let v = bytes, u = -1;
    do { v /= 1024; u++; } while (v >= 1024 && u < units.length - 1);
    return v.toFixed(1) + " " + units[u];
  }

  function fmtQuota(bytes) {
    if (bytes == null) return "unlimited";
    return fmtBytes(bytes);
  }

  function fmtSecs(v) {
    if (!v) return "0";
    if (v < 0.001) return (v * 1e6).toFixed(0) + " µs";
    if (v < 1) return (v * 1000).toFixed(1) + " ms";
    return v.toFixed(3) + " s";
  }

  function fmtTime(rfc3339) {
    if (!rfc3339) return "—";
    const d = new Date(rfc3339);
    return isNaN(d) ? rfc3339 : d.toLocaleString();
  }

  function field(dl, k, v) {
    const dt = document.createElement("dt");
    dt.textContent = k;
    const dd = document.createElement("dd");
    dd.textContent = String(v);
    dl.append(dt, dd);
  }

  async function loadStatus() {
    const s = await loadJSON("/console/api/status");
    document.getElementById("instance").textContent = s.instanceID || "";
    const dl = document.getElementById("status-fields");
    dl.textContent = "";
    field(dl, "Version", s.version + (s.versionstring ? " (" + s.versionstring + ")" : ""));
    field(dl, "Product", s.productname + (s.edition ? " " + s.edition : ""));
    field(dl, "Installed", s.installed);
    field(dl, "Maintenance", s.maintenance);
    field(dl, "Needs DB upgrade", s.needsDbUpgrade);
    field(dl, "Database", s.db.dialect + (s.db.reachable ? " (reachable)" : " (UNREACHABLE)"));
    field(dl, "Users", s.users);
    field(dl, "Encryption", s.encryption);
    field(dl, "Metrics", s.metrics);
    field(dl, "Previews", s.previews);
    field(dl, "Plugins", s.plugins);
    field(dl, "Server time (UTC)", s.time);
    const tbody = document.querySelector("#storage-table tbody");
    tbody.textContent = "";
    const backends = s.storage.backends || [];
    if (backends.length === 0) emptyRow(tbody.insertRow(), 3, "no backends configured");
    for (const b of backends) {
      const tr = tbody.insertRow();
      cell(tr, b.name);
      cell(tr, b.type);
      cell(tr, b.name === s.storage.default ? "yes" : "");
    }
  }

  async function loadUsers() {
    const data = await loadJSON("/console/api/users?limit=" + USERS_PAGE + "&offset=" + usersOffset);
    const tbody = document.querySelector("#users-table tbody");
    tbody.textContent = "";
    const list = data.users || [];
    if (list.length === 0) emptyRow(tbody.insertRow(), 5, "no users");
    for (const u of list) {
      const tr = tbody.insertRow();
      if (!u.enabled) tr.className = "disabled";
      cell(tr, u.uid);
      cell(tr, u.displayname);
      cell(tr, u.email);
      cell(tr, u.enabled ? "yes" : "no");
      cell(tr, fmtQuota(u.quota_bytes));
    }
    const from = data.total === 0 ? 0 : usersOffset + 1;
    const to = Math.min(usersOffset + list.length, data.total);
    document.getElementById("users-range").textContent = from + "–" + to + " of " + data.total;
    document.getElementById("users-prev").disabled = usersOffset === 0;
    document.getElementById("users-next").disabled = usersOffset + USERS_PAGE >= data.total;
  }

  async function loadJobs() {
    const data = await loadJSON("/console/api/jobs?limit=50");
    const tbody = document.querySelector("#jobs-table tbody");
    tbody.textContent = "";
    const list = data.jobs || [];
    if (list.length === 0) emptyRow(tbody.insertRow(), 8, "no jobs");
    for (const j of list) {
      const tr = tbody.insertRow();
      tr.className = "state-" + j.state;
      cell(tr, j.id);
      cell(tr, j.name);
      cell(tr, j.state);
      cell(tr, fmtTime(j.run_at));
      cell(tr, fmtTime(j.started_at));
      cell(tr, fmtTime(j.completed_at));
      cell(tr, j.attempts);
      cell(tr, j.last_error);
    }
  }

  async function loadNotifs() {
    const data = await loadJSON("/console/api/notifications?limit=50");
    const tbody = document.querySelector("#notifications-table tbody");
    tbody.textContent = "";
    const list = data.notifications || [];
    if (list.length === 0) emptyRow(tbody.insertRow(), 10, "no notifications");
    for (const n of list) {
      const tr = tbody.insertRow();
      cell(tr, n.id);
      cell(tr, n.user);
      cell(tr, n.app);
      cell(tr, n.object_type);
      cell(tr, n.object_id);
      cell(tr, n.subject);
      cell(tr, n.message);
      cell(tr, n.link);
      cell(tr, n.icon);
      cell(tr, fmtTime(n.created_at));
    }
  }

  async function loadPlugins() {
    const tbody = document.querySelector("#plugins-table tbody");
    tbody.textContent = "";
    // Fetched directly (not via loadJSON): a 503 means metrics are disabled,
    // which is a section-body message, not a banner error.
    const res = await fetch("/console/api/plugins", { headers: { Accept: "application/json" } });
    if (res.status === 401 || res.status === 403) {
      authBanner(res.status);
      throw new Error("auth " + res.status);
    }
    if (res.status === 503) {
      emptyRow(tbody.insertRow(), 11, "metrics disabled");
      return;
    }
    if (!res.ok) throw new Error("/console/api/plugins: HTTP " + res.status);
    const data = await res.json();
    const list = data.plugins || [];
    if (list.length === 0) emptyRow(tbody.insertRow(), 11, "no plugin metrics yet");
    for (const p of list) {
      const tr = tbody.insertRow();
      cell(tr, p.plugin);
      cell(tr, p.host_calls);
      cell(tr, p.host_errors);
      cell(tr, p.entry_calls);
      cell(tr, p.entry_errors);
      cell(tr, fmtSecs(p.host_call_mean_seconds));
      cell(tr, fmtSecs(p.host_call_p95_seconds));
      cell(tr, fmtSecs(p.entry_call_mean_seconds));
      cell(tr, fmtSecs(p.entry_call_p95_seconds));
      cell(tr, fmtBytes(p.storage_bytes));
      cell(tr, p.capability_denials);
    }
  }

  const loaders = { status: loadStatus, users: loadUsers, jobs: loadJobs, notifications: loadNotifs, plugins: loadPlugins };

  function reload(name) {
    loaders[name]().catch((err) => {
      if (String(err.message).startsWith("auth ")) return; // banner already shown
      const span = document.createElement("span");
      span.textContent = "Failed to load " + name + ": " + err.message;
      showBanner(span);
    });
  }

  document.querySelectorAll("[data-reload]").forEach((btn) => {
    btn.addEventListener("click", () => reload(btn.dataset.reload));
  });
  document.getElementById("users-prev").addEventListener("click", () => {
    usersOffset = Math.max(0, usersOffset - USERS_PAGE);
    reload("users");
  });
  document.getElementById("users-next").addEventListener("click", () => {
    usersOffset += USERS_PAGE;
    reload("users");
  });

  reload("status");
  reload("users");
  reload("jobs");
  reload("notifications");
  reload("plugins");
})();
