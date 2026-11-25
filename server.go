package fog

import (
	"encoding/json"
	"net/http"
)

func (w *Workbench) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleIndex)
	mux.HandleFunc("/api/state", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, http.StatusOK, w.Snapshot())
	})
	mux.HandleFunc("/api/action", w.handleAction)
	return mux
}

func (w *Workbench) handleAction(rw http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	action := query.Get("action")
	snapshot, err := w.RunAction(r.Context(), action, query)
	if err != nil {
		writeJSON(rw, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(rw, http.StatusOK, snapshot)
}

func writeJSON(rw http.ResponseWriter, status int, value any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(value)
}

func (w *Workbench) handleIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = rw.Write([]byte(diagnosticHTML))
}

const diagnosticHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>体积雾分块积分诊断台</title>
<style>
:root{color-scheme:dark;font-family:Inter,system-ui,"Noto Sans CJK SC",sans-serif;background:#0b1020;color:#e5edf7}
body{margin:0;background:linear-gradient(180deg,#0b1020,#111a2e)}
header{padding:18px 24px;border-bottom:1px solid #26344f;background:#10192dcc;position:sticky;top:0;backdrop-filter:blur(8px)}
h1{margin:0;font-size:20px} h2{font-size:15px;margin:0 0 10px;color:#9cc8ff}
main{padding:20px 24px;display:grid;grid-template-columns:360px 1fr;gap:16px}
.card{background:#121c31;border:1px solid #26344f;border-radius:12px;padding:14px;box-shadow:0 8px 24px #0003}
.wide{grid-column:1/-1}.row{display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin:8px 0}
button,input,select{background:#1c2942;color:#e5edf7;border:1px solid #365079;border-radius:8px;padding:7px 9px;font-size:13px}
button{cursor:pointer}button:hover{border-color:#7db7ff;background:#223657}
button.danger{border-color:#8a4b5c;background:#3a1d29}button.good{border-color:#3f8a68;background:#17382c}
input{width:64px}.muted{color:#8ea2c0;font-size:12px}.ok{color:#79e2b0}.bad{color:#ff8fa3}.warn{color:#ffd166}
#tiles{display:grid;grid-template-columns:repeat(4,minmax(150px,1fr));gap:8px}
.tile{border:1px solid #2b3d5e;border-radius:10px;padding:9px;background:#0e1729;font-size:12px}
.tile.failed{border-color:#ff6b81;background:#351923}.tile.committed-retry{border-color:#ffd166}
.swatch{width:100%;height:10px;border-radius:999px;margin:6px 0;border:1px solid #ffffff22}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{border-bottom:1px solid #243350;padding:6px;text-align:left}
code{color:#9ee7ff;word-break:break-all}.banner{padding:10px 12px;border-radius:10px;margin:8px 0;white-space:pre-wrap}
.banner.error{background:#3d1722;border:1px solid #ff6b81}.banner.good{background:#123528;border:1px solid #4ade80}
.grid3{display:grid;grid-template-columns:repeat(3,1fr);gap:10px}.metric{background:#0d1628;border-radius:10px;padding:10px}
.metric b{font-size:20px;display:block;margin-top:4px}.chips{display:flex;gap:6px;flex-wrap:wrap}.chip{padding:3px 7px;border-radius:999px;background:#203150;border:1px solid #365079;font-size:12px}
.chip.affected{background:#4a2030;border-color:#ff8fa3}.chip.reused{background:#173d2e;border-color:#79e2b0}
canvas{width:100%;max-width:512px;image-rendering:pixelated;border:1px solid #365079;border-radius:10px;background:#000}
.flow{font-family:ui-monospace,monospace;font-size:12px;line-height:1.8}.small{font-size:11px}
@media(max-width:980px){main{grid-template-columns:1fr}#tiles{grid-template-columns:repeat(2,1fr)}}
</style>
</head>
<body>
<header><h1>透明介质体积雾 · 分块积分诊断工作台</h1>
<div class="muted">固定种子解析真值 / 纯 Go SQLite / 确定性三路径比对</div></header>
<main>
<section class="card">
<h2>操作与异常注入</h2>
<div class="row"><label>steps</label><input id="steps" value="18"/><label>shadow</label><input id="shadow" value="42"/>
<button onclick="act('preview',{steps:val('steps'),shadow:val('shadow')})">拖动预览</button></div>
<div class="row"><button class="good" onclick="act('exact')">精确分块渲染</button><button onclick="act('threeway')">三路径比对</button><button onclick="act('reset')">重置样本</button></div>
<hr style="border-color:#26344f"/>
<div class="row"><button class="danger" onclick="act('inject',{kind:'decode'})">注入 E_DECODE</button>
<button class="danger" onclick="act('inject',{kind:'order'})">注入 E_ORDER</button>
<button class="danger" onclick="act('inject',{kind:'density',x:1,y:1})">注入 E_DENS</button>
<button class="danger" onclick="act('inject',{kind:'block',x:1,y:1})">注入 E_BLOCK(1,1)</button></div>
<div class="row"><button onclick="act('retry',{x:1,y:1})">重试分块(1,1)</button></div>
<hr style="border-color:#26344f"/>
<div class="row"><label>voxel</label><input id="medium" value="fog-a" style="width:78px"/><input id="vx" value="2"/><input id="vy" value="2"/><input id="vz" value="3"/><input id="vval" value="9"/>
<button onclick="act('editVoxel',{medium:val('medium'),vx:val('vx'),vy:val('vy'),vz:val('vz'),value:val('vval')})">编辑体素</button></div>
<div class="row"><label>散射</label><input id="scat" value="0.22"/><button onclick="act('editScattering',{medium:'fog-b',value:val('scat')})">编辑 fog-b σs</button></div>
<div id="banner"></div>
<p class="muted">所有操作仅访问本地确定性样本与 SQLite 文件，不访问网络素材或账号服务。</p>
</section>

<section class="card">
<h2>预览帧</h2>
<canvas id="canvas" width="32" height="18"></canvas>
<div id="preview" class="muted" style="margin-top:8px">尚未生成预览信息。</div>
</section>

<section class="card wide">
<h2>资源占用</h2><div id="resources" class="grid3"></div>
</section>

<section class="card wide">
<h2>分块透射率 / 散射累积</h2><div id="tiles"></div>
</section>

<section class="card">
<h2>排队明细</h2><div style="max-height:280px;overflow:auto"><table id="queue"><thead><tr><th>块</th><th>原因</th><th>等待ms</th><th>说明</th></tr></thead><tbody></tbody></table></div>
</section>
<section class="card">
<h2>失效传播与依赖图</h2><div id="graph" class="muted"></div><div id="invalidation"></div>
</section>

<section class="card wide">
<h2>SQLite 检查点与三路径结论</h2><div id="threeway" class="muted">加载后自动执行一次三路径比对。</div>
</section>
</main>
<script>
let state=null;
function val(id){return document.getElementById(id).value}
async function act(action,params={}){
  const q=new URLSearchParams({action,...params});
  const r=await fetch('/api/action?'+q.toString()); state=await r.json(); render();
}
async function load(){const r=await fetch('/api/state');state=await r.json();render();act('threeway');}
function banner(){const el=document.getElementById('banner');el.innerHTML='';
 if(state.anomaly){el.innerHTML='<div class="banner error"><b>'+state.anomaly.code+'</b> '+(state.anomaly.message||'')+'\n'+JSON.stringify({field:state.anomaly.field,ray:state.anomaly.ray_id,medium:state.anomaly.medium_id,tile:state.anomaly.tile,pixel:state.anomaly.pixel,deviation:state.anomaly.deviation,differences:state.anomaly.differences},null,2)+'</div>';}
 else if(state.info){el.innerHTML='<div class="banner good">'+state.info+'</div>';}}
function drawCanvas(){const c=document.getElementById('canvas'),ctx=c.getContext('2d');ctx.clearRect(0,0,c.width,c.height);
 if(!state.preview||!state.preview.pixels)return; for(const p of state.preview.pixels){ctx.fillStyle='rgb('+p.color[0]+','+p.color[1]+','+p.color[2]+')';ctx.fillRect(p.x,p.y,1,1);} }
function preview(){const p=state.preview;if(!p)return '<span class="muted">无预览</span>';
 const cls=p.within_tolerance&&p.within_delay?'ok':'bad';
 return '<b class="'+cls+'">'+(p.within_tolerance?'一致':'E_DIVERGE')+'</b> · steps/ray '+p.average_steps_per_ray.toFixed(1)+'/'+p.step_budget_per_ray+' (total '+p.actual_steps_total+') · shadow/ray '+p.average_shadow_per_ray.toFixed(1)+'/'+p.shadow_budget_per_ray+' (total '+p.actual_shadow_total+') · '+p.elapsed_ms+'/'+p.delay_limit_ms+'ms · Δmax '+p.max_pixel_delta.toFixed(5)+'<br><span class="small">'+p.judgement+'</span>';}
function resources(){const r=state.resource;const pct=r.shadow_total?Math.min(100,100*(r.shadow_consumed+r.shadow_reserved)/r.shadow_total):0;
 document.getElementById('resources').innerHTML=[
 ['并行分块',r.active_tiles+' / max '+r.max_active_tiles+' / limit '+r.max_parallel_tiles],
 ['射线缓冲',r.ray_buffer_high_water+' / '+r.ray_buffer_capacity],
 ['Shadow',r.shadow_consumed+' used · '+r.shadow_reserved+' reserved'],
 ['Shadow 总额度',r.shadow_total+' · 可用 '+r.shadow_available],
 ['占用',pct.toFixed(2)+'%'],['DB',state.db_path]
 ].map(x=>'<div class="metric muted">'+x[0]+'<b>'+x[1]+'</b></div>').join('');}
function tiles(){document.getElementById('tiles').innerHTML=state.tiles.map(t=>{const c=t.average_color;const col='rgb('+c[0]+','+c[1]+','+c[2]+')';
 return '<div class="tile '+t.status+'"><b>('+t.tile.x+','+t.tile.y+')</b> <span class="muted">'+t.status+'</span><div class="swatch" style="background:'+col+'"></div>'+
 'T '+t.transmittance.toFixed(6)+'<br>S '+t.scattering.toFixed(6)+' / avg '+t.average_scattering.toFixed(6)+
 '<br>shadow '+t.shadow_samples+' buf '+t.ray_buffer_used+' order '+t.commit_order+
 '<br><code>'+(t.cache_key||'').slice(0,16)+'</code></div>'}).join('');}
function queue(){const tb=document.querySelector('#queue tbody');tb.innerHTML=(state.queue||[]).slice().reverse().map(q=>'<tr><td>('+q.tile.x+','+q.tile.y+')</td><td>'+q.reason+'</td><td>'+q.wait_ms+'</td><td>'+(q.detail||'')+'</td></tr>').join('');}
function invalidation(){const g=state.graph;document.getElementById('graph').innerHTML='节点 '+g.node_count+' · 边 '+g.edge_count+'<br>voxel '+g.voxel_nodes+' · segment '+g.segment_nodes+' · tile '+g.tile_nodes;
 const inv=state.invalidation;let html='';if(inv){html='<div class="flow"><b>'+inv.edit_kind+' / '+inv.medium_id+'</b><br>  └─ '+inv.affected_tiles.length+' affected tiles → '+inv.affected_tiles.map(t=>'('+t.x+','+t.y+')').join(' ')+'<br>  └─ '+inv.reused_tiles.length+' reused tiles</div>'+
 '<div class="chips" style="margin:8px 0">'+inv.affected_tiles.map(t=>'<span class="chip affected">失效 '+'('+t.x+','+t.y+')</span>').join('')+'</div><div class="chips">'+inv.reused_tiles.map(t=>'<span class="chip reused">复用 '+'('+t.x+','+t.y+')</span>').join('')+'</div>';}
 document.getElementById('invalidation').innerHTML=html;}
function threeway(){const t=state.three_way;if(!t){document.getElementById('threeway').textContent='尚未运行';return;}
 let html=t.equal?'<div class="banner good">实时渲染、断点恢复、一次性全量重算逐像素一致；提交序列：'+t.sequence.map(x=>'('+x.x+','+x.y+')').join(' ')+'</div>':'<div class="banner error">'+JSON.stringify(t.first_diff,null,2)+'</div>';
 html+='<div class="row"><span class="chip reused">恢复复用 '+t.resume_reused.length+' 块</span><span class="chip affected">重算 '+t.resume_recomputed.length+' 块</span><span class="chip">作废检查点 '+t.stale.length+'</span></div>';
 if(t.stale&&t.stale.length)html+='<table><tbody>'+t.stale.map(s=>'<tr><td>('+s.tile.x+','+s.tile.y+')</td><td>'+s.field+'</td><td>'+s.reason+'</td></tr>').join('')+'</tbody></table>';
 document.getElementById('threeway').innerHTML=html;}
function render(){banner();drawCanvas();document.getElementById('preview').innerHTML=preview();resources();tiles();queue();invalidation();threeway();}
load();
</script></body></html>`
