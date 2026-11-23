package httpapi

// indexHTML 为单文件诊断页面，仅依赖浏览器原生能力与本地 JSON API。
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<title>遮挡剔除 / 流式加载诊断工作台</title>
<style>
  body { font-family: system-ui, "Segoe UI", sans-serif; margin: 0; background:#0f1420; color:#e6ebf5; }
  header { padding:12px 18px; background:#16203355; border-bottom:1px solid #243049; }
  h1 { font-size:17px; margin:0; }
  .wrap { display:grid; grid-template-columns: 1.2fr 1fr; gap:14px; padding:14px; }
  .card { background:#162033; border:1px solid #243049; border-radius:10px; padding:12px; }
  .card h2 { font-size:13px; margin:0 0 8px; color:#9fb4d8; text-transform:uppercase; letter-spacing:.5px; }
  canvas { background:#0b1020; border-radius:8px; width:100%; }
  table { border-collapse:collapse; width:100%; font-size:12px; }
  th,td { border:1px solid #243049; padding:4px 6px; text-align:left; }
  button { background:#2f6df0; color:#fff; border:0; border-radius:6px; padding:6px 12px; cursor:pointer; margin:2px; }
  button.secondary { background:#33415c; }
  input { background:#0b1020; border:1px solid #243049; color:#e6ebf5; border-radius:6px; padding:5px; width:90px; }
  .pill { display:inline-block; padding:2px 8px; border-radius:999px; font-size:11px; }
  .committed { background:#1f7a4d; } .failed { background:#b03a48; }
  .loading { background:#b07a1f; } .culling { background:#3a5fb0; } .pending { background:#475569; }
  .muted { color:#8aa0c4; font-size:12px; }
  .ok { color:#54e08a; } .bad { color:#ff7d8c; }
  .grid2 { display:grid; grid-template-columns:1fr 1fr; gap:8px; }
</style>
</head>
<body>
<header>
  <h1>遮挡剔除与场景流式加载 · 可视化诊断工作台</h1>
  <div class="muted" id="meta">加载中…</div>
</header>
<div class="wrap">
  <div class="card">
    <h2>分块着色预览 / 遮挡网格 / 相机轨迹</h2>
    <canvas id="map" width="640" height="640"></canvas>
    <div style="margin-top:8px">
      <button onclick="runPreview()">粗粒度预览</button>
      <button onclick="runPrecise()">精确剔除</button>
      <button onclick="runVerify()">三路径比对</button>
      <button class="secondary" onclick="location.href='/api/export'">导出 JSON</button>
    </div>
    <div class="muted" id="previewMeta"></div>
  </div>
  <div>
    <div class="card">
      <h2>参数调整（触发增量重算）</h2>
      <div class="grid2">
        <label>加载槽位 <input id="slots" type="number" value="3"/></label>
        <label>内存预算 <input id="memory" type="number" value="20"/></label>
        <label>栅格分辨率 <input id="grid" type="number" value="64"/></label>
      </div>
      <button onclick="updateBudget()">更新槽位/预算</button>
      <button onclick="updateQuant()">更新量化精度</button>
      <button class="secondary" onclick="retryWindow()">重试窗口 #0</button>
    </div>
    <div class="card">
      <h2>三路径比对 / 缓存</h2>
      <div id="verify" class="muted">尚未运行</div>
    </div>
    <div class="card">
      <h2>窗口状态矩阵</h2>
      <div id="windows" class="muted">无数据</div>
    </div>
    <div class="card">
      <h2>槽位占用 / 被延迟分块</h2>
      <div id="slotsview" class="muted">无数据</div>
    </div>
  </div>
</div>
<script>
let chunks = [], occluders = [], trajectory = [];
async function getJSON(u, opt){ const r = await fetch(u, opt); return r.json(); }
async function postJSON(u, body){ const r = await fetch(u, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)}); return r.json(); }

async function loadChunks(){
  const d = await getJSON('/api/chunks');
  chunks = d.chunks; occluders = d.occluders; trajectory = d.trajectory;
  draw();
}
async function loadSnap(){
  const s = await getJSON('/api/snapshot');
  document.getElementById('meta').textContent =
    '世界 ' + s.worldName + ' · 种子 ' + s.seed + ' · 分区 ' + s.partitionCount +
    ' · 分块 ' + s.chunkCount + ' · 采样 ' + s.sampleCount +
    ' · 工作线程 ' + s.poolWorkers + ' · 缓存命中 ' + s.cacheHits + ' / 未命中 ' + s.cacheMisses;
  document.getElementById('slots').value = s.config.budget.slots;
  document.getElementById('memory').value = s.config.budget.memory;
  document.getElementById('grid').value = s.config.cullParams.gridResolution;
  renderWindows(s.windows||[]);
  renderSlots(s.windows||[]);
  if (s.previewMeta && s.previewMeta.gridResolution) {
    document.getElementById('previewMeta').textContent =
      '预览栅格分辨率=' + s.previewMeta.gridResolution + ' · 抽样步长=' + s.previewMeta.sampleStride +
      ' · 抽样比=' + s.previewMeta.sampleRatio.toFixed(3);
  }
}

function bounds(){
  let maxX=0, maxZ=0;
  chunks.forEach(c=>{ maxX=Math.max(maxX,c.maxX); maxZ=Math.max(maxZ,c.maxZ); });
  return {maxX, maxZ};
}
function draw(highlight){
  const cv = document.getElementById('map'), ctx = cv.getContext('2d');
  const b = bounds(), W=cv.width, H=cv.height;
  const sx = W/b.maxX, sz = H/b.maxZ;
  ctx.clearRect(0,0,W,H);
  chunks.forEach(c=>{
    ctx.fillStyle = highlight && highlight.includes(c.id) ? '#ffd166' : c.color;
    ctx.globalAlpha = highlight && highlight.includes(c.id) ? 1.0 : 0.55;
    ctx.fillRect(c.minX*sx, c.minZ*sz, (c.maxX-c.minX)*sx-1, (c.maxZ-c.minZ)*sz-1);
    ctx.globalAlpha = 1;
    ctx.fillStyle='#cdd8ee'; ctx.font='10px sans-serif';
    ctx.fillText(String(c.id), c.minX*sx+3, c.minZ*sz+12);
  });
  // 遮挡体网格（深色描边）
  ctx.strokeStyle='#ff5d73'; ctx.lineWidth=1.5;
  occluders.forEach(o=>{
    ctx.strokeRect(o.box.min.x*sx, o.box.min.z*sz, (o.box.max.x-o.box.min.x)*sx, (o.box.max.z-o.box.min.z)*sz);
  });
  // 相机轨迹
  ctx.strokeStyle='#54e08a'; ctx.lineWidth=2; ctx.beginPath();
  trajectory.forEach((p,i)=>{ const x=p.pos.x*sx, z=p.pos.z*sz; i?ctx.lineTo(x,z):ctx.moveTo(x,z); });
  ctx.stroke();
}

function renderWindows(ws){
  if(!ws.length){ document.getElementById('windows').textContent='无数据（点击三路径比对）'; return; }
  let html = '<table><tr><th>窗口</th><th>状态</th><th>可见</th><th>加载</th><th>卸载</th><th>延迟</th></tr>';
  ws.forEach(w=>{
    html += '<tr><td>#'+w.index+'</td><td><span class="pill '+w.state+'">'+w.state+'</span></td>'+
      '<td>'+(w.visible||[]).join(',')+'</td><td>'+(w.loadList||[]).join(',')+'</td>'+
      '<td>'+(w.unloadList||[]).join(',')+'</td><td>'+(w.delayed||[]).map(d=>d.chunkId+':'+d.reason).join('; ')+'</td></tr>';
  });
  document.getElementById('windows').innerHTML = html + '</table>';
}
function renderSlots(ws){
  const last = ws[ws.length-1];
  if(!last){ document.getElementById('slotsview').textContent='无数据'; return; }
  document.getElementById('slotsview').innerHTML =
    '当前驻留分块：<b>'+(last.resident||[]).join(', ')+'</b><br/>'+
    '被延迟：<span class="bad">'+((last.delayed||[]).map(d=>'#'+d.chunkId+'('+d.reason+')').join('; ')||'无')+'</span>';
}

async function runPreview(){
  const d = await postJSON('/api/preview', {});
  const last = d.result.samples[d.result.samples.length-1];
  draw(last.visible);
  document.getElementById('previewMeta').textContent =
    '预览栅格分辨率=' + d.meta.gridResolution + ' · 抽样步长=' + d.meta.sampleStride +
    ' · 抽样比=' + d.meta.sampleRatio.toFixed(3);
}
async function runPrecise(){ const d = await getJSON('/api/precise'); const last=d.samples[d.samples.length-1]; draw(last.visible); }
async function runVerify(){
  const d = await getJSON('/api/verify');
  const el = document.getElementById('verify');
  if(d.ok){ el.innerHTML='<span class="ok">三路径逐窗口一致 ✓</span> · 路径：'+d.paths.join(', '); }
  else {
    el.innerHTML='<span class="bad">'+d.error+'</span> '+ (d.divergence?('窗口#'+d.divergence.windowIndex+' 分块#'+d.divergence.chunkId+' 字段='+d.divergence.field):'') + '<br/><span class="muted">'+(d.detail||'')+'</span>';
  }
  loadSnap();
}
async function updateBudget(){ await postJSON('/api/budget', {slots: +slots.value, memory: +memory.value}); loadSnap(); }
async function updateQuant(){ await postJSON('/api/quant', {gridResolution: +grid.value}); loadSnap(); }
async function retryWindow(){ const d = await postJSON('/api/retry', {window:0}); alert(JSON.stringify(d)); }

loadChunks(); loadSnap(); setInterval(loadSnap, 4000);
</script>
</body>
</html>`
