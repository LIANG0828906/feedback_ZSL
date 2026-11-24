const state = {
  view: null,
  comparison: null,
  drag: null,
  transform: null,
  toastTimer: null,
  errorLog: [],
};

const $ = (id) => document.getElementById(id);
const colors = ["#4fd1b5", "#66a9ff", "#f0b35a", "#b28cff", "#ed6a6a"];
const terrainColors = ["#476d42", "#2d563a", "#69655e", "#725793"];

function fmt(value, digits = 4) {
  if (value === null || value === undefined || Number.isNaN(value)) return "—";
  if (typeof value === "number" && Math.abs(value) >= 1000) return value.toFixed(0);
  return Number(value).toFixed(digits).replace(/0+$/, "").replace(/\.$/, "");
}

function api() {
  return window.go && window.go.main && window.go.main.App;
}

async function call(method, ...args) {
  const binding = api();
  if (!binding) {
    showError({ code: "E_RUNTIME", message: "Wails runtime is unavailable. Launch with `wails dev -tags wails`." });
    return null;
  }
  try {
    const response = await binding[method](...args);
    if (!response) throw new Error("empty response");
    if (response.error) showError(response.error);
    if (response.view) {
      state.view = response.view;
      renderAll();
    }
    if (response.comparison) {
      state.comparison = response.comparison;
      renderCompare();
    }
    if (response.message && !response.error) showToast(response.message, true);
    return response;
  } catch (error) {
    showError({ code: "E_RUNTIME", message: error.message || String(error) });
    return null;
  }
}

function initialize() {
  return call("Initialize", $("region-path").value.trim(), $("db-path").value.trim());
}

function showToast(message, ok = false) {
  const toast = $("toast");
  toast.textContent = message;
  toast.classList.remove("hidden");
  toast.classList.toggle("ok", ok);
  clearTimeout(state.toastTimer);
  state.toastTimer = setTimeout(() => toast.classList.add("hidden"), 5200);
}

function showError(error) {
  state.errorLog.push(error);
  const bits = [error.code || "E_RUNTIME", error.message || "unknown error"];
  if (error.field) bits.push(`field=${error.field}`);
  if (error.object_id) bits.push(`object=${error.object_id}`);
  if (error.zone_id) bits.push(`zone=${error.zone_id}`);
  if (error.data_item) bits.push(`item=${error.data_item}`);
  if (error.point) bits.push(`point=(${fmt(error.point.x, 2)}, ${fmt(error.point.y, 2)})`);
  if (error.value !== undefined) bits.push(`value=${fmt(error.value, 6)}`);
  showToast(bits.join(" · "), false);
}

function resizeCanvas(canvas) {
  const ratio = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  if (canvas.width !== Math.round(rect.width * ratio) || canvas.height !== Math.round(rect.height * ratio)) {
    canvas.width = Math.max(1, Math.round(rect.width * ratio));
    canvas.height = Math.max(1, Math.round(rect.height * ratio));
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  return { ctx, width: rect.width, height: rect.height };
}

function drawMap() {
  const canvas = $("map-canvas");
  const { ctx, width, height } = resizeCanvas(canvas);
  ctx.clearRect(0, 0, width, height);
  ctx.fillStyle = "#091017";
  ctx.fillRect(0, 0, width, height);
  const view = state.view;
  if (!view) {
    ctx.fillStyle = "#91a3b5";
    ctx.font = "14px Inter, sans-serif";
    ctx.fillText("Load a workspace to begin.", 28, 42);
    return;
  }
  const pad = 32;
  const scale = Math.min((width - pad * 2) / (view.bounds.max_x - view.bounds.min_x), (height - pad * 2) / (view.bounds.max_y - view.bounds.min_y));
  const ox = (width - (view.bounds.max_x - view.bounds.min_x) * scale) / 2;
  const oy = (height - (view.bounds.max_y - view.bounds.min_y) * scale) / 2;
  const toScreen = (x, y) => [ox + (x - view.bounds.min_x) * scale, height - oy - (y - view.bounds.min_y) * scale];
  const toWorld = (sx, sy) => [view.bounds.min_x + (sx - ox) / scale, view.bounds.min_y + (height - oy - sy) / scale];
  state.transform = { scale, ox, oy, width, height, toScreen, toWorld };

  const raster = view.terrain_raster;
  for (let row = 0; row < raster.rows; row++) {
    for (let col = 0; col < raster.cols; col++) {
      const idx = row * raster.cols + col;
      const type = raster.values[idx];
      const x0 = view.bounds.min_x + col * raster.cell_size;
      const y0 = view.bounds.min_y + row * raster.cell_size;
      const [sx, sy] = toScreen(x0, y0 + raster.cell_size);
      ctx.fillStyle = terrainColors[type % terrainColors.length] || "#445";
      ctx.globalAlpha = 0.27;
      ctx.fillRect(sx, sy, raster.cell_size * scale + 0.4, raster.cell_size * scale + 0.4);
    }
  }
  ctx.globalAlpha = 1;

  if ($("show-heat").checked) {
    const heat = view.heatmap;
    let max = 0;
    for (const value of heat.values) max = Math.max(max, value);
    $("heat-max").textContent = max > 0 ? fmt(max, 3) : "0";
    for (let row = 0; row < heat.rows; row++) {
      for (let col = 0; col < heat.cols; col++) {
        const value = heat.values[row * heat.cols + col] || 0;
        if (value <= 0 || max <= 0) continue;
        const x0 = heat.bounds.min_x + col * heat.cell_size;
        const y0 = heat.bounds.min_y + row * heat.cell_size;
        const [sx, sy] = toScreen(x0, y0 + heat.cell_size);
        ctx.fillStyle = heatColor(value / max);
        ctx.fillRect(sx, sy, heat.cell_size * scale + 0.3, heat.cell_size * scale + 0.3);
      }
    }
  }

  view.zones.forEach((zone, index) => {
    drawPolygon(ctx, zone.polygon, toScreen, hexAlpha(colors[index % colors.length], 0.12), hexAlpha(colors[index % colors.length], 0.8), 1.4);
  });

  if ($("show-points").checked) {
    ctx.save();
    view.points.forEach((point) => {
      const [sx, sy] = toScreen(point.x, point.y);
      ctx.fillStyle = point.rarity === "rare" ? "#ed6a6a" : point.rarity === "uncommon" ? "#f0b35a" : "#f5d28a";
      ctx.globalAlpha = point.rarity === "common" ? 0.66 : 0.9;
      ctx.beginPath();
      ctx.arc(sx, sy, point.rarity === "rare" ? 1.8 : 1.25, 0, Math.PI * 2);
      ctx.fill();
    });
    ctx.restore();
  }

  view.no_collect.forEach((area) => {
    let polygon = area.polygon;
    if (state.drag && state.drag.id === area.id) polygon = state.drag.preview;
    drawPolygon(ctx, polygon, toScreen, "rgba(237,106,106,0.18)", "rgba(237,106,106,0.95)", 1.4, true);
  });

  if ($("show-samples").checked) {
    view.sample_sites.forEach((site) => {
      const [sx, sy] = toScreen(site.x, site.y);
      ctx.fillStyle = "#66a9ff";
      ctx.strokeStyle = "#cfe7ff";
      ctx.lineWidth = 0.8;
      ctx.beginPath();
      ctx.arc(sx, sy, 3.1, 0, Math.PI * 2);
      ctx.fill();
      ctx.stroke();
    });
  }

  drawPolygon(ctx, view.boundary, toScreen, "transparent", "#9bb4c8", 1.5);
  view.zones.forEach((zone, index) => {
    const cx = zone.polygon.reduce((sum, p) => sum + p.x, 0) / Math.max(1, zone.polygon.length - 1);
    const cy = zone.polygon.reduce((sum, p) => sum + p.y, 0) / Math.max(1, zone.polygon.length - 1);
    const [sx, sy] = toScreen(cx, cy);
    ctx.font = "600 10px Inter, sans-serif";
    ctx.textAlign = "center";
    ctx.fillStyle = colors[index % colors.length];
    ctx.fillText(zone.id, sx, sy);
    ctx.font = "9px Inter, sans-serif";
    ctx.fillStyle = "#cbd8e4";
    ctx.fillText(`${fmt(zone.estimated_density, 3)} density`, sx, sy + 12);
  });

  $("map-meta").textContent = `${view.region_id} · ${view.points.length} points · ${view.sample_sites.length} sample sites`;
}

function drawPolygon(ctx, polygon, toScreen, fill, stroke, width, hatch = false) {
  if (!polygon || polygon.length < 3) return;
  ctx.beginPath();
  polygon.forEach((point, index) => {
    const [sx, sy] = toScreen(point.x, point.y);
    if (index === 0) ctx.moveTo(sx, sy);
    else ctx.lineTo(sx, sy);
  });
  ctx.closePath();
  ctx.fillStyle = fill;
  ctx.fill();
  if (hatch) {
    ctx.save();
    ctx.clip();
    ctx.strokeStyle = "rgba(237,106,106,0.32)";
    ctx.lineWidth = 1;
    for (let x = -ctx.canvas.width; x < ctx.canvas.width * 2; x += 9) {
      ctx.beginPath();
      ctx.moveTo(x, 0);
      ctx.lineTo(x + ctx.canvas.height, ctx.canvas.height);
      ctx.stroke();
    }
    ctx.restore();
  }
  ctx.strokeStyle = stroke;
  ctx.lineWidth = width;
  ctx.stroke();
}

function heatColor(t) {
  const stops = [
    [24, 51, 74, 0.05],
    [61, 141, 119, 0.2],
    [228, 189, 90, 0.34],
    [240, 113, 78, 0.48],
  ];
  const index = Math.min(stops.length - 2, Math.floor(t * (stops.length - 1)));
  const local = t * (stops.length - 1) - index;
  const a = stops[index], b = stops[index + 1];
  const value = a.map((v, i) => v + (b[i] - v) * local);
  return `rgba(${value[0] | 0},${value[1] | 0},${value[2] | 0},${value[3]})`;
}

function hexAlpha(hex, alpha) {
  const value = hex.replace("#", "");
  const r = parseInt(value.slice(0, 2), 16);
  const g = parseInt(value.slice(2, 4), 16);
  const b = parseInt(value.slice(4, 6), 16);
  return `rgba(${r},${g},${b},${alpha})`;
}

function drawBias() {
  const canvas = $("bias-canvas");
  const { ctx, width, height } = resizeCanvas(canvas);
  ctx.clearRect(0, 0, width, height);
  const zones = state.view ? state.view.bias_report.zones || [] : [];
  if (!zones.length) {
    ctx.fillStyle = "#91a3b5";
    ctx.font = "12px Inter, sans-serif";
    ctx.fillText("No bias observations yet.", 18, 30);
    return;
  }
  const series = [
    { key: "density_quantization", color: "#66a9ff" },
    { key: "standard_error", color: "#4fd1b5" },
    { key: "rarity_rounding", color: "#b28cff" },
    { key: "heatmap", color: "#f0b35a" },
  ];
  let max = 0;
  zones.forEach((zone) => series.forEach((s) => { max = Math.max(max, zone.cumulative[s.key] || 0); }));
  max = Math.max(max, 1e-6);
  const left = 46, right = 12, top = 12, bottom = 29;
  const plotW = width - left - right;
  const plotH = height - top - bottom;
  ctx.strokeStyle = "#2a3b4c";
  ctx.lineWidth = 1;
  ctx.font = "9px Inter, sans-serif";
  ctx.fillStyle = "#91a3b5";
  for (let i = 0; i <= 4; i++) {
    const y = top + (plotH * i) / 4;
    ctx.beginPath();
    ctx.moveTo(left, y);
    ctx.lineTo(width - right, y);
    ctx.stroke();
    ctx.fillText(fmt(max * (1 - i / 4), 4), 2, y + 3);
  }
  const xAt = (i) => left + (zones.length === 1 ? plotW / 2 : (plotW * i) / (zones.length - 1));
  const yAt = (v) => top + plotH - (v / max) * plotH;
  zones.forEach((zone, index) => {
    ctx.fillStyle = "#91a3b5";
    ctx.textAlign = "center";
    ctx.fillText(zone.zone_id.replace("zone-", ""), xAt(index), height - 10);
  });
  series.forEach((item) => {
    ctx.strokeStyle = item.color;
    ctx.lineWidth = 1.75;
    ctx.beginPath();
    zones.forEach((zone, index) => {
      const x = xAt(index);
      const y = yAt(zone.cumulative[item.key] || 0);
      if (index === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();
    zones.forEach((zone, index) => {
      ctx.fillStyle = item.color;
      ctx.beginPath();
      ctx.arc(xAt(index), yAt(zone.cumulative[item.key] || 0), 2.7, 0, Math.PI * 2);
      ctx.fill();
    });
  });
  ctx.textAlign = "left";
}

function renderStats() {
  const view = state.view;
  if (!view) {
    $("stats").innerHTML = `<div class="stat"><b>—</b><span>workspace</span></div>`;
    return;
  }
  const reused = view.zones.filter((zone) => zone.cache_state === "reused").length;
  const recomputed = view.zones.filter((zone) => zone.cache_state === "recomputed").length;
  const snapshot = view.snapshot.enabled ? (view.snapshot.invalidated ? "invalidated" : view.snapshot.restored ? "restored" : "fresh") : "off";
  const stats = [
    [view.points.length, "reconstructed points"],
    [recomputed, "recomputed zones"],
    [reused, "cache hits"],
    [snapshot, "snapshot"],
    [view.worker.max_parallel, "worker limit"],
    [view.worker.buffer_events ? view.worker.buffer_events.length : 0, "buffer blocks"],
  ];
  $("stats").innerHTML = stats.map(([value, label]) => `<div class="stat"><b>${value}</b><span>${label}</span></div>`).join("");
  $("run-mode").textContent = view.mode;
  $("run-mode").className = "badge " + (view.bias_report.passed ? "pass" : "fail");
  $("bias-status").textContent = view.bias_report.passed ? "within threshold" : `E_BIAS · ${view.bias_report.first_violation_zone}`;
  $("bias-status").className = "badge " + (view.bias_report.passed ? "pass" : "fail");
  $("dependency-meta").textContent = `${view.dependency_graph.topology_order.length} zones · serial commit order`;
  $("subtitle").textContent = `${view.display_name || view.region_id} · deterministic density reconstruction`;
}

function renderZoneControls() {
  const select = $("zone-select");
  const previous = select.value;
  const zones = state.view ? state.view.zones : [];
  select.innerHTML = zones.map((zone) => `<option value="${zone.id}">${zone.id}</option>`).join("");
  if (previous && zones.some((zone) => zone.id === previous)) select.value = previous;
  updateZoneForm();
}

function updateZoneForm() {
  const zone = (state.view ? state.view.zones : []).find((item) => item.id === $("zone-select").value);
  if (!zone) return;
  $("density-limit").value = zone.density_limit;
  const estimated = zone.rarity_mix || {};
  const override = zone.rarity_override || null;
  $("rarity-mix").value = override ? Object.entries(override).map(([key, value]) => `${key}=${fmt(value, 4)}`).join(",") : "";
  $("rarity-mix").placeholder = `observed: ${Object.entries(estimated).map(([key, value]) => `${key}=${fmt(value, 4)}`).join(",")}`;
}

function parseMix(text) {
  const mix = {};
  text.split(",").map((item) => item.trim()).filter(Boolean).forEach((item) => {
    const [key, value] = item.split("=");
    const parsed = Number(value);
    if (!key || !Number.isFinite(parsed)) throw new Error(`invalid rarity mix item: ${item}`);
    mix[key.trim()] = parsed;
  });
  return mix;
}

async function applyZoneEdit() {
  const zoneID = $("zone-select").value;
  if (!zoneID) return;
  const density = Number($("density-limit").value);
  const request = { zone_id: zoneID, density_limit: density };
  if (!$("clear-rarity").checked && $("rarity-mix").value.trim()) {
    try {
      request.rarity_mix = parseMix($("rarity-mix").value);
    } catch (error) {
      showError({ code: "E_SCHEMA", field: "rarity_mix", message: error.message });
      return;
    }
  }
  if ($("clear-rarity").checked) request.clear_rarity_override = true;
  await call("EditZone", request);
}

function renderInvalidations() {
  const entries = state.view ? state.view.invalidations || [] : [];
  $("invalidations").innerHTML = entries.map((entry) => `
    <div class="trace-item ${entry.invalidated ? "recompute" : "reuse"}">
      <b>${entry.zone_id}</b>
      <p>${entry.invalidated ? "INVALIDATED" : "REUSED"} · ${(entry.reasons || []).join(", ")}</p>
    </div>`).join("") || `<p class="muted">No trace yet.</p>`;
}

function renderDependency() {
  const view = state.view;
  if (!view) {
    $("dependency").innerHTML = "";
    return;
  }
  const entries = new Map((view.invalidations || []).map((entry) => [entry.zone_id, entry]));
  const zones = view.zones;
  $("dependency").innerHTML = zones.map((zone) => {
    const entry = entries.get(zone.id) || {};
    const downstream = entry.downstream ? `<span class="chip">downstream</span>` : "";
    const reasons = (entry.reasons || []).map((reason) => `<span class="chip">${reason}</span>`).join("");
    return `
      <div class="zone-node ${entry.invalidated ? "recompute" : "reuse"}">
        <h3>${zone.id}</h3>
        <p>density → sample → estimate → reconstruction → heat</p>
        <div class="stage-chips">${downstream}${reasons}</div>
      </div>`;
  }).join("");
}

function renderZoneTable() {
  const zones = state.view ? state.view.zones : [];
  $("zone-table").innerHTML = zones.map((zone) => `
    <tr>
      <td>${zone.id}</td>
      <td>${fmt(zone.estimated_density, 5)}</td>
      <td>${fmt(zone.standard_error, 5)}</td>
      <td>${zone.sample_count}</td>
      <td>${fmt(zone.effective_area, 1)}</td>
      <td>${zone.point_count}</td>
      <td class="state-${zone.cache_state}">${zone.cache_state || "—"}</td>
      <td>${zone.generation}</td>
      <td><button data-retry="${zone.id}">retry</button></td>
    </tr>`).join("");
  document.querySelectorAll("[data-retry]").forEach((button) => {
    button.addEventListener("click", () => call("RetryZone", button.dataset.retry));
  });
}

function renderCompare() {
  if (!state.comparison) return;
  const c = state.comparison;
  $("compare-result").textContent = c.equal
    ? `incremental == full · ${c.checked_items} checks`
    : `E_DIVERGE · ${c.first_zone_id || "global"} · ${c.first_data_item}`;
}

function renderAll() {
  renderStats();
  renderZoneControls();
  renderInvalidations();
  renderDependency();
  renderZoneTable();
  renderCompare();
  drawMap();
  drawBias();
}

function beginDrag(event) {
  if (!$("drag-nocollect").checked || !state.view || !state.transform) return;
  const rect = $("map-canvas").getBoundingClientRect();
  const sx = event.clientX - rect.left;
  const sy = event.clientY - rect.top;
  const [wx, wy] = state.transform.toWorld(sx, sy);
  const hit = state.view.no_collect.find((area) => pointInPolygon(wx, wy, area.polygon));
  if (!hit) return;
  state.drag = {
    id: hit.id,
    startX: wx,
    startY: wy,
    origin: hit.polygon.map((point) => ({ ...point })),
    preview: hit.polygon.map((point) => ({ ...point })),
    moved: false,
  };
  $("map-canvas").classList.add("dragging");
}

function moveDrag(event) {
  if (!state.drag || !state.transform) return;
  const rect = $("map-canvas").getBoundingClientRect();
  const [wx, wy] = state.transform.toWorld(event.clientX - rect.left, event.clientY - rect.top);
  const dx = wx - state.drag.startX;
  const dy = wy - state.drag.startY;
  state.drag.preview = state.drag.origin.map((point) => ({ x: point.x + dx, y: point.y + dy }));
  state.drag.moved = Math.abs(dx) + Math.abs(dy) > 0.05;
  drawMap();
}

async function endDrag() {
  if (!state.drag) return;
  const drag = state.drag;
  state.drag = null;
  $("map-canvas").classList.remove("dragging");
  if (!drag.moved) {
    drawMap();
    return;
  }
  await call("EditNoCollect", { id: drag.id, polygon: drag.preview });
  drawMap();
}

function pointInPolygon(x, y, polygon) {
  let inside = false;
  for (let i = 0, j = polygon.length - 1; i < polygon.length; j = i++) {
    const a = polygon[j], b = polygon[i];
    if ((a.y > y) !== (b.y > y) && x < ((b.x - a.x) * (y - a.y)) / (b.y - a.y) + a.x) inside = !inside;
  }
  return inside;
}

async function exportReport() {
  const response = await call("ExportReport");
  if (!response || !response.report) return;
  const blob = new Blob([response.report], { type: "application/json" });
  const link = document.createElement("a");
  link.href = URL.createObjectURL(blob);
  link.download = `resource-workbench-${state.view ? state.view.region_id : "report"}.json`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(link.href), 2000);
}

function setup() {
  $("btn-load").addEventListener("click", initialize);
  $("btn-refresh").addEventListener("click", () => call("Refresh"));
  $("btn-compare").addEventListener("click", () => call("CompareFull"));
  $("btn-export").addEventListener("click", exportReport);
  $("btn-apply-zone").addEventListener("click", applyZoneEdit);
  $("btn-retry-zone").addEventListener("click", () => {
    if ($("zone-select").value) call("RetryZone", $("zone-select").value);
  });
  $("zone-select").addEventListener("change", updateZoneForm);
  ["show-heat", "show-points", "show-samples"].forEach((id) => $(id).addEventListener("change", drawMap));
  const canvas = $("map-canvas");
  canvas.addEventListener("mousedown", beginDrag);
  window.addEventListener("mousemove", moveDrag);
  window.addEventListener("mouseup", endDrag);
  window.addEventListener("resize", () => { drawMap(); drawBias(); });
  const boot = (attempt = 0) => {
    if (api()) {
      initialize();
      return;
    }
    if (attempt < 25) setTimeout(() => boot(attempt + 1), 200);
    else initialize();
  };
  setTimeout(boot, 120);
}

document.addEventListener("DOMContentLoaded", setup);
