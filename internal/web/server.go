package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"meshforge/internal/errs"
	"meshforge/internal/generator"
	"meshforge/internal/mesh"
	"meshforge/internal/repair"
	"sort"
)

type Server struct {
	Sample   generator.Sample
	Pipeline *repair.Pipeline
}

type StatePayload struct {
	repair.PipelineState
	SampleID string          `json:"sample_id"`
	Truth    generator.Truth `json:"truth"`
}

type Highlight struct {
	Face int    `json:"face"`
	Kind string `json:"kind"`
}

type MeshPayload struct {
	Which      string       `json:"which"`
	Stage      string       `json:"stage,omitempty"`
	Vertices   [][3]float64 `json:"vertices"`
	Faces      [][3]int     `json:"faces"`
	Highlights []Highlight  `json:"highlights"`
	Stats      *mesh.Stats  `json:"stats,omitempty"`
	Committed  bool         `json:"committed"`
}

func New(sample generator.Sample, cfg repair.Config, checkpointDir string) (*Server, error) {
	p, err := repair.NewPipeline(sample, cfg, checkpointDir, true)
	if err != nil {
		return nil, err
	}
	return &Server{Sample: sample, Pipeline: p}, nil
}

func ValidateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("address %q is not a loopback address", addr)
	}
	return nil
}

func (s *Server) Serve(addr string) error {
	if err := ValidateLoopback(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	fmt.Printf("meshforge UI listening on http://%s\n", ln.Addr().String())
	return srv.Serve(ln)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/mesh", s.handleMesh)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/retry", s.handleRetry)
	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, indexHTML)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodeState, "GET required"))
		return
	}
	writeJSON(w, StatePayload{
		PipelineState: s.Pipeline.State(),
		SampleID:      s.Sample.Truth.ID,
		Truth:         s.Sample.Truth,
	})
}

func (s *Server) handleMesh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodeState, "GET required"))
		return
	}
	which := r.URL.Query().Get("which")
	var (
		m     mesh.Mesh
		ok    bool
		label = which
	)
	switch which {
	case "original":
		m, ok = s.Sample.Mesh, true
	case "repaired":
		m, ok = s.Pipeline.MeshForStage(repair.StageStats)
		label = "repaired"
	case "stage":
		name := repair.StageName(r.URL.Query().Get("stage"))
		label = string(name)
		m, ok = s.Pipeline.MeshForStage(name)
	default:
		writeError(w, http.StatusBadRequest, errs.New(errs.CodeGeom, "unknown mesh view %q", which))
		return
	}
	if !ok {
		writeError(w, http.StatusConflict, errs.New(errs.CodeState, "mesh %s is not committed/available", label))
		return
	}
	payload := MeshPayload{
		Which:    which,
		Stage:    label,
		Vertices: make([][3]float64, len(m.Vertices)),
		Faces:    make([][3]int, len(m.Faces)),
	}
	for i, v := range m.Vertices {
		payload.Vertices[i] = [3]float64{v.X, v.Y, v.Z}
	}
	for i, f := range m.Faces {
		payload.Faces[i] = f.Indices()
	}
	if which == "original" {
		payload.Highlights = s.originalHighlights()
	}
	state := s.Pipeline.State()
	for _, st := range state.Stages {
		if st.Name == repair.StageStats && which == "repaired" {
			payload.Stats = st.Stats
		}
		if which == "stage" && st.Name == repair.StageName(label) {
			payload.Stats = st.Stats
			payload.Committed = st.Status == repair.StatusCommitted
		}
	}
	writeJSON(w, payload)
}

func (s *Server) originalHighlights() []Highlight {
	t := mesh.BuildTopology(s.Sample.Mesh)
	seen := make(map[int]string)
	add := func(face int, kind string) {
		if _, ok := seen[face]; !ok {
			seen[face] = kind
		}
	}
	for _, fi := range s.Sample.Truth.FlippedFaceIDs {
		add(fi, "flipped")
	}
	for _, fi := range s.Sample.Truth.HoleFaceIDs {
		add(fi, "hole")
	}
	for _, fi := range s.Sample.Truth.ThinSheetFaces {
		add(fi, "thin-sheet")
	}
	if s.Sample.Truth.NonManifoldFace >= 0 && s.Sample.Truth.NonManifoldFace < len(s.Sample.Mesh.Faces) {
		add(s.Sample.Truth.NonManifoldFace, "non-manifold")
	}
	for _, e := range t.SortedEdges() {
		if len(e.Uses) > 2 {
			for _, u := range e.Uses {
				add(u.Face, "non-manifold")
			}
		}
	}
	out := make([]Highlight, 0, len(seen))
	for face, kind := range seen {
		out = append(out, Highlight{Face: face, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Face < out[j].Face })
	return out
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodeState, "POST required"))
		return
	}
	if err := s.Pipeline.Run(); err != nil {
		writePipelineError(w, err)
		return
	}
	writeJSON(w, s.Pipeline.State())
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.CodeState, "POST required"))
		return
	}
	var body struct {
		Stage string `json:"stage"`
		Index int    `json:"index"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errs.New(errs.CodeState, "invalid retry body: %v", err))
		return
	}
	index := body.Index
	if body.Stage != "" {
		for i, name := range repair.StageOrder {
			if string(name) == body.Stage {
				index = i
				break
			}
		}
	}
	if err := s.Pipeline.Retry(index); err != nil {
		writePipelineError(w, err)
		return
	}
	if err := s.Pipeline.Run(); err != nil {
		writePipelineError(w, err)
		return
	}
	writeJSON(w, s.Pipeline.State())
}

func writePipelineError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if ce, ok := errs.As(err); ok {
		switch ce.Code {
		case errs.CodeGeom, errs.CodeTolo, errs.CodePara, errs.CodeOrient, errs.CodeScale, errs.CodeState:
			status = http.StatusConflict
		}
		writeError(w, status, ce)
		return
	}
	writeError(w, status, errs.New(errs.CodeIO, "%v", err))
}

func writeError(w http.ResponseWriter, status int, e *errs.CodedError) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
