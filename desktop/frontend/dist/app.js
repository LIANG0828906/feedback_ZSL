const $ = (id) => document.getElementById(id);
const state = {
  workspace: null,
  selectedRoute: null,
  selectedSegment: null,
  frame: 0,
  detail: null,
  busy: false,
};

const call = {
  Workspace: () => window.go.workbench.App.Workspace(),
  Open: (req) => window.go.workbench.App.Open(req),
  Generate: (req) => window.go.workbench.App.Generate(req),
  Run: (req) => window.go.workbench.App.Run(req),
  Cancel: () => window.go.workbench.App.Cancel(),
  RetrySegment: (req) => window.go.workbench.App.RetrySegment(req),
  Audit: () => window.go.workbench.App.Audit(),
  Compare: (req) => window.go.workbench.App.Compare(req),
  Export: (out) => window.go.workbench.App.Export(out),
  FrameDetail: (route, frame) => window.go.workbench.App.FrameDetail(route, frame),
  Events: () => window.go.workbench.App.Events(),
};

function toast(message, error = false) {
  const el = $("toast");
  el.textContent = message;
  el.className = "toast show" + (error ? " error" : "");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.className = "toast"; }, 4200);
}

function fmt(v, digits = 3) {
  if (v === null || v === undefined) return "—";
  return Number(v).toFixed(digits);
}

function params() {
  return {
    target_peak_nits: Number($("target-peak").value),
    exposure_ev: Number($("exposure").value),
    tone_map: $("tone-map").value,
    gamut_mapping: $("gamut").value,
    output_bits: 8,
  };
}

function applyWorkspace(ws) {
  state.workspace = ws;
  if (!ws) return;
  $("root-path").value = ws.root || "";
  $("samples-path").value = ws.samples_dir || "";
  $("db-path").value = ws.db_path || "";
  $("truth-fingerprint").textContent = "truth fingerprint · " + (ws.truth_fingerprint || "—");
  $("workers").value = ws.max_workers || 4;
  $("buffer").value = ws.frame_buffer_limit_per_worker || 2;
  if (ws.params) {
    $("target-peak").value = ws.params.target_peak_nits;
    $("exposure").value = ws.params.exposure_ev;
    $("tone-map").value = ws.params.tone_map;
    $("gamut").value = ws.params.gamut_mapping;
  }
  const progress = ws.progress || {};
  $("progress-pill").textContent = `${progress.routes_committed || 0} / ${progress.routes_total || 0} 路 · ${progress.segments_committed || 0} / ${progress.segments_total || 0} 段`;
  const resource = ws.resource || {};
  $("resource-grid").innerHTML = [
    ["并行度", resource.max_workers || ws.max_workers || "—"],
    ["帧缓冲/线程", resource.frame_buffer_limit_per_worker || ws.frame_buffer_limit_per_worker || "—"],
    ["阻塞次数", resource.buffer_blocks ?? 0],
    ["事件数", resource.event_count ?? 0],
  ].map(([k, v]) => `<div class="mini"><b>${v}</b><span>${k}</span></div>`).join("");
  $("buffer-reason").textContent = resource.last_blocked_reason || "未发生缓冲阻塞。";
  renderRoutes(ws.routes || []);
  if (ws.last_error) {
    showResult(ws.last_error, true);
  }
  if (state.selectedRoute) selectRoute(state.selectedRoute, false);
}

function renderRoutes(routes) {
  const host = $("routes");
  host.innerHTML = "";
  if (!routes.length) {
    host.innerHTML = '<p class="muted" style="padding:8px">没有可用路。先生成或打开样本。</p>';
    return;
  }
  for (const route of routes) {
    const card = document.createElement("div");
    card.className = "route-card" + (state.selectedRoute === route.route.id ? " active" : "");
    card.dataset.route = route.route.id;
    const failed = route.segments.some(s => s.error);
    card.innerHTML = `
      <div class="route-top">
        <span class="route-id">${route.route.id}</span>
        <span class="phase ${failed ? "partial" : route.phase}">${failed ? "failed" : route.phase}</span>
      </div>
      <div class="route-meta">
        <span>${route.route.width}×${route.route.height}</span>
        <span>${route.route.encoding}</span>
        <span>${route.route.peak_nits} nits</span>
        <span>${route.route.fps} fps</span>
        <span>${route.frames_committed}/${route.route.frame_count} 帧</span>
      </div>
      <div class="segments">${route.segments.map(s => `<span class="segment ${s.phase} ${s.error ? "failed" : ""} ${state.selectedSegment === s.index && state.selectedRoute === route.route.id ? "selected" : ""}" data-segment="${s.index}" data-start="${s.frame_start}" title="seg ${s.index} · frame ${s.frame_start}..${s.frame_end}${s.error ? "\n" + s.error : ""}">${s.index}</span>`).join("")}</div>`;
    card.onclick = () => selectRoute(route.route.id, true);
    card.querySelectorAll(".segment").forEach(seg => {
      seg.onclick = (event) => {
        event.stopPropagation();
        state.selectedRoute = route.route.id;
        state.selectedSegment = Number(seg.dataset.segment);
        state.frame = Number(seg.dataset.start);
        renderRoutes(state.workspace?.routes || []);
        loadFrameDetail();
      };
    });
    host.appendChild(card);
  }
}

function selectedRouteView() {
  if (!state.workspace || !state.selectedRoute) return null;
  return state.workspace.routes.find(r => r.route.id === state.selectedRoute) || null;
}

async function selectRoute(routeID, loadFrame = true) {
  state.selectedRoute = routeID;
  renderRoutes(state.workspace?.routes || []);
  const route = selectedRouteView();
  if (!route) return;
  if (state.selectedSegment === null || !route.segments.some(s => s.index === state.selectedSegment)) {
    state.selectedSegment = route.segments[0]?.index ?? null;
  }
  state.frame = Math.max(0, Math.min(state.frame, route.route.frame_count - 1));
  if (loadFrame) await loadFrameDetail();
  else await loadFrameDetail(false);
}

async function loadFrameDetail(render = true) {
  const route = selectedRouteView();
  if (!route) return;
  try {
    const detail = await call.FrameDetail(route.route.id, state.frame);
    state.detail = detail;
    if (render) renderFrame(detail);
  } catch (err) {
    toast(String(err), true);
  }
}

function renderFrame(detail) {
  $("frame-label").textContent = `${detail.route_id} · frame ${String(detail.frame_index).padStart(4, "0")}`;
  $("hdr-meta").textContent = `${detail.input_encoding} · ${detail.input_color_space} · ${detail.input_peak_nits} nits`;
  $("sdr-meta").textContent = detail.committed ? "SDR RGB8 · committed" : "尚未提交";
  $("hdr-image").src = detail.hdr_data_url || "";
  $("sdr-image").src = detail.sdr_data_url || "";
  const proof = detail.proof;
  $("record-hash").textContent = proof ? proof.record_sha256 : "未提交";
  $("record-hash").title = proof ? proof.record_sha256 : "";
  $("frame-facts").textContent = proof
    ? `segment ${detail.segment} · peak ${fmt(proof.output_stats.peak_nits, 2)} nits · mean RGB ${proof.output_stats.mean_linear.map(v => fmt(v, 4)).join(" / ")} · pixel ${proof.output_stats.pixel_sha256.slice(0, 16)}…`
    : `segment ${detail.segment} · timestamp ${detail.timestamp_ms} ms · 等待处理`;
  renderPatches(proof);
  $("proof-json").textContent = proof ? JSON.stringify(proof, null, 2) : "—";
}

function renderPatches(proof) {
  const host = $("patch-table");
  if (!proof || !proof.patches) {
    host.innerHTML = '<p class="muted">没有已提交的自证记录。</p>';
    return;
  }
  host.innerHTML = `
    <div class="patch"><span class="name">patch</span><span>ΔR</span><span>ΔG</span><span>ΔB</span></div>
    ${proof.patches.map(p => `
      <div class="patch">
        <span class="name">${p.name}</span>
        ${p.delta_rgb.map(d => `<span class="${Math.abs(d) <= 0.02 ? "ok" : "delta"}">${fmt(d, 4)}</span>`).join("")}
      </div>`).join("")}
    <p class="muted" style="margin:8px 0 0">真值容差 0.02 linear；偏差来自可控量化与色域边界处理。</p>`;
}

function showResult(value, isError = false) {
  $("operation-result").textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2);
  $("operation-result").style.color = isError ? "#ffb1bb" : "";
}

async function run(mode) {
  if (state.busy) return;
  state.busy = true;
  showResult(`正在执行 ${mode} …`);
  try {
    const response = await call.Run({
      mode,
      params: params(),
      max_workers: Number($("workers").value),
      buffer: Number($("buffer").value),
      cancel_after: 0,
      injection_path: "",
      auto_retry: true,
    });
    applyWorkspace(response.workspace);
    if (response.error) showResult(response.error, true);
    else showResult(response.result, false);
    await refreshEvents();
    if (state.selectedRoute) await loadFrameDetail();
  } catch (err) {
    showResult(String(err), true);
    toast(String(err), true);
  } finally {
    state.busy = false;
  }
}

async function refreshEvents() {
  try {
    const events = await call.Events();
    const host = $("event-log");
    host.innerHTML = "";
    for (const ev of events.slice(-220).reverse()) {
      const row = document.createElement("div");
      const cls = ev.kind.includes("buffer") ? "blocked" : ev.kind.includes("failed") ? "failed" : "";
      row.className = "event " + cls;
      row.innerHTML = `<span>${(ev.time_utc || "").slice(11, 19)}</span><span class="kind">${ev.kind}</span><span>${[ev.route_id, ev.segment >= 0 ? "s" + ev.segment : "", ev.frame >= 0 ? "f" + ev.frame : "", ev.reason, ev.message].filter(Boolean).join(" · ")}</span>`;
      host.appendChild(row);
    }
  } catch (_) {}
}

async function audit() {
  try {
    const result = await call.Audit();
    showResult(result, !!result.error);
    toast(result.error ? "审计未通过" : "独立审计通过", !!result.error);
  } catch (err) { showResult(String(err), true); }
}

async function compare() {
  try {
    const result = await call.Compare({ left: "", right: $("compare-right").value });
    showResult(result, !!result.error);
    toast(result.error ? "发现差异" : "快照一致", !!result.error);
  } catch (err) { showResult(String(err), true); }
}

async function exportFrames() {
  try {
    const summary = await call.Export("");
    showResult(summary, false);
    toast(`已导出 ${summary.frames} 帧`);
  } catch (err) { showResult(String(err), true); }
}

async function retrySegment() {
  const route = selectedRouteView();
  if (!route) return toast("先选择一路", true);
  const candidate = route.segments.find(s => s.index === state.selectedSegment) ||
    route.segments.find(s => s.phase !== "committed") || route.segments[0];
  try {
    const response = await call.RetrySegment({ route_id: route.route.id, segment: candidate.index });
    applyWorkspace(response.workspace);
    showResult(response.error || `已处理 ${route.route.id} segment ${candidate.index}`, !!response.error);
    await refreshEvents();
  } catch (err) { showResult(String(err), true); }
}

function bind() {
  $("btn-open").onclick = async () => {
    try {
      const ws = await call.Open({ root: $("root-path").value, samples_dir: $("samples-path").value, db_path: $("db-path").value });
      applyWorkspace(ws);
      toast("工作区已打开");
    } catch (err) { toast(String(err), true); }
  };
  $("btn-generate").onclick = async () => {
    try {
      const ws = await call.Generate({ root: $("root-path").value || state.workspace?.root, seed: 42 });
      applyWorkspace(ws);
      state.selectedRoute = ws.routes?.[0]?.route.id || null;
      renderRoutes(ws.routes || []);
      if (state.selectedRoute) await loadFrameDetail();
      toast("确定性样本已生成");
    } catch (err) { toast(String(err), true); }
  };
  $("btn-realtime").onclick = () => run("realtime");
  $("btn-resume").onclick = () => run("resume");
  $("btn-full").onclick = () => run("full");
  $("btn-cancel").onclick = () => call.Cancel();
  $("btn-prev").onclick = () => { state.frame = Math.max(0, state.frame - 1); loadFrameDetail(); };
  $("btn-next").onclick = () => { const max = (selectedRouteView()?.route.frame_count || 1) - 1; state.frame = Math.min(max, state.frame + 1); loadFrameDetail(); };
  $("btn-audit").onclick = audit;
  $("btn-compare").onclick = compare;
  $("btn-export").onclick = exportFrames;
  $("btn-retry").onclick = retrySegment;
  $("compare-right").value = "";
  document.addEventListener("keydown", (e) => {
    if (e.key === "ArrowLeft") $("btn-prev").click();
    if (e.key === "ArrowRight") $("btn-next").click();
  });
}

async function boot() {
  bind();
  try {
    const ws = await call.Workspace();
    applyWorkspace(ws);
    if (ws.routes?.length) {
      state.selectedRoute = ws.routes[0].route.id;
      renderRoutes(ws.routes);
      await loadFrameDetail();
    }
    await refreshEvents();
  } catch (err) {
    toast("Wails 绑定尚未就绪: " + err, true);
  }
}

window.addEventListener("DOMContentLoaded", boot);
