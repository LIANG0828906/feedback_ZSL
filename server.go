package maskqa

import (
	"embed"
	"encoding/json"
	"image"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

//go:embed web/index.html
var webFS embed.FS

// HTTPServer serves the local diagnostic workbench.
type HTTPServer struct {
	mu       sync.Mutex
	baseDir  string
	options  SchedulerOptions
	sessions map[int]*Workbench
	projects map[int]*Project
}

// NewHTTPServer creates a local server using repository-local checkpoint files.
func NewHTTPServer(baseDir string) (*HTTPServer, error) {
	if baseDir == "" {
		baseDir = filepath.Join(".maskqa-checkpoints", "web")
	}
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return nil, err
	}
	return &HTTPServer{
		baseDir: baseDir, options: DefaultSchedulerOptions(),
		sessions: map[int]*Workbench{}, projects: map[int]*Project{},
	}, nil
}

// Handler returns the HTTP handler.
func (s *HTTPServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/roundtrip", s.handleRoundtrip)
	return mux
}

// Close releases all checkpoint file locks held by test/in-process servers.
func (s *HTTPServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id, wb := range s.sessions {
		if err := wb.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.sessions, id)
	}
	return firstErr
}

func (s *HTTPServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	data, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

type stateResponse struct {
	SampleIndex    int              `json:"sample_index"`
	Samples        []SampleInfo     `json:"samples"`
	Project        *Project         `json:"project"`
	Result         *ComputeResult   `json:"result"`
	Graph          *DependencyGraph `json:"graph"`
	CandidatePix   []byte           `json:"candidate_pix"`
	ReferencePix   []byte           `json:"reference_pix"`
	AccumulatedPix []byte           `json:"accumulated_pix"`
	IconPNGBase64  string           `json:"icon_png_base64"`
	Recovery       RecoveryReport   `json:"recovery"`
	BusinessError  *Error           `json:"business_error,omitempty"`
}

// SampleInfo describes a deterministic sample in the selector.
type SampleInfo struct {
	Index  int    `json:"index"`
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func (s *HTTPServer) session(index int) (*Project, *Workbench, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wb, ok := s.sessions[index]; ok {
		return s.projects[index], wb, nil
	}
	project, err := GenerateSample(index)
	if err != nil {
		return nil, nil, err
	}
	dir := filepath.Join(s.baseDir, "sample-"+strconv.Itoa(index))
	for _, edit := range loadEditHistory(filepath.Join(dir, "edits.json")) {
		if _, err := ApplyEdit(project, edit); err != nil {
			return nil, nil, err
		}
	}
	wb, err := NewWorkbench(project, dir, s.options)
	if err != nil {
		return nil, nil, err
	}
	var result *ComputeResult
	if wb.RecoveryReport.Action == RecoveryRebuild || wb.RecoveryReport.LastSequence == 0 {
		result, err = wb.InitializeFull()
	} else {
		result, _, err = wb.Recover()
	}
	if err != nil && result == nil {
		return nil, nil, err
	}
	s.projects[index] = project
	s.sessions[index] = wb
	return project, wb, nil
}

func loadEditHistory(path string) []Edit {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var edits []Edit
	if err := json.Unmarshal(data, &edits); err != nil {
		return nil
	}
	return edits
}

func (s *HTTPServer) appendEditHistory(index int, edit Edit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.baseDir, "sample-"+strconv.Itoa(index))
	path := filepath.Join(dir, "edits.json")
	edits := loadEditHistory(path)
	edits = append(edits, edit)
	data, err := json.MarshalIndent(edits, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteJSON(path, data)
}

func (s *HTTPServer) handleState(w http.ResponseWriter, r *http.Request) {
	index := queryInt(r, "sample", 0)
	project, wb, err := s.session(index)
	if err != nil {
		writeError(w, err)
		return
	}
	s.mu.Lock()
	result := wb.Last
	recovery := wb.RecoveryReport
	graph := wb.Graph
	s.mu.Unlock()
	if result == nil {
		result, err = wb.InitializeFull()
		if err != nil && result == nil {
			writeError(w, err)
			return
		}
	}
	iconB64, _ := encodePNG(project.Icon)
	resp := stateResponse{
		SampleIndex:    index,
		Samples:        sampleInfos(),
		Project:        project,
		Result:         result,
		Graph:          graph,
		CandidatePix:   grayPixels(project.Candidate),
		ReferencePix:   grayPixels(project.Reference),
		AccumulatedPix: grayPixels(result.Accumulated),
		IconPNGBase64:  iconB64,
		Recovery:       recovery,
	}
	if biz, ok := AsError(err); ok {
		resp.BusinessError = biz
	} else if result != nil && result.FirstError != nil {
		resp.BusinessError = result.FirstError
	}
	writeJSON(w, http.StatusOK, resp)
}

type editRequest struct {
	SampleIndex     int    `json:"sample_index"`
	StrokeID        string `json:"stroke_id"`
	StartX          *int   `json:"start_x"`
	StartY          *int   `json:"start_y"`
	EndX            *int   `json:"end_x"`
	EndY            *int   `json:"end_y"`
	WeightPermyriad *int   `json:"weight_permyriad"`
	Priority        *int   `json:"priority"`
}

func (s *HTTPServer) handleEdit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req editRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, decodeError("$", "invalid edit JSON: %v", err))
		return
	}
	project, wb, err := s.session(req.SampleIndex)
	if err != nil {
		writeError(w, err)
		return
	}
	edit := Edit{StrokeID: req.StrokeID, WeightPermyriad: req.WeightPermyriad, Priority: req.Priority}
	for _, stroke := range project.Strokes {
		if stroke.ID != req.StrokeID {
			continue
		}
		if req.StartX != nil || req.StartY != nil {
			start := stroke.Start
			if req.StartX != nil {
				start.X = *req.StartX
			}
			if req.StartY != nil {
				start.Y = *req.StartY
			}
			edit.Start = &start
		}
		if req.EndX != nil || req.EndY != nil {
			end := stroke.End
			if req.EndX != nil {
				end.X = *req.EndX
			}
			if req.EndY != nil {
				end.Y = *req.EndY
			}
			edit.End = &end
		}
	}
	result, plan, err := wb.Edit(edit)
	if err != nil && result == nil {
		writeError(w, err)
		return
	}
	if historyErr := s.appendEditHistory(req.SampleIndex, edit); historyErr != nil {
		writeError(w, historyErr)
		return
	}
	if result != nil && plan != nil {
		result.Invalidation = plan
	}
	iconB64, _ := encodePNG(project.Icon)
	resp := stateResponse{
		SampleIndex: req.SampleIndex, Samples: sampleInfos(), Project: project,
		Result: result, Graph: wb.Graph,
		CandidatePix: grayPixels(project.Candidate), ReferencePix: grayPixels(project.Reference),
		AccumulatedPix: grayPixels(result.Accumulated), IconPNGBase64: iconB64,
		Recovery: wb.RecoveryReport,
	}
	if biz, ok := AsError(err); ok {
		resp.BusinessError = biz
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *HTTPServer) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	index := queryInt(r, "sample", 0)
	s.mu.Lock()
	if wb, ok := s.sessions[index]; ok {
		_ = wb.Close()
		delete(s.sessions, index)
		delete(s.projects, index)
	}
	_ = os.RemoveAll(filepath.Join(s.baseDir, "sample-"+strconv.Itoa(index)))
	s.mu.Unlock()
	_, _, err := s.session(index)
	if err != nil {
		writeError(w, err)
		return
	}
	http.Redirect(w, r, "/api/state?sample="+strconv.Itoa(index), http.StatusSeeOther)
}

func (s *HTTPServer) handleExport(w http.ResponseWriter, r *http.Request) {
	index := queryInt(r, "sample", 0)
	version := SchemaV3
	switch r.URL.Query().Get("schema") {
	case "v1":
		version = SchemaV1
	case "v2":
		version = SchemaV2
	case "", "v3":
		version = SchemaV3
	default:
		writeError(w, decodeError("schema", "unknown schema short name"))
		return
	}
	project, _, err := s.session(index)
	if err != nil {
		writeError(w, err)
		return
	}
	exported, err := ExportProject(project, version)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+project.ID+"-"+version+".json")
	_, _ = w.Write(exported.Data)
}

func (s *HTTPServer) handleRoundtrip(w http.ResponseWriter, r *http.Request) {
	index := queryInt(r, "sample", 0)
	version := SchemaV3
	switch r.URL.Query().Get("schema") {
	case "v1":
		version = SchemaV1
	case "v2":
		version = SchemaV2
	case "v3", "":
		version = SchemaV3
	}
	project, _, err := s.session(index)
	if err != nil {
		writeError(w, err)
		return
	}
	exported, err := ExportProject(project, version)
	if err != nil {
		writeError(w, err)
		return
	}
	loaded, err := LoadProject(exported.Data)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schema": version, "source_version": loaded.Project.SourceVersion,
		"lossy": exported.Lossy, "export_conversions": exported.Conversions,
		"traces": loaded.Traces, "load_conversions": loaded.Conversions,
		"stroke_count": len(loaded.Project.Strokes),
		"truth_count":  len(loaded.Project.Truth),
	})
}

func sampleInfos() []SampleInfo {
	specs := SampleSpecs()
	out := make([]SampleInfo, 0, len(specs))
	for i, spec := range specs {
		out = append(out, SampleInfo{Index: i, ID: spec.ID, Width: spec.Width, Height: spec.Height})
	}
	return out
}

func grayPixels(img *image.Gray) []byte {
	if img == nil {
		return nil
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out[y*w+x] = img.GrayAt(x, y).Y
		}
	}
	return out
}

func queryInt(r *http.Request, key string, fallback int) int {
	value := r.URL.Query().Get(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, err error) {
	if biz, ok := AsError(err); ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": biz})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}
