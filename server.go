package maskqc

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"sync"
)

//go:embed web
var webFS embed.FS

type Server struct {
	Root     string
	Store    *CheckpointStore
	mu       sync.Mutex
	projects map[string]*Project
	paths    map[string]string
	results  map[string]*RunResult
}

func EnsureSampleRoot(root string) (*SampleSet, error) {
	projects, _ := filepath.Glob(filepath.Join(root, "*", "project-v3.json"))
	if len(projects) > 0 {
		return nil, nil
	}
	set, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		return nil, err
	}
	if err := set.WriteTo(root); err != nil {
		return nil, err
	}
	return set, nil
}

func NewServer(root, dbPath string) (*Server, error) {
	if _, err := EnsureSampleRoot(root); err != nil {
		return nil, err
	}
	store, err := OpenCheckpoint(dbPath)
	if err != nil {
		return nil, err
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "project-v3.json"))
	if err != nil {
		return nil, err
	}
	s := &Server{
		Root: root, Store: store,
		projects: map[string]*Project{}, paths: map[string]string{},
		results: map[string]*RunResult{},
	}
	for _, path := range paths {
		p, err := LoadProjectFile(path)
		if err != nil {
			return nil, err
		}
		s.projects[p.ID] = p
		s.paths[p.ID] = path
	}
	return s, nil
}

func (s *Server) Close() error { return s.Store.Close() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/project", s.handleProject)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/edit", s.handleEdit)
	mux.HandleFunc("/api/reset", s.handleReset)
	mux.HandleFunc("/api/png", s.handlePNG)
	mux.HandleFunc("/api/component", s.handleComponent)
	mux.HandleFunc("/api/checkpoints", s.handleCheckpoints)
	staticSub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(staticSub)))
	return withJSONHeaders(mux)
}

func withJSONHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if qerr, ok := err.(*QCError); ok {
		switch qerr.Code {
		case CodeCheckpoint:
			status = http.StatusConflict
		case CodeTopology:
			status = http.StatusUnprocessableEntity
		}
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": err.Error(), "structured": err})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.projects))
	for id := range s.projects {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	type summary struct {
		ID          string `json:"id"`
		Width       int    `json:"width"`
		Height      int    `json:"height"`
		Strokes     int    `json:"strokes"`
		Domains     int    `json:"domains"`
		Segments    int    `json:"segments"`
		HasResult   bool   `json:"hasResult"`
		PixelExact  bool   `json:"pixelExact"`
		TopologyOK  bool   `json:"topologyOk"`
		DirectionOK bool   `json:"directionOk"`
	}
	out := make([]summary, 0, len(ids))
	for _, id := range ids {
		p := s.projects[id]
		sum := summary{ID: id, Width: p.Width, Height: p.Height, Strokes: len(p.Strokes), Domains: len(p.Truth.Domains), Segments: len(p.Truth.Segments)}
		if res := s.results[id]; res != nil {
			sum.HasResult = true
			sum.PixelExact = res.PixelExact
			sum.TopologyOK = res.Topology.OK
			sum.DirectionOK = res.Directions.OK
		}
		out = append(out, sum)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "projects": out, "fixedSeed": FixedSeed})
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	if !ok {
		writeAPIError(w, schemaError("$.id", "project %q not found", id))
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"project": map[string]any{
			"id": p.ID, "seed": p.Seed, "width": p.Width, "height": p.Height,
			"schemaVersion": p.SchemaVersion, "strokes": p.Strokes,
			"truth":         map[string]any{"domains": p.Truth.Domains, "segments": p.Truth.Segments},
			"convertReport": p.ConvertReport,
		},
		"tiles": Tiles(p.Width, p.Height, DefaultTileSize),
		"graph": BuildDependencyGraph(p),
	})
}

type verifyRequest struct {
	ProjectID   string `json:"projectId"`
	Mode        string `json:"mode"`
	FailTileID  string `json:"failTileId"`
	PanicTileID string `json:"panicTileId"`
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, schemaError("$", "invalid JSON: %v", err))
		return
	}
	s.mu.Lock()
	p, ok := s.projects[req.ProjectID]
	if !ok {
		s.mu.Unlock()
		writeAPIError(w, schemaError("$.projectId", "project %q not found", req.ProjectID))
		return
	}
	mode := req.Mode
	if mode == "" {
		mode = "incremental"
	}
	var res *RunResult
	var err error
	switch mode {
	case "full":
		res, err = NewEngine(p).RunFullRecompute()
	case "scheduled":
		sched := NewScheduler(2, 256, 4)
		res, err = NewEngine(p).RunScheduled(sched, req.FailTileID, req.PanicTileID)
	case "resume":
		if err = s.Store.ResetProject(p.ID); err == nil {
			engine := NewEngine(p)
			breakAt := len(p.Strokes) / 2
			if breakAt == 0 && len(p.Strokes) > 0 {
				breakAt = 1
			}
			_, err = engine.RunPrefix(s.Store, breakAt)
			if err == nil {
				res, err = NewEngine(p).Resume(s.Store)
			}
		}
	case "incremental":
		if err = s.Store.ResetProject(p.ID); err == nil {
			res, err = NewEngine(p).RunSequential(RunOptions{Path: "incremental", Store: s.Store, Commit: true})
		}
	default:
		err = schemaError("$.mode", "unknown verify mode %q", mode)
	}
	if err != nil {
		s.mu.Unlock()
		if res != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "result": res})
			return
		}
		writeAPIError(w, err)
		return
	}
	s.results[p.ID] = res
	commits, _ := s.Store.Sequence(p.ID)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "result": res, "checkpointSequence": commits})
}

type editRequest struct {
	ProjectID string `json:"projectId"`
	EditRequest
}

func (s *Server) handleEdit(w http.ResponseWriter, r *http.Request) {
	var req editRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, schemaError("$", "invalid JSON: %v", err))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[req.ProjectID]
	if !ok {
		writeAPIError(w, schemaError("$.projectId", "project %q not found", req.ProjectID))
		return
	}
	baseMask := (*Mask)(nil)
	if existing := s.results[req.ProjectID]; existing != nil && existing.Mask != nil {
		baseMask = existing.Mask
	} else {
		baseline, err := NewEngine(p).RunFullRecompute()
		if err != nil {
			writeAPIError(w, err)
			return
		}
		baseMask = baseline.Mask
	}
	plan, err := PlanEdit(p, req.EditRequest)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	delete(s.results, p.ID)
	result, err := NewEngine(p).RecomputeAffected(baseMask, plan.TileIDs)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	s.results[p.ID] = result
	writeJSON(w, 200, map[string]any{"ok": true, "plan": plan, "result": result})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"projectId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	defer s.mu.Unlock()
	path, ok := s.paths[req.ProjectID]
	if !ok {
		writeAPIError(w, schemaError("$.projectId", "project %q not found", req.ProjectID))
		return
	}
	p, err := LoadProjectFile(path)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	s.projects[p.ID] = p
	delete(s.results, p.ID)
	writeJSON(w, 200, map[string]any{"ok": true, "projectId": p.ID})
}

func (s *Server) handlePNG(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("project")
	kind := r.URL.Query().Get("kind")
	s.mu.Lock()
	p, ok := s.projects[id]
	var res *RunResult
	if ok {
		res = s.results[id]
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	var data []byte
	var err error
	switch kind {
	case "image":
		data, err = EncodeImagePNG(p.Image)
	case "candidate":
		data, err = EncodeMaskPNG(p.Candidate)
	case "reference":
		data, err = EncodeMaskPNG(p.Truth.Reference)
	case "accumulated":
		if res == nil || res.Mask == nil {
			res, err = NewEngine(p).RunFullRecompute()
			if err == nil {
				s.mu.Lock()
				s.results[id] = res
				s.mu.Unlock()
			}
		}
		if err == nil {
			data, err = EncodeMaskPNG(res.Mask)
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleComponent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, strokeID, tileID := q.Get("project"), q.Get("stroke"), q.Get("tile")
	s.mu.Lock()
	p, ok := s.projects[id]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	stroke := findStrokeByID(p.Strokes, strokeID)
	if stroke.ID == "" {
		writeAPIError(w, schemaError("$.stroke", "stroke %q not found", strokeID))
		return
	}
	tile, ok := NewEngine(p).tileMap[tileID]
	if !ok {
		writeAPIError(w, schemaError("$.tile", "tile %q not found", tileID))
		return
	}
	comp, err := RasterizeStrokeTile(stroke, tile)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	data, err := EncodeMaskPNG(&Mask{Width: comp.Width, Height: comp.Height, Alpha: comp.Alpha})
	if err != nil {
		writeAPIError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (s *Server) handleCheckpoints(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("project")
	commits, err := s.Store.LoadCommits(id)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	seq, _ := s.Store.Sequence(id)
	writeJSON(w, 200, map[string]any{"ok": true, "commits": commits, "sequence": seq})
}
