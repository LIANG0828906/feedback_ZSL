package terrain

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(dashboardHTML))
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.State())
	})
	mux.HandleFunc("GET /api/export", func(w http.ResponseWriter, r *http.Request) {
		data, err := svc.ExportJSON()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="terrain-export.json"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("POST /api/blocks/{block}/edit", func(w http.ResponseWriter, r *http.Request) {
		block, err := strconv.Atoi(r.PathValue("block"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid block number"})
			return
		}
		var req EditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		manifest, runErr := svc.ApplyEdit(r.Context(), block, req)
		writeRunResponse(w, manifest, runErr)
	})
	mux.HandleFunc("POST /api/blocks/{block}/retry", func(w http.ResponseWriter, r *http.Request) {
		block, err := strconv.Atoi(r.PathValue("block"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid block number"})
			return
		}
		manifest, runErr := svc.RetryBlock(r.Context(), block)
		writeRunResponse(w, manifest, runErr)
	})
	mux.HandleFunc("POST /api/blocks/{block}/fault", func(w http.ResponseWriter, r *http.Request) {
		block, err := strconv.Atoi(r.PathValue("block"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid block number"})
			return
		}
		svc.InjectFailureOnce(block, fmt.Errorf("injected diagnostic failure in block %d", block))
		manifest, runErr := svc.RetryBlock(r.Context(), block)
		writeRunResponse(w, manifest, runErr)
	})
	return mux
}

func writeRunResponse(w http.ResponseWriter, manifest *RunManifest, runErr error) {
	status := http.StatusOK
	payload := map[string]any{"manifest": manifest}
	if runErr != nil {
		payload["run_error"] = runErr.Error()
		payload["ok"] = false
	} else {
		payload["ok"] = true
	}
	writeJSON(w, status, payload)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

const dashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>Block Minimal Terrain Diagnostics</title>
<style>
:root{color-scheme:dark;font-family:Inter,Segoe UI,system-ui,sans-serif}
body{margin:0;background:#0b1020;color:#e5eefc;display:grid;grid-template-columns:minmax(520px,1.5fr) 430px;min-height:100vh}
header{grid-column:1/3;padding:14px 20px;border-bottom:1px solid #26344f;background:#10192d;display:flex;gap:16px;align-items:center}
h1{font-size:18px;margin:0}.hint{color:#8ea3c8;font-size:12px}
main{padding:14px}aside{padding:14px;border-left:1px solid #26344f;background:#0e1729;overflow:auto;max-height:calc(100vh - 57px)}
.card{background:#121e36;border:1px solid #26344f;border-radius:10px;padding:12px;margin-bottom:12px}
.card h2{font-size:13px;margin:0 0 8px;color:#9fc1ff;text-transform:uppercase;letter-spacing:.08em}
canvas{width:100%;height:620px;background:#080d1a;border-radius:8px;border:1px solid #26344f}
table{width:100%;border-collapse:collapse;font-size:12px}td,th{border-bottom:1px solid #24324d;padding:5px 6px;text-align:left}
button,input,select{background:#0b1428;border:1px solid #314366;color:#e5eefc;border-radius:6px;padding:6px 8px;font-size:12px}
button{cursor:pointer;background:#1d4ed8;border-color:#3b82f6}button.secondary{background:#26344f}button.danger{background:#991b1b;border-color:#ef4444}
.row{display:flex;gap:6px;flex-wrap:wrap;align-items:center;margin:6px 0}.pill{display:inline-block;border-radius:999px;padding:2px 8px;font-size:11px;background:#24324d}.ok{color:#86efac}.bad{color:#fca5a5}.warn{color:#fcd34d}
pre{white-space:pre-wrap;word-break:break-word;font-size:11px;max-height:220px;overflow:auto;background:#080d1a;padding:8px;border-radius:6px}
a{color:#93c5fd}
</style></head>
<body>
<header><h1>远景分块极简着色网格 · 本地诊断</h1><span class="hint">确定性样本 / SQLite checkpoint / 无外部服务</span><span style="flex:1"></span><a href="/api/export">导出 JSON</a></header>
<main><div class="card"><h2>着色预览 + 线框 + 边界归因</h2><canvas id="view" width="900" height="620"></canvas></div>
<div class="card"><h2>失效传播示意</h2><div id="graph"></div></div></main>
<aside>
<div class="card"><h2>编辑 / 重试</h2>
<div class="row"><label>块 <select id="block"></select></label><label>预算 <input id="budget" type="number" min="3" style="width:80px"></label><button onclick="applyBudget()">改预算</button></div>
<div class="row"><label>容限 <input id="tol" type="number" min="0" step="0.01" style="width:80px"></label><button onclick="applyTol()">改容限</button></div>
<div class="row"><label>格点 x <input id="gx" type="number" value="8" style="width:55px"></label><label>y <input id="gy" type="number" value="6" style="width:55px"></label><label>Δh <input id="dh" type="number" step="0.1" value="1.2" style="width:65px"></label><button onclick="applyHeight()">改高程</button></div>
<div class="row"><button class="secondary" onclick="retry()">逐块重试</button><button class="danger" onclick="fault()">注入瞬时故障</button><button class="secondary" onclick="load()">刷新</button></div>
<div id="action" class="hint"></div></div>
<div class="card"><h2>块状态 / 缓存 / 耗时</h2><div id="blocks"></div></div>
<div class="card"><h2>边界折角与归因</h2><div id="boundaries"></div></div>
<div class="card"><h2>断裂样本：欠采样 / 错位 / E_ALIGN</h2><div id="faults"></div></div>
<div class="card"><h2>预算占用与排队</h2><div id="budget"></div></div>
<div class="card"><h2>失效清单</h2><div id="invalidation"></div></div>
</aside>
<script>
let STATE=null;
async function load(){const r=await fetch('/api/state');STATE=await r.json();render();}
function blockOptions(){const n=STATE.project.blocks.length;const sel=document.getElementById('block');if(sel.options.length)return;for(let i=0;i<n;i++){const o=document.createElement('option');o.value=i;o.textContent='块 '+i;sel.add(o)}}
function selected(){return parseInt(document.getElementById('block').value||'0')}
async function post(url,body){document.getElementById('action').textContent='处理中...';const r=await fetch(url,{method:'POST',headers:{'Content-Type':'application/json'},body:body?JSON.stringify(body):'{}'});const j=await r.json();document.getElementById('action').textContent=j.run_error?('失败已隔离: '+j.run_error):'完成并串行提交';await load();return j}
function applyBudget(){const b=selected(),v=parseInt(document.getElementById('budget').value);post('/api/blocks/'+b+'/edit',{sample_budget:v})}
function applyTol(){const b=selected(),v=parseFloat(document.getElementById('tol').value);post('/api/blocks/'+b+'/edit',{error_tolerance:v})}
function applyHeight(){const b=selected();post('/api/blocks/'+b+'/edit',{grid_x:parseInt(gx.value),grid_y:parseInt(gy.value),height_delta:parseFloat(dh.value)})}
function retry(){post('/api/blocks/'+selected()+'/retry',{})}
function fault(){post('/api/blocks/'+selected()+'/fault',{})}
function colorFor(h,n){const light=Math.max(24,Math.min(72,48+h*5+n*18));return 'hsl('+Math.max(95,150-h*10)+',48%,'+light+'%)'}
function render(){blockOptions();renderBlocks();renderBoundaries();renderFaults();renderBudget();renderInvalidation();renderGraph();draw()}
function renderBlocks(){let html='<table><tr><th>块</th><th>状态</th><th>顶点/三角</th><th>缓存</th><th>ms</th><th>hash</th></tr>';(STATE.manifest?.blocks||[]).forEach(b=>{html+='<tr><td>'+b.block_no+'</td><td class="'+(b.status==='failed'?'bad':'ok')+'">'+b.status+(b.failure?' ⚠':'')+'</td><td>'+b.vertices+'/'+b.triangles+'</td><td>'+(b.cache_hit?'命中':'重剖')+'</td><td>'+b.build_ms+'</td><td>'+(b.content_hash||'').slice(8,18)+'</td></tr>'});html+='</table>';document.getElementById('blocks').innerHTML=html}
function renderBoundaries(){let html='';Object.values(STATE.reports||{}).forEach(r=>{const cls=r.category==='ok'?'ok':'bad';html+='<div style="margin-bottom:8px"><b>'+r.boundary_id+'</b> <span class="pill '+cls+'">'+r.category+'</span><div>最大折角 '+r.max_fold_angle_deg.toFixed(3)+'° / 限 '+r.limit_deg+'°</div><div class="hint">'+(r.evidence||[]).join('；')+'</div></div>'});document.getElementById('boundaries').innerHTML=html||'<span class=hint>尚无报告，先运行剖分。</span>'}
function renderFaults(){let html='';(STATE.fault_reports||[]).forEach(r=>{const cls=r.category==='ok'?'ok':'bad';html+='<div style="margin-bottom:8px"><b>'+r.case_id+'</b> <span class="pill '+cls+'">'+r.category+'</span>'+(r.error_code?' <span class=pill bad>'+r.error_code+'</span>':'')+'<div>折角 '+(r.max_fold_angle_deg||0).toFixed(2)+'°，配对残差 '+(r.max_pair_residual||0).toFixed(3)+'</div><div class=hint>'+(r.evidence||[]).join('；')+'</div></div>'});document.getElementById('faults').innerHTML=html}
function renderBudget(){const b=STATE.budget||{};let html='<div>并行 '+b.active_workers+'/'+b.max_parallel+'，内存 '+(b.used_bytes/1024).toFixed(1)+' / '+(b.budget_bytes/1024/1024).toFixed(1)+' MiB，排队 '+b.queued+'</div><table><tr><th>块</th><th>状态</th><th>原因</th><th>等待ms</th></tr>';(STATE.queue||[]).slice(-12).reverse().forEach(q=>{html+='<tr><td>'+q.block_no+'</td><td>'+q.state+'</td><td>'+(q.reason||'')+'</td><td>'+(q.wait_ms||'')+'</td></tr>'});html+='</table>';document.getElementById('budget').innerHTML=html}
function renderInvalidation(){const items=STATE.manifest?.invalidation||[];let html='<table><tr><th>块</th><th>失效</th><th>原因</th><th>复用</th></tr>';items.forEach(i=>{html+='<tr><td>'+i.block_no+'</td><td>'+(i.invalid?'是':'否')+'</td><td>'+i.reason+'</td><td>'+i.reuse_status+'</td></tr>'});html+='</table>';document.getElementById('invalidation').innerHTML=html}
function renderGraph(){const blocks=STATE.project.blocks;let html='<div style="display:grid;grid-template-columns:repeat(2,120px);gap:18px;justify-content:center">';blocks.forEach(b=>{const st=(STATE.manifest?.blocks||[]).find(x=>x.block_no===b.block_no);const color=st?.status==='failed'?'#7f1d1d':(st?.cache_hit?'#14532d':'#1e3a8a');html+='<div style="background:'+color+';border:1px solid #60a5fa;border-radius:10px;padding:14px;text-align:center"><b>B'+b.block_no+'</b><div class=hint>'+b.feature+'</div><div>'+(st?.cache_hit?'缓存复用':'重剖/恢复')+'</div></div>'});html+='</div><div class=hint style="text-align:center;margin-top:8px">双向边：edge-0-1 / edge-0-2 / edge-1-3 / edge-2-3；编辑块只传播到直接重对齐邻块。</div>';document.getElementById('graph').innerHTML=html}
function draw(){const c=document.getElementById('view'),ctx=c.getContext('2d');ctx.clearRect(0,0,c.width,c.height);if(!STATE?.cache)return;let minX=1e9,maxX=-1e9,minZ=1e9,maxZ=-1e9;Object.values(STATE.cache).forEach(e=>e?.mesh?.vertices.forEach(v=>{minX=Math.min(minX,v.x);maxX=Math.max(maxX,v.x);minZ=Math.min(minZ,v.z);maxZ=Math.max(maxZ,v.z)}));const pad=45,sx=(c.width-pad*2)/Math.max(1,maxX-minX),sz=(c.height-pad*2)/Math.max(1,maxZ-minZ),sc=Math.min(sx,sz);const X=x=>pad+(x-minX)*sc,Z=z=>pad+(z-minZ)*sc;
Object.entries(STATE.cache).forEach(([no,e])=>{if(!e?.mesh)return;const m=e.mesh;m.triangles.forEach(t=>{const a=m.vertices[t.vertices[0]],b=m.vertices[t.vertices[1]],d=m.vertices[t.vertices[2]];const n=normal(a,b,d);ctx.beginPath();ctx.moveTo(X(a.x),Z(a.z));ctx.lineTo(X(b.x),Z(b.z));ctx.lineTo(X(d.x),Z(d.z));ctx.closePath();ctx.fillStyle=colorFor((a.y+b.y+d.y)/3,Math.abs(n[1]));ctx.globalAlpha=.82;ctx.fill();ctx.globalAlpha=1;ctx.strokeStyle='rgba(220,235,255,.38)';ctx.lineWidth=.6;ctx.stroke()})});
Object.values(STATE.reports||{}).forEach(r=>{const loc=r.max_angle_location;if(!loc)return;ctx.beginPath();ctx.arc(X(loc.at_xyz[0]),Z(loc.at_xyz[2]),6,0,7);ctx.fillStyle=r.category==='ok'?'#22c55e':'#ef4444';ctx.fill();ctx.fillStyle='#fff';ctx.font='12px sans-serif';ctx.fillText(r.category+' '+r.max_fold_angle_deg.toFixed(1)+'°',X(loc.at_xyz[0])+8,Z(loc.at_xyz[2])-8)});
ctx.fillStyle='#9fb6dd';ctx.font='12px sans-serif';STATE.project.blocks.forEach(b=>{ctx.fillText('B'+b.block_no,b.origin_x*sc+pad,b.origin_z*sc+pad+12)});}
function normal(a,b,c){const ux=b.x-a.x,uy=b.y-a.y,uz=b.z-a.z,vx=c.x-a.x,vy=c.y-a.y,vz=c.z-a.z;let x=uy*vz-uz*vy,y=uz*vx-ux*vz,z=ux*vy-uy*vx;const l=Math.hypot(x,y,z)||1;return[x/l,y/l,z/l]}
load();setInterval(load,2500);
</script></body></html>`
