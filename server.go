package dressbatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

type DiagnosticCase struct {
	CaseID    string     `json:"case_id"`
	Pose      string     `json:"pose"`
	Legal     bool       `json:"legal"`
	Accepted  []string   `json:"accepted_part_ids"`
	Conflicts []Conflict `json:"conflicts"`
}

type DiagnosticState struct {
	ProjectID        string              `json:"project_id"`
	ProjectName      string              `json:"project_name"`
	Constraints      BatchConstraints    `json:"constraints"`
	Cases            []DiagnosticCase    `json:"cases"`
	Result           *PackResult         `json:"result,omitempty"`
	Truth            *TruthReport        `json:"truth,omitempty"`
	Invalidation     *InvalidationReport `json:"invalidation,omitempty"`
	Scheduler        *SchedulerReport    `json:"scheduler,omitempty"`
	DependencyGraph  map[string][]string `json:"dependency_graph,omitempty"`
	MaterialVersions []MaterialSlot      `json:"material_versions"`
}

func BuildDiagnosticState(project *Project, result *PackResult) (*DiagnosticState, error) {
	state := &DiagnosticState{
		ProjectID:        project.ID,
		ProjectName:      project.Name,
		Constraints:      project.Constraints,
		MaterialVersions: append([]MaterialSlot(nil), project.Materials...),
	}
	sort.Slice(state.MaterialVersions, func(i, j int) bool {
		return state.MaterialVersions[i].ID < state.MaterialVersions[j].ID
	})
	for _, truthCase := range project.Truth.Assemblies {
		assembly, err := EvaluateAssembly(project, truthCase.CandidatePartIDs)
		if err != nil {
			return nil, err
		}
		state.Cases = append(state.Cases, DiagnosticCase{
			CaseID:    truthCase.ID,
			Pose:      truthCase.Pose,
			Legal:     assembly.Legal,
			Accepted:  append([]string(nil), assembly.AcceptedPartIDs...),
			Conflicts: append([]Conflict(nil), assembly.Conflicts...),
		})
	}
	state.Result = result
	if result != nil {
		state.DependencyGraph = BuildDependencyGraph(project, result)
	}
	return state, nil
}

func NewDiagnosticHandler(state *DiagnosticState) (http.Handler, error) {
	tmpl, err := template.New("diagnostics").Funcs(template.FuncMap{
		"percent": func(used, limit int) int {
			if limit <= 0 {
				return 0
			}
			value := used * 100 / limit
			if value > 100 {
				return 100
			}
			return value
		},
		"join": strings.Join,
	}).Parse(diagnosticTemplate)
	if err != nil {
		return nil, newError(CodeHTTP, "diagnostics", "template", "cannot parse template: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var body bytes.Buffer
		if err := tmpl.Execute(&body, state); err != nil {
			log.Printf("diagnostic template: %v", err)
			http.Error(w, "template failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(body.Bytes())
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(state); err != nil {
			http.Error(w, "encode failed", http.StatusInternalServerError)
		}
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux, nil
}

func ServeDiagnostics(ctx context.Context, addr string, state *DiagnosticState) error {
	handler, err := NewDiagnosticHandler(state)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return newError(CodeHTTP, "server", "shutdown", "cannot stop diagnostics server: %v", err)
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return newError(CodeHTTP, "server", "listen", "diagnostics server failed: %v", err)
	}
}

const diagnosticTemplate = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Dress Batch Diagnostics · {{.ProjectID}}</title>
<style>
:root { color-scheme: light dark; --bg:#0f172a; --panel:#172033; --line:#2e3b55; --text:#e5edf9; --muted:#9fb0c9; --ok:#31c48d; --warn:#f5a524; --bad:#f05252; }
* { box-sizing: border-box; }
body { margin:0; background:var(--bg); color:var(--text); font:14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace; }
main { max-width:1200px; margin:0 auto; padding:28px; }
h1 { font-size:22px; margin:0 0 4px; }
h2 { font-size:15px; margin:0 0 14px; letter-spacing:.04em; text-transform:uppercase; color:var(--muted); }
.sub { color:var(--muted); margin-bottom:24px; }
.grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(280px,1fr)); gap:16px; }
.panel { background:var(--panel); border:1px solid var(--line); border-radius:10px; padding:16px; margin-bottom:16px; }
.stats { display:grid; grid-template-columns:repeat(4,1fr); gap:8px; }
.stat { padding:10px; background:#111a2b; border-radius:7px; }
.stat b { display:block; font-size:17px; }
.muted { color:var(--muted); }
.ok { color:var(--ok); } .warn { color:var(--warn); } .bad { color:var(--bad); }
.batch { border:1px solid var(--line); border-radius:8px; padding:12px; margin:10px 0; }
.batch-head { display:flex; justify-content:space-between; align-items:center; gap:12px; }
.bar { height:8px; background:#0a1220; border-radius:99px; overflow:hidden; margin-top:6px; }
.bar > span { display:block; height:100%; background:linear-gradient(90deg,#31c48d,#2f74d0); }
.mono { white-space:pre-wrap; overflow-wrap:anywhere; }
table { width:100%; border-collapse:collapse; }
th,td { text-align:left; padding:7px 8px; border-bottom:1px solid var(--line); vertical-align:top; }
th { color:var(--muted); font-weight:500; }
.pill { display:inline-block; border:1px solid var(--line); border-radius:99px; padding:1px 8px; margin:2px 4px 2px 0; color:var(--muted); }
.chain { margin:0; padding-left:20px; }
.chain li { margin:5px 0; }
a { color:#7db4ff; }
@media (max-width:720px){ .stats{grid-template-columns:repeat(2,1fr)} main{padding:16px} }
</style>
</head>
<body>
<main>
  <h1>角色换装合批诊断</h1>
  <div class="sub">{{.ProjectID}} · {{.ProjectName}} · algorithm {{if .Result}}{{.Result.AlgorithmVersion}}{{else}}-{{end}}</div>

  <section class="panel">
    <h2>约束与状态</h2>
    <div class="stats">
      <div class="stat"><span class="muted">材质槽/批</span><b>{{.Constraints.MaxMaterialSlotsPerBatch}}</b></div>
      <div class="stat"><span class="muted">顶点预算/批</span><b>{{.Constraints.MaxVerticesPerBatch}}</b></div>
      <div class="stat"><span class="muted">索引预算/批</span><b>{{.Constraints.MaxIndicesPerBatch}}</b></div>
      <div class="stat"><span class="muted">跨蒙皮</span><b>{{if .Constraints.AllowCrossSkin}}允许{{else}}禁止{{end}}</b></div>
    </div>
  </section>

  <div class="grid">
    <section class="panel">
      <h2>装配场景与冲突</h2>
      <table><thead><tr><th>场景</th><th>姿态</th><th>判定</th><th>冲突</th></tr></thead><tbody>
      {{range .Cases}}<tr>
        <td>{{.CaseID}}</td><td>{{.Pose}}</td>
        <td>{{if .Legal}}<span class="ok">LEGAL</span>{{else}}<span class="bad">CONFLICT</span>{{end}}</td>
        <td>{{range .Conflicts}}<div><span class="bad">{{.Category}}</span> {{range $i, $p := .PartIDs}}{{if $i}}×{{end}}{{$p}}{{end}}{{if .MountID}} @{{.MountID}}{{end}}{{if .RuleID}} [{{.RuleID}}]{{end}}</div>{{else}}<span class="muted">无</span>{{end}}</td>
      </tr>{{end}}
      </tbody></table>
    </section>

    <section class="panel">
      <h2>材质槽版本</h2>
      <table><thead><tr><th>槽位</th><th>版本</th></tr></thead><tbody>
      {{range .MaterialVersions}}<tr><td>{{.ID}}</td><td>{{.Version}}</td></tr>{{end}}
      </tbody></table>
    </section>
  </div>

  <section class="panel">
    <h2>批次划分与预算占用</h2>
    {{if .Result}}{{range .Result.Batches}}
    <div class="batch">
      <div class="batch-head"><b>{{.ID}}</b><span class="muted">skin={{.Skin}} mixed={{.MixedSkin}} · {{join .PartIDs ", "}}</span></div>
      <div class="muted">slots: {{join .MaterialSlots ", "}}</div>
      <div>vertices {{.VertexUsed}} / {{$.Constraints.MaxVerticesPerBatch}} <span class="muted">({{percent .VertexUsed $.Constraints.MaxVerticesPerBatch}}%)</span></div>
      <div class="bar"><span style="width:{{percent .VertexUsed $.Constraints.MaxVerticesPerBatch}}%"></span></div>
      <div>indices {{.IndexUsed}} / {{$.Constraints.MaxIndicesPerBatch}} <span class="muted">({{percent .IndexUsed $.Constraints.MaxIndicesPerBatch}}%)</span></div>
      <div class="bar"><span style="width:{{percent .IndexUsed $.Constraints.MaxIndicesPerBatch}}%"></span></div>
      <details><summary class="muted">子网格偏移与依赖</summary>
        <table><thead><tr><th>part</th><th>submesh</th><th>slot</th><th>vertex</th><th>index</th></tr></thead><tbody>
        {{range .Placements}}{{range .Submeshes}}<tr><td>{{.PartID}}</td><td>{{.SubmeshID}}</td><td>{{.MaterialSlot}}</td><td>{{.VertexOffset}}–{{.VertexEnd}}</td><td>{{.IndexOffset}}–{{.IndexEnd}}</td></tr>{{end}}{{end}}
        </tbody></table>
        <div class="mono muted">{{join .Dependencies "\n"}}</div>
      </details>
    </div>
    {{end}}{{else}}<div class="muted">未提供合批结果</div>{{end}}
  </section>

  <div class="grid">
    <section class="panel">
      <h2>失效传播</h2>
      {{with .Invalidation}}
        <div>first invalid: <b>{{.FirstInvalid}}</b></div>
        <div>reused: {{range .ReusedBatchIDs}}<span class="pill">{{.}}</span>{{else}}<span class="muted">none</span>{{end}}</div>
        <ol class="chain">{{range .Entries}}<li><b>{{.BatchID}}</b> · parts {{join .AffectedParts ", "}}<div class="muted">{{join .Reasons " → "}}</div></li>{{else}}<li class="muted">无失效批次</li>{{end}}</ol>
      {{else}}<div class="muted">未执行增量重划分</div>{{end}}
    </section>

    <section class="panel">
      <h2>并行调度</h2>
      {{with .Scheduler}}
        <div class="stats">
          <div class="stat"><span class="muted">最大并行度</span><b>{{.MaxParallelism}}</b></div>
          <div class="stat"><span class="muted">每线程缓冲</span><b>{{.CandidateBufferPerTask}}</b></div>
          <div class="stat"><span class="muted">队列预算</span><b>{{.QueueBudgetParts}}</b></div>
          <div class="stat"><span class="muted">失败批次</span><b>{{range .Results}}{{if not .Valid}}1{{end}}{{end}}</b></div>
        </div>
        <table><thead><tr><th>批次</th><th>结果</th><th>尝试</th><th>错误</th></tr></thead><tbody>
        {{range .Results}}<tr><td>{{.BatchID}}</td><td>{{if .Valid}}<span class="ok">OK</span>{{else}}<span class="bad">FAILED</span>{{end}}</td><td>{{.Attempts}}</td><td class="muted">{{if .Error}}{{.Error}}{{end}}</td></tr>{{end}}
        </tbody></table>
        <h2 style="margin-top:16px">排队事件</h2>
        {{range .QueueEvents}}<div>{{.BatchID}} · {{.Reason}} · {{.WaitMillis}}ms · {{.QueuedParts}} parts</div>{{else}}<div class="muted">无排队等待</div>{{end}}
      {{else}}<div class="muted">未执行并行验证</div>{{end}}
    </section>
  </div>

  <section class="panel">
    <h2>依赖图</h2>
    <div class="mono">{{range $node,$deps := .DependencyGraph}}{{$node}} → {{join $deps ", "}}
{{end}}</div>
    <div class="muted" style="margin-top:10px"><a href="/api/state">/api/state</a> · <a href="/healthz">/healthz</a></div>
  </section>
</main>
</body>
</html>`

func DiagnosticAddr(host string, port int) string {
	if host == "" {
		host = "127.0.0.1"
	}
	if port <= 0 || port > 65535 {
		port = 8080
	}
	return fmt.Sprintf("%s:%d", host, port)
}
