const api = window.go && window.go.main && window.go.main.App;

let state = null;
let selectedBlock = null;

const $ = (id) => document.getElementById(id);

function call(name, ...args) {
  if (!api) return Promise.reject(new Error("Wails runtime is not available. Run the app with wails dev/build."));
  return api[name](...args);
}

async function boot() {
  bind();
  try {
    state = await call("Initialize");
    render();
    toast("Local project loaded and checkpoint state verified.");
  } catch (error) {
    showError(error);
    $("statusLine").textContent = "Initialization failed. Check the local project and SQLite checkpoint.";
  }
}

function bind() {
  $("initializeBtn").onclick = () => run(() => call("Initialize"));
  $("applyHeight").onclick = () => run(() => call(
    "EditHeightDelta",
    Number($("heightBlock").value),
    Number($("heightX").value),
    Number($("heightY").value),
    Number($("heightDelta").value),
  ));
  $("applyMaterial").onclick = () => run(() => call(
    "EditMaterial",
    $("layerSelect").value,
    Number($("biasX").value),
    Number($("biasY").value),
    Number($("biasZ").value),
    Number($("strength").value),
  ));
  $("applyOperator").onclick = () => run(() => call("SetDifferenceOperator", $("operatorSelect").value));
  $("retryBtn").onclick = () => run(() => call("RetryFailedBlocks"));
  $("exportBtn").onclick = () => run(() => call("ExportNormals"));
  $("rebuildBtn").onclick = () => {
    if (confirm("Wipe checkpoints and rebuild every block?")) run(() => call("RebuildAll"));
  };
}

async function run(action) {
  setBusy(true);
  try {
    state = await action();
    render();
    toast("Incremental bake committed.");
  } catch (error) {
    showError(error);
    try {
      state = await call("GetState");
      render();
    } catch (_) {}
  } finally {
    setBusy(false);
  }
}

function setBusy(busy) {
  document.body.style.cursor = busy ? "progress" : "";
  $("statusBadge").textContent = busy ? "baking" : (state?.lastReport?.failed ? "failed" : "ready");
  $("statusBadge").className = `badge ${busy ? "neutral" : (state?.lastReport?.failed ? "bad" : "good")}`;
}

function render() {
  if (!state) return;
  renderMeta();
  renderControls();
  renderMetrics();
  drawViews();
  renderSeams();
  renderInvalidation();
  renderBudget();
  renderBlocks();
  $("statusLine").textContent = state.statusMessage || "Incremental diagnostics ready.";
}

function renderMeta() {
  const project = state.project;
  $("projectMeta").innerHTML = [
    ["seed", project.seed],
    ["revision", project.revision],
    ["blocks", project.blockCount],
    ["seams", project.seamCount],
    ["algorithm", project.algorithmVersion],
    ["param", short(project.parameterVersion, 10)],
  ].map(([label, value]) => `<div><span>${label}</span><strong title="${value}">${value}</strong></div>`).join("");
}

function renderControls() {
  if ($("heightBlock").options.length === 0) {
    $("heightBlock").innerHTML = state.blocks.map((block) =>
      `<option value="${block.id}">#${block.id} ${escapeHtml(block.name)}</option>`).join("");
  }
  if ($("layerSelect").options.length === 0) {
    $("layerSelect").innerHTML = ["rock", "sand", "grass"].map((id) => `<option value="${id}">${id}</option>`).join("");
  }
}

function renderMetrics() {
  const report = state.lastReport || {};
  const seams = state.seams || [];
  const maxAngle = seams.reduce((max, seam) => Math.max(max, seam.finalMaxAngleDeg || 0), 0);
  const violations = seams.reduce((sum, seam) => sum + (seam.violations?.length || 0), 0);
  const values = [
    ["cache hits", report.cacheHits || 0],
    ["rebaked", report.rebaked || 0],
    ["failed", report.failed || 0],
    ["max seam angle", `${maxAngle.toFixed(3)}°`],
    ["attributed raw seams", violations],
  ];
  $("metrics").innerHTML = values.map(([label, value]) =>
    `<div class="metric"><span>${label}</span><strong>${value}</strong></div>`).join("");
}

function drawViews() {
  const blocks = state.blocks || [];
  const maxX = blocks.reduce((max, block) => Math.max(max, block.originX + block.width), 1);
  const maxY = blocks.reduce((max, block) => Math.max(max, block.originY + block.height), 1);
  drawCanvas($("normalCanvas"), maxX, maxY, (block, pixel) => {
    const offset = pixel * 3;
    return [block.colors[offset], block.colors[offset + 1], block.colors[offset + 2]];
  });
  drawCanvas($("slopeCanvas"), maxX, maxY, (block, pixel) => slopeColor(block.slopes[pixel]));
  const label = selectedBlock ? `Selected block #${selectedBlock.id} ${selectedBlock.name}` : "Select a block by clicking the canvas.";
  $("normalLegend").textContent = label;
}

function drawCanvas(canvas, width, height, colorFn) {
  const margin = 8;
  const scale = Math.max(3, Math.floor(Math.min((canvas.width - margin * 2) / width, (canvas.height - margin * 2) / height)));
  const drawWidth = width * scale;
  const drawHeight = height * scale;
  canvas.width = drawWidth + margin * 2;
  canvas.height = drawHeight + margin * 2;
  const ctx = canvas.getContext("2d");
  ctx.fillStyle = "#070b11";
  ctx.fillRect(0, 0, canvas.width, canvas.height);

  for (const block of state.blocks) {
    const blockCanvas = document.createElement("canvas");
    blockCanvas.width = block.width;
    blockCanvas.height = block.height;
    const blockCtx = blockCanvas.getContext("2d");
    const image = blockCtx.createImageData(block.width, block.height);
    for (let pixel = 0; pixel < block.width * block.height; pixel++) {
      const rgb = colorFn(block, pixel);
      image.data[pixel * 4] = rgb[0];
      image.data[pixel * 4 + 1] = rgb[1];
      image.data[pixel * 4 + 2] = rgb[2];
      image.data[pixel * 4 + 3] = 255;
    }
    blockCtx.putImageData(image, 0, 0);
    const x = margin + block.originX * scale;
    const y = margin + block.originY * scale;
    ctx.imageSmoothingEnabled = false;
    ctx.drawImage(blockCanvas, x, y, block.width * scale, block.height * scale);
    ctx.strokeStyle = block.failed ? "#ff6b78" : (selectedBlock?.id === block.id ? "#63a8ff" : "#34465b");
    ctx.lineWidth = selectedBlock?.id === block.id ? 2 : 1;
    ctx.strokeRect(x + 0.5, y + 0.5, block.width * scale - 1, block.height * scale - 1);
    ctx.fillStyle = "rgba(8, 13, 20, 0.74)";
    ctx.fillRect(x + 4, y + 4, 28, 16);
    ctx.fillStyle = "#dce7f5";
    ctx.font = "10px monospace";
    ctx.fillText(`#${block.id}`, x + 8, y + 15);
  }

  drawSeamOverlay(ctx, scale, margin);
  canvas.onclick = (event) => {
    const rect = canvas.getBoundingClientRect();
    const sx = (event.clientX - rect.left) * (canvas.width / rect.width);
    const sy = (event.clientY - rect.top) * (canvas.height / rect.height);
    const gx = Math.floor((sx - margin) / scale);
    const gy = Math.floor((sy - margin) / scale);
    const block = state.blocks.find((candidate) =>
      gx >= candidate.originX && gy >= candidate.originY &&
      gx < candidate.originX + candidate.width && gy < candidate.originY + candidate.height);
    if (block) {
      selectedBlock = block;
      $("heightBlock").value = String(block.id);
      $("heightX").value = Math.floor(block.width / 2);
      $("heightY").value = Math.floor(block.height / 2);
      drawViews();
    }
  };
}

function drawSeamOverlay(ctx, scale, margin) {
  for (const seam of state.seams || []) {
    const a = state.blocks.find((block) => block.id === seam.blockA);
    const b = state.blocks.find((block) => block.id === seam.blockB);
    if (!a || !b) continue;
    const violating = (seam.violations?.length || 0) > 0;
    const x1 = margin + (a.originX + a.width - 1) * scale;
    const y1 = margin + a.originY * scale;
    const x2 = margin + b.originX * scale;
    const y2 = margin + b.originY * scale;
    const horizontal = a.originY + a.height - 1 === b.originY;
    ctx.strokeStyle = violating ? "rgba(255,107,120,0.92)" : "rgba(84,208,167,0.6)";
    ctx.lineWidth = 2;
    ctx.beginPath();
    if (horizontal) {
      ctx.moveTo(margin + a.originX * scale, margin + (a.height - 1 + a.originY) * scale);
      ctx.lineTo(margin + (a.originX + a.width) * scale, margin + (a.height - 1 + a.originY) * scale);
    } else {
      ctx.moveTo(x1, y1);
      ctx.lineTo(x1, margin + (a.originY + a.height) * scale);
    }
    ctx.stroke();
    if (violating) {
      const first = seam.violations[0];
      const text = `${first.attribution} ${seam.maxAngleDeg.toFixed(2)}°`;
      ctx.font = "10px monospace";
      const tx = horizontal ? margin + (a.originX + Math.min(first.ax, 3)) * scale : x1 + 4;
      const ty = horizontal ? margin + (a.originY + a.height - 1) * scale - 4 : y1 + 12;
      ctx.fillStyle = "rgba(12,18,27,0.9)";
      ctx.fillRect(tx, ty - 10, ctx.measureText(text).width + 8, 14);
      ctx.fillStyle = "#ffc0c5";
      ctx.fillText(text, tx + 4, ty);
    }
  }
}

function renderSeams() {
  const container = $("seams");
  if (!state.seams?.length) {
    container.innerHTML = `<div class="seam-item muted">No seam reports.</div>`;
    return;
  }
  container.innerHTML = state.seams.map((seam) => {
    const tags = (seam.violations || []).slice(0, 4).map((violation) =>
      `<span class="tag ${violation.attribution}">${violation.attribution}</span> A(${violation.ax},${violation.ay}) B(${violation.bx},${violation.by}) ${violation.angleDeg.toFixed(2)}°`).join("<br>");
    return `<div class="seam-item">
      <strong>${seam.seamId}</strong> <span class="muted">#${seam.blockA}/#${seam.blockB}</span><br>
      <span class="muted">threshold ${seam.thresholdDeg}° · raw ${seam.maxAngleDeg.toFixed(3)}° · final ${seam.finalMaxAngleDeg.toFixed(3)}° · ${seam.correctedPixels} corrected</span>
      <div style="margin-top:6px">${tags || '<span class="muted">continuous</span>'}</div>
    </div>`;
  }).join("");
}

function renderInvalidation() {
  const entries = state.invalidation || [];
  $("invalidation").innerHTML = entries.length ? entries.map((entry) => `<div class="stack-item">
    <strong>#${entry.blockId}</strong>
    <span class="tag ${entry.invalid ? (entry.reused ? "reused" : "rebaked") : "reused"}">${entry.invalid ? (entry.reused ? "reused" : "invalid") : "cache hit"}</span>
    <div class="muted">${escapeHtml(entry.reason || "")}</div>
    ${entry.dependencies?.length ? `<div class="muted">depends: ${entry.dependencies.map((id) => `#${id}`).join(" → ")}</div>` : ""}
  </div>`).join("") : `<div class="stack-item muted">No invalidation report yet.</div>`;
}

function renderBudget() {
  const report = state.lastReport || {};
  const used = report.peakMemoryBytes || 0;
  const limit = state.budget.normalMapMemoryBytes || 1;
  const pct = Math.min(100, used / limit * 100);
  $("budget").innerHTML = `
    <div class="budget-labels"><span>peak memory</span><span>${formatBytes(used)} / ${formatBytes(limit)}</span></div>
    <div class="budget-bar"><div style="width:${pct}%"></div></div>
    <div class="budget-labels"><span>workers ${report.workerCount || state.budget.maxParallelism}</span><span>buffer ${state.budget.perThreadPixelBuffer}px</span></div>`;
  const queue = state.queue || [];
  $("queue").innerHTML = queue.length ? queue.map((item) => `<div class="queue-item">
    <strong>#${item.blockId}</strong> <span class="muted">seq ${item.sequence}</span><br>
    <span class="muted">${escapeHtml(item.reason)} · ${formatBytes(item.estimatedBytes)}</span>
  </div>`).join("") : `<div class="queue-item muted">No blocks entered the wait queue.</div>`;
}

function renderBlocks() {
  $("blocks").innerHTML = (state.blocks || []).map((block) => `<div class="block-item">
    <span class="block-id">#${block.id}</span>
    <span class="block-name">${escapeHtml(block.name)}<br><span class="muted">${block.width}×${block.height} · bakes ${block.bakeCount} · ${block.rebakeMs.toFixed(3)}ms</span></span>
    <span><span class="tag ${block.failed ? "failed" : (block.status === "rebaked" ? "rebaked" : "reused")}">${block.status}</span>${block.failed ? `<br><button data-retry-block="${block.id}" style="margin-top:4px;padding:4px 7px;font-size:10px">retry</button>` : ""}</span>
  </div>`).join("");
  document.querySelectorAll("[data-retry-block]").forEach((button) => {
    button.onclick = () => run(() => call("RetryBlock", Number(button.dataset.retryBlock)));
  });
}

function slopeColor(slope) {
  const t = Math.max(0, Math.min(1, (slope || 0) / 60));
  if (t < 0.5) {
    const u = t * 2;
    return [30 + 120 * u, 90 + 120 * u, 220 - 150 * u];
  }
  const u = (t - 0.5) * 2;
  return [150 + 105 * u, 210 - 170 * u, 70 - 50 * u];
}

function formatBytes(value) {
  if (value > 1024 * 1024) return `${(value / 1024 / 1024).toFixed(2)} MiB`;
  if (value > 1024) return `${(value / 1024).toFixed(1)} KiB`;
  return `${value} B`;
}

function short(value, length) {
  return value ? `${value.slice(0, length)}…` : "n/a";
}

function escapeHtml(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[char]);
}

function showError(error) {
  console.error(error);
  toast(String(error), true);
  $("statusBadge").textContent = "error";
  $("statusBadge").className = "badge bad";
}

let toastTimer = null;
function toast(message, error = false) {
  const element = $("toast");
  element.textContent = message;
  element.className = `toast${error ? " error" : ""}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => element.classList.add("hidden"), 4200);
}

boot();
