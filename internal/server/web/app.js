const $ = (id) => document.getElementById(id);
const state = {
  scenes: [],
  data: null,
  selectedSegment: null,
  variant: 0,
  lastImageKey: "",
  imageTimer: 0,
  bandSignature: "",
  busy: false,
  controlsDirty: false,
};

const bandColors = {
  B450: "#5b8cff",
  B530: "#69d987",
  B610: "#f1a94f",
  B680: "#ff655c",
  B750: "#d95d9c",
};

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
    ...options,
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const err = body.error || { message: response.statusText };
    throw new Error(`${err.code || "E_HTTP"}: ${err.message || response.statusText}`);
  }
  return body;
}

function toast(message, error = false) {
  const el = $("toast");
  el.textContent = message;
  el.classList.toggle("error", error);
  el.classList.add("show");
  clearTimeout(el._timer);
  el._timer = setTimeout(() => el.classList.remove("show"), 4200);
}

async function loadScenes() {
  const body = await api("/api/scenes");
  state.scenes = body.scenes || [];
  $("sceneSelect").innerHTML = state.scenes.map((s) =>
    `<option value="${s.key}">${escapeHTML(s.name)}</option>`).join("");
  if (state.scenes.length) {
    $("sceneSelect").value = state.scenes[0].key;
    await createSession();
  }
}

async function createSession() {
  const scene = $("sceneSelect").value;
  const schemaVersion = Number($("versionSelect").value);
  const mode = $("modeSelect").value;
  setBusy(true, "LOADING SCHEMA");
  try {
    const body = await api("/api/session", {
      method: "POST",
      body: JSON.stringify({ scene, schemaVersion, mode }),
    });
    state.data = body;
    state.selectedSegment = null;
    state.lastImageKey = "";
    state.controlsDirty = false;
    render();
    toast(`Loaded ${scene} schema v${schemaVersion} via ${mode}`);
  } catch (err) {
    toast(err.message, true);
  } finally {
    setBusy(false);
  }
}

async function poll() {
  try {
    const body = await api("/api/session");
    if (body.session === null) return;
    state.data = body;
    render();
  } catch (err) {
    if (!state.busy) console.warn(err);
  }
}

function setBusy(value, label) {
  state.busy = value;
  if (value && label) {
    $("statusPill").className = "status running";
    $("statusPill").querySelector("span").textContent = label;
  }
}

function render() {
  const data = state.data;
  if (!data || !data.view) return;
  const view = data.view;
  const pct = view.totalSegments ? Math.round(100 * view.committedSegments / view.totalSegments) : 0;
  $("committedCount").textContent = view.committedSegments;
  $("totalCount").textContent = `/ ${view.totalSegments}`;
  $("progressPct").textContent = `${pct}%`;
  $("progressFill").style.width = `${pct}%`;
  $("sceneTitle").textContent = view.sceneName.toUpperCase();
  $("peakRadiance").textContent = data.patches ? fmt(view.segments.reduce((m, s) => Math.max(m, s.peakRadiance || 0), 0), 4) : "—";
  const oog = view.segments.reduce((n, s) => n + (s.outOfGamut || 0), 0);
  $("oogCount").textContent = view.committedSegments ? oog.toLocaleString() : "—";
  $("sceneSize").textContent = `${view.width}×${view.height}`;
  $("bandCountLabel").textContent = `${view.bandIDs.length} BANDS`;
  $("toleranceBadge").textContent = `TOL ${fmt(data.truth?.patchTolerance || 0, 4)}`;

  renderStatus(data, view);
  renderPipeline(view);
  renderSegments(view);
  renderControls(view);
  renderBands(view);
  renderPatches(data.patches || []);
  renderHistogram(data.histograms || []);
  renderInvalidation(view.lastInvalidation);
  renderSnapshot(data, view);
  renderVerification(data.verification);
  refreshImages(view);
}

function renderStatus(data, view) {
  const pill = $("statusPill");
  if (data.lastError || view.segments.some((s) => s.state === "failed")) {
    pill.className = "status failed";
    pill.querySelector("span").textContent = "ATTENTION";
  } else if (data.running && !view.paused) {
    pill.className = "status running";
    pill.querySelector("span").textContent = "PROCESSING";
  } else if (view.committedSegments === view.totalSegments) {
    pill.className = "status complete";
    pill.querySelector("span").textContent = "COMPLETE";
  } else if (view.paused) {
    pill.className = "status idle";
    pill.querySelector("span").textContent = "PAUSED";
  } else {
    pill.className = "status idle";
    pill.querySelector("span").textContent = "READY";
  }
  $("pauseBtn").disabled = !data.running || view.paused;
  $("resumeBtn").disabled = data.running && !view.paused;
  const failed = view.segments.find((s) => s.id === state.selectedSegment && s.state === "failed");
  $("retryBtn").disabled = !failed || data.running;
}

function renderPipeline(view) {
  const complete = view.committedSegments === view.totalSegments;
  document.querySelectorAll(".pipeline span").forEach((el, i) => {
    el.className = complete ? "done" : "";
    if (!complete && view.committedSegments > 0 && i < 5) el.className = "active";
  });
}

function renderSegments(view) {
  const grid = $("segmentGrid");
  const cols = new Set(view.segments.map((s) => s.x)).size || 4;
  grid.style.gridTemplateColumns = `repeat(${Math.min(cols, 6)}, 1fr)`;
  grid.innerHTML = view.segments.map((seg) => `
    <button class="segment ${seg.state} ${seg.reused ? "reused" : ""} ${state.selectedSegment === seg.id ? "selected" : ""}"
      data-id="${seg.id}" title="Segment ${seg.id} · ${seg.w}×${seg.h} @ ${seg.x},${seg.y}${seg.error ? "\n" + escapeHTML(seg.error) : ""}">
      <b>${String(seg.id).padStart(2, "0")}</b>
      <small>${seg.state.slice(0, 4)}${seg.outOfGamut ? ` · ${seg.outOfGamut} oog` : ""}</small>
    </button>`).join("");
  grid.querySelectorAll(".segment").forEach((el) => {
    el.addEventListener("click", () => {
      state.selectedSegment = Number(el.dataset.id);
      render();
    });
  });
  if (state.selectedSegment !== null) {
    const seg = view.segments.find((s) => s.id === state.selectedSegment);
    if (seg) {
      $("segmentHint").textContent = seg.error
        ? `Segment ${seg.id}: ${seg.error}`
        : `Segment ${seg.id} · ${seg.state} · ${seg.reused ? "persistent cache reused" : "live result"}`;
    }
  }
}

function renderControls(view) {
  const settings = view.settings;
  if (!state.controlsDirty) {
    $("fusionStrength").value = settings.fusionStrength;
    $("fusionPower").value = settings.fusionPower;
    $("interpolationOrder").value = String(settings.interpolationOrder);
    $("gamutIntent").value = settings.gamutIntent;
    $("detailGain").value = settings.detailGain;
  }
  $("fusionStrengthOut").textContent = fmt(settings.fusionStrength, 1);
  $("fusionPowerOut").textContent = fmt(settings.fusionPower, 1);
  $("detailGainOut").textContent = fmt(settings.detailGain, 2);
  if ($("bandSelect").options.length !== view.bandIDs.length) {
    $("bandSelect").innerHTML = view.bandIDs.map((id) => `<option>${id}</option>`).join("");
  }
}

function renderBands(view) {
  const signature = view.bandIDs.join("|");
  if (signature === state.bandSignature) return;
  state.bandSignature = signature;
  $("bandGrid").innerHTML = view.bandIDs.map((id) => `
    <article class="band-card">
      <header><b>${id}</b><span>fused</span></header>
      <img data-band="${id}" alt="${id} band">
    </article>`).join("");
}

function refreshImages(view) {
  const key = `${view.sceneID}:${view.committedSegments}:${view.bandVersion}:${view.paramVersion}:${view.sceneFingerprint}`;
  if (key === state.lastImageKey) return;
  state.lastImageKey = key;
  clearTimeout(state.imageTimer);
  state.imageTimer = setTimeout(() => {
    const stamp = encodeURIComponent(key);
    const composite = $("compositeImage");
    composite.onload = () => {
      composite.classList.add("ready");
      $("imagePlaceholder").style.display = "none";
    };
    composite.src = `/api/image?kind=composite&overlay=1&v=${stamp}`;
    document.querySelectorAll("#bandGrid img").forEach((img) => {
      img.src = `/api/image?kind=band&band=${encodeURIComponent(img.dataset.band)}&v=${stamp}`;
    });
    $("imageStamp").textContent = `${view.committedSegments}/${view.totalSegments} COMMITTED`;
  }, 180);
}

function renderPatches(patches) {
  const tbody = $("patchTable");
  if (!patches.length) {
    tbody.innerHTML = `<tr><td colspan="4" class="empty-cell">Awaiting committed composite.</td></tr>`;
    return;
  }
  tbody.innerHTML = patches.map((p) => `
    <tr class="${p.pass ? "pass" : "fail"}">
      <td>${escapeHTML(p.name)}</td>
      <td>${rgb(p.expected)}</td>
      <td>${rgb(p.actual)}</td>
      <td>${fmt(p.delta, 5)}</td>
    </tr>`).join("");
}

function renderHistogram(histograms) {
  const canvas = $("histogramCanvas");
  const ratio = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  canvas.width = Math.max(280, rect.width * ratio);
  canvas.height = 246 * ratio;
  const ctx = canvas.getContext("2d");
  ctx.scale(ratio, ratio);
  const w = canvas.width / ratio, h = canvas.height / ratio;
  ctx.clearRect(0, 0, w, h);
  ctx.strokeStyle = "rgba(255,255,255,.055)";
  ctx.lineWidth = 1;
  for (let i = 0; i < 4; i++) {
    const y = 18 + i * (h - 42) / 3;
    ctx.beginPath(); ctx.moveTo(0, y); ctx.lineTo(w, y); ctx.stroke();
  }
  if (!histograms.length) {
    ctx.fillStyle = "#536267"; ctx.font = "10px monospace";
    ctx.fillText("AWAITING COMPLETE COMPOSITE", 12, h / 2);
    $("histLegend").innerHTML = "";
    return;
  }
  const maxCount = Math.max(1, ...histograms.flatMap((hst) => hst.bins));
  const colors = ["#5b8cff", "#69d987", "#f1a94f", "#ff655c", "#d95d9c", "#6de3d0"];
  histograms.forEach((hst, idx) => {
    const color = colors[idx % colors.length];
    const bins = hst.bins;
    ctx.beginPath();
    bins.forEach((count, i) => {
      const x = i / (bins.length - 1) * w;
      const y = h - 24 - count / maxCount * (h - 44);
      if (i === 0) ctx.moveTo(x, h - 24); else ctx.lineTo(x, h - 24);
      ctx.lineTo(x, y);
    });
    ctx.lineTo(w, h - 24);
    ctx.strokeStyle = color;
    ctx.globalAlpha = idx === histograms.length - 1 ? 1 : .7;
    ctx.lineWidth = idx === histograms.length - 1 ? 2 : 1.25;
    ctx.stroke();
    ctx.globalAlpha = 1;
  });
  $("histLegend").innerHTML = histograms.map((hst, idx) =>
    `<span><i style="background:${colors[idx % colors.length]}"></i>${escapeHTML(hst.label)} · peak ${fmt(hst.peak, 3)}</span>`).join("");
}

function renderInvalidation(report) {
  const el = $("invalidationList");
  if (!report) {
    el.className = "ledger empty";
    el.textContent = "No edits in this session.";
    return;
  }
  el.className = "ledger";
  const rows = report.segments || [];
  el.innerHTML = rows.map((item) => `
    <div class="ledger-row ${item.reused ? "reused" : ""}">
      <span class="id">#${String(item.segmentID).padStart(2, "0")}</span>
      <span class="stage">${escapeHTML(item.stage)}</span>
      <span class="reason">${item.reused ? "reused committed cache" : escapeHTML((item.reasons || []).join(" + ") || "pending")}</span>
    </div>`).join("") || "No segments.";
}

function renderSnapshot(data, view) {
  const entries = [
    ["Scene fingerprint", view.sceneFingerprint],
    ["Parameter version", view.paramVersion],
    ["Band metadata", view.bandVersion],
    ["Algorithm", view.algorithmVersion],
    ["Commit sequence", (view.commitSequence || []).join(" → ") || "—"],
    ["Checkpoint", data.dbPath],
  ];
  $("snapshotMeta").innerHTML = entries.map(([k, v]) =>
    `<dt>${k}</dt><dd>${escapeHTML(String(v || "—"))}</dd>`).join("");
  const reset = $("snapshotReset");
  if (view.snapshotReset) {
    reset.classList.remove("hidden");
    reset.textContent = `SNAPSHOT DISCARDED · ${view.snapshotReset}`;
  } else {
    reset.classList.add("hidden");
  }
}

function renderVerification(report) {
  const badge = $("verifyBadge");
  if (!report) {
    badge.className = "badge";
    badge.textContent = "UNVERIFIED";
    $("verifySummary").textContent = "Run three-path verification to compare exact pixel buffers.";
    return;
  }
  const liveDiff = report.liveVsFull?.firstDifference ? "DIFF" : "0 px";
  const resumeDiff = report.resumeVsFull?.firstDifference ? "DIFF" : "0 px";
  $("liveDiffCount").textContent = liveDiff;
  $("resumeDiffCount").textContent = resumeDiff;
  $("maxDelta").textContent = fmt(report.liveVsFull?.maxAbsDelta || 0, 8);
  $("peakDelta").textContent = fmt(report.peakRadianceDelta || 0, 8);
  badge.className = `badge ${report.equal ? "pass" : "fail"}`;
  badge.textContent = report.equal ? "EXACT MATCH" : "MISMATCH";
  $("verifySummary").textContent = report.equal
    ? `3 paths · 0 differing pixels · ${report.full.commitSequence.length} segments · resume loaded from store: ${report.resume.resumedFromStore}`
    : `First mismatch: ${JSON.stringify(report.liveVsFull?.firstDifference || report.resumeVsFull?.firstDifference)}`;
  drawHeatmap(report.heatmap || [], state.data?.view?.width, state.data?.view?.height);
}

function drawHeatmap(values, width, height) {
  const canvas = $("heatmapCanvas");
  const ratio = window.devicePixelRatio || 1;
  const rect = canvas.getBoundingClientRect();
  canvas.width = Math.max(260, rect.width * ratio);
  canvas.height = 142 * ratio;
  const ctx = canvas.getContext("2d");
  ctx.scale(ratio, ratio);
  const w = canvas.width / ratio, h = canvas.height / ratio;
  ctx.fillStyle = "#0a0f11";
  ctx.fillRect(0, 0, w, h);
  const max = Math.max(0, ...values);
  if (!values.length) return;
  const cols = width || Math.ceil(Math.sqrt(values.length));
  const rows = height || Math.ceil(values.length / cols);
  const cw = w / cols, ch = h / rows;
  for (let y = 0; y < rows; y++) {
    for (let x = 0; x < cols; x++) {
      const v = values[y * cols + x] || 0;
      const t = max > 0 ? v / max : 0;
      ctx.fillStyle = max === 0 ? "#162023" : `rgb(${Math.round(255 * Math.min(1, t * 2.2))},${Math.round(255 * Math.max(0, 1.6 - t))},${Math.round(255 * Math.max(0, .5 - t))})`;
      ctx.fillRect(x * cw, y * ch, Math.ceil(cw), Math.ceil(ch));
    }
  }
  ctx.strokeStyle = "rgba(109,227,208,.2)";
  ctx.strokeRect(.5, .5, w - 1, h - 1);
  if (max === 0) {
    ctx.fillStyle = "#536267";
    ctx.font = "10px monospace";
    ctx.fillText("0 DIFFERING PIXELS · EXACT MATCH", 12, h / 2);
  }
}

async function applySettings() {
  if (!state.data) return;
  const settings = {
    interpolationOrder: Number($("interpolationOrder").value),
    maxRegistrationOffset: state.data.view.settings.maxRegistrationOffset,
    fusionPower: Number($("fusionPower").value),
    fusionStrength: Number($("fusionStrength").value),
    gamutIntent: $("gamutIntent").value,
    targetGamut: state.data.view.settings.targetGamut,
    detailGain: Number($("detailGain").value),
  };
  try {
    const body = await api("/api/session/settings", { method: "POST", body: JSON.stringify(settings) });
    const report = body.invalidation;
    const affected = report.segments.filter((s) => s.stage !== "none").length;
    toast(`Parameter edit invalidated ${affected} segments; resume to recompute.`);
    state.controlsDirty = false;
    state.data = body.state;
    render();
  } catch (err) {
    toast(err.message, true);
  }
}

async function replaceBand() {
  if (!state.data) return;
  state.variant++;
  try {
    const body = await api("/api/session/band", {
      method: "POST",
      body: JSON.stringify({
        band: $("bandSelect").value,
        variant: state.variant + Number($("variantSelect").value),
      }),
    });
    const report = body.invalidation;
    const affected = report.segments.filter((s) => s.stage !== "none").length;
    toast(`Band replacement invalidated ${affected} segments; unaffected cache remains reusable.`);
    state.data = body.state;
    render();
  } catch (err) {
    toast(err.message, true);
  }
}

async function verifyPaths() {
  if (!state.data) return;
  $("verifyBtn").disabled = true;
  $("verifyBtn").textContent = "Verifying…";
  try {
    const report = await api("/api/verify", { method: "POST" });
    state.data.verification = report;
    renderVerification(report);
    toast(report.equal ? "Realtime, resume, and full paths match exactly." : "Path mismatch detected.", !report.equal);
  } catch (err) {
    toast(err.message, true);
  } finally {
    $("verifyBtn").disabled = false;
    $("verifyBtn").textContent = "Verify 3 Paths";
  }
}

function rgb(v) {
  return (v || []).map((x) => fmt(x, 3)).join(" / ");
}
function fmt(v, digits = 3) {
  const n = Number(v);
  if (!Number.isFinite(n)) return "—";
  return n.toFixed(digits);
}
function escapeHTML(text) {
  return String(text).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

$("loadBtn").addEventListener("click", createSession);
$("verifyBtn").addEventListener("click", verifyPaths);
$("pauseBtn").addEventListener("click", () => api("/api/session/pause", { method: "POST" }).then(() => poll()).catch((e) => toast(e.message, true)));
$("resumeBtn").addEventListener("click", () => api("/api/session/resume", { method: "POST" }).then(() => poll()).catch((e) => toast(e.message, true)));
$("retryBtn").addEventListener("click", () => {
  if (state.selectedSegment === null) return;
  api("/api/session/retry", { method: "POST", body: JSON.stringify({ segment: state.selectedSegment }) })
    .then(() => poll()).catch((e) => toast(e.message, true));
});
$("applySettingsBtn").addEventListener("click", applySettings);
$("replaceBandBtn").addEventListener("click", replaceBand);
["fusionStrength", "fusionPower", "detailGain"].forEach((id) => {
  $(id).addEventListener("input", () => {
    state.controlsDirty = true;
    $("fusionStrengthOut").textContent = fmt($("fusionStrength").value, 1);
    $("fusionPowerOut").textContent = fmt($("fusionPower").value, 1);
    $("detailGainOut").textContent = fmt($("detailGain").value, 2);
  });
});
["interpolationOrder", "gamutIntent"].forEach((id) => {
  $(id).addEventListener("change", () => { state.controlsDirty = true; });
});
window.addEventListener("resize", () => {
  if (state.data) {
    renderHistogram(state.data.histograms || []);
    renderVerification(state.data.verification);
  }
});

loadScenes().catch((err) => toast(err.message, true));
setInterval(poll, 550);
