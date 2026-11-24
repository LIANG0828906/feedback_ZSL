package panel

import (
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"retargetlab/internal/engine"
	"retargetlab/internal/model"
	"retargetlab/internal/solver"
)

//go:embed static/index.html
var indexHTML []byte

type Server struct {
	engine *engine.Engine
	truth  *model.TargetTruth
	mux    *http.ServeMux
}

func New(e *engine.Engine, truth *model.TargetTruth) *Server {
	s := &Server{engine: e, truth: truth, mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.index)
	s.mux.HandleFunc("/api/health", s.health)
	s.mux.HandleFunc("/api/state", s.state)
	s.mux.HandleFunc("/api/frame", s.frame)
	s.mux.HandleFunc("/api/mapping", s.mapping)
	s.mux.HandleFunc("/api/scale", s.scale)
	s.mux.HandleFunc("/api/solve", s.solve)
	s.mux.HandleFunc("/api/retry", s.retry)
	s.mux.HandleFunc("/api/validate", s.validate)
	s.mux.HandleFunc("/api/truth", s.compareTruth)
	return s
}

func (s *Server) Handler() http.Handler {
	return s.withJSON(s.mux)
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "time": time.Now().UTC().Format(time.RFC3339)})
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, model.Err(model.E_STATE, "state", "method not allowed"))
		return
	}
	writeJSON(w, http.StatusOK, s.engine.StateSnapshot())
}

func (s *Server) frame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, model.Err(model.E_STATE, "frame", "method not allowed"))
		return
	}
	frame, err := strconv.Atoi(r.URL.Query().Get("frame"))
	if err != nil {
		writeError(w, model.Err(model.E_PARAM, "frame", "frame query is required"))
		return
	}
	detail, err := s.engine.FrameDetail(frame)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

type mappingRequest struct {
	Index int             `json:"index"`
	Rule  model.JointRule `json:"rule"`
}

func (s *Server) mapping(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, model.Err(model.E_STATE, "mapping_edit", "method not allowed"))
		return
	}
	var req mappingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	report, err := s.engine.ApplyMappingRule(req.Index, req.Rule)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

type scaleRequest struct {
	Key   string  `json:"key"`
	Value float64 `json:"value"`
}

func (s *Server) scale(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, model.Err(model.E_STATE, "scale_edit", "method not allowed"))
		return
	}
	var req scaleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	report, err := s.engine.SetScale(req.Key, req.Value)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) solve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, model.Err(model.E_STATE, "solve", "method not allowed"))
		return
	}
	run, err := s.engine.SolveAll(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

type retryRequest struct {
	Frame *int `json:"frame,omitempty"`
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, model.Err(model.E_STATE, "retry", "method not allowed"))
		return
	}
	var req retryRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
	}
	var run engine.RunStats
	var err error
	if req.Frame != nil {
		run, err = s.engine.RetryFrame(r.Context(), *req.Frame)
	} else {
		run, err = s.engine.RetryFailed(r.Context())
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) validate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, model.Err(model.E_STATE, "validate", "method not allowed"))
		return
	}
	report, err := s.engine.CompareWithFull(r.Context())
	if err != nil {
		var ce *model.CodedError
		if errors.As(err, &ce) {
			writeJSON(w, statusFor(ce.Code), struct {
				Report engine.ComparisonReport `json:"report"`
				Error  *model.CodedError       `json:"error"`
			}{Report: report, Error: ce})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) compareTruth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, model.Err(model.E_STATE, "truth", "method not allowed"))
		return
	}
	if s.truth == nil {
		writeError(w, model.Err(model.E_SCHEMA, "truth", "no truth snapshot is configured"))
		return
	}
	results := s.engine.CommittedResults()
	frames := make([]solver.FrameResult, 0, len(results))
	for _, result := range results {
		frames = append(frames, result)
	}
	snap := s.engine.StateSnapshot()
	if err := solver.CompareTruthWithSource(snap.Source, snap.Target, *s.truth, frames); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tolerance": s.truth.Tolerance})
}

func (s *Server) withJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return &model.CodedError{Code: model.E_SCHEMA, Op: "decode_request", Message: err.Error(), Cause: err}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	var ce *model.CodedError
	if errors.As(err, &ce) {
		writeJSON(w, statusFor(ce.Code), ce)
		return
	}
	writeJSON(w, http.StatusInternalServerError, &model.CodedError{
		Code: model.E_IO, Op: "http", Message: err.Error(), Cause: err,
	})
}

func statusFor(code model.ErrorCode) int {
	switch code {
	case model.E_DIFF, model.E_STATE:
		return http.StatusConflict
	case model.E_SCHEMA, model.E_CYCLE, model.E_FRAME:
		return http.StatusUnprocessableEntity
	case model.E_PARAM:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}
