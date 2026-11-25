package diag

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"volfog/fog"
)

type Service struct {
	mu       sync.Mutex
	scenario *fog.Scenario
	graph    *fog.DependencyGraph
	store    *fog.CheckpointStore
	dbPath   string
	faults   *fog.FaultController

	LastFrame       *fog.Frame                `json:"last_frame"`
	LastPreview     *fog.Frame                `json:"last_preview"`
	LastFullCompare *fog.Frame                `json:"last_full_compare"`
	LastManifest    *fog.InvalidationManifest `json:"last_manifest"`
	LastReport      any                       `json:"last_report"`
}

type ErrorResponse struct {
	OK    bool       `json:"ok"`
	Error *fog.Error `json:"error,omitempty"`
}

func NewService(dbPath string) (*Service, error) {
	sc, err := fog.Generate(fog.DefaultSeed)
	if err != nil {
		return nil, err
	}
	graph, err := fog.BuildDependencyGraph(sc, "c0")
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath = ":memory:"
	}
	store, err := fog.OpenCheckpointStore(dbPath)
	if err != nil {
		return nil, err
	}
	svc := &Service{scenario: sc, graph: graph, store: store, dbPath: dbPath, faults: fog.NewFaultController()}
	p := &fog.Pipeline{Scenario: sc, Store: store, CameraID: "c0", Faults: svc.faults}
	res, err := p.RunCheckpointed(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	svc.LastFrame = res.Frame
	return svc, nil
}

func (s *Service) Close() error { return s.store.Close() }

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/graph", s.handleGraph)
	mux.HandleFunc("POST /api/render/preview", s.handlePreviewOnly)
	mux.HandleFunc("POST /api/render/preview-compare", s.handlePreviewCompare)
	mux.HandleFunc("POST /api/render/checkpoint", s.handleCheckpoint)
	mux.HandleFunc("POST /api/edit/density", s.handleDensityEdit)
	mux.HandleFunc("POST /api/edit/coefficient", s.handleCoefficientEdit)
	mux.HandleFunc("POST /api/faults/inject", s.handleInjectFault)
	mux.HandleFunc("POST /api/faults/retry", s.handleRetry)
	mux.HandleFunc("POST /api/verify/truth", s.handleVerifyTruth)
	mux.HandleFunc("POST /api/verify/paths", s.handleVerifyPaths)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var fe *fog.Error
	if errors.As(err, &fe) {
		writeJSON(w, http.StatusOK, ErrorResponse{OK: false, Error: fe})
		return
	}
	writeJSON(w, http.StatusInternalServerError, ErrorResponse{OK: false, Error: &fog.Error{Code: fog.EStore, Message: err.Error()}})
}

func (s *Service) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"scenario":          s.scenario,
		"viewport":          s.graph.Viewport,
		"graph_node_count":  len(s.graph.Nodes),
		"graph_edge_count":  len(s.graph.Edges),
		"last_frame":        s.LastFrame,
		"last_preview":      s.LastPreview,
		"last_full_compare": s.LastFullCompare,
		"last_manifest":     s.LastManifest,
		"last_report":       s.LastReport,
		"db_path":           s.dbPath,
	})
}

func (s *Service) handleGraph(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "graph": s.graph})
}

type previewRequest struct {
	PreviewSteps         int `json:"preview_steps"`
	PreviewShadowSamples int `json:"preview_shadow_samples"`
}

func (s *Service) applyPreviewOverrides(req previewRequest) {
	if req.PreviewSteps > 0 && req.PreviewSteps <= s.scenario.Recipe.Steps {
		s.scenario.Recipe.PreviewSteps = req.PreviewSteps
	}
	if req.PreviewShadowSamples >= 0 && req.PreviewShadowSamples <= s.scenario.Recipe.ShadowSamples {
		s.scenario.Recipe.PreviewShadowSamples = req.PreviewShadowSamples
	}
}

func (s *Service) handlePreviewOnly(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyPreviewOverrides(req)
	preview, err := fog.RenderFrame(r.Context(), s.scenario, "c0", fog.ModePreview, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	s.LastPreview = preview
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "frame": preview})
}

func (s *Service) handlePreviewCompare(w http.ResponseWriter, r *http.Request) {
	var req previewRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyPreviewOverrides(req)
	ctx := r.Context()
	preview, err := fog.RenderFrame(ctx, s.scenario, "c0", fog.ModePreview, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	full, err := fog.RenderFrame(ctx, s.scenario, "c0", fog.ModeFull, nil)
	if err != nil {
		writeError(w, err)
		return
	}
	cmpErr := fog.ComparePreviewAndFull(preview, full, s.scenario.Recipe)
	s.LastPreview = preview
	s.LastFullCompare = full
	s.LastReport = map[string]any{"type": "preview_compare", "ok": cmpErr == nil, "error": errorOrNil(cmpErr)}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": cmpErr == nil, "preview": preview, "full": full,
		"comparison_error": errorOrNil(cmpErr),
	})
}

func errorOrNil(err error) *fog.Error {
	if err == nil {
		return nil
	}
	var fe *fog.Error
	if errors.As(err, &fe) {
		return fe
	}
	return &fog.Error{Code: fog.EDiverge, Message: err.Error()}
}

func (s *Service) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.runPipeline(r.Context())
	if res != nil && res.Frame != nil {
		s.LastFrame = res.Frame
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": errorOrNil(err), "result": res})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Service) runPipeline(ctx context.Context, forceBlocks ...int) (*fog.PipelineResult, error) {
	forced := map[int]bool{}
	for _, id := range forceBlocks {
		forced[id] = true
	}
	p := &fog.Pipeline{Scenario: s.scenario, Store: s.store, CameraID: "c0", Faults: s.faults, ForceBlocks: forced}
	res, err := p.RunCheckpointed(ctx, s.LastManifest)
	if res != nil && res.Manifest != nil {
		s.LastManifest = res.Manifest
	}
	return res, err
}

type densityEditRequest struct {
	Declared bool   `json:"declared"`
	MediumID string `json:"medium_id"`
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Z        int    `json:"z"`
	NewValue int32  `json:"new_value"`
}

func (s *Service) handleDensityEdit(w http.ResponseWriter, r *http.Request) {
	var req densityEditRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()

	edit := s.scenario.DeclaredEdit
	if !req.Declared {
		edit = fog.DensityEdit{MediumID: req.MediumID, X: req.X, Y: req.Y, Z: req.Z, NewValue: req.NewValue}
	}
	manifest := s.graph.InvalidateVoxel(edit)
	applied, changed, err := fog.ApplyDensityEdit(s.scenario, edit)
	if err != nil {
		writeError(w, err)
		return
	}
	if !changed {
		empty := fog.InvalidationManifest{Reason: "density-unchanged-no-invalidation"}
		s.LastManifest = &empty
		s.LastReport = map[string]any{"type": "density_edit", "ok": true, "changed": false}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "changed": false, "manifest": empty})
		return
	}
	edit = applied
	s.graph, err = fog.BuildDependencyGraph(s.scenario, "c0")
	if err != nil {
		writeError(w, err)
		return
	}
	s.LastManifest = &manifest
	res, pipeErr := s.runPipeline(r.Context())
	if res != nil && res.Frame != nil {
		s.LastFrame = res.Frame
	}
	s.LastReport = map[string]any{"type": "density_edit", "ok": pipeErr == nil, "manifest": manifest, "error": errorOrNil(pipeErr)}
	writeJSON(w, http.StatusOK, map[string]any{"ok": pipeErr == nil, "changed": true, "edit": edit, "manifest": manifest, "result": res, "error": errorOrNil(pipeErr)})
}

type coefficientRequest struct {
	MediumID string  `json:"medium_id"`
	Field    string  `json:"field"`
	Value    float64 `json:"value"`
}

func (s *Service) handleCoefficientEdit(w http.ResponseWriter, r *http.Request) {
	var req coefficientRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, &fog.Error{Code: fog.ESchema, Field: "body", Message: err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.scenario.Medium(req.MediumID)
	if !ok {
		writeError(w, &fog.Error{Code: fog.ESchema, MediaID: req.MediumID, Field: "medium_id", Message: "unknown medium"})
		return
	}
	manifest := s.graph.InvalidateMediumCoefficients(req.MediumID, req.Field)
	switch req.Field {
	case "absorption":
		m.Absorption = req.Value
	case "scattering":
		m.Scattering = req.Value
	case "anisotropy":
		m.Anisotropy = req.Value
	default:
		writeError(w, &fog.Error{Code: fog.ESchema, MediaID: req.MediumID, Field: "field", Message: "unsupported coefficient field"})
		return
	}
	if err := fog.ValidateMedium(m, s.scenario.FixedScale); err != nil {
		writeError(w, err)
		return
	}
	m.Rehash()
	var err error
	s.graph, err = fog.BuildDependencyGraph(s.scenario, "c0")
	if err != nil {
		writeError(w, err)
		return
	}
	s.LastManifest = &manifest
	res, pipeErr := s.runPipeline(r.Context())
	if res != nil && res.Frame != nil {
		s.LastFrame = res.Frame
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": pipeErr == nil, "manifest": manifest, "result": res, "error": errorOrNil(pipeErr)})
}

type faultRequest struct {
	Kind     string `json:"kind"`
	MediumID string `json:"medium_id"`
	BlockID  int    `json:"block_id"`
	Message  string `json:"message"`
}

func (s *Service) handleInjectFault(w http.ResponseWriter, r *http.Request) {
	var req faultRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Message == "" {
		req.Message = "diagnostic injected failure"
	}
	switch req.Kind {
	case "decode":
		sc, _ := fog.Generate(fog.DefaultSeed)
		m := sc.Media[0]
		if req.MediumID != "" {
			if candidate, ok := sc.Medium(req.MediumID); ok {
				m = candidate
			}
		}
		payload, err := fog.EncodeMedium(m)
		if err != nil {
			writeError(w, err)
			return
		}
		_, err = fog.DecodeMedium(fog.FlipPayloadByte(payload, len(payload)/2))
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "injection": "E_DECODE", "error": errorOrNil(err)})
	case "order":
		sc, _ := fog.Generate(fog.DefaultSeed)
		sc.Cameras.Cameras[1].Time = sc.Cameras.Cameras[0].Time - 1
		err := fog.ValidateScenario(sc)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "injection": "E_ORDER", "error": errorOrNil(err)})
	case "density":
		sc, _ := fog.Generate(fog.DefaultSeed)
		_, _, err := fog.ApplyDensityEdit(sc, fog.DensityEdit{MediumID: sc.Media[0].ID, X: 999, Y: 1, Z: 2, NewValue: 99})
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "injection": "E_DENS", "error": errorOrNil(err)})
	case "block":
		s.mu.Lock()
		defer s.mu.Unlock()
		blockID := req.BlockID
		if blockID == 0 {
			blockID = 7
		}
		s.faults.FailBlockPersistent(blockID, req.Message)
		res, err := s.runPipeline(r.Context(), blockID)
		if res != nil && res.Frame != nil {
			s.LastFrame = res.Frame
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "injection": "E_BLOCK", "error": errorOrNil(err), "result": res})
	default:
		writeError(w, &fog.Error{Code: fog.ESchema, Field: "kind", Message: "unknown fault kind"})
	}
}

type retryRequest struct {
	BlockID int `json:"block_id"`
}

func (s *Service) handleRetry(w http.ResponseWriter, r *http.Request) {
	var req retryRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults.Clear(req.BlockID)
	res, err := s.runPipeline(r.Context(), req.BlockID)
	if res != nil && res.Frame != nil {
		s.LastFrame = res.Frame
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": err == nil, "result": res, "error": errorOrNil(err)})
}

func (s *Service) handleVerifyTruth(w http.ResponseWriter, r *http.Request) {
	sc, err := fog.Generate(fog.DefaultSeed)
	if err != nil {
		writeError(w, err)
		return
	}
	report, err := fog.VerifyTruth(sc)
	s.mu.Lock()
	s.LastReport = map[string]any{"type": "truth", "report": report, "error": errorOrNil(err)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": err == nil, "report": report, "error": errorOrNil(err)})
}

func (s *Service) handleVerifyPaths(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(filepath.Dir(s.dbPath), "verify-three-paths")
	if s.dbPath == ":memory:" {
		dir = filepath.Join(os.TempDir(), "volfog-verify-three-paths")
	}
	_ = os.MkdirAll(dir, 0o755)
	report, err := fog.VerifyThreePaths(r.Context(), dir)
	s.mu.Lock()
	s.LastReport = map[string]any{"type": "three_paths", "report": report, "error": errorOrNil(err)}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": err == nil, "report": report, "error": errorOrNil(err)})
}
