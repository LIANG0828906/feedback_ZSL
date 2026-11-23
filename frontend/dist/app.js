const api = () => window.go && window.go.main && window.go.main.App;

const dom = (id) => document.getElementById(id);
const colors = {
  color_temp: "#ffd166",
  move: "#57d9ff",
  strobe: "#ff6b7a",
  fade: "#58e3a1",
  set: "#7bdcff",
  group: "#b18cff",
};

let state = null;
let currentFrame = 0;
let currentOutput = null;
let selectedCue = null;
let selectedGroup = null;
let busy = false;
let playbackTimer = null;

function fmt(value, digits = 2) {
  if (value === null || value === undefined || Number.isNaN(Number(value))) return "-";
  return Number(value).toFixed(digits);
}

function escapeHTML(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;");
}

async function call(name, ...args) {
  if (!api()) throw new Error("Wails 绑定未就绪。请通过 `wails dev` 或 `wails build` 启动。");
  return api()[name](...args);
}

async function boot() {
  if (!api()) {
    showError("Wails 绑定未就绪。浏览器静态预览没有 Go 后端；请使用 `wails dev` 或 `wails build`。");
    return;
  }
  try {
    state = await call("Initialize");
    hydrateControls();
    render();
    await refreshFrame(0);
  } catch (err) {
    showError(err);
  }
}

function hydrateControls() {
  if (!state) return;
  dom("workers").value = state.params.max_workers || 4;
  dom("buffer").value = state.params.per_worker_buffer_bytes || 16777216;
  dom("memoryBudget").value = state.params.memory_budget_bytes || 0;
  dom("windowMs").value = state.params.window_ms || 0;
  dom("previewFps").value = state.params.preview_fps || 24;
  dom("forceMode").value = state.params.force_mode || "";
}

function render() {
  if (!state || !state.project) return;
  const p = state.project;
  const d = state.decision || {};
  dom("cueValue").textContent = p.cues.length;
  dom("lightValue").textContent = p.lights.length;
  dom("frameValue").textContent = d.frame_count || 0;
  dom("opsValue").textContent = Number(d.estimated_ops || 0).toLocaleString();
  dom("capacityValue").textContent = Number(d.preview_capacity || 0).toLocaleString();
  dom("modeValue").textContent = d.mode || "-";
  dom("modeReason").textContent = (d.reasons || []).join(" · ");
  dom("progressText").textContent = `${state.progress.committed} / ${state.progress.total} windows`;
  const pct = state.progress.total ? (100 * state.progress.committed / state.progress.total) : 0;
  dom("progressBar").style.width = `${pct}%`;
  dom("runningBadge").classList.toggle("hidden", !state.running);
  dom("truthBadge").classList.toggle("hidden", !state.truth_dirty);
  dom("lastNotice").classList.toggle("hidden", !state.last_notice);
  dom("lastNotice").textContent = state.last_notice || "";
  const frameCount = d.frame_count || 1;
  dom("frameSlider").max = Math.max(0, frameCount - 1);
  dom("frameInput").max = Math.max(0, frameCount - 1);
  currentFrame = Math.max(0, Math.min(currentFrame, frameCount - 1));
  dom("frameSlider").value = currentFrame;
  dom("frameInput").value = currentFrame;
  renderTimeline();
  renderWindows();
  renderDecisions();
  renderBudget();
  renderMerge();
  renderVerify();
  renderGroups();
  renderStage(currentOutput);
}

async function refreshFrame(frame) {
  if (!state || !state.project) return;
  const frameCount = state.decision?.frame_count || 1;
  currentFrame = Math.max(0, Math.min(Number(frame) || 0, frameCount - 1));
  dom("frameSlider").value = currentFrame;
  dom("frameInput").value = currentFrame;
  try {
    currentOutput = await call("FrameAt", currentFrame);
    renderFrame();
  } catch (err) {
    showError(err);
  }
}

function renderFrame() {
  if (!currentOutput) return;
  const label = `frame ${currentOutput.frame} · ${(currentOutput.time_us / 1000).toFixed(2)} ms`;
  dom("frameLabel").textContent = label;
  dom("frameTimeLabel").textContent = label;
  renderStage(currentOutput);
  const body = dom("lightRows");
  body.innerHTML = currentOutput.lights.map((light) => `
    <tr>
      <td>${escapeHTML(light.light_id)}</td>
      <td><span class="dot" style="background:rgb(${light.rgb.r},${light.rgb.g},${light.rgb.b});box-shadow:0 0 12px rgba(${light.rgb.r},${light.rgb.g},${light.rgb.b},.55)"></span>
        ${fmt(light.rgb.r, 1)}, ${fmt(light.rgb.g, 1)}, ${fmt(light.rgb.b, 1)}</td>
      <td>${fmt(light.intensity, 3)}</td>
      <td>${fmt(light.pan, 2)}</td>
      <td>${fmt(light.tilt, 2)}</td>
      <td>${fmt(light.focus, 2)}</td>
      <td>${fmt(light.color_temp, 1)}</td>
      <td>${fmt(light.strobe_hz, 2)}</td>
    </tr>`).join("");
}

function renderStage(output) {
  const stage = dom("stage");
  if (!state?.project) {
    stage.innerHTML = "";
    return;
  }
  const lights = state.project.lights || [];
  if (!lights.length) return;
  const xs = lights.map((l) => l.layout.x);
  const ys = lights.map((l) => l.layout.y);
  const minX = Math.min(...xs), maxX = Math.max(...xs);
  const minY = Math.min(...ys), maxY = Math.max(...ys);
  const spanX = Math.max(1, maxX - minX);
  const spanY = Math.max(1, maxY - minY);
  const outputMap = new Map((output?.lights || []).map((l) => [l.light_id, l]));
  const width = 1000, height = 430, pad = 70;
  let svg = `<svg viewBox="0 0 ${width} ${height}" role="img" aria-label="stage light layout">`;
  svg += `<defs>
    <filter id="glow"><feGaussianBlur stdDeviation="8" result="b"/><feMerge><feMergeNode in="b"/><feMergeNode in="SourceGraphic"/></feMerge></filter>
    <linearGradient id="truss" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="#243246"/><stop offset=".5" stop-color="#5c718e"/><stop offset="1" stop-color="#243246"/></linearGradient>
  </defs>`;
  svg += `<rect x="20" y="20" width="${width - 40}" height="${height - 40}" rx="18" fill="none" stroke="#26384e"/>`;
  svg += `<line x1="60" y1="54" x2="${width - 60}" y2="54" stroke="url(#truss)" stroke-width="7"/>`;
  for (const light of lights) {
    const out = outputMap.get(light.id);
    const x = pad + ((light.layout.x - minX) / spanX) * (width - pad * 2);
    const y = pad + ((light.layout.y - minY) / spanY) * (height - pad * 2);
    const rgb = out ? out.rgb : { r: 30, g: 52, b: 78 };
    const intensity = out ? out.intensity : 0.2;
    const radius = 12 + Math.min(10, (light.layout.z || 0));
    const pan = (out?.pan || 0) * Math.PI / 180;
    const tilt = (out?.tilt || 0) * Math.PI / 180;
    const beamLength = 80 + (light.layout.z || 0) * 7;
    const bx = x + Math.sin(pan) * beamLength;
    const by = y + Math.cos(pan) * Math.cos(tilt) * beamLength * .52;
    svg += `<polygon points="${x},${y} ${bx - 34},${by} ${bx + 34},${by}" fill="rgba(${rgb.r},${rgb.g},${rgb.b},${0.04 + intensity * 0.16})"/>`;
    svg += `<line x1="${x}" y1="${y}" x2="${bx}" y2="${by}" stroke="rgba(${rgb.r},${rgb.g},${rgb.b},${0.15 + intensity * 0.35})" stroke-width="2"/>`;
    svg += `<circle cx="${x}" cy="${y}" r="${radius + 14}" fill="rgba(${rgb.r},${rgb.g},${rgb.b},${0.05 + intensity * 0.12})" filter="url(#glow)"/>`;
    svg += `<circle cx="${x}" cy="${y}" r="${radius}" fill="rgb(${rgb.r},${rgb.g},${rgb.b})" stroke="#dceaff" stroke-opacity=".55" stroke-width="1.5"/>`;
    svg += `<text x="${x}" y="${y + radius + 18}" fill="#a9bbd1" font-size="12" text-anchor="middle">${escapeHTML(light.id)}</text>`;
  }
  svg += `<text x="28" y="${height - 20}" fill="#5f7590" font-size="11">TRUSS / TOP VIEW · intensity-aware RGB + strobe gate</text>`;
  svg += `</svg>`;
  stage.innerHTML = svg;
}

function renderTimeline() {
  const canvas = dom("timeline");
  const rect = canvas.getBoundingClientRect();
  const dpr = window.devicePixelRatio || 1;
  const width = Math.max(600, rect.width);
  const height = 150;
  canvas.width = width * dpr;
  canvas.height = height * dpr;
  const ctx = canvas.getContext("2d");
  ctx.scale(dpr, dpr);
  ctx.clearRect(0, 0, width, height);
  ctx.fillStyle = "#0b131d";
  ctx.fillRect(0, 0, width, height);
  const p = state.project;
  const duration = p.duration_ms || 1;
  const left = 34, right = width - 16, top = 24, laneHeight = 86;
  const xOf = (ms) => left + (right - left) * (ms / duration);

  ctx.strokeStyle = "rgba(80,104,132,.28)";
  ctx.fillStyle = "#657b96";
  ctx.font = "10px ui-monospace, monospace";
  for (let i = 0; i <= 10; i++) {
    const x = left + (right - left) * i / 10;
    ctx.beginPath(); ctx.moveTo(x, top); ctx.lineTo(x, top + laneHeight + 10); ctx.stroke();
    ctx.fillText(`${(duration * i / 10 / 1000).toFixed(1)}s`, x - 12, height - 14);
  }
  ctx.strokeStyle = "rgba(177,140,255,.3)";
  (state.windows || []).forEach((w) => {
    if (!w.start_us) return;
    const x = xOf(w.start_us / 1000);
    ctx.beginPath(); ctx.moveTo(x, top - 8); ctx.lineTo(x, top + laneHeight + 10); ctx.stroke();
  });
  const cues = p.cues || [];
  const step = Math.max(1, Math.ceil(cues.length / 8000));
  for (let i = 0; i < cues.length; i += step) {
    const cue = cues[i];
    const x = xOf(cue.time_ms);
    const color = colors[cue.kind] || "#8da4bd";
    const y = top + 24 + ((i * 37) % (laneHeight - 30));
    ctx.globalAlpha = .68;
    ctx.strokeStyle = color;
    ctx.beginPath(); ctx.moveTo(x, top); ctx.lineTo(x, y); ctx.stroke();
    ctx.globalAlpha = 1;
    ctx.fillStyle = color;
    ctx.beginPath(); ctx.arc(x, y, 2.7, 0, Math.PI * 2); ctx.fill();
  }
  const playhead = xOf(currentFrame * 1000 / p.fps);
  ctx.strokeStyle = "#ffffff";
  ctx.globalAlpha = .85;
  ctx.beginPath(); ctx.moveTo(playhead, top - 10); ctx.lineTo(playhead, top + laneHeight + 10); ctx.stroke();
  ctx.fillStyle = "#ffffff";
  ctx.beginPath(); ctx.moveTo(playhead, top - 10); ctx.lineTo(playhead - 5, top - 18); ctx.lineTo(playhead + 5, top - 18); ctx.fill();
  ctx.globalAlpha = 1;
  ctx.fillStyle = "#98abc4";
  ctx.fillText(`${cues.length} cues · ${state.params.window_ms || p.window_ms} ms windows`, left, 15);
  if (selectedCue) {
    const x = xOf(selectedCue.time_ms);
    ctx.strokeStyle = "#ffffff";
    ctx.setLineDash([4, 4]);
    ctx.beginPath(); ctx.moveTo(x, top); ctx.lineTo(x, top + laneHeight + 10); ctx.stroke();
    ctx.setLineDash([]);
  }
}

function renderWindows() {
  const body = dom("windowRows");
  const windows = state.windows || [];
  const limit = Math.min(400, windows.length);
  body.innerHTML = windows.slice(0, limit).map((w) => {
    return `<tr>
      <td>${w.index}</td>
      <td>${fmt((w.start_us || 0) / 1000, 1)}–${fmt((w.end_us || 0) / 1000, 1)} ms</td>
      <td class="state-${w.state}">${w.state}</td>
      <td>${w.attempts || 0}</td>
      <td>${w.cue_start ?? "-"} → ${w.cue_offset ?? "-"}</td>
      <td title="${escapeHTML(w.error || "")}">${escapeHTML((w.error || "").slice(0, 28))}</td>
    </tr>`;
  }).join("") || `<tr><td colspan="6" class="muted">尚未调度</td></tr>`;
}

function renderDecisions() {
  const body = dom("decisionRows");
  const decisions = state.arbitrations || [];
  body.innerHTML = decisions.slice(0, 300).map((d) => `<tr>
    <td>${fmt(d.time_us / 1000, 1)} ms</td>
    <td>${escapeHTML(d.light_id)}</td>
    <td>${escapeHTML(d.field)}</td>
    <td>${escapeHTML(d.winner)}</td>
    <td>${escapeHTML(d.rule)}</td>
    <td>${(d.candidates || []).map((c) => `${c.source_type}:${c.source_id}/${c.cue_id}=${fmt(c.value, 2)}`).join(" | ")}</td>
  </tr>`).join("") || `<tr><td colspan="6" class="muted">没有同刻冲突</td></tr>`;
}

function renderBudget() {
  const records = state.budget_log || [];
  const blocked = records.filter((r) => r.event === "blocked").length;
  dom("budgetSummary").textContent = blocked
    ? `${blocked} 次预算阻塞，全部进入等待队列`
    : "未发生阻塞";
  const body = dom("budgetRows");
  body.innerHTML = records.slice(-80).reverse().map((r) => `<tr>
    <td>${r.window}</td>
    <td>${escapeHTML(r.event)}</td>
    <td>${r.wait_ms || 0} ms</td>
    <td title="${escapeHTML(r.reason || "")}">${escapeHTML((r.reason || "").slice(0, 30))}</td>
  </tr>`).join("") || `<tr><td colspan="4" class="muted">无记录</td></tr>`;
}

function renderMerge() {
  const spans = state.merge_spans || [];
  const total = state.decision?.mode === "merged" ? spans.length : 0;
  dom("mergeSummary").textContent = total ? `${total} 个合并段` : "当前模式未合并";
  const body = dom("mergeRows");
  const reason = (state.decision?.reasons || []).join("; ");
  body.innerHTML = spans.slice(0, 300).map((span) => `<tr>
    <td>${fmt(span.start_us / 1000, 1)}–${fmt(span.end_us / 1000, 1)} ms</td>
    <td>${escapeHTML(span.first_cue_id)} … ${escapeHTML(span.last_cue_id)}</td>
    <td>${fmt(span.error_bound, 3)}</td>
    <td title="${escapeHTML(reason)}">${escapeHTML(reason.slice(0, 40))}</td>
  </tr>`).join("") || `<tr><td colspan="4" class="muted">无合并段</td></tr>`;
}

function renderVerify() {
  const report = state.last_verify;
  if (!report) {
    dom("verifyResult").textContent = "尚未执行";
    return;
  }
  dom("verifyResult").textContent = report.consistent ? "逐帧一致" : "发现不一致";
  dom("verifyResult").style.color = report.consistent ? "var(--green)" : "var(--red)";
  const cards = ["realtime", "recovery", "full"].map((name) => {
    const item = report[name];
    return `<div class="verify-card ${report.consistent ? "good" : ""}">
      <h3>${name}</h3>
      <div>${item.progress.committed}/${item.progress.total} windows</div>
      <div>${item.duration / 1e6 > 1 ? `${fmt(item.duration / 1e9, 2)} s` : `${fmt(item.duration / 1e6, 0)} ms`}</div>
      <div class="muted">commits: ${(item.commit_order || []).slice(0, 12).join(",")}${item.commit_order?.length > 12 ? "…" : ""}</div>
    </div>`;
  }).join("");
  const divergence = report.first_divergence ? `<div class="verify-card bad verify-wide">
    <h3>首个不一致</h3>
    <div>frame ${report.first_divergence.frame ?? "-"} · light ${escapeHTML(report.first_divergence.light_id || "-")} · field ${escapeHTML(report.first_divergence.field || "-")}</div>
    <div class="muted">left=${escapeHTML(report.first_divergence.left || "")} right=${escapeHTML(report.first_divergence.right || "")}</div>
  </div>` : "";
  dom("verifyContent").innerHTML = `${cards}
    <div class="verify-card verify-wide ${report.consistent ? "good" : "bad"}">
      <h3>${report.consistent ? "一致" : "E_DIVERGE"}</h3>
      <div>frames ${report.frames_compared} · windows ${report.windows_compared} · blocked budget events ${report.blocked_budget_events} · mode ${report.mode}</div>
    </div>${divergence}`;
}

function renderGroups() {
  if (!state?.project) return;
  const select = dom("editGroupSelect");
  select.innerHTML = (state.project.groups || []).map((g) => `<option value="${escapeHTML(g.id)}">${escapeHTML(g.id)} · p${g.priority}</option>`).join("");
  if (selectedGroup) select.value = selectedGroup;
  populateGroupEditor();
}

function populateGroupEditor() {
  const group = (state.project.groups || []).find((g) => g.id === dom("editGroupSelect").value);
  if (!group) return;
  dom("editMembers").value = (group.members || []).join(", ");
  dom("editGroupPriority").value = group.priority;
}

async function runMode(mode) {
  if (busy) return;
  busy = true;
  setButtonsDisabled(true);
  try {
    state = await call("Run", mode, 0);
    render();
    await refreshFrame(currentFrame);
  } catch (err) {
    state = await call("GetState").catch(() => state);
    if (state) render();
    showError(err);
  } finally {
    busy = false;
    setButtonsDisabled(false);
  }
}

function setButtonsDisabled(disabled) {
  ["runBtn", "pauseBtn", "resumeBtn", "fullBtn", "verifyBtn", "generateBtn", "applyParamsBtn"].forEach((id) => {
    dom(id).disabled = disabled;
  });
}

async function applyParams() {
  try {
    const params = {
      ...state.params,
      max_workers: Number(dom("workers").value),
      per_worker_buffer_bytes: Number(dom("buffer").value),
      memory_budget_bytes: Number(dom("memoryBudget").value) || 0,
      window_ms: Number(dom("windowMs").value) || 0,
      preview_fps: Number(dom("previewFps").value),
      force_mode: dom("forceMode").value,
    };
    state = await call("UpdateParams", params);
    render();
    await refreshFrame(currentFrame);
  } catch (err) { showError(err); }
}

function openCueEditor(cue) {
  selectedCue = cue;
  dom("editorPanel").classList.remove("hidden");
  dom("editorTitle").textContent = `${cue.id} · ${cue.kind} · ${cue.light_id || cue.group_id}`;
  dom("editTime").value = cue.time_ms;
  dom("editTransition").value = cue.transition_ms;
  dom("editPriority").value = cue.priority;
  dom("editEasing").value = cue.easing;
  dom("editIntensity").value = "";
  dom("editR").value = "";
  dom("editG").value = "";
  dom("editB").value = "";
  renderTimeline();
  dom("editorPanel").scrollIntoView({ behavior: "smooth", block: "center" });
}

async function submitCueEdit(event) {
  event.preventDefault();
  if (!selectedCue) return;
  const values = JSON.parse(JSON.stringify(selectedCue.values || {}));
  if (dom("editIntensity").value !== "") values.intensity = Number(dom("editIntensity").value);
  const r = dom("editR").value, g = dom("editG").value, b = dom("editB").value;
  if (r !== "" || g !== "" || b !== "") {
    values.rgb = {
      r: r === "" ? (values.rgb?.r || 0) : Number(r),
      g: g === "" ? (values.rgb?.g || 0) : Number(g),
      b: b === "" ? (values.rgb?.b || 0) : Number(b),
    };
  }
  const patch = {
    time_ms: Number(dom("editTime").value),
    transition_ms: Number(dom("editTransition").value),
    priority: Number(dom("editPriority").value),
    easing: dom("editEasing").value,
    values,
  };
  try {
    state = await call("EditCue", selectedCue.id, patch);
    selectedCue = state.project.cues.find((c) => c.id === selectedCue.id) || null;
    render();
    showInfo(state.last_notice || "编辑已提交。");
  } catch (err) { showError(err); }
}

async function submitGroupEdit(event) {
  event.preventDefault();
  const groupID = dom("editGroupSelect").value;
  const members = dom("editMembers").value.split(",").map((item) => item.trim()).filter(Boolean);
  try {
    state = await call("EditGroup", groupID, {
      members,
      priority: Number(dom("editGroupPriority").value),
    });
    selectedGroup = groupID;
    render();
    showInfo(state.last_notice || "分组编辑已提交。");
  } catch (err) { showError(err); }
}

function bind() {
  dom("generateBtn").onclick = async () => {
    try {
      state = await call("Generate", dom("preset").value, Number(dom("seed").value));
      currentFrame = 0;
      hydrateControls();
      render();
      await refreshFrame(0);
    } catch (err) { showError(err); }
  };
  dom("editGroupsBtn").onclick = () => {
    dom("editorPanel").classList.remove("hidden");
    dom("editorTitle").textContent = "分组控制";
    populateGroupEditor();
    dom("groupForm").scrollIntoView({ behavior: "smooth", block: "center" });
  };
  dom("loadBtn").onclick = async () => {
    try {
      state = await call("LoadProject", dom("projectPath").value);
      currentFrame = 0;
      hydrateControls();
      render();
      await refreshFrame(0);
    } catch (err) { showError(err); }
  };
  dom("saveBtn").onclick = async () => {
    try {
      await call("SaveProject", dom("savePath").value);
      showInfo("工程与真值清单已写入目录。");
    } catch (err) { showError(err); }
  };
  dom("applyParamsBtn").onclick = applyParams;
  dom("runBtn").onclick = () => runMode("realtime");
  dom("resumeBtn").onclick = () => runMode("resume");
  dom("fullBtn").onclick = () => runMode("full");
  dom("pauseBtn").onclick = async () => {
    try { await call("Pause"); showInfo("已请求暂停；仅已提交窗口保留。"); }
    catch (err) { showError(err); }
  };
  dom("verifyBtn").onclick = async () => {
    if (busy) return;
    busy = true; setButtonsDisabled(true);
    try {
      await call("Verify");
      state = await call("GetState");
      render();
    } catch (err) {
      state = await call("GetState").catch(() => state);
      render();
      showError(err);
    } finally { busy = false; setButtonsDisabled(false); }
  };
  dom("frameSlider").oninput = (event) => refreshFrame(Number(event.target.value));
  dom("frameInput").onchange = (event) => refreshFrame(Number(event.target.value));
  dom("playBtn").onclick = () => togglePlayback();
  dom("timeline").onclick = (event) => {
    const rect = event.target.getBoundingClientRect();
    const width = Math.max(600, rect.width);
    const x = event.clientX - rect.left;
    const left = 34, right = width - 16;
    const cues = state?.project?.cues || [];
    const duration = state?.project?.duration_ms || 1;
    let best = null, distance = 8;
    for (const cue of cues) {
      const cueX = left + (right - left) * (cue.time_ms / duration);
      const d = Math.abs(cueX - x);
      if (d < distance) { distance = d; best = cue; }
    }
    if (best) openCueEditor(best);
  };
  dom("cueForm").onsubmit = submitCueEdit;
  dom("groupForm").onsubmit = submitGroupEdit;
  dom("closeEditorBtn").onclick = () => dom("editorPanel").classList.add("hidden");
  dom("editGroupSelect").onchange = () => {
    selectedGroup = dom("editGroupSelect").value;
    populateGroupEditor();
  };
  window.addEventListener("resize", () => renderTimeline());
  window.addEventListener("keydown", (event) => {
    if (event.key === "ArrowRight") refreshFrame(currentFrame + 1);
    if (event.key === "ArrowLeft") refreshFrame(currentFrame - 1);
    if (event.code === "Space") { event.preventDefault(); dom("runBtn").click(); }
  });
}

function togglePlayback() {
  if (playbackTimer) {
    clearInterval(playbackTimer);
    playbackTimer = null;
    dom("playBtn").textContent = "播放";
    return;
  }
  const fps = Math.min(30, Math.max(1, state?.project?.fps || 30));
  dom("playBtn").textContent = "暂停";
  playbackTimer = setInterval(async () => {
    const frameCount = state?.decision?.frame_count || 1;
    if (currentFrame >= frameCount - 1) {
      currentFrame = 0;
    } else {
      currentFrame++;
    }
    await refreshFrame(currentFrame);
  }, Math.max(33, 1000 / fps));
}

function showInfo(message) {
  const toast = dom("toast");
  toast.style.borderColor = "rgba(88,227,161,.55)";
  toast.style.background = "#10241d";
  toast.style.color = "#c9ffe5";
  toast.textContent = message;
  toast.classList.remove("hidden");
  clearTimeout(showInfo.timer);
  showInfo.timer = setTimeout(() => toast.classList.add("hidden"), 4200);
}

function showError(err) {
  const toast = dom("toast");
  toast.style.borderColor = "rgba(255,107,122,.55)";
  toast.style.background = "#24151a";
  toast.style.color = "#ffd6db";
  let text = err?.message || String(err);
  if (err?.code) text = `${err.code}: ${err.message}\n${JSON.stringify(err.details || [], null, 2)}`;
  toast.textContent = text;
  toast.classList.remove("hidden");
  clearTimeout(showError.timer);
  showError.timer = setTimeout(() => toast.classList.add("hidden"), 9000);
}

document.addEventListener("DOMContentLoaded", () => {
  bind();
  boot();
});
