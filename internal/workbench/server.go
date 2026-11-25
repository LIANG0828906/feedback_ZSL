package workbench

import (
	"encoding/json"
	"net/http"
	"strconv"

	"tunnelworkbench/internal/forward"
)

type Server struct {
	svc *Service
	mux *http.ServeMux
}

func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", s.handleIndex)
	s.mux.HandleFunc("GET /api/overview", s.handleOverview)
	s.mux.HandleFunc("GET /api/equivalence", s.handleEquivalence)
	s.mux.HandleFunc("GET /api/document", s.handleDocument)
	s.mux.HandleFunc("GET /api/simulate", s.handleSimulate)
	s.mux.HandleFunc("POST /api/edit", s.handleEdit)
	s.mux.HandleFunc("POST /api/reset-incremental", s.handleResetIncremental)
	s.mux.HandleFunc("GET /api/checkpoint", s.handleCheckpoint)
	s.mux.HandleFunc("POST /api/inject", s.handleInject)
	s.mux.HandleFunc("GET /api/anomaly-kinds", s.handleAnomalyKinds)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	if biz, ok := forward.AsBizError(err); ok {
		writeJSON(w, http.StatusUnprocessableEntity, biz)
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"code": forward.CodeInternal, "message": err.Error()})
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.svc.SampleOverview())
}

func (s *Server) handleEquivalence(w http.ResponseWriter, r *http.Request) {
	rep, err := s.svc.Equivalence()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) handleDocument(w http.ResponseWriter, r *http.Request) {
	schema := r.URL.Query().Get("schema")
	raw, err := s.svc.Document(schema)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

func (s *Server) handleSimulate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	schema := q.Get("schema")
	if schema == "" {
		schema = forward.SchemaV2
	}
	parallel := q.Get("parallel") == "1" || q.Get("parallel") == "true"
	workers, _ := strconv.Atoi(q.Get("workers"))
	buffer, _ := strconv.Atoi(q.Get("buffer"))
	rep, err := s.svc.Simulate(schema, parallel, forward.Budget{Workers: workers, PerTunnelBuffer: buffer})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	var req forward.EditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, forward.NewBizError(forward.CodeSchemaParse, "body", "invalid edit JSON: %v", err))
		return
	}
	rep, err := s.svc.ApplyEdit(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, rep)
}

func (s *Server) handleResetIncremental(w http.ResponseWriter, r *http.Request) {
	schema := r.URL.Query().Get("schema")
	if schema == "" {
		schema = forward.SchemaV2
	}
	if _, err := s.svc.ResetIncremental(schema); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "reset", "schema": schema})
}

func (s *Server) handleCheckpoint(w http.ResponseWriter, r *http.Request) {
	schema := r.URL.Query().Get("schema")
	if schema == "" {
		schema = forward.SchemaV2
	}
	stopAfter, _ := strconv.Atoi(r.URL.Query().Get("stop_after"))
	demo, err := s.svc.CheckpointDemo(r.Context(), schema, stopAfter)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, 200, demo)
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind    string         `json:"kind"`
		Payload map[string]any `json:"payload"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.Kind == "" {
		body.Kind = r.URL.Query().Get("kind")
	}
	res := s.svc.Inject(body.Kind, body.Payload)
	status := 200
	if res.Error != nil {
		status = 422
	}
	writeJSON(w, status, res)
}

func (s *Server) handleAnomalyKinds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, []string{
		"overlap_rule", "missing_peer", "mtu_too_small", "negative_keepalive", "field_conflict",
		"unknown_schema", "budget_workers", "budget_buffer", "backpressure_workers",
		"backpressure_buffer", "checkpoint_truncated", "checkpoint_version_mismatch",
	})
}
