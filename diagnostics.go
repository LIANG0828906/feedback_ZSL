package ocrflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"time"
)

type CacheEntryView struct {
	Key              string `json:"key"`
	ChunkID          string `json:"chunk_id"`
	ShotID           string `json:"shot_id"`
	LineNo           int    `json:"line_no"`
	Stage            string `json:"stage"`
	RegionHash       string `json:"region_hash"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	UpstreamKey      string `json:"upstream_key"`
	HitCount         int64  `json:"hit_count"`
}

type DiagnosticSnapshot struct {
	GeneratedAt      string             `json:"generated_at"`
	Config           Config             `json:"config"`
	Recipe           *Recipe            `json:"recipe"`
	Report           *RunReport         `json:"report"`
	Graph            *DependencyGraph   `json:"graph"`
	Invalidations    []InvalidationItem `json:"invalidations"`
	Recovery         *RecoveryReport    `json:"recovery"`
	Budget           BudgetSnapshot     `json:"budget"`
	CacheEntries     []CacheEntryView   `json:"cache_entries"`
	Checkpoints      []CheckpointChunk  `json:"checkpoints"`
	ErrorCodes       []ErrorCodeInfo    `json:"error_codes"`
	DependencyLayers []string           `json:"dependency_layers"`
}

type ErrorCodeInfo struct {
	Code    Code   `json:"code"`
	Meaning string `json:"meaning"`
}

func DiagnosticErrorCodes() []ErrorCodeInfo {
	return []ErrorCodeInfo{
		{CodeSchema, "配方 schema 缺字段、坐标退化、文本为空或阈值刻度越界"},
		{CodeOverflow, "整型换算溢出，返回字段与两个操作数"},
		{CodeParam, "重叠度/笔画一致性等运行参数不在允许集合"},
		{CodeDecode, "截图截断或 PNG 解码失败"},
		{CodeDiff, "增量与全量结果首次不一致的截图、行号和字段"},
	}
}

func (s *Service) Snapshot() DiagnosticSnapshot {
	s.mu.RLock()
	rep := s.last
	graph := s.graph
	inv := append([]InvalidationItem(nil), s.invalidations...)
	rec := s.recovery
	recipe := s.recipe
	s.mu.RUnlock()

	cacheViews := []CacheEntryView{}
	if entries, err := s.store.ListCache(); err == nil {
		for _, e := range entries {
			cacheViews = append(cacheViews, CacheEntryView{
				Key: e.Key, ChunkID: e.ChunkID, ShotID: e.ShotID, LineNo: e.LineNo, Stage: e.Stage,
				RegionHash: e.RegionHash, ParamVersion: e.ParamVersion, AlgorithmVersion: e.AlgorithmVersion,
				UpstreamKey: e.UpstreamKey, HitCount: e.HitCount,
			})
		}
	}
	checkpoints, _ := s.store.ListCheckpoints()
	return DiagnosticSnapshot{
		GeneratedAt: time.Now().Format(time.RFC3339), Config: s.cfg, Recipe: recipe, Report: rep, Graph: graph,
		Invalidations: inv, Recovery: rec, Budget: s.sched.Snapshot(), CacheEntries: cacheViews,
		Checkpoints: checkpoints, ErrorCodes: DiagnosticErrorCodes(),
		DependencyLayers: []string{"截图内容哈希", "灰度/归一化", "检测阈值", "文本行候选", "字符切分", "识别结果"},
	}
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"status": "ok", "codes": DiagnosticErrorCodes()})
	})
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.Snapshot())
	})
	mux.HandleFunc("GET /shots/{id}/image.png", s.handleShotImage)
	mux.HandleFunc("GET /action/invalidate-demo", s.handleInvalidateDemo)
	mux.HandleFunc("GET /", s.handleDashboard)
	return mux
}

func (s *Service) ListenAndServe(addr string) error {
	if addr == "" {
		addr = "127.0.0.1:18080"
	}
	return http.ListenAndServe(addr, s.Handler())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func (s *Service) handleInvalidateDemo(w http.ResponseWriter, r *http.Request) {
	shotID := r.URL.Query().Get("shot")
	lineNo := 1
	if shotID == "" {
		shotID = "shot-0002"
	}
	shot := s.findShot(shotID)
	if shot != nil {
		prep, err := PreprocessImage(shot.RGBA, s.recipe.Preprocess)
		if err == nil {
			if det, err := DetectLines(shot, s.recipe, prep); err == nil {
				_, _ = ToggleBackgroundBit(shot, det, lineNo)
			}
		}
	}
	_, _ = s.InvalidateRegion(shotID, lineNo, "diagnostic_demo_region_replace")
	_, _ = s.Run("incremental-diagnostic")
	http.Redirect(w, r, "/#invalidation", http.StatusSeeOther)
}

func (s *Service) findShot(id string) *Screenshot {
	for _, shot := range s.shots {
		if shot.ShotID == id {
			return shot
		}
	}
	return nil
}

func (s *Service) handleShotImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	shot := s.findShot(id)
	if shot == nil {
		http.NotFound(w, r)
		return
	}
	pngBytes, err := s.renderAnnotatedPNG(shot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(pngBytes)
}

func (s *Service) renderAnnotatedPNG(shot *Screenshot) ([]byte, error) {
	canvas := image.NewRGBA(image.Rect(0, 0, shot.Width, shot.Height))
	copy(canvas.Pix, shot.RGBA.Pix)
	var report *RunReport
	s.mu.RLock()
	report = s.last
	s.mu.RUnlock()
	if report != nil {
		for _, sr := range report.Shots {
			if sr.ShotID != shot.ShotID {
				continue
			}
			for _, line := range sr.Lines {
				c := color.RGBA{R: 46, G: 230, B: 140, A: 255}
				if line.Status == "failed" {
					c = color.RGBA{R: 255, G: 80, B: 80, A: 255}
				}
				drawBoxOutline(canvas, line.Box, c, 2)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, canvas); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawBoxOutline(img *image.RGBA, r Rect, c color.Color, thick int) {
	for t := 0; t < thick; t++ {
		for x := r.MinX - t; x < r.MaxX+t; x++ {
			img.Set(x, r.MinY-t, c)
			img.Set(x, r.MaxY-1+t, c)
		}
		for y := r.MinY - t; y < r.MaxY+t; y++ {
			img.Set(r.MinX-t, y, c)
			img.Set(r.MaxX-1+t, y, c)
		}
	}
}

const dashboardHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>OCRFlow 诊断台</title>
<style>
:root{--bg:#0b1220;--card:#121c2e;--line:#26364f;--text:#e7eefc;--muted:#93a4c0;--ok:#2ee68c;--warn:#ffcc66;--bad:#ff6b6b;--blue:#6cb6ff;--purple:#c297ff}
*{box-sizing:border-box} body{margin:0;background:linear-gradient(135deg,#08101f,#101b31);color:var(--text);font:14px/1.55 ui-sans-serif,system-ui,"PingFang SC","Microsoft YaHei",sans-serif}
header{padding:28px 34px;border-bottom:1px solid var(--line);background:rgba(8,16,31,.8);position:sticky;top:0;backdrop-filter:blur(10px)}
h1{margin:0 0 6px;font-size:26px} h2{margin:0 0 16px;font-size:19px} h3{margin:14px 0 8px;font-size:15px}
main{padding:24px 34px 60px;max-width:1440px;margin:0 auto}.grid{display:grid;gap:16px}.cards{grid-template-columns:repeat(auto-fit,minmax(190px,1fr))}.two{grid-template-columns:1.15fr .85fr}
.card{background:rgba(18,28,46,.92);border:1px solid var(--line);border-radius:14px;padding:18px;box-shadow:0 10px 30px rgba(0,0,0,.18)}
.metric{font-size:28px;font-weight:750;margin-top:4px}.muted{color:var(--muted)}.ok{color:var(--ok)}.warn{color:var(--warn)}.bad{color:var(--bad)}.blue{color:var(--blue)}.purple{color:var(--purple)}
table{width:100%;border-collapse:collapse;font-size:13px} th,td{border-bottom:1px solid var(--line);padding:8px 9px;text-align:left;vertical-align:top} th{color:var(--muted);font-weight:600}
code,.mono{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:12px;word-break:break-all}.pill{display:inline-block;border:1px solid var(--line);border-radius:999px;padding:2px 9px;margin:2px;background:#0d1728}.pill.ok{border-color:rgba(46,230,140,.45)}.pill.bad{border-color:rgba(255,107,107,.5)}
.shot{margin-bottom:22px}.shot img{width:100%;max-width:720px;border:1px solid var(--line);border-radius:10px;background:#000}.bar{height:10px;background:#0a1322;border:1px solid var(--line);border-radius:999px;overflow:hidden}.bar>i{display:block;height:100%;background:linear-gradient(90deg,var(--blue),var(--ok))}.flow{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:10px 0}.node{border:1px solid var(--line);background:#0d1728;border-radius:10px;padding:9px 11px}.node.hot{border-color:var(--warn);box-shadow:0 0 0 1px rgba(255,204,102,.25)}.arrow{color:var(--muted)}
.errcode{color:var(--warn);font-weight:700}.scroll{max-height:360px;overflow:auto}.right{text-align:right}.button{display:inline-block;background:#1d6ff2;color:white;text-decoration:none;border-radius:10px;padding:9px 14px;font-weight:650}.button.secondary{background:#26364f}
</style>
</head>
<body>
<header><h1>OCRFlow 长线运营 OCR 增量域</h1><div class="muted">确定性样本 · 三版本配方 · 精确失效 · SQLite 检查点恢复 · 受限并行预算</div></header>
<main>
<section class="grid cards">
 <div class="card"><div class="muted">文本行序列</div><div class="metric">{{if .Report}}{{len .Report.LineSequence}}{{else}}0{{end}}</div></div>
 <div class="card"><div class="muted">缓存命中 / 重算</div><div class="metric ok">{{if .Report}}{{.Report.CacheHits}}{{else}}0{{end}}<span class="muted" style="font-size:16px"> / </span><span class="warn" style="font-size:20px">{{if .Report}}{{.Report.CacheMisses}}{{else}}0{{end}}</span></div></div>
 <div class="card"><div class="muted">提交分块</div><div class="metric blue">{{if .Report}}{{len .Report.CommitSequence}}{{else}}0{{end}}</div></div>
 <div class="card"><div class="muted">预算等待次数</div><div class="metric purple">{{.Budget.WaitCount}}</div><div class="muted">累计等待 {{.Budget.TotalWaitMS}} ms</div></div>
</section>

<section class="grid two" style="margin-top:16px">
 <div class="card">
  <h2>检测框叠加与逐行结果</h2>
  {{range .Report.Shots}}
  <div class="shot">
    <h3>{{.ShotID}} <span class="pill {{if eq .Status "success"}}ok{{else}}bad{{end}}">{{.Status}}</span> <span class="muted">CER={{.Accuracy.CERBPS}}/10000</span></h3>
    <img src="/shots/{{.ShotID}}/image.png" alt="{{.ShotID}} detection overlay">
    <table><thead><tr><th>行</th><th>识别文本</th><th>真值</th><th>置信度</th><th>缓存</th><th>耗时</th></tr></thead><tbody>
    {{range .Lines}}<tr><td>{{.LineNo}}</td><td class="mono">{{if .Recognition}}{{.Recognition.Text}}{{else}}—{{end}}</td><td class="muted mono">{{TruthFor .ShotID .LineNo}}</td><td>{{if .Recognition}}{{confBar .Recognition.ConfidenceBPS}}{{.Recognition.ConfidenceBPS}}/10000{{end}}</td><td>{{if .CacheHit}}<span class="ok">hit</span>{{else}}<span class="warn">recompute</span>{{end}}</td><td>{{.DurationMS}}ms</td></tr>{{end}}
    </tbody></table>
  </div>
  {{end}}
 </div>
 <div>
  <div class="card" id="invalidation">
   <h2>失效传播示意</h2>
   <a class="button" href="/action/invalidate-demo">注入区域替换并重跑</a>
   <div class="flow">{{range $i, $layer := .DependencyLayers}}<span class="node {{if lt $i 4}}hot{{end}}">{{$layer}}</span>{{if lt $i 5}}<span class="arrow">→</span>{{end}}{{end}}</div>
   <p class="muted">缓存键 = SHA256(stage + 区域内容哈希 + 行标识 + 配方参数版本 + 上游键)。区域替换只改变对应行的区域哈希。</p>
   <table><thead><tr><th>截图/行</th><th>原因</th><th>字段</th><th>失效阶段</th></tr></thead><tbody>
   {{range .Invalidations}}<tr><td class="mono">{{.ShotID}}#{{.LineNo}}</td><td>{{.Reason}}</td><td class="mono">{{.Field}}</td><td>{{range .InvalidatedStages}}<span class="pill">{{.}}</span>{{end}}</td></tr>{{end}}
   </tbody></table>
  </div>
  <div class="card" style="margin-top:16px">
   <h2>预算占用明细</h2>
   {{$b := .Budget}}
   <p>最大并行度 <b>{{$b.Config.Workers}}</b>；活动 worker <b>{{$b.ActiveWorkers}}</b></p>
   <div class="bar"><i style="width:{{budgetPct $b.ActiveWorkers $b.Config.Workers}}%"></i></div>
   <p class="muted">每线程像素缓冲上限 {{$b.Config.PerWorkerPixelBytes}} B；活动像素 {{$b.ActivePixelBytes}} B</p>
   <p>队列任务 {{$b.QueuedTasks}}/{{$b.Config.MaxQueuedTasks}}；队列像素 {{$b.QueuedPixelBytes}}/{{$b.Config.MaxQueuedPixelBytes}}</p>
   <h3>排队记录</h3><div class="scroll"><table>{{range $b.Waits}}<tr><td class="mono">{{.TaskID}}</td><td>{{.Reason}}</td><td>{{.WaitedMS}}ms</td></tr>{{end}}</table></div>
  </div>
  <div class="card" style="margin-top:16px">
   <h2>异常码与诊断</h2>
   <table>{{range .ErrorCodes}}<tr><td class="errcode">{{.Code}}</td><td>{{.Meaning}}</td></tr>{{end}}</table>
   {{if .Recovery}}<h3>恢复边界</h3><pre class="mono">{{json .Recovery}}</pre>{{end}}
  </div>
 </div>
</section>

<section class="grid two" style="margin-top:16px">
 <div class="card"><h2>SQLite 检查点</h2><div class="scroll"><table><thead><tr><th>Chunk</th><th>顺序</th><th>行</th><th>参数版本</th></tr></thead><tbody>{{range .Checkpoints}}<tr><td class="mono">{{.ChunkID}}</td><td>{{.CommitOrder}}</td><td>{{len .Lines}}</td><td class="mono">{{short .ParamVersion}}</td></tr>{{end}}</tbody></table></div></div>
 <div class="card"><h2>缓存条目</h2><div class="scroll"><table><thead><tr><th>阶段</th><th>行</th><th>Key</th><th>命中</th></tr></thead><tbody>{{range .CacheEntries}}<tr><td><span class="pill">{{.Stage}}</span></td><td>{{.ShotID}}#{{.LineNo}}</td><td class="mono">{{short .Key}}</td><td>{{.HitCount}}</td></tr>{{end}}</tbody></table></div></div>
</section>
</main></body></html>`

func (s *Service) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	snap := s.Snapshot()
	tmpl := template.Must(template.New("dash").Funcs(template.FuncMap{
		"json": func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) },
		"short": func(v string) string {
			if len(v) > 18 {
				return v[:18] + "…"
			}
			return v
		},
		"budgetPct": func(a, b int) int {
			if b == 0 {
				return 0
			}
			return a * 100 / b
		},
		"confBar": func(v int) template.HTML {
			return template.HTML(fmt.Sprintf(`<div class="bar" style="display:inline-block;width:90px;vertical-align:middle;margin-right:6px"><i style="width:%d%%"></i></div>`, v/100))
		},
		"TruthFor": func(shotID string, lineNo int) string {
			shot := s.findShot(shotID)
			if shot == nil {
				return ""
			}
			for _, l := range shot.Lines {
				if l.LineNo == lineNo {
					return l.Text
				}
			}
			return ""
		},
	}).Parse(dashboardHTML))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, snap); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
