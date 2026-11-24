const state = {
  init: null,
  frames: [],
  index: 0,
  quality: null,
  poll: null,
  reloadedAfterRun: false,
};

const $ = (id) => document.getElementById(id);

function api() {
  if (!window.go || !window.go.main || !window.go.main.App) {
    throw new Error("Wails bindings are unavailable. Launch through the Wails application.");
  }
  return window.go.main.App;
}

function showError(err) {
  const box = $("errorBox");
  box.textContent = String(err && err.message ? err.message : err);
  box.classList.remove("hidden");
}

function clearError() {
  $("errorBox").classList.add("hidden");
  $("errorBox").textContent = "";
}

function b64ToUint8(value) {
  if (!value) return new Uint8ClampedArray(0);
  const raw = atob(value);
  const out = new Uint8ClampedArray(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

function putBitmap(canvas, width, height, rgba) {
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, width, height);
  if (!rgba || rgba.length !== width * height * 4) return;
  ctx.putImageData(new ImageData(rgba, width, height), 0, 0);
}

function internalToPixel(v, origin, scale) {
  return (v - origin) * scale / (1024 * 1024);
}

function drawOverlay(frame) {
  const canvas = $("overlayCanvas");
  canvas.width = frame.width;
  canvas.height = frame.height;
  const ctx = canvas.getContext("2d");
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  const b = frame.renderBounds;
  frame.lines.forEach((line, i) => {
    const stroke = frame.strokes[i] || {};
    ctx.beginPath();
    line.points.forEach((p, j) => {
      const x = internalToPixel(p.x, b.minX, frame.scale);
      const y = internalToPixel(p.y, b.minY, frame.scale);
      if (j === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
    });
    ctx.closePath();
    ctx.strokeStyle = "rgba(255,255,255,.72)";
    ctx.lineWidth = 0.85;
    ctx.stroke();
    if (stroke.hasStroke) {
      const c = stroke.color || {};
      ctx.strokeStyle = `rgba(${c.r || 0},${c.g || 0},${c.b || 0},${(c.a == null ? 255 : c.a) / 255})`;
      ctx.lineWidth = Math.max(0.8, (stroke.widthQ10 || 0) * frame.scale / (1024 * 1024));
      ctx.stroke();
    }
  });
}

function drawHeat(frame) {
  const heat = $("heatCanvas");
  if (frame.heatmap && frame.heatmap.length) {
    putBitmap(heat, frame.width, frame.height, b64ToUint8(frame.heatmap));
    $("diffLabel").textContent = "相对精细基线的通道最大差 ×5";
  } else {
    heat.width = 640;
    heat.height = 360;
    const ctx = heat.getContext("2d");
    ctx.fillStyle = "#07080a";
    ctx.fillRect(0, 0, heat.width, heat.height);
    $("diffLabel").textContent = "无差异数据";
  }
}

async function loadFrame(index) {
  const frame = await api().Frame(index);
  state.index = index;
  $("frameSlider").value = String(index);
  $("frameLabel").textContent = `frame ${index + 1}/${state.frames.length}`;
  putBitmap($("bitmapCanvas"), frame.width, frame.height, b64ToUint8(frame.rgba));
  drawOverlay(frame);
  drawHeat(frame);
  $("frameMeta").textContent =
    `${frame.timeMs} ms · ${frame.width}×${frame.height} · ${frame.lineCount} segments · ${frame.hash.slice(0, 16)}…`;
  renderCoverage(frame.pathSummaries || []);
  if (frame.quality) renderQuality(frame.quality);
}

function renderQuality(q) {
  state.quality = q;
  $("toleranceInput").value = String(q.tolerance || 64);
  $("precisionInput").value = String(q.precisionBits || 12);
  const reasons = q.reasons || [];
  $("qualityReasons").innerHTML = reasons.map((r) => `<li>${escapeHTML(r)}</li>`).join("");
}

function renderCoverage(paths) {
  $("pathCoverage").innerHTML = paths.map((p) => {
    const pct = Math.max(0, Math.min(1, p.coverageRatio || 0));
    return `<div class="coverage-row">
      <span>${escapeHTML(p.id)}</span>
      <span class="bar"><i style="width:${(pct * 100).toFixed(2)}%"></i></span>
      <span>${(pct * 100).toFixed(2)}%</span>
    </div>`;
  }).join("");
}

function renderStateMatrix(cells) {
  const host = $("stateMatrix");
  if (!cells || !cells.length) {
    host.textContent = "尚无状态数据";
    return;
  }
  const grouped = new Map();
  for (const c of cells) {
    const key = `${c.timeUs}/${c.path}`;
    if (!grouped.has(key)) grouped.set(key, { timeUs: c.timeUs, path: c.path });
    grouped.get(key)[c.stage] = c.state;
  }
  const rows = [...grouped.values()];
  rows.sort((a, b) => a.timeUs - b.timeUs || a.path.localeCompare(b.path));
  const head = `<div class="matrix-row matrix-head"><span>time</span><span>path</span><span>flatten</span><span>coverage</span><span>composite</span></div>`;
  const body = rows.map((r) => `<div class="matrix-row">
    <span>${(r.timeUs / 1000).toFixed(0)}ms</span>
    <span>${escapeHTML(r.path)}</span>
    ${cell(r.flatten)}${cell(r.coverage)}${cell(r.composite)}
  </div>`).join("");
  host.innerHTML = head + body;
}

function cell(s) {
  const value = s || "pending";
  return `<span class="cell ${value}">${value}</span>`;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (m) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[m]));
}

async function refreshStatus() {
  try {
    const s = await api().Status();
    const badge = $("runBadge");
    badge.className = "badge " + (s.running ? "running" : s.error ? "bad" : s.comparison ? "ok" : "idle");
    badge.textContent = s.running ? (s.paused ? "已暂停" : "并行运行中") : s.error ? "失败" : s.comparison ? "一致性通过" : "待运行";
    $("statusList").innerHTML = [
      ["running", s.running],
      ["paused", s.paused],
      ["frames", s.frameCount],
      ["partial", s.partial],
      ["snapshot", s.snapshot && s.snapshot.valid ? "valid" : (s.snapshot && s.snapshot.reason) || "-"],
      ["mode", s.quality ? s.quality.mode : "-"],
      ["workers", s.quality ? s.quality.maxWorkers : "-"],
      ["commit", (s.commitSequence || []).join(", ") || "-"],
    ].map(([k, v]) => `<dt>${k}</dt><dd>${escapeHTML(String(v))}</dd>`).join("");
    if (s.comparison) {
      $("diffLabel").textContent = s.comparison.consistent
        ? `三路径一致 · commit ${s.comparison.commitSequence.join(",")}`
        : `E_DIFF frame=${s.comparison.frame} field=${s.comparison.field}`;
    }
    if (s.running) {
      state.reloadedAfterRun = false;
    } else if (s.comparison && !state.reloadedAfterRun) {
      state.reloadedAfterRun = true;
      loadFrame(state.index).catch(showError);
    }
    renderStateMatrix(s.stateMatrix || []);
    if (s.error) showError(s.error); else clearError();
  } catch (err) {
    showError(err);
  }
}

async function initialize() {
  clearError();
  const init = await api().Initialize("artifacts/sample", "artifacts/ui/snapshots.db");
  state.init = init;
  state.frames = init.frames || [];
  $("frameSlider").max = String(Math.max(0, state.frames.length - 1));
  $("datasetInfo").textContent =
    `scene: ${init.sceneId}\nversions: ${init.versions.join(" / ")}\ndata: ${init.dataDir}\ndb: ${init.dbPath}\n${init.message}`;
  renderQuality(init.quality);
  renderStateMatrix(init.stateMatrix || []);
  if (state.frames.length) await loadFrame(0);
  await refreshStatus();
}

function bind() {
  $("frameSlider").addEventListener("input", (e) => loadFrame(Number(e.target.value)).catch(showError));
  $("prevBtn").addEventListener("click", () => loadFrame(Math.max(0, state.index - 1)).catch(showError));
  $("nextBtn").addEventListener("click", () => loadFrame(Math.min(state.frames.length - 1, state.index + 1)).catch(showError));
  $("applyQualityBtn").addEventListener("click", async () => {
    try {
      clearError();
      const frame = await api().AdjustFrame(state.index, Number($("toleranceInput").value), Number($("precisionInput").value));
      applyFrameView(frame);
    } catch (err) { showError(err); }
  });
  $("retryBtn").addEventListener("click", async () => {
    try {
      clearError();
      const frame = await api().RetryFrame(state.index);
      applyFrameView(frame);
    } catch (err) { showError(err); }
  });
  $("runBtn").addEventListener("click", async () => {
    try {
      clearError();
      await api().RunAll("artifacts/ui/snapshots.db");
      await refreshStatus();
    } catch (err) { showError(err); }
  });
  $("pauseBtn").addEventListener("click", () => Promise.resolve(api().Pause()).catch(showError));
  $("resumeBtn").addEventListener("click", () => Promise.resolve(api().Resume()).catch(showError));
  $("cancelBtn").addEventListener("click", () => Promise.resolve(api().Cancel()).catch(showError));
}

function applyFrameView(frame) {
  putBitmap($("bitmapCanvas"), frame.width, frame.height, b64ToUint8(frame.rgba));
  drawOverlay(frame);
  drawHeat(frame);
  $("frameMeta").textContent =
    `${frame.timeMs} ms · ${frame.width}×${frame.height} · ${frame.lineCount} segments · ${frame.hash.slice(0, 16)}…`;
  renderQuality(frame.quality);
  renderCoverage(frame.pathSummaries || []);
}

bind();
initialize().catch(showError);
state.poll = setInterval(refreshStatus, 700);
