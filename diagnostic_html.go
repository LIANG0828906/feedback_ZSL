package quantforensics

import (
	"html/template"
	"os"
)

const diagnosticHTMLTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>量化色差取证诊断面板</title>
<style>
:root{color-scheme:light dark;font-family:Inter,"Noto Sans SC",system-ui,sans-serif}
body{margin:0;background:#0b1020;color:#e5e7eb}
main{max-width:1180px;margin:0 auto;padding:28px}
h1{margin:0 0 6px;font-size:28px} h2{margin-top:34px;border-left:4px solid #38bdf8;padding-left:10px}
.muted{color:#94a3b8}.ok{color:#22c55e}.bad{color:#fb7185}.warn{color:#fbbf24}
.cards{display:grid;grid-template-columns:repeat(auto-fit,minmax(170px,1fr));gap:12px;margin:20px 0}
.card{background:#111827;border:1px solid #243044;border-radius:14px;padding:16px;box-shadow:0 8px 24px #0003}
.card .num{font-size:26px;font-weight:750;margin-top:6px}.card .label{color:#94a3b8;font-size:13px}
table{border-collapse:collapse;width:100%;margin:12px 0;background:#111827;border-radius:12px;overflow:hidden}
th,td{border-bottom:1px solid #243044;padding:9px 10px;text-align:left;vertical-align:top;font-size:14px}
th{background:#172033;color:#cbd5e1} code{color:#7dd3fc}
.pill{display:inline-block;border-radius:999px;padding:2px 9px;font-size:12px;background:#123b2e;color:#86efac}
.pill.bad{background:#4c1d24;color:#fda4af}.pill.warn{background:#422006;color:#fcd34d}
.grid2{display:grid;grid-template-columns:1fr 1fr;gap:16px}@media(max-width:900px){.grid2{grid-template-columns:1fr}}
pre{white-space:pre-wrap;word-break:break-all;background:#020617;padding:12px;border-radius:10px;border:1px solid #1e293b}
</style>
</head>
<body><main>
<h1>色差返工取证诊断面板</h1>
<div class="muted">生成时间 {{.Report.GeneratedAtUTC}} · 工程 <code>{{.Report.ProjectID}}</code> · 固定种子 <code>{{.Report.Seed}}</code></div>
<p>总体结论：
{{if .Report.OK}}<span class="pill">验收检查通过</span>{{else}}<span class="pill bad">存在验证错误</span>{{end}}
基线三路径一致：<b class="{{if .Report.BaselineThreePaths.Consistent}}ok{{else}}bad{{end}}">{{.Report.BaselineThreePaths.Consistent}}</b>；
编辑序列三路径一致：<b class="{{if .Report.EditedThreePaths.Consistent}}ok{{else}}bad{{end}}">{{.Report.EditedThreePaths.Consistent}}</b>；
容差测量不随收紧改变：<b class="ok">{{.Report.TightToleranceMeasurementsStable}}</b>。</p>

<section class="cards">
<div class="card"><div class="label">截图 / 块</div><div class="num">{{.Report.ImageCount}} / {{.Report.BlockCount}}</div></div>
<div class="card"><div class="label">Schema 互操作</div><div class="num">{{len .Report.SchemaInterop}}/3</div></div>
<div class="card"><div class="label">基线提交阶段</div><div class="num">{{.Report.BaselineThreePaths.Realtime.CommitStages}}</div></div>
<div class="card"><div class="label">失效步骤</div><div class="num">{{len .Report.Invalidation.Steps}}</div></div>
<div class="card"><div class="label">SQLite 文件</div><div class="num">{{len .Report.SQLiteFiles}}</div></div>
</section>

<h2>固定种子截图样本</h2>
<div>{{range .Report.ScreenshotFiles}}<figure style="display:inline-block;margin:0 16px 12px 0;text-align:center"><img src="{{.}}" alt="{{.}}" width="160" height="160" style="image-rendering:pixelated;border:1px solid #334155;border-radius:10px;background:#020617"><figcaption class="muted"><code>{{.}}</code></figcaption></figure>{{end}}</div>

<h2>三版本 Schema</h2>
<table><tr><th>版本</th><th>字节</th><th>加载痕迹</th><th>降级损失</th><th>语义往返</th><th>来源标识</th></tr>
{{range .Report.SchemaInterop}}<tr><td><code>{{.Version}}</code></td><td>{{.Bytes}}</td><td>{{.TraceCount}}</td><td>{{.LossCount}}</td><td>{{if .SemanticEqual}}<span class="pill">equal</span>{{else}}<span class="pill bad">mismatch</span>{{end}}</td><td><code>{{.LoadedVersion}}</code></td></tr>{{end}}
<tr><td>未知版本错误字段</td><td colspan="5"><code>{{.Report.SchemaErrorFieldPath}}</code> → E_SCHEMA</td></tr>
</table>

<h2>偏差级别取证口径</h2>
<table><tr><th>注入样例</th><th>判定层级</th><th>规则</th><th>索引不一致</th><th>R/G/B 偏差</th><th>位深包络</th></tr>
{{with .Report.Triage.Coordinate}}<tr><td>块坐标换算</td><td><span class="pill warn">{{.Level}}</span></td><td>{{.Rule}}</td><td>{{.IndexMismatches}}</td><td>{{.MaxDeviation}}</td><td>{{.BitDepthBound}}</td></tr>{{end}}
{{with .Report.Triage.Palette}}<tr><td>调色板量化</td><td><span class="pill bad">{{.Level}}</span></td><td>{{.Rule}}</td><td>{{.IndexMismatches}}</td><td>{{.MaxDeviation}}</td><td>{{.BitDepthBound}}</td></tr>{{end}}
{{with .Report.Triage.BitDepth}}<tr><td>位深量化</td><td><span class="pill">{{.Level}}</span></td><td>{{.Rule}}</td><td>{{.IndexMismatches}}</td><td>{{.MaxDeviation}}</td><td>{{.BitDepthBound}}</td></tr>{{end}}
</table>

<h2>三路径逐像素一致</h2>
{{template "pathTable" .Report.BaselineThreePaths}}
<h3>同一编辑序列</h3>
{{template "pathTable" .Report.EditedThreePaths}}

<h2>块类别判定</h2>
<table><tr><th>类别</th><th>块数</th><th>通过</th><th>不通过</th><th>结论</th><th>样例</th><th>R/G/B 最大偏差</th><th>加权色差</th></tr>
{{range .Report.Categories}}<tr><td><code>{{.Kind}}</code></td><td>{{.Blocks}}</td><td class="ok">{{.Passed}}</td><td class="{{if .Failed}}bad{{end}}">{{.Failed}}</td><td>{{.Conclusion}}</td><td><code>{{.ExampleBlockID}}</code></td><td>{{.MaxDeviation}}</td><td>{{.WeightedDiff}}</td></tr>{{end}}
</table>

<h2>脏块依赖图与精确失效</h2>
{{range .Report.Invalidation.Steps}}
<h3>{{.Name}} <span class="muted"><code>{{.EditPath}}</code></span></h3>
<table><tr><th>实际失效块</th><td>{{len .Report.AffectedBlocks}}</td><th>复用块</th><td>{{len .Report.ReusedBlocks}}</td><th>新计算块</th><td>{{len .ComputedBlocks}}</td></tr>
<tr><th>失效清单与原因</th><td colspan="5"><pre>{{range $id,$reason := .Report.BlockReasons}}{{$id}} — {{$reason}}
{{end}}</pre></td></tr></table>
{{end}}

<h2>预算排队与故障隔离</h2>
<div class="grid2">
<div class="card"><div class="label">全量重算排队事件（前 20 条）</div>
<table><tr><th>块</th><th>原因</th><th>等待 ns</th></tr>{{range .Report.QueueWaitSample}}<tr><td><code>{{.BlockID}}</code></td><td>{{.Reason}}</td><td>{{.WaitedNanos}}</td></tr>{{end}}</table></div>
<div class="card"><div class="label">单块故障重试</div><p>故障块 <code>{{.Report.FaultRetry.BlockID}}</code></p><p>首次失败：<span class="pill bad">{{.Report.FaultRetry.FirstFailedBlocks}}</span></p><p>重试后失败：<span class="pill ok">{{.Report.FaultRetry.RetryFailedBlocks}}</span></p><p>量化尝试次数：{{.Report.FaultRetry.Attempts}}</p><p>结论：<b class="ok">{{.Report.FaultRetry.OK}}</b></p></div>
</div>

<h2>生成器面板脏块记录</h2>
<table><tr><th>面板</th><th>截图</th><th>块</th><th>原因</th><th>编辑路径</th></tr>
{{range .Sample.DirtyRecords}}<tr><td><code>{{.PanelID}}</code></td><td><code>{{.ImageID}}</code></td><td><code>{{.BlockID}}</code></td><td>{{.Reason}}</td><td><code>{{.EditPath}}</code></td></tr>{{end}}
</table>

<h2>SQLite 原子检查点文件</h2>
<table><tr><th>路径</th><th>字节</th><th>阶段记录数</th></tr>{{range .Report.SQLiteFiles}}<tr><td><code>{{.Path}}</code></td><td>{{.Bytes}}</td><td>{{.StageRecords}}</td></tr>{{end}}</table>

{{if .Report.ValidationErrors}}<h2 class="bad">验证错误</h2><pre>{{range .Report.ValidationErrors}}{{.}}
{{end}}</pre>{{end}}
</main></body></html>

{{define "pathTable"}}
<table><tr><th>路径</th><th>结果块</th><th>提交阶段</th><th>计算块</th><th>复用块</th><th>失败</th><th>排队事件</th><th>并行度/缓冲/队列</th><th>中断</th></tr>
<tr><td>实时 {{.Realtime.Mode}}</td><td>{{.Realtime.Outcomes}}</td><td>{{.Realtime.CommitStages}}</td><td>{{.Realtime.ComputedBlocks}}</td><td>{{.Realtime.ReusedBlocks}}</td><td>{{.Realtime.FailedBlocks}}</td><td>{{.Realtime.QueueWaitEvents}}</td><td>{{.Realtime.MaxParallelism}} / {{.Realtime.PixelBufferCap}} / {{.Realtime.QueueBudget}}</td><td>{{.Realtime.Interrupted}}</td></tr>
<tr><td>中断前缀 {{.RecoveryPartial.Mode}}</td><td>{{.RecoveryPartial.Outcomes}}</td><td>{{.RecoveryPartial.CommitStages}}</td><td>{{.RecoveryPartial.ComputedBlocks}}</td><td>{{.RecoveryPartial.ReusedBlocks}}</td><td>{{.RecoveryPartial.FailedBlocks}}</td><td>{{.RecoveryPartial.QueueWaitEvents}}</td><td>{{.RecoveryPartial.MaxParallelism}} / {{.RecoveryPartial.PixelBufferCap}} / {{.RecoveryPartial.QueueBudget}}</td><td>{{.RecoveryPartial.Interrupted}}</td></tr>
<tr><td>断点恢复 {{.Recovery.Mode}}</td><td>{{.Recovery.Outcomes}}</td><td>{{.Recovery.CommitStages}}</td><td>{{.Recovery.ComputedBlocks}}</td><td>{{.Recovery.ReusedBlocks}}</td><td>{{.Recovery.FailedBlocks}}</td><td>{{.Recovery.QueueWaitEvents}}</td><td>{{.Recovery.MaxParallelism}} / {{.Recovery.PixelBufferCap}} / {{.Recovery.QueueBudget}}</td><td>{{.Recovery.Interrupted}}</td></tr>
<tr><td>全量重算 {{.FullRecompute.Mode}}</td><td>{{.FullRecompute.Outcomes}}</td><td>{{.FullRecompute.CommitStages}}</td><td>{{.FullRecompute.ComputedBlocks}}</td><td>{{.FullRecompute.ReusedBlocks}}</td><td>{{.FullRecompute.FailedBlocks}}</td><td>{{.FullRecompute.QueueWaitEvents}}</td><td>{{.FullRecompute.MaxParallelism}} / {{.FullRecompute.PixelBufferCap}} / {{.FullRecompute.QueueBudget}}</td><td>{{.FullRecompute.Interrupted}}</td></tr>
</table>
{{if .FirstInconsistency}}<pre class="bad">{{.FirstInconsistency}}</pre>{{end}}
{{end}}`

type htmlDiagnosticData struct {
	Report DiagnosticReport
	Sample SampleSet
}

func writeDiagnosticHTML(path string, report *DiagnosticReport, sample SampleSet) error {
	tmpl, err := template.New("diagnostic").Parse(diagnosticHTMLTemplate)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, htmlDiagnosticData{Report: *report, Sample: sample})
}
