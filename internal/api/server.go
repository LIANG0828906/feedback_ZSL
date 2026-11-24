package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"

	"storyboardstudio/internal/dependency"
	"storyboardstudio/internal/interop"
	"storyboardstudio/internal/model"
	"storyboardstudio/internal/workbench"
	"storyboardstudio/web"
)

type Server struct {
	Workbench *workbench.Workbench
}

func New(wb *workbench.Workbench) *Server {
	return &Server{Workbench: wb}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("GET /api/projects/{id}", s.getProject)
	mux.HandleFunc("GET /api/projects/{id}/graph", s.getGraph)
	mux.HandleFunc("POST /api/projects/{id}/run", s.runProject)
	mux.HandleFunc("POST /api/projects/{id}/checkpoint", s.checkpoint)
	mux.HandleFunc("POST /api/projects/{id}/resume", s.resume)
	mux.HandleFunc("POST /api/projects/{id}/edit", s.edit)
	mux.HandleFunc("POST /api/projects/{id}/verify", s.verify)
	mux.HandleFunc("GET /api/projects/{id}/export", s.export)
	mux.HandleFunc("GET /api/sessions", s.listSessions)
	mux.HandleFunc("GET /api/sessions/{id}/checkpoints", s.listCheckpoints)

	staticFS, err := fs.Sub(web.Files, ".")
	if err == nil {
		mux.Handle("GET /", http.FileServer(http.FS(staticFS)))
	}
	return jsonContentType(mux)
}

func jsonContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 4 && r.URL.Path[:4] == "/api" {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "algorithm_version": model.AlgorithmVersion})
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects := s.Workbench.ListProjects()
	result := make([]map[string]any, 0, len(projects))
	for _, project := range projects {
		result = append(result, map[string]any{
			"id": project.ID, "seed": project.Seed, "schema": project.Schema,
			"node_count": len(project.Nodes), "choice_count": len(project.ChoiceSequence.Items),
		})
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	project, err := s.Workbench.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) getGraph(w http.ResponseWriter, r *http.Request) {
	project, err := s.Workbench.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "edges": dependency.Build(project).Edges()})
}

func (s *Server) runProject(w http.ResponseWriter, r *http.Request) {
	result, err := s.Workbench.RunContinuous(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type checkpointRequest struct {
	SessionID   string `json:"session_id"`
	InterruptAt int    `json:"interrupt_at"`
}

func (s *Server) checkpoint(w http.ResponseWriter, r *http.Request) {
	var req checkpointRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.SessionID == "" {
		req.SessionID = "default-checkpoint"
	}
	result, err := s.Workbench.RunCheckpointed(r.PathValue("id"), req.SessionID, req.InterruptAt)
	if err != nil && !errors.Is(err, workbench.ErrInterrupted) {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interrupted": errors.Is(err, workbench.ErrInterrupted), "result": result})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	var req checkpointRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.SessionID == "" {
		req.SessionID = "default-checkpoint"
	}
	report, err := s.Workbench.Resume(r.PathValue("id"), req.SessionID, req.InterruptAt)
	if err != nil && !errors.Is(err, workbench.ErrInterrupted) {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"interrupted": errors.Is(err, workbench.ErrInterrupted),
		"result":      report.Result,
		"recovery":    report,
	})
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	var edit workbench.EditSpec
	if err := decodeBody(r, &edit); err != nil {
		writeError(w, err)
		return
	}
	report, err := s.Workbench.EditAndReplay(r.PathValue("id"), "ui-edit-session", edit)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	project, err := s.Workbench.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	dir, err := os.MkdirTemp("", "storyboard-api-verify-*")
	if err != nil {
		writeError(w, err)
		return
	}
	defer os.RemoveAll(dir)
	report, err := workbench.VerifyProject(project, dir)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	project, err := s.Workbench.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	nodeID := r.URL.Query().Get("node_id")
	version := r.URL.Query().Get("version")
	if version == "" {
		version = "3.0"
	}
	fullVersion := map[string]string{"1": interop.Version1, "1.0": interop.Version1, "2": interop.Version2, "2.0": interop.Version2, "3": interop.Version3, "3.0": interop.Version3}[version]
	if fullVersion == "" {
		writeError(w, &model.Error{Code: model.CodeVersion, Actual: version, Message: "unknown version"})
		return
	}
	var node *model.Node
	for i := range project.Nodes {
		if project.Nodes[i].ID == nodeID {
			node = &project.Nodes[i]
		}
	}
	if node == nil {
		writeError(w, &model.Error{Code: model.CodeRef, ConflictNode: nodeID, Message: "node not found"})
		return
	}
	result, err := interop.Encode(&interop.Document{NodeID: node.ID, Shots: node.Shots}, fullVersion)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if result.Lossy != nil {
		w.Header().Set("X-Storyboard-Lossy", string(result.Lossy.LossyFields[0]))
	}
	_, _ = w.Write(result.Data)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.Workbench.Store.ListSessions(100)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessions)
}

func (s *Server) listCheckpoints(w http.ResponseWriter, r *http.Request) {
	checkpoints, err := s.Workbench.Store.ListCheckpoints(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, checkpoints)
}

func decodeBody(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return &model.Error{Code: model.CodeSchema, Message: "invalid JSON request: " + err.Error()}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	var coded *model.Error
	if errors.As(err, &coded) {
		switch coded.Code {
		case model.CodeSchema, model.CodeChoice, model.CodeVersion, model.CodeLossy:
			status = http.StatusBadRequest
		case model.CodeRef:
			status = http.StatusUnprocessableEntity
		case model.CodeHash:
			status = http.StatusConflict
		case model.CodeState:
			status = http.StatusConflict
		}
		writeJSON(w, status, coded)
		return
	}
	writeJSON(w, status, map[string]string{"code": "E_IO", "message": err.Error()})
}
