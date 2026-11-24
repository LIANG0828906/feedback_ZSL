const SVG_NS = "http://www.w3.org/2000/svg";
const SCALE = 15;
const OX = 50;
const OY = 390;
const COLORS = ["#2563eb", "#dc2626", "#7c3aed", "#059669", "#d97706"];

let state = null;
let selectedWall = null;
let selectedDoor = null;
let selectedListener = "L1";
let drag = null;

const $ = (id) => document.getElementById(id);

function X(x) { return OX + x * SCALE; }
function Y(y) { return OY - y * SCALE; }
function worldFromEvent(evt) {
  const svg = $("floorSvg");
  const pt = svg.createSVGPoint();
  pt.x = evt.clientX;
  pt.y = evt.clientY;
  const p = pt.matrixTransform(svg.getScreenCTM().inverse());
  return { x: (p.x - OX) / SCALE, y: (OY - p.y) / SCALE };
}

function svgEl(tag, attrs = {}, parent) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (key === "text") node.textContent = value;
    else node.setAttribute(key, value);
  }
  if (parent) parent.appendChild(node);
  return node;
}

async function refresh() {
  const res = await fetch("/api/state");
  state = await res.json();
  renderAll();
}

async function postJSON(url, body = {}) {
  try {
    const res = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    const data = await res.json();
    if (data.state) {
      state = data.state;
      renderAll();
    } else {
      await refresh();
    }
    if (data.error) showError(data.error);
    else clearError();
    return data;
  } catch (err) {
    showError({ code: "E_HTTP", message: String(err) });
    return { ok: false, error: { code: "E_HTTP", message: String(err) } };
  }
}

function showError(err) {
  const banner = $("errorBanner");
  const object = err.object ? ` · 对象 ${err.object}` : "";
  const field = err.field ? ` · 字段 ${err.field}` : "";
  banner.textContent = `${err.code || "E_INTERNAL"}${object}${field}：${err.message || "未知错误"}`;
  banner.classList.remove("hidden");
}
function clearError() {
  $("errorBanner").classList.add("hidden");
}

function renderAll() {
  if (!state || !state.sample) return;
  fillSelectors();
  renderFloor();
  renderCurves();
  renderInvalidation();
  renderScheduler();
  renderProgress();
  renderDiff();
}

function renderProgress() {
  const p = state.progress || { committed: 0, total: 0 };
  $("progressText").textContent = `${p.committed}/${p.total}`;
  $("progressBar").style.width = p.total ? `${(p.committed / p.total) * 100}%` : "0%";
  $("commitSequence").textContent = "提交顺序: " + ((state.commitSequence || []).join(" → ") || "空");
}

function fillSelectors() {
  const listenerSelect = $("listenerSelect");
  const currentListener = selectedListener || listenerSelect.value;
  listenerSelect.innerHTML = "";
  for (const l of state.sample.listeners) {
    const opt = document.createElement("option");
    opt.value = l.id;
    opt.textContent = `${l.id} (${l.x},${l.y})`;
    listenerSelect.appendChild(opt);
  }
  selectedListener = state.sample.listeners.some((l) => l.id === currentListener)
    ? currentListener : state.sample.listeners[0]?.id;
  listenerSelect.value = selectedListener;

  const roomSelect = $("roomSelect");
  const currentRoom = roomSelect.value || state.sample.rooms[0]?.id;
  roomSelect.innerHTML = "";
  for (const room of state.sample.rooms) {
    const opt = document.createElement("option");
    opt.value = room.id;
    opt.textContent = room.id;
    roomSelect.appendChild(opt);
  }
  if (state.sample.rooms.some((r) => r.id === currentRoom)) roomSelect.value = currentRoom;
  fillRoomParams();
}

function fillRoomParams() {
  const id = $("roomSelect").value;
  const room = state.sample.rooms.find((r) => r.id === id);
  if (!room) return;
  $("rt60Input").value = room.params.rt60Ms;
  $("densityInput").value = room.params.earlyReflectionDensity;
  $("gainInput").value = room.params.gainDb;
}

function stateForRoom(roomID) {
  return (state.states || []).find((s) => s.roomId === roomID) || { status: "pending" };
}

function renderFloor() {
  const svg = $("floorSvg");
  svg.innerHTML = "";
  const s = state.sample;
  const reportRoomReasons = new Map();
  for (const item of state.lastInvalidation?.items || []) {
    if (item.kind === "room") reportRoomReasons.set(item.id, item);
  }

  for (const room of s.rooms) {
    const st = stateForRoom(room.id);
    const attrs = {
      x: X(room.rect.x1), y: Y(room.rect.y2),
      width: (room.rect.x2 - room.rect.x1) * SCALE,
      height: (room.rect.y2 - room.rect.y1) * SCALE,
      rx: 8, class: `room ${st.status}${st.stale ? " stale" : ""}`,
    };
    svgEl("rect", attrs, svg);
    const cx = (room.rect.x1 + room.rect.x2) / 2;
    const cy = (room.rect.y1 + room.rect.y2) / 2;
    svgEl("text", { x: X(cx), y: Y(cy) - 8, class: "room-label", text: room.id }, svg);
    svgEl("text", {
      x: X(cx), y: Y(cy) + 10, class: "room-param",
      text: `RT ${room.params.rt60Ms}ms · g ${room.params.gainDb}dB`,
    }, svg);
    const reportItem = reportRoomReasons.get(room.id);
    if (reportItem && !reportItem.reused) {
      svgEl("rect", {
        x: X(room.rect.x1) + 3, y: Y(room.rect.y2) + 3,
        width: (room.rect.x2 - room.rect.x1) * SCALE - 6,
        height: (room.rect.y2 - room.rect.y1) * SCALE - 6,
        rx: 7, fill: "none", stroke: "#f97316", "stroke-width": 2.5,
        "stroke-dasharray": "6 4", "pointer-events": "none",
      }, svg);
    }
  }

  if ($("togglePaths").checked) renderPaths(svg);

  for (const w of s.walls) {
    svgEl("line", { x1: X(w.x1), y1: Y(w.y1), x2: X(w.x2), y2: Y(w.y2), class: "wall" }, svg);
    const hit = svgEl("line", {
      x1: X(w.x1), y1: Y(w.y1), x2: X(w.x2), y2: Y(w.y2),
      class: `wall-hit${selectedWall === w.id ? " selected" : ""}`,
      "data-wall": w.id,
    }, svg);
    hit.addEventListener("click", () => {
      selectedWall = w.id;
      updateSelectionText();
      renderFloor();
    });
  }
  for (const o of s.openings) {
    svgEl("line", { x1: X(o.x1), y1: Y(o.y1), x2: X(o.x2), y2: Y(o.y2), class: "opening-gap" }, svg);
    const line = svgEl("line", {
      x1: X(o.x1), y1: Y(o.y1), x2: X(o.x2), y2: Y(o.y2),
      class: `opening${selectedDoor === o.id ? " selected" : ""}`,
      "data-door": o.id,
    }, svg);
    line.addEventListener("click", (evt) => {
      evt.stopPropagation();
      selectedDoor = o.id;
      selectedWall = o.wallId;
      updateSelectionText();
      renderFloor();
    });
  }

  for (const w of s.walls) {
    if (w.roomA && w.roomB) {
      const mx = (w.x1 + w.x2) / 2;
      const my = (w.y1 + w.y2) / 2;
      const vertical = w.x1 === w.x2;
      const handle = svgEl("circle", {
        cx: X(mx), cy: Y(my), r: 7, class: "handle",
        "data-wall-id": w.id, "data-axis": vertical ? "x" : "y",
      }, svg);
      handle.addEventListener("pointerdown", startDrag);
      handle.addEventListener("pointermove", moveDrag);
      handle.addEventListener("pointerup", finishDrag);
      handle.addEventListener("pointercancel", () => { drag = null; });
    }
  }

  for (const src of s.sources) {
    svgEl("circle", { cx: X(src.x), cy: Y(src.y), r: 7, class: "source" }, svg);
    svgEl("text", { x: X(src.x) + 10, y: Y(src.y) - 8, class: "point-label", text: src.id }, svg);
  }
  for (const l of s.listeners) {
    const points = listenerTriangle(l.x, l.y);
    svgEl("polygon", { points, class: "listener" }, svg);
    svgEl("text", { x: X(l.x) + 10, y: Y(l.y) + 14, class: "point-label", text: l.id }, svg);
  }
  updateSelectionText();
}

function listenerTriangle(x, y) {
  const cx = X(x), cy = Y(y), r = 8;
  return `${cx},${cy - r} ${cx - r},${cy + r * 0.75} ${cx + r},${cy + r * 0.75}`;
}

function renderPaths(svg) {
  const result = (state.results || []).find((r) => r.listenerId === selectedListener);
  if (!result) return;
  result.pairs.forEach((pair, i) => {
    if (pair.error || !pair.path?.polyline?.length) return;
    const d = pair.path.polyline.map((p, idx) => `${idx ? "L" : "M"} ${X(p.x)} ${Y(p.y)}`).join(" ");
    svgEl("path", { d, class: "path-line", stroke: COLORS[i % COLORS.length] }, svg);
    if ($("toggleOcclusion").checked) {
      for (const p of pair.path.occlusionPoints || []) {
        svgEl("text", { x: X(p.x), y: Y(p.y), class: "occlusion", text: "×" }, svg);
      }
    }
  });
}

function startDrag(evt) {
  evt.preventDefault();
  evt.stopPropagation();
  evt.target.setPointerCapture(evt.pointerId);
  drag = { wallId: evt.target.dataset.wallId, axis: evt.target.dataset.axis, value: null };
}
function moveDrag(evt) {
  if (!drag) return;
  const w = worldFromEvent(evt);
  drag.value = drag.axis === "x" ? Math.round(w.x) : Math.round(w.y);
}
async function finishDrag(evt) {
  if (!drag) return;
  const pending = drag;
  drag = null;
  if (pending.value === null) return;
  await postJSON("/api/boundary", { wallId: pending.wallId, value: pending.value });
}

function updateSelectionText() {
  $("selectionText").textContent = selectedDoor
    ? `已选门洞 ${selectedDoor}（墙 ${selectedWall || "?"}）`
    : selectedWall ? `已选墙段 ${selectedWall}` : "未选择墙/门洞";
}

function renderCurves() {
  const svg = $("curveSvg");
  svg.innerHTML = "";
  const result = (state.results || []).find((r) => r.listenerId === $("listenerSelect").value);
  selectedListener = $("listenerSelect").value;
  if (!result) {
    svgEl("text", { x: 210, y: 120, "text-anchor": "middle", fill: "#64748b", text: "结果尚未生成" }, svg);
    $("splList").innerHTML = "";
    return;
  }
  const pairs = result.pairs.filter((p) => !p.error && p.samples?.length);
  const allLevels = pairs.flatMap((p) => p.samples.map((q) => q.levelDb));
  if (!allLevels.length) return;
  let min = Math.floor(Math.min(...allLevels) / 10) * 10;
  let max = Math.ceil(Math.max(...allLevels) / 10) * 10;
  if (min === max) { min -= 10; max += 10; }
  const x0 = 48, y0 = 18, w = 350, h = 170;
  const maxT = Math.max(...pairs.flatMap((p) => p.samples.map((q) => q.tMs)), 1);
  const mx = (t) => x0 + (t / maxT) * w;
  const my = (v) => y0 + h - ((v - min) / (max - min)) * h;
  svgEl("line", { x1: x0, y1: y0 + h, x2: x0 + w, y2: y0 + h, stroke: "#94a3b8" }, svg);
  svgEl("line", { x1: x0, y1: y0, x2: x0, y2: y0 + h, stroke: "#94a3b8" }, svg);
  for (let tick = 0; tick <= 4; tick++) {
    const value = min + ((max - min) * tick) / 4;
    const y = my(value);
    svgEl("line", { x1: x0, y1: y, x2: x0 + w, y2: y, stroke: "#e2e8f0" }, svg);
    svgEl("text", { x: x0 - 6, y: y + 3, "text-anchor": "end", "font-size": 9, fill: "#64748b", text: value.toFixed(0) }, svg);
  }
  svgEl("text", { x: x0 + w / 2, y: 232, "text-anchor": "middle", "font-size": 11, fill: "#64748b", text: "t (ms)" }, svg);

  pairs.forEach((pair, i) => {
    const d = pair.samples.map((q, idx) => `${idx ? "L" : "M"} ${mx(q.tMs).toFixed(1)} ${my(q.levelDb).toFixed(1)}`).join(" ");
    svgEl("path", { d, fill: "none", stroke: COLORS[i % COLORS.length], "stroke-width": 2.2 }, svg);
  });

  const spl = $("splList");
  spl.innerHTML = "";
  result.pairs.forEach((pair, i) => {
    const pill = document.createElement("div");
    pill.className = "spl-pill";
    const color = COLORS[i % COLORS.length];
    if (pair.error) {
      pill.innerHTML = `<span style="color:${color};font-weight:900">●</span> ${pair.path?.sourceId || "?"} → ${result.listenerId}: <b>${pair.error.code}</b> ${pair.error.object || ""}`;
    } else {
      const chain = (pair.path.roomChain || []).join("→");
      pill.innerHTML = `<span style="color:${color};font-weight:900">●</span>
        ${pair.path.sourceId}→${result.listenerId}: ${chain}, L=${pair.path.length},
        SPL=<b>${pair.expectedSplDb.toFixed(2)}dB</b>,
        遮挡=${pair.path.occlusionCount},
        曲线真值=<span class="badge ${pair.curveMatch}">${pair.curveMatch ? "match" : "diff"}</span>`;
    }
    spl.appendChild(pill);
  });
}

function renderInvalidation() {
  const body = $("invalidationBody");
  body.innerHTML = "";
  const report = state.lastInvalidation;
  $("topoOrder").textContent = report?.topoOrder?.length ? "拓扑: " + report.topoOrder.slice(0, 18).join(" → ") + (report.topoOrder.length > 18 ? " …" : "") : "";
  if (!report) {
    body.innerHTML = `<tr><td colspan="6">暂无编辑；启动时已执行全量求解。</td></tr>`;
    return;
  }
  const items = [...(report.items || [])].sort((a, b) => {
    if (a.kind !== b.kind) return a.kind === "room" ? -1 : 1;
    return a.id.localeCompare(b.id);
  });
  for (const item of items) {
    const tr = document.createElement("tr");
    const reuseBadge = `<span class="badge ${item.reused}">${item.reused ? "复用" : "重算"}</span>`;
    tr.innerHTML = `<td>${item.kind}</td><td><code>${item.id}</code></td><td>${item.roomId || ""}</td>
      <td>${item.reason}</td><td>${item.status}</td><td>${reuseBadge}</td>`;
    body.appendChild(tr);
  }
}

function renderScheduler() {
  const d = state.scheduler || {};
  $("schedulerText").innerHTML = `
    <span>最大并行度 <b>${d.maxParallel}</b></span>
    <span>每采样缓冲上限 <b>${d.bufferSampleCap}</b></span>
    <span>活动任务 <b>${d.activeTasks}</b></span>
    <span>累计完成 <b>${d.completedTasks}</b></span>
    <span>恢复检查点 <b>${state.recovery?.recovered || 0}</b></span>
    <span>输入哈希 <b><code>${(state.inputHash || "").slice(0, 12)}</code></b></span>`;
  const qb = $("queueBody");
  qb.innerHTML = "";
  for (const ev of (d.queuedEvents || []).slice(-30).reverse()) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td><code>${ev.taskId}</code></td><td>${ev.reason}</td><td>${ev.waitMs}</td><td>${new Date(ev.atUnixMilli).toLocaleTimeString()}</td>`;
    qb.appendChild(tr);
  }
}

function renderDiff() {
  if (state.lastDiff) {
    const d = state.lastDiff;
    $("diffOutput").textContent = d.match
      ? `E_DIFF=none\n逐项一致：房间链、采样、SPL、提交顺序均相等。\nhash=${d.fullHash?.slice(0, 16)}`
      : `E_DIFF\n首个不一致: ${d.firstLocator}\nexpected=${JSON.stringify(d.expected)}\nactual=${JSON.stringify(d.actual)}`;
  }
}

function bindControls() {
  $("listenerSelect").addEventListener("change", () => {
    selectedListener = $("listenerSelect").value;
    renderFloor();
    renderCurves();
  });
  $("roomSelect").addEventListener("change", fillRoomParams);
  $("applyParamsBtn").addEventListener("click", () => {
    const roomId = $("roomSelect").value;
    postJSON("/api/rooms/params", {
      roomId,
      params: {
        rt60Ms: Number($("rt60Input").value),
        earlyReflectionDensity: Number($("densityInput").value),
        gainDb: Number($("gainInput").value),
      },
    });
  });
  $("addDoorBtn").addEventListener("click", () => {
    if (!selectedWall) return showError({ code: "E_SCHEMA", object: "selection", message: "请先点击墙段" });
    postJSON("/api/doors/add", { wallId: selectedWall, width: 2, kind: "door" })
      .then((data) => {
        if (data.report?.reason) selectedDoor = data.report.reason.split(":")[1];
      });
  });
  $("deleteDoorBtn").addEventListener("click", () => {
    if (!selectedDoor) return showError({ code: "E_SCHEMA", object: "selection", message: "请先点击门洞" });
    postJSON("/api/doors/delete", { doorId: selectedDoor }).then(() => { selectedDoor = null; });
  });
  $("fullBtn").addEventListener("click", () => postJSON("/api/recompute/full", { reset: true }));
  $("compareBtn").addEventListener("click", async () => {
    const data = await postJSON("/api/compare", {});
    if (data.diff) {
      state.lastDiff = data.diff;
      renderDiff();
    }
  });
  $("truthBtn").addEventListener("click", async () => {
    const data = await postJSON("/api/truth/verify", {});
    $("diffOutput").textContent = data.ok
      ? "解析真值校验通过：房间归属、房间链、长度、衰减采样与 SPL 均在声明容差内。"
      : `${data.error.code} ${data.error.object}: ${data.error.message}`;
  });
  $("resumeBtn").addEventListener("click", () => postJSON("/api/recompute/resume", {}));
  $("recoverBtn").addEventListener("click", () => postJSON("/api/checkpoint/recover", {}));
  $("badPrecisionBtn").addEventListener("click", () => {
    const p = { ...state.sample.precision, sampleCount: 1 };
    postJSON("/api/precision", p);
  });
  document.querySelectorAll("[data-inject]").forEach((btn) => {
    btn.addEventListener("click", () => postJSON("/api/inject", { type: btn.dataset.inject }));
  });
  $("togglePaths").addEventListener("change", renderFloor);
  $("toggleOcclusion").addEventListener("change", renderFloor);
}

bindControls();
refresh().catch(showError);
