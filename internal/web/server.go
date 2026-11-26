package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"

	"lip-pipeline-prototype/internal/domain"
	"lip-pipeline-prototype/internal/service"
)

//go:embed static/index.html
var staticFS embed.FS

type Server struct {
	svc *service.Service
	mux *http.ServeMux
}

func New(svc *service.Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	static, _ := fs.Sub(staticFS, "static")
	s.mux.Handle("GET /", http.FileServer(http.FS(static)))
	s.mux.HandleFunc("GET /api/overview", s.overview)
	s.mux.HandleFunc("GET /api/schema-reports", s.schemaReports)
	s.mux.HandleFunc("GET /api/dialogues/{id}", s.dialogue)
	s.mux.HandleFunc("GET /api/shards/{id}", s.shard)
	s.mux.HandleFunc("GET /api/schema/{name}", s.schema)
	s.mux.HandleFunc("POST /api/preview", s.preview)
	s.mux.HandleFunc("POST /api/edit", s.edit)
	s.mux.HandleFunc("POST /api/run/realtime", s.runRealtime)
	s.mux.HandleFunc("POST /api/run/resume", s.resume)
	s.mux.HandleFunc("POST /api/run/full", s.full)
	s.mux.HandleFunc("POST /api/checkpoint/corrupt", s.corrupt)
	s.mux.HandleFunc("GET /api/faults/{kind}", s.fault)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if de, ok := domain.AsError(err); ok {
		switch de.Code {
		case domain.CodeNotFound:
			status = http.StatusNotFound
		case domain.CodeSchema, domain.CodeVal:
			status = http.StatusBadRequest
		case domain.CodeState, domain.CodeOrder, domain.CodeDecode, domain.CodeLip, domain.CodeDiverge, domain.CodeStage, domain.CodeOvf:
			status = http.StatusUnprocessableEntity
		}
		writeJSON(w, status, map[string]any{"ok": false, "error": de})
		return
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error()})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.svc.Overview(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": ov})
}

func (s *Server) schemaReports(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.svc.SchemaReports()})
}

func (s *Server) dialogue(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.DialogueDetail(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": d})
}

func (s *Server) shard(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.ShardDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": d})
}

func (s *Server) schema(w http.ResponseWriter, r *http.Request) {
	version := domain.SchemaV3
	switch r.PathValue("name") {
	case "v1":
		version = domain.SchemaV1
	case "v2":
		version = domain.SchemaV2
	case "v3":
		version = domain.SchemaV3
	}
	data, err := s.svc.Document(version)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": version, "data": json.RawMessage(data)})
}

func decodeBody(r *http.Request, target any) error {
	defer r.Body.Close()
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(target)
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var req service.PreviewRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, &domain.Error{Code: domain.CodeSchema, Message: err.Error(), Path: "body"})
		return
	}
	resp, err := s.svc.Preview(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": resp})
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	var req service.EditRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, &domain.Error{Code: domain.CodeSchema, Message: err.Error(), Path: "body"})
		return
	}
	resp, err := s.svc.Edit(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": resp})
}

func (s *Server) runRealtime(w http.ResponseWriter, r *http.Request) {
	var req service.RunRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.svc.RunRealtime(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": resp})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var req service.RunRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, err)
		return
	}
	resp, err := s.svc.Resume(r.Context(), req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": resp})
}

func (s *Server) full(w http.ResponseWriter, r *http.Request) {
	records, results, err := s.svc.FullRecompute()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{
		"records": records, "results": results, "record_count": len(records),
	}})
}

func (s *Server) corrupt(w http.ResponseWriter, r *http.Request) {
	shardID := r.URL.Query().Get("shard_id")
	if shardID == "" {
		var body struct {
			ShardID string `json:"shard_id"`
		}
		_ = decodeBody(r, &body)
		shardID = body.ShardID
	}
	if err := s.svc.CorruptCheckpoint(r.Context(), shardID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "shard_id": shardID})
}

func (s *Server) fault(w http.ResponseWriter, r *http.Request) {
	err := s.svc.Fault(r.PathValue("kind"))
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeError(w, err)
}
