package web

import (
	"encoding/json"
	"html/template"
)

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"json": func(v any) (template.JS, error) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return "", err
		}
		return template.JS(b), nil
	},
}).Parse(indexHTML))

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Mask QC Stroke Workbench</title>
<style>
:root { --bg:#0f172a; --panel:#111c33; --panel2:#17233f; --line:#2b3b5f; --text:#e5edff; --muted:#9db0d0; --ok:#22c55e; --warn:#f59e0b; --bad:#ef4444; --info:#38bdf8; }
* { box-sizing:border-box; }
body { margin:0; background:#eef3fb; color:#172033; font:14px/1.45 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
header { background:linear-gradient(120deg,#102044,#1e3a8a); color:white; padding:18px 24px; }
header h1 { margin:0 0 4px; font-size:22px; }
header p { margin:0; color:#c7d7fe; }
main { padding:18px 24px 60px; max-width:1500px; margin:0 auto; }
.grid { display:grid; grid-template-columns:repeat(12,1fr); gap:16px; }
.card { background:white; border:1px solid #d9e2f2; border-radius:14px; padding:16px; box-shadow:0 8px 24px rgba(30,58,138,.08); }
.card h2 { margin:0 0 12px; font-size:16px; display:flex; align-items:center; gap:8px; }
.span12 { grid-column:span 12; } .span8{grid-column:span 8;} .span7{grid-column:span 7;} .span6{grid-column:span 6;} .span5{grid-column:span 5;} .span4{grid-column:span 4;}
@media(max-width:1100px){.span8,.span7,.span6,.span5,.span4{grid-column:span 12;}}
table { border-collapse:collapse; width:100%; font-size:12px; }
th,td { border-bottom:1px solid #e4eaf6; padding:6px 7px; text-align:left; vertical-align:top; }
th { background:#f4f7fd; position:sticky; top:0; z-index:1; }
code,.mono { font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; }
.muted { color:#64748b; } .ok{color:#15803d;font-weight:700;} .bad{color:#b91c1c;font-weight:700;} .warn{color:#b45309;font-weight:700;}
.pill { display:inline-block; border-radius:999px; padding:2px 8px; font-size:12px; font-weight:700; }
.pill.ok{background:#dcfce7;} .pill.bad{background:#fee2e2;} .pill.warn{background:#fef3c7;} .pill.info{background:#e0f2fe;color:#075985;}
.row { display:flex; gap:8px; flex-wrap:wrap; align-items:center; }
input,select,button { font:inherit; border-radius:8px; border:1px solid #c8d4e9; padding:6px 8px; background:white; }
button,.btn { cursor:pointer; background:#1d4ed8; color:white; border-color:#1d4ed8; text-decoration:none; display:inline-block; }
button.secondary,.btn.secondary{background:#475569;border-color:#475569;} button.danger{background:#b91c1c;border-color:#b91c1c;}
.images { display:flex; gap:14px; flex-wrap:wrap; }
.maskbox { text-align:center; } .maskbox img { width:256px; max-width:100%; image-rendering:pixelated; border:1px solid #cbd5e1; border-radius:8px; background:white; }
.maskbox .small img{width:160px;}
.tiles { display:grid; grid-template-columns:repeat(auto-fill,minmax(145px,1fr)); gap:6px; }
.tile { border:1px solid #cbd5e1; border-radius:8px; padding:6px; font-size:11px; background:#f8fafc; }
.tile.affected { background:#fee2e2; border-color:#ef4444; } .tile.reusable { background:#dcfce7; border-color:#22c55e; }
.flow { display:flex; align-items:center; gap:10px; flex-wrap:wrap; margin:10px 0; }
.node { border:1px solid #93c5fd; background:#eff6ff; border-radius:10px; padding:8px 10px; font-weight:700; }
.node.bad{border-color:#ef4444;background:#fee2e2;} .node.ok{border-color:#22c55e;background:#dcfce7;} .node.warn{border-color:#f59e0b;background:#fef3c7;}
.arrow{font-size:20px;color:#64748b;}
pre { background:#0b1220; color:#d7e3ff; padding:12px; border-radius:10px; overflow:auto; max-height:360px; font-size:11px; }
.scroll { max-height:430px; overflow:auto; border:1px solid #e2e8f0; border-radius:10px; }
.flash { padding:10px 12px; border-radius:10px; margin-bottom:14px; } .flash.ok{background:#dcfce7;} .flash.error{background:#fee2e2;}
.stat { display:grid; grid-template-columns:repeat(4,1fr); gap:8px; margin-bottom:12px; }
.stat div { background:#f6f8fd; border:1px solid #e0e8f6; border-radius:10px; padding:10px; }
.stat b { display:block; font-size:18px; }
form.inline { display:inline-flex; gap:4px; align-items:center; }
form.inline input { width:78px; padding:3px 5px; }
.tiny { font-size:11px; padding:3px 6px; border-radius:6px; }
</style>
</head>
<body>
<header>
  <h1>美术切图掩码质检 · 笔画级诊断工作台</h1>
  <p>固定种子 {{.Project.Seed}} · {{.Project.Name}} · 源 Schema {{.Project.Schema}} · 次序 {{.Versions.order}} / 换算 {{.Versions.conversion}} / 算法 {{.Versions.algorithm}}</p>
</header>
<main>
{{if .Flash}}<div class="flash {{.Status}}"><b>{{if eq .Status "ok"}}操作完成{{else}}诊断/错误{{end}}：</b><span class="mono">{{.Flash}}</span></div>{{end}}
<div class="grid">

<section class="card span12">
  <h2>验收路径</h2>
  <div class="stat">
    <div><span class="muted">三路径一致性</span><b class="{{if .Three.OK}}ok{{else}}bad{{end}}">{{if .Three.OK}}PASS{{else}}FAIL{{end}}</b></div>
    <div><span class="muted">逻辑提交事件</span><b>{{.Three.Events}}</b></div>
    <div><span class="muted">连通域数量</span><b>{{.Three.Domains}}</b></div>
    <div><span class="muted">末轮复用分块</span><b>{{.Three.ReusedTiles}}/{{.Three.TotalTiles}}</b></div>
  </div>
  {{if .Three.Error}}<pre>{{.Three.Error}}</pre>{{else}}<p class="muted">实时增量、SQLite 断点恢复、一次性全量重算的积算掩码、域统计、方向类别与分块提交序列已自动比对。末态掩码哈希 <code>{{.Three.MaskHash}}</code>。</p>{{end}}
  <div class="row">
    <a class="btn secondary" href="/api/state">JSON 状态</a>
    <a class="btn secondary" href="/api/budget-demo" target="_blank">预算耗尽演示 JSON</a>
    <a class="btn secondary" href="/corrupt/schema">触发损坏 Schema 输入</a>
    <a class="btn secondary" href="/corrupt/checkpoint">触发并检测检查点损坏</a>
    <a class="btn secondary" href="/export?schema=icon-project/v1">导出 V1</a>
    <a class="btn secondary" href="/export?schema=icon-project/v2">导出 V2</a>
    <a class="btn secondary" href="/export?schema=icon-project/v3">导出 V3</a>
    <form method="post" action="/reset"><button class="danger">重置固定样本与数据库</button></form>
  </div>
</section>

<section class="card span5">
  <h2>图标与掩码</h2>
  <form class="row" method="get" action="/">
    <label>图标
      <select name="icon" onchange="this.form.submit()">
      {{$current := .Icon.ID}}
      {{range .Icons}}<option value="{{.ID}}" {{if eq .ID $current}}selected{{end}}>{{.ID}} ({{.Width}}×{{.Height}})</option>{{end}}
      </select>
    </label>
    <noscript><button>切换</button></noscript>
  </form>
  <div class="images" style="margin-top:12px">
    <div class="maskbox"><div>积算掩码</div><img src="/mask.png?icon={{.Icon.ID}}&kind=accum&scale=8" alt="accumulated"></div>
    <div class="maskbox"><div>连通域/方向标注</div><img src="/mask.png?icon={{.Icon.ID}}&kind=domains&scale=8" alt="domains"></div>
    <div class="maskbox small"><div>候选掩码</div><img src="/mask.png?icon={{.Icon.ID}}&kind=candidate&scale=5" alt="candidate"></div>
    <div class="maskbox small"><div>分块真值</div><img src="/mask.png?icon={{.Icon.ID}}&kind=truth&scale=5" alt="truth"></div>
  </div>
  {{if .Result.Topology}}
  <p style="margin-top:12px">拓扑核验：
    {{if .Result.Topology.Issues}}<span class="pill bad">{{len .Result.Topology.Issues}} 个 E_TOP/偏差项</span>{{else}}<span class="pill ok">在容差内</span>{{end}}
    <span class="muted">容差：面积 {{.Result.Topology.Tolerance.AreaPermille}}‰，质心 {{.Result.Topology.Tolerance.CentroidPixels}}px，环宽 {{.Result.Topology.Tolerance.BandWidthPixels}}px。</span>
  </p>
  {{end}}
  {{if .Result.TileErrors}}
  <div class="flash error"><b>失败分块：</b>
    {{range .Result.TileErrors}}<span class="pill bad">{{.TileID}} {{.Code}}</span>
      <a class="btn tiny ok" href="/retry-tile?icon={{$.Icon.ID}}&tile={{.TileID}}">重试 {{.TileID}}</a>
    {{end}}
  </div>
  {{end}}
  <form class="row" method="get" action="/fail-tile">
    <input type="hidden" name="icon" value="{{.Icon.ID}}">
    <input name="tile" placeholder="注入失败分块 ID" style="width:220px">
    <button class="danger tiny">注入一次失败</button>
  </form>
</section>

<section class="card span7">
  <h2>笔画列表与精确编辑</h2>
  <div class="scroll">
  <table>
    <thead><tr><th>笔ID</th><th>序/优先级/组</th><th>起→止</th><th>半径/硬度‰</th><th>权重‱</th><th>方向</th><th>编辑</th></tr></thead>
    <tbody>
    {{$iconID := .Icon.ID}}
    {{range .Icon.Strokes}}
      <tr>
        <td><code>{{.ID}}</code><br><span class="muted">{{.SourceSchema}}</span></td>
        <td>#{{.Seq}} / P{{.Priority}}<br><span class="muted">{{.Group}}</span></td>
        <td>({{.Start.X}},{{.Start.Y}})→({{.End.X}},{{.End.Y}})</td>
        <td>{{.Radius}} / {{.HardnessPermille}}</td>
        <td>{{.WeightPermyriad}}</td>
        <td><span class="pill info">{{.DeclaredDirection}}</span></td>
        <td>
          <form class="inline" method="post" action="/edit/weight">
            <input type="hidden" name="icon_id" value="{{$iconID}}">
            <input type="hidden" name="stroke_id" value="{{.ID}}">
            <input name="weight" value="{{.WeightPermyriad}}" min="0" max="10000">
            <button class="tiny">权重</button>
          </form>
          <div class="row" style="margin-top:4px">
            <form class="inline" method="post" action="/edit/nudge"><input type="hidden" name="icon_id" value="{{$iconID}}"><input type="hidden" name="stroke_id" value="{{.ID}}"><input type="hidden" name="dx" value="-1"><input type="hidden" name="dy" value="0"><button class="tiny secondary">←</button></form>
            <form class="inline" method="post" action="/edit/nudge"><input type="hidden" name="icon_id" value="{{$iconID}}"><input type="hidden" name="stroke_id" value="{{.ID}}"><input type="hidden" name="dx" value="0"><input type="hidden" name="dy" value="-1"><button class="tiny secondary">↑</button></form>
            <form class="inline" method="post" action="/edit/nudge"><input type="hidden" name="icon_id" value="{{$iconID}}"><input type="hidden" name="stroke_id" value="{{.ID}}"><input type="hidden" name="dx" value="0"><input type="hidden" name="dy" value="1"><button class="tiny secondary">↓</button></form>
            <form class="inline" method="post" action="/edit/nudge"><input type="hidden" name="icon_id" value="{{$iconID}}"><input type="hidden" name="stroke_id" value="{{.ID}}"><input type="hidden" name="dx" value="1"><input type="hidden" name="dy" value="0"><button class="tiny secondary">→</button></form>
          </div>
        </td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  <form class="row" style="margin-top:12px" method="post" action="/edit/insert">
    <input type="hidden" name="icon_id" value="{{.Icon.ID}}">
    <b>插入笔画</b>
    <label>x1 <input name="x1" value="8" size="3"></label>
    <label>y1 <input name="y1" value="28" size="3"></label>
    <label>x2 <input name="x2" value="38" size="3"></label>
    <label>y2 <input name="y2" value="28" size="3"></label>
    <label>半径 <input name="radius" value="1" size="2"></label>
    <label>权重 <input name="weight" value="10000" size="5"></label>
    <button>插入并增量核验</button>
  </form>
</section>

<section class="card span12">
  <h2>连通域统计与方向类别</h2>
  <div class="scroll">
  <table>
    <thead><tr><th>域编号</th><th>面积</th><th>重心</th><th>外接框</th><th>孔洞</th><th>环带宽度(min/max)</th><th>方向类别</th><th>笔画法向投票</th></tr></thead>
    <tbody>
    {{range .Result.Analysis.Domains}}
      <tr>
        <td><b>D{{.No}}</b></td>
        <td>{{.PixelCount}}</td>
        <td class="mono">{{printf "%.2f" .CentroidX}},{{printf "%.2f" .CentroidY}}</td>
        <td class="mono">[{{.Bounds.MinX}},{{.Bounds.MinY}}]-[{{.Bounds.MaxX}},{{.Bounds.MaxY}})</td>
        <td>{{.HoleCount}}</td>
        <td>{{.BandWidth}} ({{.BandWidthMin}}/{{.BandWidthMax}})</td>
        <td><span class="pill info">{{.Direction}}</span></td>
        <td><details><summary>查看 {{len .StrokeChecks}} 笔</summary><pre>{{json .StrokeChecks}}</pre></details></td>
      </tr>
    {{end}}
    </tbody>
  </table>
  </div>
  {{if .Result.Topology.Issues}}<details style="margin-top:10px"><summary class="bad">拓扑/方向偏差明细</summary><pre>{{json .Result.Topology.Issues}}</pre></details>{{end}}
</section>

<section class="card span7">
  <h2>失效传播示意与依赖图</h2>
  {{if .Latest}}
    <p><b>{{.Latest.StrokeID}}</b>：{{.Latest.ImpactScope}}</p>
    <div class="flow">
      <div class="node bad">笔画 {{.Latest.StrokeID}}<br><span class="tiny">{{.Latest.OldContentHash}} → {{.Latest.NewContentHash}}</span></div>
      <span class="arrow">→</span>
      <div class="node warn">{{len .Latest.AffectedTiles}} 个失效分块</div>
      <span class="arrow">→</span>
      <div class="node bad">topology + direction</div>
    </div>
    <p class="muted">未受影响分块直接命中 SQLite 持久化键：{{len .Latest.ReusableTiles}} 个。</p>
  {{else}}<p class="muted">尚未发生编辑；下表绿色为可复用分块，红色为最近一次编辑失效分块。</p>{{end}}
  <div class="tiles">
    {{range .TileViews}}
      <div class="tile {{if .Affected}}affected{{else if $.Latest}}reusable{{end}}">
        <b>{{.ID}}</b><br>
        {{if .Affected}}失效/重算{{else if $.Latest}}缓存复用{{else}}已裁决{{end}}
      </div>
    {{end}}
  </div>
  <details style="margin-top:12px"><summary>完整依赖图节点/边 JSON</summary><pre>{{json .Graph}}</pre></details>
  <details><summary>历次失效清单</summary><pre>{{json .Invalidations}}</pre></details>
</section>

<section class="card span5">
  <h2>预算占用与并行调度</h2>
  <div class="stat">
    <div><span class="muted">最大并行</span><b>{{.Budget.Config.MaxParallel}}</b></div>
    <div><span class="muted">在途分块</span><b>{{.Budget.OutstandingTiles}}</b></div>
    <div><span class="muted">峰值队列</span><b>{{.Budget.PeakOutstanding}}</b></div>
    <div><span class="muted">失败</span><b class="{{if .Budget.Failed}}bad{{else}}ok{{end}}">{{.Budget.Failed}}</b></div>
  </div>
  <p class="muted">每线程像素缓冲上限 {{.Budget.Config.MaxPixelsPerWorker}}；当前缓冲 {{.Budget.ActiveBufferPixels}}，峰值 {{.Budget.PeakBufferPixels}}；已处理 {{.Budget.Processed}}。队列预算 {{.Budget.Config.MaxQueuedTiles}}。</p>
  <div class="scroll"><table><thead><tr><th>分块</th><th>原因</th><th>等待 ms</th></tr></thead><tbody>
  {{range .Budget.Waits}}<tr><td class="mono">{{.TileID}}</td><td>{{.Reason}}</td><td>{{.WaitedMS}}</td></tr>{{end}}
  </tbody></table></div>
</section>

<section class="card span12">
  <h2>SQLite 笔画级提交历史（最近 40 条）</h2>
  <div class="scroll">
  <table>
    <thead><tr><th>DB ID</th><th>次序</th><th>Event Key</th><th>Icon</th><th>Stroke</th><th>Stroke Version</th><th>UTC Time</th></tr></thead>
    <tbody>
    {{range .Commits}}<tr>
      <td>{{.ID}}</td><td>{{.OrderIndex}}</td><td class="mono">{{.EventKey}}</td><td class="mono">{{.IconID}}</td><td>{{.StrokeID}}</td><td class="mono">{{printf "%.16s" .StrokeVersion}}</td><td class="muted">{{.CreatedAt}}</td>
    </tr>{{end}}
    </tbody>
  </table>
  </div>
</section>

</div>
</main>
</body></html>`
