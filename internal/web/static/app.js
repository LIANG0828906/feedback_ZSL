const $ = (id) => document.getElementById(id);

const state = {
  manifest: null,
  imageId: null,
  paramVersion: 1,
  previewTimer: null,
  exactTimer: null,
  previewRequest: 0,
};

function defaultParams() {
  return {
    param_version: state.paramVersion,
    gradient_operator: $("gradient_operator").value,
    smooth_kernel: Number($("smooth_kernel").value),
    flood_threshold: Number($("flood_threshold").value),
    min_region_area: Number($("min_region_area").value),
    merge_cost_cap: Number($("merge_cost_cap").value),
    area_diff_cap: Number($("area_diff_cap").value),
    morph_radius: Number($("morph_radius").value),
  };
}

function updateSliderLabels() {
  $("floodValue").textContent = $("flood_threshold").value;
  $("mergeValue").textContent = $("merge_cost_cap").value;
  $("minAreaValue").textContent = $("min_region_area").value;
  $("areaDiffValue").textContent = $("area_diff_cap").value;
}

function simpleHash(obj) {
  let h = 0;
  const s = JSON.stringify(obj);
  for (let i = 0; i < s.length; i++) h = ((h << 5) - h + s.charCodeAt(i)) | 0;
  return Math.abs(h).toString(16).padStart(8, "0");
}

function updateVersion() {
  const p = defaultParams();
  $("versionLabel").textContent = "v" + state.paramVersion;
  $("hashLabel").textContent = simpleHash(p);
}

function scheduleWork() {
  updateSliderLabels();
  state.paramVersion += 1;
  updateVersion();
  clearTimeout(state.previewTimer);
  clearTimeout(state.exactTimer);
  state.previewTimer = setTimeout(runPreview, 80);
  state.exactTimer = setTimeout(async () => {
    await runPreview();
    await runRecompute();
  }, 520);
}

async function api(path, options) {
  const res = await fetch(path, options);
  const body = await res.json();
  return { status: res.status, body };
}

function postJSON(path, payload) {
  return api(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
}

function imageDataURL(b64) {
  return "data:image/png;base64," + b64;
}

async function init() {
  try {
    const { body } = await api("/api/manifest");
    state.manifest = body.data;
    state.imageId = state.manifest.images[0].id;
    $("health").textContent = "127.0.0.1 本地服务正常";
    $("health").className = "chip ok";
    renderTabs();
    renderBudget();
    bindControls();
    await selectImage(state.imageId, false);
    await runRecompute();
    setInterval(loadJudgments, 4000);
  } catch (err) {
    $("health").textContent = "服务未启动";
    $("health").className = "chip bad";
    setBanner("无法连接本地 Go 服务：" + err.message, "bad");
  }
}

function renderTabs() {
  const host = $("imageTabs");
  host.innerHTML = "";
  state.manifest.images.forEach((rec) => {
    const btn = document.createElement("button");
    btn.textContent = rec.id.replace("icon_", "");
    btn.dataset.id = rec.id;
    if (rec.id === state.imageId) btn.classList.add("active");
    btn.onclick = () => selectImage(rec.id, true);
    host.appendChild(btn);
  });
}

function renderBudget() {
  const b = state.manifest.budget;
  const t = state.manifest.tolerances;
  $("budget").textContent =
    `固定种子 ${b.seed}\n` +
    `分块 ${b.tile_size}×${b.tile_size} px\n` +
    `线程预算 ${b.worker_threads}；每线程像素缓冲 ${b.pixel_buffer_per_thread}\n` +
    `预览延迟上限 ${b.preview_delay_ms} ms\n` +
    `容差 IoU≥${t.mask_iou_min}，重心≤${t.centroid_px}px，面积≤${(t.area_rel * 100).toFixed(1)}%，孔洞差≤${t.hole_count}`;
}

function bindControls() {
  ["flood_threshold", "merge_cost_cap", "min_region_area", "area_diff_cap"].forEach((id) => {
    $(id).addEventListener("input", scheduleWork);
  });
  ["gradient_operator", "smooth_kernel", "morph_radius"].forEach((id) => {
    $(id).addEventListener("change", scheduleWork);
  });
  $("recomputeBtn").onclick = async () => {
    clearTimeout(state.exactTimer);
    await runPreview();
    await runRecompute();
  };
  $("verifyBtn").onclick = runVerify;
  $("curveBtn").onclick = runCurve;
  updateSliderLabels();
  updateVersion();
}

async function selectImage(id, recompute) {
  state.imageId = id;
  document.querySelectorAll("#imageTabs button").forEach((b) => b.classList.toggle("active", b.dataset.id === id));
  const rec = state.manifest.images.find((x) => x.id === id);
  $("originalImg").src = "/api/sample-image?id=" + encodeURIComponent(id) + "&t=" + Date.now();
  $("truthImg").src = imageDataURL(rec.reference_mask_png_base64);
  $("previewImg").removeAttribute("src");
  $("exactImg").removeAttribute("src");
  if (recompute) await runRecompute();
  await loadJudgments();
}

async function runPreview() {
  const seq = ++state.previewRequest;
  const params = defaultParams();
  setBanner("粗略预览计算中：降采样 + 减少优先队列采样…", "busy");
  const { body } = await postJSON("/api/preview", {
    image_id: state.imageId,
    params,
    factor: Number($("factor").value),
  });
  if (seq !== state.previewRequest) return;
  if (!body.ok) {
    showError(body.error);
    setBanner("预览参数错误：" + (body.error && body.error.code), "bad");
    return;
  }
  hideError();
  const p = body.data.preview;
  $("previewImg").src = imageDataURL(body.data.overlay_png_base64);
  $("previewMeta").textContent = `${p.downsample_factor}× / PQ ${p.flood_samples} / ${p.elapsed_ms}ms`;
  $("basisLine").textContent = p.basis;
  $("basisLine").title = p.basis;
  $("regionCount").textContent = `${p.stats.length}（占位）`;
  setBanner(`粗略预览已更新：${p.downsample_factor}× 降采样，淹没采样 ${p.flood_samples}，耗时 ${p.elapsed_ms}ms`, "busy");
}

async function runRecompute() {
  const params = defaultParams();
  setBanner("全精度分块重算并写入原子检查点…", "busy");
  const { status, body } = await postJSON("/api/recompute", {
    image_id: state.imageId,
    params,
    preview_param_version: params.param_version,
  });
  if (body.data) renderRecomputeData(body.data);
  if (!body.ok) {
    showError(body.error);
    if (body.error && body.error.code === "E_DIVERGE") {
      const m = body.error.metrics || {};
      setBanner(`E_DIVERGE：区域 ${m.region_count_actual}/${m.region_count_expected}，重心 ${fmt(m.max_centroid_error_px)}px，面积 ${fmtPct(m.max_area_relative_error)}；首区 ${body.error.first_region_id || "—"} @${body.error.diff_x},${body.error.diff_y}`, "bad");
    } else {
      setBanner((body.error && body.error.code) + " " + (body.error && body.error.message), "bad");
    }
  } else {
    hideError();
    const j = body.data.judgment;
    if (j && j.id) {
      setBanner(`一致性判定通过：区域差 ${j.metrics.region_count_delta}，重心 ${fmt(j.metrics.max_centroid_error_px)}px，面积 ${fmtPct(j.metrics.max_area_relative_error)}；判定 #${j.id} 已原子留存`, "ok");
    } else {
      setBanner("全精度重算完成；尚无同版本粗略预览可判定。", "ok");
    }
  }
  await loadJudgments();
  await loadSequence();
}

function renderRecomputeData(data) {
  const r = data.result;
  $("exactImg").src = imageDataURL(data.overlay_png_base64);
  $("exactMeta").textContent = `${r.width}×${r.height} / hash ${r.mask_hash.slice(0, 10)}`;
  $("regionCount").textContent = `${r.stats.length} / ${r.truth_stats.length}`;
  $("iouValue").textContent = fmt(r.metrics.iou, 4);
  $("mismatchValue").textContent = r.metrics.mismatch_pixels;
  $("centroidValue").textContent = fmt(r.metrics.max_centroid_error_px) + "px";
  $("areaValue").textContent = fmtPct(r.metrics.max_area_relative_error);
  $("commitValue").textContent = r.sequence_count;

  const rows = $("regionRows");
  rows.innerHTML = "";
  (r.report.deviations || []).forEach((d) => {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${d.truth_id}</td><td>${d.predicted_id}</td>
      <td class="${d.centroid_distance > state.manifest.tolerances.centroid_px ? "bad-text" : "good"}">${fmt(d.centroid_distance)}</td>
      <td class="${d.area_relative_error > state.manifest.tolerances.area_rel ? "bad-text" : "good"}">${fmtPct(d.area_relative_error)}</td>
      <td>${d.predicted_holes}/${d.truth_holes}</td><td>${fmt(d.iou, 3)}</td>`;
    rows.appendChild(tr);
  });
  if (r.report.error) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="bad-text">${r.report.error.code}: ${JSON.stringify(r.report.error.extra || [])} / ${JSON.stringify(r.report.error.missing || [])}</td>`;
    rows.appendChild(tr);
  }

  $("queueLog").textContent = (data.queue_records || []).length
    ? data.queue_records.map((q) => `block=${q.block_index} ${q.layer} pixels=${q.pixels} reason=${q.reason}`).join("\n")
    : "资源充足，未发生缓冲/线程排队。";

  if (data.sequence) renderSequence(data.sequence);
}

function renderSequence(seq) {
  const host = $("sequenceRows");
  host.innerHTML = "";
  seq.slice(-40).forEach((s) => {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td>${s.seq}</td><td>${s.block_index} (${s.block_x},${s.block_y})</td><td>${s.layer}</td><td><code>${s.content_hash.slice(0, 10)}</code></td>`;
    host.appendChild(tr);
  });
}

async function loadSequence() {
  const { body } = await api("/api/sequence?id=" + encodeURIComponent(state.imageId));
  if (body.ok) renderSequence(body.data);
}

async function loadJudgments() {
  if (!state.imageId) return;
  const { body } = await api("/api/judgments?id=" + encodeURIComponent(state.imageId));
  if (!body.ok) return;
  const host = $("judgmentRows");
  host.innerHTML = "";
  body.data.slice().reverse().slice(0, 30).forEach((j) => {
    const tr = document.createElement("tr");
    const cls = j.ok ? "good" : "bad-text";
    const coord = j.first_region_id ? `${j.first_region_id} @${j.diff_x},${j.diff_y}` : "—";
    tr.innerHTML = `<td>${j.id}</td><td>v${j.param_version}</td>
      <td class="${cls}">${j.ok ? "PASS" : j.code}</td>
      <td>${j.metrics.region_count_actual}/${j.metrics.region_count_expected}</td>
      <td>${fmt(j.metrics.max_centroid_error_px)}</td>
      <td>${fmtPct(j.metrics.max_area_relative_error)}</td>
      <td>${coord}</td>`;
    host.appendChild(tr);
  });
}

async function runVerify() {
  $("verifyLog").textContent = "正在从 SQLite 层产物重建掩码…";
  const { status, body } = await api("/api/verify?id=" + encodeURIComponent(state.imageId));
  const r = body.data;
  if (r && r.ok) {
    $("verifyLog").textContent = `PASS ${r.image_id}\nmask=${r.mask_hash}\ncommits=${r.sequence_count}\nIoU=${fmt(r.metrics.iou, 5)}\ncentroid=${fmt(r.metrics.max_centroid_error_px)}\narea=${fmtPct(r.metrics.max_area_relative_error)}`;
  } else if (r && r.violation) {
    const v = r.violation;
    $("verifyLog").textContent = `FAIL ${r.image_id}\nblock=(${v.block_x},${v.block_y}) pixel=(${v.pixel_x},${v.pixel_y})\nlayer=${v.layer} field=${v.field}\n${v.message}\nexpected=${JSON.stringify(v.expected)}\nactual=${JSON.stringify(v.actual)}`;
  } else {
    $("verifyLog").textContent = "验证失败 HTTP " + status + "\n" + JSON.stringify(body.error, null, 2);
  }
}

async function runCurve() {
  $("verifyLog").textContent = "使用最后一次预览/精确对计算容差收紧曲线…";
  const { body } = await postJSON("/api/tolerance-curve", {
    image_id: state.imageId,
    preview_param_version: state.paramVersion,
  });
  if (!body.ok) {
    $("verifyLog").textContent = JSON.stringify(body.error, null, 2);
    return;
  }
  const lines = body.data.map((p, i) =>
    `#${i + 1} tolCent=${p.tolerances.preview_centroid_px.toFixed(2)} tolArea=${(p.tolerances.preview_area_rel * 100).toFixed(2)}% pass=${p.judgment.ok} actualCent=${p.judgment.metrics.max_centroid_error_px.toFixed(3)} actualArea=${(p.judgment.metrics.max_area_relative_error * 100).toFixed(2)}% reported=${p.reported_stat.toFixed(3)}`
  );
  $("verifyLog").textContent = lines.join("\n");
  await loadJudgments();
}

function setBanner(text, cls) {
  const el = $("judgmentBanner");
  el.textContent = text;
  el.className = "banner " + (cls || "");
}

function showError(err) {
  if (!err) return;
  const el = $("paramError");
  el.classList.remove("hidden");
  el.textContent = `${err.code}: ${err.message}\nfield=${err.field || ""} range=[${err.min ?? ""}, ${err.max ?? ""}] expected=${JSON.stringify(err.expected)} actual=${JSON.stringify(err.actual)}`;
}
function hideError() { $("paramError").classList.add("hidden"); }

function fmt(v, n = 3) {
  if (v === undefined || v === null || Number.isNaN(v)) return "—";
  return Number(v).toFixed(n);
}
function fmtPct(v) {
  if (v === undefined || v === null || Number.isNaN(v)) return "—";
  return (Number(v) * 100).toFixed(2) + "%";
}

init();
