package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
	"edgerefine/internal/runner"
	"edgerefine/internal/sample"
	"edgerefine/internal/state"
	"edgerefine/internal/store"
)

type job struct {
	matrix *state.Matrix
	gate   *runner.Gate
	cancel context.CancelFunc
	err    string
}

type Server struct {
	samplesDir string
	dbPath     string
	db         *store.Store
	mu         sync.Mutex
	bundles    map[string]*sample.Bundle
	reports    map[string]*model.RunReport
	previews   map[string]*runner.PreviewResult
	jobs       map[string]*job
}

func New(samplesDir, dbPath string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Server{
		samplesDir: samplesDir, dbPath: dbPath, db: db,
		bundles:  map[string]*sample.Bundle{},
		reports:  map[string]*model.RunReport{},
		previews: map[string]*runner.PreviewResult{},
		jobs:     map[string]*job{},
	}, nil
}

func (s *Server) Close() error { return s.db.Close() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/samples", s.handleSamples)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/finalize", s.handleFinalize)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/pause", s.handlePause)
	mux.HandleFunc("/api/resume", s.handleResume)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/three-path", s.handleThreePath)
	mux.HandleFunc("/api/radius-sweep", s.handleRadiusSweep)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/viz", s.handleViz)
	mux.Handle("/samples/", http.StripPrefix("/samples/", http.FileServer(http.Dir(s.samplesDir))))
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) bundle(id string) (*sample.Bundle, error) {
	s.mu.Lock()
	b, ok := s.bundles[id]
	s.mu.Unlock()
	if ok {
		return b, nil
	}
	b, err := sample.LoadBundle(filepath.Join(s.samplesDir, id))
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.bundles[id] = b
	s.mu.Unlock()
	return b, nil
}

func (s *Server) handleSamples(w http.ResponseWriter, r *http.Request) {
	indexPath := filepath.Join(s.samplesDir, "index.json")
	b, err := os.ReadFile(indexPath)
	if err == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return
	}
	entries, err := os.ReadDir(s.samplesDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var ms []model.Manifest
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if m, err := sample.LoadManifest(filepath.Join(s.samplesDir, e.Name(), "truth.json")); err == nil {
			ms = append(ms, m)
		}
	}
	writeJSON(w, http.StatusOK, ms)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	if id == "" {
		id = "step128"
	}
	b, err := s.bundle(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	workers := atoiDefault(r.URL.Query().Get("workers"), 4)
	p := b.Manifest.Params
	if v := r.URL.Query().Get("radius"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Radius = n
		}
	}
	if v := r.URL.Query().Get("budget"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.IterBudget = n
		}
	}
	manual := manualThreshold(r)
	fp := sample.Fingerprint(b.Manifest)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := runner.NewGate()
	tiles := runnerTilesFor(b, p)
	mat := state.NewMatrix(tiles)
	s.mu.Lock()
	if old := s.jobs[id]; old != nil && old.cancel != nil {
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]string{"error": "sample is already running"})
		return
	}
	s.jobs[id] = &job{matrix: mat, gate: gate, cancel: cancel}
	s.mu.Unlock()

	rep, runErr := runner.Run(b, runner.Options{
		Workers: workers, Params: p, Store: s.db, TruthFingerprint: fp,
		Persist: true, Resume: true, StrictAccuracy: false, Ctx: ctx,
		Gate: gate, ManualThreshold: manual, Matrix: mat,
	})
	s.mu.Lock()
	if rep != nil {
		s.reports[id] = rep
	}
	if j := s.jobs[id]; j != nil {
		if runErr != nil {
			j.err = runErr.Error()
		}
		j.cancel = nil
	}
	s.mu.Unlock()
	if runErr != nil && rep == nil {
		writeError(w, http.StatusInternalServerError, runErr)
		return
	}
	writeJSON(w, http.StatusOK, runSummary(id, rep, runErr))
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	s.mu.Lock()
	rep := s.reports[id]
	j := s.jobs[id]
	prev := s.previews[id]
	s.mu.Unlock()
	resp := map[string]any{"sample": id}
	if rep != nil {
		resp["report"] = runSummary(id, rep, nil)
		resp["accuracy"] = rep.Accuracy
		resp["divergence"] = rep.Divergence
	}
	if j != nil {
		resp["matrix"] = j.matrix.Snapshot()
		resp["sequence"] = j.matrix.Sequence()
		resp["committed_stages"] = j.matrix.CommittedStages()
		resp["total_stages"] = j.matrix.TotalStages()
		resp["paused"] = j.gate.IsPaused()
		resp["error"] = j.err
	}
	if prev != nil && prev.Report != nil {
		resp["preview"] = prev.Report.Preview
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no active job"})
		return
	}
	j.gate.Pause()
	writeJSON(w, http.StatusOK, map[string]bool{"paused": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("sample")
	s.mu.Lock()
	j := s.jobs[id]
	s.mu.Unlock()
	if j == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no active job"})
		return
	}
	j.gate.Resume()
	writeJSON(w, http.StatusOK, map[string]bool{"paused": false})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func atoiDefault(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func manualThreshold(r *http.Request) *float64 {
	v := r.URL.Query().Get("threshold")
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return nil
	}
	return &f
}

func runnerTilesFor(b *sample.Bundle, p model.Params) []model.TileCoord {
	nx := b.Manifest.Width / p.TileSize
	ny := b.Manifest.Height / p.TileSize
	out := make([]model.TileCoord, 0, nx*ny)
	for y := 0; y < ny; y++ {
		for x := 0; x < nx; x++ {
			out = append(out, model.TileCoord{TX: x, TY: y})
		}
	}
	return out
}

func runSummary(id string, r *model.RunReport, err error) map[string]any {
	if r == nil {
		return map[string]any{"sample_id": id, "error": fmt.Sprint(err)}
	}
	return map[string]any{
		"sample_id": id, "mask_hash": r.MaskHash, "deviation_hash": r.DeviationHash,
		"committed_stages": r.CommittedStages, "total_stages": r.TotalStages,
		"progress": r.Progress, "converged_tiles": r.ConvergedTiles,
		"total_tiles": r.TotalTiles, "converged_tile_ratio": r.ConvergedTileRatio,
		"commit_sequence": r.CommitSequence, "accuracy": r.Accuracy,
		"reference": r.Reference, "divergence": r.Divergence, "error_code": errorCode(err),
	}
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if ee, ok := errs.As(err); ok {
		return ee.Code
	}
	return "E_INTERNAL"
}
