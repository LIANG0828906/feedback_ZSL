package workbench

// indexHTML 是单页工作台，纯原生 JS + Canvas，无外部素材依赖。
const indexHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>关卡声学编辑工作台</title>
<style>
  body { font-family: system-ui, sans-serif; margin: 12px; background:#0f1420; color:#e6ecf5; }
  h2 { margin: 6px 0; font-size: 16px; }
  .grid { display: grid; grid-template-columns: 560px 1fr; gap: 12px; }
  .card { background:#171f30; border:1px solid #2a3650; border-radius:8px; padding:10px; }
  canvas { background:#0b1020; border-radius:6px; }
  table { border-collapse: collapse; font-size: 12px; width:100%; }
  th, td { border:1px solid #2a3650; padding:3px 6px; text-align:right; }
  th { background:#202b42; }
  button { background:#2f6fed; color:#fff; border:0; border-radius:5px; padding:6px 10px; margin:3px; cursor:pointer; }
  button.secondary { background:#3a4763; }
  input { width:80px; background:#0b1020; color:#e6ecf5; border:1px solid #2a3650; border-radius:4px; padding:4px; }
  .hit { color:#58d68d; } .miss { color:#f5b041; } .fail { color:#ec7063; }
  .scroll { max-height: 260px; overflow:auto; }
</style>
</head>
<body>
<h2>关卡声学：环境声遮蔽与混响区域编辑工作台</h2>
<div class="grid">
  <div class="card">
    <h2>关卡俯视 / 混响区域 / 遮挡连线</h2>
    <canvas id="map" width="540" height="405"></canvas>
    <div>
      <button onclick="editReverb()">修改 R2 混响时间(增量)</button>
      <button onclick="dragBoundary()">拖动 R2 边界(增量)</button>
      <button class="secondary" onclick="fullRecompute()">全量重算</button>
      <button class="secondary" onclick="loadAll()">刷新</button>
    </div>
    <div id="stats" style="font-size:12px;margin-top:6px"></div>
  </div>
  <div class="card">
    <h2>听者衰减曲线（选择声源听者对）</h2>
    <canvas id="curve" width="520" height="200"></canvas>
    <h2>剖面热力图（逐听者 MeanSPL）</h2>
    <canvas id="heat" width="520" height="120"></canvas>
  </div>
</div>
<div class="card" style="margin-top:12px">
  <h2>声源听者对 / 缓存命中 / 重试</h2>
  <div class="scroll"><table id="pairs"></table></div>
</div>
<div class="card" style="margin-top:12px">
  <h2>失效传播清单</h2>
  <div class="scroll"><table id="invs"></table></div>
</div>
<script>
let project = null, results = [], profiles = [], selected = null;
async function getJSON(u){ const r = await fetch(u); return r.json(); }
async function postJSON(u, body){ const r = await fetch(u, {method:'POST', headers:{'Content-Type':'application/json'}, body:JSON.stringify(body)}); return r.json(); }
function sx(x){ return x / project.level.x1 * 540; }
function sy(y){ return 405 - y / project.level.y1 * 405; }
const regionColors = {"R1":"rgba(47,111,237,0.28)","R2":"rgba(237,143,47,0.28)"};
async function loadAll(){
  project = await getJSON('/api/project');
  results = await getJSON('/api/results');
  profiles = await getJSON('/api/profiles');
  const invs = await getJSON('/api/invalidations');
  const stats = await getJSON('/api/stats');
  drawMap(); drawCurve(); drawHeat(); renderPairs(); renderInvs(invs);
  document.getElementById('stats').innerHTML =
    '缓存命中 <span class="hit">'+stats.cacheHits+'</span> / 未命中 <span class="miss">'+stats.cacheMisses+'</span> / '+stats.storage+
    (stats.backpressure && stats.backpressure.length ? ' / 背压事件:'+stats.backpressure.length : '');
}
function drawMap(){
  const c = document.getElementById('map'), ctx = c.getContext('2d');
  ctx.clearRect(0,0,540,405);
  for(const r of project.regions){
    ctx.beginPath();
    r.vertices.forEach((v,i)=>{ const X=sx(v.X),Y=sy(v.Y); i?ctx.lineTo(X,Y):ctx.moveTo(X,Y); });
    ctx.closePath(); ctx.fillStyle = regionColors[r.id] || 'rgba(150,150,150,.25)'; ctx.fill();
    ctx.strokeStyle='#8fa8d8'; ctx.stroke();
    ctx.fillStyle='#cfe0ff'; ctx.fillText(r.id+' RT60='+r.reverbTime, sx(r.vertices[0].X)+6, sy(r.vertices[0].Y)-8);
  }
  for(const w of project.walls){
    ctx.beginPath(); ctx.moveTo(sx(w.a.x),sy(w.a.y)); ctx.lineTo(sx(w.b.x),sy(w.b.y));
    ctx.lineWidth=3; ctx.strokeStyle='#e6ecf5'; ctx.stroke(); ctx.lineWidth=1;
  }
  for(const s of project.sources){ ctx.fillStyle='#58d68d'; ctx.beginPath(); ctx.arc(sx(s.pos.x),sy(s.pos.y),5,0,7); ctx.fill(); ctx.fillStyle='#fff'; ctx.fillText(s.id,sx(s.pos.x)+6,sy(s.pos.y)); }
  for(const l of project.listeners){ ctx.fillStyle='#f5b041'; ctx.fillRect(sx(l.pos.x)-4,sy(l.pos.y)-4,8,8); }
  for(const r of results){
    const s = project.sources.find(x=>x.id===r.sourceId);
    const l = project.listeners.find(x=>x.id===r.listenerId);
    ctx.beginPath(); ctx.moveTo(sx(s.pos.x),sy(s.pos.y)); ctx.lineTo(sx(l.pos.x),sy(l.pos.y));
    ctx.strokeStyle = r.failed ? '#ec7063' : (r.occluded ? 'rgba(236,112,99,.7)' : 'rgba(120,180,255,.35)');
    ctx.stroke();
  }
}
function drawCurve(){
  const c=document.getElementById('curve'), ctx=c.getContext('2d');
  ctx.clearRect(0,0,520,200); ctx.strokeStyle='#2a3650';
  ctx.beginPath(); ctx.moveTo(40,10); ctx.lineTo(40,180); ctx.lineTo(510,180); ctx.stroke();
  const r = selected || results[0]; if(!r) return;
  ctx.strokeStyle='#58d68d'; ctx.beginPath();
  const vals=r.curve, min=Math.min(...vals), max=Math.max(...vals);
  vals.forEach((v,i)=>{ const X=40+i*(460/(vals.length-1)); const Y=180-((v-min)/(max-min||1))*160; i?ctx.lineTo(X,Y):ctx.moveTo(X,Y); });
  ctx.stroke();
  ctx.fillStyle='#cfe0ff'; ctx.fillText(r.sourceId+' -> '+r.listenerId+' SPL='+r.spl.toFixed(2)+' dB'+(r.occluded?' [遮挡]':''),46,24);
}
function drawHeat(){
  const c=document.getElementById('heat'), ctx=c.getContext('2d');
  ctx.clearRect(0,0,520,120);
  const vals=profiles.map(p=>p.meanSpl), min=Math.min(...vals), max=Math.max(...vals);
  profiles.forEach((p,i)=>{
    const t=(p.meanSpl-min)/((max-min)||1);
    ctx.fillStyle='rgb('+Math.floor(60+t*195)+','+Math.floor(120-t*60)+','+Math.floor(220-t*160)+')';
    const w=520/profiles.length;
    ctx.fillRect(i*w,10,w-2,80);
    ctx.fillStyle='#fff'; ctx.fillText(p.listenerId, i*w+4, 105);
  });
}
function renderPairs(){
  let html='<tr><th>对</th><th>遮挡</th><th>绕射长</th><th>SPL</th><th>状态</th><th>操作</th></tr>';
  for(const r of results){
    const key=r.sourceId+'->'+r.listenerId;
    html += '<tr><td style="text-align:left">'+key+'</td><td>'+r.occluded+'</td><td>'+r.diffractionLen.toFixed(3)+'</td><td>'+r.spl.toFixed(2)+'</td><td>'+
      (r.failed?'<span class="fail">'+r.errorCode+'</span>':(r.cacheHit?'<span class="hit">缓存命中</span>':'<span class="miss">重算</span>'))+
      '</td><td><button class="secondary" onclick="selectPair(\\''+r.sourceId+'\\',\\''+r.listenerId+'\\')">查看</button> '+
      '<button class="secondary" onclick="retryPair(\\''+r.sourceId+'\\',\\''+r.listenerId+'\\')">重试</button></td></tr>';
  }
  document.getElementById('pairs').innerHTML=html;
}
function renderInvs(invs){
  let html='<tr><th>对象</th><th>类型</th><th>原因</th></tr>';
  for(const v of invs){ html+='<tr><td style="text-align:left">'+v.objectId+'</td><td>'+v.kind+'</td><td style="text-align:left">'+v.reason+'</td></tr>'; }
  document.getElementById('invs').innerHTML=html;
}
function selectPair(s,l){ selected = results.find(r=>r.sourceId===s&&r.listenerId===l); drawCurve(); }
async function retryPair(s,l){ await postJSON('/api/retry',{sourceId:s,listenerId:l}); await loadAll(); }
async function editReverb(){ await postJSON('/api/edit/region',{regionId:'R2',reverbTime:0.9}); await loadAll(); }
async function dragBoundary(){
  const r = project.regions.find(x=>x.id==='R2');
  const verts = r.vertices.map(v=>({x:v.x+1,y:v.y}));
  await postJSON('/api/edit/region',{regionId:'R2',vertices:verts}); await loadAll();
}
async function fullRecompute(){ await postJSON('/api/full',{}); await loadAll(); }
loadAll();
</script>
</body>
</html>`
