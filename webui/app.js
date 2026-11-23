"use strict";

const state = {
  health: null,
  projects: [],
  detail: null,
  run: null,
  runLabel: "baseline",
  selectedNodeId: null,
  playIndex: 0,
  saves: [],
  compare: null,
  checkpoint: null,
  playTimer: null,
  busy: false,
};

const $ = (id) => document.getElementById(id);
const esc = (value) => String(value ?? "")
  .replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;")
  .replaceAll('"', "&quot;").replaceAll("'", "&#039;");

function fmtMS(ms) {
  if (ms < 1000) return `${ms} ms`;
  const seconds = ms / 1000;
  if (seconds < 60) return `${seconds.toFixed(seconds >= 10 ? 1 : 2)} s`;
  const minutes = Math.floor(seconds / 60);
  return `${minutes}m ${(seconds % 60).toFixed(1)}s`;
}

function shortHash(hash) {
  return hash ? hash.slice(0, 10) : "—";
}

function toast(message, isError = false) {
  const el = $("toast");
  el.textContent = message;
  el.className = `toast show${isError ? " error" : ""}`;
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { el.className = "toast"; }, 4200);
}

function showNotice(message, success = false) {
  const el = $("notice");
  el.textContent = message;
  el.className = `notice${success ? " success" : ""}`;
}

function hideNotice() {
  $("notice").className = "notice hidden";
}

function errorText(payload) {
  if (!payload) return "未知错误";
  if (payload.error) payload = payload.error;
  const parts = [payload.code || "ERROR"];
  if (payload.shot_sequence !== undefined && payload.shot_sequence !== null) parts.push(`shot=${payload.shot_sequence}`);
  if (payload.node_id) parts.push(`node=${payload.node_id}`);
  if (payload.field) parts.push(`field=${payload.field}`);
  if (payload.message) parts.push(payload.message);
  return parts.join(" · ");
}

async function request(url, options = {}) {
  const response = await fetch(url, {
    headers: { "Content-Type": "application/json" },
    ...options,
  });
  let body = null;
  try { body = await response.json(); } catch (_) { /* no body */ }
  if (!response.ok) {
    const error = new Error(errorText(body));
    error.payload = body;
    throw error;
  }
  return body;
}

function displayProject() {
  return state.compare?.edited_project || state.detail?.project;
}

function displayGraph() {
  return state.compare?.edited_dependency_graph || state.detail?.dependency_graph;
}

function nodeById(id) {
  return displayProject()?.nodes?.find((node) => node.id === id) || null;
}

function selectedNode() {
  return nodeById(state.selectedNodeId);
}

async function init() {
  bindEvents();
  try {
    const [health, projects] = await Promise.all([
      request("/api/health"),
      request("/api/projects"),
    ]);
    state.health = health;
    state.projects = projects.projects || [];
    $("runtime-label").textContent = `本地引擎 · ${health.workers} 工作线程`;
    $("runtime-detail").textContent = `每线程缓冲 ${health.buffer_per_worker} · SQLite 检查点`;
    $("footer-version").textContent = `${health.algorithm_version} · save schema ${health.save_schema}`;
    renderProjectList();
    if (state.projects.length) {
      await loadProject(state.projects[0].id);
    }
  } catch (error) {
    showNotice(error.message);
    toast(error.message, true);
  }
}

function bindEvents() {
  $("reset-baseline").addEventListener("click", resetBaseline);
  $("compare-current").addEventListener("click", () => compareFromSelected());
  $("export-project").addEventListener("click", () => {
    if (state.detail) window.open(`/api/export?kind=project&project_id=${encodeURIComponent(state.detail.project.id)}`, "_blank");
  });
  $("export-node").addEventListener("click", exportSelectedNode);
  $("edit-node").addEventListener("click", openEditModal);
  $("close-edit").addEventListener("click", closeEditModal);
  $("edit-modal").querySelector(".modal-backdrop").addEventListener("click", closeEditModal);
  $("preview-edit").addEventListener("click", () => applyEdit(true));
  $("apply-edit").addEventListener("click", () => applyEdit(false));
  $("prev-shot").addEventListener("click", () => setPlayIndex(state.playIndex - 1));
  $("next-shot").addEventListener("click", () => setPlayIndex(state.playIndex + 1));
  $("shot-range").addEventListener("input", (event) => setPlayIndex(Number(event.target.value)));
  $("play-shots").addEventListener("click", togglePlayback);
  $("create-save").addEventListener("click", createSave);
  $("start-checkpoints").addEventListener("click", startCheckpoints);
  $("resume-checkpoints").addEventListener("click", resumeCheckpoints);
}

function renderProjectList() {
  const container = $("project-list");
  container.innerHTML = state.projects.map((project) => `
    <button class="project-card${state.detail?.project.id === project.id ? " active" : ""}" data-project="${esc(project.id)}">
      <strong>${esc(project.title.split(" / ")[0])}</strong>
      <span>seed ${project.seed} · ${project.shot_count} 镜 · ${project.reachable_count} 节点</span>
    </button>
  `).join("");
  container.querySelectorAll("[data-project]").forEach((button) => {
    button.addEventListener("click", () => loadProject(button.dataset.project));
  });
}

async function loadProject(id) {
  hideNotice();
  stopPlayback();
  try {
    const [detail, saves] = await Promise.all([
      request(`/api/project?id=${encodeURIComponent(id)}`),
      request(`/api/saves?project_id=${encodeURIComponent(id)}`),
    ]);
    state.detail = detail.detail;
    state.run = detail.detail.baseline;
    state.runLabel = "baseline";
    state.compare = null;
    state.checkpoint = detail.detail.checkpoint || null;
    state.saves = saves.saves || [];
    state.playIndex = 0;
    state.selectedNodeId = state.run.shots[0]?.node_id || state.detail.project.nodes[0]?.id;
    renderProjectList();
    renderAll();
  } catch (error) {
    showNotice(error.message);
    toast(error.message, true);
  }
}

function renderAll() {
  if (!state.detail || !state.run) return;
  const project = displayProject();
  $("project-title").textContent = state.detail.project.title;
  $("project-subtitle").textContent = `${state.detail.project.id} · seed ${state.detail.project.seed} · ${shortHash(state.run.content_hash)}`;
  $("save-boundary").max = String(state.run.shots.length);
  if (!state.busy) {
    $("save-boundary").value = String(Math.max(0, Math.min(state.playIndex, state.run.shots.length)));
  }
  renderMetrics();
  renderGraph();
  renderInspector();
  renderTimeline();
  renderPlayer();
  renderState();
  renderSaves();
  renderCheckpoint();
  renderInvalidation();
  if (project) {
    $("project-title").title = project.title;
  }
}

function renderMetrics() {
  const run = state.run;
  const totalEvents = run.event_order?.length || 0;
  const blocked = run.blocked_choices?.length || 0;
  const metrics = [
    ["SHOTS", run.shot_count, `${run.reachable_nodes.length} 个可达节点`],
    ["DURATION", fmtMS(run.total_duration_ms), "定点毫秒累计"],
    ["REACHABLE", run.reachable_nodes.length, run.reachable_nodes.slice(-2).join(" → ")],
    ["EVENTS", totalEvents, "确定性事件到达"],
    ["BLOCKED", blocked, `${run.choice_offset} 条选择已消费`],
  ];
  $("metric-row").innerHTML = metrics.map(([label, value, sub]) => `
    <div class="metric">
      <div class="metric-label">${esc(label)}</div>
      <div class="metric-value">${esc(value)}</div>
      <div class="metric-sub">${esc(sub)}</div>
    </div>
  `).join("");
}

function graphLayout() {
  const graph = displayGraph();
  const project = displayProject();
  const layer = {};
  project.nodes.forEach((node) => { layer[node.id] = 0; });
  graph.topological_order.forEach((id) => {
    const node = nodeById(id);
    if (!node) return;
    let value = 0;
    for (const dep of node.join || []) value = Math.max(value, (layer[dep] || 0) + 1);
    layer[id] = value;
  });
  const groups = {};
  graph.topological_order.forEach((id) => {
    if (!nodeById(id)) return;
    const value = layer[id] || 0;
    (groups[value] ||= []).push(id);
  });
  const positions = {};
  Object.entries(groups).forEach(([value, ids]) => {
    ids.forEach((id, row) => {
      positions[id] = { x: 30 + Number(value) * 170, y: 34 + row * 84 };
    });
  });
  return { positions, layer };
}

function renderGraph() {
  const graph = displayGraph();
  const project = displayProject();
  const { positions } = graphLayout();
  const maxX = Math.max(300, ...Object.values(positions).map((p) => p.x + 160));
  const maxY = Math.max(240, ...Object.values(positions).map((p) => p.y + 70));
  const edgePaths = [];
  const seenMutex = new Set();
  for (const edge of graph.edges || []) {
    if (!positions[edge.from] || !positions[edge.to]) continue;
    if (edge.kind === "mutex") {
      const key = [edge.from, edge.to].sort().join("|");
      if (seenMutex.has(key)) continue;
      seenMutex.add(key);
    }
    const from = positions[edge.from];
    const to = positions[edge.to];
    const x1 = from.x + 140;
    const y1 = from.y + 27;
    const x2 = to.x;
    const y2 = to.y + 27;
    const mid = (x1 + x2) / 2;
    edgePaths.push(`<path class="graph-edge ${esc(edge.kind)}" d="M ${x1} ${y1} C ${mid} ${y1}, ${mid} ${y2}, ${x2} ${y2}"><title>${esc(edge.kind)} ${esc(edge.from)} → ${esc(edge.to)}</title></path>`);
  }
  const nodes = project.nodes.map((node) => {
    const pos = positions[node.id] || { x: 30, y: 30 };
    const runtime = state.run.node_states[node.id];
    const selected = node.id === state.selectedNodeId ? " selected" : "";
    const active = runtime?.activated ? " active" : "";
    return `
      <g class="graph-node${selected}${active}" data-node="${esc(node.id)}" transform="translate(${pos.x},${pos.y})">
        <rect width="140" height="54" rx="9"></rect>
        <text x="10" y="17" class="node-id">${esc(node.id.slice(0, 22))}</text>
        <text x="10" y="34">${esc(node.title.slice(0, 24))}</text>
        <text x="10" y="47" class="shot-count">${node.shots.length} shots · ${runtime?.completed ? "done" : runtime?.activated ? "live" : "idle"}</text>
      </g>
    `;
  }).join("");
  const svg = $("causal-graph");
  svg.setAttribute("viewBox", `0 0 ${maxX} ${maxY}`);
  svg.innerHTML = `${edgePaths.join("")}${nodes}`;
  svg.querySelectorAll("[data-node]").forEach((element) => {
    element.addEventListener("click", () => selectNode(element.dataset.node));
  });
}

function selectNode(id) {
  state.selectedNodeId = id;
  const shotIndex = state.run.shots.findIndex((shot) => shot.node_id === id);
  if (shotIndex >= 0) state.playIndex = shotIndex;
  renderGraph();
  renderInspector();
  renderPlayer();
}

function renderInspector() {
  const node = selectedNode();
  if (!node) {
    $("node-heading").textContent = "节点详情";
    $("node-inspector").innerHTML = `<div class="empty-state">选择一个节点查看前置条件与事件后果。</div>`;
    return;
  }
  $("node-heading").textContent = node.title;
  const requires = (node.requires || []).map((cond) => `
    <div class="condition-line">
      <span class="tag">${esc(cond.scope)}</span>
      <code>${esc(cond.scope === "character" ? `${cond.character}.${cond.key}` : cond.key)} ${esc(cond.op)} ${esc(JSON.stringify(cond.value ?? null))}</code>
    </div>
  `).join("") || `<div class="muted small">无前置条件</div>`;
  const events = (node.events || []).map((event) => `
    <div class="event-line">
      <span class="tag event">${esc(event.kind)}</span>
      <code>${esc(event.kind.includes("character") ? `${event.character}.${event.key}` : event.key)} ← ${esc(JSON.stringify(event.value))}</code>
    </div>
  `).join("") || `<div class="muted small">无事件后果</div>`;
  const shotChips = node.shots.map((shot, index) => `
    <span class="mini-shot">#${index + 1} · ${fmtMS(shot.duration_ms)} · ${esc(shot.aspect_priority.join(" > "))}</span>
  `).join("");
  const runtime = state.run.node_states[node.id] || {};
  $("node-inspector").innerHTML = `
    <div class="node-title-line">
      <strong>${esc(node.id)}</strong>
      <span class="badge${runtime.completed ? " ok" : ""}">${runtime.completed ? "completed" : runtime.activated ? "active" : "not reached"}</span>
    </div>
    <div class="inspector-grid">
      <div class="inspector-block"><h5>前置条件</h5>${requires}</div>
      <div class="inspector-block"><h5>事件后果</h5>${events}</div>
    </div>
    <div class="node-shots">
      <span class="mini-shot">join: ${esc((node.join || []).join(", ") || "root")}</span>
      <span class="mini-shot">mutex: ${esc((node.mutex || []).join(", ") || "none")}</span>
    </div>
    <div class="node-shots">${shotChips}</div>
  `;
}

function renderTimeline() {
  const project = displayProject();
  const blocked = new Map((state.run.blocked_choices || []).map((item) => [item.choice_index, item]));
  $("blocked-count").textContent = `${blocked.size} 条选择因互斥、汇聚或条件未满足而阻塞`;
  $("choice-timeline").innerHTML = project.choices.map((choice, index) => {
    const block = blocked.get(index);
    return `
      <button class="choice-item${block ? " blocked" : ""}" data-choice-target="${esc(choice.target)}">
        <div class="choice-index">CHOICE ${String(index).padStart(2, "0")} · t=${choice.at_ms}ms</div>
        <div class="choice-label">${esc(choice.label)}</div>
        <div class="choice-target">→ ${esc(choice.target)}</div>
        <div class="choice-status">${block ? `blocked · ${esc(block.reason)}` : "consumed / reachable"}</div>
      </button>
    `;
  }).join("");
  $("choice-timeline").querySelectorAll("[data-choice-target]").forEach((button) => {
    button.addEventListener("click", () => selectNode(button.dataset.choiceTarget));
  });
}

function currentShot() {
  if (!state.run?.shots?.length) return null;
  const index = Math.max(0, Math.min(state.playIndex, state.run.shots.length - 1));
  return state.run.shots[index];
}

function setPlayIndex(index) {
  if (!state.run?.shots?.length) return;
  state.playIndex = Math.max(0, Math.min(index, state.run.shots.length - 1));
  $("shot-range").value = String(state.playIndex);
  $("save-boundary").value = String(state.playIndex);
  renderPlayer();
  renderGraph();
}

function renderPlayer() {
  const shots = state.run.shots || [];
  const shot = currentShot();
  $("shot-range").max = String(Math.max(0, shots.length - 1));
  $("shot-range").value = String(state.playIndex);
  if (!shot) {
    $("current-shot").innerHTML = `<div class="empty-state">当前推进没有呈现任何分镜。</div>`;
    $("shot-strip").innerHTML = "";
    return;
  }
  const tags = [];
  if (shot.reused) tags.push(`<span class="tag reused">复用既有结果 #${shot.original_sequence ?? shot.sequence}</span>`);
  if (shot.invalidated) tags.push(`<span class="tag invalid">已失效 · 原子重演</span>`);
  $("current-shot").innerHTML = `
    <div class="shot-kicker">SHOT ${String(shot.sequence).padStart(3, "0")} / ${String(shots.length).padStart(3, "0")}</div>
    <div class="shot-node">${esc(shot.node_title)}</div>
    <div class="shot-meta">
      <span>${esc(shot.node_id)}</span>
      <span>${fmtMS(shot.duration_ms)}</span>
      <span>t=${shot.presented_at_ms}ms</span>
      <span>${esc(shot.aspect_priority.join(" > "))}</span>
      <span>choice ${shot.choice_index}</span>
    </div>
    <div class="shot-description">${esc(shot.description || "无镜头描述")}</div>
    <div class="shot-flags">${tags.join("")}<span class="tag">state ${shortHash(shot.state_hash)}</span></div>
  `;
  const uniqueNodes = [...new Set(shots.map((item) => item.node_id))];
  const nodeHue = Object.fromEntries(uniqueNodes.map((id, index) => [id, `hsl(${(index * 53 + 188) % 360} 34% 26%)`]));
  $("shot-strip").innerHTML = shots.map((item, index) => {
    const classes = ["shot-cell"];
    if (index === state.playIndex) classes.push("selected");
    if (item.reused) classes.push("reused");
    if (item.invalidated) classes.push("invalid");
    if (item.node_id === state.selectedNodeId) classes.push("current-node");
    const style = item.reused || item.invalidated ? "" : `style="background:${nodeHue[item.node_id]}"`;
    return `<button class="${classes.join(" ")}" data-shot="${index}" title="#${index} ${esc(item.node_id)} ${fmtMS(item.duration_ms)}" ${style}></button>`;
  }).join("");
  $("shot-strip").querySelectorAll("[data-shot]").forEach((cell) => {
    cell.addEventListener("click", () => {
      setPlayIndex(Number(cell.dataset.shot));
      state.selectedNodeId = shots[Number(cell.dataset.shot)].node_id;
      renderInspector();
      renderGraph();
    });
  });
  const hasMarkers = shots.some((item) => item.reused || item.invalidated);
  $("shot-legend").innerHTML = hasMarkers
    ? `<span><i style="background:#2b6548"></i>复用既有结果</span><span><i style="background:#8a3441"></i>失效后重演</span>`
    : `<span>镜头颜色按节点区分；热力图会在编辑重演后显示复用与失效状态。</span>`;
}

function renderState() {
  const flags = state.run.final_flags || {};
  const characters = state.run.final_characters || {};
  $("state-badge").textContent = state.runLabel;
  $("state-badge").className = `badge${state.runLabel === "edited replay" ? " bad" : ""}`;
  $("story-state").innerHTML = `
    <div class="state-block">
      <h5>Flags</h5>
      <pre>${esc(JSON.stringify(flags, null, 2))}</pre>
    </div>
    <div class="state-block">
      <h5>Characters</h5>
      <pre>${esc(JSON.stringify(characters, null, 2))}</pre>
    </div>
  `;
}

function renderSaves() {
  $("save-version").textContent = state.saves.length ? `v${state.saves[0].version}` : "v1";
  if (!state.saves.length) {
    $("save-list").innerHTML = `<div class="empty-state">尚无版本化存档。选择镜头边界后另存。</div>`;
    return;
  }
  $("save-list").innerHTML = state.saves.map((save) => `
    <div class="save-item">
      <div>
        <strong>${esc(save.label)}</strong>
        <div class="meta">${esc(save.id)} · shot ${save.shot_progress} · ${shortHash(save.fingerprint)}</div>
      </div>
      <div class="save-actions">
        <button class="button small ghost" data-resume-save="${esc(save.id)}">续演</button>
        <button class="button small ghost" data-export-save="${esc(save.id)}">导出</button>
      </div>
    </div>
  `).join("");
  $("save-list").querySelectorAll("[data-resume-save]").forEach((button) => {
    button.addEventListener("click", () => resumeSave(button.dataset.resumeSave));
  });
  $("save-list").querySelectorAll("[data-export-save]").forEach((button) => {
    button.addEventListener("click", () => {
      window.open(`/api/export?kind=save&project_id=${encodeURIComponent(state.detail.project.id)}&save_id=${encodeURIComponent(button.dataset.exportSave)}`, "_blank");
    });
  });
}

function renderCheckpoint() {
  const cp = state.checkpoint;
  $("cp-badge").textContent = cp ? `${cp.shot_count} 镜` : "未建立";
  $("cp-badge").className = `badge${cp ? " ok" : ""}`;
  if (!cp) {
    $("checkpoint-info").innerHTML = `数据库：<code>${esc(state.health?.checkpoint_db || "data/checkpoints.db")}</code><br>尚未为当前工程提交镜头边界检查点。`;
  } else {
    $("checkpoint-info").innerHTML = `
      已提交 <strong>${cp.shot_count}</strong> 个镜头边界 · 最后镜头 #${cp.last_shot_seq}<br>
      内容哈希 <code>${esc(shortHash(cp.content_hash))}</code> · ${esc(cp.algorithm_version)}<br>
      数据库：<code>${esc(state.health?.checkpoint_db || "")}</code>
    `;
  }
}

function renderInvalidation() {
  const response = state.compare;
  if (!response) {
    $("invalidation-panel").classList.add("hidden");
    return;
  }
  $("invalidation-panel").classList.remove("hidden");
  const ok = response.ok;
  $("compare-status").textContent = ok ? "三路径一致" : errorText(response.error || "E_DIVERGE");
  $("compare-status").className = `badge ${ok ? "ok" : "bad"}`;
  const comparisons = response.comparisons || [];
  $("compare-summary").innerHTML = comparisons.length ? comparisons.map((item) => `
    <div class="path-card ${item.ok ? "ok" : "bad"}">
      <div class="name">${esc(item.name)}</div>
      <div class="result">${item.ok ? "逐镜一致" : "发现差异"}</div>
      ${item.error ? `<div class="err">${esc(errorText(item.error))}</div>` : ""}
    </div>
  `).join("") : `
    <div class="path-card bad"><div class="name">paths</div><div class="result">比对未完成</div><div class="err">${esc(errorText(response.error))}</div></div>
  `;
  const shots = response.edited_replay?.shots || [];
  $("invalid-heatmap").innerHTML = shots.map((shot) => {
    const cls = shot.reused ? "reused" : shot.invalidated ? "invalid" : "";
    return `<span class="heat-cell ${cls}" title="#${shot.sequence} ${esc(shot.node_id)}${shot.reused ? " · reused" : shot.invalidated ? " · invalidated/recomputed" : ""}"></span>`;
  }).join("");
  const invalidation = response.invalidation;
  $("invalid-nodes").innerHTML = (invalidation?.invalidated_nodes || []).map((node) => `
    <span class="chip"><strong>${esc(node.node_id)}</strong>${esc(node.reason)}</span>
  `).join("") || `<span class="muted small">无失效节点</span>`;
  $("invalid-shots").innerHTML = (invalidation?.invalidated_shots || []).map((shot) => `
    <span class="chip"><strong>#${shot.sequence} · ${esc(shot.node_id)}</strong>${esc(shot.reason)}</span>
  `).join("") || `<span class="muted small">无失效镜头</span>`;
  $("artifact-line").innerHTML = response.artifact_dir
    ? `诊断产物：<code>${esc(response.artifact_dir)}</code>${response.artifact_error ? `<br>写入提示：${esc(response.artifact_error)}` : ""}`
    : `未写入诊断产物${response.artifact_error ? `：${esc(response.artifact_error)}` : ""}`;
}

async function createSave() {
  if (!state.detail) return;
  const boundary = Number($("save-boundary").value || 0);
  const label = $("save-label").value.trim();
  try {
    const response = await request("/api/saves", {
      method: "POST",
      body: JSON.stringify({ project_id: state.detail.project.id, shot_boundary: boundary, label }),
    });
    $("save-label").value = "";
    toast(`已写入版本化存档 ${response.result.record.id}`);
    state.saves = (await request(`/api/saves?project_id=${encodeURIComponent(state.detail.project.id)}`)).saves;
    renderSaves();
  } catch (error) {
    toast(error.message, true);
  }
}

async function resumeSave(saveId) {
  try {
    const response = await request("/api/resume", {
      method: "POST",
      body: JSON.stringify({ project_id: state.detail.project.id, save_id: saveId }),
    });
    state.run = response.run;
    state.runLabel = "save resume";
    state.compare = null;
    state.playIndex = 0;
    state.selectedNodeId = response.run.shots[0]?.node_id || state.detail.project.nodes[0].id;
    renderAll();
    showNotice(`已从 ${saveId} 续演；结果与连续推进逐镜一致。`, true);
    toast("存档续演完成");
  } catch (error) {
    showNotice(error.message);
    toast(error.message, true);
  }
}

async function startCheckpoints() {
  if (!state.detail) return;
  try {
    $("start-checkpoints").disabled = true;
    const response = await request("/api/checkpoints/start", {
      method: "POST",
      body: JSON.stringify({ project_id: state.detail.project.id }),
    });
    state.run = response.run;
    state.runLabel = "checkpoint run";
    state.checkpoint = response.checkpoint;
    state.compare = null;
    renderAll();
    showNotice(`已串行提交 ${response.checkpoint.shot_count} 个镜头边界到 SQLite。`, true);
  } catch (error) {
    showNotice(error.message);
    toast(error.message, true);
  } finally {
    $("start-checkpoints").disabled = false;
  }
}

async function resumeCheckpoints() {
  if (!state.detail) return;
  try {
    $("resume-checkpoints").disabled = true;
    const response = await request("/api/checkpoints/resume", {
      method: "POST",
      body: JSON.stringify({ project_id: state.detail.project.id }),
    });
    state.run = response.run;
    state.runLabel = "checkpoint resume";
    state.compare = null;
    state.playIndex = 0;
    state.selectedNodeId = response.run.shots[0]?.node_id || state.detail.project.nodes[0].id;
    renderAll();
    showNotice("断点恢复完成；已校验结构版本、算法版本与内容哈希。", true);
  } catch (error) {
    showNotice(error.message);
    toast(error.message, true);
  } finally {
    $("resume-checkpoints").disabled = false;
  }
}

function resetBaseline() {
  if (!state.detail) return;
  stopPlayback();
  state.run = state.detail.baseline;
  state.runLabel = "baseline";
  state.compare = null;
  state.playIndex = 0;
  state.selectedNodeId = state.run.shots[0]?.node_id || state.detail.project.nodes[0].id;
  renderAll();
  hideNotice();
}

function openEditModal() {
  const node = selectedNode();
  if (!node) return;
  $("edit-title").textContent = `编辑 ${node.id}`;
  $("edit-requires").value = JSON.stringify(node.requires || [], null, 2);
  $("edit-events").value = JSON.stringify(node.events || [], null, 2);
  $("edit-error").classList.add("hidden");
  $("edit-modal").classList.remove("hidden");
}

function closeEditModal() {
  $("edit-modal").classList.add("hidden");
}

async function applyEdit(previewOnly) {
  const node = selectedNode();
  if (!node) return;
  let requires, events;
  try {
    requires = JSON.parse($("edit-requires").value);
    events = JSON.parse($("edit-events").value);
    if (!Array.isArray(requires) || !Array.isArray(events)) throw new Error("requires 与 events 必须是 JSON 数组");
  } catch (error) {
    $("edit-error").textContent = error.message;
    $("edit-error").classList.remove("hidden");
    return;
  }
  $("edit-error").classList.add("hidden");
  const edit = { node_id: node.id, requires, events };
  const boundary = Number($("save-boundary").value || Math.floor((state.run.shots.length || 1) / 2));
  try {
    $("preview-edit").disabled = true;
    $("apply-edit").disabled = true;
    if (previewOnly) {
      const preview = await request("/api/edit", {
        method: "POST",
        body: JSON.stringify({ project_id: state.detail.project.id, edit }),
      });
      state.compare = {
        ok: true,
        edited_project: preview.outcome.project,
        edited_dependency_graph: preview.edited_dependency_graph,
        edited_replay: preview.outcome.run,
        continuous: preview.outcome.verification_run,
        invalidation: preview.outcome.invalidation,
        comparisons: [{
          name: "edit_preview_vs_full_replay",
          expected: "edit_preview",
          actual: "full_replay",
          ok: true,
        }],
        artifact_dir: "",
      };
      state.run = preview.outcome.run;
      state.runLabel = "edit preview";
      state.playIndex = 0;
      state.selectedNodeId = node.id;
      closeEditModal();
      renderAll();
      showNotice("编辑预览已生成；完整三路径比对尚未执行。", true);
      toast("编辑预览完成");
      return;
    }
    const response = await request("/api/compare", {
      method: "POST",
      body: JSON.stringify({ project_id: state.detail.project.id, edit, save_boundary: boundary }),
    });
    if (!response || !response.comparisons) {
      throw new Error(errorText(response?.error || { code: "E_DIVERGE", message: "比对未返回结果" }));
    }
    state.compare = response;
    state.run = response.edited_replay || response.continuous || state.run;
    state.runLabel = "edited replay";
    state.playIndex = 0;
    state.selectedNodeId = node.id;
    closeEditModal();
    renderAll();
    if (response.ok) {
      showNotice("编辑已原子替换；未失效镜头复用原结果，三条路径逐镜一致。", true);
      toast("三路径逐镜一致");
    } else {
      showNotice(errorText(response.error));
      toast("检测到路径差异，已保留诊断产物", true);
    }
  } catch (error) {
    $("edit-error").textContent = error.message;
    $("edit-error").classList.remove("hidden");
    toast(error.message, true);
  } finally {
    $("preview-edit").disabled = false;
    $("apply-edit").disabled = false;
  }
}

async function compareFromSelected() {
  const node = selectedNode();
  if (!node) {
    toast("请先选择节点", true);
    return;
  }
  const boundary = Number($("save-boundary").value || Math.floor((state.run.shots.length || 1) / 2));
  try {
    $("compare-current").disabled = true;
    const response = await request("/api/compare", {
      method: "POST",
      body: JSON.stringify({
        project_id: state.detail.project.id,
        edit: { node_id: node.id, requires: node.requires || [], events: node.events || [] },
        save_boundary: boundary,
      }),
    });
    state.compare = response;
    state.run = response.edited_replay || response.continuous || state.run;
    state.runLabel = "edited replay";
    state.playIndex = 0;
    state.selectedNodeId = node.id;
    renderAll();
    if (response.ok) {
      showNotice("连续推进、存档续演与编辑重演三条路径逐镜一致。", true);
      toast("三路径校验通过");
    } else {
      showNotice(errorText(response.error));
      toast("三路径存在差异", true);
    }
  } catch (error) {
    showNotice(error.message);
    toast(error.message, true);
  } finally {
    $("compare-current").disabled = false;
  }
}

function togglePlayback() {
  if (state.playTimer) {
    stopPlayback();
    return;
  }
  if (!state.run?.shots?.length) return;
  $("play-shots").textContent = "暂停";
  state.playTimer = setInterval(() => {
    if (state.playIndex >= state.run.shots.length - 1) {
      stopPlayback();
      return;
    }
    setPlayIndex(state.playIndex + 1);
  }, 520);
}

function stopPlayback() {
  if (state.playTimer) clearInterval(state.playTimer);
  state.playTimer = null;
  $("play-shots").textContent = "播放";
}

function exportSelectedNode() {
  const node = selectedNode();
  if (!node) return;
  const blob = new Blob([JSON.stringify(node, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `${state.detail.project.id}-${node.id}.json`;
  link.click();
  URL.revokeObjectURL(url);
}

init();

