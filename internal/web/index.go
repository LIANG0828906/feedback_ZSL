package web

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>MeshForge 诊断工作台</title>
<style>
:root{--bg:#0c1118;--panel:#151d28;--panel2:#1b2634;--line:#2c3a4c;--text:#dbe7f3;--muted:#8ca0b5;--accent:#57b7ff;--ok:#42d392;--warn:#ffc857;--bad:#ff6b6b;--violet:#b18cff}
*{box-sizing:border-box}body{margin:0;background:radial-gradient(circle at 20% 0,#172538 0,#0c1118 45%);color:var(--text);font:13px/1.45 ui-sans-serif,system-ui,"Segoe UI",sans-serif}
header{padding:15px 20px;border-bottom:1px solid var(--line);display:flex;gap:18px;align-items:center;flex-wrap:wrap;background:rgba(12,17,24,.88);position:sticky;top:0;z-index:4;backdrop-filter:blur(9px)}
h1{font-size:17px;margin:0;letter-spacing:.6px}h1 span{color:var(--accent)}
.pill{border:1px solid var(--line);background:var(--panel);padding:4px 9px;border-radius:999px;color:var(--muted)}
.progress{height:7px;width:170px;background:#091019;border:1px solid var(--line);border-radius:9px;overflow:hidden}.progress i{display:block;height:100%;background:linear-gradient(90deg,#2f9dff,#42d392);width:0}
button{background:var(--panel2);color:var(--text);border:1px solid var(--line);border-radius:7px;padding:7px 11px;cursor:pointer;font:inherit}button:hover{border-color:var(--accent);color:#fff}button.primary{background:#15476b;border-color:#2c82b7}button:disabled{opacity:.45;cursor:wait}
main{padding:16px;display:grid;gap:14px;grid-template-columns:minmax(0,1fr) 330px}.viewers{display:grid;grid-template-columns:1fr 1fr;gap:14px;min-width:0}
.panel{background:linear-gradient(180deg,rgba(27,38,52,.95),rgba(19,27,37,.95));border:1px solid var(--line);border-radius:11px;box-shadow:0 10px 30px rgba(0,0,0,.16)}
.panel h2{font-size:12px;text-transform:uppercase;letter-spacing:1.2px;color:var(--muted);margin:0;padding:10px 12px;border-bottom:1px solid var(--line)}
.canvasWrap{position:relative;height:430px;background:#091019}.canvasWrap canvas{width:100%;height:100%;display:block;cursor:grab}.canvasWrap canvas:active{cursor:grabbing}
.badge{position:absolute;left:10px;top:10px;font-size:11px;background:rgba(8,14,21,.72);padding:4px 7px;border-radius:5px;color:var(--muted)}
.legend{position:absolute;right:10px;top:10px;background:rgba(8,14,21,.78);border:1px solid var(--line);border-radius:6px;padding:6px 8px;font-size:11px;line-height:1.7}.dot{display:inline-block;width:8px;height:8px;border-radius:50%;margin-right:5px}
.tools{display:flex;gap:10px;align-items:center;padding:9px 12px;border-top:1px solid var(--line);flex-wrap:wrap}.tools label{color:var(--muted)}
input[type=range]{accent-color:var(--accent);width:130px}select{background:var(--panel2);border:1px solid var(--line);color:var(--text);padding:6px 8px;border-radius:6px}
.side{display:grid;gap:14px;align-content:start}.statGrid{display:grid;grid-template-columns:1fr 1fr;gap:8px;padding:11px}.stat{background:#101925;border:1px solid #243246;border-radius:7px;padding:8px}.stat b{display:block;font-size:16px;color:#fff}.stat small{color:var(--muted)}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid #223044}th{color:var(--muted);font-weight:500}
.status{display:inline-block;padding:2px 6px;border-radius:4px;font-size:10px;text-transform:uppercase;letter-spacing:.5px;border:1px solid #3b4d62}.status.committed{color:var(--ok);border-color:#24664e;background:#10291f}.status.failed{color:var(--bad);border-color:#6d3030;background:#2c1518}.status.processing{color:var(--warn);border-color:#695124;background:#2b2312}.status.pending{color:var(--muted)}
.stageBody{max-height:305px;overflow:auto}.stageBody table td{vertical-align:middle}.mono{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:11px}.muted{color:var(--muted)}
#curve{width:100%;height:150px;display:block;background:#091019}
.decisionPre{max-height:180px;overflow:auto;margin:0;padding:10px 12px;color:#a9bfd4;font-size:11px;white-space:pre-wrap}
.err{color:var(--bad);padding:8px 12px;font-size:12px;white-space:pre-wrap}
@media(max-width:1080px){main{grid-template-columns:1fr}.viewers{grid-template-columns:1fr}.canvasWrap{height:360px}.side{grid-template-columns:1fr 1fr}.side .wide{grid-column:1/-1}}
@media(max-width:700px){.side{grid-template-columns:1fr}header{gap:10px}}
</style>
</head>
<body>
<header>
  <h1>Mesh<span>Forge</span> 三角网格修复工作台</h1>
  <span class="pill" id="sample">sample</span>
  <span class="pill" id="mode">mode</span>
  <span class="pill" id="progressText">0 / 6</span>
  <div class="progress"><i id="progressBar"></i></div>
  <button class="primary" id="runBtn">运行 / 续跑</button>
  <button id="refreshBtn">刷新状态</button>
  <span class="muted" id="runState"></span>
</header>
<main>
  <section class="viewers">
    <div class="panel">
      <h2>缺陷原网格</h2>
      <div class="canvasWrap"><canvas id="left"></canvas><span class="badge" id="leftInfo">original</span>
        <div class="legend"><span class="dot" style="background:#ff6b6b"></span>翻转面<br><span class="dot" style="background:#ffc857"></span>孔洞<br><span class="dot" style="background:#b18cff"></span>薄片<br><span class="dot" style="background:#42d392"></span>非流形</div>
      </div>
    </div>
    <div class="panel">
      <h2>阶段 / 修复网格</h2>
      <div class="canvasWrap"><canvas id="right"></canvas><span class="badge" id="rightInfo">repaired</span></div>
    </div>
  </section>
  <aside class="side">
    <div class="panel wide">
      <h2>阶段状态矩阵</h2>
      <div class="stageBody"><table><thead><tr><th>#</th><th>阶段</th><th>状态</th><th>耗时</th><th></th></tr></thead><tbody id="stageRows"></tbody></table></div>
      <div class="tools">
        <label>查看</label><select id="stageSelect"><option value="repaired">最终修复网格</option></select>
        <label>剖面</label><input id="section" type="range" min="0" max="100" value="100">
        <button id="resetView">复位视角</button>
      </div>
      <div class="err" id="error"></div>
    </div>
    <div class="panel">
      <h2>拓扑统计</h2>
      <div class="statGrid" id="stats"></div>
    </div>
    <div class="panel">
      <h2>拓扑统计曲线</h2>
      <canvas id="curve"></canvas>
    </div>
    <div class="panel">
      <h2>规模取舍记录</h2>
      <pre class="decisionPre" id="decision"></pre>
    </div>
  </aside>
</main>
<script>
const $=id=>document.getElementById(id);
let state=null,leftMesh=null,rightMesh=null,rightFor="repaired";
let view={yaw:0.65,pitch:-0.35,zoom:1};
let scene=null;
const colors={"flipped":"rgba(255,91,107,.92)","hole":"rgba(255,200,87,.92)","thin-sheet":"rgba(177,140,255,.92)","non-manifold":"rgba(66,211,146,.92)"};

function api(url,opt){return fetch(url,opt).then(async r=>{const t=await r.text();let v;try{v=t?JSON.parse(t):{}}catch(e){v={message:t}}if(!r.ok)throw v;return v;});}
function esc(s){return String(s).replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;"}[c]));}

async function refresh(){
  $("error").textContent="";
  state=await api("/api/state");
  $("sample").textContent="样本 "+state.sample_id+" · L"+state.truth.level+" · seed "+state.truth.seed;
  $("mode").textContent="模式 "+state.decision.selected+" · 分块 "+state.decision.chunk.grid;
  $("progressText").textContent=state.progress.committed+" / "+state.progress.total;
  $("progressBar").style.width=(100*state.progress.committed/state.progress.total)+"%";
  $("runState").textContent=state.resume_status+(state.resume_reason?" ("+state.resume_reason+")":"");
  renderStages(); renderStats(); renderCurve(); renderDecision();
  await loadMeshes();
}

async function loadMeshes(){
  leftMesh=await api("/api/mesh?which=original");
  scene=bounds(leftMesh.vertices);
  try{
    if(rightFor==="repaired") rightMesh=await api("/api/mesh?which=repaired");
    else rightMesh=await api("/api/mesh?which=stage&stage="+encodeURIComponent(rightFor));
  }catch(e){rightMesh=null;$("error").textContent=JSON.stringify(e);}
  drawAll();
}

function bounds(vs){if(!vs||!vs.length)return{min:[0,0,0],max:[0,0,0],center:[0,0,0],extent:1};let mn=[Infinity,Infinity,Infinity],mx=[-Infinity,-Infinity,-Infinity];for(const v of vs){for(let i=0;i<3;i++){if(v[i]<mn[i])mn[i]=v[i];if(v[i]>mx[i])mx[i]=v[i];}}let ext=Math.max(mx[0]-mn[0],mx[1]-mn[1],mx[2]-mn[2])||1;return{min:mn,max:mx,center:[(mn[0]+mx[0])/2,(mn[1]+mx[1])/2,(mn[2]+mx[2])/2],extent:ext};}

function renderStages(){
  const rows=[];
  for(const st of state.stages){
    let btn="";
    if(st.status==="failed")btn='<button onclick="retryStage('+st.index+')">重试</button>';
    rows.push('<tr><td>'+st.index+'</td><td>'+esc(st.name)+'</td><td><span class="status '+st.status+'">'+st.status+'</span></td><td class="mono">'+st.duration_ms+'ms</td><td>'+btn+'</td></tr>');
  }
  $("stageRows").innerHTML=rows.join("");
  const sel=$("stageSelect"); const cur=sel.value;
  sel.innerHTML='<option value="repaired">最终修复网格</option>'+state.stages.filter(s=>s.status==="committed"&&s.name!=="stats").map(s=>'<option value="'+s.name+'">'+s.name+'</option>').join("");
  sel.value=cur&&Array.from(sel.options).some(o=>o.value===cur)?cur:"repaired";
}

function renderStats(){
  const s=state.final_stats||{};
  const items=[["顶点",s.vertices??"-"],["面片",s.faces??"-"],["边",s.edges??"-"],["欧拉 χ",s.euler_characteristic??"-"],["非流形边",s.non_manifold_edges??"-"],["边界环",s.boundary_loops??"-"],["重复顶点",s.duplicate_vertices??"-"],["自相交",s.self_intersections??"-"]];
  $("stats").innerHTML=items.map(x=>'<div class="stat"><b>'+x[1]+'</b><small>'+x[0]+'</small></div>').join("");
}

function renderCurve(){
  const c=$("curve"),dpr=window.devicePixelRatio||1;const w=c.clientWidth||300,h=c.clientHeight||150;
  c.width=w*dpr;c.height=h*dpr;const g=c.getContext("2d");g.scale(dpr,dpr);g.clearRect(0,0,w,h);
  g.strokeStyle="#223044";g.lineWidth=1;for(let i=0;i<4;i++){let y=18+i*(h-35)/3;g.beginPath();g.moveTo(35,y);g.lineTo(w-10,y);g.stroke();}
  const pts=state.topology_curve||[];if(!pts.length)return;
  const max=Math.max(...pts.map(p=>Math.max(p.vertices,p.faces,p.edges,1)));
  const x=i=>35+(w-48)*(i/Math.max(1,pts.length-1));
  const y=v=>h-17-(h-38)*(v/max);
  const series=[["vertices","#57b7ff"],["faces","#42d392"],["edges","#b18cff"]];
  for(const [key,col] of series){g.strokeStyle=col;g.fillStyle=col;g.beginPath();pts.forEach((p,i)=>{const yy=y(p[key]);if(i===0)g.moveTo(x(i),yy);else g.lineTo(x(i),yy);});g.stroke();pts.forEach((p,i)=>{g.beginPath();g.arc(x(i),y(p[key]),2.5,0,Math.PI*2);g.fill();});}
  g.fillStyle="#8ca0b5";g.font="10px sans-serif";g.fillText("V",8,h-18);g.fillText("F",8,h-33);g.fillText("E",8,h-48);
  pts.forEach((p,i)=>{g.fillText(p.stage.slice(0,3),x(i)-9,h-3);});
}

function renderDecision(){
  const d=state.decision;const reasons=d.candidates.map(c=>c.mode+": feasible="+c.feasible+" | "+c.reasons.join("; ")).join("\n");
  $("decision").textContent=JSON.stringify({mode:d.selected,requested:d.requested,rule:d.rule,faces:d.face_count,components:d.component_count,chunk:d.chunk,time_budget_ms:d.time_budget_ms,memory_budget_mb:d.memory_budget_mb,per_worker_buffer_mb:d.per_worker_buffer_mb,max_parallel:d.max_parallel},null,2)+"\n\n"+reasons;
}

function setupCanvas(id){
  const c=$(id);
  let drag=null;
  c.addEventListener("mousedown",e=>{drag={x:e.clientX,y:e.clientY};});
  window.addEventListener("mouseup",()=>{drag=null;});
  window.addEventListener("mousemove",e=>{if(!drag)return;view.yaw+=(e.clientX-drag.x)*.009;view.pitch+=(e.clientY-drag.y)*.009;drag={x:e.clientX,y:e.clientY};drawAll();});
  c.addEventListener("wheel",e=>{e.preventDefault();view.zoom*=e.deltaY>0?.92:1.08;view.zoom=Math.max(.15,Math.min(8,view.zoom));drawAll();},{passive:false});
}

function drawAll(){drawMesh($("left"),leftMesh,true);drawMesh($("right"),rightMesh,false);}

function drawMesh(c,payload,isLeft){
  const dpr=window.devicePixelRatio||1,w=c.clientWidth,h=c.clientHeight;
  if(!w||!h)return;if(c.width!==Math.round(w*dpr)||c.height!==Math.round(h*dpr)){c.width=Math.round(w*dpr);c.height=Math.round(h*dpr);}
  const g=c.getContext("2d");g.setTransform(dpr,0,0,dpr,0,0);g.clearRect(0,0,w,h);
  if(!payload){g.fillStyle="#8ca0b5";g.font="13px sans-serif";g.fillText("该阶段尚未提交",18,30);return;}
  $("leftInfo").textContent="original · "+leftMesh.faces.length+" faces";
  if(!isLeft)$("rightInfo").textContent=(rightMesh.stage||rightMesh.which)+" · "+payload.faces.length+" faces";
  const b=scene||bounds(payload.vertices);
  const scale=Math.min(w,h)*.78/b.extent*view.zoom;
  const cx=w/2,cy=h/2;
  const cyaw=Math.cos(view.yaw),syaw=Math.sin(view.yaw),cp=Math.cos(view.pitch),sp=Math.sin(view.pitch);
  const cutRatio=Number($("section").value)/100;
  const cut=b.min[2]+(b.max[2]-b.min[2])*cutRatio+1e-12;
  const project=p=>{const x=p[0]-b.center[0],y=p[1]-b.center[1],z=p[2]-b.center[2];const x1=x*cyaw+z*syaw,z1=-x*syaw+z*cyaw;const y1=y*cp-z1*sp,z2=y*sp+z1*cp;return [cx+x1*scale,cy-y1*scale,z2,x1,y1,z2];};
  const hl={};if(isLeft)for(const x of payload.highlights)hl[x.face]=x.kind;
  const maxDraw=22000,stride=Math.max(1,Math.ceil(payload.faces.length/maxDraw));
  const light=[.35,.5,.79];let draws=0,lastZ=null;
  function polyFace(fi,forceColor){
    const f=payload.faces[fi];let poly=[payload.vertices[f[0]],payload.vertices[f[1]],payload.vertices[f[2]]];
    if(cutRatio<1)poly=clipZ(poly,cut);if(poly.length<3)return;
    const a=poly[0],bb=poly[1],cc=poly[2];const u=[bb[0]-a[0],bb[1]-a[1],bb[2]-a[2]],v=[cc[0]-a[0],cc[1]-a[1],cc[2]-a[2]];let n=[u[1]*v[2]-u[2]*v[1],u[2]*v[0]-u[0]*v[2],u[0]*v[1]-u[1]*v[0]];const nl=Math.hypot(n[0],n[1],n[2])||1;n=[n[0]/nl,n[1]/nl,n[2]/nl];const shade=Math.max(.15,Math.abs(n[0]*light[0]+n[1]*light[1]+n[2]*light[2]));
    g.beginPath();for(let i=0;i<poly.length;i++){const q=project(poly[i]);if(i===0)g.moveTo(q[0],q[1]);else g.lineTo(q[0],q[1]);}g.closePath();
    if(forceColor){g.fillStyle=forceColor;g.globalAlpha=.76;g.fill();g.globalAlpha=1;g.strokeStyle=forceColor;g.lineWidth=1.2;g.stroke();}
    else{g.fillStyle="rgba("+Math.round(55+100*shade)+","+Math.round(105+110*shade)+","+Math.round(145+100*shade)+",.56)";g.fill();}
    draws++;
  }
  if(isLeft)for(const fi in hl)polyFace(Number(fi),colors[hl[fi]]);
  for(let fi=0;fi<payload.faces.length;fi+=stride){if(isLeft&&hl[fi])continue;polyFace(fi,null);}
  g.fillStyle="#8ca0b5";g.font="10px sans-serif";g.fillText(draws+" tris drawn / "+payload.faces.length,10,h-9);
}

function clipZ(poly,cut){
  const out=[];
  for(let i=0;i<poly.length;i++){
    const a=poly[i],b=poly[(i+1)%poly.length],ain=a[2]<=cut,bin=b[2]<=cut;
    if(ain)out.push(a);
    if(ain!==bin){const t=(cut-a[2])/(b[2]-a[2]);out.push([a[0]+(b[0]-a[0])*t,a[1]+(b[1]-a[1])*t,cut]);}
  }
  return out;
}

async function retryStage(i){
  $("error").textContent="";
  try{await api("/api/retry",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({index:i})});await refresh();}
  catch(e){$("error").textContent=JSON.stringify(e,null,2);}
}

async function runPipeline(){
  $("error").textContent="";$("runBtn").disabled=true;
  try{await api("/api/run",{method:"POST"});await refresh();}
  catch(e){$("error").textContent=JSON.stringify(e,null,2);await refresh();}
  finally{$("runBtn").disabled=false;}
}

$("runBtn").onclick=runPipeline;
$("refreshBtn").onclick=()=>refresh().catch(e=>$("error").textContent=JSON.stringify(e,null,2));
$("resetView").onclick=()=>{view={yaw:.65,pitch:-.35,zoom:1};drawAll();};
$("section").oninput=()=>drawAll();
$("stageSelect").onchange=async e=>{rightFor=e.target.value;$("error").textContent="";try{await loadMeshes();}catch(err){$("error").textContent=JSON.stringify(err,null,2);}};
setupCanvas("left");setupCanvas("right");
refresh().catch(e=>{$("error").textContent=JSON.stringify(e,null,2);});
window.addEventListener("resize",()=>{renderCurve();drawAll();});
</script>
</body>
</html>`
