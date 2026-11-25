package web

import (
	"encoding/json"
	"net/http"
)

func NewHandler(dash *Dashboard) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(dash)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(indexHTML))
	})
	return mux
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>战斗判定离线复算诊断台</title>
<style>
:root{--bg:#0b1020;--panel:#121a31;--line:#2a365d;--text:#e8efff;--muted:#9fb0d0;--ok:#39d98a;--bad:#ff6b6b;--warn:#ffcc66;--hit:#ff4d6d;--depleted:#f59e0b;--reuse:#38bdf8}
*{box-sizing:border-box} body{margin:0;background:linear-gradient(180deg,#0b1020,#0e1428);color:var(--text);font:14px/1.45 system-ui,"Microsoft YaHei",sans-serif}
header{padding:18px 24px;border-bottom:1px solid var(--line);display:flex;gap:16px;align-items:center;flex-wrap:wrap}
h1{font-size:20px;margin:0}.badge{padding:3px 9px;border-radius:999px;background:#1d2949;border:1px solid var(--line);color:var(--muted)}
.ok{color:var(--ok);border-color:rgba(57,217,138,.4)}.bad{color:var(--bad);border-color:rgba(255,107,107,.4)}.warn{color:var(--warn)}
main{display:grid;grid-template-columns:minmax(720px,1.5fr) minmax(360px,.8fr);gap:16px;padding:16px 24px}
.panel{background:rgba(18,26,49,.92);border:1px solid var(--line);border-radius:14px;padding:14px;box-shadow:0 10px 30px rgba(0,0,0,.25)}
.panel h2{font-size:15px;margin:0 0 10px;color:#c9d8ff}
canvas{width:100%;height:auto;background:#0a0f1f;border:1px solid var(--line);border-radius:10px}
.grid{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}.card{background:#0d152b;border:1px solid var(--line);border-radius:10px;padding:10px}
.k{color:var(--muted);font-size:12px}.v{font-weight:700;margin-top:3px;word-break:break-all}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{border-bottom:1px solid var(--line);padding:6px 4px;text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:600}.chips{display:flex;flex-wrap:wrap;gap:6px}.chip{border:1px solid var(--line);border-radius:8px;padding:4px 7px;background:#0d152b;font-size:12px}
.chip.reuse{border-color:rgba(56,189,248,.5);color:var(--reuse)}.chip.invalid{border-color:rgba(255,107,107,.55);color:var(--bad)}.chip.down{border-color:rgba(255,204,102,.55);color:var(--warn)}
.scroll{max-height:260px;overflow:auto}.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}.muted{color:var(--muted)}
@media(max-width:1100px){main{grid-template-columns:1fr}}
</style>
</head>
<body>
<header><h1>战斗判定离线复算与命中归因内核</h1><span id="overall" class="badge">装载中…</span><span class="badge">固定种子 428691</span><span class="badge">SQLite / modernc pure-Go</span></header>
<main>
<section class="panel"><h2>XZ 投影：扫掠轨迹、碰撞体与逐帧命中</h2><canvas id="scene" width="1080" height="900"></canvas>
<p class="muted">蓝框为 AABB，绿色为朝向胶囊体，虚线为武器扫掠；红点为 hit，橙点为伤害耗尽后的 depleted 段。</p></section>
<aside style="display:flex;flex-direction:column;gap:16px">
<section class="panel"><h2>三路径自动比对</h2><div id="compare" class="grid"></div></section>
<section class="panel"><h2>帧区间、失效与复用</h2><div id="intervals" class="chips"></div><div id="invalidations" style="margin-top:10px"></div></section>
<section class="panel"><h2>预算排队</h2><div id="queues" class="scroll"></div></section>
<section class="panel"><h2>版本转换与 E_LOSSY</h2><div id="versions" class="scroll"></div></section>
</aside>
<section class="panel" style="grid-column:1/-1"><h2>逐帧命中与穿透结算</h2><div class="scroll"><table id="frames"><thead><tr><th>帧</th><th>扫掠</th><th>首触</th><th>部位/表面</th><th>剩余伤害</th><th>最近距离(Q)</th><th>段序列</th></tr></thead><tbody></tbody></table></div></section>
</main>
<script>
const Q=1000000;
function q(v){return (v/Q).toFixed(3)}
function seqText(segs){return segs.map(s=>'#'+s.segment+':'+s.status+'/c'+(s.collider_no||'-')+'/'+s.remaining_damage_q).join(' ')}
async function init(){
 const d=await (await fetch('/api/dashboard')).json();
 window.D=d;
 const ok=!d.live_mismatch&&!d.resume_mismatch&&d.incremental_match;
 const o=document.getElementById('overall'); o.textContent=ok?'三路径 + 增量比对一致':'存在比对不一致'; o.className='badge '+(ok?'ok':'bad');
 renderCompare(d); renderIntervals(d); renderQueues(d); renderVersions(d); renderFrames(d); drawScene(d);
}
function card(k,v,cls){return '<div class="card"><div class="k">'+k+'</div><div class="v '+(cls||'')+'">'+v+'</div></div>'}
function mm(m){return m?('<span class="bad">首个不一致 F'+m.frame+' '+m.field+'</span>'):'<span class="ok">完全一致</span>'}
function renderCompare(d){document.getElementById('compare').innerHTML=[
 card('实时 vs 全量',mm(d.live_mismatch)),card('断点恢复 vs 全量',mm(d.resume_mismatch)),
 card('增量编辑 vs 全新全量',d.incremental_match?'<span class=ok>完全一致</span>':'<span class=bad>不一致</span>'),
 card('提交序列',d.full.commit_sequence.join(',')+' / 恢复 '+d.resume.commit_sequence.join(','))].join('')}
function renderIntervals(d){
 const invalid={}; d.invalidations.forEach(x=>invalid[x.interval_no]=x);
 const reused={}; (d.incremental.reused_intervals||[]).forEach(x=>reused[x]=1);
 const recomputed={}; (d.incremental.recomputed_intervals||[]).forEach(x=>recomputed[x]=1);
 document.getElementById('intervals').innerHTML=(d.full.intervals||[]).map(x=>{
  let cls='chip'; if(invalid[x.no]) cls+=' invalid'; else if(reused[x.no]) cls+=' reuse'; if(recomputed[x.no]) cls+=' down';
  return '<span class="'+cls+'" title="'+(invalid[x.no]?invalid[x.no].reason:'')+'">I'+x.no+' F'+x.start_frame+'-'+x.end_frame+'</span>';
 }).join('');
 document.getElementById('invalidations').innerHTML='<table><tbody>'+d.invalidations.map(x=>'<tr><td>I'+x.interval_no+'</td><td>'+x.kind+'</td><td class=muted>'+x.reason+'</td></tr>').join('')+'</tbody></table>';
}
function renderQueues(d){let rows=d.queue_waits||[]; document.getElementById('queues').innerHTML=rows.length?('<table><tbody>'+rows.map(w=>'<tr><td>'+w.path+' I'+w.interval_no+'</td><td>'+w.reason+'</td><td>'+w.waited_ms+'ms</td></tr>').join('')+'</tbody></table>'):'<span class=muted>本次预算未耗尽；测试中以 queue=1 强制验证阻塞记录。</span>'}
function renderVersions(d){document.getElementById('versions').innerHTML=d.versions.map(v=>'<div class=card style="margin-bottom:8px"><b>'+v.version+'</b><div class=k>'+v.bytes+' bytes，转换痕迹 '+v.trace_count+'，损失清单 '+(v.losses||[]).length+'</div>'+((v.losses||[]).slice(0,3).map(l=>'<div class="mono warn">'+l.code+' '+l.field_path+' → '+l.disposition+'</div>').join(''))+'</div>').join('')}
function renderFrames(d){document.querySelector('#frames tbody').innerHTML=d.full.frames.map(f=>'<tr><td>'+f.frame+'</td><td class=mono>'+f.sweep_id+'</td><td>'+(f.first_collider_no||'-')+'</td><td>'+(f.first_part||'')+'<div class=muted>'+(f.first_surface||'')+'</div></td><td class=mono>'+f.remaining_damage_q+'</td><td class=mono>'+f.nearest_distance_q+'</td><td class=mono>'+seqText(f.segments)+'</td></tr>').join('')}
function drawScene(d){
 const c=document.getElementById('scene'),x=c.getContext('2d'),W=c.width,H=c.height,pad=55;
 const minX=-2,maxX=12,minZ=-2,maxZ=33,sc=Math.min((W-pad*2)/(maxX-minX),(H-pad*2)/(maxZ-minZ));
 const X=v=>pad+(v-minX)*sc, Z=v=>H-pad-(v-minZ)*sc;
 x.clearRect(0,0,W,H); x.fillStyle='#0a0f1f';x.fillRect(0,0,W,H);
 x.strokeStyle='#1b2748';x.lineWidth=1;x.fillStyle='#6f82ad';x.font='12px monospace';
 for(let xv=minX;xv<=maxX;xv+=2){x.beginPath();x.moveTo(X(xv),Z(minZ));x.lineTo(X(xv),Z(maxZ));x.stroke();x.fillText(xv,X(xv)-4,Z(minZ)-8)}
 for(let zv=minZ;zv<=maxZ;zv+=2){x.beginPath();x.moveTo(X(minX),Z(zv));x.lineTo(X(maxX),Z(zv));x.stroke();x.fillText(zv,8,Z(zv)+4)}
 d.project.colliders.forEach(c0=>{
  x.lineWidth=2;
  if(c0.kind==='aabb'){const px=X(q(c0.min.x)),pz=Z(q(c0.max.z)),w=(q(c0.max.x)-q(c0.min.x))*sc,h=(q(c0.max.z)-q(c0.min.z))*sc;x.fillStyle='rgba(56,189,248,.16)';x.strokeStyle='#38bdf8';x.fillRect(px,pz,w,h);x.strokeRect(px,pz,w,h)}
  else{const a=c0.p0,b=c0.p1,r=q(c0.radius_q)*sc;x.strokeStyle='#39d98a';x.fillStyle='rgba(57,217,138,.16)';x.lineWidth=Math.max(2,r*2);x.lineCap='round';x.beginPath();x.moveTo(X(q(a.x)),Z(q(a.z)));x.lineTo(X(q(b.x)),Z(q(b.z)));x.stroke();x.beginPath();x.arc(X(q(a.x)),Z(q(a.z)),Math.max(4,r),0,7);x.fill();x.stroke()}
  const cx=c0.kind==='aabb'?X((q(c0.min.x)+q(c0.max.x))/2):X(q(c0.p0.x)), cz=c0.kind==='aabb'?Z((q(c0.min.z)+q(c0.max.z))/2):Z(q(c0.p0.z));
  x.fillStyle='#c9d8ff';x.font='12px system-ui';x.fillText('#'+c0.index+' '+c0.id,cx+5,cz-5);
 });
 d.project.sweeps.forEach(s=>{x.strokeStyle='#e8efff';x.setLineDash([8,6]);x.lineWidth=2;x.beginPath();x.moveTo(X(q(s.from_q.x)),Z(q(s.from_q.z)));x.lineTo(X(q(s.to_q.x)),Z(q(s.to_q.z)));x.stroke();x.setLineDash([]);x.fillStyle='#e8efff';x.fillText(s.id,X(q(s.to_q.x))+6,Z(q(s.to_q.z)))});
 d.full.frames.forEach(f=>f.segments.forEach(s=>{if(!s.point_q)return;const px=X(q(s.point_q.x)),pz=Z(q(s.point_q.z));x.beginPath();x.arc(px,pz,s.status==='depleted'?6:7,0,7);x.fillStyle=s.status==='depleted'?'#f59e0b':'#ff4d6d';x.fill();x.fillStyle='#fff';x.font='11px monospace';x.fillText('S'+s.segment+' F'+s.frame+' #'+s.collider_no,px+8,pz+4)}));
}
init();
</script>
</body></html>`
