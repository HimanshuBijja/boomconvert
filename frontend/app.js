const $ = (id) => document.getElementById(id);

function fmtBytes(n) {
  if (!n && n !== 0) return "—";
  const u = ["B","KB","MB","GB","TB"];
  let i = 0, v = Math.abs(n);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (Math.sign(n) < 0 ? "-" : "") + v.toFixed(v >= 100 || i === 0 ? 0 : 1) + " " + u[i];
}

async function jget(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
async function jpost(url, body) {
  const r = await fetch(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: body ? JSON.stringify(body) : null });
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}
async function jdel(url) {
  const r = await fetch(url, { method: "DELETE" });
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

async function refreshStatus() {
  const s = await jget("/api/status");
  const pill = $("status-pill");
  const btn = $("toggle-btn");
  if (s.paused) {
    pill.textContent = "Paused";
    pill.className = "pill pill-off";
    btn.textContent = "Resume";
  } else {
    pill.textContent = "Active";
    pill.className = "pill pill-on";
    btn.textContent = "Pause";
  }
}

async function refreshTools() {
  const tools = await jget("/api/tools");
  const grid = $("tools-grid");
  grid.innerHTML = "";
  tools.sort((a, b) => a.display_name.localeCompare(b.display_name));
  for (const t of tools) {
    const el = document.createElement("div");
    el.className = "tool";
    el.innerHTML = `
      <div class="name">
        <span>${t.display_name}</span>
        <span class="dot ${t.found ? "dot-on" : "dot-off"}"></span>
      </div>
      ${t.found
        ? `<div class="meta">${t.version || ""}</div><div class="meta">${t.path || ""}</div>`
        : `<div class="meta">Not installed</div><div class="hint">${t.install_hint || ""}</div>`}
    `;
    grid.appendChild(el);
  }
}

async function refreshFolders() {
  const folders = await jget("/api/folders");
  const list = $("folders-list");
  list.innerHTML = "";
  for (const f of folders) {
    const li = document.createElement("li");
    li.className = "folder";
    li.innerHTML = `
      <div>
        <div class="path">${f.path}</div>
        <div class="muted">${f.enabled ? "Watching" : "Disabled"}</div>
      </div>
      <div class="ctrls">
        <button class="btn-ghost" data-toggle="${f.id}">${f.enabled ? "Disable" : "Enable"}</button>
        <button class="btn-ghost btn-danger" data-remove="${f.id}">Remove</button>
      </div>`;
    list.appendChild(li);
  }
}

async function refreshMatrix() {
  const rules = await jget("/api/matrix");
  const grid = $("matrix-grid");
  grid.innerHTML = "";
  if (!rules || rules.length === 0) {
    grid.innerHTML = `<div class="muted">No conversion rules active — install tools above.</div>`;
    return;
  }
  for (const r of rules) {
    const el = document.createElement("div");
    el.className = "matrix-cell";
    el.innerHTML = `<span>${r.source}</span><span class="arrow">→</span><span>${r.target}</span>`;
    grid.appendChild(el);
  }
}

async function refreshAnalytics() {
  const a = await jget("/api/analytics");
  $("ana-total").textContent = a.total_conversions;
  $("ana-saved").textContent = fmtBytes(a.bytes_saved);
}

async function refreshHistory() {
  const records = await jget("/api/history?limit=50");
  const list = $("activity-list");
  list.innerHTML = "";
  for (const r of records) {
    list.appendChild(activityRow(r));
  }
}

function activityRow(r) {
  const li = document.createElement("li");
  const status = (r.status || "").toLowerCase();
  li.className = "activity " + status;
  const delta = (r.source_size || 0) - (r.target_size || 0);
  li.innerHTML = `
    <span class="badge">${r.source_format} → ${r.target_format}</span>
    <div>
      <div class="name">${r.target_path || r.source_path}</div>
      ${r.error ? `<div class="muted">${r.error}</div>` : ""}
    </div>
    <div class="meta">
      <div>${status}</div>
      <div>${fmtBytes(r.source_size)} → ${fmtBytes(r.target_size)} (${delta > 0 ? "−" : ""}${fmtBytes(Math.abs(delta))})</div>
    </div>`;
  return li;
}

function connectWS() {
  const ws = new WebSocket(`ws://${location.host}/ws`);
  ws.onmessage = (msg) => {
    let evt; try { evt = JSON.parse(msg.data); } catch { return; }
    switch (evt.type) {
      case "conversion_started":
      case "conversion_completed":
      case "conversion_failed":
        refreshHistory();
        refreshAnalytics();
        break;
      case "tool_health_changed":
        refreshTools();
        refreshMatrix();
        break;
    }
  };
  ws.onclose = () => setTimeout(connectWS, 1500);
}

document.addEventListener("click", async (e) => {
  const t = e.target;
  if (t.id === "toggle-btn") {
    await jpost("/api/status/toggle");
    refreshStatus();
  } else if (t.id === "refresh-tools") {
    await jpost("/api/tools/refresh");
    refreshTools();
    refreshMatrix();
  } else if (t.id === "folder-add") {
    const inp = $("folder-input");
    const path = inp.value.trim();
    if (!path) return;
    try { await jpost("/api/folders", { path }); inp.value = ""; refreshFolders(); }
    catch (err) { alert(err.message); }
  } else if (t.dataset && t.dataset.remove) {
    if (!confirm("Remove this folder from watch list?")) return;
    await jdel(`/api/folders/${t.dataset.remove}`);
    refreshFolders();
  } else if (t.dataset && t.dataset.toggle) {
    await jpost(`/api/folders/${t.dataset.toggle}/toggle`);
    refreshFolders();
  }
});

(async function init() {
  await Promise.all([refreshStatus(), refreshTools(), refreshFolders(), refreshMatrix(), refreshHistory(), refreshAnalytics()]);
  connectWS();
})();
