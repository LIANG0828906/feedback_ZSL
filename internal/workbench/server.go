// Package workbench serves the local deterministic diagnostic dashboard.
package workbench

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/diagnostic"
	"quota-cluster/internal/engine"
	"quota-cluster/internal/incremental"
	"quota-cluster/internal/model"
	"quota-cluster/internal/sample"
)

type Server struct {
	seed   int64
	bundle *sample.Bundle
	verify *diagnostic.Report
	mux    *http.ServeMux
}

func New(seed int64) (*Server, error) {
	bundle, err := sample.Generate(seed)
	if err != nil {
		return nil, err
	}
	report, _, err := diagnostic.Verify(seed, "")
	if err != nil {
		return nil, err
	}
	s := &Server{seed: seed, bundle: bundle, verify: report, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.index)
	s.mux.HandleFunc("/api/verify", s.handleVerify)
	s.mux.HandleFunc("/api/schema", s.handleSchema)
	s.mux.HandleFunc("/api/truth", s.handleTruth)
	s.mux.HandleFunc("/api/tick", s.handleTick)
	s.mux.HandleFunc("/api/incremental", s.handleIncremental)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(dashboardHTML))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.verify)
}

func (s *Server) handleSchema(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Query().Get("version") {
	case "v1":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.bundle.V1)
	case "v2":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.bundle.V2)
	case "v3":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.bundle.V3)
	default:
		writeJSON(w, http.StatusBadRequest, bizerror.New(bizerror.CodeFieldRequired, "version", "version must be v1, v2, or v3"))
	}
}

func (s *Server) handleTruth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(s.bundle.TruthJSON)
}

func (s *Server) handleTick(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
	if err != nil || n < 0 || n > s.bundle.Project.LogicalTicks {
		writeJSON(w, http.StatusBadRequest, bizerror.New(bizerror.CodeValueOutOfRange, "n", "n must be between 0 and %d", s.bundle.Project.LogicalTicks))
		return
	}
	writeJSON(w, http.StatusOK, s.bundle.Truth.Ticks[n])
}

func (s *Server) handleIncremental(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	value, _ := strconv.ParseInt(q.Get("value"), 10, 64)
	atTick, _ := strconv.ParseInt(q.Get("tick"), 10, 64)
	seq, _ := strconv.Atoi(q.Get("sequence"))
	edit := model.Edit{Kind: model.EditKind(q.Get("kind")), TargetID: q.Get("target"), AtTick: atTick, Value: value, EventSequence: seq}
	svc, err := incremental.NewService(s.bundle.Project, engine.Options{Workers: 3, BatchSize: 2})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, err)
		return
	}
	result, err := svc.Apply(edit)
	if err != nil {
		status := http.StatusBadRequest
		if be, ok := bizerror.As(err); ok && be.Code == bizerror.CodeToleranceExceeded {
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"edit": result.Edit, "start_tick": result.StartTick, "dependencies": result.Dependencies,
		"provenance": result.Provenance, "recomputed_ticks": result.RecomputedTicks,
		"reused_ticks": result.ReusedTicks, "full_equal": result.FullEqual,
	})
}

var dashboardHTML = fmt.Sprint(`<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>配额账本诊断工作台</title><style>
:root{color-scheme:light dark;font-family:ui-sans-serif,system-ui,"Noto Sans CJK SC",sans-serif}
body{margin:0;background:#0f172a;color:#e2e8f0}header{padding:24px 32px;background:#111827;border-bottom:1px solid #334155}
main{padding:24px 32px;display:grid;gap:18px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(280px,1fr));gap:16px}
.card{background:#1e293b;border:1px solid #334155;border-radius:14px;padding:18px;box-shadow:0 10px 30px #0003}
.ok{color:#86efac}.bad{color:#fca5a5}.muted{color:#94a3b8}button,select,input{background:#0f172a;color:#e2e8f0;border:1px solid #475569;border-radius:8px;padding:8px 10px}
button{cursor:pointer}button:hover{border-color:#38bdf8}pre{max-height:420px;overflow:auto;background:#020617;padding:14px;border-radius:10px;font-size:12px}
table{border-collapse:collapse;width:100%;font-size:13px}td,th{border-bottom:1px solid #334155;padding:7px 8px;text-align:left}code{color:#7dd3fc}
</style></head><body><header><h1>多租户配额账本诊断工作台</h1><div class="muted">固定种子 · 标准库 HTTP · 三 schema 装载 · 确定性引擎 · SQLite 检查点</div></header><main>
<section class="card"><h2>验收总览</h2><div id="verify">加载中…</div></section>
<section class="grid">
<div class="card"><h2>Schema 样本</h2><p><button onclick="loadSchema('v1')">V1 扁平/字节/tick/‰</button> <button onclick="loadSchema('v2')">V2 机架树/KiB/ms/‱</button> <button onclick="loadSchema('v3')">V3 扁平/KiB/ms/‱</button></p><p><button onclick="loadTruth()">解析真值</button></p><pre id="artifact">选择版本查看 JSON。</pre></div>
<div class="card"><h2>逐时刻快照</h2><p><input id="tick" type="number" min="0" max="8" value="3"> <button onclick="loadTick()">读取 tick</button></p><pre id="tickView">tick 3 是争用裁决主场景。</pre></div>
</section>
<section class="card"><h2>增量失效演练</h2>
<p><select id="editKind"><option value="NODE_CAPACITY">节点容量</option><option value="TENANT_QUOTA">租户配额</option><option value="TENANT_GUARANTEE">保底份额</option><option value="WEIGHT_EVENT">权重事件</option></select>
<input id="target" placeholder="node-a / tenant-alpha" value="node-a"> <input id="tickAt" type="number" value="2" title="生效 tick"> <input id="seq" type="number" value="0" title="权重事件序号"> <input id="value" type="number" value="12884901888"> <button onclick="runEdit()">执行增量重算</button></p>
<div class="muted">字节值示例：12 GiB = 12884901888；3000 MiB = 3145728000。权重事件使用 value=180、sequence=4。</div><pre id="editView">尚未执行。</pre></section>
</main><script>
async function json(url,opts){const r=await fetch(url,opts);return r.json()}
function show(id,v){document.getElementById(id).textContent=typeof v==='string'?v:JSON.stringify(v,null,2)}
async function verify(){const r=await json('/api/verify');let h='<p><b>总体：</b>'+(r.ok?'<span class=ok>通过</span>':'<span class=bad>失败</span>')+' · 裁决 '+r.decision_count+' · 队列峰值 '+r.queue_peak+' · 检查点 tick '+r.checkpoint_tick+'</p><table><tr><th>检查</th><th>结果</th><th>说明</th></tr>';for(const c of r.checks){h+='<tr><td>'+c.name+'</td><td class='+(c.ok?'ok':'bad')+'>'+(c.ok?'PASS':'FAIL')+'</td><td>'+c.detail+'</td></tr>'}h+='</table>';document.getElementById('verify').innerHTML=h}
async function loadSchema(v){show('artifact',await json('/api/schema?version='+v))}
async function loadTruth(){show('artifact',await json('/api/truth'))}
async function loadTick(){const n=document.getElementById('tick').value;show('tickView',await json('/api/tick?n='+n))}
async function runEdit(){const p=new URLSearchParams({kind:editKind.value,target:target.value,tick:tickAt.value,sequence:seq.value,value:value.value});show('editView',await json('/api/incremental?'+p))}
verify();
</script></body></html>`)
