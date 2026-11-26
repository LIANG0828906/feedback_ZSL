package web

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"sort"
	"sync"

	"maskqc/checkpoint"
	"maskqc/engine"
	"maskqc/model"
	"maskqc/raster"
)

type ProjectInput struct {
	ID             string
	Project        *model.Project
	IconPNG        []byte
	Versions       []string
	Lossy          map[string]int
	ResidualDigest string
}

type projectView struct {
	input  ProjectInput
	engine *engine.Engine
	last   *engine.RunResult
}

type Server struct {
	mu       sync.Mutex
	config   engine.Config
	projects map[string]*projectView
	order    []string
	mux      *http.ServeMux
}

func New(inputs []ProjectInput, config engine.Config, stores []*checkpoint.Store) (*Server, error) {
	server := &Server{config: config, projects: map[string]*projectView{}, mux: http.NewServeMux()}
	for index, input := range inputs {
		if input.Project == nil {
			return nil, &model.Error{Code: model.CodeSchema, Path: "input.project", ObjectID: input.ID, Message: "project is nil"}
		}
		var store *checkpoint.Store
		if index < len(stores) && stores[index] != nil {
			store = stores[index]
		}
		projectConfig := config
		if input.ResidualDigest != "" {
			projectConfig.ResidualDigest = input.ResidualDigest
		}
		verificationEngine, err := engine.New(input.Project, projectConfig, store)
		if err != nil {
			return nil, err
		}
		view := &projectView{input: input, engine: verificationEngine}
		server.projects[input.ID] = view
		server.order = append(server.order, input.ID)
	}
	sort.Strings(server.order)
	server.routes()
	server.mu.Lock()
	defer server.mu.Unlock()
	for _, id := range server.order {
		result, err := server.projects[id].engine.Run(context.Background(), engine.ModeResume)
		if err != nil && !onlyVerificationIssues(err) {
			return nil, err
		}
		server.projects[id].last = result
	}
	return server, nil
}

func onlyVerificationIssues(err error) bool {
	return model.IsVerificationError(err)
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	s.mux.HandleFunc("GET /api/projects", s.handleProjects)
	s.mux.HandleFunc("GET /api/project/{id}", s.handleProject)
	s.mux.HandleFunc("POST /api/project/{id}/rerun", s.handleRerun)
	s.mux.HandleFunc("POST /api/project/{id}/edit", s.handleEdit)
	s.mux.HandleFunc("GET /img/{id}/candidate.png", s.handleCandidateImage)
	s.mux.HandleFunc("GET /img/{id}/accumulated.png", s.handleAccumulatedImage)
	s.mux.HandleFunc("GET /img/{id}/icon.png", s.handleIcon)
	s.mux.HandleFunc("GET /img/{id}/stroke/{stroke}", s.handleStrokeImage)
	s.mux.HandleFunc("GET /img/{id}/truth/{domain}", s.handleTruthImage)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type item struct {
		ID       string            `json:"id"`
		Width    int               `json:"width"`
		Height   int               `json:"height"`
		Versions []string          `json:"versions"`
		Lossy    map[string]int    `json:"lossy"`
		Summary  engine.RunSummary `json:"summary"`
	}
	items := make([]item, 0, len(s.order))
	for _, id := range s.order {
		view := s.projects[id]
		items = append(items, item{
			ID: view.input.Project.ID, Width: view.input.Project.Width, Height: view.input.Project.Height,
			Versions: view.input.Versions, Lossy: view.input.Lossy,
			Summary: engine.Summarize(view.last),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": items})
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view, ok := s.projects[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	response := map[string]any{
		"id": view.input.Project.ID, "width": view.input.Project.Width, "height": view.input.Project.Height,
		"versions": view.input.Versions, "lossy": view.input.Lossy,
		"sourceVersion": view.input.Project.Version, "seed": view.input.Project.Seed,
		"residualDigest": view.input.ResidualDigest,
		"strokes":        view.input.Project.Strokes, "order": view.input.Project.StrokeIDsInOrder(),
		"summary": engine.Summarize(view.last),
	}
	if view.last != nil {
		response["summary"] = engine.Summarize(view.last)
		response["domains"] = view.last.Analysis
		response["budget"] = view.last.Budget
		response["graph"] = view.last.Graph
		response["invalidation"] = view.last.Invalidation
		response["issues"] = view.last.Issues
		response["blocks"] = view.last.Blocks
		response["commitSequence"] = view.last.CommitSequence
		response["reusedBlocks"] = view.last.ReusedBlocks
		response["checkpointInvalid"] = view.last.CheckpointInvalid
		response["checkpointReason"] = view.last.CheckpointReason
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view, ok := s.projects[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	mode := engine.Mode(r.URL.Query().Get("mode"))
	if mode == "" {
		mode = engine.ModeIncremental
	}
	result, err := view.engine.Run(r.Context(), mode)
	view.last = result
	status := http.StatusOK
	if err != nil && !onlyVerificationIssues(err) {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{"summary": engine.Summarize(result), "issues": resultIssues(result), "error": errorText(err)})
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	view, ok := s.projects[r.PathValue("id")]
	if !ok {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}
	var request struct {
		StrokeID string `json:"strokeId"`
		DX0      *int   `json:"dx0"`
		DY0      *int   `json:"dy0"`
		DX1      *int   `json:"dx1"`
		DY1      *int   `json:"dy1"`
		Hardness *int   `json:"hardness"`
		Priority *int   `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	stroke, ok := view.input.Project.StrokeByID(request.StrokeID)
	if !ok {
		writeError(w, http.StatusBadRequest, "stroke not found")
		return
	}
	if request.DX0 != nil {
		stroke.X0 += *request.DX0
	}
	if request.DY0 != nil {
		stroke.Y0 += *request.DY0
	}
	if request.DX1 != nil {
		stroke.X1 += *request.DX1
	}
	if request.DY1 != nil {
		stroke.Y1 += *request.DY1
	}
	if request.Hardness != nil {
		stroke.Hardness = *request.Hardness
	}
	if request.Priority != nil {
		stroke.Priority = *request.Priority
	}
	result, err := view.engine.ApplyStrokeEdit(r.Context(), stroke)
	view.last = result
	status := http.StatusOK
	if err != nil && !onlyVerificationIssues(err) {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, map[string]any{
		"summary": engine.Summarize(result), "error": errorText(err),
		"invalidation": resultInvalidation(result), "issues": resultIssues(result),
	})
}

func (s *Server) handleCandidateImage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	view, ok := s.projects[r.PathValue("id")]
	var project *model.Project
	if ok {
		project = view.input.Project
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	writePNG(w, maskImage(project.CandidateMask, project.Width, project.Height,
		color.NRGBA{R: 34, G: 42, B: 56, A: 255}, color.NRGBA{R: 238, G: 188, B: 88, A: 255}))
}

func (s *Server) handleAccumulatedImage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	view, ok := s.projects[r.PathValue("id")]
	var result *engine.RunResult
	var width, height int
	if ok {
		result = view.last
		width, height = view.input.Project.Width, view.input.Project.Height
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if result == nil || result.Accumulated == nil {
		writeError(w, http.StatusServiceUnavailable, "accumulated mask is not ready")
		return
	}
	writePNG(w, maskImage(result.Accumulated, width, height,
		color.NRGBA{R: 22, G: 30, B: 42, A: 255}, color.NRGBA{R: 78, G: 214, B: 169, A: 255}))
}

func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	view, ok := s.projects[r.PathValue("id")]
	var data []byte
	if ok {
		data = view.input.IconPNG
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(data)
}

func (s *Server) handleStrokeImage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	view, ok := s.projects[r.PathValue("id")]
	var coverage []byte
	var width, height int
	if ok {
		stroke, found := view.input.Project.StrokeByID(r.PathValue("stroke"))
		if found {
			width, height = view.input.Project.Width, view.input.Project.Height
			var err error
			coverage, err = raster.Coverage(stroke, width, height)
			if err != nil {
				s.mu.Unlock()
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	s.mu.Unlock()
	if !ok || coverage == nil {
		http.NotFound(w, r)
		return
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value := coverage[y*width+x]
			img.SetNRGBA(x, y, color.NRGBA{R: 255, G: 170, B: 60, A: value})
		}
	}
	writePNG(w, img)
}

func (s *Server) handleTruthImage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	view, ok := s.projects[r.PathValue("id")]
	var project *model.Project
	if ok {
		project = view.input.Project
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	domainID := r.PathValue("domain")
	for _, truth := range project.Truth {
		if truth.ID == domainID {
			writePNG(w, maskImage(truth.ReferenceMask, project.Width, project.Height,
				color.NRGBA{R: 22, G: 30, B: 42, A: 255}, color.NRGBA{R: 100, G: 160, B: 255, A: 255}))
			return
		}
	}
	http.NotFound(w, r)
}

func resultIssues(result *engine.RunResult) model.Errors {
	if result == nil {
		return nil
	}
	return result.Issues
}

func resultInvalidation(result *engine.RunResult) *engine.InvalidationReport {
	if result == nil {
		return nil
	}
	return result.Invalidation
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writePNG(w http.ResponseWriter, img image.Image) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_ = png.Encode(w, img)
}

func maskImage(mask []byte, width, height int, background, foreground color.NRGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value := mask[y*width+x]
			t := int(value)
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8((int(background.R)*(255-t) + int(foreground.R)*t) / 255),
				G: uint8((int(background.G)*(255-t) + int(foreground.G)*t) / 255),
				B: uint8((int(background.B)*(255-t) + int(foreground.B)*t) / 255),
				A: 255,
			})
		}
	}
	return img
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func (s *Server) String() string {
	return fmt.Sprintf("maskqc dashboard (%d projects)", len(s.order))
}
