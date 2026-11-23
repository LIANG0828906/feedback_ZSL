const state = {
  presets: [],
  scenes: [],
  currentId: null,
  scene: null,
  engine: null,
  result: null,
  compare: null,
  selectedView: "fused",
  histKind: "fused",
  polling: false,
  lastContent: "",
  toastTimer: null,
};

const $ = (id) => document.getElementById(id);
const fmt = (v, digits = 3) => Number.isFinite(v) ? v.toFixed(digits) : "—";
const pct = (v) => `${Math.round((v || 0) * 100)}%`;
const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  let payload = null;
  try { payload = await response.json(); } catch (_) {}
  if (!response.ok || !payload?.ok) {
    const err = payload?.error || { code: `HTTP_${response.status}`, message: response.statusText };
    err.report = payload?.report;
    throw err;
  }
  return payload.data;
}

function toast(title, message, kind = "") {
  const el = document.createElement("div");
  el.className = `toast ${kind}`;
  el.innerHTML = `<b>${esc(title)}</b>${esc(message)}`;
  $("toastStack").appendChild(el);
  setTimeout(() => el.remove(), 4800);
}

function setLive(on) {
  $("livePill").classList.toggle("online", on);
  $("livePill").querySelector("span").textContent = on ? "LOCAL / ONLINE" : "OFFLINE";
}

async function boot() {
  tickClock();
  setInterval(tickClock, 1000);
  try {
    const [health, presets, scenes] = await Promise.all([
      api("/api/health"),
      api("/api/presets"),
      api("/api/scenes"),
    ]);
    setLive(true);
    $("algorithmTag").textContent = `ALGORITHM ${health.algorithm}`;
    state.presets = presets;
    state.scenes = scenes;
    renderPresets();
    renderSceneList();
    if (scenes.length) {
      await selectScene(scenes[0].scene.id);
    } else {
      await createPreset("demo");
    }
    bindControls();
    setInterval(pollStatus, 850);
  } catch (err) {
    setLive(false);
    toast("STARTUP ERROR", err.message || String(err), "error");
  }
}

function tickClock() {
  $("clock").textContent = new Date().toLocaleTimeString("en-GB", { hour12: false });
}

function renderPresets() {
  $("presetGrid").innerHTML = state.presets.map((p) => `
    <button class="preset-button" data-preset="${esc(p.name)}">
      <div><b>${esc(p.title)}</b><small>${esc(p.description)}</small></div>
      <span>+</span>
    </button>`).join("");
  $("presetGrid").querySelectorAll("[data-preset]").forEach((el) => {
    el.addEventListener("click", () => createPreset(el.dataset.preset));
  });
}

async function createPreset(name) {
  try {
    const data = await api("/api/scenes", { method: "POST", body: JSON.stringify({ preset: name }) });
    state.scenes = await api("/api/scenes");
    renderSceneList();
    await selectScene(data.scene.id);
    toast("SCENE CREATED", data.scene.id);
  } catch (err) {
    toast(err.code || "CREATE ERROR", err.message || String(err), "error");
  }
}

function renderSceneList() {
  $("sceneCount").textContent = state.scenes.length;
  $("sceneList").innerHTML = state.scenes.map((item) => {
    const scene = item.scene;
    return `<button class="scene-row ${state.currentId === scene.id ? "active" : ""}" data-scene="${esc(scene.id)}">
      <i></i><span>${esc(scene.id)}</span><code>${scene.metadata.width}×${scene.metadata.height}</code>
    </button>`;
  }).join("");
  $("sceneList").querySelectorAll("[data-scene]").forEach((el) => {
    el.addEventListener("click", () => selectScene(el.dataset.scene));
  });
}

async function selectScene(id) {
  state.currentId = id;
  state.result = null;
  state.compare = null;
  state.lastContent = "";
  $("viewerImage").removeAttribute("src");
  $("diffImage").removeAttribute("src");
  renderSceneList();
  try {
    const data = await api(`/api/scenes/${encodeURIComponent(id)}/status`);
    state.scene = data.scene;
    state.engine = data.engine;
    state.compare = data.compare;
    applySceneView();
    applyEngineView();
    await refreshVisuals();
  } catch (err) {
    toast(err.code || "LOAD ERROR", err.message || String(err), "error");
  }
}

function applySceneView() {
  const m = state.scene.metadata;
  $("sceneTitle").textContent = state.scene.id;
  $("metaPixels").textContent = `${m.width} × ${m.height}`;
  $("metaBands").textContent = m.bandCount;
  $("metaExposures").textContent = m.exposureCount;
  $("metaBitDepth").textContent = `${m.bitDepth} bit`;
  $("metaPitch").textContent = `${fmt(m.pixelPitchMicrons, 2)} µm`;
  $("metaWhite").textContent = m.whitePoint.name;
  $("bandChips").innerHTML = m.bands.map((b) => `<span class="band-chip">B${b.index} · ${Math.round(b.centerWavelengthNm)}nm</span>`).join("");
  $("viewerDimensions").textContent = `${m.width} × ${m.height} px`;
  renderViewTabs();
}

function applyEngineView() {
  const e = state.engine;
  if (!e) return;
  const cfg = e.config;
  $("pathSelect").value = cfg.path;
  $("orderRange").value = cfg.interpolationOrder;
  $("orderOut").textContent = cfg.interpolationOrder;
  $("biasRange").value = cfg.weightBias;
  $("biasOut").textContent = fmt(cfg.weightBias, 2);
  $("sharpRange").value = cfg.weightSharpness;
  $("sharpOut").textContent = fmt(cfg.weightSharpness, 2);
  $("detailRange").value = cfg.detailStrength;
  $("detailOut").textContent = fmt(cfg.detailStrength, 2);
  $("intentSelect").value = cfg.gamutIntent;
  $("workersInput").value = cfg.maxWorkers;
  $("bufferInput").value = cfg.pixelBufferLimit;
  $("paramVersion").textContent = e.paramVersion;
  $("progressText").textContent = `${e.progress.committed} / ${e.progress.total}`;
  $("progressPct").textContent = pct(e.progress.fraction);
  $("progressBar").style.width = pct(e.progress.fraction);
  $("stateLine").textContent = e.closed ? "Engine closed" : e.paused ? "Paused · in-flight commits allowed" : e.running ? "Processing · worker pool active" : e.progress.committed === e.progress.total ? "Complete · all segments frozen" : "Idle · awaiting start";
  $("peakValue").textContent = fmt(e.peakRadiance, 4);
  $("gamutValue").textContent = e.outOfGamut;
  $("checkpointValue").textContent = `${e.checkpoint.loadedSegments || 0}/${e.progress.total}`;
  $("checkpointLine").textContent = e.checkpoint.invalidated ? `invalidated · ${e.checkpoint.reason}` : e.checkpoint.loadedSegments ? "restored + serial writes" : "ready";
  $("workerTag").textContent = `${e.activeWorkers}/${e.maxWorkers} workers · ${e.buffersInUse} buffers`;
  $("policyPath").textContent = e.scale.selectedPath.toUpperCase();
  $("policyDownsample").textContent = `x${e.scale.downsampleFactor}`;
  $("policyTile").textContent = `${e.scale.tileWidth}×${e.scale.tileHeight}`;
  $("policyMemory").textContent = `${fmt(e.scale.estimatedMemoryBytes / 1048576, 1)} MiB`;
  $("policyTime").textContent = `${fmt(e.scale.estimatedFullMs, 1)} ms`;
  $("policyCoarse").textContent = e.scale.coarseSamples ? `${e.scale.coarseSamples}` : "native";
  $("policyRationale").textContent = e.scale.rationale;
  $("pathOrb").style.background = e.scale.isTiled ? "var(--orange)" : "var(--cyan)";
  $("pathOrb").style.boxShadow = `0 0 10px ${e.scale.isTiled ? "var(--orange)" : "var(--cyan)"}`;
  renderSegments();
  renderLogs();
  if (e.resultReady) {
    if (!$("peakTruth").textContent.startsWith("truth")) {
      $("peakTruth").textContent = `truth ${fmt(state.scene.truth.peakRadiance, 4)}`;
    }
    $("startBtn").disabled = true;
  } else {
    $("startBtn").disabled = e.running;
  }
  $("pauseBtn").disabled = !e.running || e.paused;
  $("resumeBtn").disabled = !e.paused;
  $("stepBtn").disabled = e.running || e.resultReady;
  $("compareBtn").disabled = !e.resultReady;
  $("exportBtn").disabled = !e.resultReady;
}

function renderSegments() {
  const segments = state.engine?.segments || [];
  $("segmentGrid").style.gridTemplateColumns = `repeat(${Math.min(18, Math.max(8, Math.ceil(Math.sqrt(segments.length * 1.9))))}, minmax(34px, 1fr))`;
  $("segmentGrid").innerHTML = segments.map((s) => {
    const label = String(s.index).padStart(2, "0");
    const tip = `Segment ${s.index} · ${s.state}\n${s.width}×${s.height} @ ${s.x},${s.y}\nparams ${s.paramVersion || "—"} · x${s.downsampleFactor || 1}`;
    return `<div class="segment-cell ${s.state}" data-seg="${s.index}" data-state="${s.state}">${label}<span class="seg-tip">${esc(tip)}</span></div>`;
  }).join("");
  $("segmentGrid").querySelectorAll("[data-state=failed]").forEach((el) => {
    el.addEventListener("click", () => retrySegment(Number(el.dataset.seg)));
  });
}

function renderLogs() {
  const logs = (state.engine?.logs || []).slice(-80).reverse();
  $("logStream").innerHTML = logs.map((log) => {
    const time = (log.at || "").slice(11, 19);
    return `<div class="log-line ${esc(log.level)}"><time>${esc(time)}</time><span class="lvl">${esc(log.level)}</span><span>${log.segment >= 0 ? `[S${log.segment}] ` : ""}${esc(log.message)}</span></div>`;
  }).join("") || `<div class="log-line"><time>—</time><span class="lvl">idle</span><span>No events yet.</span></div>`;
}

function renderViewTabs() {
  const bands = state.scene?.metadata?.bands || [];
  const tabs = [
    ...bands.map((b) => ({ id: `band${b.index}`, label: `B${b.index} ${Math.round(b.centerWavelengthNm)}nm` })),
    { id: "fused", label: "Fused" },
    { id: "rgb", label: "Linear RGB" },
    { id: "gamut", label: "Gamut mask" },
    { id: "detail", label: "Detail" },
    { id: "diff", label: "Diff" },
  ];
  if (!tabs.some((t) => t.id === state.selectedView)) state.selectedView = "fused";
  $("viewTabs").innerHTML = tabs.map((t) => `<button class="view-tab ${state.selectedView === t.id ? "active" : ""}" data-view="${t.id}">${esc(t.label)}</button>`).join("");
  $("viewTabs").querySelectorAll("[data-view]").forEach((el) => {
    el.addEventListener("click", () => {
      state.selectedView = el.dataset.view;
      renderViewTabs();
      refreshVisuals();
    });
  });
}

async function refreshVisuals() {
  if (!state.currentId || !state.scene) return;
  const version = `${state.engine?.contentVersion || "new"}-${state.engine?.paramVersion || "p1"}-${state.engine?.progress?.committed || 0}`;
  if (version !== state.lastContent) {
    state.lastContent = version;
    state.result = null;
    if (state.engine?.resultReady) {
      try { state.result = await api(`/api/scenes/${encodeURIComponent(state.currentId)}/result`); } catch (_) {}
    }
    renderResultState();
    await Promise.allSettled([loadHistogram(), loadChromaticity()]);
  }
  if (state.engine?.resultReady) {
    $("compareBtn").disabled = false;
    $("exportBtn").disabled = false;
  }
}

function renderResultState() {
  if (!state.result) return;
  const r = state.result;
  $("peakValue").textContent = fmt(r.peakRadiance, 4);
  $("peakTruth").textContent = `truth ${fmt(r.truthPeakRadiance, 4)} · Δ ${pct(r.peakRelativeError)}`;
  $("gamutValue").textContent = r.outOfGamut;
  $("gamutHash").textContent = `set ${String(r.outOfGamutHash || "—").slice(0, 10)}`;
  $("checkpointValue").textContent = `${r.checkpoint.loadedSegments || 0}/${r.progress.total}`;
  $("checkpointLine").textContent = `${r.segments.filter((s) => s.state === "committed").length} frozen segments`;
  $("truthPill").textContent = r.patches.every((p) => p.pass) && r.peakPass ? "TRUTH PASS" : "OUT OF TOLERANCE";
  $("truthPill").className = `result-pill ${r.patches.every((p) => p.pass) && r.peakPass ? "pass" : "fail"}`;
  $("workerTag").textContent = `${state.engine.activeWorkers}/${state.engine.maxWorkers} workers`;
  renderPatches(r.patches);
  if (!state.compare) $("comparePill").textContent = "not run";
}

function renderPatches(patches) {
  if (!patches?.length) {
    $("patchRows").innerHTML = `<tr><td colspan="5" class="empty-cell">Complete processing to evaluate truth patches.</td></tr>`;
    return;
  }
  $("patchRows").innerHTML = patches.map((p) => {
    const sw = `rgb(${Math.round(p.truthLinearRGB[0] * 255)},${Math.round(p.truthLinearRGB[1] * 255)},${Math.round(p.truthLinearRGB[2] * 255)})`;
    return `<tr>
      <td><span class="swatch" style="background:${sw}"></span>${esc(p.id)}</td>
      <td>${p.truthLinearRGB.map((v) => fmt(v, 3)).join(" / ")}</td>
      <td>${p.actualLinearRGB.map((v) => fmt(v, 3)).join(" / ")}</td>
      <td>${fmt(p.maxError, 5)}</td>
      <td><span class="result-pill ${p.pass ? "pass" : "fail"}">${p.pass ? "PASS" : "FAIL"}</span></td>
    </tr>`;
  }).join("");
}

async function loadHistogram() {
  if (!state.currentId) return;
  try {
    const data = await api(`/api/scenes/${encodeURIComponent(state.currentId)}/histogram?kind=${state.histKind}&bins=64`);
    drawHistogram(data);
    $("histFoot").textContent = `${data.kind.toUpperCase()} · ${data.bins} bins · range ${fmt(data.min, 3)}..${fmt(data.max, 3)}`;
  } catch (_) {}
}

async function loadChromaticity() {
  if (!state.currentId) return;
  try {
    const data = await api(`/api/scenes/${encodeURIComponent(state.currentId)}/chromaticity`);
    drawChromaticity(data);
    $("chromaTag").textContent = `${data.points.length} samples · ${data.outOfGamut} OOG`;
  } catch (_) {}
}

function canvasContext(canvas) {
  const dpr = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  if (canvas.width !== Math.round(rect.width * dpr) || canvas.height !== Math.round(rect.height * dpr)) {
    canvas.width = Math.round(rect.width * dpr);
    canvas.height = Math.round(rect.height * dpr);
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { ctx, width: rect.width, height: rect.height };
}

function drawHistogram(data) {
  const canvas = $("histCanvas");
  if (!canvas.getBoundingClientRect().width) return;
  const { ctx, width, height } = canvasContext(canvas);
  ctx.clearRect(0, 0, width, height);
  const pad = { l: 8, r: 8, t: 10, b: 14 };
  const plotW = width - pad.l - pad.r;
  const plotH = height - pad.t - pad.b;
  ctx.strokeStyle = "rgba(160,191,220,.08)";
  ctx.lineWidth = 1;
  for (let i = 0; i <= 4; i++) {
    const y = pad.t + plotH * i / 4;
    ctx.beginPath(); ctx.moveTo(pad.l, y); ctx.lineTo(width - pad.r, y); ctx.stroke();
  }
  const series = data.channels ? [
    { key: "r", color: "#ff718c" }, { key: "g", color: "#66e3a1" }, { key: "b", color: "#69e5d1" }
  ] : [{ key: "counts", color: "#69e5d1" }];
  const max = Math.max(1, ...series.flatMap((s) => data.channels ? data.channels[s.key] : data.counts));
  for (const s of series) {
    const counts = data.channels ? data.channels[s.key] : data.counts;
    const grad = ctx.createLinearGradient(0, pad.t, 0, pad.t + plotH);
    grad.addColorStop(0, s.color + "aa");
    grad.addColorStop(1, s.color + "12");
    ctx.fillStyle = grad;
    ctx.strokeStyle = s.color;
    ctx.beginPath();
    counts.forEach((c, i) => {
      const x = pad.l + (i + .5) * plotW / counts.length;
      const y = pad.t + plotH - (c / max) * plotH;
      i ? ctx.lineTo(x, y) : ctx.moveTo(x, y);
    });
    ctx.lineTo(width - pad.r, pad.t + plotH);
    ctx.lineTo(pad.l, pad.t + plotH);
    ctx.closePath();
    ctx.fill();
    ctx.globalAlpha = .55;
    ctx.stroke();
    ctx.globalAlpha = 1;
  }
}

function drawChromaticity(data) {
  const canvas = $("chromaCanvas");
  if (!canvas.getBoundingClientRect().width) return;
  const { ctx, width, height } = canvasContext(canvas);
  ctx.clearRect(0, 0, width, height);
  const pad = 20;
  const xMin = 0, xMax = .78, yMin = 0, yMax = .88;
  const x = (v) => pad + (v - xMin) / (xMax - xMin) * (width - pad * 2);
  const y = (v) => height - pad - (v - yMin) / (yMax - yMin) * (height - pad * 2);
  ctx.strokeStyle = "rgba(160,191,220,.09)";
  ctx.fillStyle = "rgba(133,149,168,.75)";
  ctx.font = "8px monospace";
  for (let i = 0; i <= 6; i++) {
    const gx = xMin + (xMax - xMin) * i / 6;
    ctx.beginPath(); ctx.moveTo(x(gx), y(yMin)); ctx.lineTo(x(gx), y(yMax)); ctx.stroke();
    ctx.fillText(gx.toFixed(1), x(gx) - 7, height - 5);
  }
  for (let i = 0; i <= 5; i++) {
    const gy = yMin + (yMax - yMin) * i / 5;
    ctx.beginPath(); ctx.moveTo(x(xMin), y(gy)); ctx.lineTo(x(xMax), y(gy)); ctx.stroke();
    ctx.fillText(gy.toFixed(1), 3, y(gy) + 3);
  }
  if (data.boundaryXY?.length) {
    ctx.beginPath();
    data.boundaryXY.forEach((p, i) => i ? ctx.lineTo(x(p.x), y(p.y)) : ctx.moveTo(x(p.x), y(p.y)));
    ctx.closePath();
    ctx.fillStyle = "rgba(105,229,211,.045)";
    ctx.fill();
    ctx.strokeStyle = "rgba(105,229,211,.55)";
    ctx.lineWidth = 1;
    ctx.stroke();
  }
  for (const p of data.points || []) {
    ctx.fillStyle = p.oog ? "rgba(255,95,125,.55)" : "rgba(105,229,211,.26)";
    ctx.fillRect(x(p.x) - 1, y(p.y) - 1, p.oog ? 2.4 : 1.8, p.oog ? 2.4 : 1.8);
  }
  if (data.targetWhiteXY) {
    ctx.fillStyle = "#ffffff";
    ctx.beginPath(); ctx.arc(x(data.targetWhiteXY.x), y(data.targetWhiteXY.y), 2.8, 0, Math.PI * 2); ctx.fill();
    ctx.strokeStyle = "rgba(255,255,255,.5)";
    ctx.beginPath(); ctx.arc(x(data.targetWhiteXY.x), y(data.targetWhiteXY.y), 6, 0, Math.PI * 2); ctx.stroke();
  }
}

async function pollStatus() {
  if (!state.currentId || state.polling) return;
  state.polling = true;
  try {
    const data = await api(`/api/scenes/${encodeURIComponent(state.currentId)}/status`);
    state.scene = data.scene;
    state.engine = data.engine;
    state.compare = data.compare;
    applyEngineView();
    await refreshVisuals();
    updateViewer();
    updateDiff();
    if (state.compare) renderCompare(state.compare);
  } catch (err) {
    if (err.code !== "E_STATE") setLive(false);
  } finally {
    state.polling = false;
  }
}

function updateViewer() {
  const kind = state.selectedView;
  const img = $("viewerImage");
  if (!state.currentId) { img.removeAttribute("src"); return; }
  const needsResult = ["fused", "rgb", "gamut", "diff"].includes(kind);
  if (needsResult && !state.engine?.progress?.committed) {
    img.removeAttribute("src");
    $("viewerEmpty").style.display = "";
    $("viewerEmpty").textContent = "Start processing to reveal fused output";
  } else {
    $("viewerEmpty").style.display = "none";
    const q = kind === "diff" ? "against=tiled&" : "";
    img.src = `/api/scenes/${encodeURIComponent(state.currentId)}/image/${kind}?${q}v=${state.engine?.contentVersion || 0}-${state.engine?.paramVersion || "p1"}`;
  }
  const names = {
    fused: ["Fused radiance", "Log radiance heat map · native resolution"],
    rgb: ["Linear RGB", "Tone-mapped target gamut presentation"],
    gamut: ["Gamut mask", "Compressed pixels remain visible at the boundary"],
    detail: ["Noise detail layer", "Generator-authored fusion detail"],
    diff: ["Path difference", "Native-radiance residual against tiled/full synthesis"],
  };
  const [title, caption] = names[kind] || [`Band ${kind.replace("band", "")}`, "Registered source band preview"];
  $("viewerTitle").textContent = title;
  $("viewerCaption").textContent = caption;
  $("viewerMeta").textContent = `${state.engine?.scale?.selectedPath || ""} · x${state.engine?.scale?.downsampleFactor || 1}`;
}

function updateDiff() {
  if (!state.currentId || !state.engine?.progress?.committed) {
    $("diffImage").removeAttribute("src");
    $("diffCaption").style.display = "";
    return;
  }
  $("diffCaption").style.display = "none";
  $("diffImage").src = `/api/scenes/${encodeURIComponent(state.currentId)}/image/diff?against=tiled&v=${state.engine.contentVersion}`;
}

function renderCompare(report) {
  const pill = $("comparePill");
  pill.textContent = report.consistent ? "CONSISTENT" : "MISMATCH";
  pill.className = `result-pill ${report.consistent ? "pass" : "fail"}`;
  const fmtPeak = (v) => fmt(v, 5);
  const paths = report.paths || [];
  $("compareBody").innerHTML = `
    <div class="compare-metrics">
      <div><span>Live peak</span><b>${fmtPeak(report.livePeak)}</b></div>
      <div><span>Resume peak</span><b>${fmtPeak(report.resumePeak)}</b></div>
      <div><span>Full peak</span><b>${fmtPeak(report.fullPeak)}</b></div>
      <div><span>Tiled peak</span><b>${fmtPeak(report.tiledPeak)}</b></div>
    </div>
    <div class="path-list">
      ${paths.map((p) => `<div class="path-row"><span>${esc(p.pathA || "live")} ↔ ${esc(p.pathB || "full")}</span><span class="${p.consistent ? "ok" : "bad"}">${p.consistent ? "equal" : `mismatch @ ${p.firstMismatch?.x ?? "?"},${p.firstMismatch?.y ?? "?"}`}</span></div>`).join("")}
    </div>
    ${report.consistent ? "" : `<p style="margin-top:8px;color:var(--red)">First mismatch block ${report.firstMismatchBlock} at ${report.firstMismatch?.x ?? "?"},${report.firstMismatch?.y ?? "?"}. Inspect E_SCALE metrics in the API response.</p>`}
  `;
}

async function runAction(path, body = null) {
  if (!state.currentId) return false;
  try {
    const data = await api(`/api/scenes/${encodeURIComponent(state.currentId)}/${path}`, {
      method: "POST",
      body: body ? JSON.stringify(body) : undefined,
    });
    if (data?.engine) state.engine = data.engine;
    else if (data?.segments) state.engine = data;
    await pollStatus();
    return true;
  } catch (err) {
    toast(err.code || "ACTION ERROR", err.message || String(err), "error");
    return false;
  }
}

async function retrySegment(index) {
  if (await runAction("retry", { index, clearFault: true })) {
    await runAction("resume");
  }
}

async function comparePaths() {
  if (!state.currentId) return;
  $("compareBtn").disabled = true;
  $("comparePill").textContent = "running";
  try {
    const report = await api(`/api/scenes/${encodeURIComponent(state.currentId)}/compare`, { method: "POST" });
    state.compare = report;
    renderCompare(report);
    toast("COMPARE COMPLETE", "Live, resume, full and tiled paths are consistent.", "");
  } catch (err) {
    if (err.report) {
      state.compare = err.report;
      renderCompare(err.report);
    }
    toast(err.code || "COMPARE ERROR", err.message || String(err), "error");
  } finally {
    $("compareBtn").disabled = false;
  }
}

function bindControls() {
  $("orderRange").addEventListener("input", (e) => $("orderOut").textContent = e.target.value);
  $("biasRange").addEventListener("input", (e) => $("biasOut").textContent = Number(e.target.value).toFixed(2));
  $("sharpRange").addEventListener("input", (e) => $("sharpOut").textContent = Number(e.target.value).toFixed(2));
  $("detailRange").addEventListener("input", (e) => $("detailOut").textContent = Number(e.target.value).toFixed(2));
  $("startBtn").addEventListener("click", () => runAction("start"));
  $("pauseBtn").addEventListener("click", () => runAction("pause"));
  $("resumeBtn").addEventListener("click", () => runAction("resume"));
  $("stepBtn").addEventListener("click", () => runAction("step"));
  $("compareBtn").addEventListener("click", comparePaths);
  $("exportBtn").addEventListener("click", () => {
    if (state.currentId) window.location.href = `/api/scenes/${encodeURIComponent(state.currentId)}/export`;
  });
  $("applyParams").addEventListener("click", async () => {
    if (!state.engine) return;
    const cfg = { ...state.engine.config };
    cfg.path = $("pathSelect").value;
    cfg.interpolationOrder = Number($("orderRange").value);
    cfg.weightBias = Number($("biasRange").value);
    cfg.weightSharpness = Number($("sharpRange").value);
    cfg.detailStrength = Number($("detailRange").value);
    cfg.gamutIntent = $("intentSelect").value;
    cfg.maxWorkers = Number($("workersInput").value);
    cfg.pixelBufferLimit = Number($("bufferInput").value);
    if (await runAction("params", cfg)) {
      toast("PARAMETERS APPLIED", "Committed segments remain frozen; only pending segments will use this version.");
    }
  });
  document.querySelectorAll("[data-hist]").forEach((el) => {
    el.addEventListener("click", () => {
      state.histKind = el.dataset.hist;
      document.querySelectorAll("[data-hist]").forEach((x) => x.classList.toggle("active", x === el));
      loadHistogram();
    });
  });
  window.addEventListener("resize", () => {
    if (state.result) {
      loadHistogram();
      loadChromaticity();
    }
  });
}

boot();
