package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/compare"
	"qualitybench/internal/core"
	"qualitybench/internal/dataset"
	"qualitybench/internal/model"
	"qualitybench/internal/pipeline"
	"qualitybench/internal/quality"
	"qualitybench/internal/runner"
	"qualitybench/internal/verify"
)

type Options struct {
	Workers       int
	TaskDelay     time.Duration
	ValidatePaths bool
}

type pathView struct {
	Status      string        `json:"status"`
	RunID       string        `json:"run_id,omitempty"`
	Identical   bool          `json:"identical"`
	Differences int           `json:"differences"`
	FirstDiff   *compare.Diff `json:"first_diff,omitempty"`
	Verified    bool          `json:"verified"`
	Note        string        `json:"note,omitempty"`
}

type Service struct {
	ds   *dataset.Dataset
	st   checkpoint.Store
	opts Options

	mu            sync.RWMutex
	status        string
	message       string
	paused        bool
	currentImage  string
	progress      map[string][2]int
	frames        map[string]map[int]*core.Frame
	results       map[string]*pipeline.Result
	engines       map[string]*pipeline.Engine
	currentEngine *pipeline.Engine
	invalidations []pipeline.Invalidation
	cache         pipeline.CacheStats
	bufferWaits   []pipeline.WaitEvent
	queueWaits    []pipeline.WaitEvent
	paths         map[string]pathView
	verification  map[string]bool
	runToken      int
	runCancel     context.CancelFunc
}

func New(ds *dataset.Dataset, st checkpoint.Store, opts Options) *Service {
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	return &Service{
		ds: ds, st: st, opts: opts,
		status:       "idle",
		progress:     make(map[string][2]int),
		frames:       make(map[string]map[int]*core.Frame),
		results:      make(map[string]*pipeline.Result),
		engines:      make(map[string]*pipeline.Engine),
		paths:        map[string]pathView{"live": {Status: "pending"}, "resume": {Status: "pending"}, "full": {Status: "pending"}},
		verification: make(map[string]bool),
	}
}

func (s *Service) Start() {
	s.mu.Lock()
	if s.status == "running" || s.status == "canceling" {
		s.mu.Unlock()
		return
	}
	s.runToken++
	token := s.runToken
	s.status = "running"
	s.message = ""
	s.paused = false
	s.frames = make(map[string]map[int]*core.Frame)
	s.results = make(map[string]*pipeline.Result)
	s.engines = make(map[string]*pipeline.Engine)
	s.progress = make(map[string][2]int)
	s.invalidations = nil
	s.cache = pipeline.CacheStats{}
	s.bufferWaits = nil
	s.queueWaits = nil
	s.paths = map[string]pathView{"live": {Status: "running"}, "resume": {Status: "pending"}, "full": {Status: "pending"}}
	s.verification = make(map[string]bool)
	for _, image := range s.ds.Project.Images {
		s.frames[image.ID] = map[int]*core.Frame{0: s.ds.Baseline[image.ID]}
	}
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.runCancel = cancel
	s.mu.Unlock()
	go s.runMain(ctx, token)
}

func (s *Service) runMain(ctx context.Context, token int) {
	for _, spec := range s.ds.Project.Images {
		if ctx.Err() != nil {
			s.finish(token, "canceled", "run canceled")
			return
		}
		eng, err := runner.NewEngine(s.ds, spec.ID, s.st, runner.Config{
			RunID: "serve-live", Mode: pipeline.ModeLive, Workers: s.opts.Workers,
			MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20,
			PersistentCache: true, TaskDelay: s.opts.TaskDelay,
			OnStepComplete: func(imageID string, step int, frame *core.Frame) {
				s.mu.Lock()
				if s.runToken == token {
					s.frames[imageID][step] = frame.Clone()
					s.currentImage = imageID
				}
				s.mu.Unlock()
			},
			OnProgress: func(imageID string, completed, total int) {
				s.mu.Lock()
				if s.runToken == token {
					s.progress[imageID] = [2]int{completed, total}
					s.currentImage = imageID
				}
				s.mu.Unlock()
			},
		})
		if err != nil {
			s.finish(token, "failed", err.Error())
			return
		}
		s.mu.Lock()
		if s.runToken != token {
			s.mu.Unlock()
			return
		}
		s.engines[spec.ID] = eng
		s.currentEngine = eng
		s.currentImage = spec.ID
		paused := s.paused
		s.mu.Unlock()
		if paused {
			eng.Pause()
		}
		res, err := eng.Run(ctx, pipeline.ModeLive)
		if err != nil {
			if errors.Is(err, context.Canceled) || core.CodeOf(err) == core.CodeCanceled {
				s.finish(token, "canceled", "run canceled")
			} else {
				s.finish(token, "failed", err.Error())
			}
			return
		}
		s.mu.Lock()
		if s.runToken != token {
			s.mu.Unlock()
			return
		}
		s.results[spec.ID] = res
		s.frames[spec.ID] = frameMap(res.StepFrames)
		s.cache.Hits += res.Cache.Hits
		s.cache.Misses += res.Cache.Misses
		s.cache.PersistentHits += res.Cache.PersistentHits
		s.cache.LoadedReused += res.Cache.LoadedReused
		s.bufferWaits = append(s.bufferWaits, res.BufferWaits...)
		s.queueWaits = append(s.queueWaits, res.QueueWaits...)
		s.mu.Unlock()
	}
	if err := s.demoInvalidation(ctx, token); err != nil {
		// The demo is diagnostic-only; the completed main run remains valid.
		s.mu.Lock()
		s.message = "invalidation demo: " + err.Error()
		s.mu.Unlock()
	}
	s.finish(token, "completed", "")
	if s.opts.ValidatePaths {
		go s.validatePaths(token)
	} else {
		s.mu.Lock()
		if s.runToken == token {
			s.paths["live"] = pathView{Status: "disabled", RunID: "serve-live",
				Note: "-validate-paths=false"}
			s.paths["resume"] = pathView{Status: "disabled"}
			s.paths["full"] = pathView{Status: "disabled"}
		}
		s.mu.Unlock()
	}
}

func (s *Service) demoInvalidation(ctx context.Context, token int) error {
	s.mu.RLock()
	eng := s.engines[s.ds.Project.Images[0].ID]
	res := s.results[s.ds.Project.Images[0].ID]
	s.mu.RUnlock()
	if eng == nil || res == nil || len(eng.Chain().Steps) < 2 {
		return nil
	}
	strength := eng.Chain().Steps[1].Strength + 0.08
	if strength > 1 {
		strength = 1
	}
	edit, err := eng.ApplyEdit(ctx, model.Edit{
		Op: "set_strength", ImageID: s.ds.Project.Images[0].ID, Step: 2, Strength: &strength,
	})
	if err != nil {
		return err
	}
	rollback, err := eng.Rollback(ctx, res.VersionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runToken != token {
		return nil
	}
	s.invalidations = edit.Invalidations
	s.cache.Hits += edit.Cache.Hits + rollback.Cache.Hits
	s.cache.Misses += edit.Cache.Misses + rollback.Cache.Misses
	s.frames[s.ds.Project.Images[0].ID] = frameMap(eng.Frames())
	if rollback.Evaluations != nil {
		cp := *res
		cp.Evaluations = rollback.Evaluations
		cp.VersionID = rollback.VersionID
		s.results[s.ds.Project.Images[0].ID] = &cp
	}
	return nil
}

func (s *Service) validatePaths(token int) {
	ctx := context.Background()
	s.mu.Lock()
	if s.runToken != token {
		s.mu.Unlock()
		return
	}
	s.paths["resume"] = pathView{Status: "running"}
	s.paths["full"] = pathView{Status: "running"}
	s.mu.Unlock()

	cfg := runner.Config{
		RunID: "serve-full", Mode: pipeline.ModeFull, Workers: s.opts.Workers,
		MaxThreadBufferBytes: 1 << 20, BufferCapacityBytes: 4 << 20,
		PersistentCache: true,
	}
	_, fullErr := runner.RunImages(ctx, s.ds, s.st, cfg, nil)
	resumeCfg := cfg
	resumeCfg.RunID = "serve-resume"
	resumeCfg.Mode = pipeline.ModeLive
	resumeCfg.StopAfter = 31
	_, resumeErr := runner.RunImages(ctx, s.ds, s.st, resumeCfg, nil)
	if resumeErr == nil {
		resumeCfg.Mode = pipeline.ModeResume
		resumeCfg.StopAfter = 0
		_, resumeErr = runner.RunImages(ctx, s.ds, s.st, resumeCfg, nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runToken != token {
		return
	}
	if fullErr != nil {
		s.paths["full"] = pathView{Status: "failed", Note: fullErr.Error()}
	} else {
		s.paths["full"] = pathView{Status: "completed", RunID: "serve-full"}
	}
	if resumeErr != nil {
		s.paths["resume"] = pathView{Status: "failed", Note: resumeErr.Error()}
	} else {
		s.paths["resume"] = pathView{Status: "completed", RunID: "serve-resume"}
	}
	if fullErr == nil && resumeErr == nil {
		cmpLF, errLF := compare.Runs(ctx, s.st, "serve-live", "serve-full", nil)
		cmpRF, errRF := compare.Runs(ctx, s.st, "serve-resume", "serve-full", nil)
		cmpLR, errLR := compare.Runs(ctx, s.st, "serve-live", "serve-resume", nil)
		if errLF == nil {
			s.paths["live"] = pathView{Status: "completed", RunID: "serve-live", Identical: cmpLF.Identical}
		} else if cmpLF != nil {
			s.paths["live"] = pathView{Status: "mismatch", RunID: "serve-live",
				Differences: cmpLF.Differences, FirstDiff: cmpLF.FirstDiff, Note: errLF.Error()}
		}
		if errRF == nil {
			s.paths["resume"] = pathView{Status: "completed", RunID: "serve-resume", Identical: cmpRF.Identical}
		} else if cmpRF != nil {
			s.paths["resume"] = pathView{Status: "mismatch", RunID: "serve-resume",
				Differences: cmpRF.Differences, FirstDiff: cmpRF.FirstDiff, Note: errRF.Error()}
		}
		if errLR != nil && cmpLR != nil {
			s.paths["live"] = pathView{Status: "mismatch", RunID: "serve-live",
				Differences: cmpLR.Differences, FirstDiff: cmpLR.FirstDiff, Note: errLR.Error()}
		}
		s.paths["full"] = pathView{Status: "completed", RunID: "serve-full",
			Identical: errLF == nil && errRF == nil && errLR == nil}
	}
	for _, runID := range []string{"serve-live", "serve-full", "serve-resume"} {
		vr, err := verify.Run(ctx, s.st, runID, s.ds.Truth, s.ds.TruthFP)
		s.verification[runID] = err == nil && vr != nil && vr.Verified
	}
}

func (s *Service) finish(token int, status, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runToken != token {
		return
	}
	s.status = status
	if message != "" {
		s.message = message
	}
	s.paused = false
	s.currentEngine = nil
}

func (s *Service) Pause() {
	s.mu.Lock()
	eng := s.currentEngine
	s.paused = true
	s.mu.Unlock()
	if eng != nil {
		eng.Pause()
	}
}

func (s *Service) Resume() {
	s.mu.Lock()
	eng := s.currentEngine
	s.paused = false
	s.mu.Unlock()
	if eng != nil {
		eng.Resume()
	}
}

func (s *Service) Cancel() {
	s.mu.Lock()
	eng := s.currentEngine
	cancel := s.runCancel
	if s.status == "running" {
		s.status = "canceling"
	}
	s.mu.Unlock()
	if eng != nil {
		eng.Cancel()
	}
	if cancel != nil {
		cancel()
	}
}

func (s *Service) RetryBlock(ctx context.Context, imageID string, step, x, y int) (*pipeline.EditResult, error) {
	s.mu.RLock()
	eng := s.engines[imageID]
	bs := s.ds.Project.BlockSize
	s.mu.RUnlock()
	if eng == nil {
		return nil, core.StateError("pending", "completed", step, nil, "engine is not ready for retry")
	}
	cols := s.ds.Project.Width / bs
	if x < 0 || y < 0 || x%bs != 0 || y%bs != 0 || x >= s.ds.Project.Width || y >= s.ds.Project.Height {
		return nil, core.Range("block", "block origin (%d,%d) is not on the grid", x, y)
	}
	idx := (y/bs)*cols + x/bs
	result, err := eng.RetryBlock(ctx, step, idx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.frames[imageID] = frameMap(eng.Frames())
	s.invalidations = append(result.Invalidations, s.invalidations...)
	s.cache.Hits += result.Cache.Hits
	s.cache.Misses += result.Cache.Misses
	if existing := s.results[imageID]; existing != nil {
		cp := *existing
		cp.Evaluations = result.Evaluations
		cp.VersionID = result.VersionID
		s.results[imageID] = &cp
	}
	s.mu.Unlock()
	return result, nil
}

type stateResponse struct {
	Status        string                  `json:"status"`
	Message       string                  `json:"message,omitempty"`
	Paused        bool                    `json:"paused"`
	CurrentImage  string                  `json:"current_image"`
	Progress      map[string][2]int       `json:"progress"`
	Width         int                     `json:"width"`
	Height        int                     `json:"height"`
	BlockSize     int                     `json:"block_size"`
	StepCount     int                     `json:"step_count"`
	Images        []imageView             `json:"images"`
	Cache         pipeline.CacheStats     `json:"cache"`
	BufferWaits   []pipeline.WaitEvent    `json:"buffer_waits"`
	QueueWaits    []pipeline.WaitEvent    `json:"queue_waits"`
	Invalidations []pipeline.Invalidation `json:"invalidations"`
	Paths         map[string]pathView     `json:"paths"`
	Verification  map[string]bool         `json:"verification"`
}

type imageView struct {
	ID        string                    `json:"id"`
	Frame     int                       `json:"frame"`
	StepCount int                       `json:"step_count"`
	Available []int                     `json:"available_steps"`
	Evals     []quality.BlockEvaluation `json:"evaluations"`
	Traces    []pipeline.StepTrace      `json:"step_traces"`
	Version   string                    `json:"version,omitempty"`
}

func (s *Service) State() stateResponse {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := stateResponse{
		Status: s.status, Message: s.message, Paused: s.paused, CurrentImage: s.currentImage,
		Width: s.ds.Project.Width, Height: s.ds.Project.Height,
		BlockSize: s.ds.Project.BlockSize, StepCount: len(s.ds.Project.Chains[0].Steps),
		Cache: s.cache, BufferWaits: append([]pipeline.WaitEvent(nil), s.bufferWaits...),
		QueueWaits:    append([]pipeline.WaitEvent(nil), s.queueWaits...),
		Invalidations: append([]pipeline.Invalidation(nil), s.invalidations...),
	}
	out.Progress = make(map[string][2]int, len(s.progress))
	for k, v := range s.progress {
		out.Progress[k] = v
	}
	out.Paths = make(map[string]pathView, len(s.paths))
	for k, v := range s.paths {
		out.Paths[k] = v
	}
	out.Verification = make(map[string]bool, len(s.verification))
	for k, v := range s.verification {
		out.Verification[k] = v
	}
	for _, spec := range s.ds.Project.Images {
		stepCount := 0
		if chain, ok := s.ds.Project.Chain(spec.ID); ok {
			stepCount = len(chain.Steps)
		}
		view := imageView{ID: spec.ID, Frame: spec.Frame, StepCount: stepCount}
		for step := range s.frames[spec.ID] {
			view.Available = append(view.Available, step)
		}
		sortInts(view.Available)
		if result := s.results[spec.ID]; result != nil {
			view.Evals = result.Evaluations
			view.Traces = result.StepTraces
			view.Version = result.VersionID
		}
		out.Images = append(out.Images, view)
	}
	return out
}

func (s *Service) imagePNG(imageID string, step int) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	frame := s.frames[imageID][step]
	if frame == nil {
		return nil, fmt.Errorf("image %s step %d is not available", imageID, step)
	}
	return frame.EncodePNG()
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageHTML))
	})
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, s.State())
	})
	mux.HandleFunc("/api/image", func(w http.ResponseWriter, r *http.Request) {
		step, err := strconv.Atoi(r.URL.Query().Get("step"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "E_PARA", "message": "step must be an integer"})
			return
		}
		data, err := s.imagePNG(r.URL.Query().Get("image"), step)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"code": "E_RANGE", "message": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("/api/control", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": "E_PARA", "message": "POST required"})
			return
		}
		switch r.URL.Query().Get("action") {
		case "pause":
			s.Pause()
		case "resume":
			s.Resume()
		case "cancel":
			s.Cancel()
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": "E_PARA", "message": "unknown action"})
			return
		}
		writeJSON(w, http.StatusOK, s.State())
	})
	mux.HandleFunc("/api/retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": "E_PARA", "message": "POST required"})
			return
		}
		step, _ := strconv.Atoi(r.URL.Query().Get("step"))
		x, _ := strconv.Atoi(r.URL.Query().Get("x"))
		y, _ := strconv.Atoi(r.URL.Query().Get("y"))
		result, err := s.RetryBlock(r.Context(), r.URL.Query().Get("image"), step, x, y)
		if err != nil {
			if e, ok := core.As(err); ok {
				writeJSON(w, http.StatusBadRequest, e)
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]any{"message": err.Error()})
			}
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"version": result.VersionID, "invalidations": result.Invalidations,
			"cache": result.Cache, "recomputed_blocks": result.RecomputedBlocks,
		})
	})
	mux.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"code": "E_PARA", "message": "POST required"})
			return
		}
		s.Start()
		writeJSON(w, http.StatusOK, s.State())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

func frameMap(frames []*core.Frame) map[int]*core.Frame {
	out := make(map[int]*core.Frame, len(frames))
	for i, frame := range frames {
		if frame != nil {
			out[i] = frame.Clone()
		}
	}
	return out
}

func sortInts(v []int) {
	for i := 0; i < len(v); i++ {
		for j := i + 1; j < len(v); j++ {
			if v[j] < v[i] {
				v[i], v[j] = v[j], v[i]
			}
		}
	}
}
