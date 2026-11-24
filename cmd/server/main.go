package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"spectralrender"
)

type app struct {
	mu         sync.Mutex
	renderMu   sync.Mutex
	scene      *spectralrender.Scene
	sceneHash  string
	truth      *spectralrender.TruthManifest
	params     *spectralrender.ParamStore
	store      *spectralrender.CheckpointStore
	frame      *spectralrender.Frame
	report     *spectralrender.RenderReport
	truthCheck *spectralrender.TruthCheckReport
	lastError  string
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "loopback HTTP listen address")
	samples := flag.String("samples", "samples", "deterministic sample directory")
	checkpointDir := flag.String("checkpoints", "checkpoints", "fixed persistent checkpoint directory")
	autoGenerate := flag.Bool("auto-generate", true, "generate samples if missing")
	flag.Parse()

	scenePath := filepath.Join(*samples, "scene.json")
	if *autoGenerate {
		if _, err := os.Stat(scenePath); os.IsNotExist(err) {
			fmt.Printf("samples missing; generating deterministic samples in %s\n", *samples)
			if _, err := spectralrender.GenerateSamples(*samples, 42); err != nil {
				log.Fatal(err)
			}
		}
	}
	bundle, err := spectralrender.LoadSampleBundle(*samples)
	if err != nil {
		log.Fatal(err)
	}
	scene, sceneHash, truth := bundle.Scene, bundle.SceneHash, bundle.Truth
	store, err := spectralrender.OpenCheckpointStore(*checkpointDir)
	if err != nil {
		log.Fatal(err)
	}
	paramStore, perr := spectralrender.NewParamStore(spectralrender.DefaultRenderParams())
	if perr != nil {
		log.Fatal(perr)
	}
	a := &app{scene: scene, sceneHash: sceneHash, truth: truth, params: paramStore, store: store}
	frame, report, rerr := spectralrender.RenderPreview(scene, sceneHash, paramStore,
		spectralrender.RenderOptions{Store: store})
	if rerr != nil {
		log.Fatal(rerr)
	}
	tc, terr := spectralrender.CheckTruthFrame(frame, truth)
	if terr != nil {
		log.Fatal(terr)
	}
	a.frame, a.report, a.truthCheck = frame, report, tc

	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/preview", a.handlePreview)
	mux.HandleFunc("/api/exact", a.handleExact)
	mux.HandleFunc("/api/verify", a.handleVerify)
	mux.HandleFunc("/frame.png", a.handleFramePNG)
	fmt.Printf("spectralrender example listening on http://%s (checkpoints=%s)\n", *addr, *checkpointDir)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

type statusResponse struct {
	SceneHash       string                           `json:"scene_hash"`
	CurrentVersion  uint64                           `json:"current_param_version"`
	FrameVersion    uint64                           `json:"frame_param_version"`
	SamplingVersion string                           `json:"sampling_version"`
	MaterialKey     string                           `json:"material_cache_key"`
	Frame           *frameSummary                    `json:"frame,omitempty"`
	Report          *spectralrender.RenderReport     `json:"render_report,omitempty"`
	Truth           *spectralrender.TruthCheckReport `json:"truth_check,omitempty"`
	LastError       string                           `json:"last_error,omitempty"`
}

type frameSummary struct {
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	ParamVersion  uint64  `json:"param_version"`
	Exposure      float64 `json:"exposure"`
	ToneCurve     string  `json:"tone_curve"`
	Gamma         float64 `json:"gamma"`
	EncodedSHA256 string  `json:"encoded_sha256"`
}

func summarize(frame *spectralrender.Frame) *frameSummary {
	if frame == nil {
		return nil
	}
	return &frameSummary{
		Width: frame.Width, Height: frame.Height,
		ParamVersion: frame.ParamVersion,
		Exposure:     frame.Exposure, ToneCurve: string(frame.ToneCurve), Gamma: frame.Gamma,
		EncodedSHA256: frame.EncodedSHA256,
	}
}

func (a *app) status() statusResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := statusResponse{
		SceneHash:       a.sceneHash,
		CurrentVersion:  a.params.Snapshot().Version,
		FrameVersion:    a.frame.ParamVersion,
		SamplingVersion: a.frame.SamplingVersion,
		MaterialKey:     a.frame.MaterialCacheKey,
		Frame:           summarize(a.frame),
		Report:          a.report,
		Truth:           a.truthCheck,
		LastError:       a.lastError,
	}
	return s
}

func (a *app) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.status())
}

func (a *app) handlePreview(w http.ResponseWriter, r *http.Request) {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	current := a.params.Snapshot()
	if err := applyQuery(&current, r); err != nil {
		writeError(w, err)
		return
	}
	updated, uerr := a.params.Update(current)
	if uerr != nil {
		writeError(w, uerr)
		return
	}
	frame, report, err := spectralrender.Render(a.scene, a.sceneHash, updated,
		spectralrender.RenderOptions{Store: a.store})
	if err != nil {
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		if report != nil {
			a.mu.Lock()
			a.report = report
			a.mu.Unlock()
		}
		writeError(w, err)
		return
	}
	tc, terr := spectralrender.CheckTruthFrame(frame, a.truth)
	if terr != nil {
		writeError(w, terr)
		return
	}
	a.mu.Lock()
	a.frame, a.report, a.truthCheck, a.lastError = frame, report, tc, ""
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, a.status())
}

func (a *app) handleExact(w http.ResponseWriter, r *http.Request) {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	current := a.params.Snapshot()
	if err := applyQuery(&current, r); err != nil {
		writeError(w, err)
		return
	}
	if _, err := a.params.Update(current); err != nil {
		writeError(w, err)
		return
	}
	// Capture the version at the start of the exact render.
	exact := spectralrender.BeginExact(a.params)
	frame, report, err := exact.Render(a.scene, a.sceneHash,
		spectralrender.RenderOptions{Store: a.store})
	if err != nil {
		a.mu.Lock()
		a.lastError = err.Error()
		a.mu.Unlock()
		writeError(w, err)
		return
	}
	tc, terr := spectralrender.CheckTruthFrame(frame, a.truth)
	if terr != nil {
		writeError(w, terr)
		return
	}
	a.mu.Lock()
	a.frame, a.report, a.truthCheck, a.lastError = frame, report, tc, ""
	a.mu.Unlock()
	writeJSON(w, http.StatusOK, struct {
		LockedVersion uint64         `json:"locked_param_version"`
		Status        statusResponse `json:"status"`
	}{LockedVersion: exact.Params().Version, Status: a.status()})
}

func (a *app) handleVerify(w http.ResponseWriter, r *http.Request) {
	a.renderMu.Lock()
	defer a.renderMu.Unlock()
	report, err := spectralrender.VerifyCheckpoint(a.scene, a.sceneHash, a.truth,
		a.store.Root(), spectralrender.DefaultBoundModel())
	if err != nil {
		if report != nil {
			writeJSON(w, http.StatusConflict, struct {
				Report *spectralrender.VerifyReport `json:"report"`
				Error  any                          `json:"error"`
			}{Report: report, Error: spectralrender.AsError(err)})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (a *app) handleFramePNG(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	frame := a.frame
	a.mu.Unlock()
	if frame == nil {
		writeError(w, &spectralrender.Error{Code: spectralrender.ErrState, Message: "no frame available"})
		return
	}
	if raw := r.URL.Query().Get("v"); raw != "" {
		v, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			writeError(w, &spectralrender.Error{Code: spectralrender.ErrPara, Field: "v", Message: "invalid version"})
			return
		}
		if v != frame.ParamVersion {
			writeError(w, spectralrender.RequireSameVersion(v, frame.ParamVersion, "version"))
			return
		}
	}
	img := image.NewRGBA(image.Rect(0, 0, frame.Width, frame.Height))
	for y := 0; y < frame.Height; y++ {
		for x := 0; x < frame.Width; x++ {
			i := (y*frame.Width + x) * 3
			img.SetRGBA(x, y, color.RGBA{
				R: frame.EncodedRGB[i], G: frame.EncodedRGB[i+1], B: frame.EncodedRGB[i+2], A: 255,
			})
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_ = png.Encode(w, img)
}

func applyQuery(p *spectralrender.RenderParams, r *http.Request) error {
	q := r.URL.Query()
	if v := q.Get("exposure"); v != "" {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return &spectralrender.Error{Code: spectralrender.ErrPara, Field: "exposure", Message: "invalid number"}
		}
		p.Exposure = x
	}
	if v := q.Get("gamma"); v != "" {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return &spectralrender.Error{Code: spectralrender.ErrPara, Field: "gamma", Message: "invalid number"}
		}
		p.Gamma = x
	}
	if v := q.Get("bounces"); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil {
			return &spectralrender.Error{Code: spectralrender.ErrPara, Field: "bounces", Message: "invalid integer"}
		}
		p.Bounces = x
	}
	if v := q.Get("sample_count"); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil {
			return &spectralrender.Error{Code: spectralrender.ErrPara, Field: "sample_count", Message: "invalid integer"}
		}
		p.SampleCount = x
	}
	if v := q.Get("parallel"); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil {
			return &spectralrender.Error{Code: spectralrender.ErrPara, Field: "max_parallel", Message: "invalid integer"}
		}
		p.MaxParallel = x
	}
	if v := q.Get("curve"); v != "" {
		p.ToneCurve = spectralrender.ToneCurve(v)
	}
	if v := q.Get("integration"); v != "" {
		p.Integration = spectralrender.IntegrationMethod(v)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch spectralrender.CodeOf(err) {
	case spectralrender.ErrVer:
		status = http.StatusConflict
	case spectralrender.ErrGamut:
		status = http.StatusUnprocessableEntity
	case spectralrender.ErrState, spectralrender.ErrCheckpoint, spectralrender.ErrDecode:
		status = http.StatusInternalServerError
	}
	e := spectralrender.AsError(err)
	if e == nil {
		message := "unknown server error"
		if err != nil {
			func() {
				defer func() { _ = recover() }()
				message = err.Error()
			}()
		}
		e = &spectralrender.Error{Code: spectralrender.ErrState, Message: message}
	}
	writeJSON(w, status, struct {
		Error *spectralrender.Error `json:"error"`
	}{Error: e})
}

const indexHTML = `<!doctype html>
<html lang="zh">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>spectralrender diagnostics</title>
<style>
body{font-family:ui-monospace,SFMono-Regular,Menlo,monospace;background:#10151b;color:#d8e2ea;margin:0;padding:24px}
main{max-width:1180px;margin:auto;display:grid;grid-template-columns:minmax(0,1.3fr) minmax(320px,.7fr);gap:20px}
section{background:#17202a;border:1px solid #2b3a48;border-radius:10px;padding:16px}
img{width:100%;image-rendering:pixelated;border-radius:6px;background:#000}
label{display:block;margin:10px 0 4px;color:#8ea6b8;font-size:12px}
input,select{width:100%;box-sizing:border-box;background:#0d1318;color:#e8f0f5;border:1px solid #34495a;border-radius:5px;padding:7px}
button{margin:8px 8px 0 0;background:#1f6f8b;color:white;border:0;border-radius:6px;padding:9px 12px;cursor:pointer}
button.alt{background:#6a4a24}button.verify{background:#286a45}
pre{white-space:pre-wrap;word-break:break-word;font-size:12px;color:#a9c0d0;max-height:52vh;overflow:auto}
.err{color:#ff8a8a}.ok{color:#7ce6a1}
@media(max-width:800px){main{grid-template-columns:1fr}}
</style>
</head>
<body><main>
<section>
<h2>光谱材质渲染 / 诊断面板</h2>
<img id="frame" src="/frame.png" alt="rendered frame">
<div id="msg"></div>
<h3>参数快照</h3>
<label>曝光 exposure</label><input id="exposure" type="number" step="0.05" value="1">
<label>伽马 gamma</label><input id="gamma" type="number" step="0.05" value="2.2">
<label>间接反弹 bounces</label><input id="bounces" type="number" step="1" min="0" max="8" value="2">
<label>波长采样步数 sample_count</label><input id="sample_count" type="number" step="1" min="4" max="471" value="35">
<label>积分方式 integration</label>
<select id="integration"><option value="trapezoid">trapezoid</option><option value="rectangle">rectangle</option><option value="simpson">simpson</option></select>
<label>色调曲线 tone_curve</label>
<select id="curve"><option value="aces">aces</option><option value="reinhard">reinhard</option><option value="linear">linear</option></select>
<button onclick="preview()">连续预览（最新快照）</button>
<button class="alt" onclick="exact()">精确渲染（启动时锁定）</button>
<button class="verify" onclick="verify()">独立校验块/图像</button>
</section>
<section>
<h3>状态</h3><div id="state"></div>
<h3>诊断 JSON</h3><pre id="diag">loading...</pre>
</section>
</main>
<script>
const $=id=>document.getElementById(id);
function query(){return new URLSearchParams({exposure:$('exposure').value,gamma:$('gamma').value,bounces:$('bounces').value,sample_count:$('sample_count').value,integration:$('integration').value,curve:$('curve').value})}
async function call(path, opts){const r=await fetch(path,opts);const t=await r.text();let j;try{j=JSON.parse(t)}catch(e){j={raw:t}};if(!r.ok){$('msg').className='err';$('msg').textContent=(j.error&&j.error.code)||('HTTP '+r.status)+' '+(j.error?j.error.message:t);$('diag').textContent=JSON.stringify(j,null,2);return null}$('msg').className='ok';$('msg').textContent='';return j}
async function status(){const j=await call('/api/status');if(j){$('diag').textContent=JSON.stringify(j,null,2);$('state').innerHTML='<span class="'+(j.truth_check&&j.truth_check.pass?'ok':'err')+'">'+(j.truth_check&&j.truth_check.pass?'truth PASS':'truth FAIL')+'</span> · frame v'+j.frame_param_version+' · current v'+j.current_param_version;const f=$('frame');f.src='/frame.png?v='+j.frame_param_version+'&t='+Date.now()}}
async function preview(){await call('/api/preview?'+query().toString());await status()}
async function exact(){await call('/api/exact?'+query().toString());await status()}
async function verify(){const j=await call('/api/verify');if(j){$('diag').textContent=JSON.stringify(j,null,2)}}
status();setInterval(status,5000);
</script>
</body></html>`
