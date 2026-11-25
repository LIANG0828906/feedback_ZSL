// Package web serves the local deterministic verification UI using only the Go
// standard library.
package web

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/quant"
	"spectralforge/internal/render"
	"spectralforge/internal/store"
	"spectralforge/internal/verify"
)

type Snapshot struct {
	Version string              `json:"version"`
	Config  domain.RenderConfig `json:"config"`
}

type SnapshotStore struct {
	mu     sync.RWMutex
	latest Snapshot
}

func NewSnapshotStore(cfg domain.RenderConfig) *SnapshotStore {
	return &SnapshotStore{latest: Snapshot{Version: domain.ConfigVersion(cfg), Config: cfg}}
}

func (s *SnapshotStore) Update(cfg domain.RenderConfig) (Snapshot, error) {
	if err := domain.ValidateConfig(&cfg); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = Snapshot{Version: domain.ConfigVersion(cfg), Config: cfg}
	return s.latest, nil
}

func (s *SnapshotStore) Latest() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

type RunSummary struct {
	Mode               string               `json:"mode"`
	Completed          bool                 `json:"completed"`
	Version            string               `json:"version"`
	SamplingVersion    string               `json:"sampling_version"`
	TilesRendered      int                  `json:"tiles_rendered"`
	TilesReused        int                  `json:"tiles_reused"`
	TilesTotal         int                  `json:"tiles_total"`
	ElapsedSeconds     float64              `json:"elapsed_seconds"`
	WaitStats          store.WaitStats      `json:"wait_stats"`
	TileAttempts       map[int]int          `json:"tile_attempts"`
	Failures           []store.Failure      `json:"failures"`
	Invalidated        bool                 `json:"invalidated"`
	InvalidationReason string               `json:"invalidation_reason,omitempty"`
	QuantStats         quant.ErrorStats     `json:"quant_stats"`
	TruthPass          bool                 `json:"truth_pass"`
	TruthResults       []render.TruthResult `json:"truth_results,omitempty"`
	Message            string               `json:"message,omitempty"`
	ImageURL           string               `json:"image_url,omitempty"`
}

type Server struct {
	Bundle           *domain.SampleBundle
	Snapshots        *SnapshotStore
	CheckpointRoot   string
	OutputRoot       string
	Address          string
	renderMu         sync.Mutex
	stateMu          sync.RWMutex
	preview          *RunSummary
	exact            *RunSummary
	verification     *verify.Report
	lastPreview      *render.Frame
	lastPreviewCfg   domain.RenderConfig
	lastPreviewQuant quant.ErrorStats
}

func NewServer(bundle *domain.SampleBundle, checkpointRoot, outputRoot, address string) *Server {
	return &Server{
		Bundle:         bundle,
		Snapshots:      NewSnapshotStore(*bundle.Config),
		CheckpointRoot: checkpointRoot,
		OutputRoot:     outputRoot,
		Address:        address,
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/params", s.handleParams)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/exact", s.handleExact)
	mux.HandleFunc("/api/resume", s.handleResume)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/image.png", s.handleImage)
	return mux
}

func (s *Server) ListenAndServe() error {
	srv := &http.Server{
		Addr:              s.Address,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.ListenAndServe()
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "GET required"))
		return
	}
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": s.Snapshots.Latest(),
		"sample": map[string]any{
			"width":        s.Bundle.Scene.Width,
			"height":       s.Bundle.Scene.Height,
			"content":      s.Bundle.ContentHash,
			"truth_points": len(s.Bundle.Truth.Points),
			"seed":         s.Bundle.Scene.Seed,
		},
		"preview":      s.preview,
		"exact":        s.exact,
		"verification": s.verification,
	})
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "POST required"))
		return
	}
	var cfg domain.RenderConfig
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, errs.Decode("request", "invalid config JSON: %v", err))
		return
	}
	snap, err := s.Snapshots.Update(cfg)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "POST required"))
		return
	}
	snap := s.Snapshots.Latest()
	s.stateMu.RLock()
	lastFrame := s.lastPreview
	lastCfg := s.lastPreviewCfg
	lastQuant := s.lastPreviewQuant
	s.stateMu.RUnlock()
	if lastFrame != nil && render.DisplayOnlyChange(lastCfg, snap.Config) {
		s.renderMu.Lock()
		frame := render.RetoneFrame(lastFrame, snap.Config)
		err := render.WriteOutputs(filepath.Join(s.OutputRoot, "preview"), frame)
		if err == nil {
			err = render.DetectSeam(frame, snap.Config.SeamThreshold)
		}
		s.renderMu.Unlock()
		if err == nil {
			summary := &RunSummary{
				Mode:            "preview",
				Completed:       true,
				Version:         snap.Version,
				SamplingVersion: frame.SamplingVersion,
				TilesTotal:      len(frame.Blocks),
				TilesReused:     len(frame.Blocks),
				QuantStats:      lastQuant,
				TruthPass:       frame.TruthPass,
				TruthResults:    frame.TruthResults,
				Message:         "display-only retone: 0 spectral blocks recomputed",
				ImageURL:        "/api/image.png?kind=preview&v=" + snap.Version[:min(12, len(snap.Version))],
			}
			s.stateMu.Lock()
			s.preview = summary
			s.stateMu.Unlock()
			writeJSON(w, http.StatusOK, summary)
			return
		}
	}
	report, err := s.render(r.Context(), "preview", snap, false)
	summary := summarize(report, err, "preview")
	s.stateMu.Lock()
	s.preview = summary
	if report != nil && report.Frame != nil {
		s.lastPreview = report.Frame
		s.lastPreviewCfg = snap.Config
		s.lastPreviewQuant = report.QuantStats
	}
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleExact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "POST required"))
		return
	}
	snap := s.Snapshots.Latest()
	report, err := s.render(r.Context(), "exact", snap, false)
	summary := summarize(report, err, "exact")
	s.stateMu.Lock()
	s.exact = summary
	s.verification = nil
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "POST required"))
		return
	}
	cpDir := filepath.Join(s.CheckpointRoot, "exact")
	manifest, err := store.ReadManifest(cpDir)
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	if manifest.ContentHash != s.Bundle.ContentHash {
		writeError(w, http.StatusConflict, errs.Version(s.Bundle.ContentHash, manifest.ContentHash))
		return
	}
	snap := Snapshot{Version: manifest.ParamVersion, Config: manifest.Config}
	report, err := s.render(r.Context(), "resume", snap, true)
	summary := summarize(report, err, "exact")
	s.stateMu.Lock()
	s.exact = summary
	s.stateMu.Unlock()
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "POST required"))
		return
	}
	report, err := verify.Run(s.Bundle, filepath.Join(s.CheckpointRoot, "exact"), filepath.Join(s.OutputRoot, "exact"))
	if err != nil {
		writeError(w, statusFor(err), err)
		return
	}
	s.stateMu.Lock()
	s.verification = report
	s.stateMu.Unlock()
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodePara, "GET required"))
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "preview" && kind != "exact" {
		writeError(w, http.StatusBadRequest, errs.Para("kind", `["preview","exact"]`, "invalid image kind"))
		return
	}
	path := filepath.Join(s.OutputRoot, kind, "frame.png")
	if _, err := os.Stat(path); err != nil {
		writeError(w, http.StatusNotFound, errs.Decode("frame.png", "image is not available yet"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (s *Server) render(ctx context.Context, kind string, snap Snapshot, resume bool) (*render.Report, error) {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()
	cp := filepath.Join(s.CheckpointRoot, kind)
	out := filepath.Join(s.OutputRoot, kind)
	return render.Render(ctx, s.Bundle, render.Options{
		Config:          snap.Config,
		CheckpointDir:   cp,
		OutputDir:       out,
		Resume:          resume,
		Mode:            kind,
		SnapshotVersion: snap.Version,
	})
}

func summarize(report *render.Report, err error, kind string) *RunSummary {
	if report == nil {
		summary := &RunSummary{Mode: kind, Message: errorMessage(err)}
		return summary
	}
	out := &RunSummary{
		Mode:               report.Mode,
		Completed:          report.Completed,
		Version:            report.ParamVersion,
		SamplingVersion:    report.SamplingVersion,
		TilesRendered:      report.TilesRendered,
		TilesReused:        report.TilesReused,
		TilesTotal:         report.TilesTotal,
		ElapsedSeconds:     report.ElapsedSeconds,
		WaitStats:          report.WaitStats,
		TileAttempts:       report.TileAttempts,
		Failures:           report.Failures,
		Invalidated:        report.Invalidated,
		InvalidationReason: report.InvalidationReason,
		QuantStats:         report.QuantStats,
		Message:            errorMessage(err),
	}
	if report.Frame != nil {
		out.TruthPass = report.Frame.TruthPass
		out.TruthResults = report.Frame.TruthResults
		out.ImageURL = "/api/image.png?kind=" + kind + "&v=" + report.ParamVersion[:min(12, len(report.ParamVersion))]
	}
	return out
}

func errorMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func statusFor(err error) int {
	if e, ok := errs.As(err); ok {
		switch e.Code {
		case errs.CodeVersion:
			return http.StatusConflict
		case errs.CodeSchema, errs.CodePara, errs.CodeDecode:
			return http.StatusBadRequest
		case errs.CodePaused, errs.CodeCancel:
			return http.StatusAccepted
		}
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	if e, ok := errs.As(err); ok {
		writeJSON(w, status, e)
		return
	}
	writeJSON(w, status, errs.New(errs.CodeIO, "%v", err))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, map[string]any{
		"Address": s.Address,
		"Width":   s.Bundle.Scene.Width,
		"Height":  s.Bundle.Scene.Height,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

var pageTemplate = template.Must(template.New("page").Parse(pageHTML))

const pageHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>SpectralForge</title>
<style>
:root { color-scheme: dark; font-family: ui-sans-serif, system-ui, "Segoe UI", sans-serif; background:#0e1117; color:#e8edf7; }
body { margin:0; padding:24px; background:radial-gradient(circle at 15% -10%,#18344d 0,transparent 38%),#0e1117; }
header { display:flex; align-items:flex-end; justify-content:space-between; gap:20px; margin-bottom:20px; }
h1 { margin:0; font-size:28px; letter-spacing:-.04em; }
.sub { color:#8fa1b9; font-size:13px; margin-top:6px; }
.grid { display:grid; grid-template-columns:minmax(320px,410px) 1fr; gap:20px; }
.card { background:#151b25; border:1px solid #263246; border-radius:14px; padding:18px; box-shadow:0 18px 60px #0004; }
.card h2 { margin:0 0 14px; font-size:14px; text-transform:uppercase; letter-spacing:.12em; color:#9fb1ca; }
.fields { display:grid; grid-template-columns:1fr 1fr; gap:10px; }
label { display:flex; flex-direction:column; gap:5px; font-size:11px; color:#9eb0c7; }
input,select { background:#0d131c; color:#edf3ff; border:1px solid #2c3b51; border-radius:8px; padding:8px 9px; font:inherit; font-size:13px; }
input:focus,select:focus { outline:2px solid #2f81f7; border-color:transparent; }
.actions { display:grid; grid-template-columns:1fr 1fr; gap:9px; margin-top:15px; }
button { border:0; border-radius:9px; padding:10px 12px; font-weight:650; cursor:pointer; color:#eaf2ff; background:#22324a; }
button.primary { background:linear-gradient(135deg,#2f81f7,#56b6ff); color:#06101d; }
button.warn { background:#49312a; color:#ffc9b5; }
button:disabled { opacity:.45; cursor:wait; }
.statusline { display:flex; flex-wrap:wrap; gap:8px; margin-bottom:12px; }
.pill { font:12px ui-monospace,Consolas,monospace; background:#0d131c; border:1px solid #273449; border-radius:999px; padding:5px 9px; color:#a9bbd2; }
.pill.good { color:#7ee2a8; border-color:#24513a; }
.pill.bad { color:#ff9f8f; border-color:#5b3029; }
.previewframe { min-height:260px; display:grid; place-items:center; background:#0b0f15; border-radius:11px; overflow:hidden; border:1px solid #202b3b; }
.previewframe img { width:100%; image-rendering:auto; display:block; }
.empty { color:#63748a; font-size:13px; }
table { width:100%; border-collapse:collapse; font:12px ui-monospace,Consolas,monospace; }
th,td { text-align:left; padding:7px 6px; border-bottom:1px solid #202b3b; white-space:nowrap; }
th { color:#7f93ad; font-weight:600; }
#log { min-height:80px; max-height:180px; overflow:auto; white-space:pre-wrap; background:#0b0f15; border:1px solid #202b3b; border-radius:9px; padding:10px; font:12px ui-monospace,Consolas,monospace; color:#a9bbd2; }
@media (max-width:900px) { .grid { grid-template-columns:1fr; } body { padding:14px; } }
</style>
</head>
<body>
<header>
  <div>
    <h1>SpectralForge</h1>
    <div class="sub">Deterministic spectral material pipeline · {{.Width}}×{{.Height}} · local only</div>
  </div>
  <div class="statusline" id="headline"></div>
</header>
<div class="grid">
  <section class="card">
    <h2>Parameter Snapshot</h2>
    <div class="statusline"><span class="pill" id="version">version …</span><span class="pill" id="content">content …</span></div>
    <div class="fields">
      <label>Integration<select id="integration"><option>trapezoid</option><option>midpoint</option></select></label>
      <label>Wavelength steps<input id="wavelength_steps" type="number" min="2" max="4096"></label>
      <label>Exposure<input id="exposure" type="number" step="0.05"></label>
      <label>Tone curve<select id="tone_curve"><option>aces</option><option>reinhard</option><option>linear</option></select></label>
      <label>Gamma<input id="gamma" type="number" step="0.05"></label>
      <label>Indirect bounces<input id="indirect_bounces" type="number" min="0" max="16"></label>
      <label>Tile size<input id="tile_size" type="number" min="4"></label>
      <label>Workers<input id="max_workers" type="number" min="1" max="256"></label>
      <label>Queue budget<input id="queue_budget" type="number" min="1"></label>
      <label>Per-thread pixels<input id="per_thread_pixel_buffer" type="number" min="16"></label>
      <label>Albedo bits<input id="albedo_bits" type="number" min="2" max="24"></label>
      <label>Basis bits<input id="basis_bits" type="number" min="2" max="24"></label>
      <label>Seam threshold<input id="seam_threshold" type="number" step="0.005"></label>
      <label>Color tolerance<input id="color_tolerance" type="number" step="0.0001"></label>
    </div>
    <div class="actions">
      <button onclick="applyConfig()">Apply snapshot</button>
      <button class="primary" onclick="runRender('preview')">Latest preview</button>
      <button onclick="runRender('exact')">Exact (lock version)</button>
      <button onclick="runRender('resume')">Resume exact</button>
      <button class="warn" onclick="runVerify()">Independent verify</button>
      <button onclick="refresh()">Refresh status</button>
    </div>
  </section>
  <section class="card">
    <h2>Exact Frame</h2>
    <div class="statusline" id="exactPills"></div>
    <div class="previewframe" id="imageBox"><span class="empty">Run an exact render to inspect pixels.</span></div>
    <h2 style="margin-top:18px">Analytic Truth</h2>
    <div id="truth"><span class="empty">Truth comparison appears after rendering.</span></div>
  </section>
</div>
<section class="card" style="margin-top:20px">
  <h2>Diagnostics</h2>
  <div id="log">Ready.</div>
</section>
<script>
let state = null;
const ids = ["integration","wavelength_steps","exposure","tone_curve","gamma","indirect_bounces","tile_size","max_workers","queue_budget","per_thread_pixel_buffer","albedo_bits","basis_bits","seam_threshold","color_tolerance"];
const q = id => document.getElementById(id);
function log(msg){ q("log").textContent = msg; }
function pill(text, cls=""){ const s=document.createElement("span"); s.className="pill "+cls; s.textContent=text; return s; }
function fillConfig(cfg){
  for(const id of ids){ if(cfg[id] !== undefined) q(id).value = cfg[id]; }
  q("albedo_bits").value = cfg.quantization.albedo_bits;
  q("basis_bits").value = cfg.quantization.basis_bits;
}
function collectConfig(){
  const cfg = JSON.parse(JSON.stringify(state.snapshot.config));
  for(const id of ids){
    if(id==="albedo_bits"||id==="basis_bits") continue;
    const el=q(id);
    cfg[id] = el.tagName==="SELECT" ? el.value : (el.type==="number" ? Number(el.value) : el.value);
  }
  cfg.quantization.albedo_bits=Number(q("albedo_bits").value);
  cfg.quantization.basis_bits=Number(q("basis_bits").value);
  return cfg;
}
async function api(path, opts={}){
  const r = await fetch(path, {headers:{"Content-Type":"application/json"}, ...opts});
  const text = await r.text();
  let data; try { data=JSON.parse(text); } catch { data={message:text}; }
  if(!r.ok) throw new Error(JSON.stringify(data));
  return data;
}
async function refresh(){
  try {
    state = await api("/api/status");
    fillConfig(state.snapshot.config);
    q("version").textContent = "version " + state.snapshot.version.slice(0,12);
    q("content").textContent = "content " + state.sample.content.slice(0,12);
    q("headline").replaceChildren(
      pill(state.sample.width+"×"+state.sample.height),
      pill(state.sample.truth_points+" truth points"),
      pill("seed "+state.sample.seed)
    );
    renderExact(state.exact);
    if(state.verification) renderVerify(state.verification);
  } catch(e){ log("Status error: "+e.message); }
}
function renderExact(run){
  const box=q("exactPills"), img=q("imageBox"), truth=q("truth");
  if(!run){ box.replaceChildren(); img.innerHTML='<span class="empty">Run an exact render to inspect pixels.</span>'; truth.innerHTML='<span class="empty">Truth comparison appears after rendering.</span>'; return; }
  box.replaceChildren(
    pill(run.mode),
    pill(run.completed?"completed":"incomplete", run.completed?"good":"bad"),
    pill("v "+run.version.slice(0,10)),
    pill(run.tiles_rendered+" rendered / "+run.tiles_reused+" reused"),
    pill(run.elapsed_seconds.toFixed(3)+" s"),
    pill("quant max "+run.quant_stats.max_abs.toExponential(2))
  );
  if(run.image_url){
    img.innerHTML='<img src="'+run.image_url+'" alt="rendered frame">';
  }
  if(run.truth_results && run.truth_results.length){
    let html='<table><tr><th>pixel</th><th>object</th><th>max ΔRGB</th><th>Δpeak</th><th>Δxy</th><th>pass</th></tr>';
    for(const t of run.truth_results){
      html += '<tr><td>'+t.x+','+t.y+'</td><td>'+t.object_id+'</td><td>'+t.max_abs_error.toExponential(3)+'</td><td>'+t.peak_abs_error.toExponential(3)+'</td><td>'+t.chromaticity_abs_error.toExponential(3)+'</td><td>'+(t.pass?'yes':'NO')+'</td></tr>';
    }
    truth.innerHTML=html+'</table>';
  } else truth.innerHTML='<span class="empty">No completed frame.</span>';
}
function renderVerify(v){
  const mismatch = v.first_mismatch ? JSON.stringify(v.first_mismatch) : "none";
  log((v.ok?"VERIFY OK":"VERIFY FAILED")+"\n"+
      "tiles="+v.tiles_checked+" pixels="+v.pixels_checked+" commit_order="+v.commit_order_ok+"\n"+
      "main="+v.main_render_seconds.toFixed(4)+"s verifier="+v.verifier_seconds.toFixed(4)+"s ratio="+v.main_to_verifier_ratio.toFixed(2)+"\n"+
      "first_mismatch="+mismatch+"\n"+
      "wait="+JSON.stringify(v.wait_stats));
}
async function applyConfig(){
  try {
    const cfg=collectConfig();
    const snap=await api("/api/params",{method:"POST",body:JSON.stringify(cfg)});
    state.snapshot=snap;
    q("version").textContent="version "+snap.version.slice(0,12);
    log("Snapshot updated: "+snap.version);
  } catch(e){ log("Apply failed: "+e.message); }
}
async function runRender(kind){
  try {
    log((kind==="preview"?"Applying latest snapshot preview":"Running "+kind)+" …");
    const data=await api("/api/"+kind,{method:"POST"});
    if(kind==="preview"){ log("Preview complete: "+data.tiles_rendered+" tiles in "+data.elapsed_seconds.toFixed(3)+"s\n"+JSON.stringify(data.wait_stats)); }
    else { state.exact=data; renderExact(data); log("Exact complete: "+data.tiles_rendered+" tiles in "+data.elapsed_seconds.toFixed(3)+"s"); }
    refresh();
  } catch(e){ log(kind+" error: "+e.message); }
}
async function runVerify(){
  try { log("Verifying persisted checkpoint independently …"); const data=await api("/api/verify",{method:"POST"}); renderVerify(data); }
  catch(e){ log("Verify error: "+e.message); }
}
refresh();
</script>
</body>
</html>`
