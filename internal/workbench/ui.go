package workbench

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>录屏内容完整性核验台</title>
<style>
:root{--bg:#08111f;--panel:#0e1b2c;--line:#22344c;--ink:#e9f2ff;--muted:#8da4bf;--ok:#4bd6a2;--bad:#ff667d;--warn:#f6c85f;--accent:#55a8ff}
*{box-sizing:border-box}body{margin:0;background:radial-gradient(circle at 30% 0,#132844 0,#08111f 48%);color:var(--ink);font:14px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace}
header{padding:24px 28px 12px;display:flex;gap:18px;align-items:end;flex-wrap:wrap;border-bottom:1px solid var(--line)}
h1{font-size:21px;margin:0;letter-spacing:.04em}small{color:var(--muted)}
main{padding:18px 28px 60px;max-width:1500px;margin:auto}.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center}
select,input,button{background:#0a1626;color:var(--ink);border:1px solid var(--line);border-radius:7px;padding:8px 10px;font:inherit}
button{cursor:pointer}button:hover{border-color:var(--accent)}button.primary{background:linear-gradient(180deg,#1f7fe8,#1261b6);border-color:#3f9af7}button.danger{border-color:#7a2f3c;color:#ff93a3}
.panel{background:rgba(14,27,44,.82);border:1px solid var(--line);border-radius:12px;padding:14px;margin-top:14px;box-shadow:0 12px 30px #0003}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:12px}.stat{border-left:3px solid var(--accent);padding:4px 10px}.stat b{display:block;font-size:19px;margin-top:2px}
.timeline{display:flex;gap:5px;overflow:auto;padding:8px 0}.seg{min-width:56px;padding:7px;border-radius:6px;border:1px solid var(--line);background:#122239;cursor:pointer;text-align:center;font-size:11px;white-space:nowrap}.seg.committed{border-color:#237d61;background:#0e332a}.seg.verifying{border-color:#a07a1c;background:#332b10}.seg.failed{border-color:#8e3544;background:#35151b}.seg.pending{opacity:.65}
.media{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:12px}.media figure{margin:0}.media img{width:100%;image-rendering:pixelated;background:#000;border:1px solid var(--line);border-radius:8px;min-height:120px}.media figcaption{color:var(--muted);font-size:12px;margin-top:5px}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{padding:6px 8px;border-bottom:1px solid #1b2d43;text-align:left;vertical-align:top}th{color:var(--muted);position:sticky;top:0;background:#0e1b2c}tr:hover td{background:#12243a}tr.replaced td{color:#ffc1ca}tr.suspect td{color:#ffe19a}
.tablewrap{max-height:420px;overflow:auto;border:1px solid var(--line);border-radius:8px}.mono{font-family:inherit}.muted{color:var(--muted)}.bad{color:var(--bad)}.ok{color:var(--ok)}.warn{color:var(--warn)}
#status{padding:8px 11px;border-radius:7px;border:1px solid var(--line);min-width:180px}.hidden{display:none}.err{white-space:pre-wrap;color:#ff9aaa;background:#2b1118;border:1px solid #6b2836;padding:10px;border-radius:8px}
.legend{color:var(--muted);font-size:12px}.bar{height:7px;background:#102237;border-radius:8px;overflow:hidden}.bar>i{display:block;height:100%;background:linear-gradient(90deg,#36b98a,#55a8ff);width:0}
@media(max-width:900px){.media{grid-template-columns:1fr}main,header{padding-left:14px;padding-right:14px}}
</style>
</head>
<body>
<header>
  <div><h1>录屏内容完整性核验台</h1><small>块级哈希 · 时间轴连续性 · 残差与噪声复核 · 分段检查点</small></div>
  <div class="row" style="margin-left:auto">
    <label><small>样本</small><br><select id="sample"></select></label>
    <label><small>模式</small><br><select id="mode"><option>auto</option><option>fine</option><option>candidate</option></select></label>
  </div>
</header>
<main>
  <div class="panel">
    <div class="row">
      <label>预算 <input id="budget" type="number" min="1" style="width:82px"></label>
      <label>分块 <input id="block" type="number" min="1" style="width:76px"></label>
      <label>残差阈值 <input id="residual" type="number" step="0.001" style="width:96px"></label>
      <label>噪声阈值 <input id="noise" type="number" step="0.001" style="width:96px"></label>
      <label>线程 <input id="workers" type="number" min="1" max="64" style="width:70px"></label>
      <button class="primary" onclick="startJob(false)">开始/续跑</button>
      <button onclick="startJob(true)">全量重算</button>
      <button onclick="postAction('pause')">暂停</button>
      <button onclick="postAction('resume')">继续</button>
      <button class="danger" onclick="postAction('cancel')">取消</button>
      <button onclick="postAction('retry')">重试失败段</button>
      <a id="export" class="hidden" href="#">导出 JSON</a>
      <span id="status">idle</span>
    </div>
    <div id="error" class="err hidden" style="margin-top:12px"></div>
    <div id="sampleMeta" class="muted" style="margin-top:10px"></div>
  </div>

  <div class="panel">
    <div class="row"><strong>分段状态</strong><span id="progressText" class="muted">0 / 0</span></div>
    <div class="bar" style="margin-top:8px"><i id="progressBar"></i></div>
    <div id="timeline" class="timeline"></div>
    <div id="scaleBasis" class="legend"></div>
  </div>

  <div class="panel">
    <div class="grid">
      <div class="stat">检测替换区间<b id="replacement">-</b></div>
      <div class="stat">真值区间<b id="truth">-</b></div>
      <div class="stat">判定统计<b id="counts">-</b></div>
      <div class="stat">算法取舍<b id="decision">-</b></div>
      <div class="stat">检查点<b id="checkpoint">-</b></div>
    </div>
  </div>

  <div class="panel">
    <div class="row" style="justify-content:space-between"><strong id="frameTitle">帧视图</strong><span class="legend">点击时间轴分段或下方帧号查看</span></div>
    <div class="media" style="margin-top:10px">
      <figure><img id="frameImg" alt="原帧"><figcaption>原帧（含水印）</figcaption></figure>
      <figure><img id="residualImg" alt="残差热力图"><figcaption>相邻帧残差热力图</figcaption></figure>
      <figure><img id="noiseImg" alt="块级噪声方差"><figcaption>块级噪声方差示意</figcaption></figure>
    </div>
  </div>

  <div class="panel">
    <div class="row" style="justify-content:space-between"><strong>逐帧报告</strong><span class="legend">异常分块坐标、摘要、残差、噪声、外接框偏移</span></div>
    <div class="tablewrap" style="margin-top:10px">
      <table><thead><tr><th>帧</th><th>判定</th><th>置信度</th><th>摘要</th><th>残差</th><th>噪声离散度</th><th>外接框偏移</th><th>异常块</th><th>依据</th></tr></thead><tbody id="frames"></tbody></table>
    </div>
  </div>
</main>
<script>
let samples=[], current=null, report=null, job=null, selectedFrame=0, pollTimer=null;
const $=id=>document.getElementById(id);
const esc=v=>String(v??'');
function showError(e){$('error').classList.remove('hidden');$('error').textContent=typeof e==='string'?e:JSON.stringify(e,null,2)}
function clearError(){$('error').classList.add('hidden');$('error').textContent=''}
async function api(url,opt){const r=await fetch(url,opt);const ct=r.headers.get('content-type')||'';const data=ct.includes('json')?await r.json():await r.text();if(!r.ok){throw data.error||data||('HTTP '+r.status)}return data}
async function loadSamples(){
  const data=await api('/api/samples');samples=data.catalog.samples||[];$('sample').innerHTML='';
  for(const s of samples){const o=document.createElement('option');o.value=s.id;o.textContent=s.id+' · '+s.width+'x'+s.height+' · '+s.frame_count+'帧';$('sample').appendChild(o)}
  if(samples.length){$('sample').value=samples[0].id;await selectSample()}
}
async function selectSample(){
  if(pollTimer){clearInterval(pollTimer);pollTimer=null}
  current=$('sample').value;report=null;job=null;selectedFrame=0;clearError();
  const data=await api('/api/samples/'+encodeURIComponent(current));
  const m=data.manifest;$('sampleMeta').textContent=JSON.stringify({sample_id:m.sample_id,size:[m.width,m.height],channels:m.channels,frames:m.frame_count,segment_seconds:m.segment_seconds,truth_replacements:data.truth.replacements},null,0);
  $('budget').value=m.sampling_budget;$('block').value=m.block_size;$('residual').value='0.03';$('noise').value='0.03';$('workers').value='3';
  if(data.job){job=data.job;report=data.job.report;render()}
  else render();
  refreshImages();
}
async function startJob(fresh){
  clearError();const q=new URLSearchParams({mode:$('mode').value,budget:$('budget').value,block:$('block').value,residual_threshold:$('residual').value,noise_threshold:$('noise').value,workers:$('workers').value});
  if(fresh)q.set('fresh','1');
  try{await api('/api/samples/'+encodeURIComponent(current)+'/verify?'+q,{method:'POST'});await poll()}catch(e){showError(e)}
}
async function postAction(a){
  try{const data=await api('/api/samples/'+encodeURIComponent(current)+'/'+a,{method:'POST'});if(data.status){$('status').textContent=data.status}if(a==='resume'||a==='retry')await poll();else await refreshJob()}catch(e){showError(e)}
}
async function poll(){
  if(pollTimer){clearInterval(pollTimer);pollTimer=null}
  await refreshJob();
  pollTimer=setInterval(refreshJob,700);
}
async function refreshJob(){
  try{const data=await api('/api/samples/'+encodeURIComponent(current)+'/job');job=data;if(data.report){report=data.report}render();if(!['running','paused'].includes(data.status)){clearInterval(pollTimer);pollTimer=null}}catch(e){showError(e)}
}
function statusClass(s){return ['committed','verifying','failed','pending'].includes(s)?s:''}
function render(){
  if(!job){$('status').textContent='idle';return}
  $('status').textContent=job.status+(job.error?' · '+job.error.code:'');
  if(job.error){showError(job.error)}
  if(!report){$('timeline').innerHTML='';return}
  const committed=report.committed_segments||0,total=report.total_segments||(report.segment_states||[]).length||0;
  $('progressText').textContent=committed+' / '+total;$('progressBar').style.width=total?(100*committed/total)+'%':'0';
  const states=report.segment_states||[];$('timeline').innerHTML='';
  for(const s of states){const d=document.createElement('div');d.className='seg '+statusClass(s.state);d.textContent='S'+s.index+' ['+s.start_frame+'-'+s.end_frame+'] '+s.state;d.onclick=()=>viewSegment(s.index);$('timeline').appendChild(d)}
  const r=report.replacement||{};const det=r.detected||{};const truth=r.truth||{};
  $('replacement').textContent=(det.start>=0?det.start+'..'+det.end:'none')+(r.within_tolerance?' ✓':'');
  $('truth').textContent=truth.start>=0?truth.start+'..'+truth.end:'none';
  const c=report.counts||{};$('counts').textContent=(c.replaced||0)+' replaced / '+(c.intact||0)+' intact / '+(c.suspect||0)+' suspect';
  const s=report.scale||{};$('decision').textContent=(s.mode||'-')+' · stride '+s.stride+' · block ↓'+(s.block_downsample??1)+' · refined '+(s.refined_frames||[]).length;
  $('scaleBasis').textContent=s.basis||'';
  $('checkpoint').textContent=report.checkpoint_invalidation?('invalidated: '+(report.checkpoint_invalidation.fields||[]).join(', ')):'version matched';
  $('export').classList.remove('hidden');$('export').href='/api/samples/'+encodeURIComponent(current)+'/export';
  renderFrames(report.frames||[]);
  if(!report.frames||selectedFrame>=report.frames.length)selectedFrame=0;
  refreshImages();
}
function renderFrames(frames){
  const body=$('frames');body.innerHTML='';
  for(const f of frames){const tr=document.createElement('tr');tr.className=f.verdict==='replaced'?'replaced':(f.verdict==='suspect'?'suspect':'');
    const blocks=(f.abnormal_blocks||[]).slice(0,4).map(b=>'('+b.col+','+b.row+')').join(' ')+((f.abnormal_blocks||[]).length>4?' …':'');
    const cells=[f.frame,f.verdict||'pending',Number(f.confidence||0).toFixed(3),(f.summary||'').slice(0,14),Number(f.residual_score||0).toFixed(5),Number(f.noise_dispersion||0).toFixed(5),Number(f.bbox_shift_pixels||0).toFixed(1),blocks,(f.reasons||[]).join('; ')];
    for(const v of cells){const td=document.createElement('td');td.textContent=esc(v);tr.appendChild(td)}
    tr.onclick=()=>{selectedFrame=f.frame;refreshImages();$('frameTitle').textContent='帧 #'+f.frame+' · '+(f.verdict||'pending')};body.appendChild(tr)
  }
}
async function viewSegment(i){
  try{const seg=await api('/api/samples/'+encodeURIComponent(current)+'/segment/'+i);renderFrames(seg.frames||[]);if(seg.frames&&seg.frames.length){selectedFrame=seg.frames[0].frame;refreshImages()};$('frameTitle').textContent='分段 S'+i+' · '+seg.start_frame+'..'+seg.end_frame}catch(e){showError(e)}
}
function refreshImages(){
  if(!current)return;const base='/api/samples/'+encodeURIComponent(current);$('frameImg').src=base+'/frame/'+selectedFrame+'.png?t='+Date.now();$('residualImg').src=base+'/residual/'+selectedFrame+'.png?t='+Date.now();$('noiseImg').src=base+'/noise/'+selectedFrame+'.png?t='+Date.now();$('frameTitle').textContent='帧 #'+selectedFrame;
}
$('sample').onchange=selectSample;
loadSamples().catch(showError);
</script>
</body>
</html>`
