const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];

const state = {
  sets: [], set: null, info: null, version: 0, params: {},
  preview: null, exact: null, task: null, poll: null,
  view: "preview", diag: "matches",
  previewTimer: null, exactTimer: null, paramTimer: null,
  pendingParams: {}, previewAbort: null, interacting: false,
};

const PARAM_DEFS = [
  ["feature_budget", "特征预算", 500, 24000, 100, 0],
  ["preview_feature_budget", "预览特征预算", 100, 8000, 100, 0],
  ["preview_downsample", "预览降采样", 1, 4, 1, 0],
  ["scale_levels", "尺度层数", 1, 6, 1, 0],
  ["response_threshold", "响应阈值", 0.001, 0.05, 0.001, 3],
  ["match_ratio", "匹配比率", 0.5, 0.97, 0.01, 2],
  ["match_min", "最少匹配", 4, 100, 1, 0],
  ["ransac_threshold", "重投影阈值", 0.2, 5, 0.1, 1],
  ["ransac_iterations", "RANSAC 迭代", 100, 12000, 100, 0],
  ["exposure_max_gain", "曝光最大增益", 1.05, 3, 0.05, 2],
  ["seam_gradient_weight", "缝合梯度权重", 0, 4, 0.1, 1],
  ["fusion_levels", "融合层数", 1, 6, 1, 0],
  ["fusion_sigma", "融合 sigma", 0.4, 4, 0.1, 1],
  ["max_workers", "最大并行度", 1, 8, 1, 0],
  ["divergence_coverage_tol", "覆盖率容差", 0, 0.2, 0.005, 3],
];

function api(path, opts = {}) {
  return fetch(path, {
    headers: { "Content-Type": "application/json", ...(opts.headers || {}) },
    ...opts,
  }).then(async (r) => {
    const text = await r.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch { data = text; }
    if (!r.ok) {
      const err = new Error(data?.message || `HTTP ${r.status}`);
      err.payload = data;
      throw err;
    }
    return data;
  });
}

function esc(v) {
  return String(v).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}
function shortHash(h) { return h ? h.slice(0, 10) + "…" : "—"; }
function fmt(v, digits = 2) {
  if (v === null || v === undefined || Number.isNaN(Number(v))) return "—";
  return Number(v).toFixed(digits);
}

function log(msg, isErr = false) {
  const el = document.createElement("div");
  el.className = "entry" + (isErr ? " err" : "");
  el.textContent = new Date().toLocaleTimeString() + "  " + msg;
  $("#log").prepend(el);
  while ($("#log").children.length > 80) $("#log").lastChild.remove();
}

function showError(err) {
  const e = err.payload || { code: "E_INTERNAL", message: err.message };
  const parts = [`${e.code || "E_INTERNAL"}: ${e.message || err.message}`];
  if (e.field) parts.push(`field: ${e.field}`);
  if (e.actual !== undefined) parts.push(`actual: ${JSON.stringify(e.actual)}`);
  if (e.threshold !== undefined) parts.push(`threshold: ${JSON.stringify(e.threshold)}`);
  if (e.details) parts.push(`details: ${JSON.stringify(e.details)}`);
  $("#errorBanner").textContent = parts.join("\n");
  $("#errorBanner").classList.remove("hidden");
  clearTimeout(showError.t);
  showError.t = setTimeout(() => $("#errorBanner").classList.add("hidden"), 9000);
  log(parts[0], true);
}

function setLoading(on) { $("#canvasLoading").classList.toggle("visible", on); }
function showImage(url, mode) {
  if (!url) return;
  const img = $("#canvasImage");
  img.onload = () => {
    img.classList.add("visible");
    $("#emptyState").classList.add("hidden");
  };
  img.src = url + (url.includes("?") ? "&" : "?") + "t=" + Date.now();
  $("#canvasMode").textContent = mode;
}

async function boot() {
  try {
    const h = await api("/api/health");
    $("#healthDot").classList.add("ok");
    $("#healthText").textContent = "本机服务在线";
    $("#algoVersion").textContent = h.algorithm_version;
    const r = await api("/api/sets");
    state.sets = r.sets || [];
    const sel = $("#setSelect");
    sel.innerHTML = "";
    if (!state.sets.length) {
      sel.innerHTML = "<option>未发现样本</option>";
      log("未发现样本，请运行 samplegen", true);
      return;
    }
    for (const s of state.sets) {
      const o = document.createElement("option");
      o.value = s.id;
      o.textContent = `${s.id} · ${s.images} 张`;
      sel.appendChild(o);
    }
    sel.addEventListener("change", () => loadSet(sel.value));
    buildParams();
    await loadSet(state.sets[0].id);
  } catch (e) {
    $("#healthDot").classList.add("bad");
    showError(e);
  }
}

async function loadSet(id) {
  clearTimers();
  state.set = id;
  state.preview = null;
  state.exact = null;
  state.task = null;
  state.view = "preview";
  $("#canvasImage").classList.remove("visible");
  $("#emptyState").classList.remove("hidden");
  $("#diagImage").classList.remove("visible");
  $("#diagEmpty").classList.remove("hidden");
  $("#pauseBtn").disabled = true;
  $("#commitAllBtn").disabled = true;
  try {
    const info = await api(`/api/sets/${encodeURIComponent(id)}`);
    state.info = info;
    state.version = info.version;
    state.params = info.params;
    $("#setMeta").textContent = `${info.images} 张 · ${info.canvas.width}×${info.canvas.height}`;
    $("#contentHash").textContent = shortHash(info.content_hash);
    $("#committedCount").textContent = info.committed;
    $("#versionPill").textContent = "v" + info.version;
    $("#workers").textContent = info.params.max_workers;
    $("#canvasSize").textContent = `${info.canvas.width} × ${info.canvas.height} truth`;
    $("#pairCount").textContent = `${info.images - 1} 对`;
    $("#taskState").textContent = "空闲";
    $("#taskVersion").textContent = "—";
    $("#cumError").textContent = "—";
    $("#seamMetric").textContent = "—";
    $("#pairList").innerHTML = "";
    if (info.warning) {
      log(info.warning, true);
      showError({ payload: { code: "E_CHECKPOINT", message: info.warning } });
    }
    syncParamsUI(info.params);
    log(`载入 ${id}，参数版本 v${info.version}，冻结 ${info.committed} 对`);
    await previewNow();
    scheduleExact(80);
  } catch (e) {
    showError(e);
  }
}

function buildParams() {
  const root = $("#params");
  root.innerHTML = "";
  for (const [key, label, min, max, step, digits] of PARAM_DEFS) {
    const row = document.createElement("div");
    row.className = "param";
    row.innerHTML = `<label for="p_${key}">${label}</label><output id="o_${key}">—</output>
      <input id="p_${key}" type="range" min="${min}" max="${max}" step="${step}" data-key="${key}" data-digits="${digits}">`;
    root.appendChild(row);
    const input = row.querySelector("input");
    input.addEventListener("pointerdown", () => {
      state.interacting = true;
      clearTimeout(state.exactTimer);
    });
    input.addEventListener("input", () => {
      const v = Number(input.value);
      updateParamLabel(key, v, digits);
      state.pendingParams[key] = v;
      scheduleParamCommit();
    });
    input.addEventListener("change", () => {
      state.interacting = false;
      scheduleExact(520);
    });
    input.addEventListener("pointerup", () => {
      state.interacting = false;
      scheduleExact(520);
    });
  }
}

function syncParamsUI(params) {
  state.params = params;
  for (const [key, , , , , digits] of PARAM_DEFS) {
    const input = $("#p_" + key);
    if (!input) continue;
    input.value = params[key];
    updateParamLabel(key, params[key], digits);
  }
  $("#workers").textContent = params.max_workers;
}

function updateParamLabel(key, v, digits) {
  $("#o_" + key).textContent = Number(v).toFixed(Number(digits));
}

function scheduleParamCommit() {
  clearTimeout(state.paramTimer);
  state.paramTimer = setTimeout(commitParams, 120);
}

async function commitParams() {
  if (!state.set || !Object.keys(state.pendingParams).length) return;
  const body = state.pendingParams;
  state.pendingParams = {};
  try {
    const snap = await api(`/api/sets/${encodeURIComponent(state.set)}/params`, {
      method: "POST", body: JSON.stringify(body),
    });
    if (snap.version !== state.version) {
      state.version = snap.version;
      state.params = snap.params;
      state.exact = null;
      $("#versionPill").textContent = "v" + snap.version;
      log(`参数快照 v${snap.version} 已生成`);
      schedulePreview(45);
    }
  } catch (e) {
    showError(e);
  }
}

function schedulePreview(delay = 0) {
  clearTimeout(state.previewTimer);
  state.previewTimer = setTimeout(previewNow, delay);
}

async function previewNow() {
  if (!state.set) return;
  if (state.previewAbort) state.previewAbort.abort();
  const ctrl = new AbortController();
  state.previewAbort = ctrl;
  const requestedVersion = state.version;
  setLoading(true);
  $("#canvasMode").textContent = "粗略预览";
  try {
    const res = await api(`/api/sets/${encodeURIComponent(state.set)}/preview`, {
      method: "POST", body: JSON.stringify({ version: requestedVersion }), signal: ctrl.signal,
    });
    if (requestedVersion < state.version) return;
    state.preview = res;
    state.version = res.param_version;
    $("#previewDown").textContent = `${res.downsample}×`;
    $("#previewBudget").textContent = res.feature_budget;
    $("#canvasSize").textContent = `${res.canvas.width} × ${res.canvas.height} · ${res.image_width}×${res.image_height} preview`;
    $("#pathBadge").textContent = `coarse · ${res.duration_ms} ms`;
    $("#timingBadge").textContent = `${res.duration_ms} ms / limit ${res.elapsed_budget_ms} ms`;
    if (state.view === "preview" || !state.exact) showImage(res.panorama_url, "粗略预览");
    log(`v${res.param_version} 预览完成：${res.duration_ms} ms，降采样 ${res.downsample}×，预算 ${res.feature_budget}`);
    renderPairs(res.pairs);
  } catch (e) {
    if (e.name !== "AbortError") showError(e);
  } finally {
    if (state.previewAbort === ctrl) setLoading(false);
  }
}

function scheduleExact(delay = 420) {
  clearTimeout(state.exactTimer);
  if (state.interacting) return;
  state.exactTimer = setTimeout(startExact, delay);
}

async function startExact() {
  if (!state.set) return;
  if (state.pendingParams && Object.keys(state.pendingParams).length) await commitParams();
  try {
    const task = await api(`/api/sets/${encodeURIComponent(state.set)}/exact`, {
      method: "POST", body: JSON.stringify({ version: state.version, auto_commit: false }),
    });
    state.task = task;
    $("#pauseBtn").disabled = false;
    $("#taskState").textContent = "精算中";
    $("#taskVersion").textContent = "v" + task.version;
    setLoading(true);
    $("#canvasMode").textContent = "精确重算";
    log(`v${task.version} 精算任务启动 ${task.id}`);
    pollTask(task.id);
  } catch (e) {
    setLoading(false);
    showError(e);
  }
}

function pollTask(id) {
  clearInterval(state.poll);
  state.poll = setInterval(async () => {
    try {
      const v = await api(`/api/sets/${encodeURIComponent(state.set)}/tasks/${id}`);
      state.task = v;
      $("#taskState").textContent = v.status === "running" ? `${v.phase} ${v.progress}%` : v.status;
      $("#taskVersion").textContent = "v" + v.version;
      if (v.status === "running") {
        setLoading(true);
        return;
      }
      clearInterval(state.poll);
      state.poll = null;
      setLoading(false);
      $("#pauseBtn").disabled = true;
      if (v.status === "completed") {
        state.exact = v.result;
        state.version = v.result.param_version;
        $("#commitAllBtn").disabled = false;
        $("#canvasSize").textContent = `${v.result.canvas.width} × ${v.result.canvas.height} · exact`;
        $("#pathBadge").textContent = `full · ${v.result.duration_ms} ms`;
        $("#timingBadge").textContent = `preview ${v.result.divergence.preview_ms} ms / exact ${v.result.duration_ms} ms`;
        $("#divergeBadge").textContent = v.result.divergence.consistent ? "✓ consistent" : "✕ divergent";
        $("#coverageBadge").textContent = `${fmt(v.result.divergence.max_coverage_delta, 4)} / ${v.result.divergence.coverage_tolerance}`;
        $("#cumError").textContent = fmt(v.result.cumulative_error, 4);
        $("#seamMetric").textContent = `${fmt(v.result.seam_gradient_p95, 1)} / ${fmt(v.result.seam_gradient_tolerance, 1)}`;
        $("#committedCount").textContent = v.result.committed_count;
        showImage(v.result.panorama_url, "精算全景");
        state.view = "exact";
        setActiveTab("exact");
        renderPairs(v.result.pairs);
        setDiag("matches");
        log(`v${v.result.param_version} 精算完成：${v.result.duration_ms} ms，判定一致=${v.result.divergence.consistent}`);
      } else if (v.status === "paused") {
        $("#taskState").textContent = "已暂停";
        log("精算任务已暂停；已冻结图像对保留", true);
        schedulePreview(0);
      } else {
        $("#taskState").textContent = "失败";
        if (v.error) showError({ payload: v.error });
      }
    } catch (e) {
      clearInterval(state.poll);
      state.poll = null;
      setLoading(false);
      showError(e);
    }
  }, 260);
}

function renderPairs(pairs) {
  const root = $("#pairList");
  root.innerHTML = "";
  const list = (pairs || []).filter((p) => p.pair_index > 0);
  $("#pairCount").textContent = `${list.length} 对`;
  if (!list.length) {
    root.innerHTML = '<div class="hint">运行预览或精算后显示逐对报告。</div>';
    return;
  }
  for (const p of list) {
    const card = document.createElement("div");
    card.className = "pair-card" + (!p.decision ? " failed" : "") + (p.committed ? " committed" : "");
    const name = (p.source_image || "").split("/").pop();
    const canCommit = state.exact && p.decision && !p.committed;
    card.innerHTML = `
      <div class="pair-head">
        <strong title="${esc(p.source_image)}">${esc(name)}</strong>
        <span class="badge ${p.decision ? "ok" : "bad"}">${p.decision ? "ok" : (p.error_code || "fail")}</span>
        ${p.committed ? `<span class="badge freeze">frozen v${p.committed_version || p.param_version}</span>` : ""}
      </div>
      <div class="pair-metrics">
        <div class="metric"><span>matches</span><code>${p.matches}</code></div>
        <div class="metric"><span>inliers</span><code>${p.inliers}</code></div>
        <div class="metric"><span>residual</span><code>${fmt(p.residual_median, 2)}</code></div>
        <div class="metric"><span>H err</span><code>${fmt(p.homography_err, 3)}</code></div>
        <div class="metric"><span>coverage</span><code>${fmt(p.overlap_coverage, 3)}</code></div>
        <div class="metric"><span>cond</span><code>${fmt(p.condition_number, 1)}</code></div>
        <div class="metric"><span>rate</span><code>${fmt(p.inlier_rate, 2)}</code></div>
        <div class="metric"><span>version</span><code>v${p.param_version}</code></div>
      </div>
      <div class="pair-actions">
        ${canCommit ? `<button class="btn mini secondary" data-commit="${p.pair_index}">提交冻结</button>` : ""}
        ${!p.decision ? `<button class="btn mini danger" data-retry="${p.pair_index}">重试此对</button>` : ""}
      </div>`;
    root.appendChild(card);
  }
  $$("[data-commit]").forEach((b) => b.addEventListener("click", () => commitPair(Number(b.dataset.commit))));
  $$("[data-retry]").forEach((b) => b.addEventListener("click", retryPair));
}

async function commitPair(idx) {
  if (!state.exact) return;
  try {
    const st = await api(`/api/sets/${encodeURIComponent(state.set)}/commit`, {
      method: "POST", body: JSON.stringify({ version: state.exact.param_version, pair_index: idx }),
    });
    log(`冻结 ${st.source_image} @ v${st.committed_version}`);
    if (state.exact) {
      const p = state.exact.pairs.find((x) => x.pair_index === idx);
      if (p) { p.committed = true; p.committed_version = st.committed_version; }
      state.exact.committed_count = state.exact.pairs.filter((x) => x.committed).length;
      $("#committedCount").textContent = state.exact.committed_count;
      renderPairs(state.exact.pairs);
    }
  } catch (e) { showError(e); }
}

async function commitAll() {
  if (!state.exact) return;
  for (const p of state.exact.pairs) {
    if (p.pair_index > 0 && p.decision && !p.committed) await commitPair(p.pair_index);
  }
}

async function retryPair() {
  try {
    const t = await api(`/api/sets/${encodeURIComponent(state.set)}/retry`, { method: "POST", body: "{}" });
    state.task = t;
    $("#pauseBtn").disabled = false;
    log(`失败对重试任务 ${t.id} 启动`);
    pollTask(t.id);
  } catch (e) { showError(e); }
}

function setDiag(kind) {
  state.diag = kind;
  $$("#diagTabs button").forEach((b) => b.classList.toggle("active", b.dataset.src === kind));
  if (!state.exact) return;
  const map = {
    matches: state.exact.match_overlay_url,
    overlay: state.exact.diagnostics_url,
    seam: state.exact.seam_overlay_url,
    heatmap: state.exact.heatmap_url,
  };
  const url = map[kind];
  if (!url) return;
  $("#diagImage").src = url + "?t=" + Date.now();
  $("#diagImage").classList.add("visible");
  $("#diagEmpty").classList.add("hidden");
}

function setActiveTab(view) {
  state.view = view;
  $$("#viewTabs button").forEach((b) => b.classList.toggle("active", b.dataset.view === view));
  if (view === "preview" && state.preview) showImage(state.preview.panorama_url, "粗略预览");
  if (view === "exact" && state.exact) showImage(state.exact.panorama_url, "精算全景");
}

async function pauseTask() {
  if (!state.task) return;
  try {
    const t = await api(`/api/sets/${encodeURIComponent(state.set)}/tasks/${state.task.id}/pause`, { method: "POST", body: "{}" });
    log(`暂停请求：${t.status}`);
  } catch (e) { showError(e); }
}

async function resetCommits() {
  if (!confirm("清除全部冻结图像对并重新计算？")) return;
  try {
    await api(`/api/sets/${encodeURIComponent(state.set)}/reset-commits`, { method: "POST", body: "{}" });
    $("#committedCount").textContent = "0";
    state.exact = null;
    log("冻结图像对已清空");
    scheduleExact(50);
  } catch (e) { showError(e); }
}

function clearTimers() {
  clearInterval(state.poll);
  clearTimeout(state.previewTimer);
  clearTimeout(state.exactTimer);
  clearTimeout(state.paramTimer);
  state.poll = null;
}

$("#viewTabs").addEventListener("click", (e) => {
  const b = e.target.closest("button"); if (b) setActiveTab(b.dataset.view);
});
$("#diagTabs").addEventListener("click", (e) => {
  const b = e.target.closest("button"); if (b) setDiag(b.dataset.src);
});
$("#exactBtn").addEventListener("click", () => { clearTimeout(state.exactTimer); startExact(); });
$("#applyParams").addEventListener("click", async () => { await commitParams(); schedulePreview(0); });
$("#pauseBtn").addEventListener("click", pauseTask);
$("#commitAllBtn").addEventListener("click", commitAll);
$("#resetBtn").addEventListener("click", resetCommits);
$("#clearLog").addEventListener("click", () => { $("#log").innerHTML = ""; });

boot();
