package web

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>MaskQC 笔画拓扑方向核验工作台</title>
<style>
:root{--bg:#0d121b;--panel:#151d2a;--line:#273449;--text:#e8eef8;--muted:#8fa0b8;--accent:#4ed6a9;--warn:#f3b45b;--bad:#ff7b7b}
*{box-sizing:border-box}body{margin:0;background:radial-gradient(circle at 20% 0,#1a2638 0,var(--bg) 45%);color:var(--text);font:14px/1.5 ui-sans-serif,system-ui,"Noto Sans SC",sans-serif}
header{display:flex;gap:12px;align-items:center;flex-wrap:wrap;padding:18px 24px;border-bottom:1px solid var(--line);background:rgba(13,18,27,.85);position:sticky;top:0;z-index:3;backdrop-filter:blur(10px)}
h1{font-size:18px;margin:0 18px 0 0;letter-spacing:.04em}h2{font-size:15px;margin:0 0 12px;color:var(--muted);font-weight:600}
button,select{background:#1f2b3d;color:var(--text);border:1px solid #33445e;border-radius:8px;padding:8px 12px;font:inherit}
button:hover,select:hover{border-color:var(--accent);cursor:pointer}main{padding:22px 24px 60px;max-width:1500px;margin:0 auto}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:16px}.panel{background:linear-gradient(180deg,#172131,#131b27);border:1px solid var(--line);border-radius:14px;padding:16px;margin-bottom:16px;min-width:0}
.visuals{display:grid;grid-template-columns:repeat(auto-fit,minmax(250px,1fr));gap:16px}.visuals figure{margin:0}.visuals img{width:100%;max-height:360px;object-fit:contain;image-rendering:pixelated;background:#0a0f16;border:1px solid var(--line);border-radius:10px}
figcaption{color:var(--muted);font-size:12px;margin-top:7px}.badge{display:inline-block;border:1px solid #38506c;border-radius:999px;padding:3px 9px;color:var(--muted);font-size:12px;margin-right:6px}
.ok{color:var(--accent)}.warn{color:var(--warn)}.bad{color:var(--bad)}table{width:100%;border-collapse:collapse;font-size:12.5px}th,td{text-align:left;border-bottom:1px solid #223044;padding:6px 8px;vertical-align:top}th{color:var(--muted);font-weight:600}code,.mono{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:12px}
.strokes{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:12px}.strokes figure{margin:0;background:#101722;border:1px solid var(--line);border-radius:10px;padding:8px}.strokes img{width:100%;image-rendering:pixelated;background:#0a0f16}
.two{display:grid;grid-template-columns:1.1fr .9fr;gap:16px}@media(max-width:900px){.two{grid-template-columns:1fr}}
#graph{width:100%;min-height:220px;background:#0c131d;border:1px solid var(--line);border-radius:10px}#status{color:var(--muted);margin-left:auto}
.issue{border-left:3px solid var(--warn);padding:8px 12px;background:#251f17;border-radius:6px;margin-bottom:8px}.issue.bad{border-color:var(--bad);background:#29191b}
.scroll{overflow:auto;max-height:360px}.muted{color:var(--muted)}.kv{display:flex;gap:18px;flex-wrap:wrap}.kv div{min-width:120px}.kv strong{display:block;font-size:18px;color:var(--accent)}
</style>
</head>
<body>
<header>
  <h1>MaskQC · 笔画拓扑方向核验</h1>
  <select id="project" aria-label="工程"></select>
  <button id="rerun">全量重算</button>
  <button id="edit">编辑首笔：X1+3 / 硬度 82</button>
  <span id="status" class="muted">加载中</span>
</header>
<main>
  <section class="panel">
    <div class="kv" id="overview"></div>
  </section>
  <section class="visuals">
    <figure><img id="candidate" alt="候选掩码"><figcaption>候选掩码：内缩、外溢与小空洞</figcaption></figure>
    <figure><img id="accumulated" alt="积算掩码"><figcaption>积算掩码：逐笔覆盖与 alpha-over 合成</figcaption></figure>
    <figure><img id="icon" alt="图标"><figcaption>确定性生成图标：色块、渐变、半透明描边与噪声</figcaption></figure>
  </section>
  <section class="panel" id="issuesPanel"><h2>核验问题</h2><div id="issues"></div></section>
  <section class="two">
    <div class="panel"><h2>连通域、孔洞、环带宽与方向</h2><div class="scroll"><table id="domains"><thead><tr><th>域</th><th>面积</th><th>重心</th><th>外接框</th><th>孔洞</th><th>环宽</th><th>方向类别</th></tr></thead><tbody></tbody></table></div></div>
    <div class="panel"><h2>失效传播</h2><pre id="invalidation" class="mono scroll muted" style="margin:0"></pre><svg id="graph"></svg></div>
  </section>
  <section class="panel"><h2>逐笔覆盖分量</h2><div id="strokes" class="strokes"></div></section>
  <section class="two">
    <div class="panel"><h2>预算占用与排队</h2><div id="budget"></div></div>
    <div class="panel"><h2>分块提交序列</h2><div id="blocks" class="scroll"></div></div>
  </section>
</main>
<script>
var state={project:null,data:null};
function q(id){return document.getElementById(id)}
function esc(v){return String(v==null?"":v).replace(/[&<>"]/g,function(c){return {"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;"}[c]})}
function json(v){return JSON.stringify(v,null,2)}
function setStatus(text,cls){q("status").textContent=text;q("status").className=cls||"muted"}
async function loadProjects(){
  var res=await fetch("/api/projects");
  var data=await res.json();
  var select=q("project");
  select.innerHTML="";
  data.projects.forEach(function(p){
    var option=document.createElement("option");
    option.value=p.id;
    option.textContent=p.id+" · "+p.width+"×"+p.height+" · "+p.summary.domainCount+" 域";
    select.appendChild(option);
  });
  if(data.projects.length){state.project=data.projects[0].id;select.value=state.project;await loadProject(state.project)}
}
async function loadProject(id){
  setStatus("加载 "+id,"muted");
  state.project=id;
  var res=await fetch("/api/project/"+encodeURIComponent(id));
  if(!res.ok){setStatus("加载失败","bad");return}
  var data=await res.json();
  state.data=data;
  q("candidate").src="/img/"+encodeURIComponent(id)+"/candidate.png?t="+Date.now();
  q("accumulated").src="/img/"+encodeURIComponent(id)+"/accumulated.png?t="+Date.now();
  q("icon").src="/img/"+encodeURIComponent(id)+"/icon.png?t="+Date.now();
  renderOverview(data);
  renderIssues(data.issues||[]);
  renderDomains(data);
  renderStrokes(data);
  renderInvalidation(data);
  renderBudget(data);
  renderBlocks(data);
  var issueCount=(data.issues||[]).length;
  setStatus(issueCount?("完成，"+issueCount+" 个核验问题"):"完成，核验通过",issueCount?"warn":"ok");
}
function renderOverview(data){
  var s=data.summary||{};
  var domains=(data.domains&&data.domains.domains)||[];
  var directions=domains.map(function(d){return d.id+":"+d.direction}).join(" · ");
  q("overview").innerHTML=[
    ["工程",esc(data.id)],["画布",data.width+"×"+data.height],
    ["源版本",esc(data.sourceVersion)],["版本", (data.versions||[]).join(" / ")],
    ["连通域",String(s.domainCount||domains.length)],["掩码哈希",String(s.accumulatedHash||"").slice(0,16)],
    ["提交块",String((data.commitSequence||[]).length)],["复用块",String((data.reusedBlocks||[]).length)],
    ["方向摘要",esc(directions)],["耗时",String(s.elapsedMillis||0)+" ms"]
  ].map(function(item){return "<div><span class='muted'>"+item[0]+"</span><strong>"+item[1]+"</strong></div>"}).join("");
}
function renderIssues(issues){
  var root=q("issues");
  if(!issues.length){root.innerHTML="<span class='ok'>E_TOP / E_DIR 均未触发。三路径与真值核验通过。</span>";return}
  root.innerHTML=issues.map(function(issue){
    var bad=issue.code==="E_TOP"||issue.code==="E_DIR";
    return "<div class='issue "+(bad?"bad":"")+"'><strong>"+esc(issue.code)+"</strong> · 域 "+esc(issue.domain||"-")+" · "+esc(issue.objectId||"")+"<br>"+esc(issue.message||"")+"<br><span class='mono'>"+esc(issue.path||"")+"</span> <span class='muted'>实测 "+esc(issue.measured||"")+"</span></div>";
  }).join("");
}
function renderDomains(data){
  var body=q("domains").querySelector("tbody");
  var domains=(data.domains&&data.domains.domains)||[];
  body.innerHTML=domains.map(function(d){
    return "<tr><td><strong>"+esc(d.id)+"</strong></td><td>"+d.area+"</td><td>"+d.centroidX.toFixed(2)+", "+d.centroidY.toFixed(2)+"</td><td class='mono'>["+d.bbox.minX+","+d.bbox.minY+","+d.bbox.maxX+","+d.bbox.maxY+"]</td><td>"+d.holes+"</td><td>"+d.ringWidth+"</td><td><span class='badge ok'>"+esc(d.direction)+"</span></td></tr>";
  }).join("");
}
function renderStrokes(data){
  var root=q("strokes");
  root.innerHTML=(data.strokes||[]).map(function(s){
    return "<figure><img src='/img/"+encodeURIComponent(data.id)+"/stroke/"+encodeURIComponent(s.id)+"' alt='"+esc(s.id)+"'><figcaption><strong>"+esc(s.id)+"</strong> · seq "+s.seq+" · 优先级 "+s.priority+"<br><span class='muted'>"+esc(s.direction)+" · 硬度 "+s.hardness+" · 权重 "+s.weight+"</span></figcaption></figure>";
  }).join("");
}
function renderInvalidation(data){
  q("invalidation").textContent=json(data.invalidation||{state:"无本次编辑失效；缓存键与提交序列见依赖图"});
  drawGraph(data.graph||{nodes:[],edges:[]});
}
function drawGraph(graph){
  var svg=q("graph"),w=svg.clientWidth||880,h=240;
  svg.setAttribute("viewBox","0 0 "+w+" "+h);
  var layers={conversion:0,stroke:0,order:1,block:2,topology:3,direction:4};
  var groups={};
  (graph.nodes||[]).forEach(function(n){(groups[n.kind]=groups[n.kind]||[]).push(n)});
  var positions={};
  Object.keys(groups).forEach(function(kind){
    var list=groups[kind],layer=layers[kind]||0;
    list.forEach(function(n,i){
      var x=55+layer*(w-110)/4;
      var y=28+(i+1)*Math.max(18,(h-45)/Math.max(1,list.length+1));
      positions[n.id]={x:x,y:y,node:n};
    });
  });
  var parts=[];
  (graph.edges||[]).forEach(function(e){
    var a=positions[e.from],b=positions[e.to];
    if(a&&b){parts.push("<line x1='"+a.x+"' y1='"+a.y+"' x2='"+b.x+"' y2='"+b.y+"' stroke='#31445c' stroke-width='1'><title>"+esc(e.reason)+"</title></line>")}
  });
  Object.keys(positions).forEach(function(id){
    var p=positions[id],fill=p.node.kind==="direction"?"#4ed6a9":p.node.kind==="block"?"#5d7fbe":"#f3b45b";
    parts.push("<circle cx='"+p.x+"' cy='"+p.y+"' r='4.5' fill='"+fill+"'><title>"+esc(id)+"</title></circle>");
    parts.push("<text x='"+(p.x+8)+"' y='"+(p.y+4)+"' fill='#9fb0c7' font-size='9'>"+esc(id)+"</text>");
  });
  ["conversion","stroke","order","block","topology","direction"].forEach(function(kind,layer){
    parts.push("<text x='"+(55+layer*(w-110)/4)+"' y='14' fill='#71849f' font-size='10' text-anchor='middle'>"+kind+"</text>");
  });
  svg.innerHTML=parts.join("");
}
function renderBudget(data){
  var b=data.budget||{},events=b.events||[];
  var head="<div class='kv'><div><span class='muted'>最大并行</span><strong>"+(b.maxObservedParallel||0)+" / "+(b.maxParallel||0)+"</strong></div><div><span class='muted'>观察像素占用</span><strong>"+(b.maxObservedPixels||0)+"</strong></div><div><span class='muted'>线程像素预算</span><strong>"+(b.pixelBufferPerWorker||0)+"</strong></div><div><span class='muted'>队列预算</span><strong>"+(b.queueCapacity||0)+"</strong></div></div>";
  var rows=events.length?events.map(function(e){return "<tr><td>"+e.blockIndex+"</td><td>"+esc(e.stage)+"</td><td>"+esc(e.reason)+"</td><td>"+e.waitMicros+" µs</td></tr>"}).join(""):"<tr><td colspan='4' class='muted'>本路径未发生预算阻塞；并行度与像素峰值仍受限。</td></tr>";
  q("budget").innerHTML=head+"<table style='margin-top:12px'><thead><tr><th>块</th><th>阶段</th><th>排队原因</th><th>等待</th></tr></thead><tbody>"+rows+"</tbody></table>";
}
function renderBlocks(data){
  var blocks=data.blocks||[];
  var rows=blocks.map(function(b){
    return "<tr><td>"+b.index+"</td><td class='mono'>"+String(b.cacheKey).slice(0,12)+"</td><td>"+(b.reused?"复用":"重算")+"</td><td>"+esc((b.strokeIds||[]).join(", "))+"</td><td>"+b.queueWaitMicros+" / "+b.pixelWaitMicros+" µs</td></tr>";
  }).join("");
  q("blocks").innerHTML="<table><thead><tr><th>#</th><th>缓存键</th><th>状态</th><th>参与笔</th><th>队列/像素等待</th></tr></thead><tbody>"+rows+"</tbody></table>";
}
q("project").addEventListener("change",function(e){loadProject(e.target.value)});
q("rerun").addEventListener("click",async function(){
  if(!state.project)return;
  setStatus("全量重算中","warn");
  var res=await fetch("/api/project/"+encodeURIComponent(state.project)+"/rerun?mode=full",{method:"POST"});
  var body=await res.json();
  if(!res.ok){setStatus(body.error||"重算失败","bad");return}
  await loadProject(state.project);
});
q("edit").addEventListener("click",async function(){
  if(!state.project||!state.data||!state.data.strokes.length)return;
  var stroke=state.data.strokes[0];
  setStatus("应用笔画编辑","warn");
  var res=await fetch("/api/project/"+encodeURIComponent(state.project)+"/edit",{method:"POST",headers:{"Content-Type":"application/json"},body:JSON.stringify({strokeId:stroke.id,dx1:3,hardness:82})});
  var body=await res.json();
  if(!res.ok){setStatus(body.error||"编辑失败","bad");return}
  await loadProject(state.project);
});
loadProjects().catch(function(err){setStatus(String(err),"bad")});
</script>
</body>
</html>`
