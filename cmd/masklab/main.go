package main

import (
	"context"
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
	"runtime"
	"sync"
	"time"

	"example.com/iconmask"
)

type application struct {
	project    *iconmask.Project
	samples    map[string]iconmask.ImageSample
	database   string
	samplesDir string
	seed       int64

	mu   sync.RWMutex
	last map[string]*runState
}

type runState struct {
	Request     runRequest
	Snapshot    iconmask.ParameterSnapshot
	Exact       *iconmask.RepairResult
	Preview     *iconmask.PreviewResult
	Comparison  iconmask.PathComparison
	Consistency *iconmask.ThreePathReport
	RunID       string
}

type runRequest struct {
	ImageID           string  `json:"imageId"`
	Shape             string  `json:"shape"`
	RadiusMin         int     `json:"radiusMin"`
	RadiusMax         int     `json:"radiusMax"`
	CurvatureBands    int     `json:"curvatureBands"`
	MaxRounds         int     `json:"maxRounds"`
	BlockSize         int     `json:"blockSize"`
	ConvergencePixels int     `json:"convergencePixels"`
	Downsample        int     `json:"downsample"`
	PreviewRounds     int     `json:"previewRounds"`
	CentroidTolerance float64 `json:"centroidTolerance"`
	AreaTolerance     float64 `json:"areaTolerance"`
	MaxWorkers        int     `json:"maxWorkers"`
	PixelBufferBudget int     `json:"pixelBufferBudget"`
	QueueBudget       int     `json:"queueBudget"`
	RunThreePath      bool    `json:"runThreePath"`
}

func defaultRunRequest(imageID string) runRequest {
	repair := iconmask.DefaultRepairConfig()
	preview := iconmask.DefaultPreviewConfig()
	parallel := iconmask.DefaultParallelConfig()
	return runRequest{
		ImageID:           imageID,
		Shape:             repair.Shape,
		RadiusMin:         repair.Radius.Min,
		RadiusMax:         repair.Radius.Max,
		CurvatureBands:    repair.Radius.CurvatureBands,
		MaxRounds:         repair.MaxRounds,
		BlockSize:         repair.BlockSize,
		ConvergencePixels: repair.ConvergencePixels,
		Downsample:        preview.Downsample,
		PreviewRounds:     preview.Rounds,
		CentroidTolerance: preview.CentroidTolerance,
		AreaTolerance:     preview.AreaRelativeTolerance,
		MaxWorkers:        parallel.MaxWorkers,
		PixelBufferBudget: parallel.PixelBufferBudget,
		QueueBudget:       parallel.QueueBudget,
		RunThreePath:      true,
	}
}

func (request runRequest) repairConfig() iconmask.RepairConfig {
	return iconmask.RepairConfig{
		Radius: iconmask.RadiusConfig{
			Min:            request.RadiusMin,
			Max:            request.RadiusMax,
			CurvatureBands: request.CurvatureBands,
		},
		Shape:             request.Shape,
		MaxRounds:         request.MaxRounds,
		BlockSize:         request.BlockSize,
		ConvergencePixels: request.ConvergencePixels,
	}
}

func (request runRequest) previewConfig() iconmask.PreviewConfig {
	return iconmask.PreviewConfig{
		Downsample:            request.Downsample,
		Rounds:                request.PreviewRounds,
		LatencyBudget:         iconmask.DefaultPreviewConfig().LatencyBudget,
		CentroidTolerance:     request.CentroidTolerance,
		AreaRelativeTolerance: request.AreaTolerance,
	}
}

func (request runRequest) parallelConfig() iconmask.ParallelConfig {
	return iconmask.ParallelConfig{
		MaxWorkers:        request.MaxWorkers,
		PixelBufferBudget: request.PixelBufferBudget,
		QueueBudget:       request.QueueBudget,
	}
}

type previewSummary struct {
	RegionCount      int           `json:"regionCount"`
	TotalArea        int           `json:"totalArea"`
	StatsPlaceholder bool          `json:"statsPlaceholder"`
	ActualDownsample int           `json:"actualDownsample"`
	RoundBudget      int           `json:"roundBudget"`
	RoundsUsed       int           `json:"roundsUsed"`
	Elapsed          time.Duration `json:"elapsedNanos"`
	WithinLatency    bool          `json:"withinLatency"`
	BudgetExhausted  bool          `json:"budgetExhausted"`
	Rationale        string        `json:"rationale"`
}

type threePathSummary struct {
	RunID                string `json:"runId"`
	MasksEqual           bool   `json:"masksEqual"`
	StatsEqual           bool   `json:"statsEqual"`
	CommitSequencesEqual bool   `json:"commitSequencesEqual"`
	IncrementalCommits   int    `json:"incrementalCommits"`
	ResumeCommits        int    `json:"resumeCommits"`
	FullCommits          int    `json:"fullCommits"`
	FirstInconsistent    string `json:"firstInconsistentBlock,omitempty"`
	QueueWaitEvents      int    `json:"queueWaitEvents"`
}

type runResponse struct {
	ImageID          string                    `json:"imageId"`
	ParameterVersion string                    `json:"parameterVersion"`
	EdgeEmpty        bool                      `json:"edgeEmpty"`
	Success          bool                      `json:"success"`
	EdgePixels       int                       `json:"edgePixels"`
	Rounds           []iconmask.RepairRoundLog `json:"rounds"`
	RadiusTable      *iconmask.RadiusTable     `json:"radiusTable"`
	InitialStats     iconmask.RegionStats      `json:"initialStats"`
	TruthStats       iconmask.RegionStats      `json:"truthStats"`
	RepairedStats    iconmask.RegionStats      `json:"repairedStats"`
	Accuracy         iconmask.AccuracyReport   `json:"accuracy"`
	Preview          previewSummary            `json:"preview"`
	Comparison       iconmask.PathComparison   `json:"comparison"`
	ThreePath        *threePathSummary         `json:"threePath,omitempty"`
	ExactElapsed     time.Duration             `json:"exactElapsedNanos"`
	Error            *iconmask.Error           `json:"error,omitempty"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8099", "localhost HTTP listen address")
	seed := flag.Int64("seed", 42, "deterministic sample seed")
	database := flag.String("db", "artifacts/masklab.db", "SQLite checkpoint database")
	samplesDir := flag.String("samples", "artifacts/samples", "generated sample directory")
	flag.Parse()

	project := iconmask.GenerateProject(*seed)
	if err := iconmask.ValidateProject(project); err != nil {
		log.Fatalf("generated project invalid: %v", err)
	}
	if err := iconmask.SaveProject(filepath.Join(*samplesDir, "project.json"), project); err != nil {
		log.Fatalf("save project: %v", err)
	}
	if err := writeSamplePNGs(project, *samplesDir); err != nil {
		log.Fatalf("write sample PNGs: %v", err)
	}
	store, err := iconmask.OpenCheckpointStore(*database)
	if err != nil {
		log.Fatalf("open checkpoint database: %v", err)
	}
	_ = store.Close()

	samples := make(map[string]iconmask.ImageSample, len(project.Images))
	for _, sample := range project.Images {
		samples[sample.ID] = sample
	}
	app := &application{
		project:    project,
		samples:    samples,
		database:   *database,
		samplesDir: *samplesDir,
		seed:       *seed,
		last:       make(map[string]*runState),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/health", app.handleHealth)
	mux.HandleFunc("/api/samples", app.handleSamples)
	mux.HandleFunc("/api/run", app.handleRun)
	mux.HandleFunc("/api/mask", app.handleMask)
	mux.HandleFunc("/api/source-image", app.handleSourceImage)
	mux.HandleFunc("/api/checkpoints", app.handleCheckpoints)
	mux.HandleFunc("/api/project", app.handleProject)

	server := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("iconmask workbench listening on http://%s", *addr)
	log.Printf("samples=%s database=%s seed=%d go=%s", *samplesDir, *database, *seed, runtime.Version())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func (app *application) handleIndex(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = writer.Write([]byte(indexHTML))
}

func (app *application) handleHealth(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":           "ok",
		"go":               runtime.Version(),
		"seed":             app.seed,
		"algorithmVersion": iconmask.AlgorithmVersion,
		"images":           len(app.project.Images),
		"database":         app.database,
	})
}

func (app *application) handleSamples(writer http.ResponseWriter, request *http.Request) {
	type sampleInfo struct {
		ID             string `json:"id"`
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		Seed           int64  `json:"seed"`
		Components     int    `json:"components"`
		Segments       int    `json:"segments"`
		InitialPixels  int    `json:"initialForegroundPixels"`
		TruthPixels    int    `json:"truthForegroundPixels"`
		ReferenceHoles int    `json:"referenceHoles"`
	}
	infos := make([]sampleInfo, 0, len(app.project.Images))
	for _, sample := range app.project.Images {
		holes := 0
		for _, component := range sample.Truth.Components {
			holes += component.Holes
		}
		infos = append(infos, sampleInfo{
			ID:             sample.ID,
			Width:          sample.Width,
			Height:         sample.Height,
			Seed:           sample.Seed,
			Components:     len(sample.Truth.Components),
			Segments:       len(sample.Truth.Segments),
			InitialPixels:  sample.InitialMask.Count(),
			TruthPixels:    sample.Truth.ReferenceMask.Count(),
			ReferenceHoles: holes,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{"seed": app.seed, "images": infos})
}

func (app *application) handleRun(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}
	imageID := request.URL.Query().Get("image")
	if imageID == "" {
		imageID = app.project.Images[0].ID
	}
	req := defaultRunRequest(imageID)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&req); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.ImageID == "" {
		req.ImageID = imageID
	}
	sample, ok := app.samples[req.ImageID]
	if !ok {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": "unknown image " + req.ImageID})
		return
	}
	repairCfg := req.repairConfig()
	snapshot := iconmask.NewParameterSnapshot(repairCfg)
	start := time.Now()
	dual, dualErr := iconmask.RunDualPath(sample, snapshot, req.previewConfig())
	if dualErr != nil && dual == nil {
		writeJSON(writer, statusForError(dualErr), map[string]any{"error": dualErr})
		return
	}
	state := &runState{
		Request:  req,
		Snapshot: snapshot,
	}
	if dual != nil {
		state.Exact = dual.Exact
		state.Preview = dual.Preview
		state.Comparison = dual.Comparison
	}
	runID := "ui-" + sample.ID + "-" + snapshot.Version
	if req.RunThreePath {
		consistencyCfg := iconmask.DefaultConsistencyConfig()
		consistencyCfg.Repair = repairCfg
		consistencyCfg.Preview = req.previewConfig()
		consistencyCfg.Parallel = req.parallelConfig()
		consistencyCfg.DatabasePath = app.database
		consistencyCfg.RunID = runID
		report, consistencyErr := iconmask.RunThreePathConsistency(context.Background(), sample, snapshot, consistencyCfg)
		if consistencyErr != nil && report == nil {
			writeJSON(writer, statusForError(consistencyErr), map[string]any{"error": consistencyErr})
			return
		}
		state.Consistency = report
		if consistencyErr != nil {
			dualErr = consistencyErr
		}
	}
	state.RunID = runID
	app.mu.Lock()
	app.last[sample.ID] = state
	app.mu.Unlock()

	response := runResponse{
		ImageID:          sample.ID,
		ParameterVersion: snapshot.Version,
		InitialStats:     iconmask.AnalyzeMask(sample.InitialMask),
		TruthStats:       iconmask.AnalyzeMask(sample.Truth.ReferenceMask),
		ExactElapsed:     time.Since(start),
	}
	if state.Exact != nil {
		response.EdgeEmpty = state.Exact.EdgeEmpty
		response.Success = state.Exact.Success
		response.EdgePixels = state.Exact.EdgeBand.Count()
		response.Rounds = state.Exact.Rounds
		response.RadiusTable = state.Exact.RadiusTable
		response.RepairedStats = state.Exact.Stats
		response.Accuracy = state.Exact.Accuracy
	}
	if state.Preview != nil {
		response.Preview = previewSummary{
			RegionCount:      state.Preview.Stats.Count,
			TotalArea:        state.Preview.Stats.TotalArea,
			StatsPlaceholder: state.Preview.StatsPlaceholder,
			ActualDownsample: state.Preview.ActualDownsample,
			RoundBudget:      state.Preview.RoundBudget,
			RoundsUsed:       state.Preview.RoundsUsed,
			Elapsed:          state.Preview.Elapsed,
			WithinLatency:    state.Preview.WithinLatency,
			BudgetExhausted:  state.Preview.BudgetExhausted,
			Rationale:        state.Preview.Rationale,
		}
		response.Comparison = state.Comparison
	}
	if state.Consistency != nil {
		summary := &threePathSummary{
			RunID:                state.RunID,
			MasksEqual:           state.Consistency.MasksEqual,
			StatsEqual:           state.Consistency.StatsEqual,
			CommitSequencesEqual: state.Consistency.CommitSequencesEqual,
			IncrementalCommits:   len(state.Consistency.Incremental.Commits),
			ResumeCommits:        len(state.Consistency.Resume.Commits),
			FullCommits:          len(state.Consistency.Full.Commits),
			QueueWaitEvents:      len(state.Consistency.Full.WaitEvents),
		}
		if state.Consistency.FirstInconsistentBlock != nil {
			summary.FirstInconsistent = fmt.Sprintf("%d,%d",
				state.Consistency.FirstInconsistentBlock.X, state.Consistency.FirstInconsistentBlock.Y)
		}
		response.ThreePath = summary
	}
	response.Error = dualErr
	status := http.StatusOK
	if dualErr != nil {
		status = statusForError(dualErr)
	}
	writeJSON(writer, status, response)
}

func statusForError(err error) int {
	switch {
	case iconmask.IsCode(err, iconmask.CodeSchema),
		iconmask.IsCode(err, iconmask.CodeParameter),
		iconmask.IsCode(err, iconmask.CodeRadius):
		return http.StatusBadRequest
	case iconmask.IsCode(err, iconmask.CodeDivergence):
		return http.StatusUnprocessableEntity
	case iconmask.IsCode(err, iconmask.CodeIteration):
		return http.StatusConflict
	case iconmask.IsCode(err, iconmask.CodeState):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (app *application) handleMask(writer http.ResponseWriter, request *http.Request) {
	imageID := request.URL.Query().Get("image")
	kind := request.URL.Query().Get("kind")
	sample, ok := app.samples[imageID]
	if !ok {
		http.Error(writer, "unknown image", http.StatusNotFound)
		return
	}
	var mask iconmask.Mask
	switch kind {
	case "initial":
		mask = sample.InitialMask
	case "truth":
		mask = sample.Truth.ReferenceMask
	case "edge", "repaired", "preview":
		app.mu.RLock()
		state := app.last[imageID]
		app.mu.RUnlock()
		if state == nil || state.Exact == nil {
			http.Error(writer, "run the diagnostics first", http.StatusNotFound)
			return
		}
		switch kind {
		case "edge":
			mask = state.Exact.EdgeBand
		case "repaired":
			mask = state.Exact.Mask
		case "preview":
			if state.Preview == nil {
				http.Error(writer, "preview unavailable", http.StatusNotFound)
				return
			}
			mask = state.Preview.Mask
		}
	default:
		http.Error(writer, "kind must be initial, truth, edge, repaired, or preview", http.StatusBadRequest)
		return
	}
	writeMaskPNG(writer, mask)
}

func (app *application) handleSourceImage(writer http.ResponseWriter, request *http.Request) {
	imageID := request.URL.Query().Get("image")
	sample, ok := app.samples[imageID]
	if !ok || len(sample.ImagePNG) == 0 {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "image/png")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = writer.Write(sample.ImagePNG)
}

func (app *application) handleCheckpoints(writer http.ResponseWriter, request *http.Request) {
	imageID := request.URL.Query().Get("image")
	app.mu.RLock()
	state := app.last[imageID]
	app.mu.RUnlock()
	if state == nil || state.Consistency == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"error": "no three-path run for image"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"runId":       state.RunID,
		"incremental": state.Consistency.Incremental.Commits,
		"waitEvents":  state.Consistency.Full.WaitEvents,
	})
}

func (app *application) handleProject(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, app.project)
}

func writeMaskPNG(writer http.ResponseWriter, mask iconmask.Mask) {
	gray := image.NewGray(image.Rect(0, 0, mask.Width, mask.Height))
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			value := byte(0)
			if mask.At(x, y) != 0 {
				value = 255
			}
			gray.SetGray(x, y, color.Gray{Y: value})
		}
	}
	writer.Header().Set("Content-Type", "image/png")
	writer.Header().Set("Cache-Control", "no-store")
	if err := png.Encode(writer, gray); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
	}
}

func writeSamplePNGs(project *iconmask.Project, directory string) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	for _, sample := range project.Images {
		if err := os.WriteFile(filepath.Join(directory, sample.ID+".png"), sample.ImagePNG, 0o644); err != nil {
			return err
		}
		if err := writeMaskFile(filepath.Join(directory, sample.ID+"-initial.png"), sample.InitialMask); err != nil {
			return err
		}
		if err := writeMaskFile(filepath.Join(directory, sample.ID+"-truth.png"), sample.Truth.ReferenceMask); err != nil {
			return err
		}
	}
	return nil
}

func writeMaskFile(path string, mask iconmask.Mask) error {
	gray := image.NewGray(image.Rect(0, 0, mask.Width, mask.Height))
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			if mask.At(x, y) != 0 {
				gray.Pix[y*gray.Stride+x] = 255
			}
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return png.Encode(file, gray)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		start := time.Now()
		next.ServeHTTP(writer, request)
		log.Printf("%s %s %s", request.Method, request.URL.RequestURI(), time.Since(start))
	})
}

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Icon Mask Workbench</title>
<style>
:root { color-scheme: dark; --bg:#101317; --panel:#171c22; --line:#2c3440; --fg:#e9eef4; --muted:#93a1b1; --ok:#61d095; --bad:#ff7272; --accent:#f0be5c; }
* { box-sizing: border-box; }
body { margin:0; font:14px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace; background:radial-gradient(1200px 700px at 10% 0%,#1d2733 0%,var(--bg) 55%); color:var(--fg); }
header { padding:24px 28px 8px; display:flex; align-items:end; justify-content:space-between; gap:20px; }
h1 { margin:0; font-size:22px; letter-spacing:.04em; }
.sub { color:var(--muted); margin-top:4px; }
.grid { display:grid; grid-template-columns: 330px minmax(0,1fr); gap:18px; padding:16px 28px 36px; }
.panel { background:rgba(23,28,34,.92); border:1px solid var(--line); border-radius:14px; padding:16px; box-shadow:0 12px 40px #0005; }
.panel h2 { font-size:13px; text-transform:uppercase; letter-spacing:.12em; color:var(--muted); margin:0 0 14px; }
label { display:grid; grid-template-columns:1fr 100px; gap:10px; align-items:center; margin:8px 0; color:var(--muted); }
input,select,button { font:inherit; color:var(--fg); background:#0f141a; border:1px solid var(--line); border-radius:8px; padding:7px 9px; }
input:focus,select:focus { outline:1px solid var(--accent); }
button { cursor:pointer; background:var(--accent); color:#16130c; border:none; font-weight:700; padding:10px 12px; }
button:hover { filter:brightness(1.08); }
.actions { display:flex; gap:8px; margin-top:14px; }
.actions button { flex:1; }
.preview-grid { display:grid; grid-template-columns:repeat(6,minmax(0,1fr)); gap:10px; }
.figure { background:#0d1116; border:1px solid var(--line); border-radius:10px; padding:8px; }
.figure img { width:100%; aspect-ratio:1; image-rendering:pixelated; object-fit:contain; background:repeating-conic-gradient(#1b222a 0% 25%,#11161c 0% 50%) 50%/16px 16px; border-radius:6px; }
.figure span { display:block; text-align:center; color:var(--muted); padding-top:6px; }
.cards { display:grid; grid-template-columns:repeat(4,minmax(0,1fr)); gap:10px; margin-top:14px; }
.card { border:1px solid var(--line); border-radius:10px; padding:12px; background:#11161c; }
.card b { display:block; font-size:20px; color:var(--accent); }
.card span { color:var(--muted); font-size:12px; }
pre { white-space:pre-wrap; word-break:break-word; background:#0b0f14; border:1px solid var(--line); border-radius:10px; padding:12px; max-height:280px; overflow:auto; }
.ok { color:var(--ok); } .bad { color:var(--bad); }
table { width:100%; border-collapse:collapse; margin-top:12px; }
th,td { text-align:left; border-bottom:1px solid var(--line); padding:7px 5px; }
th { color:var(--muted); font-weight:500; }
@media (max-width:1100px) { .grid { grid-template-columns:1fr; } .preview-grid { grid-template-columns:repeat(3,1fr); } }
@media (max-width:650px) { .preview-grid { grid-template-columns:repeat(2,1fr); } .cards { grid-template-columns:repeat(2,1fr); } header { align-items:flex-start; flex-direction:column; } }
</style>
</head>
<body>
<header>
  <div><h1>ICON MASK WORKBENCH</h1><div class="sub">deterministic samples · adaptive radii · preview/exact · SQLite checkpoints</div></div>
  <div class="sub" id="health">probing…</div>
</header>
<main class="grid">
  <section class="panel">
    <h2>Run Parameters</h2>
    <label>Sample <select id="imageId"></select></label>
    <label>Element <select id="shape"><option>square</option><option>disk</option><option>diamond</option><option>cross</option></select></label>
    <label>Radius min <input id="radiusMin" type="number" min="1" value="2"></label>
    <label>Radius max <input id="radiusMax" type="number" min="1" value="4"></label>
    <label>Curvature bands <input id="curvatureBands" type="number" min="1" value="4"></label>
    <label>Max rounds <input id="maxRounds" type="number" min="1" value="8"></label>
    <label>Block size <input id="blockSize" type="number" min="4" value="32"></label>
    <label>Preview scale <input id="downsample" type="number" min="1" value="2"></label>
    <label>Preview rounds <input id="previewRounds" type="number" min="1" value="2"></label>
    <label>Centroid tol <input id="centroidTolerance" type="number" step="0.1" value="3"></label>
    <label>Area rel tol <input id="areaTolerance" type="number" step="0.01" value="0.22"></label>
    <label>Workers <input id="maxWorkers" type="number" min="1" value="2"></label>
    <label>Pixel budget <input id="pixelBufferBudget" type="number" min="1" value="4096"></label>
    <label>Queue budget <input id="queueBudget" type="number" min="0" value="2"></label>
    <label>Three paths <input id="runThreePath" type="checkbox" checked></label>
    <div class="actions"><button id="run">Run diagnostics</button></div>
    <p class="sub">localhost only · no external assets</p>
  </section>
  <section class="panel">
    <h2>Visual Lab</h2>
    <div class="preview-grid">
      <div class="figure"><img id="imgSource" alt="source"><span>source</span></div>
      <div class="figure"><img id="imgInitial" alt="initial"><span>initial mask</span></div>
      <div class="figure"><img id="imgTruth" alt="truth"><span>reference truth</span></div>
      <div class="figure"><img id="imgEdge" alt="edge"><span>edge band</span></div>
      <div class="figure"><img id="imgPreview" alt="preview"><span>preview approx</span></div>
      <div class="figure"><img id="imgRepaired" alt="repaired"><span>repaired exact</span></div>
    </div>
    <div class="cards">
      <div class="card"><b id="verdict">—</b><span>verdict</span></div>
      <div class="card"><b id="regions">—</b><span>regions exact / preview</span></div>
      <div class="card"><b id="iou">—</b><span>IoU vs truth</span></div>
      <div class="card"><b id="rounds">—</b><span>rounds / checkpoints</span></div>
    </div>
    <table>
      <thead><tr><th>metric</th><th>initial</th><th>truth</th><th>repaired</th><th>preview</th></tr></thead>
      <tbody id="statsBody"><tr><td colspan="5" class="sub">Run a sample to populate diagnostics.</td></tr></tbody>
    </table>
    <h2 style="margin-top:20px">Diagnostics JSON</h2>
    <pre id="output">Ready.</pre>
  </section>
</main>
<script>
const ids=['imageId','shape','radiusMin','radiusMax','curvatureBands','maxRounds','blockSize','downsample','previewRounds','centroidTolerance','areaTolerance','maxWorkers','pixelBufferBudget','queueBudget','runThreePath'];
let currentImage='';
async function boot(){
  const health=await (await fetch('/api/health')).json();
  document.getElementById('health').textContent=health.go+' · seed '+health.seed;
  const samples=await (await fetch('/api/samples')).json();
  const select=document.getElementById('imageId');
  select.innerHTML='';
  for(const s of samples.images){
    const opt=document.createElement('option'); opt.value=s.id; opt.textContent=s.id+' · '+s.width+'×'+s.height+' · '+s.components+' regions'; select.appendChild(opt);
  }
  currentImage=select.value;
  refreshStatic();
}
function refreshStatic(){
  const image=document.getElementById('imageId').value;
  currentImage=image;
  document.getElementById('imgSource').src='/api/source-image?image='+encodeURIComponent(image);
  document.getElementById('imgInitial').src='/api/mask?kind=initial&image='+encodeURIComponent(image);
  document.getElementById('imgTruth').src='/api/mask?kind=truth&image='+encodeURIComponent(image);
  document.getElementById('imgEdge').removeAttribute('src');
  document.getElementById('imgPreview').removeAttribute('src');
  document.getElementById('imgRepaired').removeAttribute('src');
}
function payload(){
  const p={};
  for(const id of ids){
    const el=document.getElementById(id);
    if(el.type==='checkbox') p[id]=el.checked;
    else if(el.type==='number') p[id]=Number(el.value);
    else p[id]=el.value;
  }
  p.imageId=document.getElementById('imageId').value;
  return p;
}
async function run(){
  const image=document.getElementById('imageId').value;
  const out=document.getElementById('output');
  document.getElementById('verdict').textContent='RUN';
  document.getElementById('verdict').className='';
  try{
    const res=await fetch('/api/run',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(payload())});
    const data=await res.json();
    out.textContent=JSON.stringify(data,null,2);
    const stamp=Date.now();
    document.getElementById('imgEdge').src='/api/mask?kind=edge&image='+encodeURIComponent(image)+'&v='+stamp;
    document.getElementById('imgPreview').src='/api/mask?kind=preview&image='+encodeURIComponent(image)+'&v='+stamp;
    document.getElementById('imgRepaired').src='/api/mask?kind=repaired&image='+encodeURIComponent(image)+'&v='+stamp;
    document.getElementById('regions').textContent=data.repairedStats.count+' / '+data.preview.regionCount;
    document.getElementById('iou').textContent=(data.accuracy.iou||0).toFixed(4);
    document.getElementById('rounds').textContent=(data.rounds||[]).length+' / '+(data.threePath?data.threePath.fullCommits:0);
    document.getElementById('statsBody').innerHTML=row('regions',data.initialStats.count,data.truthStats.count,data.repairedStats.count,data.preview.regionCount)+row('area',data.initialStats.totalArea,data.truthStats.totalArea,data.repairedStats.totalArea,data.preview.totalArea)+row('version',data.parameterVersion.slice(0,8),'','',data.parameterVersion.slice(0,8));
    if(data.error){ document.getElementById('verdict').textContent=data.error.code; document.getElementById('verdict').className='bad'; }
    else if(!data.success){ document.getElementById('verdict').textContent='EMPTY'; document.getElementById('verdict').className='bad'; }
    else { document.getElementById('verdict').textContent='PASS'; document.getElementById('verdict').className='ok'; }
  }catch(e){ out.textContent=String(e); document.getElementById('verdict').textContent='ERR'; document.getElementById('verdict').className='bad'; }
}
function row(name,a,b,c,d){return '<tr><td>'+name+'</td><td>'+a+'</td><td>'+b+'</td><td>'+c+'</td><td>'+d+'</td></tr>';}
document.getElementById('imageId').addEventListener('change',refreshStatic);
document.getElementById('run').addEventListener('click',run);
boot().catch(e=>{document.getElementById('health').textContent='boot error: '+e});
</script>
</body>
</html>`
