package server

const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>QualityBench diagnostics</title>
<style>
:root{--bg:#0e1218;--panel:#171e27;--line:#293442;--fg:#dce6f2;--muted:#8ca0b5;--accent:#5cc8a4;--warn:#e4b45f;--bad:#e66b6b}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.45 ui-monospace,SFMono-Regular,Consolas,monospace}
header{display:flex;gap:14px;align-items:center;padding:14px 20px;border-bottom:1px solid var(--line);position:sticky;top:0;background:#0e1218ee;backdrop-filter:blur(8px);z-index:5}
h1{font-size:17px;margin:0;font-weight:600}.grow{flex:1}.muted{color:var(--muted)}
.badge{border:1px solid var(--line);border-radius:999px;padding:2px 9px;font-size:12px}.ok{color:var(--accent);border-color:#2f6f5d}.warn{color:var(--warn);border-color:#6d5a31}.bad{color:var(--bad);border-color:#703d3d}
main{padding:18px;display:grid;gap:16px;grid-template-columns:minmax(0,1.5fr) minmax(320px,.8fr)}
section{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px;min-width:0}
h2{font-size:13px;text-transform:uppercase;letter-spacing:.08em;color:var(--muted);margin:0 0 12px}
button,select,input{background:#111820;color:var(--fg);border:1px solid var(--line);border-radius:6px;padding:6px 9px;font:inherit}
button{cursor:pointer}button:hover{border-color:#466077}button.primary{border-color:#32785f;background:#173027}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}.controls{grid-column:1/-1;display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.figs{display:grid;grid-template-columns:1fr 1fr;gap:12px}.figure{background:#0d131a;border:1px solid var(--line);border-radius:8px;padding:8px}
.figure img{width:100%;image-rendering:pixelated;display:block;border-radius:4px;background:#000}.figure .cap{display:flex;justify-content:space-between;margin-bottom:6px;color:var(--muted);font-size:12px}
.heatmap{display:grid;gap:2px;margin-top:8px}.cell{aspect-ratio:1;border-radius:2px;position:relative;cursor:crosshair;min-width:8px}.cell span{position:absolute;inset:0;display:grid;place-items:center;font-size:9px;color:#fff;text-shadow:0 0 2px #000}
.legend{display:flex;gap:12px;flex-wrap:wrap;color:var(--muted);font-size:12px;margin-top:8px}.swatch{width:10px;height:10px;display:inline-block;border-radius:2px;margin-right:4px;vertical-align:middle}
.stats{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px}.stat{background:#111820;border:1px solid var(--line);border-radius:7px;padding:9px}.stat b{display:block;font-size:18px;color:#fff}.stat span{color:var(--muted);font-size:11px}
table{width:100%;border-collapse:collapse;font-size:12px}td,th{padding:5px 6px;border-bottom:1px solid var(--line);text-align:left}th{color:var(--muted);font-weight:500}
.list{max-height:220px;overflow:auto;border:1px solid var(--line);border-radius:7px}.item{padding:6px 8px;border-bottom:1px solid #202a36;font-size:12px}.item:last-child{border-bottom:0}
.steps{display:flex;gap:4px;flex-wrap:wrap}.stepcol{background:#111820;border:1px solid var(--line);border-radius:5px;padding:5px 7px;min-width:74px}.stepcol b{display:block;color:#fff}.stepcol small{color:var(--muted)}
#detail{font-size:12px;color:var(--muted);min-height:38px}.err{color:var(--bad);white-space:pre-wrap}.path-ok{color:var(--accent)}.path-bad{color:var(--bad)}
@media(max-width:900px){main{grid-template-columns:1fr}.figs{grid-template-columns:1fr}}
</style>
</head>
<body>
<header>
  <h1>QualityBench</h1>
  <span id="status" class="badge">loading</span>
  <span id="msg" class="muted"></span>
  <span class="grow"></span>
  <span class="muted" id="clock"></span>
</header>
<main>
  <section class="controls">
    <button class="primary" onclick="control('pause')">暂停</button>
    <button onclick="control('resume')">继续</button>
    <button onclick="control('cancel')">取消</button>
    <button onclick="startRun()">重新运行</button>
    <span class="muted">图像</span><select id="imageSel" onchange="selectImage(this.value)"></select>
    <span class="muted">步骤</span><input id="stepSel" type="range" min="0" max="5" value="0" oninput="selectStep(+this.value)">
    <span id="stepLabel" class="muted">baseline</span>
    <span class="muted">热力图</span>
    <select id="metricSel" onchange="render()">
      <option value="noise">噪声方差</option>
      <option value="edge">边缘清晰度</option>
      <option value="chroma">色度偏移</option>
    </select>
  </section>

  <section>
    <h2>基准图与逐步劣化</h2>
    <div class="figs">
      <div class="figure"><div class="cap"><span>baseline</span><span id="baseCap"></span></div><img id="baseImg" alt="baseline"></div>
      <div class="figure"><div class="cap"><span id="degTitle">degraded</span><span id="degCap"></span></div><img id="degImg" alt="degraded"></div>
    </div>
    <div class="legend" id="legend"></div>
  </section>

  <section>
    <h2>逐块质量指标 / 首贡献归因</h2>
    <div id="heatmap" class="heatmap"></div>
    <div id="detail" style="margin-top:10px"></div>
  </section>

  <section>
    <h2>运行与缓存</h2>
    <div class="stats" id="stats"></div>
  </section>

  <section>
    <h2>失效传播</h2>
    <div class="steps" id="stepSummary"></div>
    <div class="list" id="invalidationList" style="margin-top:10px"></div>
  </section>

  <section>
    <h2>三路径比对与独立验证</h2>
    <table id="paths"></table>
  </section>

  <section>
    <h2>逐块重试</h2>
    <div class="row">
      <span class="muted">step</span><input id="retryStep" type="number" min="1" value="1" style="width:70px">
      <span class="muted">x</span><input id="retryX" type="number" min="0" value="0" style="width:80px">
      <span class="muted">y</span><input id="retryY" type="number" min="0" value="0" style="width:80px">
      <button onclick="retryBlock()">重试该块</button>
    </div>
    <div id="retryResult" style="margin-top:10px" class="muted"></div>
  </section>
</main>
<script>
let S=null, selectedImage=null, selectedStep=null, selectedEval=null;
const CAT={noise:'N',blur:'B',blockiness:'K',chroma_shift:'C',overexpose:'O',none:'-'};
const CAT_COLOR={noise:'#b84d4d',blur:'#3e78b8',blockiness:'#9168b3',chroma_shift:'#3d9b78',overexpose:'#c2843d',none:'#5b6470'};
async function api(path,opt){const r=await fetch(path,opt);const ct=r.headers.get('content-type')||'';if(!r.ok){let e;try{e=await r.json()}catch(_){e={message:await r.text()}}throw e}return ct.includes('json')?r.json():r.text()}
async function poll(){try{S=await api('/api/state');render()}catch(e){document.getElementById('msg').textContent=JSON.stringify(e);document.getElementById('msg').className='err'}finally{setTimeout(poll,700)}}
function ensureSelection(){if(!S)return;if(!selectedImage||!S.images.find(i=>i.id===selectedImage))selectedImage=S.images[0]?.id;const img=S.images.find(i=>i.id===selectedImage);if(!img)return;const avail=img.available_steps||[0];if(selectedStep===null||!avail.includes(selectedStep))selectedStep=Math.max(...avail);const sel=document.getElementById('imageSel');if(sel.options.length!==S.images.length||sel.dataset.sig!==S.images.map(i=>i.id).join(',')){sel.innerHTML='';sel.dataset.sig=S.images.map(i=>i.id).join(',');for(const i of S.images){const o=document.createElement('option');o.value=i.id;o.textContent=i.id;sel.appendChild(o)}}sel.value=selectedImage;const range=document.getElementById('stepSel');const maxStep=Math.max(...avail);range.max=maxStep;if(selectedStep>maxStep)selectedStep=maxStep;range.value=selectedStep;range.disabled=maxStep===0;document.getElementById('stepLabel').textContent=selectedStep===0?'baseline':'step '+selectedStep}
function render(){if(!S)return;const st=document.getElementById('status');st.textContent=S.status+(S.paused?' / paused':'');st.className='badge '+(S.status==='completed'?'ok':(S.status==='failed'||S.status==='canceled'?'bad':'warn'));document.getElementById('msg').textContent=S.message||'';document.getElementById('clock').textContent=new Date().toLocaleTimeString();ensureSelection();const img=S.images.find(i=>i.id===selectedImage);if(!img)return;const q=new URLSearchParams({image:selectedImage,step:selectedStep});const cache='&t='+Date.now();document.getElementById('baseImg').src='/api/image?image='+encodeURIComponent(selectedImage)+'&step=0'+cache;document.getElementById('baseCap').textContent=(S.width||'')+'x'+(S.height||'');document.getElementById('degTitle').textContent=selectedStep===0?'degraded (same)':'step '+selectedStep;document.getElementById('degImg').src='/api/image?image='+encodeURIComponent(selectedImage)+'&step='+selectedStep+cache;document.getElementById('degCap').textContent=(img.traces&&img.traces[selectedStep-1])?img.traces[selectedStep-1].type:'';
 renderStats();renderHeatmap(img);renderInvalidations();renderPaths()}
function renderStats(){const c=S.cache||{};const maxWait=Math.max(0,...(S.buffer_waits||[]).map(w=>w.duration_ns||0),...(S.queue_waits||[]).map(w=>w.duration_ns||0));const prog=Object.values(S.progress||{}).reduce((a,p)=>a+(p[0]||0),0);const total=Object.values(S.progress||{}).reduce((a,p)=>a+(p[1]||0),0);document.getElementById('stats').innerHTML=[
 ['cache hits',c.hits||0],['cache misses',c.misses||0],['persistent hits',c.persistent_hits||0],['reused blocks',c.unaffected_reused||0],
 ['progress',prog+' / '+total],['buffer waits',(S.buffer_waits||[]).length],['queue waits',(S.queue_waits||[]).length],['max wait ms',(maxWait/1e6).toFixed(3)]
].map(([k,v])=>'<div class="stat"><b>'+v+'</b><span>'+k+'</span></div>').join('')}
function metricValue(m,kind){if(kind==='noise')return Math.log1p(Math.max(0,m.noise_variance||0));if(kind==='edge')return Math.max(0,m.edge_sharpness||0);return Math.max(0,m.chroma_shift||0)}
function heatColor(v,max,kind){const t=max>0?Math.min(1,v/max):0;if(kind==='noise')return 'rgb('+Math.round(30+220*t)+','+Math.round(55+70*(1-t))+','+Math.round(80+20*(1-t))+')';if(kind==='edge')return 'rgb('+Math.round(30+50*(1-t))+','+Math.round(60+150*t)+','+Math.round(80+120*t)+')';return 'rgb('+Math.round(40+170*t)+','+Math.round(70+70*t)+','+Math.round(90+140*(1-t))+')'}
function renderHeatmap(img){const el=document.getElementById('heatmap');const evals=img.evaluations||[];const kind=document.getElementById('metricSel').value;const cols=Math.max(1,Math.round((S.width||1)/(S.blockSize||1)));el.style.gridTemplateColumns='repeat('+cols+',minmax(8px,1fr))';el.innerHTML='';if(!evals.length){el.innerHTML='<div class="muted">指标将在当前运行完成后出现。</div>';document.getElementById('detail').textContent='';return}const vals=evals.map(e=>metricValue(e.metrics,kind));const max=Math.max(...vals,1e-9);const by={};for(const e of evals)by[e.rect.y+','+e.rect.x]=e;for(let y=0;y<Math.ceil((S.height||1)/(S.blockSize||1));y++){for(let x=0;x<cols;x++){const e=by[(y*S.blockSize)+','+(x*S.blockSize)];const d=document.createElement('div');d.className='cell';if(!e){el.appendChild(d);continue}const v=metricValue(e.metrics,kind);d.style.background=heatColor(v,max,kind);const cat=e.attribution.category||'none';const sp=document.createElement('span');sp.textContent=CAT[cat]||'?';sp.style.background=CAT_COLOR[cat]||'#5b6470';d.title='block '+e.rect.x+','+e.rect.y+'  noise='+e.metrics.noise_variance.toFixed(4)+' edge='+e.metrics.edge_sharpness.toFixed(4)+' chroma='+e.metrics.chroma_shift.toFixed(4)+'  attribution='+cat+' step '+e.attribution.step;d.onclick=()=>{selectedEval=e;document.getElementById('retryX').value=e.rect.x;document.getElementById('retryY').value=e.rect.y;if(e.attribution.step>0)document.getElementById('retryStep').value=e.attribution.step;renderDetail(e)};el.appendChild(d)}}const first=selectedEval&&by[selectedEval.rect.y+','+selectedEval.rect.x]||evals[0];renderDetail(first);document.getElementById('legend').innerHTML=(S.width?('<span><i class="swatch" style="background:#b84d4d"></i>N noise</span><span><i class="swatch" style="background:#3e78b8"></i>B blur</span><span><i class="swatch" style="background:#9168b3"></i>K blockiness</span><span><i class="swatch" style="background:#3d9b78"></i>C chroma</span><span><i class="swatch" style="background:#c2843d"></i>O overexpose</span>'):'')}
function renderDetail(e){const d=document.getElementById('detail');if(!e){d.textContent='';return}selectedEval=e;d.innerHTML='<b>block '+e.rect.x+','+e.rect.y+'</b> &nbsp; noise='+e.metrics.noise_variance.toFixed(5)+' &nbsp; edge='+e.metrics.edge_sharpness.toFixed(5)+' &nbsp; chroma='+e.metrics.chroma_shift.toFixed(5)+'<br>首贡献步骤 '+e.attribution.step+' / '+e.attribution.category+' &nbsp; share='+(100*e.attribution.share).toFixed(1)+'% total='+e.attribution.total_score.toFixed(3)}
function renderInvalidations(){const inv=S.invalidations||[];const groups={};for(const x of inv){const k=x.step+'|'+(x.reason||'unknown');groups[k]=(groups[k]||0)+1}const cols=Object.entries(groups).sort((a,b)=>parseInt(a[0])-parseInt(b[0]));document.getElementById('stepSummary').innerHTML=cols.length?cols.map(([k,v])=>{const [step,reason]=k.split('|');return '<div class="stepcol"><b>step '+step+'</b><small>'+v+' blocks</small><br><small>'+reason+'</small></div>'}).join(''):'<span class="muted">等待失效演示或逐块重试。</span>';const list=document.getElementById('invalidationList');list.innerHTML='';for(const x of inv.slice(0,120)){const d=document.createElement('div');d.className='item';d.textContent='step '+x.step+' · block '+(x.block?x.block.x+','+x.block.y:'?')+' · '+(x.reason||'');list.appendChild(d)}if(inv.length>120){const d=document.createElement('div');d.className='item muted';d.textContent='... '+(inv.length-120)+' more';list.appendChild(d)}}
function renderPaths(){const t=document.getElementById('paths');const rows=['live','resume','full'];let html='<tr><th>path</th><th>status</th><th>identical</th><th>verified</th><th>first diff</th></tr>';for(const k of rows){const p=(S.paths||{})[k]||{};const fd=p.first_diff?('step '+p.first_diff.step+' block '+(p.first_diff.block?p.first_diff.block.x+','+p.first_diff.block.y:'')+' '+p.first_diff.field):'';const vk='serve-'+k;const verified=(S.verification||{})[vk]?'yes':'no';html+='<tr><td>'+k+'</td><td class="'+(p.status==='completed'?'path-ok':(p.status==='mismatch'||p.status==='failed'?'path-bad':''))+'">'+(p.status||'pending')+'</td><td>'+(p.identical?'yes':'no')+'</td><td>'+verified+'</td><td>'+fd+'</td></tr>'}t.innerHTML=html}
function selectImage(id){selectedImage=id;selectedStep=null;selectedEval=null;render()}
function selectStep(v){selectedStep=v;selectedEval=null;render()}
async function control(action){try{await api('/api/control?action='+encodeURIComponent(action),{method:'POST'});S=await api('/api/state');render()}catch(e){document.getElementById('msg').textContent=JSON.stringify(e);document.getElementById('msg').className='err'}}
async function startRun(){try{await api('/api/start',{method:'POST'});selectedStep=null;selectedEval=null;S=await api('/api/state');render()}catch(e){document.getElementById('msg').textContent=JSON.stringify(e);document.getElementById('msg').className='err'}}
async function retryBlock(){const step=+document.getElementById('retryStep').value;const x=+document.getElementById('retryX').value;const y=+document.getElementById('retryY').value;const q=new URLSearchParams({image:selectedImage,step,x,y});try{const r=await api('/api/retry?'+q,{method:'POST'});document.getElementById('retryResult').textContent='version '+r.version+', recomputed '+r.recomputed_blocks+', invalidations '+r.invalidations.length;selectedEval=null;S=await api('/api/state');render()}catch(e){document.getElementById('retryResult').textContent=JSON.stringify(e)}}
poll();
</script>
</body>
</html>`
