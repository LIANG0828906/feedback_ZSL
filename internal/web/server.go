package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"roomreverb/internal/checkpoint"
	"roomreverb/internal/engine"
	"roomreverb/internal/errs"
	"roomreverb/internal/model"
	"roomreverb/internal/verify"
)

//go:embed static/*
var staticFiles embed.FS

type Server struct {
	mu         sync.Mutex
	base       model.Level
	truth      model.Truth
	session    *engine.PreviewSession
	store      *checkpoint.Store
	cfg        engine.Config
	dbPath     string
	lastChange *engine.ChangeReport
	httpServer *http.Server
}

type Options struct {
	DBPath string
	Config engine.Config
}

func New(level model.Level, truth model.Truth, store *checkpoint.Store, opts Options) (*Server, error) {
	session, err := engine.NewPreview(context.Background(), level, store, opts.Config)
	if err != nil {
		return nil, err
	}
	return &Server{
		base: level.Clone(), truth: truth, session: session, store: store,
		cfg: opts.Config, dbPath: opts.DBPath,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/params", s.handleParams)
	mux.HandleFunc("/api/portal", s.handlePortal)
	mux.HandleFunc("/api/edge", s.handleEdge)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/compare", s.handleCompare)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "time": time.Now().UTC()})
	})
	sub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	s.httpServer = &http.Server{
		Addr: addr, Handler: s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("room reverb workbench: http://%s\n", addr)
		errCh <- s.httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return s.httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "GET required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.statePayload())
}

func (s *Server) handleParams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "POST required"})
		return
	}
	var req struct {
		RoomID string  `json:"room_id"`
		T60MS  float64 `json:"t60_ms"`
		Early  float64 `json:"early_density"`
		Gain   float64 `json:"reverb_gain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errs.Decode("", "", "invalid JSON: %v", err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	edit := model.Edit{
		Kind: model.EditSetRoomParams, RoomID: req.RoomID,
		Params: &model.RoomParams{T60MS: req.T60MS, EarlyDensity: req.Early, ReverbGain: req.Gain},
	}
	report, err := s.session.Apply(r.Context(), edit)
	if err == nil || len(report.Invalidations) > 0 {
		s.lastChange = &report
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "change": report, "state": s.statePayload()})
}

func (s *Server) handlePortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "POST required"})
		return
	}
	var req struct {
		Action   string      `json:"action"`
		PortalID string      `json:"portal_id"`
		ID       string      `json:"id"`
		WallID   string      `json:"wall_id"`
		Center   model.Point `json:"center"`
		Width    float64     `json:"width"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errs.Decode("", "", "invalid JSON: %v", err))
		return
	}
	var edit model.Edit
	switch req.Action {
	case "add":
		edit = model.Edit{Kind: model.EditAddPortal, Portal: &model.Opening{
			ID: req.ID, WallID: req.WallID, Center: req.Center, Width: req.Width, Active: true,
		}}
	case "remove":
		edit = model.Edit{Kind: model.EditRemovePortal, PortalID: req.PortalID}
	default:
		writeError(w, &errs.Error{Code: errs.EInput, Field: "action", Message: "action must be add or remove"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	report, err := s.session.Apply(r.Context(), edit)
	if err == nil || len(report.Invalidations) > 0 {
		s.lastChange = &report
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "change": report, "state": s.statePayload()})
}

func (s *Server) handleEdge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "POST required"})
		return
	}
	var req struct {
		RoomID  string  `json:"room_id"`
		Edge    string  `json:"edge"`
		DeltaMM float64 `json:"delta_mm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errs.Decode("", "", "invalid JSON: %v", err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	report, err := s.session.Apply(r.Context(), model.Edit{
		Kind: model.EditMoveRoomEdge, RoomID: req.RoomID, Edge: req.Edge, DeltaMM: req.DeltaMM,
	})
	if err == nil || len(report.Invalidations) > 0 {
		s.lastChange = &report
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "change": report, "state": s.statePayload()})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "POST required"})
		return
	}
	var req struct {
		RoomID string `json:"room_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, errs.Decode("", "", "invalid JSON: %v", err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.session.Retry(r.Context(), req.RoomID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.statePayload()})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "POST required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	cfg.ParamsVersion = 0
	session, err := engine.NewPreview(r.Context(), s.base, s.store, cfg)
	if err != nil {
		writeError(w, err)
		return
	}
	s.session = session
	s.lastChange = nil
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.statePayload()})
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "POST required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	dbPath := filepath.Join(os.TempDir(), fmt.Sprintf("roomreverb-ui-%d-%d.sqlite", os.Getpid(), time.Now().UnixNano()))
	cfg := s.cfg
	cfg.UseCache = false
	three, err := verify.ThreeWay(ctx, s.base, s.session.Edits(), cfg, dbPath)
	_ = os.Remove(dbPath)
	_ = os.Remove(dbPath + "-wal")
	_ = os.Remove(dbPath + "-shm")
	if err != nil {
		payload := map[string]any{"ok": false, "error": errorPayload(err), "comparison": compareSummary(three)}
		writeJSON(w, http.StatusConflict, payload)
		return
	}
	current := s.session.RunResult()
	if err := engine.Compare(three.Full, current); err != nil {
		payload := map[string]any{"ok": false, "error": errorPayload(err), "comparison": compareSummary(three)}
		writeJSON(w, http.StatusConflict, payload)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "comparison": compareSummary(three)})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, &errs.Error{Code: errs.EInput, Field: "method", Message: "GET required"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := map[string]any{
		"schema_version":    "roomreverb.export.v1",
		"exported_at":       time.Now().UTC().Format(time.RFC3339),
		"algorithm_version": model.AlgorithmVersion,
		"precision":         model.Precision,
		"base_level":        s.base,
		"current_level":     s.session.Level(),
		"edits":             s.session.Edits(),
		"dependencies":      s.session.Dependencies(),
		"result":            s.session.RunResult(),
		"last_change":       s.lastChange,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="roomreverb-export.json"`)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

func (s *Server) statePayload() map[string]any {
	run := s.session.RunResult()
	return map[string]any{
		"level": s.session.Level(),
		"truth": map[string]any{
			"algorithm_version": s.truth.AlgorithmVersion,
			"precision":         s.truth.Precision,
			"tolerance":         s.truth.Tolerance,
			"listener_rooms":    s.truth.ListenerRooms,
		},
		"result":       run,
		"dependencies": s.session.Dependencies(),
		"last_change":  s.lastChange,
		"db_path":      s.dbPath,
		"config":       s.cfg,
		"budget": map[string]any{
			"max_parallel":       s.cfg.Solvers.Normalized().MaxParallel,
			"path_buffer_limit":  s.cfg.Solvers.Normalized().PathBufferLimit,
			"graph_cache_budget": s.cfg.Solvers.Normalized().GraphCacheBudget,
			"checkpoint_serial":  true,
		},
	}
}

func compareSummary(three verify.ThreeWayResult) map[string]any {
	if three.Full == nil {
		return map[string]any{"ok": false}
	}
	out := map[string]any{
		"ok":           three.Full != nil && three.Preview != nil && three.Resume != nil,
		"commit_order": three.Full.CommitOrder,
		"stats":        engine.Stats(three.Full),
		"progress":     three.Full.Progress,
		"rooms":        len(three.Full.Rooms),
	}
	if three.Preview != nil {
		out["preview_reused"] = countReused(three.Preview.Invalidations)
	}
	if three.Resume != nil {
		out["resume_metrics"] = three.Resume.Metrics
		out["resume_reset"] = three.Resume.Reset
	}
	return out
}

func countReused(records []engine.InvalidationRecord) int {
	n := 0
	for _, r := range records {
		if r.Reused {
			n++
		}
	}
	return n
}

type apiError struct {
	Code     string `json:"code"`
	Room     string `json:"room,omitempty"`
	Source   string `json:"source,omitempty"`
	Listener string `json:"listener,omitempty"`
	Field    string `json:"field,omitempty"`
	Message  string `json:"message"`
}

func errorPayload(err error) apiError {
	var typed *errs.Error
	_ = errors.As(err, &typed)
	if typed == nil {
		return apiError{Code: "E_INTERNAL", Message: err.Error()}
	}
	return apiError{Code: string(typed.Code), Room: typed.Room, Source: typed.Source, Listener: typed.Listener, Field: typed.Field, Message: typed.Message}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	switch errs.CodeOf(err) {
	case errs.EState:
		status = http.StatusConflict
	case errs.EConn:
		status = http.StatusConflict
	case errs.EDiff:
		status = http.StatusConflict
	case errs.EInput, errs.EDecode:
		status = http.StatusBadRequest
	case errs.ECheck:
		status = http.StatusInternalServerError
	case errs.EResource:
		status = http.StatusTooManyRequests
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": errorPayload(err)})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
