const ui = {
  data: null,
  controller: null,
  refreshTimer: null,
};

const $ = (id) => document.getElementById(id);
const colors = {
  pending: "#8092a4",
  processing: "#f2c35a",
  committed: "#7bd88f",
  failed: "#ff6b6b",
  cyan: "#41d8d0",
  blue: "#56a8ff",
  grid: "#23303d",
  text: "#e7eef5",
  muted: "#8092a4",
};

async function api(path, options = {}) {
  const init = { ...options, headers: { "Content-Type": "application/json", ...(options.headers || {}) } };
  const response = await fetch(path, init);
  const body = await response.json().catch(() => ({}));
  if (!response.ok) {
    const err = new Error(body.message || `HTTP ${response.status}`);
    err.payload = body;
    throw err;
  }
  return body;
}

function showError(error) {
  const box = $("error");
  if (!error) {
    box.classList.add("hidden");
    return;
  }
  box.textContent = JSON.stringify(error.payload || { message: error.message }, null, 2);
  box.classList.remove("hidden");
}

async function refresh() {
  try {
    ui.data = await api("/api/state");
    render();
    showError(ui.data.last_error || null);
  } catch (error) {
    showError(error);
  }
}

function render() {
  const data = ui.data;
  if (!data) return;
  const committed = data.segments.filter((segment) => segment.state === "committed").length;
  $("run-status").textContent = data.running ? "PROCESSING" : committed === data.segments.length ? "COMMITTED" : "IDLE";
  $("run-status").classList.toggle("running", data.running);
  $("run").disabled = data.running;
  $("cancel").disabled = !data.running;
  $("verify").disabled = data.running;
  $("apply").disabled = data.running || committed > 0;
  $("program-meta").textContent =
    `${data.manifest.program_id} · ${data.manifest.sample_rate_hz} Hz · seed ${data.manifest.seed} · ${data.segments.length} segments`;

  renderVersionSelect(data);
  renderParameters(data);
  renderSegments(data);
  renderStats(data);
  drawTimeline(data);
  drawLoudness(data);
  renderCurveOptions(data);
  drawGainCurve(data);
  drawHeatmap(data);
}

function renderVersionSelect(data) {
  const select = $("version");
  if (select.options.length === 0) {
    data.versions.forEach((version) => select.add(new Option(version.toUpperCase(), version)));
  }
  select.value = data.version;
}

function renderParameters(data) {
  const chain = data.chain;
  $("target").value = chain.target_lufs;
  $("frame").value = chain.frame_ms;
  $("hop").value = chain.hop_ms;
  $("gate-abs").value = chain.gate.absolute_lufs;
  $("gate-rel").value = chain.gate.relative_lu;
  $("threshold").value = chain.compressor.threshold_dbfs;
  $("ratio").value = chain.compressor.ratio;
  $("attack").value = chain.compressor.attack_ms;
  $("release").value = chain.compressor.release_ms;
  $("ceiling").value = chain.limiter.ceiling_dbtp;
  $("lookahead").value = chain.limiter.lookahead_ms;
}

function renderSegments(data) {
  const table = $("segment-table");
  const rows = data.segments.map((segment) => {
    const loudness = formatLevel(segment.source_measurement?.integrated_lufs);
    const peak = formatLevel(segment.source_measurement?.true_peak_dbtp);
    const gain = segment.normalization_gain_db ? segment.normalization_gain_db.toFixed(2) : "—";
    return `<tr>
      <td>${segment.index}</td><td>${escapeHTML(segment.id)}</td><td>${segment.channels === 1 ? "mono" : "stereo"}</td>
      <td>${(segment.frames / data.manifest.sample_rate_hz).toFixed(2)} s</td>
      <td class="${segment.state === "failed" ? "bad" : segment.state === "committed" ? "good" : ""}">${segment.state}</td>
      <td>${loudness}</td><td>${peak}</td><td>${gain}</td>
    </tr>`;
  }).join("");
  table.innerHTML = `<table><thead><tr><th>#</th><th>段</th><th>通道</th><th>时长</th><th>状态</th><th>LUFS</th><th>dBTP</th><th>归一 dB</th></tr></thead><tbody>${rows}</tbody></table>`;
}

function renderStats(data) {
  const result = data.result;
  const stats = [
    ["目标响度", `${data.chain.target_lufs.toFixed(1)} LUFS`],
    ["压缩后响度", result ? `${result.compressed_integrated_lufs.toFixed(2)} LUFS` : "—"],
    ["最终节目响度", result ? `${result.final_program_loudness_lufs.toFixed(2)} LUFS` : "—"],
    ["全局归一", result ? `${result.global_gain_db.toFixed(2)} dB` : "—"],
    ["节目真峰值", result ? `${result.program_true_peak_dbtp.toFixed(2)} dBTP` : "—"],
    ["缓冲峰值", result ? `${(result.buffer_peak_bytes / 1048576).toFixed(2)} MiB` : "—"],
    ["输出哈希", result ? result.mix_hash.slice(0, 16) + "…" : "—"],
  ];
  $("stats").innerHTML = stats.map(([label, value]) =>
    `<div class="stat"><span class="label">${label}</span><span class="value">${value}</span></div>`).join("");
}

function setupCanvas(canvas) {
  const rect = canvas.getBoundingClientRect();
  const ratio = window.devicePixelRatio || 1;
  canvas.width = Math.max(1, Math.round(rect.width * ratio));
  canvas.height = Math.max(1, Math.round(rect.height * ratio));
  const ctx = canvas.getContext("2d");
  ctx.setTransform(ratio, 0, 0, ratio, 0, 0);
  return { ctx, width: rect.width, height: rect.height };
}

function drawTimeline(data) {
  const { ctx, width, height } = setupCanvas($("timeline"));
  const margin = 20;
  const totalFrames = data.segments.reduce((sum, segment) => sum + segment.frames, 0) || 1;
  let x = margin;
  const usable = width - margin * 2;
  data.segments.forEach((segment) => {
    const w = usable * segment.frames / totalFrames;
    ctx.fillStyle = colors[segment.state] || colors.pending;
    ctx.globalAlpha = segment.state === "committed" ? 0.82 : 0.42;
    ctx.fillRect(x, margin, Math.max(2, w - 3), height - margin * 2);
    ctx.globalAlpha = 1;
    ctx.fillStyle = colors.text;
    ctx.font = "11px monospace";
    if (w > 60) ctx.fillText(`${segment.index}`, x + 6, margin + 18);
    if (w > 150) ctx.fillText(segment.channels === 1 ? "MONO" : "STEREO", x + 6, margin + 34);
    x += w;
  });
  ctx.strokeStyle = colors.grid;
  ctx.beginPath();
  ctx.moveTo(margin, height - margin);
  ctx.lineTo(width - margin, height - margin);
  ctx.stroke();
}

function drawLoudness(data) {
  const { ctx, width, height } = setupCanvas($("loudness-chart"));
  const margin = { left: 48, right: 20, top: 20, bottom: 32 };
  const segments = data.segments;
  const actual = segments.filter((segment) => segment.source_measurement);
  if (actual.length === 0) {
    ctx.fillStyle = colors.muted;
    ctx.font = "13px monospace";
    ctx.fillText("运行处理链后显示逐段统计", margin.left, height / 2);
    return;
  }
  const values = [];
  actual.forEach((segment) => {
    if (Number.isFinite(segment.source_measurement.integrated_lufs)) values.push(segment.source_measurement.integrated_lufs);
    if (Number.isFinite(segment.source_measurement.true_peak_dbtp)) values.push(segment.source_measurement.true_peak_dbtp);
    if (segment.output_measurement && Number.isFinite(segment.output_measurement.integrated_lufs)) values.push(segment.output_measurement.integrated_lufs);
  });
  if (values.length === 0) {
    ctx.fillStyle = colors.muted;
    ctx.font = "13px monospace";
    ctx.fillText("节目段低于门限，无有限响度值", margin.left, height / 2);
    return;
  }
  const min = Math.floor(Math.min(...values) - 2);
  const max = Math.ceil(Math.max(...values) + 2);
  const plotW = width - margin.left - margin.right;
  const plotH = height - margin.top - margin.bottom;
  const xAt = (i) => margin.left + (segments.length === 1 ? plotW / 2 : plotW * i / (segments.length - 1));
  const yAt = (value) => margin.top + plotH * (max - value) / (max - min);
  ctx.strokeStyle = colors.grid;
  ctx.fillStyle = colors.muted;
  ctx.font = "10px monospace";
  for (let g = 0; g <= 4; g++) {
    const value = min + (max - min) * g / 4;
    const y = yAt(value);
    ctx.beginPath(); ctx.moveTo(margin.left, y); ctx.lineTo(width - margin.right, y); ctx.stroke();
    ctx.fillText(`${value.toFixed(0)}`, 8, y + 3);
  }
  line(ctx, segments, xAt, yAt, (s) => s.source_measurement?.integrated_lufs, colors.cyan);
  line(ctx, segments, xAt, yAt, (s) => s.source_measurement?.true_peak_dbtp, colors.blue);
  line(ctx, segments, xAt, yAt, (s) => s.output_measurement?.integrated_lufs, colors.committed);
  const legend = [["source LUFS", colors.cyan], ["true peak dBTP", colors.blue], ["output LUFS", colors.committed]];
  legend.forEach(([label, color], i) => {
    const x = width - 260 + i * 88;
    ctx.fillStyle = color; ctx.fillRect(x, 8, 12, 3);
    ctx.fillStyle = colors.muted; ctx.fillText(label, x + 16, 12);
  });
}

function line(ctx, segments, xAt, yAt, getter, color) {
  ctx.strokeStyle = color;
  ctx.lineWidth = 1.7;
  ctx.beginPath();
  let started = false;
  segments.forEach((segment, i) => {
    const value = getter(segment);
    if (!Number.isFinite(value)) { started = false; return; }
    const x = xAt(i), y = yAt(value);
    if (!started) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y);
  });
  ctx.stroke();
  segments.forEach((segment, i) => {
    const value = getter(segment);
    if (!Number.isFinite(value)) return;
    ctx.fillStyle = color;
    ctx.beginPath(); ctx.arc(xAt(i), yAt(value), 3, 0, Math.PI * 2); ctx.fill();
  });
}

function renderCurveOptions(data) {
  const select = $("curve-segment");
  const current = select.value;
  if (select.options.length !== data.segments.length) {
    select.innerHTML = "";
    data.segments.forEach((segment) => select.add(new Option(segment.id, segment.id)));
  }
  if (current) select.value = current;
}

function drawGainCurve(data) {
  const { ctx, width, height } = setupCanvas($("gain-chart"));
  const selectedID = $("curve-segment").value;
  const segment = data.segments.find((item) => item.id === selectedID) || data.segments[0];
  const curve = segment?.compression_gain_curve_db || [];
  if (curve.length === 0) {
    ctx.fillStyle = colors.muted;
    ctx.font = "13px monospace";
    ctx.fillText("所选节目段尚无压缩增益数据", 20, height / 2);
    return;
  }
  const margin = { left: 48, right: 20, top: 20, bottom: 26 };
  const plotW = width - margin.left - margin.right;
  const plotH = height - margin.top - margin.bottom;
  const min = Math.min(-24, Math.floor(Math.min(...curve) - 1));
  const max = 1;
  ctx.strokeStyle = colors.grid;
  ctx.fillStyle = colors.muted;
  ctx.font = "10px monospace";
  for (let g = 0; g <= 4; g++) {
    const value = max + (min - max) * g / 4;
    const y = margin.top + plotH * g / 4;
    ctx.beginPath(); ctx.moveTo(margin.left, y); ctx.lineTo(width - margin.right, y); ctx.stroke();
    ctx.fillText(`${value.toFixed(0)} dB`, 4, y + 3);
  }
  ctx.strokeStyle = colors.amber;
  ctx.lineWidth = 1.8;
  ctx.beginPath();
  curve.forEach((value, i) => {
    const x = margin.left + plotW * i / Math.max(1, curve.length - 1);
    const y = margin.top + plotH * (max - value) / (max - min);
    if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
  });
  ctx.stroke();
}

function drawHeatmap(data) {
  const { ctx, width, height } = setupCanvas($("heatmap"));
  const margin = 18;
  const spans = data.diff?.segments || data.segments.map((segment) => ({
    id: segment.id, index: segment.index, max_absolute_diff: 0,
    differing_samples: 0, frame_count: segment.frames,
  }));
  const total = spans.reduce((sum, span) => sum + span.frame_count, 0) || 1;
  let x = margin;
  const usable = width - margin * 2;
  spans.forEach((span) => {
    const w = usable * span.frame_count / total;
    const intensity = Math.min(1, (span.max_absolute_diff || 0) * 40);
    const base = data.diff ? [255, 107, 107] : [65, 216, 208];
    const alpha = data.diff ? 0.12 + intensity * 0.88 : 0.20;
    ctx.fillStyle = `rgba(${base[0]},${base[1]},${base[2]},${alpha})`;
    ctx.fillRect(x, margin, Math.max(2, w - 3), height - margin * 2);
    ctx.fillStyle = colors.text;
    ctx.font = "11px monospace";
    if (w > 55) ctx.fillText(span.index, x + 6, margin + 18);
    x += w;
  });
  const label = data.diff
    ? data.diff.identical ? "三路径逐样本一致" : "检测到路径差异"
    : "尚未运行三路径比对";
  $("diff-label").textContent = label;
  if (data.diff?.first_divergence) {
    const d = data.diff.first_divergence;
    $("diff-details").textContent =
      `首个差异 sample=${d.sample_index} frame=${d.frame_index} segment=${d.segment_id} channel=${d.channel} left=${d.left} right=${d.right}`;
  } else if (data.diff) {
    $("diff-details").textContent =
      `逐样本一致=${data.diff.identical}，增益决策一致=${data.diff.gain_decisions_equal}`;
  } else {
    $("diff-details").textContent = "";
  }
}

function escapeHTML(value) {
  return String(value).replace(/[&<>"']/g, (char) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[char]);
}

function formatLevel(value) {
  return Number.isFinite(value) ? value.toFixed(2) : "−∞";
}

$("run").addEventListener("click", async () => {
  showError(null);
  ui.controller = new AbortController();
  try {
    await api("/api/run", {
      method: "POST", signal: ui.controller.signal,
      body: JSON.stringify({
        mode: $("mode").value,
        max_parallel: Number($("parallel").value),
        per_worker_buffer_bytes: Math.round(Number($("buffer").value) * 1048576),
      }),
    });
  } catch (error) {
    if (error.name !== "AbortError") showError(error);
  } finally {
    ui.controller = null;
    await refresh();
  }
});

$("cancel").addEventListener("click", () => ui.controller?.abort());
$("verify").addEventListener("click", async () => {
  showError(null);
  ui.controller = new AbortController();
  try {
    await api("/api/verify", { method: "POST", body: "{}", signal: ui.controller.signal });
  } catch (error) {
    if (error.name !== "AbortError") showError(error);
  } finally {
    ui.controller = null;
    await refresh();
  }
});

$("apply").addEventListener("click", async () => {
  showError(null);
  try {
    await api("/api/config", {
      method: "POST",
      body: JSON.stringify({
        target_lufs: Number($("target").value),
        frame_ms: Number($("frame").value),
        hop_ms: Number($("hop").value),
        gate: { absolute_lufs: Number($("gate-abs").value), relative_lu: Number($("gate-rel").value) },
        compressor: {
          threshold_dbfs: Number($("threshold").value),
          ratio: Number($("ratio").value),
          attack_ms: Number($("attack").value),
          release_ms: Number($("release").value),
        },
        limiter: {
          ceiling_dbtp: Number($("ceiling").value),
          lookahead_ms: Number($("lookahead").value),
        },
      }),
    });
    await refresh();
  } catch (error) {
    showError(error);
  }
});

$("reset").addEventListener("click", async () => {
  try {
    await api("/api/reset", { method: "POST", body: "{}" });
    await refresh();
  } catch (error) {
    showError(error);
  }
});

$("version").addEventListener("change", async (event) => {
  try {
    await api("/api/version", { method: "POST", body: JSON.stringify({ version: event.target.value }) });
    await refresh();
  } catch (error) {
    showError(error);
  }
});

$("curve-segment").addEventListener("change", () => ui.data && drawGainCurve(ui.data));
window.addEventListener("resize", () => ui.data && render());

refresh();
ui.refreshTimer = setInterval(refresh, 2000);
