const $ = (selector) => document.querySelector(selector);
const state = {
  snapshot: null,
  selectedWindow: 0,
  selectedFrame: 0,
  heatmap: null,
  busy: false,
};

const wailsApp = () => window.go && window.go.main && window.go.main.App;

const formatRatio = (rational) => {
  if (!rational) return "—";
  return rational.den === 1 ? `${rational.num}s` : `${rational.num}/${rational.den}s`;
};

const formatFloat = (value, digits = 3) => Number(value || 0).toFixed(digits);
const hex = (value) => `#${value.map(v => Math.round(Math.max(0, Math.min(1, v)) * 255).toString(16).padStart(2, "0")).join("")}`;
const parseHex = (value) => [1, 3, 5].map(index => parseInt(value.slice(index, index + 2), 16) / 255);
const esc = (value) => String(value ?? "").replace(/[&<>"']/g, char => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[char]));

async function requestJSON(url, options = {}) {
  const response = await fetch(url, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  const payload = await response.json();
  if (!response.ok || payload.ok === false) {
    const err = new Error(payload.error?.message || payload.error || "Request failed");
    err.code = payload.error?.code || "E_PARA";
    throw err;
  }
  return payload;
}

async function apiState() {
  const binding = wailsApp();
  if (binding) return { ok: true, snapshot: await binding.GetState() };
  return requestJSON("/api/state");
}

async function apiFull() {
  setBusy(true, "Recompositing all windows…");
  try {
    const binding = wailsApp();
    if (binding) return { ok: true, snapshot: await binding.RunFull() };
    return await requestJSON("/api/full", { method: "POST", body: "{}" });
  } finally {
    setBusy(false);
  }
}

async function apiEdit(edit) {
  setBusy(true, "Applying invalidation…");
  try {
    const binding = wailsApp();
    if (binding) {
      const result = await binding.ApplyEdit(edit);
      return result;
    }
    return await requestJSON("/api/edit", { method: "POST", body: JSON.stringify(edit) });
  } finally {
    setBusy(false);
  }
}

async function apiExport(version) {
  setBusy(true, "Exporting and verifying round trip…");
  try {
    const binding = wailsApp();
    if (binding) return await binding.ExportVersion(version);
    return await requestJSON(`/api/export?version=${version}`, { method: "POST", body: "{}" });
  } finally {
    setBusy(false);
  }
}

async function apiValidate() {
  setBusy(true, "Validating sample truth…");
  try {
    const binding = wailsApp();
    if (binding) return await binding.ValidateTruth();
    return await requestJSON("/api/validate", { method: "POST", body: "{}" });
  } finally {
    setBusy(false);
  }
}

async function apiHeatmap(windowIndex, frame) {
  const binding = wailsApp();
  if (binding) {
    return await binding.GetHeatmap(windowIndex, frame, 32, 18);
  }
  const payload = await requestJSON(`/api/heatmap?window=${windowIndex}&frame=${frame}&cx=32&cy=18`);
  return payload.heatmap;
}

async function setFrameImage(windowIndex, frame) {
  const image = $("#frameImage");
  const binding = wailsApp();
  if (binding) {
    const dataURL = await binding.FrameDataURL(windowIndex, frame);
    image.src = dataURL;
    image.style.display = "block";
    $("#viewerEmpty").classList.add("hidden");
    return;
  }
  const win = state.snapshot?.windows?.[windowIndex];
  if (!win || win.state !== "committed") {
    image.style.display = "none";
    $("#viewerEmpty").classList.remove("hidden");
    return;
  }
  image.onload = () => {
    image.style.display = "block";
    $("#viewerEmpty").classList.add("hidden");
  };
  image.onerror = () => {
    image.style.display = "none";
    $("#viewerEmpty").classList.remove("hidden");
  };
  image.src = `/api/frame?window=${windowIndex}&frame=${frame}&rev=${win.snapshot?.frame_hash || ""}`;
}

function setBusy(value, message = "") {
  state.busy = value;
  $("#busy").classList.toggle("hidden", !value);
  $("#busy b").textContent = message || "Processing windows…";
  document.querySelectorAll("button").forEach(button => {
    if (button.id !== "viewer") button.disabled = value;
  });
}

let toastTimer = null;
function toast(message, error = false) {
  const el = $("#toast");
  el.textContent = message;
  el.classList.toggle("error", error);
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 4000);
}

function renderHeader() {
  const snapshot = state.snapshot;
  if (!snapshot) return;
  const model = snapshot.model;
  const versions = [...new Set(model.layers.map(layer => layer.source_version))].sort();
  $("#sceneMeta").innerHTML = `
    <span class="meta-chip"><strong>${esc(model.name)}</strong></span>
    <span class="meta-chip">${model.canvas.width}×${model.canvas.height} <strong>${model.canvas.channels}ch</strong></span>
    <span class="meta-chip">${esc(model.time_base.unit)} <strong>${formatRatio(model.time_base.frame_period)}</strong></span>
    <span class="meta-chip">${model.frame_count} frames</span>
    ${versions.map(version => `<span class="meta-chip version">SRC v${version}</span>`).join("")}
  `;
  $("#sourceBadges").innerHTML = versions.map(version => `<span class="version-badge">v${version}</span>`).join(" ");
  $("#algorithm").textContent = `algo vsp-composite/1 · source ${snapshot.source_hash.slice(0, 12)}`;
}

function layerCard(layer, index) {
  const color = hex(layer.color);
  const visible = Boolean(layer.visible);
  return `
    <article class="layer-card ${visible ? "" : "hidden-layer"}" data-layer="${esc(layer.id)}">
      <div class="layer-title-row">
        <span class="layer-index">${String(index + 1).padStart(2, "0")}</span>
        <span class="layer-title" title="${esc(layer.name)}">${esc(layer.name)}</span>
        <span class="version-badge">v${layer.source_version}</span>
        <label class="visibility-toggle" title="Layer visibility">
          <input type="checkbox" data-action="visible" ${visible ? "checked" : ""}>
          <span></span>
        </label>
      </div>
      <div class="layer-meta"><span class="kind-badge">${esc(layer.kind)}</span><span>${layer.in_frame} → ${layer.visible_until_frame}</span></div>
      <div class="layer-controls">
        <div class="slider-row">
          <label>Opacity</label>
          <input type="range" min="0" max="1" step="0.01" value="${layer.opacity}" data-action="opacity">
          <output>${formatFloat(layer.opacity, 2)}</output>
        </div>
        <div class="slider-row">
          <label>Alpha</label>
          <input type="range" min="0" max="1" step="0.01" value="${layer.alpha}" data-action="alpha">
          <output>${formatFloat(layer.alpha, 2)}</output>
        </div>
        <div class="color-row">
          <label class="field-label" style="margin:0;flex:1">Source color</label>
          <input type="color" value="${color}" data-action="color">
          <span class="mono" style="color:var(--dim);font-size:9px">${layer.color_encoding}</span>
        </div>
      </div>
      <button class="layer-apply" data-action="apply">Apply parameter snapshot</button>
    </article>
  `;
}

function renderLayers() {
  const model = state.snapshot.model;
  const byID = Object.fromEntries(model.layers.map(layer => [layer.id, layer]));
  const ordered = model.stack.map(id => byID[id]).filter(Boolean);
  $("#layerCount").textContent = String(ordered.length);
  $("#layerStack").innerHTML = ordered.map(layerCard).join("");
}

function renderProgress() {
  const snapshot = state.snapshot;
  $("#progressText").textContent = `${snapshot.committed_windows} / ${snapshot.total_windows}`;
  $("#progressBar").style.width = `${snapshot.total_windows ? snapshot.committed_windows / snapshot.total_windows * 100 : 0}%`;
  $("#windowMatrix").innerHTML = snapshot.windows.map(window => {
    const classes = ["window-cell", window.state];
    if (window.invalidated) classes.push("invalid");
    if (window.reused) classes.push("reused");
    if (window.index === state.selectedWindow) classes.push("selected");
    return `<button class="${classes.join(" ")}" data-window="${window.index}">
      <span class="w-index">W${String(window.index).padStart(2, "0")}</span>
      <span class="w-frames">${window.start_frame}–${window.end_frame - 1}</span>
    </button>`;
  }).join("");
}

function renderInvalidation() {
  const list = state.snapshot.last_invalidation;
  const target = $("#invalidationList");
  if (!list || !list.affected || list.affected.length === 0) {
    target.className = "invalidation-list muted";
    target.textContent = "No affected windows. All committed windows are reusable.";
    return;
  }
  target.className = "invalidation-list";
  target.innerHTML = list.affected.map(item => `
    <div class="invalidation-entry">
      <b>Window ${String(item.window).padStart(2, "0")}</b> · ${esc(item.reason)}
      <span>layers: ${esc((item.layer_ids || []).join(", "))} · stage: ${esc(item.stage)}</span>
    </div>
  `).join("") + `<div class="muted">Reused: ${list.reused?.length ? list.reused.map(i => `W${i}`).join(", ") : "none"}</div>`;
}

function updateSelectedWindow() {
  const window = state.snapshot?.windows?.[state.selectedWindow];
  if (!window) return;
  state.selectedFrame = Math.max(window.start_frame, Math.min(state.selectedFrame, window.end_frame - 1));
  const scrubber = $("#frameScrubber");
  scrubber.min = String(window.start_frame);
  scrubber.max = String(window.end_frame - 1);
  scrubber.value = String(state.selectedFrame);
  $("#frameLabel").textContent = `W${String(window.index).padStart(2, "0")} · F${String(state.selectedFrame).padStart(4, "0")}`;
  $("#viewerTitle").textContent = `Window ${window.index} · ${window.state}`;
  renderStats();
  renderHeatmap();
  setFrameImage(window.index, state.selectedFrame).catch(err => toast(err.message, true));
}

function renderStats() {
  const window = state.snapshot?.windows?.[state.selectedWindow];
  const target = $("#frameStats");
  if (!window?.snapshot || !window.snapshot.frame_summaries) {
    target.innerHTML = `<div class="stat-cell"><span>Status</span><strong>${window?.state || "—"}</strong><small>frame statistics unavailable</small></div>`;
    return;
  }
  const summary = window.snapshot.frame_summaries.find(item => item.frame === state.selectedFrame) || window.snapshot.frame_summaries[0];
  if (!summary) {
    target.innerHTML = `<div class="stat-cell"><span>Status</span><strong>no stats</strong></div>`;
    return;
  }
  target.innerHTML = `
    <div class="stat-cell"><span>Mean luma</span><strong>${formatFloat(summary.mean_luma)}</strong><small>linear</small></div>
    <div class="stat-cell"><span>Mean alpha</span><strong>${formatFloat(summary.mean_alpha)}</strong><small>coverage</small></div>
    <div class="stat-cell"><span>Min / max</span><strong>${formatFloat(summary.min_luma, 2)} / ${formatFloat(summary.max_luma, 2)}</strong><small>luma range</small></div>
    <div class="stat-cell"><span>Timestamp</span><strong>${formatRatio(summary.timestamp)}</strong><small>${summary.content_hash.slice(0, 12)}</small></div>
  `;
}

function drawHeatmap(heatmap) {
  const canvas = $("#heatmap");
  const ctx = canvas.getContext("2d");
  const width = canvas.width;
  const height = canvas.height;
  ctx.clearRect(0, 0, width, height);
  ctx.fillStyle = "#0b0e12";
  ctx.fillRect(0, 0, width, height);
  if (!heatmap?.available || !heatmap.diff) return;
  const cellW = width / heatmap.diff[0].length;
  const cellH = height / heatmap.diff.length;
  heatmap.diff.forEach((row, y) => row.forEach((value, x) => {
    const v = Math.max(0, Math.min(1, value * 5));
    const r = Math.round(35 + 220 * v);
    const g = Math.round(55 + 190 * Math.pow(v, .7) * (1 - v * .45));
    const b = Math.round(75 + 95 * (1 - v) * v);
    ctx.fillStyle = `rgb(${r},${g},${b})`;
    ctx.fillRect(x * cellW, y * cellH, Math.ceil(cellW), Math.ceil(cellH));
  }));
}

async function renderHeatmap() {
  const label = $("#heatmapLabel");
  try {
    const heatmap = await apiHeatmap(state.selectedWindow, state.selectedFrame);
    state.heatmap = heatmap;
    drawHeatmap(heatmap);
    $("#heatmapDelta").textContent = formatFloat(heatmap.max_delta, 4);
    label.textContent = heatmap.available
      ? `Previous committed packet vs current · window ${heatmap.window} · frame ${heatmap.frame}`
      : (heatmap.message || "No previous packet for this window");
  } catch (err) {
    drawHeatmap(null);
    label.textContent = err.message;
  }
}

function renderAll() {
  if (!state.snapshot) return;
  renderHeader();
  renderLayers();
  renderProgress();
  renderInvalidation();
  updateSelectedWindow();
}

async function refresh() {
  const payload = await apiState();
  state.snapshot = payload.snapshot;
  const firstCommitted = state.snapshot.windows.find(window => window.state === "committed");
  if (firstCommitted && state.selectedWindow == null) state.selectedWindow = firstCommitted.index;
  renderAll();
}

async function runFull() {
  try {
    const payload = await apiFull();
    state.snapshot = payload.snapshot;
    const firstCommitted = state.snapshot.windows.find(window => window.state === "committed");
    if (firstCommitted) state.selectedWindow = firstCommitted.index;
    state.selectedFrame = state.snapshot.windows[state.selectedWindow]?.start_frame || 0;
    renderAll();
    toast(`Full rebuild committed ${state.snapshot.committed_windows}/${state.snapshot.total_windows} windows`);
  } catch (err) {
    toast(err.message, true);
  }
}

async function applyLayerEdit(card, action) {
  const layerID = card.dataset.layer;
  const layer = state.snapshot.model.layers.find(item => item.id === layerID);
  if (!layer) return;
  let edit;
  if (action === "visible") {
    edit = { kind: "layer_visibility", layer_id: layerID, visible: card.querySelector('[data-action="visible"]').checked };
  } else {
    edit = {
      kind: "layer_parameter",
      layer_id: layerID,
      opacity: Number(card.querySelector('[data-action="opacity"]').value),
      alpha: Number(card.querySelector('[data-action="alpha"]').value),
      color: parseHex(card.querySelector('[data-action="color"]').value),
    };
  }
  try {
    const payload = await apiEdit(edit);
    state.snapshot = payload.snapshot;
    if (payload.invalidation?.affected?.length) {
      state.selectedWindow = payload.invalidation.affected[0].window;
      state.selectedFrame = state.snapshot.windows[state.selectedWindow]?.start_frame || 0;
    }
    renderAll();
    const affected = payload.invalidation?.affected?.length || 0;
    toast(`Recomputed ${affected} affected window${affected === 1 ? "" : "s"}; reused ${payload.invalidation?.reused?.length || 0}`);
  } catch (err) {
    toast(`${err.code || "E_PARA"} · ${err.message}`, true);
  }
}

async function exportVersion() {
  const version = Number($("#exportVersion").value);
  try {
    const payload = await apiExport(version);
    const verification = payload.verification || payload;
    const warnings = verification.warnings || [];
    $("#exportResult").innerHTML = `
      <div>${verification.round_trip_ok ? "Round trip verified" : "Round trip failed"} · deterministic ${verification.deterministic ? "yes" : "no"}</div>
      <div class="mono">${esc(verification.report?.content_hash || "")}</div>
      ${warnings.length ? warnings.map(w => `<div class="warning">E_LOSSY · ${esc(w.layer_id)} · ${esc(w.property)}: ${esc(w.from)} → ${esc(w.to)}</div>`).join("") : '<div class="muted">No degradation warnings.</div>'}
    `;
    toast(`Exported v${version}: ${warnings.length} E_LOSSY warning${warnings.length === 1 ? "" : "s"}`);
  } catch (err) {
    $("#exportResult").innerHTML = `<div class="warning">${esc(err.message)}</div>`;
    toast(err.message, true);
  }
}

async function validateTruth() {
  try {
    const payload = await apiValidate();
    const validation = payload.validation || payload;
    toast(validation.ok
      ? `Truth matched: ${validation.frames_checked} frames, ${validation.samples_checked} samples, max Δ ${formatFloat(validation.max_abs_error, 8)}`
      : `Truth failed: ${(validation.errors || []).slice(0, 2).join("; ")}`, !validation.ok);
  } catch (err) {
    toast(err.message, true);
  }
}

function bindEvents() {
  $("#fullBtn").addEventListener("click", runFull);
  $("#validateBtn").addEventListener("click", validateTruth);
  $("#exportBtn").addEventListener("click", exportVersion);
  $("#prevFrame").addEventListener("click", () => {
    const window = state.snapshot.windows[state.selectedWindow];
    if (!window) return;
    state.selectedFrame = state.selectedFrame > window.start_frame ? state.selectedFrame - 1 : window.end_frame - 1;
    updateSelectedWindow();
  });
  $("#nextFrame").addEventListener("click", () => {
    const window = state.snapshot.windows[state.selectedWindow];
    if (!window) return;
    state.selectedFrame = state.selectedFrame < window.end_frame - 1 ? state.selectedFrame + 1 : window.start_frame;
    updateSelectedWindow();
  });
  $("#frameScrubber").addEventListener("input", event => {
    state.selectedFrame = Number(event.target.value);
    updateSelectedWindow();
  });
  $("#windowMatrix").addEventListener("click", event => {
    const cell = event.target.closest("[data-window]");
    if (!cell) return;
    state.selectedWindow = Number(cell.dataset.window);
    state.selectedFrame = state.snapshot.windows[state.selectedWindow].start_frame;
    renderProgress();
    updateSelectedWindow();
  });
  document.addEventListener("input", event => {
    const card = event.target.closest(".layer-card");
    if (!card) return;
    const action = event.target.dataset.action;
    if (action === "opacity" || action === "alpha") {
      card.querySelector(`[data-action="${action}"]`).nextElementSibling.textContent = formatFloat(event.target.value, 2);
    }
  });
  document.addEventListener("change", event => {
    const card = event.target.closest(".layer-card");
    if (!card || event.target.dataset.action !== "visible") return;
    applyLayerEdit(card, "visible");
  });
  $("#layerStack").addEventListener("click", event => {
    const button = event.target.closest('[data-action="apply"]');
    if (!button) return;
    applyLayerEdit(button.closest(".layer-card"), "parameters");
  });
  window.addEventListener("resize", () => {
    if (state.heatmap) drawHeatmap(state.heatmap);
  });
}

async function boot() {
  bindEvents();
  try {
    await refresh();
    $("#statusMessage").textContent = "Ready · shared model snapshot loaded";
  } catch (err) {
    $("#statusMessage").textContent = err.message;
    toast(err.message, true);
  }
}

boot();
