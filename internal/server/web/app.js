const $ = (id) => document.getElementById(id);

const state = {
  bootstrap: null,
  session: null,
  comparison: null,
  selectedBlock: null,
  logs: [],
  pollTimer: 0,
  pollSerial: 0,
  busy: false,
  sourceURL: "",
  targetURL: "",
  heatmapURL: "",
  targetVisualKey: "",
  seenRestore: "",
  seenGeneration: -1,
};

const channelColors = {
  R: "#ff6b6b",
  G: "#5eea8b",
  B: "#5f9dff",
  A: "#d9e7e1",
  Y: "#ffbd52",
};

document.addEventListener("DOMContentLoaded", init);

async function init() {
  wireEvents();
  try {
    const boot = await api("/api/bootstrap");
    state.bootstrap = boot;
    $("healthPill").textContent = "LOCAL / ONLINE";
    $("healthPill").classList.add("live");
    $("seedPill").textContent = `SEED ${boot.seed}`;
    $("algoPill").textContent = `ALGO ${boot.algorithm}`;
    renderPreflight(boot.preflight);
    populateSamples(boot.samples);
    populateDescriptors(boot.descriptors);
    addLog("BOOT", `样本清单已加载：${boot.samples.length} 图像 / ${boot.descriptors.length} 目标描述`, "ok");
    const first = $("#sampleSelect").value;
    if (first) {
      chooseDescriptorForSample(first);
      await loadSourcePreview(first);
      await applyDescriptor(true);
    }
  } catch (err) {
    addLog("E_BOOT", errorMessage(err), "error");
    toast("启动失败", errorMessage(err), "error");
    $("healthPill").textContent = "OFFLINE";
    $("healthPill").classList.add("warn");
  }
}

function wireEvents() {
  $("sampleSelect").addEventListener("change", async () => {
    const sampleID = $("sampleSelect").value;
    chooseDescriptorForSample(sampleID);
    updateSourceFacts();
    await loadSourcePreview(sampleID);
  });
  $("descriptorSelect").addEventListener("change", () => {
    const descriptor = descriptorByID($("descriptorSelect").value);
    if (descriptor) {
      applyDescriptorToControls(descriptor);
    }
  });
  $("identityButton").addEventListener("click", () => {
    const info = sampleByID($("sampleSelect").value);
    if (!info) return;
    const channels = info.channels || layoutChannels(info.layout);
    $("permutationInput").value = Array.from({ length: channels }, (_, i) => i).join(",");
  });
  $("applyButton").addEventListener("click", () => applyDescriptor(false));
  $("runButton").addEventListener("click", runSession);
  $("pauseButton").addEventListener("click", pauseSession);
  $("cancelButton").addEventListener("click", cancelSession);
  $("compareButton").addEventListener("click", comparePaths);
  $("faultButton").addEventListener("click", injectFaultAndRun);
  $("exportPngButton").addEventListener("click", () => exportSession("png"));
  $("exportRawButton").addEventListener("click", () => exportSession("twimg"));
}

async function api(path, options = {}) {
  const request = {
    method: options.method || "GET",
    headers: { "Content-Type": "application/json", ...(options.headers || {}) },
  };
  if (options.body !== undefined) {
    request.body = JSON.stringify(options.body);
  }
  const response = await fetch(path, request);
  const text = await response.text();
  let data = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = { message: text };
  }
  if (!response.ok) {
    const err = new Error(data?.message || `${response.status} ${response.statusText}`);
    err.status = response.status;
    err.data = data;
    throw err;
  }
  return data;
}

function populateSamples(samples) {
  const select = $("sampleSelect");
  select.innerHTML = "";
  const groups = { "8 BIT": [], "16 BIT": [] };
  for (const sample of samples) {
    (sample.bit_depth === 8 ? groups["8 BIT"] : groups["16 BIT"]).push(sample);
  }
  for (const [label, entries] of Object.entries(groups)) {
    if (!entries.length) continue;
    const group = document.createElement("optgroup");
    group.label = label;
    for (const sample of entries) {
      const option = document.createElement("option");
      option.value = sample.id;
      option.textContent = `${sample.id} · ${sample.width}×${sample.height} · ${sample.layout.toUpperCase()}`;
      group.appendChild(option);
    }
    select.appendChild(group);
  }
}

function populateDescriptors(descriptors) {
  const select = $("descriptorSelect");
  select.innerHTML = "";
  for (const descriptor of descriptors) {
    const option = document.createElement("option");
    option.value = descriptor.id;
    option.textContent = descriptor.id;
    select.appendChild(option);
  }
  updateSourceFacts();
}

function chooseDescriptorForSample(sampleID) {
  const descriptors = state.bootstrap?.descriptors || [];
  const match = descriptors.find((item) => item.source_image === sampleID);
  if (match) {
    $("descriptorSelect").value = match.id;
    applyDescriptorToControls(match);
  } else {
    const info = sampleByID(sampleID);
    if (info) {
      $("depthSelect").value = String(info.bit_depth);
      $("layoutSelect").value = info.layout;
      $("fillSelect").value = "zero";
      $("permutationInput").value = Array.from({ length: info.channels }, (_, i) => i).join(",");
    }
  }
  updateSourceFacts();
}

function applyDescriptorToControls(descriptor) {
  if (!descriptor) return;
  if (descriptor.source_image && sampleByID(descriptor.source_image)) {
    $("sampleSelect").value = descriptor.source_image;
    updateSourceFacts();
    loadSourcePreview(descriptor.source_image);
  }
  $("depthSelect").value = String(descriptor.target_bit_depth);
  $("layoutSelect").value = descriptor.target_layout;
  $("fillSelect").value = descriptor.fill;
  $("permutationInput").value = (descriptor.permutation || []).join(",");
}

function sampleByID(id) {
  return state.bootstrap?.samples.find((item) => item.id === id);
}

function descriptorByID(id) {
  return state.bootstrap?.descriptors.find((item) => item.id === id);
}

function layoutChannels(layout) {
  return { gray: 1, rgb: 3, rgba: 4, bgr: 3, bgra: 4 }[layout] || 0;
}

function updateSourceFacts() {
  const info = sampleByID($("sampleSelect").value);
  if (!info) return;
  $("sampleMeta").textContent = `${info.width}×${info.height}`;
  $("sourceFacts").innerHTML = [
    fact("RESOLUTION", `${info.width} × ${info.height}`),
    fact("BIT DEPTH", `${info.bit_depth} bit`),
    fact("LAYOUT", info.layout.toUpperCase()),
    fact("CHANNELS", `${info.channels}`),
  ].join("");
  $("sourceFooter").textContent = `${info.file} · hash ${shortHash(info.content_hash)}`;
  $("sourceLabel").textContent = `${info.bit_depth} BIT / ${info.layout.toUpperCase()}`;
}

function fact(label, value) {
  return `<div><b>${escapeHTML(value)}</b><span>${escapeHTML(label)}</span></div>`;
}

function buildDescriptor() {
  const info = sampleByID($("sampleSelect").value);
  if (!info) throw new Error("请选择源图像");
  const permutation = parsePermutation($("permutationInput").value);
  const depth = Number($("depthSelect").value);
  const targetLayout = $("layoutSelect").value;
  const fill = $("fillSelect").value;
  const signature = `${info.id}-${depth}-${targetLayout}-${fill}-${permutation.join("_")}`;
  return {
    id: `ui-${signature}`,
    source_image: info.id,
    source_layout: info.layout,
    target_layout: targetLayout,
    target_bit_depth: depth,
    permutation,
    fill,
  };
}

function parsePermutation(value) {
  const text = String(value).trim();
  if (!/^\d+(\s*,\s*\d+)*$/.test(text)) {
    throw new Error("置换顺序必须是逗号分隔的非负整数");
  }
  return text.split(",").map((part) => Number(part.trim()));
}

function buildConfig() {
  const info = sampleByID($("sampleSelect").value);
  const blockWidth = Number($("blockWidthInput").value);
  const blockHeight = Number($("blockHeightInput").value);
  return {
    block_width: blockWidth,
    block_height: blockHeight,
    max_workers: Number($("workersInput").value),
    buffer_pixels_per_worker: Number($("bufferInput").value),
    queue_depth: Number($("queueInput").value),
    parameter_version: $("parameterInput").value.trim() || "params-v1",
    session_key: info?.id || "",
  };
}

async function applyDescriptor(silent) {
  if (state.busy) return;
  let descriptor;
  try {
    descriptor = buildDescriptor();
  } catch (err) {
    toast("描述不合法", err.message, "error");
    addLog("E_PARA", err.message, "error");
    return;
  }
  const info = sampleByID(descriptor.source_image);
  if (!info) return;
  state.busy = true;
  try {
    if (state.session && state.session.image_id === info.id) {
      const same =
        descriptorEquals(state.session.descriptor, descriptor) &&
        state.session.parameter_version === $("parameterInput").value.trim();
      if (same) {
        render();
        if (!silent) toast("描述未变化", "当前快照与目标描述一致", "ok");
        return;
      }
      if (state.session.progress.committed > 0 || state.session.restore?.restored_blocks > 0) {
        const proceed = confirm(
          `目标描述或参数版本变化将整体作废当前快照。\n\n已提交 ${state.session.progress.committed}/${state.session.progress.total} 块。\n已提交结果不会被静默改写；本次操作会显式标记全部块失效并清空持久化检查点。\n\n继续？`
        );
        if (!proceed) {
          addLog("SNAPSHOT", "用户取消快照作废", "warn");
          return;
        }
      }
      const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/reconfigure`, {
        method: "POST",
        body: {
          descriptor,
          parameter_version: $("parameterInput").value.trim(),
        },
      });
      setSession(data.session);
      const count = data.invalidated_blocks?.length || 0;
      addLog("SNAPSHOT", `显式作废 ${count} 个块；原因：target_descriptor_changed / parameter_version_changed`, "warn");
      toast("快照已整体作废", `${count} 个块被标记失效，等待重新计算`, "ok");
      return;
    }

    const config = buildConfig();
    try {
      const data = await api("/api/sessions", {
        method: "POST",
        body: { image_id: info.id, descriptor, config },
      });
      setSession(data.session);
      if (!silent) toast("会话已创建", "已加载匹配的持久化检查点（如有）", "ok");
    } catch (err) {
      if (err.status === 409 && err.data?.session) {
        setSession(err.data.session);
        if (err.data.session.descriptor) {
          applyDescriptorToControls(err.data.session.descriptor);
        }
        addLog("E_STATE", "同名会话已存在；请修改描述后显式应用以作废快照", "warn");
        if (!silent) toast("会话已存在", "使用现有快照，未静默替换已提交块", "error");
        return;
      }
      throw err;
    }
  } catch (err) {
    handleError(err, "应用描述失败");
  } finally {
    state.busy = false;
    render();
  }
}

function descriptorEquals(a, b) {
  if (!a || !b) return false;
  return (
    a.source_layout === b.source_layout &&
    a.target_layout === b.target_layout &&
    a.target_bit_depth === b.target_bit_depth &&
    a.fill === b.fill &&
    JSON.stringify(a.permutation) === JSON.stringify(b.permutation)
  );
}

function setSession(session) {
  state.session = session;
  state.comparison = null;
  state.selectedBlock = null;
  state.targetVisualKey = "";
  if (session.restore?.attempted) {
    const restoreKey = JSON.stringify(session.restore);
    if (state.seenRestore !== restoreKey) {
      state.seenRestore = restoreKey;
      if (session.restore.invalidated) {
        addLog("RESTORE", `版本失配，整体作废：${(session.restore.reasons || []).join(", ")}`, "warn");
      } else if (session.restore.used) {
        addLog("RESTORE", `从检查点恢复 ${session.restore.restored_blocks} 个已提交块`, "ok");
      }
    }
  }
  if ((session.generations?.length || 0) - 1 > state.seenGeneration) {
    state.seenGeneration = session.generations.length - 1;
  }
  render();
  updateSourceFacts();
  loadSourcePreview(session.image_id);
  ensureTargetVisuals(true);
  if (session.running) schedulePoll(250);
}

async function loadSourcePreview(sampleID) {
  const url = `/api/samples/${encodeURIComponent(sampleID)}/preview`;
  try {
    const blobURL = await fetchBlobURL(url);
    if (state.sourceURL) URL.revokeObjectURL(state.sourceURL);
    state.sourceURL = blobURL;
    const image = $("sourceImage");
    image.src = blobURL;
    image.classList.add("visible");
    $("sourcePlaceholder").style.display = "none";
  } catch (err) {
    addLog("E_DECODE", `源图预览失败：${errorMessage(err)}`, "error");
  }
}

async function ensureTargetVisuals(force = false) {
  if (!state.session || !state.session.complete) {
    clearTargetVisuals();
    return;
  }
  const key = `${state.session.descriptor_version}:${state.session.progress.committed}`;
  if (!force && state.targetVisualKey === key) return;
  state.targetVisualKey = key;
  const sessionID = encodeURIComponent(state.session.session_id);
  try {
    const [targetBlob, heatBlob] = await Promise.all([
      fetchBlobURL(`/api/sessions/${sessionID}/preview?side=target&v=${encodeURIComponent(key)}`),
      fetchBlobURL(`/api/sessions/${sessionID}/heatmap?v=${encodeURIComponent(key)}`),
    ]);
    if (state.targetURL) URL.revokeObjectURL(state.targetURL);
    if (state.heatmapURL) URL.revokeObjectURL(state.heatmapURL);
    state.targetURL = targetBlob;
    state.heatmapURL = heatBlob;
    const target = $("targetImage");
    target.src = targetBlob;
    target.classList.add("visible");
    $("targetPlaceholder").style.display = "none";
    const heat = $("heatmapImage");
    heat.src = heatBlob;
    heat.classList.add("visible");
    $("heatmapPlaceholder").style.display = "none";
    $("targetLabel").textContent = `${state.session.descriptor.target_bit_depth} BIT / ${state.session.descriptor.target_layout.toUpperCase()}`;
    $("targetFooter").textContent = `${state.session.width} × ${state.session.height} · ${state.session.progress.total} blocks · hash ${shortHash(state.session.descriptor_version)}`;
  } catch (err) {
    addLog("E_PREVIEW", errorMessage(err), "warn");
  }
}

function clearTargetVisuals() {
  $("targetImage").classList.remove("visible");
  $("targetPlaceholder").style.display = "";
  $("heatmapImage").classList.remove("visible");
  $("heatmapPlaceholder").style.display = "";
  $("targetLabel").textContent = "—";
  state.targetVisualKey = "";
}

async function fetchBlobURL(url) {
  const response = await fetch(url, { cache: "no-store" });
  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    try {
      const data = await response.json();
      message = data.message || message;
    } catch {}
    throw new Error(message);
  }
  return URL.createObjectURL(await response.blob());
}

async function runSession() {
  if (!state.session || state.session.running) return;
  try {
    const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/run`, {
      method: "POST",
      body: { max_blocks: 0 },
    });
    setSession(data.session);
    addLog("RUN", "分段处理已启动；每提交一个像素块即原子写入检查点", "ok");
    schedulePoll(180);
  } catch (err) {
    handleError(err, "启动转换失败");
  }
}

async function pauseSession() {
  if (!state.session) return;
  try {
    const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/pause`, { method: "POST", body: {} });
    setSession(data.session);
    addLog("PAUSE", "已请求暂停；仅停止调度新块，已完成块保留", "warn");
  } catch (err) {
    handleError(err, "暂停失败");
  }
}

async function cancelSession() {
  if (!state.session) return;
  if (!confirm("取消后不会承诺完整导出；重启仅能依据已持久化且版本匹配的块恢复。继续？")) return;
  try {
    const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/cancel`, { method: "POST", body: {} });
    setSession(data.session);
    addLog("CANCEL", "已请求取消；未调度块保持待处理", "warn");
  } catch (err) {
    handleError(err, "取消失败");
  }
}

function schedulePoll(delay = 350) {
  clearTimeout(state.pollTimer);
  const serial = ++state.pollSerial;
  state.pollTimer = setTimeout(() => pollSession(serial), delay);
}

async function pollSession(serial) {
  if (!state.session || serial !== state.pollSerial) return;
  try {
    const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}`);
    if (serial !== state.pollSerial) return;
    const wasRunning = state.session.running;
    state.session = data.session;
    if (data.comparison) state.comparison = data.comparison;
    render();
    if (data.session.running) {
      schedulePoll(320);
    } else if (wasRunning) {
      addLog(
        "RUN",
        `调度结束：${data.session.progress.committed}/${data.session.progress.total} 已提交，${data.session.progress.failed} 失败`,
        data.session.progress.failed ? "warn" : "ok"
      );
      ensureTargetVisuals(true);
    }
  } catch (err) {
    addLog("POLL", errorMessage(err), "error");
    if (state.session?.running) schedulePoll(900);
  }
}

async function comparePaths() {
  if (!state.session) return;
  $("compareButton").disabled = true;
  $("compareStatus").textContent = "RUNNING…";
  addLog("COMPARE", "开始实时分段 / 断点恢复 / 全量重算三路径比对", "ok");
  try {
    const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/compare`, {
      method: "POST",
      body: {},
    });
    state.comparison = data.comparison;
    addLog("E_DIFF", "三路径逐像素、量化误差与直方图完全一致", "ok");
    toast("三路径一致", `已验证 ${state.session.width}×${state.session.height} 全部像素`, "ok");
  } catch (err) {
    if (err.data?.comparison) {
      state.comparison = err.data.comparison;
      const diff = state.comparison.first_diff || err.data.details?.diff;
      if (diff) {
        addLog(
          "E_DIFF",
          `首个不一致：阶段=${diff.stage} x=${diff.x} y=${diff.y} channel=${diff.channel} block=${diff.block_index} left=${JSON.stringify(diff.left)} right=${JSON.stringify(diff.right)}`,
          "error"
        );
        toast("三路径不一致", `阶段 ${diff.stage} · 坐标 (${diff.x}, ${diff.y}) · 块 ${diff.block_index}`, "error");
      }
    } else {
      handleError(err, "三路径比对失败");
    }
  } finally {
    renderPaths();
    render();
  }
}

async function injectFaultAndRun() {
  if (!state.session || state.session.running) return;
  const pending = state.session.blocks.find((block) => block.state === "pending");
  const target = pending || state.session.blocks.find((block) => block.state === "failed");
  if (!target) {
    toast("无可注入块", "当前没有待处理或失败块", "error");
    return;
  }
  try {
    await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/blocks/${target.index}/fault`, {
      method: "POST",
      body: { times: 1 },
    });
    addLog("FAULT", `已在块 ${target.index} 注入一次 pixel_stage 异常；仅该块会失败`, "warn");
    await runSession();
  } catch (err) {
    handleError(err, "注入故障失败");
  }
}

async function retryBlock(index) {
  if (!state.session) return;
  try {
    const data = await api(`/api/sessions/${encodeURIComponent(state.session.session_id)}/blocks/${index}/retry`, {
      method: "POST",
      body: {},
    });
    setSession(data.session);
    addLog("RETRY", `块 ${index} 已从 failed → processing → committed`, "ok");
    toast("块重试成功", `块 ${index} 已重新提交`, "ok");
    ensureTargetVisuals(true);
  } catch (err) {
    if (err.data?.session) setSession(err.data.session);
    handleError(err, `块 ${index} 重试失败`);
  }
}

async function exportSession(format) {
  if (!state.session) return;
  const url = `/api/sessions/${encodeURIComponent(state.session.session_id)}/export?format=${format}`;
  try {
    const response = await fetch(url);
    if (!response.ok) {
      const data = await response.json().catch(() => ({}));
      throw new Error(data.message || `${response.status} ${response.statusText}`);
    }
    const blob = await response.blob();
    const disposition = response.headers.get("Content-Disposition") || "";
    const match = disposition.match(/filename="([^"]+)"/);
    const filename = match?.[1] || `${state.session.image_id}.${format === "twimg" ? "twimg" : "png"}`;
    const objectURL = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = objectURL;
    anchor.download = filename;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(objectURL), 2000);
    addLog("EXPORT", `导出 ${format.toUpperCase()}：${filename}`, "ok");
  } catch (err) {
    handleError(err, "导出失败");
  }
}

function render() {
  renderHeader();
  renderProgress();
  renderMetrics();
  renderHistogram();
  renderMapping();
  renderBlockMatrix();
  renderPaths();
  renderLogs();
  renderButtons();
  renderGenerationLog();
}

function renderHeader() {
  const session = state.session;
  if (!session) {
    $("stageTitle").textContent = "等待加载样本";
    return;
  }
  $("stageTitle").textContent = `${session.image_id} → ${session.descriptor.target_layout.toUpperCase()} / ${session.descriptor.target_bit_depth} BIT`;
  $("sessionFacts").innerHTML = `
    <span>SESSION <b>${escapeHTML(session.session_id)}</b></span>
    <span>DESC <b>${shortHash(session.descriptor_version)}</b></span>
    <span>PARAM <b>${escapeHTML(session.parameter_version)}</b></span>
  `;
  $("sourceLabel").textContent = `${session.source_bit_depth} BIT / ${session.source_layout.toUpperCase()}`;
  $("actionMeta").textContent = session.running ? "RUNNING" : session.cancelled ? "CANCELLED" : session.complete ? "COMPLETE" : "READY";
}

function renderProgress() {
  const session = state.session;
  if (!session) return;
  const progress = session.progress;
  const percent = progress.total ? (progress.committed / progress.total) * 100 : 0;
  $("progressFill").style.width = `${percent.toFixed(2)}%`;
  $("progressText").textContent = `${progress.committed} / ${progress.total} blocks committed · ${progress.failed} failed · ${progress.pending} pending`;
  let label = "READY";
  if (session.running) label = "PROCESSING";
  else if (session.cancelled) label = "CANCELLED";
  else if (progress.failed > 0) label = "ATTENTION / RETRY FAILED BLOCKS";
  else if (session.complete) label = "ALL COMMITTED";
  else if (session.paused) label = "PAUSED";
  $("progressState").textContent = label;
}

function renderMetrics() {
  const session = state.session;
  if (!session) return;
  $("metricCommitted").textContent = `${session.progress.committed}`;
  $("metricCache").textContent = session.stats?.cache_hits || session.restore?.restored_blocks || 0;
  $("metricRetries").textContent = session.stats?.retries || 0;
  $("metricWaits").textContent = session.stats?.buffer_queue_waits || 0;
  $("metricWaits").title = session.stats?.buffer_wait_reason || "buffered queue waits";
  const stats = session.quant_stats;
  $("metricMean").textContent = stats ? formatNumber(stats.mean_abs, 3) : "—";
  $("metricMax").textContent = stats ? stats.max_abs : "—";
  $("heatmapScale").textContent = stats ? `MAX ${stats.max_abs}` : "MAX —";
}

function renderButtons() {
  const session = state.session;
  const running = !!session?.running;
  const complete = !!session?.complete;
  const hasPending = session?.blocks?.some((block) => block.state === "pending");
  $("runButton").disabled = !session || running || !hasPending;
  $("pauseButton").disabled = !session || !running;
  $("cancelButton").disabled = !session || !running;
  $("compareButton").disabled = !session || running || state.busy;
  $("faultButton").disabled = !session || running || !hasPending;
  $("exportPngButton").disabled = !session || !complete;
  $("exportRawButton").disabled = !session || !complete;
  $("applyButton").disabled = running || state.busy;
  $("runButton").textContent = complete ? "全部完成" : session?.paused ? "继续处理" : "开始 / 继续";
}

function renderHistogram() {
  const canvas = $("histogramCanvas");
  const ctx = prepareCanvas(canvas);
  ctx.clearRect(0, 0, canvas.clientWidth, canvas.clientHeight);
  const width = canvas.clientWidth;
  const height = canvas.clientHeight;
  ctx.fillStyle = "#080b0b";
  ctx.fillRect(0, 0, width, height);
  ctx.strokeStyle = "#192320";
  ctx.lineWidth = 1;
  for (let i = 1; i < 8; i++) {
    const y = (height / 8) * i;
    ctx.beginPath();
    ctx.moveTo(0, y + 0.5);
    ctx.lineTo(width, y + 0.5);
    ctx.stroke();
  }
  for (let i = 1; i < 8; i++) {
    const x = (width / 8) * i;
    ctx.beginPath();
    ctx.moveTo(x + 0.5, 0);
    ctx.lineTo(x + 0.5, height);
    ctx.stroke();
  }
  const hist = state.session?.histogram;
  if (!hist) {
    ctx.fillStyle = "#4c5b56";
    ctx.font = "11px Cascadia Mono, monospace";
    ctx.fillText("WAITING FOR COMPLETE RESULT", 18, height / 2);
    $("channelLegend").innerHTML = "";
    return;
  }
  const channels = hist.channels || [];
  $("channelLegend").innerHTML = channels
    .map((name) => `<span><i style="background:${channelColor(name)}"></i>${escapeHTML(name)}</span>`)
    .join("");
  const pad = 8;
  let maxLog = 0;
  const logs = hist.counts.map((counts) => counts.map((value) => Math.log1p(value)));
  for (const channel of logs) {
    for (const value of channel) maxLog = Math.max(maxLog, value);
  }
  if (maxLog <= 0) maxLog = 1;
  hist.counts.forEach((counts, channelIndex) => {
    const color = channelColor(channels[channelIndex]);
    const values = logs[channelIndex];
    const points = values.map((value, index) => {
      const x = pad + (index / Math.max(1, values.length - 1)) * (width - pad * 2);
      const y = height - pad - (value / maxLog) * (height - pad * 2);
      return [x, y];
    });
    ctx.beginPath();
    points.forEach(([x, y], index) => (index === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y)));
    ctx.strokeStyle = color;
    ctx.lineWidth = 1.2;
    ctx.stroke();
    ctx.lineTo(width - pad, height - pad);
    ctx.lineTo(pad, height - pad);
    ctx.closePath();
    ctx.fillStyle = hexAlpha(color, 0.08);
    ctx.fill();
  });
  ctx.fillStyle = "#4c5b56";
  ctx.font = "9px Cascadia Mono, monospace";
  ctx.fillText("0", 8, height - 2);
  ctx.fillText("255", width - 26, height - 2);
  ctx.fillText("LOG-SCALED COUNT", 8, 12);
}

function prepareCanvas(canvas) {
  const dpr = window.devicePixelRatio || 1;
  const width = Math.max(1, Math.floor(canvas.clientWidth));
  const height = Math.max(1, Math.floor(canvas.clientHeight));
  if (canvas.width !== Math.floor(width * dpr) || canvas.height !== Math.floor(height * dpr)) {
    canvas.width = Math.floor(width * dpr);
    canvas.height = Math.floor(height * dpr);
  }
  const ctx = canvas.getContext("2d");
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return ctx;
}

function renderMapping() {
  const session = state.session;
  const list = $("mappingList");
  if (!session) {
    $("mappingStatus").textContent = "—";
    list.innerHTML = `<div class="empty-note">尚无目标描述。</div>`;
    return;
  }
  const mapping = session.mapping;
  $("mappingStatus").textContent = mapping.reversible ? "REVERSIBLE" : "E_LOSS";
  $("mappingStatus").style.color = mapping.reversible ? "var(--green)" : "var(--red)";
  const links = (mapping.links || [])
    .map((link) => {
      const target = `${link.target.name}[${link.target.index}]`;
      const source = link.source ? `${link.source.name}[${link.source.index}]` : "—";
      return `<div class="mapping-chip ${link.kind === "fill_zero" ? "loss" : ""}"><span>${escapeHTML(target)} ← ${escapeHTML(source)}</span><b>${escapeHTML(link.kind)}</b></div>`;
    })
    .join("");
  const dropped = (mapping.dropped_channels || [])
    .map((channel) => `<div class="mapping-chip loss"><span>DROPPED ${escapeHTML(channel.name)}[${channel.index}]</span><b>E_LOSS</b></div>`)
    .join("");
  const alpha =
    mapping.alpha_source && session.descriptor.fill === "repeat_alpha"
      ? `<div class="mapping-chip alpha"><span>TARGET ALPHA ← ${escapeHTML(mapping.alpha_source.name)}[${mapping.alpha_source.index}]</span><b>NEAREST</b></div>`
      : "";
  list.innerHTML = links + alpha + dropped;
}

function renderBlockMatrix() {
  const container = $("blockMatrix");
  const session = state.session;
  if (!session?.blocks?.length) {
    container.innerHTML = "";
    $("matrixFooter").textContent = "无块数据";
    return;
  }
  const cols = Math.max(...session.blocks.map((block) => block.col)) + 1;
  container.style.gridTemplateColumns = `repeat(${cols}, 13px)`;
  container.innerHTML = "";
  let invalid = 0;
  let cache = 0;
  const invalidationReasons = new Map();
  for (const block of session.blocks) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = `block-cell ${block.state}`;
    if (block.cache_hit) button.classList.add("cache");
    if (block.invalidated) button.classList.add("invalid");
    if (state.selectedBlock === block.index) button.classList.add("selected");
    const parts = [
      `block ${block.index}`,
      `rect ${block.rect.x},${block.rect.y} ${block.rect.w}×${block.rect.h}`,
      `state ${block.state}`,
      `attempts ${block.attempts}`,
      `cache ${block.cache_hit}`,
    ];
    if (block.invalidated) parts.push(`invalid ${block.invalidation_reason}`);
    if (block.failure) parts.push(`failure ${block.failure}`);
    if (block.output_hash) parts.push(`hash ${shortHash(block.output_hash)}`);
    button.title = parts.join(" · ");
    button.setAttribute("aria-label", parts.join(", "));
    if (block.invalidated) {
      invalid++;
      const reason = block.invalidation_reason || "unspecified";
      invalidationReasons.set(reason, (invalidationReasons.get(reason) || 0) + 1);
    }
    if (block.cache_hit) cache++;
    button.addEventListener("click", () => {
      state.selectedBlock = block.index;
      addLog("BLOCK", parts.join(" · "), block.state === "failed" ? "error" : "ok");
      if (block.state === "failed") retryBlock(block.index);
      else renderBlockMatrix();
    });
    container.appendChild(button);
  }
  const reasons = [...invalidationReasons.entries()].map(([reason, count]) => `${reason}×${count}`).join(", ");
  $("matrixFooter").textContent =
    `${session.blocks.length} blocks · ${cols} columns · cache ${cache} · invalid ${invalid}` +
    `${reasons ? ` (${reasons})` : ""} · click failed block to retry`;
}

function renderPaths() {
  const list = $("pathList");
  const comparison = state.comparison;
  if (!comparison) {
    const stats = state.session?.stats;
    $("compareStatus").textContent = stats?.path ? stats.path.toUpperCase() : "NOT RUN";
    if (!stats) {
      list.innerHTML = `<div class="empty-note">运行自动比对以区分实时分段、断点恢复与全量重算。</div>`;
      return;
    }
    list.innerHTML = pathRow({
      path: stats.path || "current",
      block_recomputes: stats.block_recomputes || 0,
      total_time_ms: stats.total_time_ms || 0,
      cache_hits: stats.cache_hits || 0,
    });
    return;
  }
  $("compareStatus").textContent = comparison.match ? "MATCH / E_DIFF CLEAR" : "E_DIFF";
  $("compareStatus").style.color = comparison.match ? "var(--green)" : "var(--red)";
  list.innerHTML = (comparison.paths || []).map(pathRow).join("");
  if (comparison.first_diff) {
    const diff = comparison.first_diff;
    list.innerHTML += `<div class="mapping-chip loss"><span>${escapeHTML(diff.stage)} · (${diff.x}, ${diff.y}) · ch ${diff.channel} · block ${diff.block_index}</span><b>${escapeHTML(String(diff.left))} ≠ ${escapeHTML(String(diff.right))}</b></div>`;
  }
}

function pathRow(path) {
  return `<div class="path-row">
    <strong>${escapeHTML(path.path || "path")}</strong>
    <span>RECOMPUTE<b>${formatInteger(path.block_recomputes)}</b></span>
    <span>CACHE<b>${formatInteger(path.cache_hits)}</b></span>
    <span>TIME<b>${formatNumber(path.total_time_ms, 2)} ms</b></span>
  </div>`;
}

function renderLogs() {
  const stream = $("logStream");
  $("logCount").textContent = `${state.logs.length} EVENTS`;
  stream.innerHTML = state.logs
    .slice(0, 80)
    .map(
      (entry) => `<div class="log-entry ${entry.kind}">
        <time>${escapeHTML(entry.time)}</time>
        <span class="log-code">${escapeHTML(entry.code)}</span>
        <p>${escapeHTML(entry.message)}</p>
      </div>`
    )
    .join("");
}

function renderGenerationLog() {
  const session = state.session;
  if (!session?.generations?.length) return;
  const index = session.generations.length - 1;
  if (index > state.seenGeneration) {
    state.seenGeneration = index;
    const generation = session.generations[index];
    addLog("VERSION", `快照代际 ${index + 1}：${generation.reason} · 失效块 ${generation.blocks?.length || 0}`, "warn");
    renderLogs();
  }
}

function renderPreflight(preflight) {
  const node = $("preflight");
  if (preflight?.checkpoint_writable && preflight?.data_readable) {
    node.textContent = `PREFLIGHT OK · ${state.bootstrap?.os}/${state.bootstrap?.arch} · ${state.bootstrap?.go_version}`;
    node.classList.add("ok");
  } else {
    node.textContent = `PREFLIGHT FAILED · ${preflight?.message || "本地存储不可写"}`;
    node.classList.add("bad");
  }
}

function addLog(code, message, kind = "") {
  const now = new Date();
  state.logs.unshift({
    time: now.toLocaleTimeString("zh-CN", { hour12: false }) + "." + String(now.getMilliseconds()).padStart(3, "0"),
    code,
    message,
    kind,
  });
  if (state.logs.length > 250) state.logs.length = 250;
  if (document.readyState !== "loading") renderLogs();
}

function handleError(err, title) {
  const data = err?.data || {};
  const code = data.code || "E_INTERNAL";
  const field = data.field ? ` [${data.field}]` : "";
  const message = `${data.message || err.message || String(err)}${field}`;
  addLog(code, message, "error");
  toast(title, message, "error");
}

function toast(title, message, kind = "") {
  const stack = $("toastStack");
  const node = document.createElement("div");
  node.className = `toast ${kind}`;
  node.innerHTML = `<strong>${escapeHTML(title)}</strong>${escapeHTML(message)}`;
  stack.appendChild(node);
  setTimeout(() => node.remove(), 5200);
}

function channelColor(name) {
  return channelColors[name] || "#b8ff45";
}

function hexAlpha(hex, alpha) {
  const value = hex.replace("#", "");
  const r = parseInt(value.slice(0, 2), 16);
  const g = parseInt(value.slice(2, 4), 16);
  const b = parseInt(value.slice(4, 6), 16);
  return `rgba(${r}, ${g}, ${b}, ${alpha})`;
}

function shortHash(value) {
  return value ? String(value).slice(0, 10) : "—";
}

function formatNumber(value, digits = 2) {
  const number = Number(value);
  if (!Number.isFinite(number)) return "—";
  return number.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits });
}

function formatInteger(value) {
  const number = Number(value);
  return Number.isFinite(number) ? number.toLocaleString("en-US") : "0";
}

function errorMessage(err) {
  if (err?.data?.code) {
    return `${err.data.code}: ${err.data.message || err.message}`;
  }
  return err?.message || String(err);
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#039;");
}

