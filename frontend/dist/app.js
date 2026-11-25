const $ = (id) => document.getElementById(id);
let state = null;
let actionInfo = "";

function wailsApp() {
  return window.go && window.go.main && window.go.main.App;
}

function installBrowserShim() {
  if (wailsApp()) return false;
  const api = new Proxy({}, {
    get: (_target, method) => async (...args) => {
      const response = await fetch("/api/call", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ method, args })
      });
      if (!response.ok) throw new Error(await response.text());
      return response.json();
    }
  });
  window.go = { main: { App: api } };
  window.__browserShim = true;
  return true;
}

function clamp01(v) { return Math.max(0, Math.min(1, v)); }
function radianceColor(r) {
  const scale = 1.25;
  return `rgb(${Math.round(clamp01(r[0] / scale) * 255)},${Math.round(clamp01(r[1] / scale) * 255)},${Math.round(clamp01(r[2] / scale) * 255)})`;
}
function shadedColor(c) {
  return `rgb(${Math.round(clamp01(c[0]) * 255)},${Math.round(clamp01(c[1]) * 255)},${Math.round(clamp01(c[2]) * 255)})`;
}
function heat(t) {
  t = clamp01(t);
  const hue = 220 - t * 220;
  return `hsl(${hue}, 90%, ${42 + t * 18}%)`;
}
function vec(v) { return v ? `(${v.map(x => Number(x).toFixed(4)).join(", ")})` : "-"; }
function shortID(id) { return id ? id.replace("probe:", "") : ""; }

async function waitForAPI() {
  for (let i = 0; i < 100; i++) {
    if (wailsApp()) {
      $("connection").textContent = window.__browserShim ? "浏览器 HTTP 回退已连接" : "Wails bindings connected";
      $("connection").className = "badge ok";
      await refresh();
      return;
    }
    await new Promise(r => setTimeout(r, 100));
  }
  $("connection").textContent = "未检测到 Wails API；请通过 wails dev/build 启动";
  $("connection").className = "badge";
}

async function refresh() {
  state = await wailsApp().GetBootstrap();
  render();
}

function showMessage(msg, bad) {
  $("message").textContent = msg || "";
  $("message").style.color = bad ? "#fca5a5" : "#bae6fd";
}

async function handleResponse(promise, okMessage) {
  try {
    const res = await promise;
    if (res.bootstrap) state = res.bootstrap;
    render();
    if (res.ok) showMessage(okMessage || res.message || "操作完成", false);
    else showMessage(formatError(res.error || res.message), true);
    return res;
  } catch (err) {
    showMessage(formatError(err), true);
    return { ok: false, message: String(err) };
  }
}

function formatError(err) {
  if (!err) return "未知错误";
  if (typeof err === "string") return err;
  const e = err;
  return `${e.code || "ERROR"}${e.room_id ? " room=" + e.room_id : ""}${e.id ? " id=" + e.id : ""}${e.field ? " field=" + e.field : ""}: ${e.message || ""}`;
}

function selectedRoomID() { return $("selected-room").value || (state.scene.rooms[0] && state.scene.rooms[0].id); }
function selectedProbeID() { return $("selected-probe").value; }

function render() {
  if (!state || !state.scene) return;
  renderProgress();
  renderRoomSelect();
  renderProbeSelect();
  renderRoomCards();
  renderPixels();
  renderInvalidation();
  renderCache();
  renderQueue();
  drawRooms();
  drawHeat();
  drawBlend();
}

function renderProgress() {
  const p = state.progress;
  $("progress-text").textContent = `${p.committed_rooms}/${p.total_rooms} 房间 · ${p.reconstructed_probes}/${p.total_probes} 格点`;
  $("progress-bar").style.width = `${(p.committed_rooms / p.total_rooms) * 100}%`;
}

function renderRoomSelect() {
  const select = $("selected-room");
  const current = select.value;
  select.innerHTML = state.scene.rooms.map(r => `<option value="${r.id}">${r.id} (${r.resolution.x}×${r.resolution.y}×${r.resolution.z})</option>`).join("");
  if (current) select.value = current;
  const room = state.scene.rooms.find(r => r.id === selectedRoomID()) || state.scene.rooms[0];
  $("heat-title").textContent = `${room.id} · ${room.name}`;
  $("selected-k").max = room.resolution.z - 1;
  if (Number($("selected-k").value) > room.resolution.z - 1) $("selected-k").value = 0;
  $("res-x").value = room.resolution.x;
  $("res-y").value = room.resolution.y;
  $("res-z").value = room.resolution.z;
}

function recordsByID() {
  const map = {};
  for (const r of state.records) map[r.id] = r;
  return map;
}

function renderProbeSelect() {
  const roomID = selectedRoomID();
  const select = $("selected-probe");
  const current = select.value;
  const probes = state.records.filter(r => r.room_id === roomID);
  select.innerHTML = probes.map(r => `<option value="${r.id}">${shortID(r.id)} · ${r.status}</option>`).join("");
  if (current && probes.some(p => p.id === current)) select.value = current;
  updateProbeDetail();
}

function updateProbeDetail() {
  const rec = recordsByID()[selectedProbeID()];
  if (!rec) return;
  $("rad-r").value = rec.input_radiance[0].toFixed(3);
  $("rad-g").value = rec.input_radiance[1].toFixed(3);
  $("rad-b").value = rec.input_radiance[2].toFixed(3);
  $("probe-detail").innerHTML = `
    <b class="mono">${rec.id}</b><br>
    position ${vec(rec.position)}<br>
    input ${vec(rec.input_radiance)}<br>
    reconstructed ${vec(rec.radiance)}<br>
    SH digest <span class="mono">${(rec.sh_digest || "").slice(0, 16)}…</span><br>
    status=${rec.status}, attempts=${rec.attempts}, recomputes=${rec.recompute_count}
    ${rec.last_error ? `<br><span class="fail">${rec.last_error}</span>` : ""}`;
}

function renderRoomCards() {
  $("room-cards").innerHTML = state.scene.rooms.map(room => {
    const st = state.progress.room_states[room.id] || "pending";
    return `<div class="room-card">
      <h3>${room.name}</h3>
      <span class="state ${st}">${st}</span>
      <div class="mono" style="font-size:11px;color:var(--muted)">${room.id}<br>${room.resolution.x}×${room.resolution.y}×${room.resolution.z}</div>
      <div class="mini-actions">
        <button data-room-retry="${room.id}">重试房间</button>
        <button data-room-commit="${room.id}">提交</button>
      </div>
    </div>`;
  }).join("");
}

function renderPixels() {
  $("pixel-body").innerHTML = state.pixels.map(p => `
    <tr>
      <td class="mono">${p.pixel_id}</td>
      <td><span style="display:inline-block;width:42px;height:12px;border-radius:6px;background:${p.error ? "#7f1d1d" : shadedColor(p.color)};border:1px solid #fff3"></span> ${p.error ? p.error_code : vec(p.color)}</td>
      <td>${p.alpha ? p.alpha.toFixed(3) : "0"}</td>
      <td>${p.cache_hit ? "hit" : "fresh"}</td>
    </tr>`).join("");
}

function renderInvalidation() {
  const items = state.plan ? state.plan.invalidation : [];
  $("invalidation-summary").textContent = items.length ? `${state.plan.mutation} · target=${state.plan.target_id} · ${items.length} nodes` : "尚无编辑";
  $("invalidation-body").innerHTML = items.length ? items.map(i => `
    <tr>
      <td class="mono">${i.node_id}</td>
      <td>${i.kind}</td>
      <td>${i.room_id || ""}</td>
      <td class="mono">${i.probe_id || ""}</td>
      <td>${i.adjacent_room_id || ""}</td>
      <td>${i.reason}</td>
    </tr>`).join("") : `<tr><td colspan="6">编辑格点后这里显示精确失效节点。</td></tr>`;
}

function renderCache() {
  const r = state.report;
  $("cache-stats").innerHTML = r ? `
    <div class="stat"><span>重算格点</span><b>${r.recomputed_probes.length}</b></div>
    <div class="stat"><span>探针缓存命中</span><b>${r.cache_hits}</b></div>
    <div class="stat"><span>像素重算</span><b>${r.pixels_recomputed.length}</b></div>
    <div class="stat"><span>像素缓存命中</span><b>${r.pixel_cache_hits}</b></div>` : `
    <div class="stat"><span>初始构建</span><b>42</b></div><div class="stat"><span>增量报告</span><b>—</b></div>`;
}

function renderQueue() {
  const events = (state.queue || []).slice(-80).reverse();
  const totalWait = (state.queue || []).reduce((s, e) => s + e.wait_nanos, 0);
  $("queue-summary").innerHTML = `
    <div class="stat"><span>排队事件</span><b>${(state.queue || []).length}</b></div>
    <div class="stat"><span>总等待</span><b>${(totalWait / 1e6).toFixed(3)}ms</b></div>`;
  $("queue-body").innerHTML = events.length ? events.map(e => `
    <tr><td class="mono">${e.probe_id}</td><td>${e.resource}</td><td>${e.wait_ms.toFixed(4)}</td><td>${e.reason}</td></tr>`).join("")
    : `<tr><td colspan="4">预算未发生等待。</td></tr>`;
}

function worldTransform(canvas, bounds) {
  const margin = 42;
  const w = canvas.width, h = canvas.height;
  const scale = Math.min((w - margin * 2) / (bounds.maxX - bounds.minX), (h - margin * 2) / (bounds.maxY - bounds.minY));
  return {
    x: (x) => margin + (x - bounds.minX) * scale,
    y: (y) => h - margin - (y - bounds.minY) * scale,
    scale
  };
}

function drawRooms() {
  const canvas = $("room-canvas"), ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  const t = worldTransform(canvas, { minX: -0.4, maxX: 8.4, minY: -0.4, maxY: 8.4 });
  const byID = recordsByID();
  for (const room of state.scene.rooms) {
    const x = t.x(room.bounds.min[0]), y = t.y(room.bounds.max[1]);
    const w = (room.bounds.max[0] - room.bounds.min[0]) * t.scale;
    const h = (room.bounds.max[1] - room.bounds.min[1]) * t.scale;
    const roomRecs = state.records.filter(r => r.room_id === room.id && r.status === "reconstructed");
    const avg = [0, 1, 2].map(c => roomRecs.reduce((s, r) => s + r.radiance[c], 0) / Math.max(1, roomRecs.length));
    ctx.fillStyle = radianceColor(avg) + "55";
    ctx.strokeStyle = "#7dd3fc"; ctx.lineWidth = 2;
    roundRect(ctx, x, y, w, h, 10); ctx.fill(); ctx.stroke();
    ctx.fillStyle = "#e5eefc"; ctx.font = "13px sans-serif";
    ctx.fillText(room.id, x + 10, y + 20);
  }
  for (const r of state.records) {
    ctx.beginPath();
    ctx.arc(t.x(r.position[0]), t.y(r.position[1]), r.status === "failed" ? 6 : 3.2, 0, Math.PI * 2);
    ctx.fillStyle = r.status === "failed" ? "#ef4444" : radianceColor(r.radiance);
    ctx.fill();
  }
  for (const p of state.pixels) {
    const result = state.pixels.find(x => x.pixel_id === p.id) || p;
    ctx.beginPath();
    ctx.arc(t.x(p.position[0]), t.y(p.position[1]), 8, 0, Math.PI * 2);
    ctx.fillStyle = result.error ? "#ef4444" : shadedColor(result.color || [0, 0, 0]);
    ctx.fill(); ctx.strokeStyle = "#fff"; ctx.lineWidth = 2; ctx.stroke();
  }
}

function roundRect(ctx, x, y, w, h, r) {
  ctx.beginPath();
  ctx.moveTo(x + r, y); ctx.arcTo(x + w, y, x + w, y + h, r); ctx.arcTo(x + w, y + h, x, y + h, r);
  ctx.arcTo(x, y + h, x, y, r); ctx.arcTo(x, y, x + w, y, r); ctx.closePath();
}

function drawHeat() {
  const canvas = $("heat-canvas"), ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  const roomID = selectedRoomID();
  const room = state.scene.rooms.find(r => r.id === roomID);
  if (!room) return;
  const k = Number($("selected-k").value);
  const rec = state.records.find(r => r.room_id === roomID);
  const cw = (canvas.width - 54) / room.resolution.x;
  const ch = (canvas.height - 54) / room.resolution.y;
  let min = Infinity, max = -Infinity, sum = 0, n = 0;
  for (let i = 0; i < room.resolution.x; i++) {
    for (let j = 0; j < room.resolution.y; j++) {
      const r = state.records.find(x => x.id === `probe:${roomID}:${i}:${j}:${k}`);
      const lum = r ? (0.2126 * r.radiance[0] + 0.7152 * r.radiance[1] + 0.0722 * r.radiance[2]) : 0;
      min = Math.min(min, lum); max = Math.max(max, lum); sum += lum; n++;
      const x = 28 + i * cw, y = canvas.height - 28 - (j + 1) * ch;
      ctx.fillStyle = r ? (r.status === "failed" ? "#ef4444" : radianceColor(r.radiance)) : "#1e293b";
      ctx.fillRect(x + 2, y + 2, cw - 4, ch - 4);
      ctx.strokeStyle = "#0b1220"; ctx.strokeRect(x + 2, y + 2, cw - 4, ch - 4);
      ctx.fillStyle = "#dbeafe"; ctx.font = "10px monospace";
      ctx.fillText(`${i},${j}`, x + 6, y + 14);
    }
  }
  $("heat-values").textContent = `k=${k}; lum min=${min.toFixed(3)} avg=${(sum / n).toFixed(3)} max=${max.toFixed(3)}`;
}

function drawBlend() {
  const canvas = $("blend-canvas"), ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  const boundary = chooseBoundary();
  if (!boundary) return;
  const t = worldTransform(canvas, { minX: -0.4, maxX: 8.4, minY: -0.4, maxY: 8.4 });
  for (const roomID of [boundary.room_a, boundary.room_b]) {
    const room = state.scene.rooms.find(r => r.id === roomID);
    const x = t.x(room.bounds.min[0]), y = t.y(room.bounds.max[1]);
    ctx.fillStyle = "rgba(56,189,248,.10)";
    ctx.strokeStyle = "#64748b";
    ctx.fillRect(x, y, (room.bounds.max[0] - room.bounds.min[0]) * t.scale, (room.bounds.max[1] - room.bounds.min[1]) * t.scale);
    ctx.strokeRect(x, y, (room.bounds.max[0] - room.bounds.min[0]) * t.scale, (room.bounds.max[1] - room.bounds.min[1]) * t.scale);
  }
  const p = boundary.axis === 0 ? { x: boundary.position, y: 2 } : { x: 2, y: boundary.position };
  ctx.strokeStyle = "#fbbf24"; ctx.lineWidth = 4;
  ctx.beginPath();
  if (boundary.axis === 0) { ctx.moveTo(t.x(boundary.position), t.y(0)); ctx.lineTo(t.x(boundary.position), t.y(4)); }
  else { ctx.moveTo(t.x(0), t.y(boundary.position)); ctx.lineTo(t.x(4), t.y(boundary.position)); }
  ctx.stroke();
  const pixel = state.pixels.find(p => p.boundary_id === boundary.id);
  if (pixel) {
    ctx.beginPath(); ctx.arc(t.x(pixel.position[0]), t.y(pixel.position[1]), 8, 0, Math.PI * 2);
    ctx.fillStyle = shadedColor(pixel.color); ctx.fill(); ctx.strokeStyle = "#ffffff"; ctx.lineWidth = 2; ctx.stroke();
  }
  $("blend-info").innerHTML = pixel
    ? `${boundary.id}: α=${pixel.alpha.toFixed(3)}<br>owner=${vec(pixel.owner_radiance)}<br>adjacent=${vec(pixel.other_radiance)}<br>blended=${vec(pixel.radiance)}`
    : boundary.id;
}

function chooseBoundary() {
  const planText = state.plan ? JSON.stringify(state.plan.invalidation) : "";
  if (planText.includes("boundary-ac") || selectedRoomID() === "room-c") return state.scene.boundaries.find(b => b.id === "boundary-ac");
  return state.scene.boundaries[0];
}

function bindEvents() {
  $("selected-room").addEventListener("change", render);
  $("selected-probe").addEventListener("change", updateProbeDetail);
  $("selected-k").addEventListener("input", drawHeat);
  $("btn-edit").addEventListener("click", async () => {
    const id = selectedProbeID();
    await handleResponse(wailsApp().EditProbe(id, Number($("rad-r").value), Number($("rad-g").value), Number($("rad-b").value)), "增量重建完成");
  });
  $("btn-retry-probe").addEventListener("click", () => handleResponse(wailsApp().RetryProbe(selectedProbeID()), "格点重试成功"));
  $("btn-fault").addEventListener("click", () => handleResponse(wailsApp().SimulateProbeFailure(selectedProbeID()), "已注入并捕获一次异常"));
  $("btn-resolution").addEventListener("click", () => handleResponse(wailsApp().ApplyResolution(selectedRoomID(), Number($("res-x").value), Number($("res-y").value), Number($("res-z").value)), "分辨率调整完成"));
  $("btn-reset").addEventListener("click", () => handleResponse(wailsApp().ResetDemo(), "演示已重置"));
  $("btn-query").addEventListener("click", async () => {
    const res = await handleResponse(wailsApp().QueryRadiance($("query-id").value, $("query-room").value, Number($("query-x").value), Number($("query-y").value), Number($("query-z").value)));
    if (res && res.query) $("query-result").textContent = JSON.stringify(res.query, null, 2);
  });
  $("btn-verify").addEventListener("click", async () => {
    const res = await handleResponse(wailsApp().VerifyTruth());
    if (res && res.verify) renderPrecision(res.verify);
  });
  $("btn-acceptance").addEventListener("click", async () => {
    $("acceptance-body").innerHTML = `<tr><td colspan="3">正在运行确定性验收…</td></tr>`;
    const res = await handleResponse(wailsApp().RunAcceptance());
    if (res && res.acceptance) renderAcceptance(res.acceptance);
  });
  $("room-cards").addEventListener("click", async (ev) => {
    const retry = ev.target.getAttribute("data-room-retry");
    const commit = ev.target.getAttribute("data-room-commit");
    if (retry) await handleResponse(wailsApp().RetryRoom(retry), `房间 ${retry} 重试完成`);
    if (commit) await handleResponse(wailsApp().CommitRoom(commit), `房间 ${commit} 已提交冻结`);
  });
}

function renderPrecision(v) {
  $("precision-summary").innerHTML = `
    <span class="pill ${v.pass ? "good" : "bad"}">precision ${v.pass ? "PASS" : "FAIL"}</span>
    <span class="pill">probe max err ${v.max_probe_error.toExponential(2)}</span>
    <span class="pill">pixel max err ${v.max_pixel_error.toExponential(2)}</span>
    <span class="pill">jump max err ${v.max_jump_error.toExponential(2)}</span>`;
}

function renderAcceptance(a) {
  $("acceptance-summary").textContent = a.ok ? "ALL CHECKS PASS" : "HAS FAILURES";
  if (a.precision) renderPrecision(a.precision);
  $("acceptance-body").innerHTML = a.cases.map(c => `
    <tr>
      <td class="mono">${c.name}</td>
      <td class="${c.pass ? "pass" : "fail"}">${c.pass ? "PASS" : "FAIL"}</td>
      <td>${c.code ? `<span class="pill">${c.code}</span> ` : ""}${c.detail || ""}${c.mismatch ? `<pre class="code-block">${JSON.stringify(c.mismatch, null, 2)}</pre>` : ""}</td>
    </tr>`).join("");
}

async function detectBrowserFallback() {
  try {
    const response = await fetch("/api/call", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ method: "GetBootstrap", args: [] })
    });
    if (!response.ok || !(response.headers.get("content-type") || "").includes("application/json")) return false;
    const data = await response.json();
    return Boolean(data && data.scene);
  } catch {
    return false;
  }
}

async function start() {
  bindEvents();
  if (await detectBrowserFallback()) {
    installBrowserShim();
    $("connection").textContent = "浏览器 HTTP 回退已连接";
    $("connection").className = "badge ok";
    await refresh();
    return;
  }
  await waitForAPI();
}

start();
