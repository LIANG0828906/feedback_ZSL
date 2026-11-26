package web

import (
	"encoding/json"
	"net/http"

	"dialoguefit/model"
)

type EditFunc func(model.Edit) (*model.InvalidationReport, error)
type RetryFunc func(dialogueID string) (*model.InvalidationReport, error)
type SnapshotFunc func() []*model.DialogueResult
type InjectFunc func(dialogueID string) error

type Config struct {
	Report    any
	Truth     *model.TruthFile
	ApplyEdit EditFunc
	Retry     RetryFunc
	Snapshot  SnapshotFunc
	Inject    InjectFunc
}

type Server struct {
	cfg Config
	mux *http.ServeMux
}

func New(cfg Config) *Server {
	s := &Server{cfg: cfg, mux: http.NewServeMux()}
	s.mux.HandleFunc("/", s.index)
	s.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("/api/state", s.state)
	s.mux.HandleFunc("/api/edit", s.edit)
	s.mux.HandleFunc("/api/retry", s.retry)
	s.mux.HandleFunc("/api/inject-failure", s.injectFailure)
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	results := []*model.DialogueResult{}
	if s.cfg.Snapshot != nil {
		results = s.cfg.Snapshot()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"report":  s.cfg.Report,
		"truth":   s.cfg.Truth,
		"results": results,
	})
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.ApplyEdit == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "edit callback is not configured"})
		return
	}
	var edit model.Edit
	if err := json.NewDecoder(r.Body).Decode(&edit); err != nil {
		writeJSON(w, http.StatusBadRequest, model.DecodeError("*", "body", "invalid edit JSON: %v", err))
		return
	}
	report, err := s.cfg.ApplyEdit(edit)
	if err != nil {
		status := http.StatusInternalServerError
		if e, ok := model.AsError(err); ok && e.Code == model.CodeFit {
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, map[string]any{"error": err.Error(), "report": report})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) retry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.Retry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "retry callback is not configured"})
		return
	}
	var body struct {
		DialogueID string `json:"dialogue_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, model.DecodeError("*", "body", "invalid retry JSON: %v", err))
		return
	}
	report, err := s.cfg.Retry(body.DialogueID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "report": report})
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) injectFailure(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.Inject == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "inject callback is not configured"})
		return
	}
	var body struct {
		DialogueID string `json:"dialogue_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, model.DecodeError("*", "body", "invalid inject JSON: %v", err))
		return
	}
	if err := s.cfg.Inject(body.DialogueID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "injected", "dialogue_id": body.DialogueID})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
