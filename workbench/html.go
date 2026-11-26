package workbench

import "net/http"

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>IconFlow 诊断工作台</title>
<style>
:root{color-scheme:dark;font-family:ui-sans-serif,system-ui,-apple-system,"Segoe UI",sans-serif;background:#0b1020;color:#e5e7eb}
body{margin:0}header{padding:20px 28px;background:linear-gradient(120deg,#172554,#312e81 55%,#4c1d95);box-shadow:0 8px 30px #0005}
h1{margin:0 0 6px;font-size:22px}.sub{color:#c7d2fe;font-size:13px}
main{padding:22px 28px;display:grid;gap:18px}.card{background:#111827;border:1px solid #243044;border-radius:14px;padding:16px;box-shadow:0 10px 30px #0003}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center}button,select,input{border:1px solid #334155;background:#1e293b;color:#e5e7eb;border-radius:9px;padding:8px 12px}
button{cursor:pointer;font-weight:650}button:hover{background:#334155}.ok{color:#86efac}.bad{color:#fca5a5}.muted{color:#94a3b8}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:12px}.mask{font:10px/11px ui-monospace,SFMono-Regular,Menlo,monospace;white-space:pre;background:#020617;padding:8px;border-radius:8px;overflow:auto}
table{border-collapse:collapse;width:100%;font-size:12px}td,th{border-bottom:1px solid #243044;padding:7px 8px;text-align:left;vertical-align:top}th{color:#c4b5fd}
pre.json{max-height:420px;overflow:auto;background:#020617;border:1px solid #1e293b;border-radius:10px;padding:12px;font-size:12px}
.pill{display:inline-block;border-radius:999px;padding:2px 8px;background:#1e3a8a;color:#bfdbfe;font-size:12px;margin:2px}.pill.loss{background:#7f1d1d;color:#fecaca}
</style>
</head>
<body>
<header><h1>IconFlow 三版本图标分割诊断工作台</h1>
<div class="sub">确定性种子 · 整型换算 · 动态阈值 · 连通域 · SQLite/原子检查点 · 依赖失效 · 三路径一致性</div></header>
<main>
<section class="card"><div class="row">
<button onclick="loadOverview()">装载概览</button>
<button onclick="runConsistency()">运行三路径核对</button>
<label>字段 <select id="field"><option value="threshold_scale">阈值刻度</option><option value="channel_order">通道次序</option><option value="quant_step">量化步长</option><option value="operator_version">算子版本</option></select></label>
<label>范围 <select id="scope"><option value="block">单块</option><option value="global">全局</option></select></label>
<button onclick="invalidate()">生成失效清单并复用</button>
<button onclick="checkpoint()">检查 SQLite</button>
<span id="status" class="muted"></span>
</div></section>
<section id="overview" class="card"><h2>工程概览</h2><div class="muted">点击“装载概览”开始。</div></section>
<section id="consistency" class="card" style="display:none"><h2>逐块掩码与三路径比对</h2><div id="diff"></div><div id="blockGrid" class="grid"></div></section>
<section id="invalidation" class="card" style="display:none"><h2>精确失效传播</h2><div id="invSummary"></div><pre id="manifest" class="json"></pre></section>
<section id="checkpoint" class="card" style="display:none"><h2>检查点</h2><pre id="cp" class="json"></pre></section>
</main>
<script>
const $=id=>document.getElementById(id);
async function getJSON(url){$('status').textContent='运行中…';const r=await fetch(url);const data=await r.json();$('status').textContent='完成';if(!r.ok)throw data.error;return data}
function table(rows){return '<table><thead><tr><th>块</th><th>阈值</th><th>分位值</th><th>前景</th><th>区域</th><th>复用</th><th>校验</th></tr></thead><tbody>'+rows.map(b=>'<tr><td>'+b.coord+'</td><td>'+b.threshold+'</td><td>'+b.quantile_value+'/'+b.maximum_magnitude+'</td><td>'+b.foreground+' (marker '+b.markers+')</td><td>'+b.regions+'</td><td>'+(b.reused?'是':'重算')+'</td><td>'+(b.validation||'<span class=ok>OK</span>')+'</td></tr>').join('')+'</tbody></table>'}
function masks(blocks){return '<div class=grid>'+blocks.map(b=>'<div class=card><b>'+b.coord+'</b><div class=muted>thr='+b.threshold+', fg='+b.foreground+', regions='+b.regions+'</div><div class=mask>'+b.mask+'</div></div>').join('')+'</div>'}
async function loadOverview(){const d=await getJSON('/api/overview');$('overview').innerHTML='<h2>工程概览</h2><div><span class=pill>'+d.project_id+'</span><span class=pill>seed '+d.seed+'</span><span class=pill>'+d.block_count+' blocks</span><span class=pill>'+d.panel_count+' windows</span><span class=pill>'+d.dirty_count+' dirty</span></div>'+table(d.versions.map(v=>({coord:v.version,threshold:'',quantile_value:'',foreground:v.blocks,regions:v.images,reused:v.loaded,validation:v.error||('upgrade '+v.upgrade_records+', trace '+v.traces)})))+'<h3>低版本默认补齐</h3><pre class=json>'+JSON.stringify(d.legacy_upgrades,null,2)+'</pre><h3>脏块记录</h3><pre class=json>'+JSON.stringify(d.dirty,null,2)+'</pre>'}
async function runConsistency(){const d=await getJSON('/api/consistency?workers=2&queue=4&buffer=1024');$('consistency').style.display='block';$('diff').innerHTML=d.ok?'<p class=ok>✓ realtime / resume / full 逐像素与提交序列一致；stage commits='+d.commit_entries+'</p>':'<p class=bad>✗ '+JSON.stringify(d.diff)+'</p>';$('blockGrid').innerHTML=masks(d.realtime)}
async function invalidate(){const f=$('field').value,s=$('scope').value;const d=await getJSON('/api/invalidate?field='+f+'&scope='+s);$('invalidation').style.display='block';$('invSummary').innerHTML='<p>目标：<b>'+d.target_block+'</b>；失效块 '+d.manifest.blocks.length+'；复用块 '+d.reused_blocks.length+'；重算块 '+d.recomputed_blocks.length+'</p><div class=row>'+d.reused_blocks.map(x=>'<span class="pill ok">复用 '+x+'</span>').join('')+d.recomputed_blocks.map(x=>'<span class="pill loss">重算 '+x+'</span>').join('')+'</div>';$('manifest').textContent=JSON.stringify(d.manifest,null,2)}
async function checkpoint(){const d=await getJSON('/api/checkpoint');$('checkpoint').style.display='block';$('cp').textContent=JSON.stringify(d,null,2)}
</script>
</body></html>`

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}
