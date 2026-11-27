package web

const pageHTML = `
{{define "layout_start"}}
<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>{{.Title}} - 技能排程工作台</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;margin:0;background:#0f1420;color:#e6ebf5}
nav{background:#172033;padding:12px 24px;display:flex;gap:18px;flex-wrap:wrap}
nav a{color:#8ec5ff;text-decoration:none;font-weight:600}
main{padding:24px;max-width:1100px;margin:0 auto}
h1{font-size:22px} h2{font-size:17px;color:#9fb4d8;margin-top:28px}
table{border-collapse:collapse;width:100%;margin-top:10px;font-size:13px}
th,td{border:1px solid #2a3852;padding:7px 9px;text-align:left}
th{background:#1d2942;color:#bcd2f5}
tr:nth-child(even){background:#141c2e}
.ok{color:#54d18c;font-weight:700}.bad{color:#ff7a7a;font-weight:700}
.pill{display:inline-block;padding:2px 8px;border-radius:10px;background:#243450;font-size:12px}
.card{background:#141c2e;border:1px solid #243450;border-radius:10px;padding:16px;margin-top:14px}
code{color:#ffd479}
</style></head><body>
<nav>
<a href="/">场景总览</a>
<a href="/ticks?tick=0">逐 Tick 诊断</a>
<a href="/compare">三路径比对</a>
<a href="/invalidate">失效传播</a>
<a href="/checkpoint">检查点</a>
<a href="/api/truth">真值 JSON</a>
</nav><main>
{{end}}
{{define "layout_end"}}</main></body></html>{{end}}

{{define "overview"}}
{{template "layout_start" .}}
<h1>{{.Title}} · 场景 {{.Scenario.Name}} · 种子 {{.Scenario.Seed}}</h1>
<div class="card">
<p>Horizon={{.Scenario.Rules.Params.Horizon}} ticks，窗口={{.Scenario.Rules.Params.WindowSize}}，意图数={{.Intents}}</p>
<h2>资源池</h2>
<table><tr><th>ID</th><th>名称</th><th>容量</th><th>回复常量表</th></tr>
{{range .Pools}}<tr><td>{{.ID}}</td><td>{{.Name}}</td><td>{{.Capacity}}</td><td><code>{{.Regen.Const}} + 系数项</code></td></tr>{{end}}
</table>
<h2>技能（内部 tick / 万分比）</h2>
<table><tr><th>ID</th><th>资源池</th><th>消耗</th><th>冷却(tick)</th><th>CDR</th><th>上界</th><th>共享组</th><th>优先级</th></tr>
{{range .Skills}}<tr><td>{{.ID}}</td><td>{{.PoolID}}</td><td>{{.Cost}}</td><td>{{.Cooldown}}</td>
<td>{{.CDR}}</td><td>{{.CDRCap}}</td><td>{{.SharedGroup}}</td><td>{{.Priority}}</td></tr>{{end}}
</table>
<h2>共享冷却组</h2>
<table><tr><th>ID</th><th>组锁(tick)</th><th>成员</th></tr>
{{range .Groups}}<tr><td>{{.ID}}</td><td>{{.GroupCooldown}}</td><td>{{range .Members}}<span class="pill">{{.}}</span> {{end}}</td></tr>{{end}}
</table>
</div>
{{template "layout_end" .}}
{{end}}

{{define "ticks"}}
{{template "layout_start" .}}
<h1>{{.Title}} — tick {{.Tick.Tick}} / {{.Total}}</h1>
<p>
<a href="/ticks?tick={{.Prev}}">← 上一个</a> |
<a href="/ticks?tick={{.Next}}">下一个 →</a>
</p>
<div class="card">
<h2>资源池余量</h2>
<table><tr><th>池</th><th>余量</th></tr>
{{range .PoolKeys}}<tr><td>{{.}}</td><td>{{index $.Tick.Pools .}}</td></tr>{{end}}
</table>
<h2>技能状态</h2>
<table><tr><th>技能</th><th>可用</th><th>冷却剩余</th><th>池余量</th><th>状态</th></tr>
{{range .Tick.Skills}}<tr><td>{{.SkillID}}</td>
<td>{{if .Available}}<span class="ok">是</span>{{else}}<span class="bad">否</span>{{end}}</td>
<td>{{.CooldownRem}}</td><td>{{.PoolRemain}}</td><td><span class="pill">{{.State}}</span></td></tr>{{end}}
</table>
<h2>裁决明细（{{len .Tick.Arbitrations}}）</h2>
<table><tr><th>争用</th><th>技能</th><th>胜出</th><th>抑制</th><th>依据</th></tr>
{{range .Tick.Arbitrations}}<tr><td>{{.Contention}}</td><td>{{.SkillID}}</td><td>{{.Winner}}</td><td>{{.Suppressed}}</td><td>{{.Basis}}</td></tr>{{end}}
</table>
</div>
{{template "layout_end" .}}
{{end}}

{{define "compare"}}
{{template "layout_start" .}}
<h1>{{.Title}}</h1>
<div class="card">
<table><tr><th>路径对比</th><th>结论</th><th>首个不一致</th></tr>
{{range .Diffs}}<tr><td>{{.Name}}</td>
<td>{{if .OK}}<span class="ok">逐 tick 一致</span>{{else}}<span class="bad">不一致</span>{{end}}</td>
<td>{{if .OK}}—{{else}}tick={{.Tick}} skill={{.Skill}} field={{.Field}}{{end}}</td></tr>{{end}}
</table>
<h2>线程池排队诊断</h2>
<table><tr><th>序号</th><th>任务</th><th>工作线程</th><th>排队原因</th><th>等待</th><th>错误</th></tr>
{{range .Queue}}<tr><td>{{.Seq}}</td><td>{{.JobID}}</td><td>{{.Worker}}</td><td>{{.QueueReason}}</td><td>{{.Wait}}</td><td>{{.Err}}</td></tr>{{end}}
</table>
</div>
{{template "layout_end" .}}
{{end}}

{{define "invalidate"}}
{{template "layout_start" .}}
<h1>{{.Title}} — 编辑节点 {{.Node}}（tick 10-19）</h1>
<div class="card">
<h2>失效清单（{{len .Items}}）</h2>
<table><tr><th>节点</th><th>区间</th><th>原因</th></tr>
{{range .Items}}<tr><td>{{.NodeID}}</td><td>{{.TickFrom}}-{{.TickTo}}</td><td>{{.Reason}}</td></tr>{{end}}
</table>
<p>编辑前缓存 tick 数={{len .Before}}，编辑后仍命中 tick 数={{len .After}}（区间外复用持久化结果）。</p>
</div>
{{template "layout_end" .}}
{{end}}

{{define "checkpoint"}}
{{template "layout_start" .}}
<h1>{{.Title}}</h1>
<div class="card">
<p>算法版本 <code>{{.Algo}}</code>，内容哈希 <code>{{.Hash}}</code>，重启加载窗口数={{.Loaded}}</p>
<table><tr><th>窗口</th><th>tick 区间</th><th>状态</th></tr>
{{range .Windows}}<tr><td>{{.Index}}</td><td>{{.From}}-{{.To}}</td><td><span class="pill">{{.State}}</span></td></tr>{{end}}
</table>
</div>
{{template "layout_end" .}}
{{end}}
`
