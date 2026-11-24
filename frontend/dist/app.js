"use strict";

const $ = (id) => document.getElementById(id);
const state = {
  app: null,
  session: null,
  preview: null,
  run: null,
  selectedStage: null,
  selectedBlock: null,
  autoTimer: null,
  previewTimer: null,
  previewSeq: 0,
  exactSeq: 0,
  paused: false,
  logs: [],
};

function bindings() {
  return (window.go && window.go.main && window.go.main.App) ||
         (window.go && window.go.workbench && window.go.workbench.Workbench) || null;
}

function api(name) {
  const app = bindings();
  if (!app || !app[name]) throw new Error("Wails binding not ready: " + name);
  return app[name].bind(app);
}

function log(message, kind) {
  const now = new Date().toLocaleTimeString();
  state.logs.unshift(`[${now}] ${message}`);
  state.logs = state.logs.slice(0, 160);
  const pane = $("logPane");
  pane.innerHTML = "";
  state.logs.forEach(line => {
    const div = document.createElement("div");
    div.className = kind === "error" && line === state.logs[0] ? "error" : "";
    div.textContent = line;
    pane.appendChild(div);
  });
}

function setBadge(text, cls) {
  const el = $("stateBadge");
  el.textContent = text.toUpperCase();
  el.className = "badge " + (cls || "idle");
}

function params() {
  return {
    lowThreshold: Number($("lowThreshold").value),
    highThreshold: Number($("highThreshold").value),
    operator: $("operator").value,
    openRadius: Number($("openRadius").value),
    closeRadius: Number($("closeRadius").value),
    thinIterations: Number($("thinIterations").value),
    pruneLength: Number($("pruneLength").value)
  };
}

function refreshOutputs() {
  const p = params();
  $("lowOut").textContent = p.lowThreshold;
  $("highOut").textContent = p.highThreshold;
  $("openOut").textContent = p.openRadius;
  $("closeOut").textContent = p.closeRadius;
  $("thinOut").textContent = p.thinIterations;
  $("pruneOut").textContent = p.pruneLength;
}

function applyParams(p) {
  if (!p) return;
  $("lowThreshold").value = p.lowThreshold;
  $("highThreshold").value = p.highThreshold;
  $("operator").value = p.operator;
  $("openRadius").value = p.openRadius;
  $("closeRadius").value = p.closeRadius;
  $("thinIterations").value = p.thinIterations;
  $("pruneLength").value = p.pruneLength;
  refreshOutputs();
}

async function bootstrap() {
  if (!bindings()) {
    setTimeout(bootstrap, 180);
    return;
  }
  try {
    const app = await api("Bootstrap")();
    state.app = app;
    $("algoVersion").textContent = app.algorithmVersion;
    $("checkpointPath").textContent = app.checkpointPath || "--";
    renderSamples(app.samples || []);
    if (app.session) applySession(app.session);
    log("工作台已就绪，本地样本与 SQLite 检查点可用。", "ok");
  } catch (err) {
    log(String(err), "error");
    setBadge("error", "failed");
  }
}

function applySession(session) {
  state.session = session;
  state.preview = null;
  state.run = null;
  state.selectedStage = null;
  $("sourceImage").src = session.imageUrl;
  $("previewImage").removeAttribute("src");
  $("stageImage").removeAttribute("src");
  $("heatmapImage").removeAttribute("src");
  $("stageStrip").innerHTML = "";
  applyParams(session.params);
  $("paramVersion").textContent = session.paramVersion;
  const m = session.manifest;
  $("sampleMeta").textContent = `${m.width}×${m.height} · ${m.channels === 1 ? "单通道" : "三通道"} · ${m.noisy ? "含噪" : "干净"} · ${m.lowContrast ? "低对比度" : "常规对比"}`;
  $("mCoverage").textContent = (m.truth.edgeCoverage * 100).toFixed(2) + "%";
  log(`载入样本 ${m.name}；指纹 ${m.fingerprint.slice(0, 16)}…`);
}

function renderSamples(samples) {
  const list = $("sampleList");
  list.innerHTML = "";
  samples.forEach(sample => {
    const btn = document.createElement("button");
    btn.className = "sample" + (state.session && state.session.name === sample.name ? " active" : "");
    btn.innerHTML = `<strong>${sample.name}</strong><span>${sample.width}×${sample.height} · ${sample.channels}ch · 覆盖 ${(sample.edgeCoverage * 100).toFixed(2)}%</span>`;
    btn.onclick = async () => {
      try {
        const session = await api("LoadSample")(sample.name);
        applySession(session);
        renderSamples(samples);
        setBadge("idle", "idle");
      } catch (err) { log(String(err), "error"); }
    };
    list.appendChild(btn);
  });
}

function previewRequest() {
  clearTimeout(state.autoTimer);
  clearTimeout(state.previewTimer);
  state.previewTimer = setTimeout(async () => {
    const seq = ++state.previewSeq;
    const versionAtRequest = $("paramVersion").textContent;
    try {
      const response = await api("Preview")(params());
      if (seq !== state.previewSeq) return;
      if (response.error) {
        showError(response.error);
        return;
      }
      state.preview = response.view;
      const view = response.view;
      $("previewImage").src = view.maskUrl;
      $("previewMeta").textContent = `${view.maskWidth}×${view.maskHeight} · factor ${view.policy.downsampleFactor} · ${view.placeholder ? "简化" : "全精度"}`;
      $("mLatency").textContent = view.elapsedMs.toFixed(1) + " ms";
      $("mLatency").className = view.elapsedMs <= 100 ? "ok" : "error";
      $("mPolicy").textContent = `factor ${view.policy.downsampleFactor} · ${view.placeholder ? "结构简化" : "无简化"}`;
      $("mFrame").textContent = "PREVIEW";
      $("mVersion").textContent = "version " + view.paramVersion;
      $("mCoverage").textContent = (view.stats.coverage * 100).toFixed(2) + "%";
      $("mTruth").textContent = view.truthDeviation.toFixed(2) + " px";
      $("paramVersion").textContent = view.paramVersion;
      setBadge("preview", "preview");
      log(`预览 ${view.elapsedMs.toFixed(1)}ms · factor ${view.policy.downsampleFactor} · 参数 ${view.paramVersion}`);
      if ($("autoExact").checked) scheduleExact(view.paramVersion || versionAtRequest);
    } catch (err) {
      showError(err);
    }
  }, 35);
}

function scheduleExact(version) {
  clearTimeout(state.autoTimer);
  state.autoTimer = setTimeout(() => runExact(version), 260);
}

async function runExact(version) {
  if (!state.preview) return;
  const seq = ++state.exactSeq;
  try {
    const lockedVersion = version || state.preview.paramVersion || "";
    setBadge("exact", "running");
    $("mFrame").textContent = "EXACT…";
    const response = await api("Exact")({ params: state.preview.params, expectedVersion: lockedVersion });
    if (seq !== state.exactSeq) return;
    if (response.divergence) {
      const d = response.divergence;
      $("mPixels").textContent = (d.pixelDiffFraction * 100).toFixed(2) + "%";
      if (d.firstMismatch) {
        log(`首差异像素 (${d.firstMismatch.x},${d.firstMismatch.y}) ch${d.firstMismatch.channel}`, "error");
      }
    }
    if (response.error) {
      showError(response.error);
      setBadge("diverge", "failed");
      if (response.view) applyRun(response.view);
      return;
    }
    applyRun(response.view);
    log(`精确重算完成 ${response.view.elapsedMs.toFixed(1)}ms；预览/精确容差内一致。`, "ok");
  } catch (err) {
    showError(err);
    setBadge("error", "failed");
  }
}

async function runMode() {
  if (!state.session) return;
  try {
    setBadge("running", "running");
    const response = await api("Run")({ mode: $("runMode").value, params: params() });
    if (response.error) { showError(response.error); setBadge("failed", "failed"); return; }
    applyRun(response.view);
    setBadge("complete", "complete");
    log(`${response.view.mode} 路径完成：${response.view.status} · ${response.view.elapsedMs.toFixed(1)}ms`, "ok");
  } catch (err) { showError(err); }
}

function applyRun(view) {
  state.run = view;
  state.selectedStage = view.stages[view.stages.length - 1] || null;
  $("mFrame").textContent = view.frameState.toUpperCase();
  $("mVersion").textContent = "version " + view.paramVersion;
  $("stageTitle").textContent = "精确阶段掩膜";
  renderStageStrip(view.stages);
  renderRunSummary(view);
  renderMatrix(view.blockStates);
  renderGeometry(view.stats);
  if (view.heatmapUrl) $("heatmapImage").src = view.heatmapUrl;
  $("mPixels").textContent = view.heatmapUrl ? "已生成" : "--";
  $("mTruth").textContent = view.truthDeviation.toFixed(2) + " px";
  $("mPool").textContent = `pool ${view.poolStats.maxParallel}/${view.poolStats.workerCount} · ${(view.poolStats.bufferBytes / 1024).toFixed(0)} KiB`;
  $("exportBtn").disabled = false;
  setBadge(view.status === "complete" ? "exact" : "failed", view.status === "complete" ? "exact" : "failed");
  if (state.selectedStage) selectStage(state.selectedStage);
}

function renderStageStrip(stages) {
  const strip = $("stageStrip");
  strip.innerHTML = "";
  stages.forEach(stage => {
    const chip = document.createElement("button");
    chip.className = "stage-chip";
    chip.innerHTML = `<img src="${stage.maskUrl}" alt="${stage.stage}"><div><strong>${stage.stage}</strong><span>覆盖 ${(stage.coverage * 100).toFixed(2)}% · ${stage.durationMs.toFixed(1)}ms</span></div>`;
    chip.onclick = () => selectStage(stage, chip);
    if (state.selectedStage && state.selectedStage.stage === stage.stage) chip.classList.add("active");
    strip.appendChild(chip);
  });
}

function selectStage(stage, chip) {
  state.selectedStage = stage;
  $("stageImage").src = stage.maskUrl;
  $("stageMeta").textContent = `${stage.stage} · 覆盖 ${(stage.coverage * 100).toFixed(2)}% · ${stage.fingerprint.slice(0, 12)}…`;
  $("stageTitle").textContent = "精确阶段掩膜 · " + stage.stage;
  document.querySelectorAll(".stage-chip").forEach(el => el.classList.remove("active"));
  if (chip) chip.classList.add("active");
}

function renderRunSummary(view) {
  const failed = view.failedBlocks || [];
  const restored = view.restoredCommits || 0;
  const waits = view.poolStats.waitReasons || {};
  const waitText = Object.keys(waits).length ? Object.entries(waits).map(([k, v]) => `${k} ×${v}`).join("，") : "无";
  $("runSummary").innerHTML = `
    <div>模式 <b>${view.mode}</b> · 状态 <b class="${view.status === "complete" ? "ok" : "error"}">${view.status}</b></div>
    <div>参数版本 <b>${view.paramVersion}</b> · 算法 <b>${view.algorithmVersion}</b></div>
    <div>提交块 <b>${view.commitSequence.length}</b> · 恢复块 <b>${restored}</b> · 失败块 <b class="${failed.length ? "error" : "ok"}">${failed.length}</b></div>
    <div>并行峰值 <b>${view.poolStats.maxParallel}/${view.poolStats.workerCount}</b> · 单线程像素上限 <b>${view.poolStats.perWorkerPixels}</b></div>
    <div>缓冲等待 <b>${waitText}</b></div>
    <div>检查点作废 <b>${view.checkpointInvalidated ? "是" : "否"}</b> ${view.checkpointReason || ""}</div>
    <div>真实几何偏差 <b>${view.truthDeviation.toFixed(2)} px</b></div>`;
  $("runStatus").textContent = `${view.status} · ${view.elapsedMs.toFixed(1)}ms`;
}

function renderMatrix(blockStates) {
  const host = $("blockMatrix");
  host.innerHTML = "";
  if (!blockStates || !blockStates.length) return;
  const cols = blockStates[0].states.length;
  blockStates.forEach(row => {
    const div = document.createElement("div");
    div.className = "matrix-row";
    div.style.setProperty("--cols", cols);
    const label = document.createElement("span");
    label.className = "matrix-label";
    label.textContent = row.stage;
    div.appendChild(label);
    row.states.forEach((blockState, i) => {
      const cell = document.createElement("button");
      cell.className = "block-cell " + blockState;
      cell.title = `${row.stage} #${i}: ${blockState}`;
      cell.onclick = () => selectBlock(row.stage, i, blockState, cell);
      div.appendChild(cell);
    });
    host.appendChild(div);
  });
}

function selectBlock(stage, index, blockState, cell) {
  state.selectedBlock = { stage, index, state: blockState };
  document.querySelectorAll(".block-cell").forEach(el => el.classList.remove("selected"));
  cell.classList.add("selected");
  $("blockDetail").textContent = `${stage} / block ${index} → ${blockState}`;
  $("retryBtn").disabled = blockState !== "failed";
}

function renderGeometry(stats) {
  const wrap = $("geometryTable");
  if (!stats) return;
  let html = `<table><thead><tr><th>类型</th><th>通道</th><th>真值</th><th>观测</th><th>偏差 px</th></tr></thead><tbody>`;
  (stats.lines || []).forEach((line, i) => {
    html += `<tr><td>直线 ${i}</td><td>${line.channel}</td><td>(${line.truthX1.toFixed(1)},${line.truthY1.toFixed(1)})–(${line.truthX2.toFixed(1)},${line.truthY2.toFixed(1)})</td><td>(${line.x1.toFixed(1)},${line.y1.toFixed(1)})–(${line.x2.toFixed(1)},${line.y2.toFixed(1)})</td><td>${line.endpointErr.toFixed(2)}</td></tr>`;
  });
  (stats.circles || []).forEach((c, i) => {
    html += `<tr><td>圆形 ${i}</td><td>${c.channel}</td><td>c(${c.truthCx.toFixed(1)},${c.truthCy.toFixed(1)}) d${c.truthDiameter.toFixed(1)}</td><td>c(${c.cx.toFixed(1)},${c.cy.toFixed(1)}) d${c.diameter.toFixed(1)}</td><td>${Math.max(c.centerErr, c.diameterErr).toFixed(2)}</td></tr>`;
  });
  html += `</tbody></table>`;
  wrap.innerHTML = html;
}

function showError(error) {
  const code = error.code || "E_UNKNOWN";
  const field = error.field ? " / " + error.field : "";
  const details = error.details ? "\n" + JSON.stringify(error.details, null, 2) : "";
  log(`${code}${field}: ${error.message}${details}`, "error");
  $("mFrame").textContent = code;
  setBadge(code, "failed");
}

async function exportDiagnostics() {
  try {
    const result = await api("ExportDiagnostics")();
    log(`诊断已导出：${result.path}（${result.fileCount} 个文件）`, "ok");
  } catch (err) { showError(err); }
}

async function retryBlock() {
  if (!state.selectedBlock) return;
  try {
    const response = await api("RetryBlock")({ stage: state.selectedBlock.stage, block: state.selectedBlock.index });
    if (response.error) { showError(response.error); return; }
    applyRun(response.view);
    log(`已重试 ${state.selectedBlock.stage} #${state.selectedBlock.index}`, "ok");
  } catch (err) { showError(err); }
}

function attachEvents() {
  ["operator", "lowThreshold", "highThreshold", "openRadius", "closeRadius", "thinIterations", "pruneLength"].forEach(id => {
    $(id).addEventListener("input", () => { refreshOutputs(); previewRequest(); });
    $(id).addEventListener("change", () => { refreshOutputs(); previewRequest(); });
  });
  $("exactBtn").onclick = () => runExact(state.preview ? state.preview.paramVersion : "");
  $("runBtn").onclick = runMode;
  $("exportBtn").onclick = exportDiagnostics;
  $("retryBtn").onclick = retryBlock;
  $("pauseBtn").onclick = () => {
    state.paused = !state.paused;
    api("SetPaused")(state.paused).catch(showError);
    $("pauseBtn").textContent = state.paused ? "继续" : "暂停";
    setBadge(state.paused ? "paused" : "running", "running");
  };
  $("cancelBtn").onclick = () => { api("Cancel")().catch(showError); log("已请求取消当前任务。"); };
  $("clearLog").onclick = () => { state.logs = []; $("logPane").textContent = ""; };
  $("refreshStats").onclick = () => { if (state.run) renderGeometry(state.run.stats); };
}

attachEvents();
bootstrap();
