const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));

const ZONE_COLORS = ["#67d4d0", "#b8dc63", "#e9b45d", "#ae91df", "#ef8c72", "#6ea8e8"];
const TERRAIN_COLORS = {
  forest: "#17352e",
  meadow: "#2c3c21",
  ridge: "#31343b",
  marsh: "#17343a",
};
const RARITY_COLORS = {
  common: "#b8dc63",
  uncommon: "#67d4d0",
  rare: "#ae91df",
};

const state = {
  catalog: [],
  snapshot: null,
  selectedZone: null,
  comparison: null,
  busy: false,
  layers: {
    terrain: true, heat: true, zones: true, samples: true, points: true, exclusions: true,
  },
};

const bridge = () => window.go?.main?.App;

document.addEventListener("DOMContentLoaded", async () => {
  bindEvents();
  if (!bridge()) {
    $("#status-message").textContent = "未检测到 Wails 运行时；请通过 wails dev/build 启动桌面应用。";
    $("#engine-status").classList.add("err");
    $("#engine-status span").textContent = "NO BRIDGE";
    return;
  }
  try {
    const catalog = await bridge().Catalog();
    state.catalog = catalog.data || [];
    fillRegionOptions();
  } catch (err) {
    toast("读取区域目录失败：" + err, "error");
  }
  await runBusy("生成确定性样本", "构造地形、观测值、隐藏真值并准备密度模型…", async () => {
    const response = await bridge().Bootstrap();
    if (!response.ok) {
      handleFailure(response);
      return;
    }
    setSnapshot(response.data);
  });
});

function bindEvents() {
  $("#region-select").addEventListener("change", async (event) => {
    const seed = Number(event.target.value);
    await invokeSnapshot("LoadRegion", [seed], "加载区域", "从固定种子重新生成区域与观测…");
  });
  $$("#layer-switches input").forEach((input) => {
    input.addEventListener("change", () => {
      state.layers[input.dataset.layer] = input.checked;
      renderMap();
    });
  });
  $("#btn-full").addEventListener("click", () => invokeSnapshot("FullRebuild", [], "全量重建", "清空地带缓存并从空缓存重建…"));
  $("#btn-compare").addEventListener("click", compareIncremental);
  $("#btn-export").addEventListener("click", exportWorkspace);
  $("#btn-save-sample").addEventListener("click", saveSample);
  $("#btn-import").addEventListener("click", () => $("#scenario-file").click());
  $("#scenario-file").addEventListener("change", importScenario);
  $("#btn-cancel").addEventListener("click", cancelCurrent);
  $("#busy-cancel").addEventListener("click", cancelCurrent);
  $("#btn-apply-zone").addEventListener("click", applyZoneParams);
  $("#btn-retry-zone").addEventListener("click", retryZone);
  $("#btn-shrink-zone").addEventListener("click", shrinkZoneBoundary);
  $("#btn-move-exclusion").addEventListener("click", moveExclusion);
  $("#only-changed").addEventListener("change", renderInvalidation);

  $("#density-scale").addEventListener("input", (event) => {
    $("#density-output").textContent = Number(event.target.value).toFixed(2);
  });
  $("#rarity-shift").addEventListener("input", (event) => {
    $("#rarity-output").textContent = Number(event.target.value).toFixed(2);
  });

  $("#zone-list").addEventListener("click", async (event) => {
    const retry = event.target.closest("[data-retry-zone]");
    if (retry) {
      event.stopPropagation();
      state.selectedZone = retry.dataset.retryZone;
      renderAll();
      await invokeSnapshot("RetryZone", [state.selectedZone], "逐地带重试", "仅重算所选地带及其下游缓存…");
      return;
    }
    const card = event.target.closest("[data-zone]");
    if (card) {
      state.selectedZone = card.dataset.zone;
      renderAll();
    }
  });

  $("#map-canvas").addEventListener("click", (event) => {
    if (!state.snapshot) return;
    const rect = event.currentTarget.getBoundingClientRect();
    const view = mapView(rect.width, rect.height);
    const x = view.minX + (event.clientX - rect.left - view.ox) / view.scale;
    const y = view.maxY - (event.clientY - rect.top - view.oy) / view.scale;
    const zone = state.snapshot.zones.find((z) => pointInPolygon({ x, y }, z.polygon.vertices));
    if (zone) {
      state.selectedZone = zone.id;
      renderAll();
    }
  });

  $$(".tab").forEach((tab) => {
    tab.addEventListener("click", () => {
      $$(".tab").forEach((item) => item.classList.toggle("active", item === tab));
      $$(".tab-panel").forEach((panel) => panel.classList.remove("active"));
      $(`#tab-${tab.dataset.tab}`).classList.add("active");
    });
  });

  let resizeTimer = 0;
  window.addEventListener("resize", () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(renderMap, 60);
  });
}

function fillRegionOptions() {
  const select = $("#region-select");
  select.innerHTML = state.catalog.map((region) =>
    `<option value="${region.seed}">${escapeHTML(region.name)} · ${escapeHTML(region.profile)} · ${region.seed}</option>`
  ).join("");
}

async function invokeSnapshot(method, args, title, detail) {
  await runBusy(title, detail, async () => {
    const response = await bridge()[method](...args);
    if (!response.ok) {
      handleFailure(response);
      return;
    }
    setSnapshot(response.data);
    toast(`${title}完成`, "success");
  });
}

async function runBusy(title, detail, fn) {
  if (state.busy) {
    toast("已有任务运行中", "warn");
    return;
  }
  setBusy(true, title, detail);
  try {
    await fn();
  } catch (err) {
    toast(String(err), "error");
  } finally {
    setBusy(false);
  }
}

function setBusy(busy, title = "正在计算", detail = "") {
  state.busy = busy;
  $("#busy-overlay").classList.toggle("show", busy);
  $("#map-loader").classList.toggle("show", busy && !!state.snapshot);
  $("#busy-title").textContent = title;
  $("#busy-detail").textContent = detail;
  $$("button").forEach((button) => {
    if (button.id !== "busy-cancel") button.disabled = busy;
  });
}

async function cancelCurrent() {
  if (!bridge()) return;
  try {
    const response = await bridge().Cancel();
    if (response.ok) {
      toast("已请求取消；已提交地带检查点将保留", "warn");
    }
  } catch (err) {
    toast("取消失败：" + err, "error");
  }
}

function setSnapshot(snapshot) {
  if (!snapshot) return;
  state.snapshot = snapshot;
  if (!snapshot.zones.some((zone) => zone.id === state.selectedZone)) {
    state.selectedZone = snapshot.zones[0]?.id || null;
  }
  const region = state.catalog.find((item) => item.seed === snapshot.seed);
  if (region) $("#region-select").value = String(region.seed);
  renderAll();
}

function renderAll() {
  if (!state.snapshot) return;
  renderHeader();
  renderZones();
  renderExclusions();
  renderInspector();
  renderMap();
  renderGraph();
  renderBounds();
  renderInvalidation();
  renderReport();
  renderLayers();
  renderCheckpoint();
}

function renderHeader() {
  const snap = state.snapshot;
  $("#engine-status").classList.add("ok");
  $("#engine-status span").textContent = "模型已加载";
  $("#seed-value").textContent = snap.seed;
  $("#algorithm-value").textContent = snap.algorithmVersion;
  $("#map-region").textContent = `${snap.regionId} / ${snap.zones.length} strata`;
  $("#map-bounds").textContent = `${fmt(snap.bounds.minX)},${fmt(snap.bounds.minY)} → ${fmt(snap.bounds.maxX)},${fmt(snap.bounds.maxY)} · ${snap.heatmap.width}×${snap.heatmap.height}`;
  $("#status-message").textContent = snap.invalidation?.explanation || "确定性模型已加载";
  $("#status-revision").textContent = `rev ${snap.revision}`;
  const t = snap.tolerances;
  $("#status-tolerance").textContent = `Δρ≤${t.heatmapAbs} · rarity L1≤${t.rarityL1} · count±${t.reconstructionCountPercent}%`;
}

function renderZones() {
  const snap = state.snapshot;
  $("#zone-counter").textContent = snap.zones.length;
  const list = $("#zone-list");
  list.innerHTML = snap.zones.map((zone, index) => {
    const zoneState = stateFor(zone.id);
    const color = ZONE_COLORS[index % ZONE_COLORS.length];
    const rarity = zoneState?.estimate?.rarity || {};
    return `
      <article class="zone-card ${zone.id === state.selectedZone ? "active" : ""}" data-zone="${escapeAttr(zone.id)}" style="--zone-color:${color}">
        <div class="zone-card-head">
          <h3>${escapeHTML(zone.name)}</h3>
          <button class="zone-retry" data-retry-zone="${escapeAttr(zone.id)}" title="逐地带重试">↻</button>
        </div>
        <p><b>${fmt(zoneState?.estimate?.density, 4)}</b> 点/面积 · <b>${zoneState?.points?.length || 0}</b> 重建点</p>
        <p>rare ${pct(rarity.rare)} · uncommon ${pct(rarity.uncommon)} · area ${fmt(zoneState?.estimate?.effectiveArea, 0)}</p>
      </article>`;
  }).join("");
}

function renderExclusions() {
  const snap = state.snapshot;
  $("#exclusion-list").innerHTML = (snap.exclusions || []).map((exclusion) => `
    <div class="exclusion-item"><b>${escapeHTML(exclusion.name)}</b>${escapeHTML(exclusion.reason)}</div>
  `).join("");
  const select = $("#exclusion-select");
  const previous = select.value;
  select.innerHTML = (snap.exclusions || []).map((exclusion) =>
    `<option value="${escapeAttr(exclusion.id)}">${escapeHTML(exclusion.name)} / ${escapeHTML(exclusion.reason)}</option>`
  ).join("");
  if (snap.exclusions?.some((item) => item.id === previous)) select.value = previous;
}

function renderInspector() {
  const zone = state.snapshot.zones.find((item) => item.id === state.selectedZone);
  const zoneState = stateFor(state.selectedZone);
  if (!zone || !zoneState) {
    $("#inspector-title").textContent = "选择地带";
    $("#inspector-token").textContent = "—";
    return;
  }
  $("#inspector-title").textContent = zone.name;
  $("#inspector-token").textContent = zone.id;
  $("#metric-density").textContent = fmt(zoneState.estimate.density, 4);
  $("#metric-samples").textContent = zoneState.samples.length;
  $("#metric-points").textContent = zoneState.points.length;
  $("#metric-area").textContent = fmt(zoneState.estimate.effectiveArea, 0);
  $("#param-version").textContent = (zoneState.paramsVersion || "").slice(0, 10);
  $("#density-scale").value = zone.params.densityScale;
  $("#rarity-shift").value = zone.params.rarityShift;
  $("#density-output").textContent = Number(zone.params.densityScale).toFixed(2);
  $("#rarity-output").textContent = Number(zone.params.rarityShift).toFixed(2);
}

function currentLayerRarity(stateValue, terrain) {
  return stateValue?.estimate?.layers?.find((layer) => layer.terrain === terrain)?.rarity || {};
}

function renderMap() {
  const canvas = $("#map-canvas");
  const snap = state.snapshot;
  if (!canvas || !snap) return;
  const rect = canvas.getBoundingClientRect();
  if (rect.width < 2 || rect.height < 2) return;
  const dpr = window.devicePixelRatio || 1;
  canvas.width = Math.max(1, Math.floor(rect.width * dpr));
  canvas.height = Math.max(1, Math.floor(rect.height * dpr));
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, rect.width, rect.height);
  const view = mapView(rect.width, rect.height);
  const mapX = (x) => view.ox + (x - view.minX) * view.scale;
  const mapY = (y) => view.oy + (view.maxY - y) * view.scale;
  const cellW = Math.max(1, snap.heatmap.cellSize * view.scale);
  const cellH = cellW;

  ctx.fillStyle = "#081013";
  ctx.fillRect(0, 0, rect.width, rect.height);
  drawWorldGrid(ctx, rect, view);

  if (state.layers.terrain) {
    const raster = snap.terrain;
    const terrainW = raster.cellSize * view.scale;
    const terrainH = terrainW;
    for (let row = 0; row < raster.height; row++) {
      for (let col = 0; col < raster.width; col++) {
        const type = raster.types[row * raster.width + col] || "meadow";
        ctx.fillStyle = TERRAIN_COLORS[type] || "#24302c";
        ctx.fillRect(view.ox + col * terrainW, view.oy + (raster.height - row - 1) * terrainH, terrainW + 0.35, terrainH + 0.35);
      }
    }
  }

  if (state.layers.heat) {
    const heat = snap.heatmap;
    const max = Math.max(0.0001, ...heat.values);
    for (let row = 0; row < heat.height; row++) {
      for (let col = 0; col < heat.width; col++) {
        const value = heat.values[row * heat.width + col];
        if (value <= 0) continue;
        ctx.fillStyle = heatColor(value, max, 0.76);
        ctx.fillRect(view.ox + col * cellW, view.oy + (heat.height - row - 1) * cellH, cellW + 0.4, cellH + 0.4);
      }
    }
  }

  if (state.layers.zones) {
    snap.zones.forEach((zone, index) => {
      const color = ZONE_COLORS[index % ZONE_COLORS.length];
      tracePolygon(ctx, zone.polygon.vertices, mapX, mapY);
      ctx.fillStyle = hexAlpha(color, zone.id === state.selectedZone ? 0.15 : 0.055);
      ctx.fill();
      ctx.strokeStyle = hexAlpha(color, zone.id === state.selectedZone ? 0.98 : 0.56);
      ctx.lineWidth = zone.id === state.selectedZone ? 2 : 1;
      ctx.stroke();
      const centroid = polygonCentroid(zone.polygon.vertices);
      ctx.font = "600 10px Segoe UI, Microsoft YaHei UI";
      ctx.fillStyle = hexAlpha(color, 0.98);
      ctx.fillText(zone.name, mapX(centroid.x) + 5, mapY(centroid.y) - 5);
    });
  }

  if (state.layers.exclusions) {
    (snap.exclusions || []).forEach((exclusion) => {
      tracePolygon(ctx, exclusion.polygon.vertices, mapX, mapY);
      ctx.fillStyle = "rgba(7, 12, 14, 0.72)";
      ctx.fill();
      ctx.save();
      ctx.clip();
      ctx.strokeStyle = "rgba(239,119,107,.32)";
      ctx.lineWidth = 1;
      for (let i = -rect.height; i < rect.width + rect.height; i += 8) {
        ctx.beginPath();
        ctx.moveTo(i, 0);
        ctx.lineTo(i - rect.height, rect.height);
        ctx.stroke();
      }
      ctx.restore();
      tracePolygon(ctx, exclusion.polygon.vertices, mapX, mapY);
      ctx.strokeStyle = "rgba(239,119,107,.82)";
      ctx.lineWidth = 1.2;
      ctx.setLineDash([3, 3]);
      ctx.stroke();
      ctx.setLineDash([]);
    });
  }

  if (state.layers.samples) {
    for (const zoneState of snap.zoneStates) {
      for (const sample of zoneState.samples) {
        ctx.beginPath();
        ctx.arc(mapX(sample.x), mapY(sample.y), 2.25, 0, Math.PI * 2);
        ctx.fillStyle = "rgba(8,15,17,.92)";
        ctx.fill();
        ctx.strokeStyle = "#e9b45d";
        ctx.lineWidth = 1.2;
        ctx.stroke();
      }
    }
  }

  if (state.layers.points) {
    for (const point of snap.points) {
      ctx.beginPath();
      ctx.arc(mapX(point.x), mapY(point.y), 1.35, 0, Math.PI * 2);
      ctx.fillStyle = RARITY_COLORS[point.rarity] || RARITY_COLORS.common;
      ctx.globalAlpha = point.rarity === "rare" ? 0.98 : 0.72;
      ctx.fill();
    }
    ctx.globalAlpha = 1;
  }

  ctx.strokeStyle = "rgba(103,212,208,.42)";
  ctx.lineWidth = 1;
  tracePolygon(ctx, snap.boundary.vertices, mapX, mapY);
  ctx.stroke();
}

function drawWorldGrid(ctx, rect, view) {
  ctx.strokeStyle = "rgba(97,130,135,.08)";
  ctx.lineWidth = 1;
  const step = 20;
  for (let x = Math.ceil(view.minX / step) * step; x <= view.maxX; x += step) {
    const px = view.ox + (x - view.minX) * view.scale;
    ctx.beginPath();
    ctx.moveTo(px, view.oy);
    ctx.lineTo(px, view.oy + (view.maxY - view.minY) * view.scale);
    ctx.stroke();
  }
  for (let y = Math.ceil(view.minY / step) * step; y <= view.maxY; y += step) {
    const py = view.oy + (view.maxY - y) * view.scale;
    ctx.beginPath();
    ctx.moveTo(view.ox, py);
    ctx.lineTo(view.ox + (view.maxX - view.minX) * view.scale, py);
    ctx.stroke();
  }
}

function mapView(width, height) {
  const b = state.snapshot.bounds;
  const pad = 24;
  const scale = Math.min((width - pad * 2) / (b.maxX - b.minX), (height - pad * 2) / (b.maxY - b.minY));
  const ox = (width - (b.maxX - b.minX) * scale) / 2;
  const oy = (height - (b.maxY - b.minY) * scale) / 2;
  return { ...b, scale, ox, oy };
}

function tracePolygon(ctx, vertices, mapX, mapY) {
  ctx.beginPath();
  vertices.forEach((point, index) => {
    if (index === 0) ctx.moveTo(mapX(point.x), mapY(point.y));
    else ctx.lineTo(mapX(point.x), mapY(point.y));
  });
  ctx.closePath();
}

function renderGraph() {
  const snap = state.snapshot;
  const svg = $("#graph-svg");
  const affected = new Set(snap.invalidation?.affectedNodes || []);
  const parts = [];
  parts.push(`<defs><marker id="arrow" markerWidth="6" markerHeight="6" refX="5" refY="3" orient="auto"><path d="M0,0 L6,3 L0,6 z" fill="#34505a"/></marker></defs>`);
  const exclusions = snap.exclusions || [];
  exclusions.slice(0, 5).forEach((exclusion, index) => {
    const y = 50 + index * 48;
    parts.push(nodeBox(22, y, 112, 28, exclusion.name, affected.has(`exclusion:${exclusion.id}`), "#ef776b", 8));
    parts.push(`<path d="M134,${y + 14} C154,${y + 14} 150,70 170,70" fill="none" stroke="#34505a" stroke-width=".8" marker-end="url(#arrow)"/>`);
  });
  const rows = snap.zones.map((zone, index) => {
    const y = 44 + index * 66;
    const nodes = [
      { id: `zone:${zone.id}:params`, label: "参数", x: 170, w: 85 },
      { id: `zone:${zone.id}:sampling`, label: "分层采样", x: 320, w: 95 },
      { id: `zone:${zone.id}:estimate`, label: "密度估计", x: 480, w: 95 },
      { id: `zone:${zone.id}:points`, label: "重建点", x: 640, w: 85 },
    ];
    nodes.forEach((node, i) => {
      parts.push(nodeBox(node.x, y, node.w, 30, node.label, affected.has(node.id), ZONE_COLORS[index % ZONE_COLORS.length], 16));
      if (i > 0) {
        const px = nodes[i - 1].x + nodes[i - 1].w;
        parts.push(`<path d="M${px},${y + 15} L${node.x - 2},${y + 15}" fill="none" stroke="#34505a" stroke-width=".9" marker-end="url(#arrow)"/>`);
      }
    });
    parts.push(`<text x="12" y="${y + 19}" fill="#819699" font-size="9">${escapeSVG(zone.name)}</text>`);
    return y;
  });
  // Heatmap tile matrix; color reflects the concrete invalidation manifest.
  const entries = snap.invalidation?.entries || [];
  const tileMap = new Map();
  for (const entry of entries) {
    const key = `${entry.tileX}:${entry.tileY}`;
    const current = tileMap.get(key) || { recomputed: 0, reused: 0 };
    current.recomputed += entry.recomputedPixels;
    current.reused += entry.reusedPixels;
    tileMap.set(key, current);
  }
  const tilesX = Math.ceil(snap.heatmap.width / snap.heatmap.tileSize);
  const tilesY = Math.ceil(snap.heatmap.height / snap.heatmap.tileSize);
  const tileW = 14;
  const tileH = 11;
  const startX = 750;
  const startY = 48;
  for (let ty = 0; ty < tilesY; ty++) {
    for (let tx = 0; tx < tilesX; tx++) {
      const stats = tileMap.get(`${tx}:${ty}`) || { recomputed: 0, reused: 0 };
      const changed = affected.has(`heat:tile:${tx}:${ty}`) || stats.recomputed > 0;
      const fill = changed ? (stats.reused > 0 ? "#9b7338" : "#8d3e3e") : "#1f454a";
      const stroke = changed ? "#e9b45d" : "#2b545b";
      parts.push(`<rect x="${startX + tx * (tileW + 2)}" y="${startY + ty * (tileH + 3)}" width="${tileW}" height="${tileH}" rx="2" fill="${fill}" stroke="${stroke}" stroke-width=".7"/>`);
    }
  }
  parts.push(`<text x="${startX}" y="34" fill="#819699" font-size="9">热力面分块 ${tilesX}×${tilesY}</text>`);
  parts.push(`<rect x="${startX}" y="214" width="10" height="8" fill="#8d3e3e"/><text x="${startX + 15}" y="221" fill="#566b6f" font-size="8">重算</text>`);
  parts.push(`<rect x="${startX + 60}" y="214" width="10" height="8" fill="#9b7338"/><text x="${startX + 75}" y="221" fill="#566b6f" font-size="8">部分复用</text>`);
  parts.push(`<rect x="${startX + 145}" y="214" width="10" height="8" fill="#1f454a"/><text x="${startX + 160}" y="221" fill="#566b6f" font-size="8">复用</text>`);
  svg.innerHTML = parts.join("");
  const inv = snap.invalidation;
  $("#reuse-stat").textContent = inv ? `复用 ${inv.reusedPixels} px · 重算 ${inv.recomputedPixels} px` : "尚未运行";
}

function nodeBox(x, y, w, h, label, hot, color, fontSize) {
  return `<g>
    <rect x="${x}" y="${y}" width="${w}" height="${h}" rx="5" fill="${hot ? "rgba(239,119,107,.17)" : "rgba(255,255,255,.025)"}" stroke="${hot ? "#ef776b" : color}" stroke-opacity="${hot ? "1" : ".42"}" stroke-width="${hot ? "1.2" : ".8"}"/>
    <text x="${x + w / 2}" y="${y + h / 2 + 3}" fill="${hot ? "#ff9b91" : "#a8bbbb"}" text-anchor="middle" font-size="${fontSize}">${escapeSVG(label)}</text>
  </g>`;
}

function renderBounds() {
  const report = state.snapshot.report?.zones || [];
  const configs = [
    { label: "量化上界", bound: "cumulativeQuantizationBound", actual: "cumulativeQuantizationError", color: "#67d4d0" },
    { label: "标准误上界", bound: "cumulativeStandardErrorBound", actual: "cumulativeStandardError", color: "#b8dc63" },
    { label: "稀有度上界", bound: "cumulativeRarityBound", actual: "cumulativeRarityResidual", color: "#ae91df" },
    { label: "热力面上界", bound: "cumulativeHeatmapBound", actual: "cumulativeHeatmapError", color: "#e9b45d" },
  ];
  $("#bound-charts").innerHTML = configs.map((config) => {
    const bounds = report.map((zone) => Number(zone[config.bound] || 0));
    const actuals = report.map((zone) => Number(zone[config.actual] || 0));
    const max = Math.max(0.0001, ...bounds, ...actuals);
    const points = bounds.map((value, index) => chartPoint(index, value, bounds.length, max)).join(" ");
    const actualPoints = actuals.map((value, index) => chartPoint(index, value, actuals.length, max)).join(" ");
    return `<div class="bound-chart">
      <div class="bound-label"><span>${config.label}</span><b style="color:${config.color}">${fmt(bounds.at(-1), 4)}</b></div>
      <svg viewBox="0 0 180 52" preserveAspectRatio="none">
        <path d="M2 49 H178" stroke="#22343b" stroke-width=".7"/>
        <polyline points="${actualPoints}" fill="none" stroke="#566b6f" stroke-width="1" stroke-dasharray="2,2"/>
        <polyline points="${points}" fill="none" stroke="${config.color}" stroke-width="1.5"/>
        ${bounds.map((value, index) => {
          const [x, y] = chartPoint(index, value, bounds.length, max).split(",");
          return `<circle cx="${x}" cy="${y}" r="1.7" fill="${config.color}"/>`;
        }).join("")}
      </svg>
    </div>`;
  }).join("");
}

function chartPoint(index, value, count, max) {
  const x = 6 + (count <= 1 ? 0 : index / (count - 1)) * 168;
  const y = 47 - (Math.max(0, value) / max) * 43;
  return `${x.toFixed(1)},${y.toFixed(1)}`;
}

function renderInvalidation() {
  const inv = state.snapshot.invalidation;
  if (!inv) {
    $("#invalidation-summary").textContent = "尚未运行重建";
    $("#invalidation-table").innerHTML = "";
    return;
  }
  const onlyChanged = $("#only-changed").checked;
  let entries = inv.entries || [];
  if (onlyChanged) entries = entries.filter((entry) => entry.recomputedPixels > 0);
  $("#invalidation-summary").textContent =
    `受影响地带 ${inv.affectedZones.length} · 分块 ${inv.affectedTileCount} 失效 / ${inv.reusedTileCount} 复用 · 像素 ${inv.reusedPixels} 复用 / ${inv.recomputedPixels} 重算`;
  const shown = entries.slice(0, 260);
  const rows = shown.map((entry) => `
    <tr>
      <td><b>${escapeHTML(zoneName(entry.zoneId))}</b></td>
      <td>${entry.tileX},${entry.tileY}</td>
      <td><span class="state-pill ${entry.reuseState}">${entry.reuseState}</span></td>
      <td>${escapeHTML(entry.reason)}</td>
      <td>${entry.reusedPixels}</td>
      <td>${entry.recomputedPixels}</td>
    </tr>`).join("");
  const suffix = entries.length > shown.length ? `<tr><td colspan="6">另有 ${entries.length - shown.length} 条，完整清单可通过导出查看。</td></tr>` : "";
  $("#invalidation-table").innerHTML = `
    <thead><tr><th>地带</th><th>分块 X,Y</th><th>复用状态</th><th>失效原因</th><th>复用像素</th><th>重算像素</th></tr></thead>
    <tbody>${rows}${suffix}</tbody>`;
}

function renderReport() {
  const rows = (state.snapshot.report?.zones || []).map((zone) => {
    const boundTitle = Object.entries(zone.bounds || {}).map(([key, bound]) =>
      `${key}: ${bound.formula} | ${JSON.stringify(bound.inputs)}`).join("\n");
    return `<tr>
      <td><b>${escapeHTML(zoneName(zone.zoneId))}</b></td>
      <td>${zone.sampleCount}</td>
      <td>${fmt(zone.area, 0)}</td>
      <td>${fmt(zone.quantizationError, 6)}</td>
      <td>${fmt(zone.standardError, 6)}</td>
      <td>${fmt(zone.rarityRoundingResidual, 6)}</td>
      <td>${fmt(zone.heatmapReconstructionError, 6)}</td>
      <td>${fmt(zone.cumulativeQuantizationBound, 6)}</td>
      <td>${fmt(zone.cumulativeStandardErrorBound, 6)}</td>
      <td>${fmt(zone.cumulativeRarityBound, 6)}</td>
      <td>${fmt(zone.cumulativeHeatmapBound, 6)}</td>
      <td title="${escapeAttr(boundTitle)}">4 项 · hover 查看输入</td>
    </tr>`;
  }).join("");
  $("#report-table").innerHTML = `
    <thead><tr>
      <th>地带</th><th>n</th><th>面积</th><th>量化误差</th><th>标准误</th><th>稀有度残差</th><th>热力面误差</th>
      <th>累计量化上界</th><th>累计标准误上界</th><th>累计稀有度上界</th><th>累计热力面上界</th><th>推导输入</th>
    </tr></thead><tbody>${rows}</tbody>`;
}

function renderLayers() {
  const rows = [];
  for (const zoneState of state.snapshot.zoneStates) {
    for (const layer of zoneState.estimate.layers || []) {
      rows.push(`<tr>
        <td><b>${escapeHTML(zoneName(zoneState.zoneId))}</b></td>
        <td>${escapeHTML(layer.terrain)}</td>
        <td>${layer.sampleCount}</td>
        <td>${fmt(layer.area, 0)}</td>
        <td>${fmt(layer.density, 6)}</td>
        <td>${fmt(layer.standardError, 6)}</td>
        <td>${fmt(layer.rarity?.rare, 4)}</td>
      </tr>`);
    }
  }
  $("#layer-table").innerHTML = `<thead><tr><th>地带</th><th>地形层</th><th>采样数</th><th>有效面积</th><th>密度</th><th>层内标准误</th><th>稀有比例</th></tr></thead><tbody>${rows.join("")}</tbody>`;
}

function renderCheckpoint() {
  const snap = state.snapshot;
  const cp = snap.checkpoint;
  $("#checkpoint-state").textContent = cp ? "已提交" : "未启用";
  $("#checkpoint-session").textContent = cp?.sessionId || "—";
  $("#checkpoint-count").textContent = cp ? `${cp.committedZones}/${snap.zones.length}` : "—";
  const recovery = snap.recovery;
  const note = $("#recovery-note");
  if (recovery && (recovery.source !== "none" || recovery.discarded)) {
    note.classList.add("show");
    note.textContent = recovery.discarded
      ? `快照作废：${recovery.discardReason || "校验失败"}。已执行全量重建。`
      : `恢复来源：${recovery.source}；复用 ${recovery.resumedZones?.length || 0} 个地带检查点。`;
  } else {
    note.classList.remove("show");
  }
  const metrics = snap.metrics || {};
  $("#run-workers").textContent = metrics.maxParallelism ?? "—";
  $("#run-buffer").textContent = metrics.perWorkerBufferSize ?? "—";
  $("#run-tasks").textContent = metrics.tasks ?? "—";
  $("#run-blocked").textContent = metrics.bufferBlockEvents ?? "—";
  $("#run-reason").textContent = metrics.lastBlockReason || "无阻塞";
  $("#run-session-state").textContent = recovery?.snapshotStatus || "—";
  $("#run-algorithm").textContent = snap.algorithmVersion;
  $("#run-recovery").textContent = recovery?.source || "none";
  $("#run-discard").textContent = recovery?.discarded ? recovery.discardReason : "完整有效";
}

async function applyZoneParams() {
  if (!state.selectedZone) return;
  const req = {
    zoneId: state.selectedZone,
    densityScale: Number($("#density-scale").value),
    rarityShift: Number($("#rarity-shift").value),
  };
  await invokeSnapshot("UpdateZone", [req], "增量重算", "仅失效所选地带及其下游分量…");
}

async function retryZone() {
  if (!state.selectedZone) return;
  await invokeSnapshot("RetryZone", [state.selectedZone], "逐地带重试", "重建所选地带的采样、估计、点集与热力像素…");
}

async function shrinkZoneBoundary() {
  const zone = state.snapshot?.zones?.find((item) => item.id === state.selectedZone);
  if (!zone) return;
  const vertices = zone.polygon.vertices;
  const centroid = polygonCentroid(vertices);
  const factor = 0.97;
  const polygon = {
    vertices: vertices.map((point) => ({
      x: centroid.x + (point.x - centroid.x) * factor,
      y: centroid.y + (point.y - centroid.y) * factor,
    })),
  };
  await invokeSnapshot("UpdateZoneBoundary", [{ zoneId: zone.id, polygon }], "边界编辑", "仅失效该地带的采样、估计、点集与热力分块…");
}

async function moveExclusion() {
  const req = {
    exclusionId: $("#exclusion-select").value,
    dx: Number($("#exclusion-dx").value || 0),
    dy: Number($("#exclusion-dy").value || 0),
  };
  if (!req.exclusionId || (req.dx === 0 && req.dy === 0)) {
    toast("请选择禁采区并输入非零位移", "warn");
    return;
  }
  await invokeSnapshot("MoveExclusion", [req], "禁采区拖动", "传播到实际相交地带并逐像素复用其余热力面…");
}

async function compareIncremental() {
  await runBusy("增量 / 全量比对", "从空缓存独立重建并逐点、逐像素、逐缓存键比较…", async () => {
    const response = await bridge().Compare();
    if (!response.ok) {
      if (response.code === "E_DIFF" && response.data?.result?.mismatch) {
        const mismatch = response.data.result.mismatch;
        const location = mismatch.kind === "heatmap-pixel"
          ? `像素 (${mismatch.pixelX},${mismatch.pixelY}) / ${zoneName(mismatch.zoneId)}`
          : `序号 ${mismatch.index} / ${zoneName(mismatch.zoneId)}`;
        toast(`E_DIFF 首个不一致：${location}`, "error");
      } else {
        handleFailure(response);
      }
      return;
    }
    state.comparison = response.data.result;
    toast(`逐项一致：${response.data.result.pointCount} 点、${response.data.result.pixelCount} 像素、${response.data.result.zoneCount} 个缓存键序列`, "success");
  });
}

async function exportWorkspace() {
  await runBusy("导出工作区", "序列化当前模型、增量清单、偏差报告与检查点状态…", async () => {
    const response = await bridge().Export();
    if (!response.ok) {
      handleFailure(response);
      return;
    }
    const blob = new Blob([response.data.json], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = response.data.filename;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1500);
    toast(`已导出 ${response.data.filename}`, "success");
  });
}

async function saveSample() {
  const seed = Number(state.snapshot?.seed || $("#region-select").value || 2042);
  await runBusy("保存生成样本", "将区域、观测与隐藏真值清单写入 samples/…", async () => {
    const response = await bridge().SaveGeneratedSample(seed);
    if (!response.ok) {
      handleFailure(response);
      return;
    }
    toast(`样本已写入 ${response.data.path}`, "success");
  });
}

async function importScenario(event) {
  const file = event.target.files?.[0];
  event.target.value = "";
  if (!file) return;
  await runBusy("导入样本", `校验 ${file.name} 的结构、几何与真值清单…`, async () => {
    const payload = await file.text();
    const response = await bridge().LoadScenarioJSON(payload);
    if (!response.ok) {
      handleFailure(response);
      return;
    }
    setSnapshot(response.data);
    toast(`样本 ${file.name} 已加载并完成重建`, "success");
  });
}

function handleFailure(response) {
  const code = response.code || response.error?.code || "E_UNKNOWN";
  const message = response.message || response.error?.message || "未知错误";
  $("#engine-status").classList.remove("ok");
  $("#engine-status").classList.add("err");
  toast(`${code} · ${message}`, "error");
  $("#status-message").textContent = `${code} · ${message}`;
}

function toast(message, type = "") {
  const node = document.createElement("div");
  node.className = `toast ${type}`;
  node.textContent = message;
  $("#toast-stack").appendChild(node);
  setTimeout(() => node.remove(), 5200);
}

function stateFor(zoneID) {
  return state.snapshot?.zoneStates?.find((item) => item.zoneId === zoneID);
}

function zoneName(zoneID) {
  return state.snapshot?.zones?.find((item) => item.id === zoneID)?.name || zoneID || "外部";
}

function heatColor(value, max, alpha) {
  const t = Math.min(1, Math.max(0, value / max));
  const stops = [
    [0, [18, 68, 70]],
    [0.35, [55, 140, 121]],
    [0.62, [184, 220, 99]],
    [0.82, [233, 180, 93]],
    [1, [239, 119, 107]],
  ];
  let index = 0;
  while (index < stops.length - 2 && t > stops[index + 1][0]) index++;
  const [t0, c0] = stops[index];
  const [t1, c1] = stops[index + 1];
  const f = (t - t0) / Math.max(0.0001, t1 - t0);
  const rgb = c0.map((c, i) => Math.round(c + (c1[i] - c) * f));
  return `rgba(${rgb[0]},${rgb[1]},${rgb[2]},${alpha})`;
}

function hexAlpha(hex, alpha) {
  const value = hex.replace("#", "");
  const r = parseInt(value.slice(0, 2), 16);
  const g = parseInt(value.slice(2, 4), 16);
  const b = parseInt(value.slice(4, 6), 16);
  return `rgba(${r},${g},${b},${alpha})`;
}

function pointInPolygon(point, vertices) {
  let inside = false;
  for (let i = 0, j = vertices.length - 1; i < vertices.length; j = i++) {
    const a = vertices[i];
    const b = vertices[j];
    if (((a.y > point.y) !== (b.y > point.y)) &&
      (point.x < (b.x - a.x) * (point.y - a.y) / (b.y - a.y) + a.x)) {
      inside = !inside;
    }
  }
  return inside;
}

function polygonCentroid(vertices) {
  let x = 0;
  let y = 0;
  let n = vertices.length;
  if (n > 1 && Math.abs(vertices[0].x - vertices[n - 1].x) < 1e-9 && Math.abs(vertices[0].y - vertices[n - 1].y) < 1e-9) n--;
  for (let i = 0; i < n; i++) {
    x += vertices[i].x;
    y += vertices[i].y;
  }
  return { x: x / Math.max(1, n), y: y / Math.max(1, n) };
}

function fmt(value, digits = 3) {
  if (value === undefined || value === null || Number.isNaN(Number(value))) return "—";
  return Number(value).toFixed(digits);
}

function pct(value) {
  return value === undefined || value === null ? "—" : `${(Number(value) * 100).toFixed(1)}%`;
}

function escapeHTML(value) {
  return String(value ?? "").replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#039;",
  }[char]));
}

function escapeAttr(value) { return escapeHTML(value); }
function escapeSVG(value) { return escapeHTML(value); }
