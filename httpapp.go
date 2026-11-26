package silhouette

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"
)

type RunRequest struct {
	Seed              int64 `json:"seed"`
	Version           int   `json:"version"`
	Tolerance         int64 `json:"tolerance"`
	Budget            int   `json:"budget"`
	Workers           int   `json:"workers"`
	VerticesPerWorker int   `json:"vertices_per_worker"`
}

type EditRequest struct {
	RunRequest
	SegmentID string `json:"segment_id"`
	Control   int    `json:"control_index"`
	PointX    int64  `json:"point_x"`
	PointY    int64  `json:"point_y"`
}

type RunResponse struct {
	Generated  *GeneratedSample   `json:"generated"`
	Loaded     *Scene             `json:"loaded"`
	Run        *RunResult         `json:"run"`
	Comparison *ConsistencyReport `json:"comparison,omitempty"`
}

func NewHTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, pageHTML)
	})
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "algorithm": AlgorithmVersion, "time": time.Now().UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("/api/scene", handleScene)
	mux.HandleFunc("/api/run", handleRun)
	mux.HandleFunc("/api/compare", handleCompare)
	mux.HandleFunc("/api/invalidation", handleInvalidation)
	return mux
}

func handleScene(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, NewDomainError(CodeSchema, "$.method", "GET required"))
		return
	}
	seed := int64(DefaultSeed)
	version := VersionCurrent
	if v := r.URL.Query().Get("seed"); v != "" {
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, NewDomainError(CodeSchema, "$.seed", "seed must be an integer"))
			return
		}
		seed = parsed
	}
	if v := r.URL.Query().Get("version"); v != "" {
		parsed, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, NewDomainError(CodeSchema, "$.version", "version must be 1, 2 or 3"))
			return
		}
		version = parsed
	}
	generated, err := Generate(seed, version)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, generated)
}

func decodeRunRequest(r *http.Request) (RunRequest, error) {
	var req RunRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return req, err
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, NewDomainError(CodeSchema, "$.body", "invalid JSON request")
	}
	if req.Seed == 0 {
		req.Seed = DefaultSeed
	}
	if req.Version == 0 {
		req.Version = VersionCurrent
	}
	if req.Workers <= 0 {
		req.Workers = DefaultWorkers
	}
	if req.VerticesPerWorker <= 0 {
		req.VerticesPerWorker = DefaultVerticesPerWorker
	}
	return req, nil
}

func loadGenerated(req RunRequest) (*GeneratedSample, *Scene, error) {
	generated, err := Generate(req.Seed, req.Version)
	if err != nil {
		return nil, nil, err
	}
	scene, err := Load(generated.Schema)
	if err != nil {
		return nil, nil, err
	}
	if req.Tolerance > 0 {
		scene.Tolerance.Value = req.Tolerance
		scene.Tolerance.ParamVersion = "http-tolerance-" + strconv.FormatInt(req.Tolerance, 10)
	}
	if req.Budget > 0 {
		scene.Budget.MaxVertices = req.Budget
		scene.Budget.ParamVersion = "http-budget-" + strconv.Itoa(req.Budget)
	}
	if err := scene.Validate(); err != nil {
		return nil, nil, err
	}
	return generated, scene, nil
}

func runConfig(req RunRequest) RunConfig {
	return RunConfig{
		Workers: req.Workers, VerticesPerWorker: req.VerticesPerWorker,
		Sampling: SamplingOptions{Tolerance: req.Tolerance, Budget: req.Budget},
	}
}

func handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, NewDomainError(CodeSchema, "$.method", "POST required"))
		return
	}
	req, err := decodeRunRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	generated, scene, err := loadGenerated(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	store, err := OpenCheckpointStore(":memory:")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer store.Close()
	run, err := RunRealtime(r.Context(), scene, store, runConfig(req))
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	writeJSON(w, http.StatusOK, RunResponse{Generated: generated, Loaded: scene, Run: run})
}

func handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, NewDomainError(CodeSchema, "$.method", "POST required"))
		return
	}
	req, err := decodeRunRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	_, scene, err := loadGenerated(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	cfg := runConfig(req)
	realStore, err := OpenCheckpointStore(":memory:")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer realStore.Close()
	realtime, err := RunRealtime(ctx, scene, realStore, cfg)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	resumeStore, err := OpenCheckpointStore(":memory:")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer resumeStore.Close()
	cfg.StopAfterCommits = 2
	if _, err := RunRealtime(ctx, scene, resumeStore, cfg); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	cfg.StopAfterCommits = 0
	resumed, err := RunResume(ctx, scene, resumeStore, cfg)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	fullStore, err := OpenCheckpointStore(":memory:")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer fullStore.Close()
	full, err := RunFullRecompute(ctx, scene, fullStore, cfg)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	report := CompareRuns(realtime, resumed)
	fullReport := CompareRuns(realtime, full)
	combined := *report
	if fullReport.Consistent == false {
		combined = *fullReport
	}
	combined.Details = append(combined.Details, "resume_consistent="+strconv.FormatBool(report.Consistent), "full_recompute_consistent="+strconv.FormatBool(fullReport.Consistent))
	writeJSON(w, http.StatusOK, map[string]any{
		"realtime":       realtime,
		"resume":         resumed,
		"full_recompute": full,
		"comparison":     combined,
	})
}

func handleInvalidation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, NewDomainError(CodeSchema, "$.method", "POST required"))
		return
	}
	var req EditRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, NewDomainError(CodeSchema, "$.body", "invalid JSON request"))
		return
	}
	runReq := req.RunRequest
	if runReq.Seed == 0 {
		runReq.Seed = DefaultSeed
	}
	if runReq.Version == 0 {
		runReq.Version = VersionCurrent
	}
	if runReq.Workers <= 0 {
		runReq.Workers = DefaultWorkers
	}
	if runReq.VerticesPerWorker <= 0 {
		runReq.VerticesPerWorker = DefaultVerticesPerWorker
	}
	_, scene, err := loadGenerated(runReq)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	store, err := OpenCheckpointStore(":memory:")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	defer store.Close()
	cfg := runConfig(runReq)
	base, err := RunRealtime(r.Context(), scene, store, cfg)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	segmentID := req.SegmentID
	if segmentID == "" {
		segmentID = "seg-0"
	}
	if req.Control < 0 || req.Control > 3 {
		writeError(w, http.StatusBadRequest, NewDomainError(CodeSchema, "$.control_index", "control index must be 0..3"))
		return
	}
	seg, ok := scene.Segment(segmentID)
	if !ok {
		writeError(w, http.StatusBadRequest, NewDomainError(CodeSchema, "$.segment_id", "unknown segment"))
		return
	}
	point := seg.Controls[req.Control]
	if req.PointX != 0 {
		point.X = req.PointX
	}
	if req.PointY != 0 {
		point.Y = req.PointY
	}
	edited, err := RunEdit(r.Context(), scene, base.Assembly, Edit{
		Kind: EditSegmentControl, SegmentID: segmentID, Control: req.Control, Point: point,
	}, store, cfg)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	commits, _ := store.ListCommits(r.Context(), scene.ID)
	writeJSON(w, http.StatusOK, map[string]any{"base": base, "edit": edited, "sqlite_commits": commits})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if err == nil {
		err = NewDomainError(CodeInternal, "$", "unknown error")
	}
	writeJSON(w, status, map[string]any{"error": err.Error(), "details": err})
}

const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Silhouette Sampling Kernel</title>
<style>
:root{color-scheme:dark;--bg:#10151c;--panel:#17212c;--line:#314154;--ink:#e8eef6;--muted:#9eb0c4;--accent:#5cc8ff}
*{box-sizing:border-box}body{margin:0;background:linear-gradient(160deg,#0c1117,#131e29);font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace;color:var(--ink)}
header{padding:28px 32px 12px}h1{font-size:22px;margin:0 0 6px}p{color:var(--muted);margin:4px 0}
main{display:grid;grid-template-columns:360px 1fr;gap:16px;padding:16px 32px 40px}section{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:16px}
label{display:block;color:var(--muted);margin:10px 0 4px}input,select{width:100%;background:#0c141c;color:var(--ink);border:1px solid var(--line);border-radius:7px;padding:8px}
button{margin:8px 6px 0 0;background:#173b52;color:var(--ink);border:1px solid #2e6b8c;border-radius:8px;padding:9px 12px;cursor:pointer}button:hover{background:#1e506f}
.row{display:grid;grid-template-columns:1fr 1fr;gap:8px}.ok{color:#6ee7a8}.bad{color:#ff8d8d}pre{white-space:pre-wrap;word-break:break-all;max-height:70vh;overflow:auto;background:#0a1016;border:1px solid #25364a;border-radius:8px;padding:12px}
.stats span{display:inline-block;margin:2px 12px 2px 0;color:var(--muted)}.stats b{color:var(--ink)}
</style>
</head>
<body>
<header><h1>曲面轮廓采样内核 / 本地诊断台</h1><p>固定种子解析样本 · 三 schema 装载 · 确定性采样 · 预算降级 · 分块 SQLite 检查点</p></header>
<main>
<section>
<label>种子 seed</label><input id="seed" value="20261127">
<label>schema 版本</label><select id="version"><option value="3">v3 显式曲线段 / micro-px</option><option value="2">v2 相对基点 / milli-px</option><option value="1">v1 绝对逻辑坐标</option></select>
<div class="row"><div><label>容差（内部 µ）</label><input id="tol" value="50000"></div><div><label>预算顶点</label><input id="budget" value="90"></div></div>
<div class="row"><div><label>工作线程</label><input id="workers" value="3"></div><div><label>每线程顶点上限</label><input id="buffer" value="8192"></div></div>
<div class="row"><div><label>编辑段</label><input id="segment" value="seg-0"></div><div><label>控制点序号</label><input id="control" value="1"></div></div>
<div class="row"><div><label>点 X（内部 µ）</label><input id="px" value="142000000"></div><div><label>点 Y（内部 µ）</label><input id="py" value="545000000"></div></div>
<button onclick="runSampling()">采样与分块</button><button onclick="comparePaths()">三路径比对</button><button onclick="invalidate()">编辑后失效</button>
<p id="status"></p>
</section>
<section>
<div id="stats" class="stats"></div>
<pre id="out">等待操作…</pre>
</section>
</main>
<script>
const $=id=>document.getElementById(id);
function req(extra={}){return {seed:+$('seed').value,version:+$('version').value,tolerance:+$('tol').value,budget:+$('budget').value,workers:+$('workers').value,vertices_per_worker:+$('buffer').value,...extra}}
async function post(path,body){$('status').textContent='运行中…';$('status').className='';const r=await fetch(path,{method:'POST',headers:{'content-type':'application/json'},body:JSON.stringify(body)});const j=await r.json();if(!r.ok){$('status').textContent='失败';$('status').className='bad';$('out').textContent=JSON.stringify(j,null,2);return null}$('status').textContent='完成';$('status').className='ok';return j}
function showRun(j){const run=j.run||j;const a=run.assembly;const b=a.budget;const events=[...(run.sampling?.worker_events||[]),...(a.worker_events||[])];const waits=events.filter(e=>e.wait_nanos>0);$('stats').innerHTML='<span>schema <b>'+(j.loaded?.source_version||'-')+'</b></span><span>顶点 <b>'+b.final_vertices+'</b></span><span>预算 <b>'+b.requested_budget+'</b></span><span>降级 <b>'+(b.degradations?.length||0)+'</b></span><span>分块 <b>'+a.tiles.length+'</b></span><span>等待事件 <b>'+waits.length+'</b></span>';$('out').textContent=JSON.stringify({truth:j.generated?.truth,traces:j.loaded?.traces,budget:b,tiles:a.tiles,commit_order:run.commit_order,worker_events:events,comparison:j.comparison},null,2)}
async function runSampling(){const j=await post('/api/run',req());if(j)showRun(j)}
async function comparePaths(){const j=await post('/api/compare',req());if(j){$('stats').innerHTML='<span>一致 <b class="'+(j.comparison.consistent?'ok':'bad')+'">'+j.comparison.consistent+'</b></span><span>分块 <b>'+j.comparison.compared_tiles+'</b></span><span>顶点 <b>'+j.comparison.compared_vertices+'</b></span><span>首差 <b>'+(j.comparison.first_mismatch_tile||'无')+' / '+(j.comparison.first_mismatch_vertex||0)+'</b></span>';$('out').textContent=JSON.stringify(j.comparison,null,2)}}
async function invalidate(){const j=await post('/api/invalidation',req({segment_id:$('segment').value,control_index:+$('control').value,point_x:+$('px').value,point_y:+$('py').value}));if(j){const inv=j.edit.invalidation;$('stats').innerHTML='<span>失效 <b>'+(inv.invalidated_tiles.join(', ')||'无')+'</b></span><span>复用 <b>'+(inv.reused_tiles.join(', ')||'无')+'</b></span><span>新提交 <b>'+j.edit.commits.length+'</b></span>';$('out').textContent=JSON.stringify({invalidation:inv,reused:j.edit.reused_tiles,commits:j.edit.commits},null,2)}}
</script>
</body></html>`
