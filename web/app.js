const state = { projects: [], project: null, graph: null, result: null, selectedNodeId: null, lastReport: null };

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  const text = await response.text();
  const data = text ? JSON.parse(text) : null;
  if (!response.ok) throw data || new Error(text);
  return data;
}

function showReport(value) {
  document.getElementById('report').textContent = typeof value === 'string' ? value : JSON.stringify(value, null, 2);
}

async function loadProjects() {
  state.projects = await api('/api/projects');
  const list = document.getElementById('projectList');
  list.innerHTML = '';
  state.projects.forEach((project) => {
    const button = document.createElement('button');
    button.className = 'project-item';
    button.textContent = `${project.id} · seed ${project.seed}`;
    button.onclick = () => selectProject(project.id);
    list.appendChild(button);
  });
  if (!state.project && state.projects[0]) await selectProject(state.projects[0].id);
  await loadSessions();
}

async function selectProject(id) {
  state.project = await api(`/api/projects/${id}`);
  state.graph = await api(`/api/projects/${id}/graph`);
  state.selectedNodeId = state.project.nodes[0]?.id;
  document.getElementById('projectMeta').textContent = `${state.project.schema} · ${state.project.nodes.length} nodes`;
  renderGraph();
  renderTimeline();
  renderNodeDetail();
  await runProject(false);
}

function renderGraph() {
  const graph = document.getElementById('graph');
  graph.innerHTML = '';
  const canvas = document.createElement('div');
  canvas.className = 'graph-canvas';
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('width', '1450');
  svg.setAttribute('height', '430');
  const arrow = document.createElementNS('http://www.w3.org/2000/svg', 'marker');
  arrow.setAttribute('id', 'arrow');
  arrow.setAttribute('viewBox', '0 0 10 10');
  arrow.setAttribute('refX', '9');
  arrow.setAttribute('refY', '5');
  arrow.setAttribute('markerWidth', '7');
  arrow.setAttribute('markerHeight', '7');
  arrow.setAttribute('orient', 'auto-start-reverse');
  const arrowPath = document.createElementNS('http://www.w3.org/2000/svg', 'path');
  arrowPath.setAttribute('d', 'M 0 0 L 10 5 L 0 10 z');
  arrowPath.setAttribute('fill', '#5f83c4');
  arrow.appendChild(arrowPath);
  svg.appendChild(arrow);

  const byId = new Map(state.project.nodes.map((node) => [node.id, node]));
  state.graph.edges
    .filter((edge) => edge.type === 'causal' || edge.type === 'mutex')
    .forEach((edge) => {
      const source = byId.get(edge.from);
      const target = byId.get(edge.to);
      if (!source?.layout || !target?.layout) return;
      const x1 = source.layout.x + 158;
      const y1 = source.layout.y + 48;
      const x2 = target.layout.x - 4;
      const y2 = target.layout.y + 48;
      const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
      path.setAttribute('d', `M ${x1} ${y1} C ${x1 + 55} ${y1}, ${x2 - 55} ${y2}, ${x2} ${y2}`);
      path.setAttribute('class', `edge ${edge.type}`);
      path.setAttribute('marker-end', 'url(#arrow)');
      svg.appendChild(path);
    });
  canvas.appendChild(svg);

  state.project.nodes.forEach((node) => {
    const card = document.createElement('div');
    card.className = 'node-card';
    card.dataset.node = node.id;
    if (state.lastReport?.plan?.replay_nodes?.includes(node.id)) card.classList.add('dirty');
    const parents = (node.parents || []).join(', ') || '起点';
    const join = node.convergence ? ` · 汇聚:${node.convergence.policy}` : '';
    card.innerHTML = `<strong>${node.id}</strong><span>${node.title || ''}</span><small>上游：${parents}${join}</small><small>镜头：${node.shots.length}</small>`;
    if (node.layout) {
      card.style.left = `${node.layout.x}px`;
      card.style.top = `${node.layout.y}px`;
    }
    card.onclick = () => { state.selectedNodeId = node.id; renderNodeDetail(); };
    canvas.appendChild(card);
  });
  graph.appendChild(canvas);
}

function renderTimeline() {
  const timeline = document.getElementById('choiceTimeline');
  timeline.innerHTML = '';
  (state.project.choice_sequence?.items || []).forEach((choice, index) => {
    const chip = document.createElement('div');
    chip.className = 'choice-chip';
    chip.textContent = `${index + 1}. ${choice.node_id} → ${choice.option_id}`;
    timeline.appendChild(chip);
  });
}

function selectedNode() {
  return state.project.nodes.find((node) => node.id === state.selectedNodeId);
}

function renderNodeDetail() {
  const node = selectedNode();
  const detail = document.getElementById('nodeDetail');
  if (!node) return;
  detail.innerHTML = '';
  const title = document.createElement('h3');
  title.textContent = `${node.id} · ${node.title || ''}`;
  detail.appendChild(title);

  const pre = document.createElement('textarea');
  pre.rows = 4;
  pre.value = JSON.stringify(node.preconditions || { all: [], any: [], none: [] }, null, 2);
  detail.appendChild(pre);
  const savePre = document.createElement('button');
  savePre.textContent = '编辑前置条件并重演';
  savePre.onclick = () => editNode({ node_id: node.id, kind: 'preconditions', condition: JSON.parse(pre.value) });
  detail.appendChild(savePre);

  node.shots.forEach((shot, index) => {
    const row = document.createElement('div');
    row.className = 'shot-edit';
    row.innerHTML = `<label>${shot.id}<input type="number" min="0" value="${shot.duration_ms}" /></label>`;
    const input = row.querySelector('input');
    const button = document.createElement('button');
    button.textContent = '改时长';
    button.onclick = () => editNode({ node_id: node.id, kind: 'duration_ms', local_index: index, duration_ms: Number(input.value) });
    row.appendChild(button);
    detail.appendChild(row);
  });
}

function renderPreview() {
  const preview = document.getElementById('preview');
  preview.innerHTML = '';
  if (!state.result) return;
  state.result.shots.forEach((shot) => {
    const card = document.createElement('article');
    card.className = 'shot-card';
    if (state.lastReport?.plan?.invalid_shot_index?.[shot.global_index] || state.lastReport?.plan?.InvalidShotIndex?.[shot.global_index]) {
      card.classList.add('invalid');
    }
    const samples = shot.samples.map((sample) => sample.local_ms).join(', ');
    card.innerHTML = `<header>#${shot.global_index} ${shot.node_id}</header><strong>${shot.shot_id}</strong><div>${shot.aspect_path} · ${shot.duration_ms}ms</div><small>${shot.camera || ''}</small><p>采样：${samples}</p>`;
    preview.appendChild(card);
  });
}

async function runProject(show = true) {
  if (!state.project) return;
  state.result = await api(`/api/projects/${state.project.id}/run`, { method: 'POST' });
  renderPreview();
  if (show) showReport(state.result);
}

async function editNode(body) {
  try {
    state.lastReport = await api(`/api/projects/${state.project.id}/edit`, { method: 'POST', body: JSON.stringify(body) });
    state.result = state.lastReport.result;
    showReport({
      invalidations: state.lastReport.plan.invalidations,
      replay_order: state.lastReport.replay_order,
      cache_hits: state.lastReport.cache_hits,
      cache_misses: state.lastReport.cache_misses,
      comparison: state.lastReport.comparison,
    });
    renderGraph();
    renderPreview();
    await selectProjectSilent();
  } catch (error) {
    showReport(error);
  }
}

async function selectProjectSilent() {
  const id = state.project.id;
  state.project = await api(`/api/projects/${id}`);
  state.graph = await api(`/api/projects/${id}/graph`);
  renderGraph();
  renderNodeDetail();
}

async function loadSessions() {
  const sessions = await api('/api/sessions');
  const list = document.getElementById('sessionList');
  list.innerHTML = '';
  sessions.forEach((session) => {
    const row = document.createElement('div');
    row.className = 'session-row';
    const item = document.createElement('button');
    item.className = 'session-item';
    item.textContent = `${session.id} · ${session.status} · r${session.revision}`;
    item.onclick = async () => showReport(await api(`/api/sessions/${session.id}/checkpoints`));
    const resume = document.createElement('button');
    resume.className = 'session-resume';
    resume.textContent = '续演';
    resume.onclick = () => resumeSession(session.id);
    row.append(item, resume);
    list.appendChild(row);
  });
}

async function resumeSession(sessionId) {
  try {
    const response = await api(`/api/projects/${state.project.id}/resume`, {
      method: 'POST',
      body: JSON.stringify({ session_id: sessionId, interrupt_at: -1 }),
    });
    state.result = response.result;
    renderPreview();
    showReport(response);
    await loadSessions();
  } catch (error) {
    showReport(error);
  }
}

document.getElementById('runBtn').onclick = () => runProject(true);
document.getElementById('verifyBtn').onclick = async () => {
  showReport({ status: 'running deterministic verification…' });
  try {
    showReport(await api(`/api/projects/${state.project.id}/verify`, { method: 'POST' }));
  } catch (error) { showReport(error); }
};
document.getElementById('checkpointBtn').onclick = async () => {
  const total = state.result?.shots?.length || 1;
  const interruptAt = Math.max(0, Math.floor(total / 2) - 1);
  const response = await api(`/api/projects/${state.project.id}/checkpoint`, {
    method: 'POST', body: JSON.stringify({ session_id: 'ui-resume', interrupt_at: interruptAt }),
  });
  state.result = response.result;
  showReport(response);
  loadSessions();
};
document.getElementById('resumeBtn').onclick = async () => {
  await resumeSession('ui-resume');
};
document.getElementById('exportBtn').onclick = async () => {
  const node = selectedNode();
  const version = document.getElementById('exportVersion').value;
  const data = await api(`/api/projects/${state.project.id}/export?node_id=${encodeURIComponent(node.id)}&version=${version}`);
  showReport(data);
};

api('/api/health').then((health) => {
  document.getElementById('health').textContent = `本地核心就绪 · ${health.algorithm_version}`;
  return loadProjects();
}).catch((error) => {
  document.getElementById('health').textContent = `启动失败：${error.message}`;
});
