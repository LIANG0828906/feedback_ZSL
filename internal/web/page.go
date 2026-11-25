package web

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>音素到口型包调度诊断</title>
<style>
:root { color-scheme: dark; --bg:#0b1020; --panel:#121a31; --line:#263456; --text:#e8eefc; --muted:#93a4c7; --accent:#67e8f9; --ok:#34d399; --warn:#fbbf24; --bad:#fb7185; }
* { box-sizing: border-box; }
body { margin:0; font:14px/1.45 ui-sans-serif,system-ui,"Noto Sans CJK SC","Microsoft YaHei",sans-serif; background:var(--bg); color:var(--text); }
header { padding:16px 22px; border-bottom:1px solid var(--line); display:flex; gap:12px; align-items:center; flex-wrap:wrap; background:linear-gradient(120deg,#111c3a,#0b1020); }
h1 { font-size:18px; margin:0; }
h2 { font-size:14px; margin:0 0 10px; color:#c9d8ff; }
button,select,input { background:#0e1730; color:var(--text); border:1px solid var(--line); border-radius:8px; padding:7px 10px; }
button { cursor:pointer; }
button:hover { border-color:var(--accent); }
main { padding:18px 22px; display:grid; grid-template-columns:260px 1fr 360px; gap:16px; align-items:start; }
.panel { background:rgba(18,26,49,.92); border:1px solid var(--line); border-radius:14px; padding:14px; box-shadow:0 10px 30px rgba(0,0,0,.18); }
.full { grid-column:1 / -1; }
.dialogue { width:100%; text-align:left; margin-bottom:8px; display:flex; justify-content:space-between; gap:8px; }
.dialogue.active { border-color:var(--accent); box-shadow:inset 0 0 0 1px var(--accent); }
.muted { color:var(--muted); font-size:12px; }
.timeline { position:relative; height:170px; border-radius:10px; background:repeating-linear-gradient(90deg,rgba(255,255,255,.04) 0,rgba(255,255,255,.04) 1px,transparent 1px,transparent 5%); overflow:hidden; border:1px solid var(--line); }
.pack { position:absolute; top:18px; height:54px; border-radius:8px; padding:6px; overflow:hidden; border:1px solid rgba(255,255,255,.25); background:linear-gradient(180deg,rgba(103,232,249,.35),rgba(59,130,246,.28)); font-size:11px; }
.pack.silence { background:linear-gradient(180deg,rgba(148,163,184,.35),rgba(71,85,105,.3)); }
.scale { display:flex; justify-content:space-between; color:var(--muted); font-size:11px; margin-top:4px; }
.bars { height:120px; display:flex; align-items:flex-end; gap:1px; overflow-x:auto; padding-bottom:4px; border-bottom:1px solid var(--line); }
.bar { min-width:5px; flex:1 0 3px; background:linear-gradient(180deg,var(--accent),#3b82f6); border-radius:3px 3px 0 0; }
.bar.over { background:linear-gradient(180deg,var(--bad),#be123c); }
.bar.zero { background:#263456; height:2px !important; }
table { width:100%; border-collapse:collapse; font-size:12px; }
th,td { border-bottom:1px solid var(--line); padding:6px 4px; text-align:left; vertical-align:top; }
th { color:var(--muted); font-weight:600; }
.pill { display:inline-block; border-radius:999px; padding:2px 8px; font-size:11px; border:1px solid var(--line); }
.ok { color:var(--ok); border-color:rgba(52,211,153,.4); }
.warn { color:var(--warn); border-color:rgba(251,191,36,.4); }
.bad { color:var(--bad); border-color:rgba(251,113,133,.4); }
.row { display:flex; gap:8px; flex-wrap:wrap; align-items:center; margin-bottom:8px; }
.list { max-height:260px; overflow:auto; }
pre { white-space:pre-wrap; word-break:break-word; background:#080d1d; border:1px solid var(--line); border-radius:10px; padding:10px; max-height:360px; overflow:auto; font-size:11px; }
.kv { display:grid; grid-template-columns:120px 1fr; gap:4px 8px; font-size:12px; }
.kv b { color:var(--muted); font-weight:500; }
@media (max-width:1100px){ main{grid-template-columns:1fr;} }
</style>
</head>
<body>
<header>
  <h1>音素边界 → 口型包 → 逐帧调度</h1>
  <span class="pill" id="version"></span>
  <button onclick="loadState()">刷新状态</button>
  <button onclick="runQueueDemo()">排队预算演示</button>
  <button onclick="runAcceptance()">运行三路径验收</button>
  <span id="globalStatus" class="muted"></span>
</header>
<main>
  <section class="panel">
    <h2>对白 / 分片</h2>
    <div id="dialogueList"></div>
    <h2 style="margin-top:16px">检查点提交顺序</h2>
    <div class="list"><table id="commitTable"></table></div>
  </section>

  <section class="panel">
    <h2 id="timelineTitle">口型包时序</h2>
    <div class="timeline" id="timeline"></div>
    <div class="scale"><span id="frameStart">0</span><span id="frameEnd"></span></div>
    <h2 style="margin-top:18px">逐帧下发预算占用</h2>
    <div class="bars" id="budgetBars"></div>
    <div class="muted" id="budgetSummary"></div>
  </section>

  <section class="panel">
    <h2>编辑与失效注入</h2>
    <div class="row">
      <select id="editDialogue"></select>
      <select id="editField">
        <option value="weight">音素权重</option>
        <option value="sharedBoundary">共享边界</option>
        <option value="affinityToNext">过渡亲和度</option>
      </select>
    </div>
    <div class="row">
      <input id="phonemeIndex" type="number" value="1" min="0" style="width:80px">
      <input id="intValue" type="number" value="420" style="width:120px">
      <button onclick="submitEdit()">应用编辑</button>
    </div>
    <div class="row">
      <button onclick="submitRecipeBudget()">预算 +1</button>
      <button onclick="inject('order')">E_ORDER</button>
      <button onclick="inject('label')">E_LABEL</button>
      <button onclick="inject('vis')">E_VIS</button>
      <button onclick="inject('stage')">E_STAGE</button>
    </div>
    <div id="actionResult" class="muted"></div>
    <h2 style="margin-top:16px">失效传播</h2>
    <div id="invalidation" class="list"></div>
  </section>

  <section class="panel full">
    <h2>队列 / 资源边界明细</h2>
    <div class="kv" id="resourceSummary"></div>
    <table id="queueTable"><thead><tr><th>时间</th><th>分片</th><th>对白</th><th>原因</th><th>等待 ms</th><th>缓冲/预算</th></tr></thead><tbody></tbody></table>
  </section>

  <section class="panel full">
    <h2>验收报告 / API JSON</h2>
    <pre id="report">点击“运行三路径验收”执行增量、断点恢复与全量一致性比对。</pre>
  </section>
</main>
<script>
let state = null;
let selectedDialogue = null;

async function loadState() {
  const resp = await fetch('/api/state');
  state = await resp.json();
  document.getElementById('version').textContent = state.project.algorithmVersion + ' / ' + state.project.recipe.recipeVersion;
  document.getElementById('globalStatus').textContent = '缓存条目 ' + state.cacheStats.entries + '，复用 ' + state.cacheStats.reused;
  const select = document.getElementById('editDialogue');
  select.innerHTML = '';
  state.project.dialogues.forEach(d => {
    const opt = document.createElement('option');
    opt.value = d.id; opt.textContent = d.id + ' / shard ' + d.shardIndex;
    select.appendChild(opt);
  });
  if (!selectedDialogue && state.project.dialogues.length) selectedDialogue = state.project.dialogues[0].id;
  select.value = selectedDialogue;
  renderDialogues();
  renderCommits();
  renderBudget();
  renderInvalidation(state.manifest);
  renderQueue();
}

function renderDialogues() {
  const box = document.getElementById('dialogueList');
  box.innerHTML = '';
  state.result.dialogues.forEach(dr => {
    const btn = document.createElement('button');
    btn.className = 'dialogue' + (dr.dialogue.id === selectedDialogue ? ' active' : '');
    btn.innerHTML = '<span>' + dr.dialogue.id + '</span><span class="muted">' + dr.packs.length + ' 包 / ' + dr.attribution.length + ' 帧</span>';
    btn.onclick = () => { selectedDialogue = dr.dialogue.id; renderDialogues(); renderTimeline(); };
    box.appendChild(btn);
  });
  renderTimeline();
}

function currentDialogueResult() {
  return state.result.dialogues.find(dr => dr.dialogue.id === selectedDialogue);
}

function renderTimeline() {
  const dr = currentDialogueResult();
  if (!dr) return;
  const total = Math.max(1, dr.dialogue ? Math.max.apply(null, dr.packs.map(p => p.endFrame).concat([1])) : 1);
  document.getElementById('timelineTitle').textContent = '口型包时序 · ' + dr.dialogue.id;
  document.getElementById('frameEnd').textContent = total;
  const lane = document.getElementById('timeline');
  lane.innerHTML = '';
  dr.packs.forEach(p => {
    const el = document.createElement('div');
    const silence = p.phonemeIds.some(id => {
      const q = dr.quantized.find(x => x.id === id); return q && q.class === 'silence';
    });
    el.className = 'pack' + (silence ? ' silence' : '');
    el.style.left = (100 * p.startFrame / total) + '%';
    el.style.width = Math.max(2, 100 * (p.endFrame - p.startFrame) / total) + '%';
    el.title = p.decision.rationale;
    el.textContent = 'P' + p.index + ' [' + p.startFrame + ',' + p.endFrame + ') n=' + p.phonemeIds.length;
    lane.appendChild(el);
  });
}

function renderBudget() {
  const frames = state.result.dispatch.frames;
  const limit = state.project.recipe.framePackBudget;
  const max = Math.max(limit, Math.max.apply(null, frames.map(f => f.budgetUsed).concat([1])));
  const box = document.getElementById('budgetBars');
  box.innerHTML = '';
  frames.forEach(f => {
    const bar = document.createElement('div');
    const h = f.budgetUsed === 0 ? 2 : Math.round(100 * f.budgetUsed / max);
    bar.className = 'bar' + (f.budgetUsed > limit ? ' over' : '') + (f.budgetUsed === 0 ? ' zero' : '');
    bar.style.height = h + 'px';
    bar.title = 'frame ' + f.frame + ': used=' + f.budgetUsed + '/' + limit + ', active=' + f.activePackCount;
    box.appendChild(bar);
  });
  const d = state.result.dispatch;
  document.getElementById('budgetSummary').textContent = '帧数 ' + d.frameCount + '；延后记录 ' + d.deferrals.length + '；拒绝 ' + d.rejections.length + '；预算上限 ' + limit + '，通道 ' + state.project.recipe.speakerChannels;
}

function renderCommits() {
  const table = document.getElementById('commitTable');
  table.innerHTML = '<thead><tr><th>seq</th><th>shard</th><th>dialogue</th><th>hash/version</th></tr></thead>';
  state.commits.forEach(c => {
    const tr = document.createElement('tr');
    tr.innerHTML = '<td>' + c.commitSeq + '</td><td>' + c.shardIndex + '</td><td>' + c.dialogueId + '</td><td class="muted">' + c.contentHash.slice(0,10) + '<br>' + c.recipeVersion + '</td>';
    table.appendChild(tr);
  });
}

function renderInvalidation(m) {
  const box = document.getElementById('invalidation');
  if (!m) { box.innerHTML = '<span class="muted">尚未编辑。依赖图节点 ' + Object.keys(state.graph.nodes).length + '，边 ' + state.graph.edges.length + '。</span>'; return; }
  let html = '<div class="kv"><b>revision</b><span>' + m.revision + '</span><b>起点</b><span>' + m.startedNodes.length + '</span><b>候选传播</b><span>' + m.candidateNodes.length + '</span><b>失效包</b><span class="bad">' + (m.packs||[]).length + '</span><b>下游帧</b><span class="warn">' + (m.downstreamFrames||[]).length + '</span><b>复用包</b><span class="ok">' + (m.reusedPacks||[]).length + '</span></div>';
  html += '<table><thead><tr><th>对白</th><th>包</th><th>原因</th></tr></thead><tbody>';
  (m.packs||[]).forEach(p => { html += '<tr><td>' + p.dialogueId + '</td><td>' + p.packIndex + '</td><td>' + (p.reasons||[]).join(', ') + '</td></tr>'; });
  html += '</tbody></table>';
  box.innerHTML = html;
}

function renderQueue() {
  const p = state.poolOptions;
  document.getElementById('resourceSummary').innerHTML = '<b>最大并行度</b><span>' + p.maxParallelism + '</span><b>线程缓冲上限</b><span>' + p.perWorkerBuffer + '</span><b>分片队列预算</b><span>' + p.queueBudget + '</span><b>累计事件</b><span>' + (state.queueEvents||[]).length + ' / demo ' + (state.queueDemo||[]).length + '</span>';
  const tbody = document.querySelector('#queueTable tbody');
  tbody.innerHTML = '';
  (state.queueDemo || []).concat(state.queueEvents || []).slice(-50).forEach(e => {
    const tr = document.createElement('tr');
    tr.innerHTML = '<td>' + e.time + '</td><td>' + e.shardIndex + '</td><td>' + e.dialogueId + '</td><td><span class="pill warn">' + e.reason + '</span></td><td>' + e.waitedMillis + '</td><td>' + e.bufferUsed + '/' + e.queueBudget + '</td>';
    tbody.appendChild(tr);
  });
}

async function postJSON(url, body) {
  const resp = await fetch(url, {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body || {})});
  const data = await resp.json();
  if (!resp.ok || data.ok === false) throw data.error || new Error('request failed');
  return data;
}

async function submitEdit() {
  const field = document.getElementById('editField').value;
  const edit = {dialogueId: document.getElementById('editDialogue').value, field: field, phonemeIndex: parseInt(document.getElementById('phonemeIndex').value,10), intValue: parseInt(document.getElementById('intValue').value,10)};
  if (field === 'sharedBoundary') edit.boundaryIndex = edit.phonemeIndex;
  try {
    const data = await postJSON('/api/edit', edit);
    document.getElementById('actionResult').innerHTML = '<span class="pill ok">增量/全量一致</span>';
    state.manifest = data.manifest;
    renderInvalidation(data.manifest);
    await loadState();
  } catch (e) { showError(e); }
}

async function submitRecipeBudget() {
  const budget = state.project.recipe.framePackBudget + 1;
  try { await postJSON('/api/edit', {field:'recipe.framePackBudget', intValue:budget}); await loadState(); }
  catch(e){ showError(e); }
}

async function inject(kind) {
  try { await postJSON('/api/inject', {kind}); }
  catch(e){ showError(e); }
}

async function runQueueDemo() {
  const data = await postJSON('/api/queue-demo', {});
  state.queueDemo = data.events;
  renderQueue();
}

async function runAcceptance() {
  document.getElementById('report').textContent = '运行中...';
  try {
    const data = await postJSON('/api/acceptance', {});
    document.getElementById('report').textContent = JSON.stringify(data, null, 2);
    document.getElementById('globalStatus').textContent = '验收 OK';
  } catch(e) { showError(e); document.getElementById('report').textContent = JSON.stringify(e,null,2); }
}

function showError(e) {
  const text = e.code ? (e.code + ' ' + (e.dialogueId||'') + ' shard=' + e.shardIndex + ' frame=' + e.frame + ' ' + (e.fieldPath||'') + ': ' + e.message) : String(e);
  document.getElementById('actionResult').innerHTML = '<span class="pill bad">' + text + '</span>';
}

loadState().catch(showError);
setInterval(loadState, 5000);
</script>
</body>
</html>`
