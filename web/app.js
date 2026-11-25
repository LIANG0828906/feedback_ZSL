const state = {
  projects: [],
  current: null,
  detail: null,
  result: null,
  plan: null,
};

const $ = (id) => document.getElementById(id);

async function api(path, options = {}) {
  const response = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...options,
  });
  const data = await response.json();
  if (!response.ok || data.ok === false && !data.result) {
    throw new Error(data.error || `请求失败 ${response.status}`);
  }
  return data;
}

function post(path, body) {
  return api(path, { method: 'POST', body: JSON.stringify(body || {}) });
}

function pngUrl(kind) {
  if (!state.current) return '';
  return `/api/png?project=${encodeURIComponent(state.current)}&kind=${kind}&t=${Date.now()}`;
}

async function loadState() {
  const data = await api('/api/state');
  state.projects = data.projects || [];
  $('seed').textContent = data.fixedSeed;
  const list = $('project-list');
  list.innerHTML = '';
  state.projects.forEach((p) => {
    const btn = document.createElement('button');
    btn.className = 'project-item' + (p.id === state.current ? ' active' : '');
    btn.innerHTML = `<strong>${p.id}</strong><span>${p.width}×${p.height} · ${p.strokes} 笔 · 域 ${p.domains}</span>`;
    btn.onclick = () => selectProject(p.id);
    list.appendChild(btn);
  });
  if (!state.current && state.projects[0]) await selectProject(state.projects[0].id);
}

async function selectProject(id) {
  state.current = id;
  state.result = null;
  state.plan = null;
  const data = await api(`/api/project?id=${encodeURIComponent(id)}`);
  state.detail = data;
  renderProject();
  await loadCheckpoints();
  await loadState();
}

function orderedStrokes() {
  const strokes = state.detail?.project?.strokes || [];
  return [...strokes].sort((a, b) =>
    (a.priority - b.priority) || (a.seq - b.seq));
}

function renderProject() {
  const p = state.detail.project;
  $('image-meta').textContent = `${p.id} · schema ${p.schemaVersion} · ${p.width}×${p.height}`;
  $('img-image').src = pngUrl('image');
  $('img-candidate').src = pngUrl('candidate');
  $('img-reference').src = pngUrl('reference');
  refreshAccumulated();

  const options = orderedStrokes().map((s) => `<option value="${s.id}">${s.id} · p${s.priority}/#${s.seq}</option>`).join('');
  $('stroke-select').innerHTML = options;
  $('component-stroke').innerHTML = options;
  $('stroke-select').onchange = fillStrokeEditor;
  fillStrokeEditor();

  $('component-tile').innerHTML = state.detail.tiles
    .map((t) => `<option value="${t.id}">${t.id} · ${t.pixels}px</option>`).join('');
  $('component-stroke').onchange = refreshComponent;
  $('component-tile').onchange = refreshComponent;
  refreshComponent();
  renderTileMap();
  renderSequence();
  renderDecisions(null);
  clearTables();
}

function fillStrokeEditor() {
  const id = $('stroke-select').value;
  const s = (state.detail.project.strokes || []).find((x) => x.id === id);
  if (!s) return;
  $('edit-sx').value = s.start.x;
  $('edit-sy').value = s.start.y;
  $('edit-ex').value = s.end.x;
  $('edit-ey').value = s.end.y;
}

function refreshAccumulated() {
  $('img-accumulated').src = pngUrl('accumulated');
  requestAnimationFrame(drawOverlay);
}

function refreshComponent() {
  if (!state.current) return;
  const s = $('component-stroke').value;
  const t = $('component-tile').value;
  $('component-img').src = `/api/component?project=${encodeURIComponent(state.current)}&stroke=${encodeURIComponent(s)}&tile=${encodeURIComponent(t)}&t=${Date.now()}`;
}

function renderTileMap() {
  const tiles = state.detail?.tiles || [];
  const cols = Math.max(1, ...tiles.map((t) => t.col + 1));
  const map = $('tile-map');
  map.style.gridTemplateColumns = `repeat(${cols}, minmax(28px, 1fr))`;
  const invalid = new Set(state.plan?.tileIds || []);
  const recomputed = new Set(state.result?.recomputedTiles || []);
  const failed = new Set((state.result?.schedulerResults || []).filter((r) => !r.ok).map((r) => r.tileId));
  map.innerHTML = '';
  tiles.forEach((t) => {
    const cell = document.createElement('div');
    let cls = 'tile-cell';
    if (failed.has(t.id)) cls += ' invalid';
    else if (recomputed.has(t.id)) cls += ' done';
    else if (invalid.has(t.id)) cls += ' invalid';
    cell.className = cls;
    cell.textContent = t.id.replace('r', '').replace('c', '/');
    cell.title = `${t.id} ${t.rect.minX},${t.rect.minY}-${t.rect.maxX},${t.rect.maxY}`;
    map.appendChild(cell);
  });
}

function renderSequence() {
  const seq = state.result?.sequence || orderedStrokes().map((s) => s.id);
  $('stroke-sequence').innerHTML = seq.map((id, i) => `<li><code>${i + 1}. ${id}</code></li>`).join('');
}

function clearTables() {
  $('topology-table').querySelector('tbody').innerHTML = '';
  $('direction-table').querySelector('tbody').innerHTML = '';
  $('decisions').innerHTML = '';
  $('summary-cards').innerHTML = '<div class="metric"><div class="label">状态</div><div class="value" style="font-size:16px">待核验</div></div>';
}

function renderResult(data) {
  state.result = data.result;
  if (data.plan) state.plan = data.plan;
  renderSummary(data);
  renderSequence();
  renderTopology();
  renderDirections();
  renderDecisions(state.result);
  renderScheduler();
  renderInvalidation();
  renderTileMap();
  refreshAccumulated();
  if (data.checkpointSequence) {
    $('checkpoint-sequence').innerHTML = `<strong>本次响应提交序列：</strong><br>${data.checkpointSequence.map((x) => `<code>${x}</code>`).join(' → ')}`;
  }
  loadCheckpoints();
}

function renderSummary(data) {
  const r = state.result || {};
  const topo = r.topology || {};
  const dir = r.directions || {};
  const sched = r.schedulerMetrics;
  const cards = [
    ['逐像素', r.alphaExact ? '一致' : '不一致', r.alphaExact ? 'ok' : 'bad'],
    ['拓扑', topo.ok ? `${topo.domains?.length || 0} 域通过` : 'E_TOP/失败', topo.ok ? 'ok' : 'bad'],
    ['方向', dir.ok ? `${dir.segments?.length || 0} 段通过` : '存在不一致', dir.ok ? 'ok' : 'bad'],
    ['提交笔序', `${(data.checkpointSequence || r.sequence || []).length}`, ''],
    ['并行/队列', sched ? `${sched.succeeded} 成功 / ${sched.failed} 失败 / ${sched.waits?.length || 0} 等待` : '未启用', sched?.failed ? 'bad' : 'ok'],
  ];
  $('summary-cards').innerHTML = cards.map(([label, value, cls]) =>
    `<div class="metric ${cls}"><div class="label">${label}</div><div class="value" style="font-size:${value.length > 8 ? 15 : 22}px">${value}</div></div>`).join('');
}

function renderTopology() {
  const rows = state.result?.topology?.domains || [];
  $('topology-table').querySelector('tbody').innerHTML = rows.map((d) => `
    <tr>
      <td>${d.domainId || d.expected?.domainId || '-'}</td>
      <td>${d.area ?? '-'}</td>
      <td>${d.centroidX != null ? `(${d.centroidX.toFixed(2)}, ${d.centroidY.toFixed(2)})` : '-'}</td>
      <td>${d.bbox ? `[${d.bbox.minX},${d.bbox.minY},${d.bbox.maxX},${d.bbox.maxY}]` : '-'}</td>
      <td>${d.holes ?? '-'} / 期望 ${d.expected?.holes ?? '-'}</td>
      <td>${d.bandWidthPixels ?? '-'}px / 期望 ${d.expected?.bandWidthPixels ?? '-'}px</td>
      <td><span class="badge ${d.pass ? 'ok' : 'bad'}">${d.pass ? '通过' : (d.issues?.[0] || '失败')}</span></td>
    </tr>`).join('');
}

function renderDirections() {
  const rows = state.result?.directions?.segments || [];
  const cls = (v) => v === 'consistent' ? 'ok' : (v.includes('missing') || v.includes('unexpected') ? 'warn' : 'bad');
  $('direction-table').querySelector('tbody').innerHTML = rows.map((d) => `
    <tr>
      <td>${d.segmentId}</td><td>${d.domainId}</td><td><code>${d.strokeId}</code></td>
      <td>${d.reference}</td><td>${d.observed}</td>
      <td><span class="badge ${cls(d.verdict)}">${d.verdict}</span></td><td>${d.pointCount}</td>
    </tr>`).join('');
}

function renderDecisions(result) {
  const decisions = result?.decisions || state.detail?.tiles?.map((t) => ({ tileId: t.id, participants: [] })) || [];
  $('decisions').innerHTML = decisions.map((d) => `
    <div class="decision">
      <h3>${d.tileId}</h3>
      <div class="muted">${d.orderVersion || ''}</div>
      <ul>${(d.participants || []).map((p) => `<li><code>${p.orderKey}</code> ${p.strokeId}<br><span class="muted">${p.reason}</span></li>`).join('') || '<li>无笔画覆盖</li>'}</ul>
    </div>`).join('');
}

function renderInvalidation() {
  const plan = state.plan;
  if (!plan) {
    $('invalidation').textContent = '编辑笔画后显示精确失效清单。';
    return;
  }
  $('invalidation').innerHTML = `
    <strong>变更笔画：</strong>${plan.changedStrokeIds.map((x) => `<code>${x}</code>`).join(', ')}<br>
    <strong>失效分块：</strong>${plan.tileIds.map((x) => `<code>${x}</code>`).join(' ') || '无'}<br>
    <strong>重算分块：</strong>${(state.result?.recomputedTiles || plan.recomputeTiles || []).map((x) => `<code>${x}</code>`).join(' ')}
    <details><summary>节点清单 (${plan.items.length})</summary><pre class="json">${plan.items.map((i) => `${i.nodeId} [${i.kind}] - ${i.reason}`).join('\n')}</pre></details>`;
}

function renderScheduler() {
  const m = state.result?.schedulerMetrics;
  const results = state.result?.schedulerResults || [];
  if (!m) {
    $('scheduler-metrics').innerHTML = '';
    return;
  }
  $('scheduler-metrics').innerHTML = `
    <strong>调度预算：</strong>并行 ${m.maxParallel} · 线程像素缓冲 ${m.maxBufferPixels} · 队列预算 ${m.queueBudget}<br>
    <strong>结果：</strong>提交 ${m.submitted}，成功 ${m.succeeded}，失败 ${m.failed}<br>
    <strong>排队：</strong>${m.waits.length ? m.waits.map((w) => `<code>${w.jobId}</code> ${w.reason} 等待 ${w.waitMs}ms`).join('<br>') : '未发生队列耗尽等待'}
    ${results.filter((r) => !r.ok).map((r) => `<br><span class="badge bad">${r.tileId}: ${r.error}</span>`).join('')}`;
}

async function loadCheckpoints() {
  if (!state.current) return;
  try {
    const data = await api(`/api/checkpoints?project=${encodeURIComponent(state.current)}`);
    $('checkpoint-sequence').innerHTML = `
      <strong>SQLite 提交序列：</strong>${(data.sequence || []).map((x) => `<code>${x}</code>`).join(' → ') || '空'}
      <details><summary>事务记录 ${data.commits?.length || 0}</summary><pre class="json">${(data.commits || []).map((c) => `#${c.seq} ${c.strokeId} tiles=${c.tiles.length} residual=${c.residualDigest.slice(0, 12)}…`).join('\n')}</pre></details>`;
  } catch (err) {
    $('checkpoint-sequence').textContent = err.message;
  }
}

function drawOverlay() {
  const canvas = $('direction-overlay');
  const p = state.detail?.project;
  if (!p) return;
  canvas.width = p.width;
  canvas.height = p.height;
  const ctx = canvas.getContext('2d');
  ctx.clearRect(0, 0, canvas.width, canvas.height);
  ctx.lineWidth = 0.35;
  const verdictById = new Map((state.result?.directions?.segments || []).map((s) => [s.segmentId, s.verdict]));
  (p.truth?.segments || []).forEach((seg) => {
    const verdict = verdictById.get(seg.segmentId);
    ctx.strokeStyle = verdict === 'consistent' ? '#00d084' : verdict ? '#ff3b30' : '#245bdb';
    ctx.fillStyle = ctx.strokeStyle;
    const cx = (seg.bbox.minX + seg.bbox.maxX) / 2;
    const cy = (seg.bbox.minY + seg.bbox.maxY) / 2;
    const len = Math.max(2.2, Math.min(5, (seg.bbox.maxX - seg.bbox.minX + seg.bbox.maxY - seg.bbox.minY) / 2 + 2));
    const nx = seg.normalX || 1;
    const ny = seg.normalY || 0;
    const norm = Math.hypot(nx, ny) || 1;
    const x2 = cx + nx / norm * len;
    const y2 = cy + ny / norm * len;
    ctx.beginPath();
    ctx.moveTo(cx, cy);
    ctx.lineTo(x2, y2);
    ctx.stroke();
    const angle = Math.atan2(y2 - cy, x2 - cx);
    const head = 1.2;
    ctx.beginPath();
    ctx.moveTo(x2, y2);
    ctx.lineTo(x2 - head * Math.cos(angle - 0.5), y2 - head * Math.sin(angle - 0.5));
    ctx.lineTo(x2 - head * Math.cos(angle + 0.5), y2 - head * Math.sin(angle + 0.5));
    ctx.closePath();
    ctx.fill();
  });
}

async function runVerify(mode, extra = {}) {
  if (!state.current) return;
  $('run-status').textContent = `正在运行 ${mode}…`;
  try {
    const data = await post('/api/verify', { projectId: state.current, mode, ...extra });
    renderResult(data);
    $('run-status').textContent = data.ok ? `${mode} 完成` : `${mode} 完成但包含隔离错误/核验失败`;
  } catch (err) {
    $('run-status').textContent = err.message;
  }
}

async function applyEdit(payload) {
  const data = await post('/api/edit', { projectId: state.current, ...payload });
  state.plan = data.plan;
  if (data.result) state.result = data.result;
  syncLocalStrokeEdit(payload);
  renderResult({ result: data.result, plan: data.plan });
  $('run-status').textContent = `失效 ${data.plan.tileIds.length} 个分块，已精确重算 ${data.result?.recomputedTiles?.length || 0} 个`;
  await loadState();
}

function syncLocalStrokeEdit(payload) {
  const strokes = state.detail?.project?.strokes;
  if (!strokes) return;
  if (payload.type === 'update') {
    const stroke = strokes.find((x) => x.id === payload.strokeId);
    if (stroke) {
      if (payload.start) stroke.start = payload.start;
      if (payload.end) stroke.end = payload.end;
      $('edit-sx').value = stroke.start.x;
      $('edit-sy').value = stroke.start.y;
      $('edit-ex').value = stroke.end.x;
      $('edit-ey').value = stroke.end.y;
    }
  } else if (payload.type === 'add' && payload.stroke) {
    strokes.push(payload.stroke);
  } else if (payload.type === 'delete') {
    state.detail.project.strokes = strokes.filter((x) => x.id !== payload.strokeId);
  }
  const options = orderedStrokes().map((s) => `<option value="${s.id}">${s.id} · p${s.priority}/#${s.seq}</option>`).join('');
  $('stroke-select').innerHTML = options;
  $('component-stroke').innerHTML = options;
  if (payload.type !== 'delete' && payload.strokeId) $('stroke-select').value = payload.strokeId;
  fillStrokeEditor();
}

document.querySelectorAll('[data-mode]').forEach((btn) => {
  btn.onclick = () => runVerify(btn.dataset.mode);
});
$('verify-panic').onclick = () => runVerify('scheduled', { panicTileId: state.detail?.tiles?.[0]?.id || 'r00c00' });
$('refresh-state').onclick = loadState;
$('apply-edit').onclick = () => {
  const id = $('stroke-select').value;
  applyEdit({
    type: 'update', strokeId: id,
    start: { x: Number($('edit-sx').value), y: Number($('edit-sy').value) },
    end: { x: Number($('edit-ex').value), y: Number($('edit-ey').value) },
  }).then(loadState).catch((e) => { $('run-status').textContent = e.message; });
};
$('delete-stroke').onclick = () => applyEdit({ type: 'delete', strokeId: $('stroke-select').value }).catch((e) => { $('run-status').textContent = e.message; });
$('add-stroke').onclick = () => {
  const id = $('add-id').value.trim();
  if (!id) { $('run-status').textContent = '请填写新笔画 ID'; return; }
  applyEdit({
    type: 'add',
    stroke: {
      id, seq: 99, priority: 30, op: 'over', role: 'paint',
      start: { x: Number($('add-sx').value), y: Number($('add-sy').value) },
      end: { x: Number($('add-ex').value), y: Number($('add-ey').value) },
      radius: Number($('add-radius').value), weightBP: 10000, hardnessPermille: 1000,
    },
  }).catch((e) => { $('run-status').textContent = e.message; });
};
$('reset-project').onclick = async () => {
  await post('/api/reset', { projectId: state.current });
  await selectProject(state.current);
};

loadState().catch((err) => { $('project-list').textContent = err.message; });
