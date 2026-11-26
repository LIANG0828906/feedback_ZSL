package workbench

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"volumekernel/internal/checkpoint"
	"volumekernel/internal/generator"
	"volumekernel/internal/imageout"
	"volumekernel/internal/model"
	"volumekernel/internal/pipeline"
	"volumekernel/internal/scene"
)

//go:embed index.html
var webFS embed.FS

type Options struct {
	Addr        string
	Seed        int64
	CameraIndex int
	DBPath      string
}

type Server struct {
	mu               sync.Mutex
	seed             int64
	cameraIndex      int
	store            *checkpoint.Store
	scene            model.Scene
	snapshot         model.SceneSnapshot
	graph            pipeline.DependencyGraph
	preview          model.Frame
	exact            model.Frame
	previewMeta      metaView
	exactMeta        metaView
	compare          pipeline.CompareReport
	compareErr       *errorView
	lastError        *errorView
	invalidation     *pipeline.InvalidationReport
	injections       []errorView
	cacheHits        int
	cacheMisses      int
	exactCacheHits   int
	exactCacheMisses int
}

type metaView struct {
	Mode               string  `json:"mode"`
	ElapsedMS          float64 `json:"elapsed_ms"`
	ActualSteps        int     `json:"actual_steps"`
	ActualShadows      int     `json:"actual_shadows"`
	StepBudget         int     `json:"step_budget"`
	ShadowBudget       int     `json:"shadow_budget"`
	PreviewDeadlineMet bool    `json:"preview_deadline_met"`
}

type mediaView struct {
	ID              string     `json:"id"`
	Bounds          model.AABB `json:"bounds"`
	Absorption      float64    `json:"absorption"`
	Scattering      float64    `json:"scattering"`
	Anisotropy      float64    `json:"anisotropy"`
	MaterialVersion string     `json:"material_version"`
	Resolution      [3]int     `json:"resolution"`
}

type blockView struct {
	X                int     `json:"x"`
	Y                int     `json:"y"`
	Pixels           int     `json:"pixels"`
	TransmittanceAvg float64 `json:"transmittance_avg"`
	ScatterAvg       float64 `json:"scatter_avg"`
	Steps            int     `json:"steps"`
	ShadowSamples    int     `json:"shadow_samples"`
	CacheKey         string  `json:"cache_key"`
	ContentHash      string  `json:"content_hash"`
}

type frameView struct {
	Mode          string             `json:"mode"`
	ElapsedMS     float64            `json:"elapsed_ms"`
	StepBudget    int                `json:"step_budget"`
	ShadowBudget  int                `json:"shadow_budget"`
	ActualSteps   int                `json:"actual_steps"`
	ActualShadows int                `json:"actual_shadows"`
	DeadlineMet   bool               `json:"deadline_met"`
	Blocks        []blockView        `json:"blocks"`
	Queue         []model.QueueEvent `json:"queue"`
}

type errorView struct {
	Code         model.Code         `json:"code"`
	Message      string             `json:"message"`
	RayID        string             `json:"ray_id,omitempty"`
	MediumID     string             `json:"medium_id,omitempty"`
	CameraID     string             `json:"camera_id,omitempty"`
	Field        string             `json:"field,omitempty"`
	BlockX       int                `json:"block_x"`
	BlockY       int                `json:"block_y"`
	FirstPixelX  int                `json:"first_pixel_x"`
	FirstPixelY  int                `json:"first_pixel_y"`
	Measurements map[string]float64 `json:"measurements,omitempty"`
}

type stateView struct {
	Seed             int64                        `json:"seed"`
	SceneID          string                       `json:"scene_id"`
	SceneVersion     string                       `json:"scene_version"`
	SnapshotVersion  string                       `json:"snapshot_version"`
	CameraVersion    string                       `json:"camera_version"`
	RecipeVersion    string                       `json:"recipe_version"`
	AlgorithmVersion string                       `json:"algorithm_version"`
	Viewport         model.Viewport               `json:"viewport"`
	Recipe           model.Recipe                 `json:"recipe"`
	Light            model.DirectionalLight       `json:"light"`
	Camera           model.CameraSample           `json:"camera"`
	Media            []mediaView                  `json:"media"`
	Preview          frameView                    `json:"preview"`
	Exact            frameView                    `json:"exact"`
	Compare          pipeline.CompareReport       `json:"compare"`
	CompareError     *errorView                   `json:"compare_error,omitempty"`
	Invalidation     *pipeline.InvalidationReport `json:"invalidation,omitempty"`
	Injections       []errorView                  `json:"injections"`
	CacheEntries     int                          `json:"cache_entries"`
	LastError        *errorView                   `json:"last_error,omitempty"`
}

func New(opts Options) (*Server, error) {
	if opts.Seed == 0 {
		opts.Seed = generator.DefaultSeed
	}
	if opts.CameraIndex < 0 {
		opts.CameraIndex = 2
	}
	if opts.DBPath == "" {
		opts.DBPath = filepath.Join(".volume-lab", "workbench.db")
	}
	if err := os.MkdirAll(filepath.Dir(opts.DBPath), 0o755); err != nil {
		return nil, err
	}
	store, err := checkpoint.Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	s := generator.Generate(opts.Seed)
	if err := scene.Validate(s); err != nil {
		store.Close()
		return nil, err
	}
	snap, err := model.NewSnapshot(s)
	if err != nil {
		store.Close()
		return nil, err
	}
	if opts.CameraIndex < 0 || opts.CameraIndex >= len(s.CameraTrack.Samples) {
		store.Close()
		return nil, model.ParamError("", "camera_index", "camera index %d out of range", opts.CameraIndex)
	}
	srv := &Server{
		seed:        opts.Seed,
		cameraIndex: opts.CameraIndex,
		store:       store,
		scene:       s,
		snapshot:    snap,
	}
	if srv.graph, err = pipeline.BuildDependencyGraph(snap, opts.CameraIndex); err != nil {
		store.Close()
		return nil, err
	}
	if err := srv.renderLocked("preview", 0, 0, true); err != nil {
		store.Close()
		return nil, err
	}
	if err := srv.renderLocked("exact", 0, 0, true); err != nil {
		store.Close()
		return nil, err
	}
	return srv, nil
}

func (s *Server) Close() error { return s.store.Close() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/render", s.handleRender)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/inject", s.handleInject)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/preview.png", s.handlePNG)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "algorithm": model.AlgorithmVersion})
	})
	return mux
}

func (s *Server) Serve(ctx context.Context, addr string) error {
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		err := <-errCh
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.stateLocked()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}

func (s *Server) handleRender(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mode := r.URL.Query().Get("mode")
	if mode != "preview" && mode != "exact" {
		writeError(w, model.ParamError("", "mode", "mode must be preview or exact"))
		return
	}
	steps, err := queryInt(r, "steps", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	shadow, err := queryInt(r, "shadow", 0)
	if err != nil {
		writeError(w, err)
		return
	}
	if steps < 0 || steps > s.scene.Recipe.Steps {
		writeError(w, model.ParamError("", "steps", "steps must be in 0..%d", s.scene.Recipe.Steps))
		return
	}
	if shadow < 0 || shadow > 64 {
		writeError(w, model.ParamError("", "shadow", "shadow samples must be in 0..64"))
		return
	}
	if mode == "exact" {
		steps, shadow = 0, 0
	}
	if err := s.renderLocked(mode, steps, shadow, true); err != nil {
		s.lastError = asErrorView(err)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": s.lastError})
		return
	}
	state, err := s.stateLocked()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": s.compareErr == nil, "error": s.compareErr, "state": state})
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	medium := r.URL.Query().Get("medium")
	if medium == "" {
		medium = "media-beta"
	}
	x, err := queryInt(r, "x", 2)
	if err != nil {
		writeError(w, err)
		return
	}
	y, err := queryInt(r, "y", 2)
	if err != nil {
		writeError(w, err)
		return
	}
	z, err := queryInt(r, "z", 2)
	if err != nil {
		writeError(w, err)
		return
	}
	edit := pipeline.Edit{
		Kind: pipeline.EditVoxel, MediumID: medium, IX: x, IY: y, IZ: z,
		Reason: "workbench voxel brush",
	}
	found := false
	for _, m := range s.scene.Media {
		if m.ID != medium {
			continue
		}
		found = true
		if x < 0 || y < 0 || z < 0 || x >= m.Density.NX || y >= m.Density.NY || z >= m.Density.NZ {
			writeError(w, model.ParamError(medium, "voxel", "voxel (%d,%d,%d) outside resolution %dx%dx%d", x, y, z, m.Density.NX, m.Density.NY, m.Density.NZ))
			return
		}
	}
	if !found {
		writeError(w, model.ParamError(medium, "medium", "unknown medium"))
		return
	}
	inv, err := pipeline.InvalidateForEdit(s.graph, edit, s.store)
	if err != nil {
		writeError(w, err)
		return
	}
	edited := pipeline.ApplyEdit(s.scene, edit)
	if err := scene.Validate(edited); err != nil {
		writeError(w, err)
		return
	}
	snap, err := model.NewSnapshot(edited)
	if err != nil {
		writeError(w, err)
		return
	}
	graph, err := pipeline.BuildDependencyGraph(snap, s.cameraIndex)
	if err != nil {
		writeError(w, err)
		return
	}
	s.scene = edited
	s.snapshot = snap
	s.graph = graph
	if err := s.renderLocked("preview", 0, 0, true); err != nil {
		writeError(w, err)
		return
	}
	if err := s.renderLocked("exact", 0, 0, true); err != nil {
		writeError(w, err)
		return
	}
	inv.CacheHitsAfterEdit = s.exactCacheHits
	inv.CacheMissesAfterEdit = s.exactCacheMisses
	s.invalidation = &inv
	state, err := s.stateLocked()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "invalidation": inv, "state": state})
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kind := r.URL.Query().Get("kind")
	var err error
	switch kind {
	case "decode":
		raw, mErr := json.Marshal(s.scene)
		if mErr != nil {
			err = mErr
			break
		}
		_, err = scene.LoadJSON(raw[:len(raw)/2])
	case "order":
		_, err = scene.Inject(s.scene, scene.InjectOrder)
	case "dens":
		_, err = scene.Inject(s.scene, scene.InjectDens)
	case "block":
		_, err = (&pipeline.Engine{Store: s.store}).Render(s.snapshot, pipeline.RenderOptions{
			CameraIndex: s.cameraIndex, Mode: "exact", MaxParallel: 2,
			FailBlock: &pipeline.BlockCoord{X: 1, Y: 0},
		})
	default:
		err = model.ParamError("", "kind", "unknown injection kind %q", kind)
	}
	ev := asErrorView(err)
	if ev != nil {
		s.injections = append(s.injections, *ev)
		s.lastError = ev
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": ev})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scene = generator.Generate(s.seed)
	if err := scene.Validate(s.scene); err != nil {
		writeError(w, err)
		return
	}
	snap, err := model.NewSnapshot(s.scene)
	if err != nil {
		writeError(w, err)
		return
	}
	g, err := pipeline.BuildDependencyGraph(snap, s.cameraIndex)
	if err != nil {
		writeError(w, err)
		return
	}
	s.snapshot = snap
	s.graph = g
	s.invalidation = nil
	s.injections = nil
	s.lastError = nil
	if err := s.renderLocked("preview", 0, 0, true); err != nil {
		writeError(w, err)
		return
	}
	if err := s.renderLocked("exact", 0, 0, true); err != nil {
		writeError(w, err)
		return
	}
	state, err := s.stateLocked()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}

func (s *Server) handlePNG(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	frame := s.preview
	if r.URL.Query().Get("mode") == "exact" {
		frame = s.exact
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if err := imageout.EncodeFramePNG(w, frame, 10); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderLocked(mode string, steps, shadow int, useCache bool) error {
	result, err := (&pipeline.Engine{Store: s.store}).Render(s.snapshot, pipeline.RenderOptions{
		CameraIndex:        s.cameraIndex,
		Mode:               mode,
		Steps:              steps,
		ShadowSamples:      shadow,
		MaxParallel:        s.scene.Recipe.MaxParallelBlocks,
		UsePersistentCache: useCache,
	})
	if err != nil {
		return err
	}
	if mode == "preview" {
		s.preview = result.Frame
		s.previewMeta = metaFromFrame(result.Frame, s.scene.Recipe)
		s.cacheHits += result.CacheHits
		s.cacheMisses += result.CacheMisses
	} else {
		s.exact = result.Frame
		s.exactMeta = metaFromFrame(result.Frame, s.scene.Recipe)
		s.cacheHits += result.CacheHits
		s.cacheMisses += result.CacheMisses
		s.exactCacheHits = result.CacheHits
		s.exactCacheMisses = result.CacheMisses
	}
	if len(s.preview.Pixels) > 0 && len(s.exact.Pixels) > 0 {
		report, cerr := pipeline.CompareFrames(s.preview, s.exact, s.scene.Recipe.PixelTolerance)
		s.compare = report
		s.compareErr = asErrorView(cerr)
		if cerr != nil {
			s.lastError = s.compareErr
		}
	}
	return nil
}

func (s *Server) stateLocked() (stateView, error) {
	cacheEntries, err := s.store.CacheCount()
	if err != nil {
		return stateView{}, err
	}
	media := make([]mediaView, 0, len(s.scene.Media))
	for _, m := range s.scene.Media {
		media = append(media, mediaView{
			ID: m.ID, Bounds: m.Bounds, Absorption: m.Absorption, Scattering: m.Scattering,
			Anisotropy: m.Anisotropy, MaterialVersion: m.MaterialVersion,
			Resolution: [3]int{m.Density.NX, m.Density.NY, m.Density.NZ},
		})
	}
	return stateView{
		Seed:             s.seed,
		SceneID:          s.scene.ID,
		SceneVersion:     s.scene.Version,
		SnapshotVersion:  s.snapshot.Version,
		CameraVersion:    s.snapshot.CameraVersion,
		RecipeVersion:    s.snapshot.RecipeVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		Viewport:         s.scene.Viewport,
		Recipe:           s.scene.Recipe,
		Light:            s.scene.Light,
		Camera:           s.scene.CameraTrack.Samples[s.cameraIndex],
		Media:            media,
		Preview:          frameToView(s.preview, s.previewMeta),
		Exact:            frameToView(s.exact, s.exactMeta),
		Compare:          s.compare,
		CompareError:     s.compareErr,
		Invalidation:     s.invalidation,
		Injections:       append([]errorView(nil), s.injections...),
		CacheEntries:     cacheEntries,
		LastError:        s.lastError,
	}, nil
}

func frameToView(f model.Frame, meta metaView) frameView {
	out := frameView{
		Mode: meta.Mode, ElapsedMS: meta.ElapsedMS, StepBudget: meta.StepBudget,
		ShadowBudget: meta.ShadowBudget, ActualSteps: meta.ActualSteps,
		ActualShadows: meta.ActualShadows, DeadlineMet: meta.PreviewDeadlineMet,
		Queue: f.Queue,
	}
	for _, b := range f.Blocks {
		out.Blocks = append(out.Blocks, blockView{
			X: b.X, Y: b.Y, Pixels: len(b.Pixels),
			TransmittanceAvg: b.TransmittanceAvg, ScatterAvg: b.ScatterAvg,
			Steps: b.Steps, ShadowSamples: b.ShadowSamples,
			CacheKey: b.CacheKey, ContentHash: b.ContentHash,
		})
	}
	return out
}

func metaFromFrame(f model.Frame, recipe model.Recipe) metaView {
	return metaView{
		Mode: f.Mode, ElapsedMS: f.ElapsedMS, ActualSteps: f.ActualSteps,
		ActualShadows: f.ActualShadows, StepBudget: f.StepBudget,
		ShadowBudget:       f.ShadowBudget,
		PreviewDeadlineMet: f.Mode == "preview" && f.ElapsedMS <= float64(recipe.PreviewLatencyMS),
	}
}

func asErrorView(err error) *errorView {
	if err == nil {
		return nil
	}
	var ke *model.Error
	if !errors.As(err, &ke) {
		return &errorView{Code: model.EBlock, Message: err.Error(), BlockX: -1, BlockY: -1, FirstPixelX: -1, FirstPixelY: -1}
	}
	return &errorView{
		Code: ke.Code, Message: ke.Error(), RayID: ke.RayID, MediumID: ke.MediumID, Field: ke.Field,
		CameraID: ke.CameraID,
		BlockX:   ke.BlockX, BlockY: ke.BlockY, FirstPixelX: ke.FirstPixelX, FirstPixelY: ke.FirstPixelY,
		Measurements: ke.Measurements,
	}
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": asErrorView(err)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func queryInt(r *http.Request, name string, def int) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, model.ParamError("", name, "value must be an integer")
	}
	return v, nil
}

func WriteReadme(w io.Writer) {
	fmt.Fprintln(w, "local diagnostics are served by cmd/volume-lab serve")
	fmt.Fprintln(w, "open http://127.0.0.1:8787 after starting the command")
}
