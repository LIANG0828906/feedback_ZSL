const CELL = 22;
const PALETTE = ["#55d7ff", "#4dd0a8", "#ffb454", "#c792ea", "#ff7b88", "#8bd17c", "#f7d774", "#66a8ff"];

const ui = {
  state: null,
  selected: null,
  selectedMember: null,
  error: "",
};

const $ = (id) => document.getElementById(id);

async function api(method, ...args) {
  const app = window.go && window.go.main && window.go.main.App;
  if (!app || typeof app[method] !== "function") {
    throw new Error("Wails binding is not ready: main.App." + method);
  }
  return await app[method](...args);
}

function showError(err) {
  ui.error = err ? String(err.message || err) : "";
  const banner = $("errorBanner");
  banner.textContent = ui.error;
  banner.classList.toggle("hidden", !ui.error);
}

async function refresh() {
  try {
    setBusy(true);
    ui.state = await api("GetState");
    showError("");
    render();
  } catch (err) {
    showError(err);
  } finally {
    setBusy(false);
  }
}

async function applyCost() {
  if (!ui.selected || !ui.state) return;
  const cost = Number($("costInput").value);
  try {
    setBusy(true);
    const response = await api("SetCellCost", ui.selected.x, ui.selected.y, cost);
    ui.state = response.state;
    showError("");
    render();
  } catch (err) {
    showError(err);
    await refresh();
  } finally {
    setBusy(false);
  }
}

async function toggleRegion(region) {
  try {
    setBusy(true);
    const response = await api("SetRegionActive", region.id, !region.active);
    ui.state = response.state;
    showError("");
    render();
  } catch (err) {
    showError(err);
    await refresh();
  } finally {
    setBusy(false);
  }
}

async function retryMember(memberID) {
  try {
    setBusy(true);
    ui.state = await api("RetryMember", memberID);
    showError("");
    render();
  } catch (err) {
    showError(err);
    await refresh();
  } finally {
    setBusy(false);
  }
}

async function togglePause() {
  if (!ui.state) return;
  try {
    setBusy(true);
    ui.state = ui.state.paused ? await api("Resume") : await api("Pause");
    showError("");
    render();
  } catch (err) {
    showError(err);
  } finally {
    setBusy(false);
  }
}

async function restart() {
  try {
    setBusy(true);
    ui.state = await api("Restart");
    showError("");
    render();
  } catch (err) {
    showError(err);
  } finally {
    setBusy(false);
  }
}

function setBusy(busy) {
  document.querySelectorAll("button").forEach((button) => {
    button.disabled = busy;
  });
  if (!busy && ui.state) {
    $("pauseBtn").disabled = false;
  }
}

function render() {
  if (!ui.state || !ui.state.effective_map || !ui.state.snapshot) return;
  const state = ui.state;
  const snapshot = state.snapshot;
  $("mapTitle").textContent = `${state.effective_map.id} · ${state.effective_map.width}×${state.effective_map.height}`;
  $("statusPill").textContent = state.paused ? "已暂停 · 检查点已持久化" : "运行中 · SQLite 已同步";
  $("statusPill").className = "pill " + (state.paused ? "paused" : "online");
  $("pauseBtn").textContent = state.paused ? "继续" : "暂停";
  $("modeBadge").textContent = snapshot.stats.mode || "full";
  $("cacheHits").textContent = snapshot.stats.cache_hits ?? 0;
  $("cacheMisses").textContent = snapshot.stats.cache_misses ?? 0;
  $("reusedIntervals").textContent = snapshot.stats.reused_intervals ?? 0;
  $("commitSeq").textContent = snapshot.commit_seq ?? 0;
  $("mapHash").textContent = state.map_hash || "-";
  $("totalMicros").textContent = `${((snapshot.stats.total_micros || 0) / 1000).toFixed(2)} ms`;
  $("algorithmVersion").textContent = snapshot.algorithm_version || "-";
  $("memberCount").textContent = state.scenario.formation.members.length;
  renderMembers();
  renderInvalidation();
  renderRegions();
  drawMap();
}

function renderMembers() {
  const state = ui.state;
  const snapshot = state.snapshot;
  const list = $("memberList");
  list.innerHTML = "";
  const invalid = new Set((snapshot.invalidation || []).map((entry) => entry.member_id));
  const timings = new Map((snapshot.stats.timings || []).map((timing) => [timing.member_id, timing]));
  state.scenario.formation.members.forEach((member, index) => {
    const path = snapshot.paths[member.id] || {};
    const smooth = snapshot.smooth[member.id] || { corners: [] };
    const trace = snapshot.traces[member.id] || { states: [], stats: {} };
    const timing = timings.get(member.id) || {};
    const timingText = `${(((timing.path_micros || 0) + (timing.smooth_micros || 0) + (timing.trace_micros || 0)) / 1000).toFixed(2)} ms`;
    const row = document.createElement("div");
    row.className = "member" + (ui.selectedMember === member.id ? " selected" : "") + (invalid.has(member.id) ? " invalid" : "");
    row.innerHTML = `
      <i class="member-dot" style="background:${memberColor(index)}"></i>
      <div class="member-main">
        <b>${escapeHTML(member.id)} · ${escapeHTML(member.callsign)}</b>
        <span>${escapeHTML(member.group)} / ${escapeHTML(member.slot_id)}</span>
      </div>
      <small>cost ${path.cost ?? "-"} · ${smooth.corners.length} corners<br>${trace.states.length} ticks · dev ${trace.stats.max_deviation_milli ?? 0}<br>recalc ${timingText}<br><button class="mini-retry" title="重新计算该成员">重试</button></small>
    `;
    row.addEventListener("click", () => {
      ui.selectedMember = member.id;
      render();
    });
    row.addEventListener("dblclick", () => retryMember(member.id));
    row.querySelector(".mini-retry").addEventListener("click", (event) => {
      event.stopPropagation();
      retryMember(member.id);
    });
    list.appendChild(row);
  });
}

function renderInvalidation() {
  const entries = (ui.state.snapshot.invalidation || []).slice();
  $("invalidationCount").textContent = entries.length;
  const list = $("invalidationList");
  list.innerHTML = "";
  if (!entries.length) {
    list.innerHTML = '<div class="empty">当前快照无失效区间</div>';
    return;
  }
  entries.forEach((entry) => {
    const item = document.createElement("div");
    item.className = "invalidation-item " + entry.kind;
    item.innerHTML = `<b>${escapeHTML(entry.member_id)} · ${escapeHTML(entry.kind)}</b>
      <span>ticks ${entry.interval.start}–${entry.interval.end} · ${escapeHTML(entry.reason)}</span>`;
    list.appendChild(item);
  });
}

function renderRegions() {
  const map = ui.state.scenario.map;
  const list = $("regionList");
  list.innerHTML = "";
  map.blocked_regions.forEach((region) => {
    const row = document.createElement("div");
    row.className = "region";
    row.innerHTML = `<span class="${region.active ? "active-lock" : "inactive-lock"}">
      ${escapeHTML(region.id)} · ${region.x},${region.y} · ${region.width}×${region.height}
    </span>`;
    const button = document.createElement("button");
    button.textContent = region.active ? "解除" : "封锁";
    button.addEventListener("click", () => toggleRegion(region));
    row.appendChild(button);
    list.appendChild(row);
  });
}

function drawMap() {
  const state = ui.state;
  const map = state.effective_map;
  const snapshot = state.snapshot;
  const canvas = $("mapCanvas");
  const dpr = window.devicePixelRatio || 1;
  canvas.width = map.width * CELL * dpr;
  canvas.height = map.height * CELL * dpr;
  canvas.style.width = `${map.width * CELL}px`;
  canvas.style.height = `${map.height * CELL}px`;
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, map.width * CELL, map.height * CELL);

  const maxCost = Math.max(...map.cells.filter((cell) => cell.passable).map((cell) => cell.cost), 1);
  for (let y = 0; y < map.height; y++) {
    for (let x = 0; x < map.width; x++) {
      const cell = map.cells[y * map.width + x];
      ctx.fillStyle = cellColor(cell, maxCost);
      ctx.fillRect(x * CELL, y * CELL, CELL, CELL);
      ctx.strokeStyle = "rgba(4, 9, 12, 0.42)";
      ctx.lineWidth = 1;
      ctx.strokeRect(x * CELL + 0.5, y * CELL + 0.5, CELL - 1, CELL - 1);
      if (!cell.passable) drawBlocked(ctx, x, y);
      if (cell.cost > 1 && cell.passable && CELL >= 20) {
        ctx.fillStyle = "rgba(230, 242, 246, 0.58)";
        ctx.font = "9px Cascadia Mono, monospace";
        ctx.textAlign = "center";
        ctx.textBaseline = "middle";
        ctx.fillText(String(cell.cost), x * CELL + CELL / 2, y * CELL + CELL / 2);
      }
    }
  }

  const invalid = new Set((snapshot.invalidation || []).map((entry) => entry.member_id));
  state.scenario.formation.members.forEach((member, index) => {
    const smooth = snapshot.smooth[member.id];
    if (!smooth || !smooth.corners.length) return;
    const selected = ui.selectedMember === member.id;
    ctx.strokeStyle = invalid.has(member.id) ? "#ff5f6d" : memberColor(index);
    ctx.globalAlpha = ui.selectedMember && !selected ? 0.46 : 1;
    ctx.lineWidth = selected ? 3 : 2;
    ctx.beginPath();
    smooth.corners.forEach((corner, i) => {
      const px = (corner.fixed.x / 1000 + 0.5) * CELL;
      const py = (corner.fixed.y / 1000 + 0.5) * CELL;
      if (i === 0) ctx.moveTo(px, py);
      else ctx.lineTo(px, py);
    });
    ctx.stroke();
    smooth.corners.forEach((corner) => {
      const px = (corner.fixed.x / 1000 + 0.5) * CELL;
      const py = (corner.fixed.y / 1000 + 0.5) * CELL;
      ctx.fillStyle = invalid.has(member.id) ? "#ff5f6d" : memberColor(index);
      ctx.beginPath();
      ctx.arc(px, py, selected ? 4 : 3, 0, Math.PI * 2);
      ctx.fill();
    });
    const trace = snapshot.traces[member.id];
    if (selected && trace) {
      ctx.globalAlpha = 0.55;
      trace.states.forEach((tick, tickIndex) => {
        if (tickIndex % 5 !== 0) return;
        const px = (tick.position.x / 1000 + 0.5) * CELL;
        const py = (tick.position.y / 1000 + 0.5) * CELL;
        ctx.fillStyle = "#ffffff";
        ctx.fillRect(px - 1, py - 1, 2, 2);
      });
    }
    ctx.globalAlpha = 1;

    const slot = state.scenario.formation.slots.find((candidate) => candidate.id === member.slot_id);
    if (slot) {
      const sx = (member.start.x + slot.dx + 0.5) * CELL;
      const sy = (member.start.y + slot.dy + 0.5) * CELL;
      ctx.strokeStyle = memberColor(index);
      ctx.lineWidth = 1;
      ctx.strokeRect(sx - 4, sy - 4, 8, 8);
    }
  });

  if (ui.selected) {
    ctx.strokeStyle = "#ffffff";
    ctx.lineWidth = 2;
    ctx.strokeRect(ui.selected.x * CELL + 1, ui.selected.y * CELL + 1, CELL - 2, CELL - 2);
  }
  if (state.last_edit && state.last_edit.invalidation) {
    state.last_edit.invalidation.changed_cells.forEach((change) => {
      ctx.strokeStyle = "rgba(255, 180, 84, 0.9)";
      ctx.lineWidth = 1;
      ctx.strokeRect(change.point.x * CELL + 1, change.point.y * CELL + 1, CELL - 2, CELL - 2);
    });
  }
}

function cellColor(cell, maxCost) {
  if (!cell.passable) return "#2a1418";
  const t = Math.min(1, Math.max(0, (cell.cost - 1) / Math.max(1, maxCost - 1)));
  if (cell.terrain === "road") return blend("#2d4b3d", "#58735d", t);
  if (cell.terrain === "urban") return blend("#4e432e", "#927b4e", t);
  if (cell.terrain === "forest") return blend("#17352d", "#2c5949", t);
  if (cell.terrain === "marsh") return blend("#183b42", "#326771", t);
  return blend("#202b31", "#50636b", t);
}

function drawBlocked(ctx, x, y) {
  ctx.fillStyle = "rgba(204, 47, 64, 0.55)";
  ctx.fillRect(x * CELL, y * CELL, CELL, CELL);
  ctx.strokeStyle = "rgba(255, 118, 130, 0.72)";
  ctx.beginPath();
  ctx.moveTo(x * CELL + 4, y * CELL + CELL - 5);
  ctx.lineTo(x * CELL + CELL - 5, y * CELL + 4);
  ctx.stroke();
}

function blend(a, b, t) {
  const av = parseInt(a.slice(1), 16), bv = parseInt(b.slice(1), 16);
  const ar = av >> 16, ag = (av >> 8) & 255, ab = av & 255;
  const br = bv >> 16, bg = (bv >> 8) & 255, bb = bv & 255;
  return `rgb(${Math.round(ar + (br - ar) * t)},${Math.round(ag + (bg - ag) * t)},${Math.round(ab + (bb - ab) * t)})`;
}

function memberColor(index) {
  return PALETTE[index % PALETTE.length];
}

function escapeHTML(value) {
  return String(value)
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

function bindCanvas() {
  const canvas = $("mapCanvas");
  canvas.addEventListener("click", async (event) => {
    if (!ui.state) return;
    const rect = canvas.getBoundingClientRect();
    const x = Math.floor(((event.clientX - rect.left) / rect.width) * ui.state.effective_map.width);
    const y = Math.floor(((event.clientY - rect.top) / rect.height) * ui.state.effective_map.height);
    const cell = ui.state.effective_map.cells[y * ui.state.effective_map.width + x];
    ui.selected = { x, y };
    $("cellReadout").textContent = `格 (${x},${y}) · ${cell.terrain} · cost ${cell.cost} · ${cell.passable ? "可通行" : "封锁"}`;
    $("costInput").value = cell.cost || 1;
    if (event.shiftKey) {
      const region = ui.state.scenario.map.blocked_regions.find((candidate) =>
        x >= candidate.x && x < candidate.x + candidate.width &&
        y >= candidate.y && y < candidate.y + candidate.height);
      if (region) {
        await toggleRegion(region);
        return;
      }
    }
    drawMap();
  });
}

async function boot() {
  bindCanvas();
  $("refreshBtn").addEventListener("click", refresh);
  $("pauseBtn").addEventListener("click", togglePause);
  $("restartBtn").addEventListener("click", restart);
  $("applyCostBtn").addEventListener("click", applyCost);
  for (let attempt = 0; attempt < 80; attempt++) {
    try {
      await refresh();
      return;
    } catch (err) {
      await new Promise((resolve) => setTimeout(resolve, 150));
    }
  }
  showError("无法连接 Wails 绑定。请确认桌面应用已正确构建。");
}

boot();
