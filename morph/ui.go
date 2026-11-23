package morph

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

type UIOptions struct {
	Addr        string
	Scene       *Scene
	Truth       *Truth
	Config      Config
	WorkDir     string
	CompareFine bool
}

type uiRuntime struct {
	mu         sync.Mutex
	running    bool
	lastError  string
	summary    *RunSummary
	quality    *QualityReport
	qualityErr string
}

type uiServer struct {
	scene   *Scene
	truth   *Truth
	cfg     Config
	engine  *Engine
	ref     *Engine
	workDir string
	rt      uiRuntime
}

type uiPathInfo struct {
	ID            string    `json:"id"`
	Fill          bool      `json:"fill"`
	Stroke        bool      `json:"stroke"`
	StrokeWidth   float64   `json:"stroke_width,omitempty"`
	Opacity       float64   `json:"opacity"`
	BlendMode     string    `json:"blend_mode"`
	KeyframeTimes []float64 `json:"keyframe_times"`
}

type uiState struct {
	SceneID      string           `json:"scene_id"`
	Seed         int64            `json:"seed"`
	Paths        []uiPathInfo     `json:"paths"`
	SampleTimes  []float64        `json:"sample_times"`
	Quality      Quality          `json:"quality"`
	Progress     [2]int           `json:"progress"`
	Timeline     TimelineSnapshot `json:"timeline"`
	Running      bool             `json:"running"`
	Paused       bool             `json:"paused"`
	RunError     string           `json:"run_error,omitempty"`
	Summary      *RunSummary      `json:"summary,omitempty"`
	QualityCheck *QualityReport   `json:"quality_check,omitempty"`
	QualityError string           `json:"quality_error,omitempty"`
	Checkpoint   string           `json:"checkpoint"`
}

type uiPolyline struct {
	PathID string       `json:"path_id"`
	Points [][2]float64 `json:"points"`
}

type uiFrame struct {
	Index        int          `json:"index"`
	Time         float64      `json:"time"`
	Width        int          `json:"width"`
	Height       int          `json:"height"`
	Composite    string       `json:"composite"`
	Coverage     string       `json:"coverage,omitempty"`
	Diff         string       `json:"diff,omitempty"`
	SelectedPath string       `json:"selected_path"`
	Polylines    []uiPolyline `json:"polylines"`
	PixelHash    string       `json:"pixel_hash"`
	Mode         string       `json:"mode"`
	Grid         int          `json:"grid"`
}

func ServeDiagnostics(opts UIOptions) error {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:8080"
	}
	if opts.WorkDir == "" {
		opts.WorkDir = "artifacts/ui"
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return err
	}
	cfg := opts.Config
	if cfg.Checkpoint == "" {
		cfg.Checkpoint = filepath.Join(opts.WorkDir, "checkpoints")
	}
	eng, err := NewEngine(opts.Scene, cfg)
	if err != nil {
		return err
	}
	s := &uiServer{scene: opts.Scene, truth: opts.Truth, cfg: cfg, engine: eng, workDir: opts.WorkDir}
	if opts.CompareFine {
		refCfg := cfg
		refCfg.Adaptive.ForceFine = true
		refCfg.Adaptive.BaseGrid = cfg.Render.Grid
		refCfg.Adaptive.BaseMidTolerance = cfg.Eval.MidTolerance
		refCfg.Checkpoint = filepath.Join(opts.WorkDir, "fine-checkpoints")
		ref, err := NewEngine(opts.Scene, refCfg)
		if err != nil {
			return err
		}
		if ref.Store.CommittedCount() < len(opts.Scene.SampleTimes) {
			if _, err := ref.Run(context.Background(), RunOptions{}); err != nil {
				return err
			}
		}
		s.ref = ref
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/frame", s.handleFrame)
	mux.HandleFunc("/api/control", s.handleControl)
	go s.startRun()
	fmt.Printf("diagnostic UI: http://%s/\n", opts.Addr)
	return http.ListenAndServe(opts.Addr, mux)
}

func (s *uiServer) startRun() {
	s.rt.mu.Lock()
	if s.rt.running {
		s.rt.mu.Unlock()
		return
	}
	s.rt.running = true
	s.rt.lastError = ""
	s.rt.mu.Unlock()
	sum, err := s.engine.Run(context.Background(), RunOptions{})
	s.rt.mu.Lock()
	s.rt.running = false
	s.rt.summary = sum
	if err != nil {
		s.rt.lastError = err.Error()
	}
	s.rt.mu.Unlock()
	s.recomputeQuality()
}

func (s *uiServer) recomputeQuality() {
	if s.ref == nil || s.truth == nil {
		return
	}
	if s.engine.Store.CommittedCount() != len(s.scene.SampleTimes) {
		return
	}
	main := s.engine.Results()
	ref := s.ref.Results()
	if len(main) == 0 || len(ref) == 0 {
		return
	}
	q, err := CompareQuality(s.scene, s.truth, s.cfg.Consistency, ref, main)
	s.rt.mu.Lock()
	defer s.rt.mu.Unlock()
	s.rt.quality = q
	if err != nil {
		s.rt.qualityErr = err.Error()
	} else {
		s.rt.qualityErr = ""
	}
}

func (s *uiServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(uiHTML))
}

func (s *uiServer) handleState(w http.ResponseWriter, r *http.Request) {
	committed, total := s.engine.Timeline.Progress()
	s.rt.mu.Lock()
	running, lastErr, sum, q, qerr := s.rt.running, s.rt.lastError, s.rt.summary, s.rt.quality, s.rt.qualityErr
	s.rt.mu.Unlock()
	paths := make([]uiPathInfo, 0, len(s.scene.Paths))
	for _, p := range s.scene.Paths {
		info := uiPathInfo{ID: p.ID, Fill: p.Fill != nil, Stroke: p.Stroke != nil, Opacity: p.Opacity, BlendMode: p.BlendMode}
		if p.Stroke != nil {
			info.StrokeWidth = p.Stroke.Width
		}
		for _, k := range p.Keyframes {
			info.KeyframeTimes = append(info.KeyframeTimes, k.Time)
		}
		paths = append(paths, info)
	}
	state := uiState{
		SceneID: s.scene.ID, Seed: s.scene.Seed, Paths: paths, SampleTimes: s.scene.SampleTimes,
		Quality: s.engine.Quality, Progress: [2]int{committed, total}, Timeline: s.engine.Timeline.Snapshot(),
		Running: running, Paused: s.engine.Paused(), RunError: lastErr, Summary: sum,
		QualityCheck: q, QualityError: qerr, Checkpoint: s.cfg.Checkpoint,
	}
	writeJSON(w, state)
}

func (s *uiServer) handleFrame(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.URL.Query().Get("index"))
	if err != nil {
		http.Error(w, "index is required", http.StatusBadRequest)
		return
	}
	res := s.engine.Result(idx)
	if res == nil {
		http.Error(w, "frame is not committed", http.StatusConflict)
		return
	}
	pathID := r.URL.Query().Get("path")
	if pathID == "" && len(s.scene.Paths) > 0 {
		pathID = s.scene.Paths[0].ID
	}
	fr := uiFrame{
		Index: idx, Time: res.Time, Width: res.Width, Height: res.Height,
		Composite: encodeGrayPNG(res.Gray, res.Width, res.Height), SelectedPath: pathID,
		PixelHash: res.PixelHash, Mode: res.Mode, Grid: res.Quality.Grid,
	}
	for pi, flat := range res.Flattened {
		pts := make([][2]float64, len(flat.Points))
		for i, p := range flat.Points {
			px := res.Frame.WorldToPixel(p)
			pts[i] = [2]float64{px.X, px.Y}
		}
		fr.Polylines = append(fr.Polylines, uiPolyline{PathID: flat.PathID, Points: pts})
		if flat.PathID == pathID {
			p := findScenePath(s.scene, pathID)
			if p != nil {
				pr, err := RasterizePath(p, res.Flattened[pi], res.Frame, res.Quality.Grid)
				if err == nil {
					fr.Coverage = encodeCoveragePNG(pr, res.Width, res.Height)
				}
			}
		}
	}
	if s.ref != nil {
		if rr := s.ref.Result(idx); rr != nil && rr.Width == res.Width && rr.Height == res.Height {
			fr.Diff = encodeDiffPNG(res, rr)
		}
	}
	writeJSON(w, fr)
}

func (s *uiServer) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")
	switch action {
	case "pause":
		s.engine.Pause()
	case "resume":
		s.engine.Resume()
	case "cancel":
		s.engine.Cancel()
	case "run":
		s.rt.mu.Lock()
		running := s.rt.running
		s.rt.mu.Unlock()
		if !running {
			go s.startRun()
		}
	case "retry":
		s.rt.mu.Lock()
		running := s.rt.running
		s.rt.mu.Unlock()
		if running {
			http.Error(w, "cannot retry while running", http.StatusConflict)
			return
		}
		idx, _ := strconv.Atoi(r.URL.Query().Get("sample"))
		if _, err := s.engine.RetrySample(context.Background(), idx, nil); err != nil {
			writeJSONStatus(w, http.StatusConflict, map[string]any{"error": err.Error(), "code": CodeOf(err)})
			return
		}
	case "export":
		dir := filepath.Join(s.workDir, "frames")
		if err := ExportResults(dir, s.engine.Results()); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"exported": dir})
		return
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "action": action})
}

func writeJSON(w http.ResponseWriter, value any) { writeJSONStatus(w, http.StatusOK, value) }

func writeJSONStatus(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func encodeGrayPNG(gray []uint8, w, h int) string {
	img := image.NewGray(image.Rect(0, 0, w, h))
	copy(img.Pix, gray)
	return pngData(img)
}

func encodeCoveragePNG(pr *PathRaster, w, h int) string {
	img := image.NewGray(image.Rect(0, 0, w, h))
	if pr != nil && !pr.Rect.Empty() {
		for y := 0; y < pr.Rect.Height(); y++ {
			for x := 0; x < pr.Rect.Width(); x++ {
				v := pr.Coverage[y*pr.Rect.Width()+x]
				img.SetGray(pr.Rect.X0+x, pr.Rect.Y0+y, colorGray(v))
			}
		}
	}
	return pngData(img)
}

func colorGray(v float32) color.Gray {
	c := uint8(math.Round(math.Max(0, math.Min(1, float64(v))) * 255))
	return color.Gray{Y: c}
}

func encodeDiffPNG(a, b *SampleResult) string {
	img := image.NewRGBA(image.Rect(0, 0, a.Width, a.Height))
	for i := range a.Gray {
		d := math.Abs(float64(a.Gray[i])-float64(b.Gray[i])) / 255
		img.SetRGBA(i%a.Width, i/a.Width, heatColor(d))
	}
	return pngData(img)
}

func pngData(img image.Image) string {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return ""
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

const uiHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>PathMorph Diagnostics</title>
<style>
:root{--bg:#0f1115;--panel:#181b22;--line:#2b3040;--text:#d9e1f2;--muted:#8793ad;--accent:#57c7ff;--ok:#39d98a;--warn:#ffb020;--bad:#ff5c73}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
header{padding:12px 18px;border-bottom:1px solid var(--line);display:flex;gap:18px;align-items:center}
h1{font-size:16px;margin:0;color:var(--accent)}button{background:#232a38;color:var(--text);border:1px solid #3a445c;border-radius:6px;padding:6px 10px;cursor:pointer}
button:hover{border-color:var(--accent)}button.bad{border-color:var(--bad)}
main{display:grid;grid-template-columns:340px 1fr;height:calc(100vh - 58px)}.left{border-right:1px solid var(--line);overflow:auto;padding:12px}
.right{overflow:auto;padding:12px}.panel{background:var(--panel);border:1px solid var(--line);border-radius:8px;padding:10px;margin-bottom:10px}
.row{display:flex;gap:8px;flex-wrap:wrap;align-items:center}.muted{color:var(--muted)}.small{font-size:11px}.grow{flex:1}
.timeline{display:grid;grid-template-columns:repeat(auto-fill,minmax(44px,1fr));gap:5px}.tick{background:#202633;border:1px solid #30384b;border-radius:5px;padding:5px 3px;text-align:center;cursor:pointer}
.tick.active{border-color:var(--accent);background:#173247}.tick.committed{box-shadow:inset 0 -3px var(--ok)}
.path{display:flex;justify-content:space-between;padding:5px 7px;border-radius:5px;cursor:pointer;border:1px solid transparent}.path.active{border-color:var(--accent);background:#172536}
.grid2{display:grid;grid-template-columns:1fr 1fr;gap:10px}.grid3{display:grid;grid-template-columns:repeat(3,1fr);gap:10px}
img{width:100%;image-rendering:pixelated;background:#050608;border:1px solid var(--line);border-radius:6px}
canvas{width:100%;background:#050608;border:1px solid var(--line);border-radius:6px}
.stages{display:grid;grid-template-columns:1fr 1fr 1fr;gap:4px;margin-top:6px}.chip{padding:4px;text-align:center;border-radius:4px;background:#242b3a}
.pending{color:var(--muted)}.processing{color:var(--warn)}.committed{color:var(--ok)}.failed{color:var(--bad)}
code{color:var(--accent)}.reasons{color:var(--muted);font-size:11px;white-space:pre-wrap}.err{color:var(--bad)}
</style>
</head>
<body>
<header><h1>◆ PathMorph Diagnostics</h1><span id="scene" class="muted"></span><span class="grow"></span><span id="mode"></span></header>
<main>
<div class="left">
  <div class="panel">
    <div class="row"><button onclick="ctl('run')">运行/续跑</button><button onclick="ctl('pause')">暂停</button><button onclick="ctl('resume')">恢复</button><button class="bad" onclick="ctl('cancel')">取消</button><button onclick="ctl('export')">导出帧</button></div>
    <div class="row" style="margin-top:8px"><button onclick="retry()">重试失败阶段</button><button onclick="refreshFrame()">刷新帧</button></div>
    <div id="progress" class="muted" style="margin-top:8px"></div>
    <div id="runerr" class="err"></div>
  </div>
  <div class="panel"><div class="muted small">采样时刻</div><div id="timeline" class="timeline" style="margin-top:7px"></div></div>
  <div class="panel"><div class="muted small">路径列表</div><div id="paths" style="margin-top:6px"></div></div>
  <div class="panel"><div class="muted small">当前取舍模式与依据</div><div id="quality" class="reasons" style="margin-top:6px"></div></div>
  <div class="panel"><div class="muted small">当前时刻阶段状态</div><div id="stages"></div></div>
</div>
<div class="right">
  <div class="panel"><div class="row"><b>合成结果 + 折线叠加</b><span id="frameMeta" class="muted small"></span></div><canvas id="overlay" width="900" height="600"></canvas></div>
  <div class="grid3">
    <div class="panel"><div class="muted small">覆盖位图（当前路径）</div><img id="coverage" alt="coverage"></div>
    <div class="panel"><div class="muted small">差异热力图 vs 精细模式</div><img id="diff" alt="diff"></div>
    <div class="panel"><div class="muted small">灰阶合成位图</div><img id="composite" alt="composite"></div>
  </div>
  <div class="panel"><div class="muted small">质量指标摘要</div><div id="qcheck" class="reasons" style="margin-top:6px"></div></div>
</div>
</main>
<script>
let state=null, sample=0, pathId="";
const $=id=>document.getElementById(id);
async function getState(){const r=await fetch('/api/state');state=await r.json();renderState();}
async function getFrame(){if(!state)return;const q=new URLSearchParams({index:sample});if(pathId)q.set('path',pathId);const r=await fetch('/api/frame?'+q);if(!r.ok){$('frameMeta').textContent='帧未提交';return;}const f=await r.json();renderFrame(f);}
function renderState(){
  $('scene').textContent=state.scene_id+' seed='+state.seed;
  $('mode').textContent=state.quality.mode+' · grid '+state.quality.grid+' · '+state.progress[0]+'/'+state.progress[1];
  $('progress').textContent='进度 '+state.progress[0]+' / '+state.progress[1]+(state.running?' · running':'')+(state.paused?' · paused':'');
  $('runerr').textContent=state.run_error||'';
  $('quality').textContent='mode='+state.quality.mode+'\\nmid_tolerance='+state.quality.mid_tolerance+'\\nframe_tolerance='+state.quality.frame_tolerance+'\\ngrid='+state.quality.grid+' chunk='+state.quality.chunk_size+'\\nest_bytes='+state.quality.estimated_bytes+' budget='+state.quality.memory_budget_bytes+'\\n'+state.quality.reasons.join('\\n');
  if(!pathId && state.paths.length)pathId=state.paths[0].id;
  $('timeline').innerHTML=state.sample_times.map((t,i)=>'<div class="tick '+(i===sample?'active ':'')+(state.timeline.samples[i]&&state.timeline.samples[i].committed?'committed':'')+'" onclick="pickSample('+i+')">'+i+'<div class="small muted">'+Number(t).toFixed(3)+'</div></div>').join('');
  $('paths').innerHTML=state.paths.map(p=>'<div class="path '+(p.id===pathId?'active':'')+'" onclick="pickPath(\\''+p.id+'\\')"><span>'+p.id+'</span><span class="small muted">'+(p.fill?'F':'')+(p.stroke?'S':'')+' op='+p.opacity+' · '+p.keyframe_times.length+'K</span></div>').join('');
  const ss=state.timeline.samples[sample];$('stages').innerHTML=ss?ss.stages.map(x=>'<div class="chip '+x.state+'">'+x.path_id+'/'+x.stage+'<br>'+x.state+'</div>').join(''):'';
  if(state.quality_check){const q=state.quality_check;$('qcheck').textContent='pass='+q.pass+' midpoint='+q.midpoint_sample_covered+' segments fine='+q.fine_segments+' approx='+q.approx_segments+'\\n'+(q.first_failure?JSON.stringify(q.first_failure):'')+(state.quality_error?'\\n'+state.quality_error:'');}
}
function renderFrame(f){
  $('frameMeta').textContent='frame '+f.index+' t='+f.time+' '+f.width+'x'+f.height+' '+f.mode+' grid='+f.grid;
  $('coverage').src=f.coverage||'';$('diff').src=f.diff||'';$('composite').src=f.composite;
  const c=$('overlay'),ctx=c.getContext('2d');c.width=f.width;c.height=f.height;
  const im=new Image();im.onload=()=>{ctx.drawImage(im,0,0);ctx.lineWidth=1;for(const pl of f.polylines){if(pl.path_id!==f.selected_path)continue;ctx.strokeStyle='#57c7ff';ctx.beginPath();pl.points.forEach((p,i)=>i?ctx.lineTo(p[0],p[1]):ctx.moveTo(p[0],p[1]));ctx.stroke();}};im.src=f.composite;
}
function pickSample(i){sample=i;renderState();getFrame();}
function pickPath(id){pathId=id;renderState();getFrame();}
async function ctl(action){await fetch('/api/control?action='+action,{method:'POST'});await getState();if(action==='run'||action==='retry')getFrame();}
async function retry(){await fetch('/api/control?action=retry&sample='+sample,{method:'POST'});await getState();}
function refreshFrame(){getFrame();}
getState().then(getFrame);setInterval(()=>getState().then(getFrame),1500);
</script>
</body>
</html>`
