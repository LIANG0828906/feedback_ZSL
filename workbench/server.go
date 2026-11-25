package workbench

import (
	_ "embed"
	"encoding/json"
	"net/http"

	"chaosdiag/incremental"
	"chaosdiag/model"
)

//go:embed web/index.html
var indexHTML []byte

type Server struct {
	svc *Service
	mux *http.ServeMux
}

func NewServer(checkpointDir string) (*Server, error) {
	svc, err := NewService(checkpointDir)
	if err != nil {
		return nil, err
	}
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "chaosdiag-workbench"})
	})
	s.mux.HandleFunc("/api/state", s.handleState)
	s.mux.HandleFunc("/api/doc", s.handleDoc)
	s.mux.HandleFunc("/api/edit", s.handleEdit)
	s.mux.HandleFunc("/api/reset", s.handleReset)
	s.mux.HandleFunc("/api/anomaly", s.handleAnomaly)
	s.mux.HandleFunc("/api/checkpoint-demo", s.handleCheckpointDemo)
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
	state, err := s.svc.State(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request) {
	doc, err := s.svc.Document(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(doc)
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, model.NewError(model.CodeFieldInvalid, "method", "POST required"))
		return
	}
	var mut incremental.Mutation
	if err := json.NewDecoder(r.Body).Decode(&mut); err != nil {
		writeError(w, model.NewError(model.CodeFieldInvalid, "body", "invalid mutation JSON: %v", err))
		return
	}
	out, err := s.svc.Edit(r.URL.Query().Get("version"), mut)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, model.NewError(model.CodeFieldInvalid, "method", "POST required"))
		return
	}
	if err := s.svc.Reset(r.URL.Query().Get("version")); err != nil {
		writeError(w, err)
		return
	}
	state, err := s.svc.State(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleAnomaly(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, model.NewError(model.CodeFieldInvalid, "method", "POST required"))
		return
	}
	var req AnomalyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, model.NewError(model.CodeFieldInvalid, "body", "invalid anomaly JSON: %v", err))
		return
	}
	resp := s.svc.Anomaly(req)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCheckpointDemo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writeError(w, model.NewError(model.CodeFieldInvalid, "method", "GET or POST required"))
		return
	}
	demo, err := s.svc.CheckpointDemo(r.URL.Query().Get("version"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, demo)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	ce := model.AsErrorOrInternal(err)
	writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": ce})
}
