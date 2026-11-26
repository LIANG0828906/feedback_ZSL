package maskqa

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
)

type WebApp struct {
	mu            sync.Mutex
	Dataset       *Dataset
	DBPath        string
	Params        Params
	LastReport    *RunReport
	Invalidations []InvalidationItem
	Message       string
}

type webState struct {
	DBPath        string             `json:"db_path"`
	Params        Params             `json:"params"`
	Message       string             `json:"message"`
	Samples       []webSampleState   `json:"samples"`
	CommitCount   int                `json:"commit_count"`
	Invalidations []InvalidationItem `json:"invalidations"`
	Scheduler     *SchedulerMetrics  `json:"scheduler,omitempty"`
	GlobalMax     *PixelTrace        `json:"global_max,omitempty"`
}

type webSampleState struct {
	ID                  string  `json:"id"`
	Width               int     `json:"width"`
	Height              int     `json:"height"`
	Blocks              int     `json:"blocks"`
	InitialMismatch     int     `json:"initial_mismatch"`
	FinalMismatch       int     `json:"final_mismatch"`
	InitialMismatchRate float64 `json:"initial_mismatch_rate"`
	FinalMismatchRate   float64 `json:"final_mismatch_rate"`
	Components          int     `json:"components"`
	TruthComponents     int     `json:"truth_components"`
	DominantLevel       string  `json:"dominant_level"`
}

func NewWebApp(dbPath string) *WebApp {
	d := GenerateDataset()
	return &WebApp{Dataset: d, DBPath: dbPath, Params: d.Params, Message: "deterministic dataset ready"}
}

func (a *WebApp) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.index)
	mux.HandleFunc("/api/state", a.handleState)
	mux.HandleFunc("/api/action", a.handleAction)
	mux.HandleFunc("/api/trace", a.handleTrace)
	mux.HandleFunc("/png", a.handlePNG)
	return mux
}

func (a *WebApp) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, a.Handler())
}

func (a *WebApp) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(workbenchHTML))
}

func (a *WebApp) state() webState {
	st := webState{DBPath: a.DBPath, Params: a.Params, Message: a.Message, Invalidations: a.Invalidations}
	if a.LastReport != nil {
		st.CommitCount = len(a.LastReport.CommitSequence)
		metrics := a.LastReport.SchedulerMetrics
		st.Scheduler = &metrics
	}
	for _, s := range a.Dataset.Samples {
		item := webSampleState{ID: s.ID, Width: s.Width, Height: s.Height, Blocks: len(s.Blocks), TruthComponents: len(s.Truth.Components), InitialMismatch: XORCount(s.Rough, s.Truth.Mask)}
		item.InitialMismatchRate = float64(item.InitialMismatch) / float64(s.Width*s.Height)
		if a.LastReport != nil {
			for _, sr := range a.LastReport.SampleReports {
				if sr.SampleID != s.ID || sr.ErrorReport == nil {
					continue
				}
				item.FinalMismatch = sr.ErrorReport.ReferenceMismatchPixels
				item.FinalMismatchRate = sr.ErrorReport.ReferenceMismatchRate
				item.Components = len(sr.Components)
				if sr.ErrorReport.GlobalMax != nil {
					item.DominantLevel = sr.ErrorReport.GlobalMax.DominantLevel
					if st.GlobalMax == nil || maxContribution(*sr.ErrorReport.GlobalMax) > maxContribution(*st.GlobalMax) {
						gm := *sr.ErrorReport.GlobalMax
						st.GlobalMax = &gm
					}
				}
			}
		}
		st.Samples = append(st.Samples, item)
	}
	return st
}

func maxContribution(t PixelTrace) int64 {
	var m int64
	for _, v := range t.Contributions {
		if v > m {
			m = v
		}
	}
	return m
}

func (a *WebApp) handleState(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	writeJSON(w, a.state())
}

type actionRequest struct {
	Action string `json:"action"`
}

func (a *WebApp) handleAction(w http.ResponseWriter, r *http.Request) {
	var req actionRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	ctx := r.Context()
	switch req.Action {
	case "reset":
		_ = os.Remove(a.DBPath)
		a.Params = a.Dataset.Params
		a.LastReport = nil
		a.Invalidations = nil
		a.Message = "SQLite checkpoint reset; deterministic params restored"
	case "run", "realtime":
		rep, err := a.runLocked(ctx, RunOptions{Mode: req.Action, IncludePixels: true})
		if err != nil {
			a.Message = err.Error()
			writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "state": a.state()})
			return
		}
		a.LastReport = rep
		a.Message = fmt.Sprintf("run complete: %d stable checkpoint refs", len(rep.CommitSequence))
	case "resume-demo":
		_ = os.Remove(a.DBPath)
		svc, err := NewService(a.Dataset, a.DBPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if _, err := svc.Run(ctx, RunOptions{Mode: "interrupted", StopAfterCommits: 37}); err != nil {
			_ = svc.Close()
			http.Error(w, err.Error(), 500)
			return
		}
		_ = svc.Close()
		svc, err = NewService(a.Dataset, a.DBPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		rep, err := svc.Run(ctx, RunOptions{Mode: "resume", IncludePixels: true})
		_ = svc.Close()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		a.LastReport = rep
		a.Message = "interrupted after 37 commits and resumed from SQLite"
	case "edit-iter", "edit-radius", "edit-operator":
		if err := a.ensureBaselineLocked(ctx); err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "state": a.state()})
			return
		}
		old := a.Params
		p := a.Params
		switch req.Action {
		case "edit-iter":
			p.MaxIterations += 2
		case "edit-radius":
			if p.RadiusMax == p.RadiusMin {
				p.RadiusMax = p.RadiusMin + 1
			} else {
				p.RadiusMax = p.RadiusMin
			}
		case "edit-operator":
			if p.Operator == "sobel3" {
				p.Operator = "prewitt3"
			} else {
				p.Operator = "sobel3"
			}
		}
		svc, err := NewService(a.Dataset, a.DBPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		svc.Params = old
		items, err := svc.ApplyEdit(ctx, p)
		if err != nil {
			_ = svc.Close()
			writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "state": a.state()})
			return
		}
		rep, err := svc.Run(ctx, RunOptions{Mode: "incremental", IncludePixels: true})
		_ = svc.Close()
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error(), "state": a.state()})
			return
		}
		a.Params = p
		a.Invalidations = items
		a.LastReport = rep
		a.Message = fmt.Sprintf("edit propagated to %d nodes", len(items))
	default:
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "unknown action"})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "state": a.state()})
}

func (a *WebApp) runLocked(ctx context.Context, opts RunOptions) (*RunReport, error) {
	svc, err := NewService(a.Dataset, a.DBPath)
	if err != nil {
		return nil, err
	}
	svc.Params = a.Params
	svc.Scheduler = NewBoundedScheduler(a.Params)
	rep, err := svc.Run(ctx, opts)
	closeErr := svc.Close()
	if err != nil {
		return rep, err
	}
	return rep, closeErr
}

func (a *WebApp) ensureBaselineLocked(ctx context.Context) error {
	if _, err := os.Stat(a.DBPath); err == nil {
		svc, err := NewService(a.Dataset, a.DBPath)
		if err == nil {
			commits, _ := svc.Store.ListCheckpoints(ctx, "")
			_ = svc.Close()
			if len(commits) > 0 {
				return nil
			}
		}
	}
	rep, err := a.runLocked(ctx, RunOptions{Mode: "baseline"})
	if err != nil {
		return err
	}
	a.LastReport = rep
	return nil
}

func (a *WebApp) handleTrace(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sampleID := r.URL.Query().Get("sample")
	x := atoiDefault(r.URL.Query().Get("x"), -1)
	y := atoiDefault(r.URL.Query().Get("y"), -1)
	if a.LastReport == nil {
		http.Error(w, "run pipeline first", http.StatusNotFound)
		return
	}
	for _, sr := range a.LastReport.SampleReports {
		if sr.SampleID != sampleID || sr.ErrorReport == nil {
			continue
		}
		for _, t := range sr.ErrorReport.Pixels {
			if t.X == x && t.Y == y {
				writeJSON(w, t)
				return
			}
		}
	}
	http.NotFound(w, r)
}

func (a *WebApp) handlePNG(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	sample := a.Dataset.Sample(r.URL.Query().Get("sample"))
	if sample == nil {
		http.NotFound(w, r)
		return
	}
	kind := r.URL.Query().Get("kind")
	w.Header().Set("Content-Type", "image/png")
	switch kind {
	case "image":
		_ = WriteRasterPNG(w, sample.Raster)
	case "rough":
		_ = WriteMaskPNG(w, sample.Rough, nil)
	case "truth":
		_ = WriteMaskPNG(w, sample.Truth.Mask, nil)
	case "diff":
		sr := a.sampleReportLocked(sample.ID)
		if sr == nil || sr.Final == nil {
			http.NotFound(w, r)
			return
		}
		_ = WriteDiffPNG(w, sr.Final, sample.Truth.Mask)
	default:
		sr := a.sampleReportLocked(sample.ID)
		if sr == nil || sr.Final == nil {
			_ = WriteMaskPNG(w, sample.Rough, nil)
			return
		}
		_ = WriteMaskPNG(w, sr.Final, nil)
	}
}

func (a *WebApp) sampleReportLocked(id string) *SampleRunReport {
	if a.LastReport == nil {
		return nil
	}
	for _, sr := range a.LastReport.SampleReports {
		if sr.SampleID == id {
			return sr
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func atoiDefault(s string, def int) int {
	n := 0
	if s == "" {
		return def
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
	}
	return n
}

const workbenchHTML = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>MaskQA 定点掩码质检工作台</title>
<style>
:root{color-scheme:dark;font-family:ui-sans-serif,system-ui,"Noto Sans CJK SC",sans-serif;background:#0b1220;color:#e5e7eb}
body{margin:0}.wrap{max-width:1280px;margin:0 auto;padding:24px}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(260px,1fr));gap:16px}.card{background:#111827;border:1px solid #243044;border-radius:14px;padding:16px;box-shadow:0 10px 30px #0003}
button{background:#2563eb;color:white;border:0;border-radius:10px;padding:9px 12px;margin:4px 6px 4px 0;cursor:pointer}button.secondary{background:#334155}button.warn{background:#b45309}
img{width:100%;image-rendering:pixelated;border-radius:10px;background:#0f172a}.row{display:grid;grid-template-columns:repeat(5,1fr);gap:8px}.muted{color:#94a3b8;font-size:13px}pre{max-height:360px;overflow:auto;background:#020617;padding:12px;border-radius:10px;font-size:12px}
table{width:100%;border-collapse:collapse;font-size:12px}td,th{border-bottom:1px solid #243044;padding:6px;text-align:left}.pill{display:inline-block;border-radius:999px;padding:2px 8px;background:#1e3a8a}.ok{color:#4ade80}.bad{color:#f87171}
</style></head><body><div class="wrap">
<h1>美术切图掩码质检工作台</h1><p class="muted">确定性种子样本 · 定点梯度 · 曲率半径 · 分段形态学 · 五级溯源 · SQLite 原子检查点</p>
<div class="card"><div>
<button onclick="act('run')">一次性全量精修</button>
<button onclick="act('realtime')">受限并行实时精修</button>
<button onclick="act('resume-demo')">模拟中断并恢复</button>
<button class="warn" onclick="act('edit-iter')">编辑迭代上限 +2</button>
<button class="warn" onclick="act('edit-radius')">切换半径带</button>
<button class="warn" onclick="act('edit-operator')">切换 Sobel/Prewitt</button>
<button class="secondary" onclick="act('reset')">重置检查点</button>
</div><p id="message" class="muted"></p></div>
<div id="samples" class="grid" style="margin-top:16px"></div>
<div class="grid" style="margin-top:16px"><div class="card"><h3>失效清单</h3><div style="max-height:360px;overflow:auto"><table id="inv"></table></div></div>
<div class="card"><h3>调度/最大残差</h3><pre id="meta"></pre><h3>像素溯源查询</h3><p class="muted">输入样本中像素坐标：</p><input id="sample" placeholder="sample id" style="width:220px"><input id="x" placeholder="x" style="width:60px"><input id="y" placeholder="y" style="width:60px"><button onclick="trace()">查询</button><pre id="trace"></pre></div></div>
</div><script>
let state;
async function refresh(){const r=await fetch('/api/state');state=await r.json();document.querySelector('#message').textContent=state.message+' | commits='+state.commit_count;
document.querySelector('#meta').textContent=JSON.stringify({params:state.params,scheduler:state.scheduler,global_max:state.global_max},null,2);
document.querySelector('#samples').innerHTML=state.samples.map(s=>'<div class=card><h3>'+s.id+'</h3><p class=muted>'+s.width+'×'+s.height+' / '+s.blocks+' blocks / truth comps '+s.truth_components+'</p><div class=row><div><img src="/png?sample='+s.id+'&kind=image"><p class=muted>原图</p></div><div><img src="/png?sample='+s.id+'&kind=rough"><p class=muted>粗糙掩码</p></div><div><img src="/png?sample='+s.id+'&kind=truth"><p class=muted>真值</p></div><div><img src="/png?sample='+s.id+'&kind=final"><p class=muted>精修</p></div><div><img src="/png?sample='+s.id+'&kind=diff"><p class=muted>绿正确/红外溢/橙内缩</p></div></div><p>初始偏差 <b class=bad>'+s.initial_mismatch+'</b> → 精修偏差 <b class=ok>'+s.final_mismatch+'</b>；连通域 '+s.components+'/'+s.truth_components+'；最大残差级 '+s.dominant_level+'</p></div>').join('');
document.querySelector('#inv').innerHTML='<tr><th>sample</th><th>block</th><th>stage</th><th>reason</th><th>upstream</th></tr>'+(state.invalidations||[]).map(i=>'<tr><td>'+i.sample_id+'</td><td>'+i.block.index_x+','+i.block.index_y+'</td><td>'+i.stage+'</td><td>'+i.reason+' '+(i.param_field||'')+'</td><td class=muted>'+(i.upstream||[]).join('<br>')+'</td></tr>').join('');}
async function act(a){await fetch('/api/action',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({action:a})});refresh();}
async function trace(){const q=new URLSearchParams({sample:document.querySelector('#sample').value,x:document.querySelector('#x').value,y:document.querySelector('#y').value});const r=await fetch('/api/trace?'+q);document.querySelector('#trace').textContent=JSON.stringify(await r.json(),null,2);}
refresh();setInterval(refresh,4000);
</script></body></html>`
