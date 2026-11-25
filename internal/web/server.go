package web

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"

	"mocapbench/internal/core"
	"mocapbench/internal/service"
)

//go:embed static/index.html
var indexHTML []byte

type Server struct {
	service  *service.Service
	verifyMu sync.Mutex
}

type verifyRequest struct {
	ClipID                string            `json:"clip_id"`
	TargetID              string            `json:"target_id"`
	Workers               int               `json:"workers"`
	Buffer                int               `json:"buffer"`
	CrashAfter            int               `json:"crash_after"`
	QuantStep             float64           `json:"quant_step"`
	ContactDriftLimit     float64           `json:"contact_drift_limit"`
	AngleTolerance        float64           `json:"angle_tolerance"`
	MaxIterations         int               `json:"max_iterations"`
	GlobalScaleMultiplier float64           `json:"global_scale_multiplier"`
	SmoothingAlpha        float64           `json:"smoothing_alpha"`
	MappingOverrides      map[string]string `json:"mapping_overrides"`
}

type verifyResponse struct {
	OK     bool            `json:"ok"`
	Error  *core.Error     `json:"error,omitempty"`
	Report *core.RunReport `json:"report,omitempty"`
}

func ListenAndServe(addr, sampleDir, checkpointDir string) error {
	svc, err := service.New(sampleDir, checkpointDir)
	if err != nil {
		return err
	}
	s := &Server{service: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/verify", s.handleVerify)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"manifest":  s.service.Samples.Manifest,
		"clips":     s.service.Samples.Clips,
		"targets":   s.service.Samples.Targets,
		"truth":     s.service.Samples.Truth,
		"defaults":  core.DefaultParameters(),
		"resources": core.DefaultResources(),
	})
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	var req verifyRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, verifyResponse{OK: false, Error: core.NewError(core.ECodeSchema, err.Error())})
			return
		}
	}
	params := core.DefaultParameters()
	if req.QuantStep > 0 {
		params.QuantStep = req.QuantStep
	}
	if req.ContactDriftLimit > 0 {
		params.ContactDriftLimit = req.ContactDriftLimit
	}
	if req.AngleTolerance > 0 {
		params.AngleTolerance = req.AngleTolerance
	}
	if req.MaxIterations > 0 {
		params.MaxIterations = req.MaxIterations
	}
	if req.GlobalScaleMultiplier > 0 {
		params.GlobalScaleMultiplier = req.GlobalScaleMultiplier
	}
	if req.SmoothingAlpha > 0 {
		params.SmoothingAlpha = req.SmoothingAlpha
	}
	params.MappingOverrides = req.MappingOverrides
	resources := core.DefaultResources()
	if req.Workers > 0 {
		resources.Workers = req.Workers
	}
	if req.Buffer > 0 {
		resources.BufferPerWorker = req.Buffer
	}

	s.verifyMu.Lock()
	report, verr := s.service.Verify(req.ClipID, req.TargetID, params, resources, req.CrashAfter)
	s.verifyMu.Unlock()
	resp := verifyResponse{OK: verr == nil && report != nil && report.OK, Report: report}
	if verr != nil {
		resp.Error = verr
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}
