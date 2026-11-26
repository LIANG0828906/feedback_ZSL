package budget

import (
	"bytes"
	"html/template"
	"sort"
)

type BudgetRow struct {
	SkillID      string `json:"skill_id"`
	Casts        int    `json:"casts"`
	Spent        BP     `json:"spent"`
	ShareBP      int64  `json:"share_bp"`
	PoolPercent  int64  `json:"pool_percent"`
	SharePercent int64  `json:"share_percent"`
}

type BudgetSummary struct {
	ResourceID string      `json:"resource_id"`
	Max        BP          `json:"max"`
	TotalSpent BP          `json:"total_spent"`
	Rows       []BudgetRow `json:"rows"`
}

type DiagnosticsData struct {
	Project       *Project
	Report        *RunReport
	Plan          *InvalidationPlan
	Budget        BudgetSummary
	GeneratedNote string
}

func BuildBudget(project *Project, report *RunReport) BudgetSummary {
	casts := map[string]int{}
	spent := map[string]BP{}
	total := BP(0)
	for _, tick := range report.Ticks {
		for _, cast := range tick.Casts {
			if cast.Accepted {
				casts[cast.SkillID]++
				spent[cast.SkillID] += cast.CostPaid
				total += cast.CostPaid
			}
		}
	}
	ids := sortedSkillIDs(project.Skills)
	rows := make([]BudgetRow, 0, len(ids))
	for _, id := range ids {
		amount := spent[id]
		row := BudgetRow{SkillID: id, Casts: casts[id], Spent: amount}
		if total > 0 {
			row.ShareBP = int64(amount) * 10000 / int64(total)
			row.SharePercent = row.ShareBP / 100
		}
		if project.Resource.Max > 0 {
			row.PoolPercent = int64(amount) * 100 / int64(project.Resource.Max)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Spent > rows[j].Spent })
	return BudgetSummary{ResourceID: project.Resource.ID, Max: project.Resource.Max, TotalSpent: total, Rows: rows}
}

const diagnosticsTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>技能预算与冷却排程诊断</title>
<style>
:root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif; }
body { margin: 0; background: #0f172a; color: #e2e8f0; }
main { max-width: 1280px; margin: 0 auto; padding: 28px; }
h1 { margin: 0 0 8px; font-size: 28px; }
h2 { margin-top: 34px; border-left: 4px solid #38bdf8; padding-left: 10px; }
.meta, .cards { display: grid; gap: 12px; grid-template-columns: repeat(auto-fit, minmax(210px, 1fr)); }
.card { background: #1e293b; border: 1px solid #334155; border-radius: 12px; padding: 14px; }
.card b { display: block; color: #38bdf8; font-size: 13px; margin-bottom: 6px; }
code { color: #fde68a; word-break: break-all; }
table { width: 100%; border-collapse: collapse; background: #111827; border-radius: 12px; overflow: hidden; font-size: 13px; }
th, td { border-bottom: 1px solid #243044; padding: 8px 10px; text-align: left; vertical-align: top; }
th { background: #1e293b; position: sticky; top: 0; }
.ok { color: #86efac; font-weight: 700; }
.no { color: #fca5a5; font-weight: 700; }
.pill { display: inline-block; border-radius: 999px; padding: 2px 8px; margin: 1px; background: #334155; }
.accepted { background: #14532d; color: #bbf7d0; }
.rejected { background: #7f1d1d; color: #fecaca; }
.bar { height: 12px; background: #334155; border-radius: 999px; overflow: hidden; min-width: 120px; }
.bar > i { display: block; height: 100%; background: linear-gradient(90deg,#38bdf8,#22c55e); }
.muted { color: #94a3b8; }
</style>
</head>
<body>
<main>
  <h1>技能资源预算与冷却排程诊断</h1>
  <p class="muted">仅使用 Go 标准库生成；所有预算单位为内部 permyriad/BP，时间单位为 tick。</p>
  <section class="meta">
    <div class="card"><b>工程</b>{{.Project.ID}}<br><code>{{.Project.ContentHash}}</code></div>
    <div class="card"><b>源版本 / 算法</b>{{.Report.SourceVersion}}<br>{{.Report.AlgorithmVersion}} / {{.Report.ParamVersion}}</div>
    <div class="card"><b>Tick 数 / 窗口</b>{{len .Report.Ticks}} ticks<br>{{len .Report.Commits}} commits</div>
    <div class="card"><b>预算总消耗</b>{{.Budget.TotalSpent}} BP<br>池上限 {{.Budget.Max}} BP</div>
  </section>

  <h2>预算占用</h2>
  <table>
    <thead><tr><th>技能</th><th>施放次数</th><th>消耗 BP</th><th>占总消耗</th><th>相对池上限</th><th>占用条</th></tr></thead>
    <tbody>
    {{range .Budget.Rows}}
      <tr>
        <td><code>{{.SkillID}}</code></td><td>{{.Casts}}</td><td>{{.Spent}}</td>
        <td>{{.ShareBP}}‱</td><td>{{.PoolPercent}}%</td>
        <td><div class="bar"><i style="width:{{if gt .SharePercent 100}}100{{else}}{{.SharePercent}}{{end}}%"></i></div></td>
      </tr>
    {{end}}
    </tbody>
  </table>

  <h2>逐 Tick 状态</h2>
  <table>
    <thead><tr><th>Tick</th><th>资源</th><th>回复</th><th>消耗</th><th>意图偏移</th><th>技能可用 / 冷却</th><th>共享组</th></tr></thead>
    <tbody>
    {{range .Report.Ticks}}
      <tr>
        <td><b>{{.Tick}}</b></td>
        <td>{{.ResourceCurrent}} / {{.ResourceMax}}</td>
        <td class="ok">+{{.RegenApplied}}</td>
        <td class="no">-{{.Spent}}</td>
        <td>{{.ConsumedIntentOffset}}</td>
        <td>{{range .Skills}}<span class="pill">{{.SkillID}}: <span class="{{if .Available}}ok{{else}}no{{end}}">{{if .Available}}可施放{{else}}不可用{{end}}</span>/CD{{.CooldownRemaining}}</span>{{end}}</td>
        <td>{{range .Groups}}<span class="pill">{{.GroupID}} CD{{.CooldownRemaining}}</span>{{end}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>

  <h2>裁决明细</h2>
  <table>
    <thead><tr><th>Tick</th><th>技能</th><th>争用类型</th><th>结果</th><th>原因</th><th>依据</th></tr></thead>
    <tbody>
    {{range $group := .Report.Arbitration}}
      {{range $r := $group}}
      <tr>
        <td>{{$r.Tick}}</td><td><code>{{$r.SkillID}}</code></td><td>{{$r.Contention}}</td>
        <td><span class="pill {{$r.Decision}}">{{$r.Decision}}</span></td>
        <td>{{$r.Reason}}</td><td class="muted">{{$r.Basis}}</td>
      </tr>
      {{end}}
    {{end}}
    </tbody>
  </table>

  <h2>精确失效清单</h2>
  {{if .Plan}}
  <table>
    <thead><tr><th>变更节点</th><th>Tick 区间</th><th>失效 Tick</th><th>传播节点数</th></tr></thead>
    <tbody>
    {{range .Plan.Entries}}
      <tr>
        <td><code>{{.NodeID}}</code><div class="muted">{{.Reason}}</div></td>
        <td>{{range .Ranges}}[{{.First}}..{{.Last}}] {{end}}</td>
        <td>{{.Ticks}}</td>
        <td>{{len .AffectedNodes}}</td>
      </tr>
    {{end}}
    </tbody>
  </table>
  <p class="muted">可复用 Tick：{{.Plan.ReusableTicks}}；总失效 Tick：{{.Plan.InvalidTicks}}</p>
  {{else}}<p class="muted">本次演示未传入失效计划。</p>{{end}}

  <h2>窗口提交序列</h2>
  <table><thead><tr><th>窗口</th><th>Tick 区间</th><th>意图偏移</th><th>文件</th></tr></thead><tbody>
  {{range .Report.Commits}}<tr><td>{{.WindowIndex}}</td><td>{{.FirstTick}}..{{.LastTick}}</td><td>{{.IntentOffset}}</td><td><code>{{.Path}}</code></td></tr>{{end}}
  </tbody></table>
</main>
</body></html>`

func RenderDiagnosticsHTML(data DiagnosticsData) ([]byte, error) {
	tmpl, err := template.New("diagnostics").Parse(diagnosticsTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func WriteDiagnostics(path string, data DiagnosticsData) error {
	htmlBytes, err := RenderDiagnosticsHTML(data)
	if err != nil {
		return err
	}
	return WriteAtomicBytes(path, htmlBytes)
}
