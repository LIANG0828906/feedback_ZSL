// Package web serves the dependency-free local acoustic diagnostic page.
package web

import (
	"encoding/json"
	"net/http"
	"os"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/checkpoint"
)

func NewHandler(report *acoustic.Report) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(report)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
	})
	return mux
}

// NewFileHandler reloads report.json for each API request.
func NewFileHandler(reportPath string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		report, err := checkpoint.LoadReport(dirOf(reportPath))
		if err != nil {
			// Also accept an explicit report file path.
			b, readErr := os.ReadFile(reportPath)
			if readErr != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			var direct acoustic.Report
			if jsonErr := json.Unmarshal(b, &direct); jsonErr != nil {
				http.Error(w, jsonErr.Error(), http.StatusInternalServerError)
				return
			}
			report = &direct
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(report)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
	})
	return mux
}

func dirOf(reportPath string) string {
	for i := len(reportPath) - 1; i >= 0; i-- {
		if reportPath[i] == '/' {
			return reportPath[:i]
		}
	}
	return "."
}

const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>分块声学能量场诊断</title>
<style>
:root{color-scheme:light dark;font-family:ui-sans-serif,system-ui,"PingFang SC","Microsoft YaHei",sans-serif}
body{margin:0;background:#0b1220;color:#e5eefc}
header{padding:18px 24px;background:linear-gradient(120deg,#123b63,#172554);box-shadow:0 2px 12px #0006}
h1{margin:0;font-size:22px}.meta{opacity:.82;font-size:13px;margin-top:6px}
main{padding:20px 24px;display:grid;grid-template-columns:minmax(420px,1.2fr) minmax(360px,.8fr);gap:18px}
.card{background:#111c31;border:1px solid #243451;border-radius:14px;padding:16px;box-shadow:0 10px 30px #0003}
.card h2{font-size:16px;margin:0 0 12px}.wide{grid-column:1/-1}
.grid{display:grid;gap:6px}.chunk{min-height:86px;border:1px solid #ffffff22;border-radius:10px;padding:8px;background:#17233c;position:relative;overflow:hidden}
.chunk .id{font-weight:700;font-size:13px}.chunk .energy{font-size:12px;margin-top:5px;opacity:.92}.chunk .paths{font-size:11px;opacity:.7}
.controls{margin-bottom:12px}.controls button{border:1px solid #5b8cff;background:#1b315c;color:#fff;border-radius:999px;padding:6px 12px;margin-right:6px;cursor:pointer}.controls button.active{background:#4f8cff}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{border-bottom:1px solid #ffffff14;padding:7px 6px;text-align:left}th{color:#9db7e8;font-weight:600}
.ok{color:#67e8f9}.bad{color:#fca5a5}.pill{display:inline-block;border-radius:999px;padding:2px 8px;background:#203454;font-size:11px}
.scroll{max-height:330px;overflow:auto}.bar{height:8px;border-radius:99px;background:linear-gradient(90deg,#38bdf8,#a78bfa,#fb7185);margin-top:6px}
svg{width:100%;height:auto}.arrow{stroke:#7dd3fc;stroke-width:3;marker-end:url(#arrow)}.boundary{stroke:#fbbf24;stroke-width:4;stroke-linecap:round}
</style>
</head>
<body>
<header><h1>分块早期反射能量场 · 本地诊断页</h1><div class="meta" id="meta">loading…</div></header>
<main>
<section class="card"><h2>分块与逐频带能量热力</h2><div class="controls" id="bands"></div><div class="grid" id="grid"></div></section>
<section class="card"><h2>守恒残差</h2><div id="cons"></div></section>
<section class="card"><h2>边界传递</h2><div class="scroll"><table id="transfers"><thead><tr><th>ID</th><th>方向</th><th>位置</th><th>交付能量</th><th>损耗</th></tr></thead><tbody></tbody></table></div></section>
<section class="card"><h2>排队与预算明细</h2><div class="scroll"><table id="queue"><thead><tr><th>分块</th><th>Worker</th><th>阻塞</th><th>等待 ms</th><th>原因</th></tr></thead><tbody></tbody></table></div></section>
<section class="card wide"><h2>失效清单 / 提交序列</h2><div id="inval"></div><div class="scroll" style="margin-top:12px"><table id="commits"><thead><tr><th>序</th><th>分块</th><th>来源</th><th>事务号</th><th>Cache key</th></tr></thead><tbody></tbody></table></div></section>
</main>
<script>
let state=null, band=0;
const qs=new URLSearchParams(location.search); if(qs.get('band')!==null) band=Number(qs.get('band'));
function fmt(n){return (Number(n||0)/1e6).toFixed(4)}
function color(v,max){const t=max?Math.min(1,Math.sqrt(v/max)):0; const r=Math.round(18+210*t),g=Math.round(40+90*t),b=Math.round(80+120*(1-t)); return 'rgb('+r+','+g+','+b+')'}
function render(){
 if(!state)return; document.getElementById('meta').textContent='seed='+state.seed+' round='+state.round_id+' mode='+state.mode+' param='+state.param_version+' algo='+state.algorithm_version+' truth='+(state.truth_fingerprint||'').slice(0,12);
 const bands=(state.conservation&&state.conservation.band_ids)||[0,1,2];
 const bc=document.getElementById('bands'); bc.innerHTML=''; bands.forEach((b,i)=>{const x=document.createElement('button');x.textContent='频带 '+b;x.className=i===band?'active':'';x.onclick=()=>{band=i;render()};bc.appendChild(x)});
 let max=1; state.chunks.forEach(c=>{max=Math.max(max,c.retained_q?.[band]||0,c.listener_arrival_q?.[band]||0)});
const grid=document.getElementById('grid'); grid.innerHTML='';
 const cols=Math.max(...state.chunks.map(c=>c.col+1)); grid.style.gridTemplateColumns='repeat('+cols+',1fr)';
 [...state.chunks].sort((a,b)=>a.row-b.row||a.col-b.col).forEach(c=>{const d=document.createElement('div');d.className='chunk';d.style.background=color(c.retained_q?.[band]||0,max);d.innerHTML='<div class="id">'+c.chunk_id+'</div><div class="energy">到达 '+fmt(c.listener_arrival_q?.[band])+'<br/>归集 '+fmt(c.retained_q?.[band])+' / 吸收 '+fmt(c.absorbed_q?.[band])+' / 入界 '+fmt(c.inbound_q?.[band])+'</div><div class="paths">paths='+c.paths.length+' digest='+(c.path_digest||'').slice(0,10)+'</div><div class="bar"></div>';grid.appendChild(d)});
 const cons=state.conservation||{}; document.getElementById('cons').innerHTML='<p class="'+(cons.ok?'ok':'bad')+'">'+(cons.ok?'✓ 守恒通过':'✗ E_CONS')+' tolerance='+fmt(cons.tolerance_q)+'</p><table><thead><tr><th>频带</th><th>发射</th><th>归集</th><th>传递损耗</th><th>残差</th></tr></thead><tbody>'+(cons.residual_q||[]).map((r,i)=>'<tr><td>'+i+'</td><td>'+fmt(cons.emitted_q?.[i])+'</td><td>'+fmt(cons.retained_q?.[i])+'</td><td>'+fmt(cons.transfer_loss_q?.[i])+'</td><td class="'+(Math.abs(r)<=cons.tolerance_q?'ok':'bad')+'">'+r+'</td></tr>').join('')+'</tbody></table>';
 const trs=[]; state.chunks.forEach(c=>(c.transfers||[]).forEach(t=>trs.push(t))); document.querySelector('#transfers tbody').innerHTML=trs.map(t=>'<tr><td>'+t.id+'</td><td>'+t.from_chunk_id+'→'+t.to_chunk_id+' ('+t.boundary+')</td><td>'+t.position_q.x+','+t.position_q.y+'</td><td>'+(t.band_delivered_q||[]).map(fmt).join(' / ')+'</td><td>'+(t.band_loss_q||[]).map(fmt).join(' / ')+'</td></tr>').join('');
 document.querySelector('#queue tbody').innerHTML=(state.queue||[]).map(q=>'<tr><td>'+q.chunk_id+'</td><td>'+(q.worker_id??'')+'</td><td>'+(q.blocked?'是':'否')+'</td><td>'+q.wait_ms+'</td><td>'+(q.reason||'')+'</td></tr>').join('');
 document.getElementById('inval').innerHTML=(state.invalidations||[]).length?'<table><thead><tr><th>分块</th><th>原因</th><th>依赖键</th></tr></thead><tbody>'+state.invalidations.map(x=>'<tr><td>'+x.chunk_id+'</td><td>'+x.reason+'</td><td><span class="pill">'+x.dependency_key+'</span></td></tr>').join('')+'</tbody></table>':'<span class="pill">无失效：全部复用持久化缓存</span>';
 document.querySelector('#commits tbody').innerHTML=(state.commit_sequence||[]).map(c=>'<tr><td>'+c.seq+'</td><td>'+c.chunk_id+'</td><td>'+c.source+'</td><td>'+(c.txn_seq||'')+'</td><td><code>'+(c.cache_key||'').slice(0,16)+'</code></td></tr>').join('');
}
fetch('/api/state').then(r=>r.json()).then(j=>{state=j;render()}).catch(e=>{document.getElementById('meta').textContent='加载失败: '+e});
setInterval(()=>fetch('/api/state').then(r=>r.json()).then(j=>{state=j;render()}),2000);
</script>
</body></html>`
