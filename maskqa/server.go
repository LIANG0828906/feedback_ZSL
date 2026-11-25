package maskqa

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
)

//go:embed web/index.html
var webFS embed.FS

type Server struct {
	Set *FixtureSet
	Cfg SchedulerConfig
}

func NewServer() (*Server, error) {
	set, err := GenerateSet(DefaultSeed)
	if err != nil {
		return nil, err
	}
	return &Server{Set: set, Cfg: DefaultSchedulerConfig()}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/fixtures", s.handleFixtures)
	mux.HandleFunc("/api/project", s.handleProject)
	mux.HandleFunc("/api/image", s.handleImage)
	mux.HandleFunc("/api/diagnose", s.handleDiagnose)
	mux.HandleFunc("/api/three-paths", s.handleThreePaths)
	return mux
}

func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.Handler())
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) handleFixtures(w http.ResponseWriter, r *http.Request) {
	type summary struct {
		Index        int               `json:"index"`
		Name         string            `json:"name"`
		ProjectID    string            `json:"project_id"`
		Width        int               `json:"width"`
		Height       int               `json:"height"`
		StrokeCount  int               `json:"stroke_count"`
		TileCount    int               `json:"tile_count"`
		Versions     []string          `json:"versions"`
		Defects      []CandidateDefect `json:"defects"`
		TruthDomains []DomainMetric    `json:"truth_domains"`
		SegmentCount int               `json:"segment_count"`
	}
	out := []summary{}
	for i, fx := range s.Set.Fixtures {
		out = append(out, summary{
			Index: i, Name: fx.Name, ProjectID: fx.Project.ProjectID,
			Width: fx.Project.Width, Height: fx.Project.Height,
			StrokeCount: len(fx.Project.Strokes), TileCount: len(BuildTiles(fx.Project.Width, fx.Project.Height, DefaultTileSize)),
			Versions: []string{SchemaV1, SchemaV2, SchemaV3}, Defects: fx.Defects,
			TruthDomains: fx.Truth.Domains, SegmentCount: len(fx.Truth.Segments),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"seed": s.Set.Seed, "fixtures": out, "scheduler": s.Cfg})
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	fx, ok := s.fixture(w, r)
	if !ok {
		return
	}
	version := r.URL.Query().Get("version")
	if version == "" {
		version = SchemaV3
	}
	res, ok := fx.Exports[version]
	if !ok {
		writeAPIError(w, http.StatusBadRequest, schemaError(version, "version", "unsupported version"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(res.Data)
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	fx, ok := s.fixture(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(fx.Project.ImagePNG)
}

type diagnoseResponse struct {
	SampleIndex      int                 `json:"sample_index"`
	SampleName       string              `json:"sample_name"`
	ProjectID        string              `json:"project_id"`
	Width            int                 `json:"width"`
	Height           int                 `json:"height"`
	TileSize         int                 `json:"tile_size"`
	Tiles            []Tile              `json:"tiles"`
	Candidate        *Mask               `json:"candidate"`
	Truth            *Mask               `json:"truth"`
	Mask             *Mask               `json:"mask"`
	Order            *OrderDecision      `json:"order"`
	Graph            *DepGraph           `json:"graph"`
	Report           *VerificationReport `json:"report"`
	Invalidation     *InvalidationPlan   `json:"invalidation,omitempty"`
	RecomputedTiles  []int               `json:"recomputed_tiles"`
	ReusedTiles      []int               `json:"reused_tiles"`
	CommitSequence   []LogicalCommit     `json:"commit_sequence"`
	SchedulerTrace   *SchedulerTrace     `json:"scheduler_trace,omitempty"`
	EditError        *Error              `json:"edit_error,omitempty"`
	PixelMatchFull   bool                `json:"pixel_match_full"`
	FirstDiffTile    int                 `json:"first_diff_tile"`
	FirstDiffMessage string              `json:"first_diff_message,omitempty"`
}

func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	fx, ok := s.fixture(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	editStroke := queryInt(r, "edit_stroke", 0)
	if editStroke == 0 {
		store, err := OpenCheckpoint(":memory:")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		eng, err := NewEngine(fx.Project, fx.Truth, s.Cfg, store)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		res, err := eng.Run(ctx, RunOptions{})
		_ = store.Close()
		resp := s.baseResponse(fx, eng, res)
		if err != nil {
			if qe, ok := AsError(err); ok {
				resp.EditError = qe
				writeJSON(w, statusForError(qe), resp)
				return
			}
			writeAPIError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	workDir, err := os.MkdirTemp("", "maskqa-web-*")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(workDir)
	dbPath := filepath.Join(workDir, "edit.db")
	store, err := OpenCheckpoint(dbPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	baseEng, err := NewEngine(fx.Project, fx.Truth, s.Cfg, store)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := baseEng.Run(ctx, RunOptions{}); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	_ = store.Close()

	reopened, err := OpenCheckpoint(dbPath)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	defer reopened.Close()

	edited := cloneProject(fx.Project)
	var target *Stroke
	for i := range edited.Strokes {
		if edited.Strokes[i].Index == editStroke {
			target = &edited.Strokes[i]
			break
		}
	}
	if target == nil {
		writeAPIError(w, http.StatusBadRequest, paramError("edit_stroke", "1..5", "stroke %d not found", editStroke))
		return
	}
	dx, dy := queryInt(r, "dx", 0), queryInt(r, "dy", 0)
	switch r.URL.Query().Get("point") {
	case "end":
		target.End.X += dx
		target.End.Y += dy
	case "all":
		target.Start.X += dx
		target.Start.Y += dy
		target.End.X += dx
		target.End.Y += dy
	default:
		target.Start.X += dx
		target.Start.Y += dy
	}
	if dw := queryInt(r, "dw", 0); dw != 0 {
		target.WeightBPS += dw
	}

	plan, err := PlanInvalidation(fx.Project, edited, editStroke, baseEng.Tiles)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	incEng, err := NewEngine(edited, fx.Truth, s.Cfg, reopened)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err)
		return
	}
	inc, incErr := incEng.Run(ctx, RunOptions{Invalidation: plan})
	resp := s.baseResponse(fx, incEng, inc)
	resp.Invalidation = plan
	if inc != nil {
		resp.RecomputedTiles = inc.RecomputedTiles
		resp.ReusedTiles = inc.ReusedTiles
	}
	if incErr != nil {
		if qe, ok := AsError(incErr); ok {
			resp.EditError = qe
		}
	}

	fullEng, err := NewEngine(edited, fx.Truth, s.Cfg, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, resp)
		return
	}
	full, fullErr := fullEng.Run(context.Background(), RunOptions{})
	if fullErr != nil {
		if qe, ok := AsError(fullErr); ok && resp.EditError == nil {
			resp.EditError = qe
		}
	}
	if inc != nil && full != nil && inc.Mask != nil && full.Mask != nil {
		resp.PixelMatchFull = inc.Mask.Equal(full.Mask)
		if !resp.PixelMatchFull {
			tile, _, msg := FirstMaskDifference(inc.Mask, full.Mask, incEng.Tiles)
			resp.FirstDiffTile = tile
			resp.FirstDiffMessage = msg
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleThreePaths(w http.ResponseWriter, r *http.Request) {
	fx, ok := s.fixture(w, r)
	if !ok {
		return
	}
	dir, err := os.MkdirTemp("", "maskqa-paths-*")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(dir)
	report, err := RunThreePaths(r.Context(), fx.Project, fx.Truth, s.Cfg, dir)
	if err != nil && report == nil {
		writeAPIError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": report, "error": errorBody(err)})
}

func (s *Server) fixture(w http.ResponseWriter, r *http.Request) (*Fixture, bool) {
	idx := queryInt(r, "sample", 0)
	if idx < 0 || idx >= len(s.Set.Fixtures) {
		writeAPIError(w, http.StatusBadRequest, paramError("sample", "0.."+strconv.Itoa(len(s.Set.Fixtures)-1), "sample %d not found", idx))
		return nil, false
	}
	return s.Set.Fixtures[idx], true
}

func (s *Server) baseResponse(fx *Fixture, eng *Engine, res *RunResult) diagnoseResponse {
	resp := diagnoseResponse{
		SampleIndex: s.fixtureIndex(fx), SampleName: fx.Name, ProjectID: fx.Project.ProjectID,
		Width: fx.Project.Width, Height: fx.Project.Height, TileSize: DefaultTileSize,
		Tiles: eng.Tiles, Candidate: fx.Project.Candidate, Truth: fx.Truth.Mask,
		Order: eng.Order, Graph: eng.Graph, PixelMatchFull: true, FirstDiffTile: -1,
	}
	if res != nil {
		resp.Mask = res.Mask
		resp.Report = res.Report
		resp.RecomputedTiles = res.RecomputedTiles
		resp.ReusedTiles = res.ReusedTiles
		resp.CommitSequence = LogicalSequence(res.CommitSequence)
		resp.SchedulerTrace = res.SchedulerTrace
	}
	return resp
}

func (s *Server) fixtureIndex(target *Fixture) int {
	for i, fx := range s.Set.Fixtures {
		if fx == target {
			return i
		}
	}
	return 0
}

func queryInt(r *http.Request, key string, def int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeAPIError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": errorBody(err)})
}

func errorBody(err error) any {
	if err == nil {
		return nil
	}
	if qe, ok := AsError(err); ok {
		return qe
	}
	return map[string]string{"code": "E_INTERNAL", "message": err.Error()}
}

func statusForError(qe *Error) int {
	switch qe.Code {
	case CodeSchema, CodePARA, CodeOVF:
		return http.StatusBadRequest
	case CodeTOP, CodeDIR, CodeDIFF, CodeORDER:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func cloneProject(p *Project) *Project {
	c := *p
	c.Strokes = append([]Stroke(nil), p.Strokes...)
	c.ImagePNG = append([]byte(nil), p.ImagePNG...)
	if p.Candidate != nil {
		c.Candidate = p.Candidate.Clone()
	}
	return &c
}
