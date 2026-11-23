package workbench

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webAssets embed.FS

type SceneSession struct {
	Scene     *Scene
	Engine    *Engine
	CreatedAt time.Time

	cacheMu sync.Mutex
	cache   map[string][]byte
	lastCmp *ThreePathComparison
}

type Server struct {
	mu       sync.RWMutex
	sessions map[string]*SceneSession
	store    *CheckpointStore
	dataDir  string
}

type ThreePathComparison struct {
	Consistent         bool             `json:"consistent"`
	LivePeak           float64          `json:"livePeak"`
	ResumePeak         float64          `json:"resumePeak"`
	FullPeak           float64          `json:"fullPeak"`
	TiledPeak          float64          `json:"tiledPeak"`
	LiveGamutHash      string           `json:"liveGamutHash"`
	ResumeGamutHash    string           `json:"resumeGamutHash"`
	FullGamutHash      string           `json:"fullGamutHash"`
	TiledGamutHash     string           `json:"tiledGamutHash"`
	FirstMismatch      *PixelCoord      `json:"firstMismatch,omitempty"`
	FirstMismatchBlock int              `json:"firstMismatchBlock"`
	Paths              []PathComparison `json:"paths"`
	CheckedAt          string           `json:"checkedAt"`
}

type PatchCheck struct {
	ID       string     `json:"id"`
	X        int        `json:"x"`
	Y        int        `json:"y"`
	Truth    [3]float64 `json:"truthLinearRGB"`
	Actual   [3]float64 `json:"actualLinearRGB"`
	MaxError float64    `json:"maxError"`
	Pass     bool       `json:"pass"`
}

type ResultView struct {
	SceneID          string               `json:"sceneId"`
	Width            int                  `json:"width"`
	Height           int                  `json:"height"`
	Path             string               `json:"path"`
	Complete         bool                 `json:"complete"`
	Progress         Progress             `json:"progress"`
	Scale            ScaleDecision        `json:"scale"`
	PeakRadiance     float64              `json:"peakRadiance"`
	TruthPeak        float64              `json:"truthPeakRadiance"`
	PeakRelError     float64              `json:"peakRelativeError"`
	PeakPass         bool                 `json:"peakPass"`
	MeanRadiance     float64              `json:"meanRadiance"`
	OutOfGamut       int                  `json:"outOfGamut"`
	OutOfGamutHash   string               `json:"outOfGamutHash"`
	OutOfGamutSample []PixelCoord         `json:"outOfGamutSamples"`
	GamutBoundaryXY  []XY                 `json:"gamutBoundaryXY"`
	Patches          []PatchCheck         `json:"patches"`
	Checkpoint       CheckpointReport     `json:"checkpoint"`
	Segments         []Segment            `json:"segments"`
	ParamVersion     string               `json:"paramVersion"`
	TruthTolerance   Tolerance            `json:"truthTolerance"`
	ThreePaths       *ThreePathComparison `json:"threePathComparison,omitempty"`
}

type apiEnvelope struct {
	OK     bool            `json:"ok"`
	Data   any             `json:"data,omitempty"`
	Error  *WorkbenchError `json:"error,omitempty"`
	Report any             `json:"report,omitempty"`
}

func NewServer(dataDir string) (*Server, error) {
	s, err := newServerBase(dataDir)
	if err != nil {
		return nil, err
	}
	scene, err := GenerateScene(DefaultGenerateConfig())
	if err != nil {
		return nil, err
	}
	if err := s.AddScene(scene); err != nil {
		return nil, err
	}
	return s, nil
}

func NewServerWithScenes(dataDir string, scenes ...*Scene) (*Server, error) {
	s, err := newServerBase(dataDir)
	if err != nil {
		return nil, err
	}
	for _, scene := range scenes {
		if err := s.AddScene(scene); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func newServerBase(dataDir string) (*Server, error) {
	if dataDir == "" {
		dataDir = "data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, wbErr(CodeIO, "create data directory: %v", err)
	}
	store, err := NewCheckpointStore(filepath.Join(dataDir, "checkpoints"))
	if err != nil {
		return nil, err
	}
	return &Server{
		sessions: make(map[string]*SceneSession),
		store:    store,
		dataDir:  dataDir,
	}, nil
}

func (s *Server) AddScene(scene *Scene) error {
	if scene == nil {
		return fieldErr(CodeMeta, "scene", nil, "non-nil scene", "scene is missing")
	}
	if err := ValidateScene(scene); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.sessions[scene.ID]; ok {
		old.Engine.Close()
	}
	engine, err := NewEngine(scene, DefaultProcessConfig(), s.store)
	if err != nil {
		return err
	}
	s.sessions[scene.ID] = &SceneSession{
		Scene:     scene,
		Engine:    engine,
		CreatedAt: time.Now().UTC(),
		cache:     make(map[string][]byte),
	}
	return nil
}

func (s *Server) Close() {
	s.mu.Lock()
	engines := make([]*Engine, 0, len(s.sessions))
	for _, session := range s.sessions {
		engines = append(engines, session.Engine)
	}
	s.mu.Unlock()
	for _, engine := range engines {
		engine.Close()
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/presets", s.handlePresets)
	mux.HandleFunc("GET /api/scenes", s.handleListScenes)
	mux.HandleFunc("POST /api/scenes", s.handleCreateScene)
	mux.HandleFunc("GET /api/scenes/{id}", s.handleScene)
	mux.HandleFunc("GET /api/scenes/{id}/status", s.handleStatus)
	mux.HandleFunc("GET /api/scenes/{id}/result", s.handleResult)
	mux.HandleFunc("POST /api/scenes/{id}/start", s.handleStart)
	mux.HandleFunc("POST /api/scenes/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/scenes/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/scenes/{id}/step", s.handleStep)
	mux.HandleFunc("POST /api/scenes/{id}/retry", s.handleRetry)
	mux.HandleFunc("POST /api/scenes/{id}/params", s.handleParams)
	mux.HandleFunc("POST /api/scenes/{id}/fault", s.handleFault)
	mux.HandleFunc("POST /api/scenes/{id}/compare", s.handleCompare)
	mux.HandleFunc("GET /api/scenes/{id}/histogram", s.handleHistogram)
	mux.HandleFunc("GET /api/scenes/{id}/chromaticity", s.handleChromaticity)
	mux.HandleFunc("GET /api/scenes/{id}/image/{kind}", s.handleImage)
	mux.HandleFunc("GET /api/scenes/{id}/export", s.handleExport)
	webRoot, err := fs.Sub(webAssets, "web")
	if err == nil {
		mux.Handle("GET /", http.FileServer(http.FS(webRoot)))
	}
	return mux
}

func (s *Server) session(id string) (*SceneSession, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	session, ok := s.sessions[id]
	return session, ok
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeData(w, http.StatusOK, map[string]any{
		"status":       "ok",
		"goVersion":    runtime.Version(),
		"goos":         runtime.GOOS,
		"goarch":       runtime.GOARCH,
		"dependencies": "standard-library-only",
		"algorithm":    AlgorithmVersion,
		"time":         time.Now().UTC().Format(time.RFC3339),
	})
}

type presetDefinition struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Config      GenerateConfig `json:"config"`
}

func presets() []presetDefinition {
	demo := DefaultGenerateConfig()
	multi := demo
	multi.Seed = 20261125
	multi.Width = 144
	multi.Height = 108
	multi.Bands = 6
	multi.Exposures = 4
	multi.BitDepth = 16
	multi.BandCenters = []float64{420, 480, 530, 580, 630, 680}
	multi.ExposureTimes = []float64{1.0 / 1000, 1.0 / 250, 1.0 / 60, 1.0 / 12}
	scale := demo
	scale.Seed = 20261126
	scale.Width = 1024
	scale.Height = 720
	scale.Bands = 8
	scale.Exposures = 4
	scale.BitDepth = 16
	scale.BandCenters = []float64{410, 455, 495, 535, 575, 615, 655, 695}
	scale.ExposureTimes = []float64{1.0 / 1500, 1.0 / 250, 1.0 / 60, 1.0 / 12}
	return []presetDefinition{
		{Name: "demo", Title: "Demo 3B", Description: "96x72, three bands and three exposures. Fast deterministic calibration scene.", Config: demo},
		{Name: "multi6", Title: "Spectral 6B", Description: "144x108, six bands and four exposures for richer spectral reconstruction.", Config: multi},
		{Name: "scale8", Title: "Scale Lab 8B", Description: "1024x720, eight bands. Auto mode selects the tiled/downsample strategy.", Config: scale},
	}
}

func (s *Server) handlePresets(w http.ResponseWriter, r *http.Request) {
	writeData(w, http.StatusOK, presets())
}

func (s *Server) handleListScenes(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.sessions))
	for id := range s.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type item struct {
		Scene  any          `json:"scene"`
		Status EngineStatus `json:"status"`
	}
	out := make([]item, 0, len(ids))
	for _, id := range ids {
		session := s.sessions[id]
		out = append(out, item{Scene: sceneSummary(session.Scene), Status: session.Engine.Status()})
	}
	s.mu.RUnlock()
	writeData(w, http.StatusOK, out)
}

func (s *Server) handleCreateScene(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Preset string `json:"preset"`
		GenerateConfig
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&req); err != nil && err != io.EOF {
		writeAPIError(w, fieldErr(CodePara, "body", err.Error(), "JSON", "request body is invalid"))
		return
	}
	cfg := req.GenerateConfig
	if req.Preset != "" {
		found := false
		for _, p := range presets() {
			if p.Name == req.Preset {
				cfg = p.Config
				found = true
				break
			}
		}
		if !found {
			writeAPIError(w, fieldErr(CodePara, "preset", req.Preset, "known preset", "preset is unknown"))
			return
		}
	}
	scene, err := GenerateScene(cfg)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if err := s.AddScene(scene); err != nil {
		writeAPIError(w, err)
		return
	}
	session, _ := s.session(scene.ID)
	writeData(w, http.StatusCreated, map[string]any{
		"scene":  sceneSummary(scene),
		"status": session.Engine.Status(),
	})
}

func (s *Server) handleScene(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"scene":  sceneSummary(session.Scene),
		"status": session.Engine.Status(),
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	session.cacheMu.Lock()
	last := session.lastCmp
	session.cacheMu.Unlock()
	writeData(w, http.StatusOK, map[string]any{
		"scene":   sceneSummary(session.Scene),
		"engine":  session.Engine.Status(),
		"compare": last,
	})
}

func sceneSummary(scene *Scene) map[string]any {
	bands := make([]float64, len(scene.Meta.Bands))
	for i, band := range scene.Meta.Bands {
		bands[i] = band.CenterWavelength
	}
	return map[string]any{
		"id":              scene.ID,
		"seed":            scene.Seed,
		"metadata":        scene.Meta,
		"truth":           scene.Truth,
		"pixelCount":      scene.Meta.Width * scene.Meta.Height,
		"bandWavelengths": bands,
		"exposureTimes":   scene.Meta.ExposureTimes,
		"detailPixels":    len(scene.Detail),
	}
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	if err := session.Engine.Start(); err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, session.Engine.Status())
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	if err := session.Engine.Pause(); err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, session.Engine.Status())
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	if err := session.Engine.Resume(); err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, session.Engine.Status())
}

func (s *Server) handleStep(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	if err := session.Engine.Step(); err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, session.Engine.Status())
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	var req struct {
		Index      int  `json:"index"`
		ClearFault bool `json:"clearFault"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.ClearFault {
		session.Engine.ClearFault(req.Index)
	}
	if err := session.Engine.RetrySegment(req.Index); err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, session.Engine.Status())
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	var cfg ProcessConfig
	if err := decodeJSON(r, &cfg); err != nil {
		writeAPIError(w, err)
		return
	}
	version, err := session.Engine.UpdateParams(cfg)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, map[string]any{
		"paramVersion": version,
		"status":       session.Engine.Status(),
	})
}

func (s *Server) handleFault(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	var req struct {
		Index   int    `json:"index"`
		Message string `json:"message"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeAPIError(w, err)
		return
	}
	if req.Message == "" {
		req.Message = "manual diagnostic fault"
	}
	if err := session.Engine.InjectFault(req.Index, req.Message); err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, session.Engine.Status())
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	status := session.Engine.Status()
	if !status.ResultReady {
		e := fieldErr(CodeState, "progress", status.Progress, "all segments committed", "result is not complete")
		writeAPIError(w, e)
		return
	}
	result, err := session.Engine.Result()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	view := buildResultView(session, result, status)
	writeData(w, http.StatusOK, view)
}

func buildResultView(session *SceneSession, result *RenderResult, status EngineStatus) ResultView {
	view := ResultView{
		SceneID:          result.SceneID,
		Width:            result.Width,
		Height:           result.Height,
		Path:             result.Path,
		Complete:         status.ResultReady,
		Progress:         status.Progress,
		Scale:            result.Scale,
		PeakRadiance:     result.PeakRadiance,
		TruthPeak:        session.Scene.Truth.PeakRadiance,
		MeanRadiance:     result.MeanRadiance,
		OutOfGamut:       result.OutOfGamut,
		OutOfGamutHash:   result.OutOfGamutHash,
		OutOfGamutSample: result.OutOfGamutSample,
		GamutBoundaryXY:  result.GamutBoundaryXY,
		Checkpoint:       status.Checkpoint,
		Segments:         status.Segments,
		ParamVersion:     status.ParamVersion,
		TruthTolerance:   session.Scene.Truth.Tolerance,
	}
	if session.Scene.Truth.PeakRadiance > 0 {
		view.PeakRelError = math.Abs(result.PeakRadiance-session.Scene.Truth.PeakRadiance) / session.Scene.Truth.PeakRadiance
	}
	view.PeakPass = view.PeakRelError <= session.Scene.Truth.Tolerance.PeakRadianceRel
	for _, patch := range session.Scene.Truth.Patches {
		x := patch.X + patch.Width/2
		y := patch.Y + patch.Height/2
		idx := (y*result.Width + x) * 3
		var actual [3]float64
		if idx >= 0 && idx+2 < len(result.LinearRGB) {
			actual = [3]float64{result.LinearRGB[idx], result.LinearRGB[idx+1], result.LinearRGB[idx+2]}
		}
		maxErr := 0.0
		for c := 0; c < 3; c++ {
			maxErr = math.Max(maxErr, math.Abs(actual[c]-patch.LinearRGB[c]))
		}
		view.Patches = append(view.Patches, PatchCheck{
			ID: patch.ID, X: x, Y: y, Truth: patch.LinearRGB, Actual: actual,
			MaxError: maxErr, Pass: maxErr <= session.Scene.Truth.Tolerance.PatchRGB,
		})
	}
	return view
}

type HistogramData struct {
	Kind     string           `json:"kind"`
	Bins     int              `json:"bins"`
	Min      float64          `json:"min"`
	Max      float64          `json:"max"`
	Counts   []int            `json:"counts,omitempty"`
	Channels map[string][]int `json:"channels,omitempty"`
}

func (s *Server) handleHistogram(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	result, err := session.Engine.Result()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind == "" {
		kind = "fused"
	}
	bins, _ := strconv.Atoi(r.URL.Query().Get("bins"))
	if bins < 8 || bins > 512 {
		bins = 64
	}
	data, err := buildHistogram(session.Scene, result, kind, bins)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeData(w, http.StatusOK, data)
}

func buildHistogram(scene *Scene, result *RenderResult, kind string, bins int) (HistogramData, error) {
	data := HistogramData{Kind: kind, Bins: bins}
	switch kind {
	case "fused":
		maxV := result.PeakRadiance
		if maxV <= 0 {
			maxV = 1
		}
		data.Min, data.Max = 0, maxV
		data.Counts = make([]int, bins)
		for _, v := range result.FusedRadiance {
			idx := int(clamp(v/data.Max, 0, 0.999999) * float64(bins))
			data.Counts[idx]++
		}
	case "rgb":
		data.Min, data.Max = 0, 1
		data.Channels = map[string][]int{"r": make([]int, bins), "g": make([]int, bins), "b": make([]int, bins)}
		keys := []string{"r", "g", "b"}
		for i := 0; i < len(result.LinearRGB); i += 3 {
			for c := 0; c < 3; c++ {
				idx := int(clamp(result.LinearRGB[i+c], 0, 0.999999) * float64(bins))
				data.Channels[keys[c]][idx]++
			}
		}
	case "detail":
		maxV := 0.0
		for _, v := range scene.Detail {
			maxV = math.Max(maxV, math.Abs(v))
		}
		if maxV <= 0 {
			maxV = 1
		}
		data.Min, data.Max = -maxV, maxV
		data.Counts = make([]int, bins)
		for _, v := range scene.Detail {
			idx := int(clamp((v+maxV)/(2*maxV), 0, 0.999999) * float64(bins))
			data.Counts[idx]++
		}
	default:
		return data, fieldErr(CodePara, "kind", kind, []string{"fused", "rgb", "detail"}, "histogram kind is unsupported")
	}
	return data, nil
}

type ChromaPoint struct {
	X   float64 `json:"x"`
	Y   float64 `json:"y"`
	OOG bool    `json:"oog"`
}

type ChromaData struct {
	Points        []ChromaPoint `json:"points"`
	BoundaryXY    []XY          `json:"boundaryXY"`
	OutOfGamut    int           `json:"outOfGamut"`
	SourceWhiteXY XY            `json:"sourceWhiteXY"`
	TargetWhiteXY XY            `json:"targetWhiteXY"`
}

func (s *Server) handleChromaticity(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	result, err := session.Engine.Result()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	toXYZ, _, ok := gamutMatrices(session.Scene.Meta.TargetGamut)
	if !ok {
		writeAPIError(w, fieldErr(CodePara, "targetGamut", session.Scene.Meta.TargetGamut, "supported", "target gamut is unsupported"))
		return
	}
	pixelCount := result.Width * result.Height
	stride := maxInt(1, int(math.Sqrt(float64(pixelCount)/1800)))
	data := ChromaData{
		BoundaryXY:    gamutBoundary(session.Scene.Meta.TargetGamut),
		OutOfGamut:    result.OutOfGamut,
		SourceWhiteXY: xyzToXY([3]float64{session.Scene.Meta.SourceWhitePoint.X, session.Scene.Meta.SourceWhitePoint.Y, session.Scene.Meta.SourceWhitePoint.Z}),
		TargetWhiteXY: xyzToXY([3]float64{session.Scene.Meta.WhitePoint.X, session.Scene.Meta.WhitePoint.Y, session.Scene.Meta.WhitePoint.Z}),
	}
	for i := 0; i < pixelCount; i += stride {
		idx := i * 3
		rgb := [3]float64{result.LinearRGB[idx], result.LinearRGB[idx+1], result.LinearRGB[idx+2]}
		xyz := mulMatVec(toXYZ, rgb)
		xy := xyzToXY(xyz)
		if !finite(xy.X) || !finite(xy.Y) || xy.X < -0.25 || xy.X > 1.25 || xy.Y < -0.25 || xy.Y > 1.25 {
			continue
		}
		data.Points = append(data.Points, ChromaPoint{X: xy.X, Y: xy.Y, OOG: result.GamutMask[i] != 0})
	}
	writeData(w, http.StatusOK, data)
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	kind := r.PathValue("kind")
	engineStatus := session.Engine.Status()
	key := "image:" + engineStatus.ContentVersion + ":" + engineStatus.ParamVersion + ":" + kind
	if q := r.URL.RawQuery; q != "" {
		key += "?" + q
	}
	session.cacheMu.Lock()
	if data, ok := session.cache[key]; ok {
		session.cacheMu.Unlock()
		writePNG(w, data, kind)
		return
	}
	session.cacheMu.Unlock()
	data, err := s.renderImage(session, kind, r)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	session.cacheMu.Lock()
	if len(session.cache) > 64 {
		session.cache = make(map[string][]byte)
	}
	session.cache[key] = data
	session.cacheMu.Unlock()
	writePNG(w, data, kind)
}

func writePNG(w http.ResponseWriter, data []byte, kind string) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", kind+".png"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (s *Server) renderImage(session *SceneSession, kind string, r *http.Request) ([]byte, error) {
	scene := session.Scene
	if strings.HasPrefix(kind, "band") {
		b, err := strconv.Atoi(strings.TrimPrefix(kind, "band"))
		if err != nil || b < 0 || b >= len(scene.Bands) {
			return nil, fieldErr(CodePara, "kind", kind, "band0..bandN", "band image index is invalid")
		}
		img := image.NewRGBA(image.Rect(0, 0, scene.Meta.Width, scene.Meta.Height))
		maxQ := float64(maxCode(scene.Meta.BitDepth))
		for y := 0; y < scene.Meta.Height; y++ {
			for x := 0; x < scene.Meta.Width; x++ {
				v := math.Pow(float64(scene.Bands[b].Data[y*scene.Meta.Width+x])/maxQ, 1/2.2)
				g := uint8(math.Round(clamp(v, 0, 1) * 255))
				img.SetRGBA(x, y, color.RGBA{R: g, G: g, B: g, A: 255})
			}
		}
		return encodePNG(img)
	}
	if kind == "detail" {
		img := image.NewRGBA(image.Rect(0, 0, scene.Meta.Width, scene.Meta.Height))
		maxAbs := 0.0
		for _, v := range scene.Detail {
			maxAbs = math.Max(maxAbs, math.Abs(v))
		}
		if maxAbs == 0 {
			maxAbs = 1
		}
		for y := 0; y < scene.Meta.Height; y++ {
			for x := 0; x < scene.Meta.Width; x++ {
				v := scene.Detail[y*scene.Meta.Width+x] / maxAbs
				img.SetRGBA(x, y, signedColor(v))
			}
		}
		return encodePNG(img)
	}
	result, err := session.Engine.Result()
	if err != nil {
		return nil, err
	}
	switch kind {
	case "fused":
		img := image.NewRGBA(image.Rect(0, 0, result.Width, result.Height))
		maxV := math.Log1p(result.PeakRadiance * 8)
		if maxV <= 0 {
			maxV = 1
		}
		for i, v := range result.FusedRadiance {
			t := clamp(math.Log1p(v*8)/maxV, 0, 1)
			img.SetRGBA(i%result.Width, i/result.Width, heatColor(t))
		}
		return encodePNG(img)
	case "rgb", "display":
		img := image.NewRGBA(image.Rect(0, 0, result.Width, result.Height))
		for i := 0; i < result.Width*result.Height; i++ {
			img.SetRGBA(i%result.Width, i/result.Width, color.RGBA{
				R: result.DisplayRGB[i*3],
				G: result.DisplayRGB[i*3+1],
				B: result.DisplayRGB[i*3+2],
				A: 255,
			})
		}
		return encodePNG(img)
	case "gamut":
		img := image.NewRGBA(image.Rect(0, 0, result.Width, result.Height))
		for i := 0; i < result.Width*result.Height; i++ {
			if result.GamutMask[i] != 0 {
				img.SetRGBA(i%result.Width, i/result.Width, color.RGBA{R: 255, G: 54, B: 112, A: 255})
			} else {
				img.SetRGBA(i%result.Width, i/result.Width, color.RGBA{R: 7, G: 12, B: 18, A: 255})
			}
		}
		return encodePNG(img)
	case "diff":
		against := r.URL.Query().Get("against")
		if against != PathTiled {
			against = PathFull
		}
		alt, err := RenderScene(scene, session.Engine.Status().Config, against)
		if err != nil {
			return nil, err
		}
		img := image.NewRGBA(image.Rect(0, 0, result.Width, result.Height))
		maxDiff := 0.0
		diffs := make([]float64, result.Width*result.Height)
		for i := 0; i < len(diffs); i++ {
			d := math.Abs(result.FusedRadiance[i] - alt.FusedRadiance[i])
			for c := 0; c < 3; c++ {
				d = math.Max(d, math.Abs(result.LinearRGB[i*3+c]-alt.LinearRGB[i*3+c]))
			}
			diffs[i] = d
			maxDiff = math.Max(maxDiff, d)
		}
		scaleVal := maxDiff
		if scaleVal < 1e-12 {
			scaleVal = 1
		}
		for i, d := range diffs {
			img.SetRGBA(i%result.Width, i/result.Width, heatColor(clamp(d/scaleVal, 0, 1)))
		}
		return encodePNG(img)
	default:
		return nil, fieldErr(CodePara, "kind", kind, []string{"band0", "fused", "rgb", "gamut", "detail", "diff"}, "image kind is unsupported")
	}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, img); err != nil {
		return nil, wbErr(CodeIO, "encode PNG: %v", err)
	}
	return buf.Bytes(), nil
}

func signedColor(v float64) color.RGBA {
	v = clamp(v, -1, 1)
	if v >= 0 {
		return color.RGBA{R: uint8(255 * v), G: uint8(38 * (1 - v)), B: uint8(70 * (1 - v)), A: 255}
	}
	return color.RGBA{R: uint8(40 * (1 + v)), G: uint8(90 * (1 + v)), B: uint8(255 * -v), A: 255}
}

func heatColor(t float64) color.RGBA {
	t = clamp(t, 0, 1)
	stops := [][4]float64{
		{0.0, 3, 6, 18},
		{0.20, 28, 24, 82},
		{0.42, 102, 28, 116},
		{0.65, 196, 54, 74},
		{0.84, 245, 132, 32},
		{1.0, 252, 245, 188},
	}
	for i := 1; i < len(stops); i++ {
		if t <= stops[i][0] {
			a, b := stops[i-1], stops[i]
			f := (t - a[0]) / (b[0] - a[0])
			return color.RGBA{
				R: uint8(a[1] + (b[1]-a[1])*f),
				G: uint8(a[2] + (b[2]-a[2])*f),
				B: uint8(a[3] + (b[3]-a[3])*f),
				A: 255,
			}
		}
	}
	return color.RGBA{R: 252, G: 245, B: 188, A: 255}
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	if !session.Engine.Status().ResultReady {
		writeAPIError(w, fieldErr(CodeState, "progress", session.Engine.Progress(), "complete", "three-path comparison requires a complete live result"))
		return
	}
	report, err := s.compareThreePaths(session)
	session.cacheMu.Lock()
	session.lastCmp = report
	session.cacheMu.Unlock()
	if err != nil {
		writeAPIErrorReport(w, err, report)
		return
	}
	writeData(w, http.StatusOK, report)
}

func (s *Server) compareThreePaths(session *SceneSession) (*ThreePathComparison, error) {
	cfg := session.Engine.Status().Config
	live, err := session.Engine.Result()
	if err != nil {
		return nil, err
	}
	live.Path = "live"
	full, err := RenderScene(session.Scene, cfg, PathFull)
	if err != nil {
		return nil, err
	}
	tiled, err := RenderScene(session.Scene, cfg, PathTiled)
	if err != nil {
		return nil, err
	}
	resumeEngine, err := NewEngine(session.Scene, cfg, s.store)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !resumeEngine.Status().ResultReady {
		if err := resumeEngine.Run(ctx); err != nil {
			resumeEngine.Close()
			return nil, err
		}
	}
	resumeResult, err := resumeEngine.Result()
	resumeEngine.Close()
	if err != nil {
		return nil, err
	}
	resumeResult.Path = "resume"
	report := &ThreePathComparison{
		Consistent:      true,
		LivePeak:        live.PeakRadiance,
		ResumePeak:      resumeResult.PeakRadiance,
		FullPeak:        full.PeakRadiance,
		TiledPeak:       tiled.PeakRadiance,
		LiveGamutHash:   live.OutOfGamutHash,
		ResumeGamutHash: resumeResult.OutOfGamutHash,
		FullGamutHash:   full.OutOfGamutHash,
		TiledGamutHash:  tiled.OutOfGamutHash,
		CheckedAt:       time.Now().UTC().Format(time.RFC3339Nano),
	}
	pairs := []struct {
		a, b *RenderResult
	}{
		{live, full},
		{resumeResult, full},
		{live, resumeResult},
		{full, tiled},
	}
	var firstErr error
	for _, pair := range pairs {
		rep, cmpErr := CompareRenderResults(pair.a, pair.b, tiled.Scale.TileWidth, tiled.Scale.TileHeight, session.Scene.Truth.Tolerance)
		report.Paths = append(report.Paths, *rep)
		if cmpErr != nil {
			report.Consistent = false
			if report.FirstMismatch == nil && rep.FirstMismatch != nil {
				report.FirstMismatch = rep.FirstMismatch
				report.FirstMismatchBlock = rep.FirstMismatchBlock
			}
			if firstErr == nil {
				firstErr = cmpErr
			}
		}
	}
	return report, firstErr
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r.PathValue("id"))
	if !ok {
		writeAPIError(w, wbErr(CodeState, "scene not found"))
		return
	}
	status := session.Engine.Status()
	if !status.ResultReady {
		writeAPIError(w, fieldErr(CodeState, "progress", status.Progress, "complete", "export requires a complete result"))
		return
	}
	result, err := session.Engine.Result()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	view := buildResultView(session, result, status)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", session.Scene.ID+"-export.zip"))
	zw := zip.NewWriter(w)
	addJSON := func(name string, v any) error {
		entry, err := zw.Create(name)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(entry)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	addBytes := func(name string, data []byte) error {
		entry, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = entry.Write(data)
		return err
	}
	exportErr := func() error {
		if err := addJSON("manifest.json", map[string]any{
			"scene":      sceneSummary(session.Scene),
			"result":     view,
			"status":     status,
			"exportedAt": time.Now().UTC().Format(time.RFC3339Nano),
			"algorithm":  AlgorithmVersion,
		}); err != nil {
			return err
		}
		if err := addJSON("truth.json", session.Scene.Truth); err != nil {
			return err
		}
		if err := addJSON("segments.json", status.Segments); err != nil {
			return err
		}
		hist, err := buildHistogram(session.Scene, result, "fused", 64)
		if err != nil {
			return err
		}
		if err := addJSON("histogram-fused.json", hist); err != nil {
			return err
		}
		for _, kind := range []string{"fused", "rgb", "gamut"} {
			data, err := s.renderImage(session, kind, r)
			if err != nil {
				return err
			}
			if err := addBytes(kind+".png", data); err != nil {
				return err
			}
		}
		for b := range session.Scene.Bands {
			data, err := s.renderImage(session, fmt.Sprintf("band%d", b), r)
			if err != nil {
				return err
			}
			if err := addBytes(fmt.Sprintf("band-%02d.png", b), data); err != nil {
				return err
			}
		}
		if err := addBytes("README.txt", []byte(exportReadme(session.Scene))); err != nil {
			return err
		}
		return nil
	}()
	if exportErr != nil {
		_ = zw.Close()
		return
	}
	_ = zw.Close()
}

func exportReadme(scene *Scene) string {
	return fmt.Sprintf(`Spectral / HDR Workbench export
Scene: %s
Seed: %d
Pixels: %dx%d
Bands: %d
Exposures: %d
Target gamut: %s
Algorithm: %s

Files:
- fused.png: log-radiance heat map
- rgb.png: tone-mapped target linear RGB display encoding
- gamut.png: observable out-of-gamut mask
- band-NN.png: source band previews
- truth.json: generator-only ground truth
- segments.json: segment freeze metadata and hashes
- manifest.json: full status and result metrics
`, scene.ID, scene.Seed, scene.Meta.Width, scene.Meta.Height, scene.Meta.BandCount, scene.Meta.ExposureCount, scene.Meta.TargetGamut, AlgorithmVersion)
}

func decodeJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := decoder.Decode(dst); err != nil {
		return fieldErr(CodePara, "body", err.Error(), "valid JSON", "request body is invalid")
	}
	return nil
}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{OK: true, Data: data})
}

func writeAPIError(w http.ResponseWriter, err error) {
	we := AsWorkbenchError(err)
	status := http.StatusInternalServerError
	switch we.Code {
	case CodeMeta, CodeBand, CodeOrder, CodeReg, CodePara, CodeGamut, CodeScale:
		status = http.StatusUnprocessableEntity
	case CodeState:
		status = http.StatusConflict
	case CodeIO, CodeInternal:
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{OK: false, Error: we})
}

func writeAPIErrorReport(w http.ResponseWriter, err error, report any) {
	we := AsWorkbenchError(err)
	status := http.StatusUnprocessableEntity
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apiEnvelope{OK: false, Error: we, Report: report})
}
