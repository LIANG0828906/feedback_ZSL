package diagnostics

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Icon Segmentation Diagnostics</title>
<style>
:root{color-scheme:dark;--bg:#10151d;--panel:#19212d;--line:#304156;--ink:#edf4ff;--muted:#9cb0c9;--accent:#4cc2ff}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:14px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace}
main{max-width:1180px;margin:0 auto;padding:28px}.grid{display:grid;grid-template-columns:340px 1fr;gap:18px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:16px}
h1{font-size:20px;margin:0 0 6px}.muted{color:var(--muted)}label{display:block;margin-top:14px;color:var(--muted)}
input[type=range]{width:100%}output{color:var(--accent)}button{background:var(--accent);color:#07121a;border:0;border-radius:8px;padding:9px 14px;font-weight:700;cursor:pointer}
pre{white-space:pre-wrap;word-break:break-word;max-height:360px;overflow:auto;margin:0;color:#c9e6ff}
.views{display:grid;grid-template-columns:repeat(3,1fr);gap:12px}.views figure{margin:0;text-align:center}
.views img{width:100%;image-rendering:pixelated;border:1px solid var(--line);border-radius:8px;background:#071018}
.views figcaption{color:var(--muted);margin-top:6px}.row{display:flex;gap:16px;align-items:center;flex-wrap:wrap}
.metric{font-size:18px;color:var(--accent)}@media(max-width:840px){.grid{grid-template-columns:1fr}}
</style>
</head>
<body><main>
<h1>图标前景分离诊断台</h1>
<div class="muted">本地回环 · 固定种子 {{SEED}} · {{ICON}} · 预览预算 {{LATENCY}} ms</div>
<div class="grid" style="margin-top:18px">
  <section class="panel">
    <div class="row"><strong>预览参数</strong><output id="latency">-</output></div>
    <label>分水岭淹没阈值 <output id="floodOut">{{FLOOD}}</output></label>
    <input id="flood" type="range" min="0" max="1024" step="1" value="{{FLOOD}}">
    <label>区域合并代价上限 <output id="mergeOut">{{MERGE}}</output></label>
    <input id="merge" type="range" min="0" max="1024" step="1" value="{{MERGE}}">
    <div class="row" style="margin-top:18px"><button id="exact">停止交互并精确重算</button></div>
    <p class="muted">拖动时只调用低分辨率预览；释放滑块后执行全精度重算并做一致性判定。</p>
  </section>
  <section class="panel">
    <div class="row"><span class="muted">状态</span><strong id="status">等待预览</strong></div>
    <div class="views" style="margin-top:14px">
      <figure><img id="iconImg" src="/icon.png?icon=0"><figcaption>输入图标</figcaption></figure>
      <figure><img id="previewImg" src="/mask.png?kind=preview&icon=0&t=0"><figcaption>预览掩码</figcaption></figure>
      <figure><img id="truthImg" src="/mask.png?kind=truth&icon=0"><figcaption>参考掩码</figcaption></figure>
    </div>
    <pre id="output" style="margin-top:16px">拖动参数或点击精确重算。</pre>
  </section>
</div>
<script>
const $=id=>document.getElementById(id);let timer=0,last={};
function params(){return 'icon=0&flood='+encodeURIComponent($('flood').value)+'&merge_cost='+encodeURIComponent($('merge').value)}
function show(v){$('output').textContent=JSON.stringify(v,null,2)}
async function preview(){
  $('floodOut').textContent=$('flood').value;$('mergeOut').textContent=$('merge').value;
  try{
    const r=await fetch('/api/preview?'+params(),{cache:'no-store'});const v=await r.json();
    if(!r.ok){$('status').textContent=v.code||'错误';show(v);return}
    last=v;$('status').textContent='预览';$('latency').textContent=v.elapsed_ms.toFixed(2)+' ms / '+v.downsample+'x';
    $('previewImg').src='/mask.png?kind=preview&icon=0&t='+Date.now();show(v);
  }catch(e){show({error:String(e)})}
}
async function exact(){
  $('status').textContent='精确重算…';
  try{
    const r=await fetch('/api/exact?'+params(),{cache:'no-store'});const v=await r.json();
    if(!r.ok){$('status').textContent=v.code||'错误';show(v);return}
    $('status').textContent='已提交 '+v.committed_layers+'/'+v.total_layers;
    $('previewImg').src='/mask.png?kind=exact&icon=0&t='+Date.now();show(v.consistency.measured||v);
  }catch(e){show({error:String(e)})}
}
$('flood').addEventListener('input',()=>{clearTimeout(timer);timer=setTimeout(preview,35)});
$('merge').addEventListener('input',()=>{clearTimeout(timer);timer=setTimeout(preview,35)});
$('exact').addEventListener('click',exact);
preview();
</script>
</main></body></html>`
