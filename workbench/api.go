package workbench

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"

	"storyworkbench/engine"
	"storyworkbench/story"
	"storyworkbench/webui"
)

type Server struct {
	app *App
	mux *http.ServeMux
}

func NewServer(app *App) *Server {
	s := &Server{app: app, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("GET /api/health", s.health)
	s.mux.HandleFunc("GET /api/projects", s.projects)
	s.mux.HandleFunc("GET /api/project", s.project)
	s.mux.HandleFunc("POST /api/run", s.run)
	s.mux.HandleFunc("POST /api/compare", s.compare)
	s.mux.HandleFunc("POST /api/edit", s.edit)
	s.mux.HandleFunc("GET /api/saves", s.saves)
	s.mux.HandleFunc("POST /api/saves", s.createSave)
	s.mux.HandleFunc("POST /api/resume", s.resumeSave)
	s.mux.HandleFunc("GET /api/checkpoints", s.checkpoints)
	s.mux.HandleFunc("POST /api/checkpoints/start", s.startCheckpoints)
	s.mux.HandleFunc("POST /api/checkpoints/resume", s.resumeCheckpoints)
	s.mux.HandleFunc("GET /api/export", s.export)
	s.mux.HandleFunc("GET /", s.static)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"algorithm_version": engine.AlgorithmVersion,
		"save_schema":       engine.SaveSchemaVersion,
		"workers":           s.app.cfg.Workers,
		"buffer_per_worker": s.app.cfg.BufferPerWorker,
		"checkpoint_db":     s.app.cfg.CheckpointDB,
		"projects":          len(s.app.order),
	})
}

func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "projects": s.app.Summaries()})
}

func (s *Server) project(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	detail, err := s.app.Detail(id)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "detail": detail})
}

type runRequest struct {
	ProjectID string         `json:"project_id"`
	Choices   []story.Choice `json:"choices,omitempty"`
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	project, err := s.app.Project(req.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	choices := req.Choices
	if choices == nil {
		choices = project.Choices
	}
	run, err := engine.Play(project, choices)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run": run})
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	var req CompareRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	response := s.app.CompareThreePaths(r.Context(), req)
	writeJSON(w, http.StatusOK, response)
}

type editRequest struct {
	ProjectID string          `json:"project_id"`
	Edit      engine.NodeEdit `json:"edit"`
}

func (s *Server) edit(w http.ResponseWriter, r *http.Request) {
	var req editRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	project, err := s.app.Project(req.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	baseline, err := s.app.Baseline(req.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	outcome, err := engine.ReplayEdited(project, project.Choices, baseline, req.Edit)
	if err != nil {
		writeError(w, err)
		return
	}
	graph, err := story.BuildDependencyGraph(outcome.Project)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "outcome": outcome, "edited_dependency_graph": graph})
}

func (s *Server) saves(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saves": s.app.Saves(projectID)})
}

type saveRequest struct {
	ProjectID    string `json:"project_id"`
	ShotBoundary int    `json:"shot_boundary"`
	Label        string `json:"label"`
}

func (s *Server) createSave(w http.ResponseWriter, r *http.Request) {
	var req saveRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	response, err := s.app.CreateSave(req.ProjectID, req.ShotBoundary, req.Label)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "result": response})
}

type resumeRequest struct {
	ProjectID string `json:"project_id"`
	SaveID    string `json:"save_id"`
}

func (s *Server) resumeSave(w http.ResponseWriter, r *http.Request) {
	var req resumeRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	save, run, err := s.app.ResumeSave(req.ProjectID, req.SaveID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "save": save, "run": run})
}

func (s *Server) checkpoints(w http.ResponseWriter, r *http.Request) {
	summaries, err := s.app.store.AllSummaries()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "checkpoints": summaries})
}

type checkpointRequest struct {
	ProjectID string `json:"project_id"`
}

func (s *Server) startCheckpoints(w http.ResponseWriter, r *http.Request) {
	var req checkpointRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	run, summary, err := s.app.StartCheckpoints(r.Context(), req.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "run": run, "checkpoint": summary})
}

func (s *Server) resumeCheckpoints(w http.ResponseWriter, r *http.Request) {
	var req checkpointRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, err)
		return
	}
	save, run, err := s.app.RestoreCheckpoints(req.ProjectID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "save": save, "run": run})
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	projectID := r.URL.Query().Get("project_id")
	saveID := r.URL.Query().Get("save_id")
	var value any
	var filename string
	switch kind {
	case "project":
		project, err := s.app.Project(projectID)
		if err != nil {
			writeError(w, err)
			return
		}
		value = project
		filename = project.ID + "-project.json"
	case "run":
		run, err := s.app.Baseline(projectID)
		if err != nil {
			writeError(w, err)
			return
		}
		value = run
		filename = projectID + "-run.json"
	case "save":
		save, err := s.app.LoadSave(projectID, saveID)
		if err != nil {
			writeError(w, err)
			return
		}
		value = save
		filename = saveID + ".json"
	case "checkpoint":
		project, err := s.app.Project(projectID)
		if err != nil {
			writeError(w, err)
			return
		}
		save, err := s.app.store.LoadLatest(project)
		if err != nil {
			writeError(w, err)
			return
		}
		value = save
		filename = projectID + "-checkpoint.json"
	default:
		writeError(w, story.NewError(story.CodeNotFound, "", "kind", "unknown export kind"))
		return
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(data, '\n'))
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	requestPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if requestPath == "" || requestPath == "." {
		requestPath = "index.html"
	}
	data, err := fs.ReadFile(webui.Files, requestPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch path.Ext(requestPath) {
	case ".html":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	case ".js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case ".css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case ".svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func decodeRequest(r *http.Request, target any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return story.NewError(story.CodeSchema, "", "request", err.Error())
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	appErr := errorPayload(err)
	status := http.StatusInternalServerError
	switch appErr.Code {
	case story.CodeSchema, story.CodePara, story.CodeCycle:
		status = http.StatusBadRequest
	case story.CodeSave, story.CodeDiverge:
		status = http.StatusConflict
	case story.CodeNotFound:
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": appErr})
}

func RunServer(app *App, addr string) error {
	server := &http.Server{Addr: addr, Handler: NewServer(app).Handler()}
	_, _ = os.Stdout.WriteString("story workbench listening on http://" + addr + "\n")
	return server.ListenAndServe()
}
