package diagnostics

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"time"
)

// NewHandler serves a self-contained local diagnostics page.
func NewHandler(report *Report) http.Handler {
	mux := http.NewServeMux()
	page := template.Must(template.New("diagnostics").Parse(diagnosticsPage))
	mux.HandleFunc("/", func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := page.Execute(writer, report); err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/api/report", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		encoder := json.NewEncoder(writer)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(report)
	})
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, request *http.Request) {
		status := "ok"
		if !report.Pass() {
			status = "failed"
		}
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(writer, status)
	})
	return mux
}

// ListenLoopback refuses non-loopback bind addresses. The caller owns the
// listener and can use port 0 for an ephemeral local port.
func ListenLoopback(address string) (net.Listener, error) {
	if address == "" {
		address = "127.0.0.1:0"
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("diagnostics must bind to a loopback address, got %q", host)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return listener, nil
}

func NewServer(report *Report) *http.Server {
	return &http.Server{
		Handler:           NewHandler(report),
		ReadHeaderTimeout: 5 * time.Second,
	}
}

const diagnosticsPage = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Skill Budget Diagnostics</title>
<style>
:root { color-scheme: dark; --bg:#0e1620; --panel:#172435; --ok:#45d483; --bad:#ff6b6b; --muted:#93a4b8; --line:#2a3c52; }
body { margin:0; font:14px/1.5 ui-monospace,SFMono-Regular,Consolas,monospace; background:var(--bg); color:#e8eef6; }
main { max-width:1180px; margin:0 auto; padding:28px; }
h1 { font-size:24px; margin:0 0 4px; }
h2 { font-size:16px; margin:24px 0 10px; color:#b9cbe0; }
.muted { color:var(--muted); }
.url { font-size:12px; color:var(--muted); margin-bottom:20px; }
.cards { display:grid; grid-template-columns:repeat(auto-fit,minmax(180px,1fr)); gap:10px; }
.card,.panel { background:var(--panel); border:1px solid var(--line); border-radius:8px; }
.card { padding:12px; }
.card b { display:block; font-size:20px; margin-top:4px; }
.panel { padding:14px; overflow:auto; }
table { width:100%; border-collapse:collapse; }
th,td { text-align:left; padding:8px; border-bottom:1px solid var(--line); vertical-align:top; }
th { color:var(--muted); font-weight:500; }
.ok { color:var(--ok); } .bad { color:var(--bad); }
code { color:#ffe08a; }
</style>
</head>
<body><main>
<h1>技能冷却与资源预算编排内核</h1>
<div class="url">仅监听 127.0.0.1 回环地址 · <a href="/api/report">/api/report</a> · <a href="/healthz">/healthz</a></div>
<div class="cards">
  <div class="card">Seed<b>{{.Seed}}</b></div>
  <div class="card">Ticks<b>{{.TotalTicks}}</b></div>
  <div class="card">Windows<b>{{.WindowCount}}</b></div>
  <div class="card">Decisions<b>{{.DecisionCount}}</b></div>
  <div class="card">Rule hash<b><code>{{.RuleHash}}</code></b></div>
  <div class="card">Overall<b class="{{if .Pass}}ok{{else}}bad{{end}}">{{if .Pass}}PASS{{else}}FAIL{{end}}</b></div>
</div>
<h2>验收检查</h2>
<div class="panel"><table><tr><th>检查</th><th>结果</th><th>说明</th></tr>
{{range .Checks}}<tr><td>{{.Name}}</td><td class="{{if .Pass}}ok{{else}}bad{{end}}">{{if .Pass}}PASS{{else}}FAIL{{end}}</td><td>{{.Detail}}</td></tr>{{end}}
</table></div>
<h2>三版本加载</h2>
<div class="panel"><table><tr><th>版本</th><th>哈希一致</th><th>真值一致</th><th>哈希</th></tr>
{{range .Versions}}<tr><td>v{{.Version}}</td><td class="{{if .SemanticMatch}}ok{{else}}bad{{end}}">{{.SemanticMatch}}</td><td class="{{if .TruthMatch}}ok{{else}}bad{{end}}">{{.TruthMatch}}</td><td><code>{{.RuleHash}}</code></td></tr>{{end}}
</table></div>
<h2>版本往返</h2>
<div class="panel"><table><tr><th>路径</th><th>语义一致</th><th>E_LOSSY 条目</th><th>补齐痕迹</th><th>说明</th></tr>
{{range .RoundTrips}}<tr><td>{{.Name}}</td><td class="{{if .HashMatch}}ok{{else}}bad{{end}}">{{.HashMatch}}</td><td>{{.Losses}}</td><td>{{.FillTraces}}</td><td>{{.Detail}}</td></tr>{{end}}
</table></div>
<h2>执行路径</h2>
<div class="panel"><table><tr><th>路径</th><th>完成</th><th>真值一致</th><th>恢复 tick</th><th>恢复窗口</th><th>说明</th></tr>
{{range .Paths}}<tr><td>{{.Name}}</td><td>{{.Complete}}</td><td class="{{if .MatchTruth}}ok{{else}}bad{{end}}">{{.MatchTruth}}</td><td>{{.ResumedTick}}</td><td>{{.ResumedWindows}}</td><td>{{.Detail}}</td></tr>{{end}}
</table></div>
<h2>争用裁决样例</h2>
<div class="panel"><table><tr><th>Tick</th><th>技能</th><th>争用</th><th>结果</th><th>原因</th></tr>
{{range .Conflicts}}<tr><td>{{.Tick}}</td><td>{{.SkillID}}</td><td>{{.ConflictType}}</td><td>{{.Result}}</td><td>{{.Reason}}</td></tr>{{end}}
</table></div>
</main></body></html>`
