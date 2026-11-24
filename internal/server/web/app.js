let session = null;
let sessionId = null;

const $ = (id) => document.getElementById(id);
const log = (msg, cls) => {
  const el = $("logView");
  el.textContent = (cls ? "[" + cls + "] " : "") + msg;
  el.className = cls === "ERROR" ? "errc" : cls === "OK" ? "ok" : "";
};

async function api(path, opts = {}) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const detail = data.code ? `${data.code}: ${data.message} ${JSON.stringify(data.details || {})}` : JSON.stringify(data);
    log(detail, "ERROR");
    throw new Error(detail);
  }
  return data;
}

async function refreshMaterials() {
  const data = await api("/api/materials");
  const sel = $("materialSelect");
  sel.innerHTML = "";
  data.descriptors.forEach((d) => {
    const opt = document.createElement("option");
    opt.value = d;
    opt.textContent = d.split("/").pop().replace(".json", "");
    sel.appendChild(opt);
  });
}

function fillParams(p) {
  $("frameLength").value = p.frame_length;
  $("frameShift").value = p.frame_shift;
  $("bands").value = p.bands;
  $("gapTol").value = p.gap_tolerance_frames;
  $("minHit").value = p.min_hit_frames;
  $("threshold").value = p.match_threshold;
  $("quietRatio").value = p.quiet_ratio_threshold;
}

function readParams() {
  return {
    frame_length: +$("frameLength").value,
    frame_shift: +$("frameShift").value,
    bands: +$("bands").value,
    gap_tolerance_frames: +$("gapTol").value,
    min_hit_frames: +$("minHit").value,
    match_threshold: +$("threshold").value,
    quiet_ratio_threshold: +$("quietRatio").value,
  };
}

async function openSession() {
  const descriptor = $("materialSelect").value;
  session = await api("/api/sessions", { method: "POST", body: JSON.stringify({ descriptor }) });
  sessionId = session.material_id;
  fillParams(session.params);
  render(session);
  log(`已打开素材 ${sessionId}，总采样点 ${session.total_samples}`, "OK");
}

function render(s) {
  session = s;
  renderWave(s);
  renderMatrix(s);
  renderEvents(s);
  renderQuality(s);
  renderFingerprints(s);
  if (s.progress) $("progress").textContent = `进度：已提交 ${s.progress.committed} / ${s.progress.total} 窗口`;
}

const colors = ["#4f9cff", "#37c97e", "#f0b429", "#ff8f6b", "#b48cff", "#5ad6e0"];
function colorFor(label, cats) {
  const idx = (cats || []).findIndex((c) => c.label === label);
  return colors[(idx < 0 ? 0 : idx) % colors.length];
}

function renderWave(s) {
  const c = $("waveCanvas");
  const ctx = c.getContext("2d");
  c.width = c.clientWidth;
  ctx.clearRect(0, 0, c.width, c.height);
  const wf = s.waveform || [];
  const mid = c.height / 2;
  ctx.strokeStyle = "#7fa8dd";
  ctx.beginPath();
  wf.forEach((v, i) => {
  const x = (i / Math.max(1, wf.length - 1)) * c.width;
  const y = mid - v * mid * 0.9;
    i === 0 ? ctx.moveTo(x, y) : ctx.lineTo(x, y);
  });
  ctx.stroke();
  const total = s.total_samples || 1;
  const xOf = (sample) => (sample / total) * c.width;
  // 声明事件：上半部，实色填充。
  (s.declared || []).forEach((e) => {
    ctx.fillStyle = colorFor(e.label, s.categories);
    ctx.globalAlpha = 0.35;
    ctx.fillRect(xOf(e.start_sample), 4, Math.max(2, xOf(e.end_sample) - xOf(e.start_sample)), 22);
    ctx.globalAlpha = 1;
  });
  // 检测事件：下半部，描边。
  const det = s.annotation ? s.annotation.events : [];
  det.forEach((e) => {
    ctx.strokeStyle = colorFor(e.label, s.categories);
    ctx.lineWidth = 2;
    ctx.strokeRect(xOf(e.start_sample), c.height - 30, Math.max(2, xOf(e.end_sample) - xOf(e.start_sample)), 24);
  });
  $("legend").innerHTML = (s.categories || []).map((c2) =>
    `<span style="color:${colorFor(c2.label, s.categories)}">■ ${c2.label}</span>`).join(" &nbsp; ");
}

function renderMatrix(s) {
  const c = $("matrixCanvas");
  const ctx = c.getContext("2d");
  c.width = c.clientWidth;
  ctx.clearRect(0, 0, c.width, c.height);
  const wins = s.windows || [];
  if (!wins.length) return;
  const w = c.width / wins.length;
  const stateColor = { pending: "#33415c", extracting: "#7a5c00", committed: "#0f5132", failed: "#5c1f24" };
  wins.forEach((win, i) => {
    ctx.fillStyle = stateColor[win.state] || "#33415c";
    ctx.fillRect(i * w + 1, 10, Math.max(2, w - 2), c.height - 20);
  });
  const rows = wins.map((win) => `<tr><td>${win.index}</td><td>[${win.start_frame},${win.end_frame})</td>
    <td><span class="pill ${win.state}">${win.state}</span></td><td class="muted">${win.fail_reason || ""}</td></tr>`).join("");
  $("windowTable").innerHTML = `<table><thead><tr><th>窗</th><th>帧区间</th><th>状态</th><th>失败原因</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function renderEvents(s) {
  const tbody = $("eventTable").querySelector("tbody");
  const det = s.annotation ? s.annotation.events : [];
  const decl = s.declared || [];
  const n = Math.max(decl.length, det.length);
  let html = "";
  for (let i = 0; i < n; i++) {
    const d = decl[i], e = det[i];
    html += `<tr>
      <td>${i}</td>
      <td>${d ? d.label : '<span class="muted">—</span>'}</td>
      <td class="muted">${d ? `[${d.start_sample},${d.end_sample})` : "—"}</td>
      <td style="color:${e ? colorFor(e.label, s.categories) : ""}">${e ? e.label : '<span class="muted">无命中</span>'}</td>
      <td class="muted">${e ? `[${e.start_sample},${e.end_sample})` : "—"}</td>
      <td>${e ? e.confidence.toFixed(3) : "—"}</td></tr>`;
  }
  tbody.innerHTML = html;
}

function renderQuality(s) {
  const q = s.annotation ? s.annotation.quality : null;
  if (!q) { $("quality").innerHTML = '<span class="muted">暂无</span>'; return; }
  const stat = (k, v) => `<div class="stat"><span class="muted">${k}</span><span>${v}</span></div>`;
  $("quality").innerHTML =
    stat("声明/检测/匹配", `${q.total_declared} / ${q.total_detected} / ${q.matched}`) +
    stat("类别正确率", q.label_accuracy) +
    stat("平均起点/终点偏差", `${q.mean_start_bias} / ${q.mean_end_bias}`) +
    stat("最大绝对偏差", `${q.max_abs_start_bias} / ${q.max_abs_end_bias}（容差 ${q.tolerance}）`) +
    stat("置信度均值/区间", `${q.confidence_mean} [${q.confidence_min}, ${q.confidence_max}]`) +
    stat("置信度分布", JSON.stringify(q.confidence_hist)) +
    (s.annotation.quality_error ? `<div class="errc">${s.annotation.quality_error.code}: ${s.annotation.quality_error.message}</div>` : '<div class="ok">质量校验通过</div>');
}

function renderFingerprints(s) {
  const fps = s.annotation ? s.annotation.fingerprints_head : null;
  if (!fps || !fps.length) { $("fpView").textContent = "运行标注后展示"; return; }
  $("fpView").textContent = fps.map((f) =>
    `frame=${String(f.frame).padStart(4)} off=${String(f.offset_sample).padStart(6)} E=${f.total_energy.toExponential(2)} q=[${f.quantized.join(",")}]`).join("\n");
}

async function runAction(btnId, url, key) {
  try {
    log("执行中…");
    const data = await api(url, { method: "POST" });
    const ann = data.annotation || data;
    const merged = { ...session, annotation: ann, windows: ann && ann.path ? undefined : session.windows };
    const fresh = await api(`/api/sessions/${sessionId}`);
    if (ann) fresh.annotation = ann;
    render(fresh);
    log(`${key}完成，事件数 ${ann ? (ann.events || []).length : 0}`, "OK");
  } catch (_) { /* 错误已在 api 内记录 */ }
}

$("openBtn").onclick = openSession;
$("applyParams").onclick = async () => {
  try {
    const s = await api(`/api/sessions/${sessionId}/params`, { method: "PUT", body: JSON.stringify(readParams()) });
    render(s); log("参数已应用到未提交窗口", "OK");
  } catch (_) {}
};
$("runBtn").onclick = () => runAction("runBtn", `/api/sessions/${sessionId}/run`, "实时标注");
$("resumeBtn").onclick = async () => {
  try {
    const data = await api(`/api/sessions/${sessionId}/resume`, { method: "POST" });
    const fresh = await api(`/api/sessions/${sessionId}`);
    fresh.annotation = data.annotation;
    render(fresh);
    log(`断点恢复完成（作废快照=${data.invalidated}，原因：${data.reason || "无"}）`, "OK");
  } catch (_) {}
};
$("fullBtn").onclick = () => runAction("fullBtn", `/api/sessions/${sessionId}/full`, "全量重算");
$("pauseBtn").onclick = async () => { await api(`/api/sessions/${sessionId}/pause`, { method: "POST" }); log("已暂停", "OK"); };
$("cancelBtn").onclick = async () => { await api(`/api/sessions/${sessionId}/cancel`, { method: "POST" }); log("已取消，仅已持久化窗口可恢复", "OK"); };
$("compareBtn").onclick = async () => {
  try {
    const data = await api(`/api/sessions/${sessionId}/compare`, { method: "POST" });
    $("compareView").className = data.consistent ? "ok" : "errc";
    $("compareView").innerHTML = data.consistent
      ? `三路径逐帧/逐事件一致（恢复作废快照=${data.resume_invalidated}）`
      : `<pre>${JSON.stringify(data.divergences, null, 2)}</pre>`;
    log(data.consistent ? "三路径一致" : "发现 E_DIV 分歧", data.consistent ? "OK" : "ERROR");
  } catch (_) {}
};
$("exportBtn").onclick = async () => {
  try {
    const data = await api(`/api/sessions/${sessionId}/export`, { method: "POST" });
    log(`已导出到 ${data.export_path}`, "OK");
  } catch (_) {}
};

fetch("/api/health").then((r) => r.json()).then((d) => {
  $("health").textContent = `本地离线服务 · ${d.status}`;
});
refreshMaterials();
