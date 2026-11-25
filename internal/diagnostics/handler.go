package diagnostics

import (
	"encoding/json"
	"net/http"
	"strconv"

	"replaybench/internal/eerr"
	"replaybench/internal/model"
)

func NewHandler(svc *Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(indexHTML))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Overview())
	})
	mux.HandleFunc("/api/full", func(w http.ResponseWriter, r *http.Request) {
		format := model.FormatWeightCast
		if v := r.URL.Query().Get("format"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeError(w, eerr.Schema("$.query.format", "format must be integer: %v", err))
				return
			}
			format = model.FormatID(n)
		}
		rep, err := svc.RunFull(r.Context(), format)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	mux.HandleFunc("/api/incremental", func(w http.ResponseWriter, r *http.Request) {
		rep, err := svc.RunIncremental(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	mux.HandleFunc("/api/checkpoint", func(w http.ResponseWriter, r *http.Request) {
		rep, err := svc.RunCheckpointDemo(r.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, rep)
	})
	mux.HandleFunc("/api/scheduler", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.SchedulerStats())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	if ee, ok := eerr.AsError(err); ok {
		status := http.StatusUnprocessableEntity
		switch ee.Code {
		case eerr.EDiff, eerr.EConflict:
			status = http.StatusConflict
		case eerr.ECanceled:
			status = http.StatusRequestTimeout
		case eerr.EOvf:
			status = http.StatusBadRequest
		}
		writeJSON(w, status, ee)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "E_INTERNAL", "error": err.Error()})
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8"/>
<title>ReplayBench 诊断台</title>
<style>
body{font-family:ui-sans-serif,system-ui,"Microsoft YaHei",sans-serif;margin:2rem;background:#0b1020;color:#e5e7eb}
code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,monospace}
.card{background:#111827;border:1px solid #243044;border-radius:12px;padding:1rem 1.2rem;margin:1rem 0;box-shadow:0 8px 30px #0004}
a{color:#7dd3fc;margin-right:1rem}
button{background:#2563eb;color:white;border:0;border-radius:8px;padding:.55rem .9rem;margin-right:.6rem;cursor:pointer}
button:hover{background:#1d4ed8}
pre{white-space:pre-wrap;word-break:break-word;background:#020617;padding:1rem;border-radius:8px;max-height:420px;overflow:auto}
.ok{color:#86efac}.bad{color:#fca5a5}
</style>
</head>
<body>
<h1>ReplayBench 回放规则结算诊断台</h1>
<p>确定性三格式样本、逐回合裁决、DAG 增量失效、SQLite 检查点与有界调度器。</p>
<div class="card">
<a href="/healthz">healthz</a>
<a href="/api/overview">overview JSON</a>
<a href="/api/full?format=1">format 1</a>
<a href="/api/full?format=2">format 2</a>
<a href="/api/full?format=3">format 3</a>
</div>
<div class="card">
<button onclick="load('/api/incremental','inc')">权重编辑增量重算</button>
<button onclick="load('/api/checkpoint','cp')">检查点恢复比对</button>
<button onclick="load('/api/scheduler','sched')">调度器统计</button>
<pre id="out">选择一个诊断动作；所有结果均来自本地确定性样本。</pre>
</div>
<script>
async function load(url, id){
  const out=document.getElementById('out');
  out.textContent='加载中 '+url;
  try{
    const r=await fetch(url); const data=await r.json();
    out.innerHTML='<span class="'+(r.ok?'ok':'bad')+'">HTTP '+r.status+'</span>\\n'+JSON.stringify(data,null,2);
  }catch(e){ out.innerHTML='<span class="bad">'+e+'</span>'; }
}
</script>
</body></html>`
