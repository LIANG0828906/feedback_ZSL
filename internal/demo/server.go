package demo

import (
	"encoding/json"
	"net/http"
)

type Server struct {
	report *SuiteReport
}

func NewServer(report *SuiteReport) *Server {
	return &Server{report: report}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/report", s.apiReport)
	mux.HandleFunc("/api/project", s.apiProject)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) apiReport(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.report)
}

func (s *Server) apiProject(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.report.Project)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>室内一次反射可见性验收台</title>
<style>
:root{color-scheme:dark;--bg:#0d1117;--panel:#161b22;--line:#30363d;--text:#e6edf3;--muted:#8b949e;--cyan:#58a6ff;--green:#3fb950;--yellow:#d29922;--red:#f85149}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
header{padding:18px 24px;border-bottom:1px solid var(--line);display:flex;gap:16px;align-items:baseline;flex-wrap:wrap}
h1{font-size:18px;margin:0}h2{font-size:15px;margin:0 0 10px;color:var(--cyan)}
main{display:grid;grid-template-columns:minmax(520px,1.25fr) minmax(420px,1fr);gap:18px;padding:18px;max-width:1600px;margin:auto}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px}
canvas{width:100%;height:520px;border-radius:6px;background:#0a0c10;border:1px solid var(--line);display:block}
.legend{display:flex;flex-wrap:wrap;gap:12px;margin-top:10px;color:var(--muted)}
.dot{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:5px;vertical-align:middle}
table{width:100%;border-collapse:collapse;margin:6px 0 14px}th,td{text-align:left;padding:6px 8px;border-bottom:1px solid var(--line)}th{color:var(--muted);font-weight:500}
.bad{color:var(--red)}.good{color:var(--green)}.warn{color:var(--yellow)}
.pill{display:inline-block;border:1px solid var(--line);border-radius:999px;padding:1px 8px;margin:2px;color:var(--muted)}
pre{white-space:pre-wrap;word-break:break-word;background:#0a0c10;border:1px solid var(--line);border-radius:6px;padding:10px;max-height:220px;overflow:auto}
@media(max-width:980px){main{grid-template-columns:1fr}}
</style>
</head>
<body>
<header><h1>室内一次反射点可见性验收台</h1><span id="status" class="pill">loading</span><span id="summary" class="muted"></span></header>
<main>
<section class="panel">
<h2>房间链与反射点叠加</h2>
<canvas id="scene" width="1000" height="560"></canvas>
<div class="legend">
<span><i class="dot" style="background:#58a6ff"></i>房间/门洞</span>
<span><i class="dot" style="background:#f85149"></i>声源</span>
<span><i class="dot" style="background:#3fb950"></i>听者</span>
<span><i class="dot" style="background:#d29922"></i>反射点</span>
<span><i class="dot" style="background:#bc8cff"></i>房间链</span>
</div>
</section>
<section class="panel">
<h2>三路径比对</h2>
<table id="paths"></table>
<h2>真值与误差</h2>
<table id="truth"></table>
<h2>区划编辑失效传播</h2>
<table id="invalidation"></table>
<h2>运行明细</h2>
<pre id="details"></pre>
</section>
</main>
<script>
const palette=['#bc8cff','#56d4dd','#ffa657','#79c0ff','#ff7b72','#a5d6ff','#7ee787','#f2cc60'];
function fmt(v){return typeof v==='number'?v.toFixed(v<10?4:2):String(v)}
function esc(v){return String(v).replace(/[&<>"]/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]))}
function table(el,rows,headers){el.innerHTML='<thead><tr>'+headers.map(h=>'<th>'+h+'</th>').join('')+'</tr></thead><tbody>'+rows.join('')+'</tbody>'}
function segmentFromOpening(w,o){const horiz=w.a.y===w.b.y;if(horiz){const lo=Math.min(w.a.x,w.b.x);return {a:{x:lo+o.start,y:w.a.y},b:{x:lo+o.start+o.width,y:w.a.y},horiz:true}}const lo=Math.min(w.a.y,w.b.y);return {a:{x:w.a.x,y:lo+o.start},b:{x:w.a.x,y:lo+o.start+o.width},horiz:false}}
function draw(){
  const report=window.__report;if(!report)return
  const project=report.project, solutions=report.full.result.listenerSolutions, chains=report.full.result.chainSummaries
  const pts=[];project.rooms.forEach(r=>{pts.push({x:r.rect.minX,y:r.rect.minY},{x:r.rect.maxX,y:r.rect.maxY})})
  project.listeners.forEach(l=>pts.push(l.pos));project.sources.forEach(s=>pts.push(s.pos));project.reflectors.forEach(r=>pts.push(r.a,r.b))
  const minX=Math.min(...pts.map(p=>p.x)),maxX=Math.max(...pts.map(p=>p.x)),minY=Math.min(...pts.map(p=>p.y)),maxY=Math.max(...pts.map(p=>p.y))
  const c=document.getElementById('scene'),ctx=c.getContext('2d'),W=c.width,H=c.height,pad=46
  ctx.clearRect(0,0,W,H)
  const sx=(W-2*pad)/Math.max(1,maxX-minX),sy=(H-2*pad)/Math.max(1,maxY-minY),s=Math.min(sx,sy)
  const ox=(W-(maxX-minX)*s)/2,oy=(H-(maxY-minY)*s)/2
  const X=x=>ox+(x-minX)*s,Y=y=>H-oy-(y-minY)*s
  ctx.font='12px monospace';ctx.textBaseline='top'
  project.rooms.forEach((r,i)=>{ctx.fillStyle=i%2?'rgba(88,166,255,.08)':'rgba(63,185,80,.06)';ctx.strokeStyle='#30363d';ctx.lineWidth=1;ctx.fillRect(X(r.rect.minX),Y(r.rect.maxY),(r.rect.maxX-r.rect.minX)*s,(r.rect.maxY-r.rect.minY)*s);ctx.strokeRect(X(r.rect.minX),Y(r.rect.maxY),(r.rect.maxX-r.rect.minX)*s,(r.rect.maxY-r.rect.minY)*s);ctx.fillStyle='#8b949e';ctx.fillText('room '+r.id,X(r.rect.minX)+8,Y(r.rect.maxY)+8)})
  project.walls.forEach(w=>{ctx.strokeStyle='#c9d1d9';ctx.lineWidth=3;ctx.beginPath();ctx.moveTo(X(w.a.x),Y(w.a.y));ctx.lineTo(X(w.b.x),Y(w.b.y));ctx.stroke()})
  project.openings.forEach(o=>{const w=project.walls.find(v=>v.id===o.wallId);if(!w)return;const d=segmentFromOpening(w,o);ctx.strokeStyle='#56d4dd';ctx.lineWidth=7;ctx.beginPath();ctx.moveTo(X(d.a.x),Y(d.a.y));ctx.lineTo(X(d.b.x),Y(d.b.y));ctx.stroke()})
  project.reflectors.forEach(r=>{ctx.strokeStyle='rgba(88,166,255,.7)';ctx.lineWidth=3;ctx.beginPath();ctx.moveTo(X(r.a.x),Y(r.a.y));ctx.lineTo(X(r.b.x),Y(r.b.y));ctx.stroke()})
  chains.forEach((ch,i)=>{ctx.strokeStyle=palette[i%palette.length]+'88';ctx.lineWidth=1.5;ch.segments.forEach(seg=>{ctx.beginPath();ctx.moveTo(X(seg.from.x),Y(seg.from.y));ctx.lineTo(X(seg.to.x),Y(seg.to.y));ctx.stroke()})})
  solutions.forEach(sol=>sol.paths.forEach(p=>{ctx.strokeStyle='rgba(210,153,34,.45)';ctx.lineWidth=1.5;ctx.beginPath();const src=project.sources.find(v=>v.id===p.sourceId),lis=project.listeners.find(v=>v.id===sol.listenerId);if(src){ctx.moveTo(X(src.pos.x),Y(src.pos.y));ctx.lineTo(X(p.reflectionPoint.x),Y(p.reflectionPoint.y))}ctx.lineTo(X(lis.pos.x),Y(lis.pos.y));ctx.stroke();ctx.fillStyle='#d29922';ctx.beginPath();ctx.arc(X(p.reflectionPoint.x),Y(p.reflectionPoint.y),4,0,Math.PI*2);ctx.fill()}))
  project.sources.forEach(s=>{ctx.fillStyle='#f85149';ctx.beginPath();ctx.arc(X(s.pos.x),Y(s.pos.y),6,0,Math.PI*2);ctx.fill();ctx.fillStyle='#ffdcd7';ctx.fillText('S'+s.id,X(s.pos.x)+8,Y(s.pos.y)-16)})
  project.listeners.forEach(l=>{ctx.fillStyle='#3fb950';ctx.beginPath();ctx.arc(X(l.pos.x),Y(l.pos.y),6,0,Math.PI*2);ctx.fill();ctx.fillStyle='#dafbe1';ctx.fillText('L'+l.id,X(l.pos.x)+8,Y(l.pos.y)-16)})
}
function render(report){
  window.__report=report;document.getElementById('status').textContent=report.allPathsEqual&&report.truth.passed?'PASS':'CHECK'
  document.getElementById('status').className='pill '+(report.allPathsEqual&&report.truth.passed?'good':'bad')
  const paths=report.preview.result.commitSequence.join(',');
  document.getElementById('summary').textContent='seed='+report.seed+' rooms='+report.project.rooms.length+' paths='+report.truth.actualPaths+' max-bound='+fmt(report.errorBounds.maxCumulativeDb)+' dB'
  const cmp=(name)=>report.comparisons.find(c=>(c.left===name&&c.right==='full')||(c.right===name&&c.left==='full'))
  table(document.getElementById('paths'),[report.preview,report.resume,report.full].map(p=>{const c=p.name==='full'?{equal:true}:cmp(p.name);return '<tr><td>'+esc(p.name)+'</td><td>'+p.result.commitSequence.join(' → ')+'</td><td>'+p.result.listenerSolutions.reduce((a,s)=>a+s.paths.length,0)+'</td><td class="'+(c?.equal?'good':'bad')+'">'+(p.name==='full'?'reference':(c?.equal?'equal':'diff'))+'</td></tr>'}),['路径','提交序列','有效路径','与 full 比对'])
  table(document.getElementById('truth'),[['期望路径',report.truth.expectedPaths],['实际路径',report.truth.actualPaths],['真值判定',report.truth.passed?'PASS':'FAIL'],['1 dB 通过',report.errorBounds.passAtOneDb],['0.1 dB 通过',report.errorBounds.passAtPointOneDb],['0.01 dB 通过',report.errorBounds.passAtPointZeroOneDb]].map(r=>'<tr><td>'+r[0]+'</td><td>'+esc(r[1])+'</td></tr>'),['项目','值'])
  if(report.invalidation){table(document.getElementById('invalidation'),report.invalidation.entries.map(e=>'<tr><td>'+e.roomId+'</td><td class="'+(e.reused?'good':'bad')+'">'+(e.reused?'reused':'invalidated')+'</td><td>'+esc((e.reasons||[]).join('; '))+'</td><td>'+esc((e.invalidatedListeners||[]).join(', '))+'</td></tr>'),['房间','缓存','原因','下游听者'])}else{const el=document.getElementById('invalidation');el.innerHTML='<tr><td colspan="4">'+esc(report.invalidationError||'no report')+'</td></tr>'}
  document.getElementById('details').textContent=JSON.stringify({comparisons:report.comparisons,restore:report.resume.restore,workerStats:report.full.result.workerStats,dependencyEdges:report.invalidation?.dependencies?.length||0},null,2)
  draw()
}
fetch('/api/report').then(r=>r.json()).then(render).catch(err=>{document.getElementById('status').textContent=err;document.getElementById('status').className='pill bad'})
window.addEventListener('resize',()=>{const c=document.getElementById('scene');c.width=c.clientWidth;draw()})
</script>
</body>
</html>`
