const state = {
  data: null,
  selectedRoom: null,
  selectedPortal: null,
  selectedListener: "l0",
  selectedSource: "s0",
  drag: null,
  toastTimer: null,
};

const $ = (id) => document.getElementById(id);
const COLORS = {
  bg: "#0b111b",
  room: "#142236",
  roomAlt: "#172941",
  line: "#3a536f",
  committed: "#3ee59b",
  pending: "#74839a",
  connectivity: "#ffbd5c",
  parameters: "#ffbd5c",
  failed: "#ff6b7a",
  source: "#ffbd5c",
  listener: "#49d3ff",
  selected: "#8ce9ff",
  path: "#a992ff",
  text: "#e8eef8",
  muted: "#8fa0b8",
};

async function api(path, options = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  let body = null;
  try { body = await res.json(); } catch (_) { /* ignore */ }
  if (!res.ok || (body && body.ok === false)) {
    const err = (body && body.error) || { code: "E_HTTP", message: `HTTP ${res.status}` };
    const e = new Error(err.message || err.code);
    e.payload = err;
    e.body = body;
    throw e;
  }
  return body;
}

async function load() {
  try {
    const data = await api("/api/state");
    state.data = data;
    if (!state.selectedRoom) state.selectedRoom = data.level.rooms[0]?.id || null;
    if (!data.level.listeners.some(l => l.id === state.selectedListener)) {
      state.selectedListener = data.level.listeners[0]?.id || "";
    }
    if (!data.level.sources.some(s => s.id === state.selectedSource)) {
      state.selectedSource = data.level.sources[0]?.id || "";
    }
    if (state.selectedPortal && !data.level.openings.some(o => o.id === state.selectedPortal)) {
      state.selectedPortal = null;
    }
    render();
  } catch (err) {
    toastError(err);
  }
}

function render() {
  if (!state.data) return;
  const { level, result } = state.data;
  $("subtitle").textContent = `${level.id} · seed ${level.seed} · ${level.rooms.length} rooms · ${level.sources.length} sources · ${level.listeners.length} listeners`;
  $("algo-chip").textContent = `${result.precision} · ${result.algorithm_version}`;
  $("progress").textContent = `${result.progress.committed} / ${result.progress.total}`;
  $("progress-bar").style.width = `${result.progress.total ? 100 * result.progress.committed / result.progress.total : 0}%`;
  $("state-count").textContent = `${result.states.filter(s => s.state === "committed").length} committed · ${result.states.filter(s => s.state === "failed").length} failed`;
  $("plan-summary").textContent = `${level.walls.length} 墙体 · ${level.openings.length} 门洞 · 版本 ${result.params_version}`;

  populateSelectors(level);
  renderStateMatrix(result);
  renderInvalidations(state.data.last_change);
  renderBudget(state.data);
  renderEditor(level, result);
  renderTables(level, result);
  renderCharts(level, result);
  drawPlan();
  updateCompareStatus();
}

function populateSelectors(level) {
  fillSelect($("listener-select"), level.listeners.map(l => [l.id, `${l.id}`]), state.selectedListener);
  fillSelect($("source-select"), level.sources.map(s => [s.id, `${s.id}`]), state.selectedSource);
  fillSelect($("portal-select"), [["", "—"], ...level.openings.map(o => [o.id, `${o.id} · ${o.wall_id}`])], state.selectedPortal || "");
  fillSelect($("wall-select"), level.walls.map(w => [w.id, `${w.id} · ${w.rooms.join(" / ")}`]), null);
}

function fillSelect(el, entries, selected) {
  const previous = selected ?? el.value;
  el.replaceChildren();
  for (const [value, label] of entries) {
    const opt = document.createElement("option");
    opt.value = value;
    opt.textContent = label;
    if (value === previous) opt.selected = true;
    el.appendChild(opt);
  }
}

function stateOf(roomId) {
  return state.data?.result?.states.find(s => s.room_id === roomId)?.state || "pending";
}

function renderStateMatrix(result) {
  const wrap = $("state-matrix");
  wrap.replaceChildren();
  for (const roomState of result.states) {
    const card = document.createElement("button");
    card.type = "button";
    card.className = `state-card ${roomState.state}${roomState.room_id === state.selectedRoom ? " selected" : ""}`;
    card.onclick = () => {
      state.selectedRoom = roomState.room_id;
      render();
    };
    const top = document.createElement("div");
    top.className = "state-card-top";
    const id = document.createElement("strong");
    id.textContent = roomState.room_id;
    const badge = document.createElement("span");
    badge.className = "state-badge";
    badge.textContent = roomState.state_label;
    top.append(id, badge);
    const detail = document.createElement("small");
    const rr = result.rooms[roomState.room_id];
    detail.textContent = rr ? `${rr.stats.reachable}/${rr.stats.pairs} paths · ${fmt(rr.params.t60_ms, 0)} ms` : (roomState.last_error || "no committed result");
    card.append(top, detail);
    wrap.appendChild(card);
  }
  const selected = result.states.find(s => s.room_id === state.selectedRoom);
  const last = $("last-state");
  last.textContent = selected?.state_label || "IDLE";
  last.className = `chip ${selected?.state === "failed" ? "error" : selected?.state === "committed" ? "ok" : "warn"}`;
}

function renderInvalidations(change) {
  const wrap = $("invalidation-list");
  wrap.replaceChildren();
  if (!change || !change.invalidations?.length) {
    wrap.className = "invalidation-list empty";
    wrap.textContent = "暂无编辑";
    $("reuse-count").textContent = "复用 0";
    return;
  }
  wrap.className = "invalidation-list";
  const reused = change.invalidations.filter(r => r.reused).length;
  $("reuse-count").textContent = `复用 ${reused}`;
  for (const rec of change.invalidations) {
    const row = document.createElement("div");
    row.className = "inv-row";
    const room = document.createElement("strong");
    room.textContent = rec.room_id;
    const reason = document.createElement("span");
    reason.className = "reason";
    reason.textContent = rec.reason + (rec.invalidated_listeners?.length ? ` · ${rec.invalidated_listeners.join(",")}` : "");
    const status = document.createElement("span");
    status.className = `status ${rec.invalidated ? "invalid" : "reused"}`;
    status.textContent = rec.invalidated ? "失效" : "复用";
    row.append(room, reason, status);
    wrap.appendChild(row);
  }
}

function renderBudget(data) {
  const b = data.budget;
  const wrap = $("budget");
  wrap.replaceChildren();
  for (const [label, value] of [
    ["并行度", b.max_parallel],
    ["路径缓冲/线程", b.path_buffer_limit],
    ["图缓存预算", b.graph_cache_budget],
    ["检查点写入", b.checkpoint_serial ? "串行" : "并发"],
  ]) {
    const item = document.createElement("div");
    item.className = "budget-item";
    const span = document.createElement("span");
    span.textContent = label;
    const strong = document.createElement("strong");
    strong.textContent = value;
    item.append(span, strong);
    wrap.appendChild(item);
  }
  const m = data.result.metrics || {};
  const metrics = $("metrics");
  metrics.replaceChildren();
  for (const [label, value] of [
    ["峰值线程", m.peak_workers || 0],
    ["排队阻塞", m.queue_blocked || 0],
    ["缓存等待", m.graph_cache_budget_waits || 0],
    ["路径高水位", m.path_buffer_high_water || 0],
  ]) {
    const s = document.createElement("span");
    s.textContent = `${label} ${value}`;
    if (label === "缓存等待" && m.resource_events?.length) {
      s.title = m.resource_events.map(e => `${e.kind}: ${e.reason}`).join("\n");
    }
    metrics.appendChild(s);
  }
}

function renderEditor(level, result) {
  const room = level.rooms.find(r => r.id === state.selectedRoom);
  const roomState = result.states.find(s => s.room_id === state.selectedRoom);
  const rr = result.rooms[state.selectedRoom];
  const editable = Boolean(room) && roomState?.state !== "parameters" && roomState?.state !== "connectivity";
  $("selection-label").textContent = room
    ? `${room.id} · ${room.name} · rect(${fmt(room.rect.x, 0)}, ${fmt(room.rect.y, 0)}, ${fmt(room.rect.w, 0)}, ${fmt(room.rect.h, 0)})`
    : "未选择房间";
  for (const id of ["t60", "density", "gain", "edge-select", "edge-delta", "btn-edge-minus", "btn-edge-plus", "btn-apply-params"]) {
    $(id).disabled = !editable;
  }
  $("btn-retry").disabled = roomState?.state !== "failed";
  if (room) {
    $("t60").value = room.params.t60_ms;
    $("density").value = room.params.early_density;
    $("gain").value = room.params.reverb_gain;
    $("t60-out").value = `${fmt(room.params.t60_ms, 0)} ms`;
    $("density-out").value = fmt(room.params.early_density, 2);
    $("gain-out").value = fmt(room.params.reverb_gain, 2);
    $("t60").max = Math.max(3000, Math.ceil(room.params.t60_ms / 100) * 100);
  }
  $("btn-remove-portal").disabled = !state.selectedPortal;
  const wall = level.walls[0];
  if (wall && (!$("portal-x").value || $("portal-x").value === "0")) {
    $("portal-x").value = ((wall.start.x + wall.end.x) / 2).toFixed(0);
    $("portal-y").value = ((wall.start.y + wall.end.y) / 2).toFixed(0);
  }
}

function renderTables(level, result) {
  const tbody = $("pair-table");
  tbody.replaceChildren();
  const pairs = [];
  for (const room of Object.values(result.rooms)) {
    for (const p of room.pairs) pairs.push({ room: room.room_id, ...p });
  }
  pairs.sort((a, b) => a.listener_id.localeCompare(b.listener_id) || a.source_id.localeCompare(b.source_id));
  $("pair-count").textContent = `${pairs.length} pairs`;
  for (const p of pairs) {
    const tr = document.createElement("tr");
    if (p.listener_id === state.selectedListener) tr.className = "pair-selected";
    for (const value of [
      p.listener_id,
      p.source_id,
      p.reachable ? p.room_path.join(" → ") : "UNREACHABLE",
      p.portal_path?.join(" → ") || "—",
      p.reachable ? fmt(p.path_mm, 3) : "—",
      p.reachable ? fmt(p.decay_ms, 3) : "—",
      p.reachable ? fmt(p.early_energy, 6) : "—",
    ]) {
      const td = document.createElement("td");
      td.textContent = value;
      if (typeof value === "string" && /^[\d.,-]+$/.test(value)) td.className = "mono";
      tr.appendChild(td);
    }
    tbody.appendChild(tr);
  }
}

function pairsForListener(result, listenerId) {
  const found = [];
  for (const room of Object.values(result.rooms)) {
    for (const p of room.pairs) {
      if (p.listener_id === listenerId && p.reachable) found.push(p);
    }
  }
  found.sort((a, b) => a.source_id.localeCompare(b.source_id));
  return found;
}

function renderCharts(level, result) {
  const listener = level.listeners.find(l => l.id === state.selectedListener);
  $("listener-caption").textContent = listener ? `${listener.id} · (${fmt(listener.pos.x, 0)}, ${fmt(listener.pos.y, 0)})` : "选择听者";
  const listenerRoom = state.data.truth.listener_rooms[state.selectedListener] || "—";
  $("listener-room-chip").textContent = `房间 ${listenerRoom}`;
  const pairs = pairsForListener(result, state.selectedListener);
  drawChart($("decay-chart"), pairs, "decay_ms", COLORS.cyan);
  drawChart($("energy-chart"), pairs, "early_energy", COLORS.violet);
}

function drawChart(canvas, pairs, field, color) {
  prepareCanvas(canvas);
  const ctx = canvas.getContext("2d");
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  ctx.clearRect(0, 0, w, h);
  const pad = { left: 44, right: 12, top: 14, bottom: 26 };
  ctx.strokeStyle = "#25364b";
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad.top + (h - pad.top - pad.bottom) * i / 4;
    ctx.beginPath();
    ctx.moveTo(pad.left, y);
    ctx.lineTo(w - pad.right, y);
    ctx.stroke();
  }
  if (!pairs.length) {
    ctx.fillStyle = COLORS.muted;
    ctx.font = "11px system-ui";
    ctx.fillText("no reachable result", pad.left, h / 2);
    return;
  }
  const values = pairs.map(p => Number(p[field] || 0));
  const min = Math.min(...values);
  const max = Math.max(...values);
  const lo = min === max ? min - 1 : min;
  const hi = min === max ? max + 1 : max;
  const x = (i) => pad.left + (w - pad.left - pad.right) * (pairs.length === 1 ? .5 : i / (pairs.length - 1));
  const y = (v) => pad.top + (h - pad.top - pad.bottom) * (1 - (v - lo) / (hi - lo));
  ctx.strokeStyle = color;
  ctx.lineWidth = 2;
  ctx.beginPath();
  pairs.forEach((p, i) => {
    const px = x(i), py = y(values[i]);
    if (i === 0) ctx.moveTo(px, py); else ctx.lineTo(px, py);
  });
  ctx.stroke();
  ctx.fillStyle = color;
  pairs.forEach((p, i) => {
    ctx.beginPath();
    ctx.arc(x(i), y(values[i]), 3.5, 0, Math.PI * 2);
    ctx.fill();
    ctx.fillStyle = COLORS.muted;
    ctx.font = "9px system-ui";
    ctx.textAlign = "center";
    ctx.fillText(p.source_id, x(i), h - 9);
    ctx.fillStyle = color;
    ctx.textAlign = "left";
  });
  ctx.fillStyle = COLORS.muted;
  ctx.font = "9px system-ui";
  ctx.textAlign = "right";
  ctx.fillText(fmt(hi, field === "early_energy" ? 3 : 1), pad.left - 5, pad.top + 3);
  ctx.fillText(fmt(lo, field === "early_energy" ? 3 : 1), pad.left - 5, h - pad.bottom);
  ctx.textAlign = "left";
}

function prepareCanvas(canvas) {
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  const width = Math.max(1, Math.round(rect.width * dpr));
  const height = Math.max(1, Math.round(rect.height * dpr));
  if (canvas.width !== width || canvas.height !== height) {
    canvas.width = width;
    canvas.height = height;
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return ctx;
}

function worldBounds(level) {
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  for (const room of level.rooms) {
    minX = Math.min(minX, room.rect.x);
    minY = Math.min(minY, room.rect.y);
    maxX = Math.max(maxX, room.rect.x + room.rect.w);
    maxY = Math.max(maxY, room.rect.y + room.rect.h);
  }
  const pad = 140;
  return { minX: minX - pad, minY: minY - pad, maxX: maxX + pad, maxY: maxY + pad };
}

function planTransform() {
  const canvas = $("plan");
  const rect = canvas.getBoundingClientRect();
  const b = worldBounds(state.data.level);
  const sx = rect.width / (b.maxX - b.minX);
  const sy = rect.height / (b.maxY - b.minY);
  const scale = Math.min(sx, sy);
  const ox = (rect.width - (b.maxX - b.minX) * scale) / 2 - b.minX * scale;
  const oy = (rect.height - (b.maxY - b.minY) * scale) / 2 - b.minY * scale;
  return {
    scale, ox, oy,
    toScreen: (x, y) => [x * scale + ox, y * scale + oy],
    toWorld: (x, y) => [(x - ox) / scale, (y - oy) / scale],
  };
}

function drawPlan() {
  if (!state.data) return;
  const canvas = $("plan");
  prepareCanvas(canvas);
  const ctx = canvas.getContext("2d");
  const { level, result } = state.data;
  const tr = planTransform();
  const w = canvas.clientWidth;
  const h = canvas.clientHeight;
  ctx.clearRect(0, 0, w, h);

  for (const room of level.rooms) {
    const st = stateOf(room.id);
    const [x, y] = tr.toScreen(room.rect.x, room.rect.y);
    const rw = room.rect.w * tr.scale;
    const rh = room.rect.h * tr.scale;
    ctx.fillStyle = st === "committed" ? "rgba(62, 229, 155, .075)" : st === "failed" ? "rgba(255, 107, 122, .1)" : "rgba(255, 189, 92, .07)";
    ctx.fillRect(x, y, rw, rh);
    ctx.strokeStyle = room.id === state.selectedRoom ? COLORS.selected : COLORS.line;
    ctx.lineWidth = room.id === state.selectedRoom ? 2.5 : 1.3;
    ctx.strokeRect(x, y, rw, rh);
    ctx.fillStyle = COLORS.text;
    ctx.font = "600 12px system-ui";
    ctx.fillText(room.id, x + 9, y + 17);
    ctx.fillStyle = COLORS.muted;
    ctx.font = "10px system-ui";
    ctx.fillText(room.name, x + 9, y + 31);
    ctx.fillStyle = COLORS.muted;
    ctx.fillText(`T60 ${fmt(room.params.t60_ms, 0)} ms`, x + 9, y + 45);
    if (result.rooms[room.id]) {
      ctx.fillStyle = COLORS.committed;
      ctx.fillText(`${result.rooms[room.id].stats.reachable} paths`, x + 9, y + 59);
    }
  }

  for (const wall of level.walls) {
    const [x1, y1] = tr.toScreen(wall.start.x, wall.start.y);
    const [x2, y2] = tr.toScreen(wall.end.x, wall.end.y);
    ctx.strokeStyle = "#5e7898";
    ctx.lineWidth = 4;
    ctx.lineCap = "round";
    ctx.beginPath();
    ctx.moveTo(x1, y1);
    ctx.lineTo(x2, y2);
    ctx.stroke();
  }

  const selectedPair = pairsForListener(result, state.selectedListener).find(p => p.source_id === state.selectedSource);
  if (selectedPair) drawPath(ctx, tr, level, selectedPair);

  for (const o of level.openings) {
    const [x, y] = tr.toScreen(o.center.x, o.center.y);
    const radius = Math.max(5, o.width * tr.scale / 2);
    ctx.save();
    ctx.translate(x, y);
    const wall = level.walls.find(w => w.id === o.wall_id);
    if (wall) {
      const vertical = Math.abs(wall.start.x - wall.end.x) < Math.abs(wall.start.y - wall.end.y);
      if (vertical) ctx.rotate(Math.PI / 2);
    }
    ctx.fillStyle = o.id === state.selectedPortal ? "rgba(73, 211, 255, .45)" : "rgba(62, 229, 155, .35)";
    ctx.strokeStyle = o.id === state.selectedPortal ? COLORS.selected : COLORS.committed;
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    ctx.roundRect(-radius, -4, radius * 2, 8, 4);
    ctx.fill();
    ctx.stroke();
    ctx.restore();
    ctx.fillStyle = COLORS.muted;
    ctx.font = "9px system-ui";
    ctx.fillText(o.id, x + 7, y - 8);
  }

  for (const s of level.sources) {
    const [x, y] = tr.toScreen(s.pos.x, s.pos.y);
    drawMarker(ctx, x, y, COLORS.source, "S");
    ctx.fillStyle = COLORS.source;
    ctx.font = "10px system-ui";
    ctx.fillText(s.id, x + 10, y + 4);
  }
  for (const l of level.listeners) {
    const [x, y] = tr.toScreen(l.pos.x, l.pos.y);
    drawMarker(ctx, x, y, COLORS.listener, "L");
    ctx.fillStyle = COLORS.listener;
    ctx.font = "10px system-ui";
    ctx.fillText(l.id, x + 10, y + 4);
  }
  if (!state.data.last_change) $("plan-hint").textContent = "选择房间以编辑参数与边界";
}

function drawMarker(ctx, x, y, color, label) {
  ctx.fillStyle = color;
  ctx.beginPath();
  ctx.arc(x, y, 5.5, 0, Math.PI * 2);
  ctx.fill();
  ctx.fillStyle = COLORS.bg;
  ctx.font = "bold 8px system-ui";
  ctx.textAlign = "center";
  ctx.fillText(label, x, y + 3);
  ctx.textAlign = "left";
}

function drawPath(ctx, tr, level, pair) {
  const source = level.sources.find(s => s.id === pair.source_id);
  const listener = level.listeners.find(l => l.id === pair.listener_id);
  if (!source || !listener) return;
  const points = [source.pos];
  for (const pid of pair.portal_path || []) {
    const p = level.openings.find(o => o.id === pid);
    if (p) points.push(p.center);
  }
  points.push(listener.pos);
  ctx.save();
  ctx.strokeStyle = "rgba(169, 146, 255, .8)";
  ctx.lineWidth = 2;
  ctx.setLineDash([6, 5]);
  ctx.beginPath();
  points.forEach((p, i) => {
    const [x, y] = tr.toScreen(p.x, p.y);
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.stroke();
  ctx.restore();
}

function handlePlanClick(evt) {
  if (!state.data) return;
  const canvas = $("plan");
  const rect = canvas.getBoundingClientRect();
  const px = evt.clientX - rect.left;
  const py = evt.clientY - rect.top;
  const tr = planTransform();
  const [wx, wy] = tr.toWorld(px, py);
  for (const o of state.data.level.openings) {
    const [x, y] = tr.toScreen(o.center.x, o.center.y);
    if (Math.hypot(px - x, py - y) < 16) {
      state.selectedPortal = o.id;
      $("portal-select").value = o.id;
      render();
      return;
    }
  }
  const room = state.data.level.rooms.find(r => wx >= r.rect.x && wx <= r.rect.x + r.rect.w && wy >= r.rect.y && wy <= r.rect.y + r.rect.h);
  if (room) {
    state.selectedRoom = room.id;
    render();
  }
}

function edgeAt(room, wx, wy) {
  if (!room) return null;
  const tol = 55;
  const candidates = [
    ["left", Math.abs(wx - room.rect.x)],
    ["right", Math.abs(wx - (room.rect.x + room.rect.w))],
    ["top", Math.abs(wy - room.rect.y)],
    ["bottom", Math.abs(wy - (room.rect.y + room.rect.h))],
  ].filter(([, d]) => d <= tol).sort((a, b) => a[1] - b[1]);
  return candidates[0]?.[0] || null;
}

function bindPlanDrag() {
  const canvas = $("plan");
  canvas.addEventListener("click", handlePlanClick);
  canvas.addEventListener("pointerdown", (evt) => {
    if (!state.data) return;
    const room = state.data.level.rooms.find(r => r.id === state.selectedRoom);
    if (!room) return;
    const rect = canvas.getBoundingClientRect();
    const [wx, wy] = planTransform().toWorld(evt.clientX - rect.left, evt.clientY - rect.top);
    const edge = edgeAt(room, wx, wy);
    if (!edge) return;
    state.drag = { room: room.id, edge, startX: evt.clientX, startY: evt.clientY, startWorldX: wx, startWorldY: wy };
    canvas.setPointerCapture(evt.pointerId);
    $("plan-hint").textContent = `拖动 ${room.id} ${edge} 边界…`;
  });
  canvas.addEventListener("pointerup", async (evt) => {
    const drag = state.drag;
    state.drag = null;
    if (!drag) return;
    const rect = canvas.getBoundingClientRect();
    const [wx, wy] = planTransform().toWorld(evt.clientX - rect.left, evt.clientY - rect.top);
    let delta = 0;
    if (drag.edge === "left" || drag.edge === "right") delta = wx - drag.startWorldX;
    else delta = wy - drag.startWorldY;
    delta = Math.round(delta / 5) * 5;
    if (Math.abs(delta) < 5) {
      $("plan-hint").textContent = "位移过小，未提交";
      return;
    }
    try {
      await api("/api/edge", { method: "POST", body: JSON.stringify({ room_id: drag.room, edge: drag.edge, delta_mm: delta }) });
      $("plan-hint").textContent = `${drag.room} ${drag.edge} 位移 ${delta} mm`;
      await load();
    } catch (err) {
      await load();
      toastError(err);
    }
  });
  canvas.addEventListener("pointermove", (evt) => {
    if (!state.drag || !state.data) return;
    const rect = canvas.getBoundingClientRect();
    const [wx, wy] = planTransform().toWorld(evt.clientX - rect.left, evt.clientY - rect.top);
    const delta = state.drag.edge === "left" || state.drag.edge === "right"
      ? wx - state.drag.startWorldX : wy - state.drag.startWorldY;
    $("plan-hint").textContent = `${state.drag.room} ${state.drag.edge} Δ ${delta.toFixed(0)} mm`;
  });
}

async function applyParams() {
  if (!state.selectedRoom) return;
  try {
    await api("/api/params", {
      method: "POST",
      body: JSON.stringify({
        room_id: state.selectedRoom,
        t60_ms: Number($("t60").value),
        early_density: Number($("density").value),
        reverb_gain: Number($("gain").value),
      }),
    });
    await load();
    toast("房间参数已提交");
  } catch (err) {
    await load();
    toastError(err);
  }
}

async function moveEdge(sign) {
  if (!state.selectedRoom) return;
  const delta = Math.abs(Number($("edge-delta").value || 80)) * sign;
  try {
    await api("/api/edge", {
      method: "POST",
      body: JSON.stringify({ room_id: state.selectedRoom, edge: $("edge-select").value, delta_mm: delta }),
    });
    await load();
    toast(`边界位移 ${delta} mm 已提交`);
  } catch (err) {
    await load();
    toastError(err);
  }
}

async function addPortal() {
  try {
    const id = `o${Date.now().toString(36).slice(-4)}`;
    await api("/api/portal", {
      method: "POST",
      body: JSON.stringify({
        action: "add", id,
        wall_id: $("wall-select").value,
        center: { x: Number($("portal-x").value), y: Number($("portal-y").value) },
        width: Number($("portal-width").value),
      }),
    });
    state.selectedPortal = id;
    await load();
    toast(`门洞 ${id} 已添加`);
  } catch (err) {
    await load();
    toastError(err);
  }
}

async function removePortal() {
  if (!state.selectedPortal) return;
  try {
    await api("/api/portal", {
      method: "POST",
      body: JSON.stringify({ action: "remove", portal_id: state.selectedPortal }),
    });
    toast(`门洞 ${state.selectedPortal} 已删除`);
    state.selectedPortal = null;
    await load();
  } catch (err) {
    await load();
    toastError(err);
  }
}

async function retryRoom() {
  if (!state.selectedRoom) return;
  try {
    await api("/api/retry", { method: "POST", body: JSON.stringify({ room_id: state.selectedRoom }) });
    await load();
    toast(`房间 ${state.selectedRoom} 已重试`);
  } catch (err) {
    await load();
    toastError(err);
  }
}

async function comparePaths() {
  const btn = $("btn-compare");
  btn.disabled = true;
  btn.textContent = "比对中…";
  $("compare-status").textContent = "运行中";
  $("compare-status").className = "chip warn";
  try {
    const body = await api("/api/compare", { method: "POST", body: "{}" });
    $("compare-status").textContent = body.ok ? "一致" : "不一致";
    $("compare-status").className = `chip ${body.ok ? "ok" : "error"}`;
    renderCompare(body.comparison, null);
  } catch (err) {
    $("compare-status").textContent = err.payload?.code || "失败";
    $("compare-status").className = "chip error";
    renderCompare(err.body?.comparison, err.body?.error || err.payload);
  } finally {
    btn.disabled = false;
    btn.textContent = "三路径比对";
  }
}

function renderCompare(comparison, error) {
  const wrap = $("compare-result");
  wrap.replaceChildren();
  wrap.className = "compare-result";
  if (!comparison) {
    wrap.textContent = error?.message || "无比对结果";
    return;
  }
  const grid = document.createElement("div");
  grid.className = "compare-grid";
  for (const [label, value] of [
    ["提交序列", comparison.commit_order?.join(" → ") || "—"],
    ["听者统计", comparison.stats ? `${comparison.stats.reachable}/${comparison.stats.pairs} reachable` : "—"],
    ["断点恢复", comparison.resume_metrics ? "SQLite snapshot" : "未返回"],
    ["复用房间", comparison.preview_reused ?? "—"],
  ]) {
    const item = document.createElement("div");
    item.className = "compare-item";
    const span = document.createElement("span");
    span.textContent = label;
    const strong = document.createElement("strong");
    strong.textContent = value;
    item.append(span, strong);
    grid.appendChild(item);
  }
  wrap.appendChild(grid);
  const detail = document.createElement("div");
  detail.className = "compare-detail";
  if (error) {
    detail.innerHTML = `<strong>${escapeHTML(error.code || "E_DIFF")}</strong> · room <code>${escapeHTML(error.room || "—")}</code> · listener <code>${escapeHTML(error.listener || "—")}</code><br>field <code>${escapeHTML(error.field || "—")}</code> · ${escapeHTML(error.message || "")}`;
  } else {
    detail.textContent = "实时预览、断点恢复、全量重算的房间链、路径、衰减、早期能量、统计与提交序列逐项一致。";
  }
  wrap.appendChild(detail);
}

function updateCompareStatus() {
  if (!state.data?.last_change) return;
  const invalid = state.data.last_change.invalidations.filter(r => r.invalidated).length;
  $("plan-hint").textContent = `上次编辑：${state.data.last_change.edit?.kind || "edit"} · ${invalid} 个房间失效 · ${state.data.last_change.candidate_rooms?.length || 0} 个候选`;
}

function toast(message) {
  const el = $("toast");
  el.className = "toast show";
  el.textContent = message;
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => { el.className = "toast"; }, 2800);
}

function toastError(err) {
  const p = err.payload || {};
  const parts = [p.code || "E_ERROR", p.room && `room=${p.room}`, p.listener && `listener=${p.listener}`, p.source && `source=${p.source}`, p.field && `field=${p.field}`, p.message || err.message].filter(Boolean);
  const el = $("toast");
  el.className = "toast show error";
  el.textContent = parts.join(" · ");
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => { el.className = "toast"; }, 5200);
}

function fmt(v, digits = 2) {
  if (v === null || v === undefined || Number.isNaN(Number(v))) return "-";
  return Number(v).toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

function escapeHTML(v) {
  return String(v).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

function bindUI() {
  $("listener-select").addEventListener("change", (e) => { state.selectedListener = e.target.value; render(); });
  $("source-select").addEventListener("change", (e) => { state.selectedSource = e.target.value; drawPlan(); });
  $("portal-select").addEventListener("change", (e) => { state.selectedPortal = e.target.value || null; drawPlan(); renderEditor(state.data.level, state.data.result); });
  $("wall-select").addEventListener("change", (e) => {
    const wall = state.data.level.walls.find(w => w.id === e.target.value);
    if (!wall) return;
    $("portal-x").value = ((wall.start.x + wall.end.x) / 2).toFixed(0);
    $("portal-y").value = ((wall.start.y + wall.end.y) / 2).toFixed(0);
  });
  $("btn-remove-portal").addEventListener("click", removePortal);
  $("btn-apply-params").addEventListener("click", applyParams);
  $("btn-edge-minus").addEventListener("click", () => moveEdge(-1));
  $("btn-edge-plus").addEventListener("click", () => moveEdge(1));
  $("btn-add-portal").addEventListener("click", addPortal);
  $("btn-retry").addEventListener("click", retryRoom);
  $("btn-compare").addEventListener("click", comparePaths);
  $("btn-export").addEventListener("click", () => { window.location.href = "/api/export"; });
  $("btn-reset").addEventListener("click", async () => {
    if (!confirm("复位到样本初始关卡并清空当前编辑序列？")) return;
    try {
      await api("/api/reset", { method: "POST", body: "{}" });
      state.lastChange = null;
      await load();
      toast("已复位");
    } catch (err) { toastError(err); }
  });
  for (const id of ["t60", "density", "gain"]) {
    $(id).addEventListener("input", () => {
      $("t60-out").value = `${fmt($("t60").value, 0)} ms`;
      $("density-out").value = fmt($("density").value, 2);
      $("gain-out").value = fmt($("gain").value, 2);
    });
  }
  bindPlanDrag();
  let resizeTimer = null;
  window.addEventListener("resize", () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => { drawPlan(); if (state.data) renderCharts(state.data.level, state.data.result); }, 100);
  });
}

bindUI();
load();
