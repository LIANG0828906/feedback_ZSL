package lipsync

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RenderHTML produces a self-contained local diagnostic page. No CDN, font,
// script, or network request is required.
func RenderHTML(report DemoReport) ([]byte, error) {
	data, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("marshal report for html: %w", err)
	}
	safe := strings.NewReplacer("<", "\\u003c", ">", "\\u003e", "&", "\\u0026").Replace(string(data))
	return []byte(htmlHead + safe + htmlTail), nil
}

const htmlHead = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>口型同步校准诊断台</title>
<style>
:root{--bg:#0c1117;--panel:#121a23;--line:#253342;--text:#e8eef5;--muted:#8da2b5;--accent:#5bd6c4;--warn:#ffb454;--bad:#ff6b7a;--good:#7ee787;--mono:ui-monospace,SFMono-Regular,Consolas,monospace}
*{box-sizing:border-box}body{margin:0;background:linear-gradient(180deg,#0c1117,#101923 45%,#0b1016);color:var(--text);font:14px/1.55 Inter,Segoe UI,Microsoft YaHei,sans-serif}
header{padding:28px clamp(18px,4vw,56px) 12px;border-bottom:1px solid var(--line)}
h1{margin:0 0 8px;font-size:clamp(24px,3vw,38px);letter-spacing:-.03em}h2{margin:0 0 12px;font-size:18px}h3{margin:0 0 8px;font-size:15px}
.sub{color:var(--muted);max-width:980px}.layout{display:grid;grid-template-columns:250px minmax(0,1fr);gap:22px;padding:22px clamp(18px,4vw,56px) 56px}
.sidebar{position:sticky;top:16px;align-self:start}.panel{background:rgba(18,26,35,.9);border:1px solid var(--line);border-radius:14px;padding:16px;margin-bottom:16px;box-shadow:0 12px 30px rgba(0,0,0,.16)}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(140px,1fr));gap:10px}.card{background:#0d151d;border:1px solid var(--line);border-radius:12px;padding:12px}.card b{display:block;font-size:22px;margin-top:4px}.muted{color:var(--muted)}.mono{font-family:var(--mono)}
.btn{display:block;width:100%;text-align:left;border:1px solid var(--line);background:#0e1720;color:var(--text);padding:10px 12px;border-radius:9px;margin:7px 0;cursor:pointer}.btn.active{border-color:var(--accent);background:#10251f;color:#d8fff7}.btn small{display:block;color:var(--muted)}
.meta{display:flex;flex-wrap:wrap;gap:8px;margin:8px 0 14px}.chip{border:1px solid var(--line);border-radius:999px;padding:4px 9px;color:var(--muted);font-family:var(--mono);font-size:12px}.chip.ok{color:var(--good);border-color:#245c38}.chip.bad{color:var(--bad);border-color:#642c37}
table{width:100%;border-collapse:collapse;font-size:12.5px}th,td{padding:7px 8px;border-bottom:1px solid #1c2935;text-align:left;vertical-align:top}th{color:var(--muted);font-weight:600;position:sticky;top:0;background:#111a23}tr:hover td{background:#101b24}
.scroll{max-height:460px;overflow:auto;border:1px solid var(--line);border-radius:10px}.timeline{padding:8px;overflow:auto}.seg{display:grid;grid-template-columns:72px minmax(620px,1fr);align-items:center;gap:8px;margin:7px 0}.lane{height:38px;position:relative;background:repeating-linear-gradient(90deg,#101923 0,#101923 24px,#15212c 24px,#15212c 25px);border:1px solid var(--line);border-radius:8px}
.phoneme{position:absolute;top:5px;bottom:5px;border-radius:5px;padding:2px 4px;overflow:hidden;white-space:nowrap;font-size:10px;border:1px solid rgba(255,255,255,.16);min-width:2px}
.phoneme.silent{background:#334155;color:#dbeafe}.phoneme.closed{background:#7c3aed;color:#f5f3ff}.phoneme.press{background:#6d28d9;color:#f5f3ff}.phoneme.round{background:#0f766e;color:#ccfbf1}.phoneme.wide{background:#2563eb;color:#eff6ff}.phoneme.smile{background:#be123c;color:#fff1f2}.phoneme.open{background:#b45309;color:#fffbeb}.phoneme.transition{outline:2px solid var(--warn)}
.frameaxis{display:flex;justify-content:space-between;color:var(--muted);font:10px var(--mono);padding:0 2px}
.inval{display:grid;grid-template-columns:100px 1fr 150px;gap:8px;align-items:center;margin:6px 0}.bar{height:14px;background:#0b131b;border:1px solid var(--line);border-radius:5px;position:relative}.bar i{position:absolute;top:0;bottom:0;background:linear-gradient(90deg,#ff6b7a,#ffb454);border-radius:4px}
.grid2{display:grid;grid-template-columns:repeat(auto-fit,minmax(300px,1fr));gap:16px}.errorbox{border-left:3px solid var(--bad);padding-left:10px;margin:8px 0}.ok{color:var(--good)}.bad{color:var(--bad)}.warn{color:var(--warn)}
.legend{display:flex;gap:10px;flex-wrap:wrap;font-size:11px;color:var(--muted)}.legend i{display:inline-block;width:9px;height:9px;border-radius:2px;margin-right:4px}
@media(max-width:860px){.layout{grid-template-columns:1fr}.sidebar{position:static}.seg{grid-template-columns:58px minmax(480px,1fr)}}
</style>
</head>
<body>
<header>
  <h1>字幕与口型对时校准诊断台</h1>
  <div class="sub">确定性生成器 → 逐帧解析真值 → 实时 / 断点恢复 / 全量重算三路径对拍。页面完全离线，数据来自同目录 <span class="mono">report.json</span>。</div>
</header>
<div class="layout">
  <aside class="sidebar">
    <div class="panel"><h2>对白</h2><div id="dialogue-list"></div></div>
    <div class="panel"><h3>图例</h3><div class="legend" id="legend"></div></div>
  </aside>
  <main>
    <section class="panel"><h2 id="dialogue-title"></h2><div id="summary-cards" class="cards"></div><div id="dialogue-meta" class="meta"></div></section>
    <section class="panel"><h2>音素与帧边界叠加</h2><div class="timeline" id="timeline"></div><div class="frameaxis" id="frameaxis"></div></section>
    <section class="panel"><h2>逐帧字幕区间与口型</h2><div class="scroll"><table id="frames-table"></table></div></section>
    <section class="panel"><h2>三路径一致性</h2><div class="grid2"><div><table id="paths-table"></table></div><div><table id="compare-table"></table></div></div></section>
    <section class="panel"><h2>精确失效传播</h2><div id="invalidations"></div><div class="grid2" style="margin-top:14px"><div><table id="scenario-table"></table></div><div id="queue-events"></div></div></section>
    <section class="panel"><h2>依赖图摘要</h2><div id="dependency-summary"></div></section>
    <section class="panel"><h2>异常注入链路</h2><div id="injections"></div></section>
  </main>
</div>
<script id="report-data" type="application/json">`

const htmlTail = `</script>
<script>
(function(){
  const R = JSON.parse(document.getElementById('report-data').textContent);
  const lips = ['silent','closed','press','round','wide','smile','open'];
  const esc = v => String(v == null ? '' : v).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const pct = (v,total) => total <= 0 ? 0 : Math.max(0, Math.min(100, 100*v/total));
  document.getElementById('legend').innerHTML = lips.map(x => '<span><i class="phoneme '+x+'" style="position:static;display:inline-block"></i>'+x+'</span>').join('');
  const summary = document.getElementById('summary-cards');
  summary.innerHTML = [
    ['对白', R.summary.dialogues], ['总帧数', R.summary.frames], ['过渡帧', R.summary.transitions],
    ['三路径一致', R.summary.all_paths_equal ? 'PASS' : 'FAIL'], ['真值一致', R.summary.all_truth_equal ? 'PASS' : 'FAIL'],
    ['错误链路', R.summary.injected_matched + '/' + R.summary.injected_failures]
  ].map(x => '<div class="card"><span class="muted">'+esc(x[0])+'</span><b>'+esc(x[1])+'</b></div>').join('');
  const list = document.getElementById('dialogue-list');
  list.innerHTML = R.dialogues.map((d,i) => {
    const ok = d.comparisons.every(c => c.consistent);
    return '<button class="btn" data-i="'+i+'">'+esc(d.project.dialogue_id)+' <small>'+d.project.fps+' fps · '+d.truth.frame_count+' frames · '+(ok?'一致':'不一致')+'</small></button>';
  }).join('');
  function render(index){
    document.querySelectorAll('#dialogue-list .btn').forEach((b,i)=>b.classList.toggle('active', i===index));
    const d = R.dialogues[index], frames = d.truth.frames, total = d.truth.frame_count, text = Array.from(d.project.subtitle.text);
    document.getElementById('dialogue-title').textContent = d.project.dialogue_id + ' · ' + d.project.subtitle.text.replace(/\n/g,' ⏎ ');
    document.getElementById('dialogue-meta').innerHTML = [
      ['fps',d.project.fps], ['sample_rate',d.project.sample_rate], ['quant',d.project.quantization.direction+' / '+d.project.quantization.version],
      ['frames',total], ['min_readable',d.project.min_readable_frames], ['tolerance',d.project.lip_transition_tolerance],
      ['content',d.truth.content_hash.slice(0,16)+'…']
    ].map(x=>'<span class="chip">'+esc(x[0])+'='+esc(x[1])+'</span>').join('');
    document.getElementById('timeline').innerHTML = d.project.segments.map((seg,si) => {
      const phs = d.timing.phonemes.filter(p => p.segment_index === si);
      const blocks = phs.map(p => {
        const left = pct(p.start_frame,total), width = Math.max(0.35,pct(p.end_frame-p.start_frame,total));
        const transition = frames[p.start_frame] && frames[p.start_frame].transition ? ' transition' : '';
        return '<div class="phoneme '+p.lip+transition+'" title="'+esc(p.id+' '+p.start_sample+'→'+p.end_sample+' frame '+p.start_frame+'→'+p.end_frame)+'" style="left:'+left+'%;width:'+width+'%">'+esc(p.id)+'</div>';
      }).join('');
      return '<div class="seg"><div class="mono muted">'+esc(seg.id)+'</div><div class="lane">'+blocks+'</div></div>';
    }).join('');
    const axis = '<span>frame 0</span><span>'+total+'</span>';
    document.getElementById('frameaxis').innerHTML = axis;
    document.getElementById('frames-table').innerHTML =
      '<thead><tr><th>帧</th><th>字符区间</th><th>字符</th><th>基础口型</th><th>呈现口型</th><th>过渡</th><th>音素</th></tr></thead><tbody>' +
      frames.map(f => { const ch=text[f.char_start]||''; return '<tr><td class="mono">'+f.frame+'</td><td class="mono">['+f.char_start+','+f.char_end+')</td><td>'+esc(ch==='\n'?'⏎':ch)+'</td><td>'+esc(f.base_lip)+'</td><td class="'+(f.transition?'warn':'')+'">'+esc(f.lip)+'</td><td>'+(f.transition?'YES':'—')+'</td><td class="mono">'+esc(f.phoneme_id)+'</td></tr>'; }).join('') + '</tbody>';
    const paths = ['realtime','resume','full'].map(name => {
      const p=d[name]; return '<tr><td>'+name+'</td><td>'+p.frame_count+'</td><td>'+p.reused_frames+'</td><td>'+p.recomputed_frames+'</td><td>'+p.committed_windows+'</td></tr>';
    }).join('');
    document.getElementById('paths-table').innerHTML = '<thead><tr><th>路径</th><th>帧</th><th>复用</th><th>重算</th><th>提交窗口</th></tr></thead><tbody>'+paths+'</tbody>';
    document.getElementById('compare-table').innerHTML = '<thead><tr><th>比对</th><th>结果</th><th>首差</th></tr></thead><tbody>'+
      d.comparisons.map(c=>'<tr><td>'+esc(c.name)+'</td><td class="'+(c.consistent?'ok':'bad')+'">'+(c.consistent?'PASS':'FAIL')+'</td><td class="mono">'+(c.mismatch?esc('frame '+c.mismatch.frame+' '+c.mismatch.field):'—')+'</td></tr>').join('')+'</tbody>';
    const inval = d.incremental.invalidations || [];
    document.getElementById('invalidations').innerHTML = inval.length ? inval.map(x => {
      const left=pct(x.frame_start,total), width=Math.max(.4,pct(x.frame_end-x.frame_start,total));
      return '<div class="inval"><span class="mono">['+x.frame_start+','+x.frame_end+')</span><div class="bar"><i style="left:'+left+'%;width:'+width+'%"></i></div><span class="mono muted">'+esc(x.reasons.join(','))+'</span></div>';
    }).join('') : '<span class="muted">无失效帧</span>';
    document.getElementById('scenario-table').innerHTML = '<thead><tr><th>场景</th><th>失效</th><th>复用</th><th>重算</th><th>输出</th></tr></thead><tbody>'+
      d.invalidation_scenarios.map(s=>'<tr><td>'+esc(s.name)+'</td><td>'+s.incremental.invalidated_frames+'</td><td>'+s.incremental.reused_frames+'</td><td>'+s.incremental.recomputed_frames+'</td><td class="'+(s.incremental.output_match?'ok':'bad')+'">'+(s.incremental.output_match?'PASS':'FAIL')+'</td></tr>').join('')+'</tbody>';
    document.getElementById('queue-events').innerHTML = '<h3>有界帧缓冲排队</h3>' + (d.queue_events && d.queue_events.length ? '<div class="scroll"><table><thead><tr><th>窗口</th><th>帧</th><th>原因</th></tr></thead><tbody>'+d.queue_events.map(q=>'<tr><td>'+q.window+'</td><td>'+q.frame+'</td><td class="mono">'+esc(q.reason)+'</td></tr>').join('')+'</tbody></table></div>' : '<span class="muted">本次调度未触发阻塞</span>');
    const kinds = {}; d.dependency_graph.nodes.forEach(n=>kinds[n.kind]=(kinds[n.kind]||0)+1);
    document.getElementById('dependency-summary').innerHTML = '<div class="cards">'+Object.keys(kinds).map(k=>'<div class="card"><span class="muted">'+esc(k)+'</span><b>'+kinds[k]+'</b></div>').join('')+'<div class="card"><span class="muted">edges</span><b>'+d.dependency_graph.edges.length+'</b></div></div><p class="muted">每个帧节点接收 timing、音素区间与字幕字符上游边；缓存键包含内容哈希、帧号、量化版本与全部上游键。</p>';
  }
  list.addEventListener('click', e => { const b=e.target.closest('button[data-i]'); if(b) render(Number(b.dataset.i)); });
  document.getElementById('injections').innerHTML = R.injections.map(x => {
    const loc = x.error ? x.error.field + (x.error.frame != null ? ' @ frame '+x.error.frame : '') : '';
    return '<div class="errorbox"><b class="'+(x.matched?'ok':'bad')+'">'+esc(x.fault.toUpperCase())+' → '+esc(x.actual_code||'NO_ERROR')+'</b> <span class="muted">期望 '+esc(x.expected_code)+' · '+esc(x.dialogue_id)+' · '+esc(loc)+'</span><div class="mono">'+esc(x.error?x.error.message:'')+'</div></div>';
  }).join('');
  if(R.dialogues.length) render(0);
})();
</script>
</body>
</html>
`
