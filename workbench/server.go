// Package workbench serves the local deterministic diagnostics console.
package workbench

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"dnslab/codes"
	"dnslab/editor"
	"dnslab/service"
)

//go:embed static/index.html
var staticFS embed.FS

type Server struct {
	svc *service.Service
	mux *http.ServeMux
}

func NewServer(svc *service.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("GET /api/source", s.handleSource)
	s.mux.HandleFunc("POST /api/load", s.handleLoad)
	s.mux.HandleFunc("POST /api/run", s.handleRun)
	s.mux.HandleFunc("GET /api/query", s.handleQuery)
	s.mux.HandleFunc("POST /api/edit", s.handleEdit)
	s.mux.HandleFunc("POST /api/replace", s.handleReplace)
	s.mux.HandleFunc("POST /api/inject", s.handleInject)
	s.mux.HandleFunc("POST /api/checkpoint/save", s.handleCheckpointSave)
	s.mux.HandleFunc("POST /api/checkpoint/restore", s.handleCheckpointRestore)
	s.mux.HandleFunc("POST /api/checkpoint/recover", s.handleCheckpointRecover)
}

type apiError struct {
	OK    bool         `json:"ok"`
	Error *codes.Error `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeBizError(w http.ResponseWriter, err error) {
	biz, ok := codes.As(err)
	if !ok {
		biz = codes.New("E_INTERNAL", err.Error())
	}
	status := http.StatusBadRequest
	switch biz.Code {
	case codes.EVersionConflict, codes.ECheckpointVersion, codes.EBackpressureWorkers, codes.ERuleOverlap:
		status = http.StatusConflict
	case codes.ECheckpointOpen, codes.ECheckpointCorrupt, codes.ECheckpointTruncated:
		status = http.StatusUnprocessableEntity
	case codes.ECheckpointNotConfigured:
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, apiError{OK: false, Error: biz})
}

func decodeBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return codes.Wrap(codes.EFieldInvalid, "invalid JSON request body", err)
	}
	return nil
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": true, "state": s.svc.State()})
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	schema := r.URL.Query().Get("schema")
	data, err := s.svc.SourceDocument(schema)
	if err != nil {
		writeBizError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}

type loadRequest struct {
	Schema string `json:"schema"`
}

func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	var req loadRequest
	if err := decodeBody(r, &req); err != nil {
		writeBizError(w, err)
		return
	}
	if err := s.svc.LoadSchema(req.Schema); err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "state": s.svc.State()})
}

type runRequest struct {
	Workers int `json:"workers"`
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	resp, err := s.svc.RunAll(r.Context(), req.Workers)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "run": resp})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	seq, err := atoiQuery(r, "sequence")
	if err != nil {
		writeBizError(w, err)
		return
	}
	result, truth, err := s.svc.QuerySample(seq)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "result": result, "truth": truth})
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	var edit editor.Edit
	if err := decodeBody(r, &edit); err != nil {
		writeBizError(w, err)
		return
	}
	resp, err := s.svc.ApplyEdit(edit)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "edit": resp})
}

func (s *Server) handleInject(w http.ResponseWriter, r *http.Request) {
	var req editor.InjectionRequest
	if err := decodeBody(r, &req); err != nil {
		writeBizError(w, err)
		return
	}
	if err := s.svc.Inject(req); err != nil {
		biz, _ := codes.As(err)
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "injected": req.Kind, "error": biz,
			"source_project_modified": false, "committed_cache_polluted": false,
		})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "injected": req.Kind, "note": "request was valid; no exception"})
}

type replaceRequest struct {
	Schema   string          `json:"schema"`
	Document json.RawMessage `json:"document"`
}

func (s *Server) handleReplace(w http.ResponseWriter, r *http.Request) {
	var req replaceRequest
	if err := decodeBody(r, &req); err != nil {
		writeBizError(w, err)
		return
	}
	var resp *service.EditResponse
	var err error
	if len(req.Document) > 0 {
		var raw any
		if jerr := json.Unmarshal(req.Document, &raw); jerr != nil {
			writeBizError(w, codes.Wrap(codes.EFieldInvalid, "document must be raw JSON", jerr))
			return
		}
		resp, err = s.svc.ReplaceSnapshot(req.Document)
	} else {
		resp, err = s.svc.ReplaceGeneratedSchema(req.Schema)
	}
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "edit": resp})
}

func (s *Server) handleCheckpointSave(w http.ResponseWriter, r *http.Request) {
	cp, err := s.svc.SaveCheckpoint(r.Context())
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "checkpoint": checkpointSummary(cp)})
}

func (s *Server) handleCheckpointRestore(w http.ResponseWriter, r *http.Request) {
	cp, err := s.svc.RestoreCheckpoint(r.Context())
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "checkpoint": checkpointSummary(cp), "restored": true})
}

func (s *Server) handleCheckpointRecover(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	resp, err := s.svc.Recover(r.Context(), req.Workers)
	if err != nil {
		writeBizError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "recovery": resp})
}

func checkpointSummary(cp any) any {
	b, _ := json.Marshal(cp)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func atoiQuery(r *http.Request, key string) (int, error) {
	v := r.URL.Query().Get(key)
	if v == "" {
		return 0, codes.New(codes.EFieldMissing, "missing query parameter").WithField(key)
	}
	var n int
	if _, err := json.Number(v).Int64(); err != nil {
		return 0, codes.Wrap(codes.EFieldType, "query parameter must be integer", err).WithField(key)
	}
	n64, _ := json.Number(v).Int64()
	n = int(n64)
	return n, nil
}
