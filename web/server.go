package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"

	"maskqc"
)

type Server struct {
	mu      sync.RWMutex
	project *maskqc.Project
	store   *maskqc.Store
	engine  *maskqc.Engine
	budget  maskqc.BudgetConfig
	dbPath  string
	workDir string

	iconID        string
	results       map[string]*maskqc.IconResult
	graphs        map[string]*maskqc.DependencyGraph
	invalidations []*maskqc.InvalidationReport
	insertCount   int

	threeReport *maskqc.ThreePathReport
	threeErr    error

	mux *http.ServeMux
}

func NewServer(workDir, dbPath string, budget maskqc.BudgetConfig) (*Server, error) {
	if budget.MaxParallel == 0 {
		budget = maskqc.DefaultBudgetConfig()
	}
	project, err := maskqc.HarnessProject(maskqc.DefaultSeed)
	if err != nil {
		return nil, err
	}
	if dbPath == "" {
		dbPath = filepath.Join(workDir, "workbench-checkpoint.db")
	}
	store, err := maskqc.OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	meta := maskqc.RecoveryMeta{
		ProjectHash:       maskqc.ProjectIdentityHash(project),
		OrderVersion:      maskqc.OrderRulesVersion,
		ConversionVersion: maskqc.ConversionParamsVersion,
		AlgorithmVersion:  maskqc.AlgorithmVersion,
		TopologyVersion:   maskqc.AlgorithmVersion,
	}
	if err := store.Rebuild(meta); err != nil {
		_ = store.Close()
		return nil, err
	}
	s := &Server{
		project: project, store: store, dbPath: dbPath, workDir: workDir, budget: budget,
		engine: maskqc.NewEngine(store, budget, project.ConvertTrace),
		iconID: maskqc.HarnessIconID, results: map[string]*maskqc.IconResult{},
		graphs: map[string]*maskqc.DependencyGraph{},
	}
	if err := s.commitInitial(context.Background()); err != nil {
		return nil, err
	}
	if _, err := s.ensureResultLocked(context.Background(), s.iconID); err != nil {
		return nil, err
	}
	s.runThreePaths()
	s.routes()
	return s, nil
}

func (s *Server) commitInitial(ctx context.Context) error {
	for _, icon := range s.project.Icons {
		order := s.project.SortedStrokes(icon.ID)
		for i, stroke := range order {
			if _, err := s.engine.CommitStrokeComponents(ctx, s.project, icon, stroke, i+1, i+1); err != nil {
				return err
			}
		}
		result, err := s.engine.BuildIconResult(ctx, icon, true)
		if err != nil {
			return err
		}
		s.results[icon.ID] = result
		s.graphs[icon.ID] = maskqc.BuildDependencyGraph(icon, order)
	}
	return nil
}

func (s *Server) runThreePaths() {
	report, err := maskqc.RunThreePaths(context.Background(), maskqc.ThreePathOptions{
		WorkDir: s.workDir, Budget: s.budget, CrashAfter: 1, Seed: maskqc.DefaultSeed,
	})
	s.threeReport = report
	s.threeErr = err
}

func (s *Server) routes() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/mask.png", s.handlePNG)
	mux.HandleFunc("/edit/weight", s.handleWeight)
	mux.HandleFunc("/edit/nudge", s.handleNudge)
	mux.HandleFunc("/edit/insert", s.handleInsert)
	mux.HandleFunc("/reset", s.handleReset)
	mux.HandleFunc("/fail-tile", s.handleFailTile)
	mux.HandleFunc("/retry-tile", s.handleRetryTile)
	mux.HandleFunc("/corrupt/schema", s.handleCorruptSchema)
	mux.HandleFunc("/corrupt/checkpoint", s.handleCorruptCheckpoint)
	mux.HandleFunc("/api/budget-demo", s.handleBudgetDemo)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/export", s.handleExport)
	s.mux = mux
}

func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) ListenAndServe(addr string) error {
	fmt.Printf("maskqc workbench: http://%s\n", addr)
	return http.ListenAndServe(addr, s.mux)
}

func (s *Server) selectIconID(r *http.Request) string {
	queryID := r.URL.Query().Get("icon")
	formID := ""
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		formID = r.FormValue("icon_id")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if queryID != "" && s.project.Icon(queryID) != nil {
		return queryID
	}
	if formID != "" && s.project.Icon(formID) != nil {
		return formID
	}
	return s.iconID
}

func (s *Server) Close() error { return s.store.Close() }

func (s *Server) ensureResultLocked(ctx context.Context, iconID string) (*maskqc.IconResult, error) {
	icon := s.project.Icon(iconID)
	if icon == nil {
		return nil, maskqc.NewSchemaError("icon_id", "unknown icon %q", iconID)
	}
	result, err := s.engine.BuildIconResult(ctx, icon, true)
	if err != nil {
		return result, err
	}
	s.results[iconID] = result
	s.graphs[iconID] = maskqc.BuildDependencyGraph(icon, s.project.SortedStrokes(iconID))
	s.iconID = iconID
	return result, nil
}

func (s *Server) mutateAndRender(w http.ResponseWriter, r *http.Request, edit maskqc.Edit) {
	iconID := s.selectIconID(r)
	flash, status := s.applyEdit(r.Context(), iconID, edit)
	http.Redirect(w, r, "/?icon="+iconID+"&flash="+flash+"&status="+status, http.StatusSeeOther)
}

func (s *Server) applyEdit(ctx context.Context, iconID string, edit maskqc.Edit) (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	icon := s.project.Icon(iconID)
	old, current, err := maskqc.ApplyEdit(s.project, iconID, edit)
	if err != nil {
		return urlFlash(err.Error()), "error"
	}
	inv := maskqc.PlanInvalidation(icon, edit, old, current)
	s.invalidations = append(s.invalidations, inv)
	last, err := s.store.LastCommit(iconID)
	if err != nil {
		return urlFlash(err.Error()), "error"
	}
	ordinal := len(s.project.SortedStrokes(iconID))
	if last != nil && int(last.OrderIndex) > ordinal {
		ordinal = int(last.OrderIndex)
	}
	if last != nil {
		ordinal = int(last.OrderIndex) + 1
	}
	rank := 0
	for i, st := range s.project.SortedStrokes(iconID) {
		if st.ID == current.ID {
			rank = i + 1
		}
	}
	if _, err := s.engine.CommitStrokeComponents(ctx, s.project, icon, current, rank, ordinal); err != nil {
		return urlFlash(err.Error()), "error"
	}
	if _, err := s.ensureResultLocked(ctx, iconID); err != nil {
		if de, ok := maskqc.AsError(err); ok && de.Code == maskqc.EBudget {
			return urlFlash(err.Error()), "error"
		}
		return urlFlash(err.Error()), "error"
	}
	return urlFlash(inv.ImpactScope), "ok"
}

func (s *Server) handleWeight(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	weight, err := strconv.Atoi(r.FormValue("weight"))
	if err != nil {
		http.Redirect(w, r, "/?status=error&flash="+urlFlash("weight must be integer"), http.StatusSeeOther)
		return
	}
	id := r.FormValue("stroke_id")
	v := weight
	s.mutateAndRender(w, r, maskqc.Edit{Type: maskqc.EditUpdate, StrokeID: id, NewWeight: &v})
}

func (s *Server) handleNudge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	dx, _ := strconv.Atoi(r.FormValue("dx"))
	dy, _ := strconv.Atoi(r.FormValue("dy"))
	id := r.FormValue("stroke_id")
	s.mu.RLock()
	icon := s.project.Icon(s.selectIconID(r))
	var stroke *maskqc.Stroke
	if icon != nil {
		for _, st := range icon.Strokes {
			if st.ID == id {
				stroke = st
			}
		}
	}
	s.mu.RUnlock()
	if stroke == nil {
		http.Redirect(w, r, "/?status=error&flash="+urlFlash("stroke not found"), http.StatusSeeOther)
		return
	}
	start := maskqc.Point{X: stroke.Start.X + dx, Y: stroke.Start.Y + dy}
	end := maskqc.Point{X: stroke.End.X + dx, Y: stroke.End.Y + dy}
	s.mutateAndRender(w, r, maskqc.Edit{Type: maskqc.EditUpdate, StrokeID: id, NewStart: &start, NewEnd: &end})
}

func (s *Server) handleInsert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	x1, _ := strconv.Atoi(r.FormValue("x1"))
	y1, _ := strconv.Atoi(r.FormValue("y1"))
	x2, _ := strconv.Atoi(r.FormValue("x2"))
	y2, _ := strconv.Atoi(r.FormValue("y2"))
	radius, _ := strconv.Atoi(r.FormValue("radius"))
	if radius == 0 {
		radius = 1
	}
	weight, _ := strconv.Atoi(r.FormValue("weight"))
	if weight == 0 {
		weight = 10000
	}
	s.mu.Lock()
	s.insertCount++
	n := s.insertCount
	s.mu.Unlock()
	stroke := &maskqc.Stroke{
		ID: fmt.Sprintf("web-insert-%02d", n), Seq: 100 + n, Priority: 50, Group: "interactive",
		Start: maskqc.Point{X: x1, Y: y1}, End: maskqc.Point{X: x2, Y: y2}, Radius: radius,
		HardnessPermille: maskqc.HardnessScale, WeightPermyriad: weight,
		DeclaredDirection: maskqc.DirAlong, CompositionOrder: maskqc.DefaultCompositionOrder,
	}
	s.mutateAndRender(w, r, maskqc.Edit{Type: maskqc.EditInsert, Insert: stroke, StrokeID: stroke.ID})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	project, err := maskqc.HarnessProject(maskqc.DefaultSeed)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.project = project
	meta := metaForServer(project)
	if err := s.store.Rebuild(meta); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.engine = maskqc.NewEngine(s.store, s.budget, project.ConvertTrace)
	s.results = map[string]*maskqc.IconResult{}
	s.graphs = map[string]*maskqc.DependencyGraph{}
	s.invalidations = nil
	s.insertCount = 0
	if err := s.commitInitial(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := s.ensureResultLocked(r.Context(), s.iconID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/?reset=1", http.StatusSeeOther)
}

func (s *Server) handleFailTile(w http.ResponseWriter, r *http.Request) {
	tileID := r.URL.Query().Get("tile")
	iconID := s.selectIconID(r)
	s.mu.Lock()
	if tileID != "" {
		s.engine.FailTileOnce(tileID)
	}
	result, err := s.ensureResultLocked(r.Context(), iconID)
	s.mu.Unlock()
	if err != nil {
		msg := err.Error()
		if result != nil && len(result.TileErrors) > 0 && result.TileErrors[0].Error != nil {
			msg += "; first=" + result.TileErrors[0].TileID + ":" + result.TileErrors[0].Error.Error()
		}
		http.Redirect(w, r, "/?icon="+iconID+"&status=error&flash="+urlFlash(msg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?icon="+iconID+"&status=ok&flash="+urlFlash("failure injection did not trigger; check tile id"), http.StatusSeeOther)
}

func (s *Server) handleRetryTile(w http.ResponseWriter, r *http.Request) {
	iconID := s.selectIconID(r)
	s.mu.Lock()
	_, err := s.ensureResultLocked(r.Context(), iconID)
	s.mu.Unlock()
	if err != nil {
		http.Redirect(w, r, "/?icon="+iconID+"&status=error&flash="+urlFlash(err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/?icon="+iconID+"&status=ok&flash="+urlFlash("tile retry completed; isolated failure cleared"), http.StatusSeeOther)
}

func (s *Server) handleCorruptSchema(w http.ResponseWriter, r *http.Request) {
	bad := []byte(`{"schema":"icon-project/v1","images":[{"width":32}]}`)
	_, err := maskqc.LoadProject(bad)
	flash := "unexpected success"
	status := "error"
	if err != nil {
		flash = err.Error()
		status = "ok"
	}
	http.Redirect(w, r, "/?corrupt=schema&status="+status+"&flash="+urlFlash(flash), http.StatusSeeOther)
}

func (s *Server) handleCorruptCheckpoint(w http.ResponseWriter, r *http.Request) {
	msg, status := s.demoCorruptCheckpoint(r.Context())
	http.Redirect(w, r, "/?corrupt=checkpoint&status="+status+"&flash="+urlFlash(msg), http.StatusSeeOther)
}

func (s *Server) demoCorruptCheckpoint(ctx context.Context) (string, string) {
	path := filepath.Join(s.workDir, "corruption-demo.db")
	store, err := maskqc.OpenStore(path)
	if err != nil {
		return err.Error(), "error"
	}
	defer store.Close()
	meta := maskqc.RecoveryMeta{ProjectHash: "demo", OrderVersion: maskqc.OrderRulesVersion, ConversionVersion: maskqc.ConversionParamsVersion, AlgorithmVersion: maskqc.AlgorithmVersion}
	if err := store.Rebuild(meta); err != nil {
		return err.Error(), "error"
	}
	_, err = store.CommitStroke(ctx, maskqc.StrokeCommit{
		EventKey: "demo-1", IconID: "demo", StrokeID: "s", OrderIndex: 1,
		Components: []maskqc.ComponentRecord{{TileID: "t", StrokeID: "s", CacheKey: "demo-key", ComponentHash: maskqc.HashBytes([]byte("abc")), InputHash: "in", ResidualDigest: "rd", OrderVersion: meta.OrderVersion, ConversionVersion: meta.ConversionVersion, AlgorithmVersion: meta.AlgorithmVersion, Width: 1, Height: 1, Alpha: []byte("abc")}},
	})
	if err != nil {
		return err.Error(), "error"
	}
	if err := store.CorruptComponent(); err != nil {
		return err.Error(), "error"
	}
	err = store.ValidateRecovery(meta)
	if de, ok := maskqc.AsError(err); ok && de.Code == maskqc.ECorrupt {
		return "recovery rejected as expected: " + err.Error(), "ok"
	}
	return "expected E_CORRUPT, got " + fmt.Sprint(err), "error"
}

func (s *Server) handleBudgetDemo(w http.ResponseWriter, r *http.Request) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	sched := maskqc.NewScheduler(maskqc.BudgetConfig{MaxParallel: 1, MaxPixelsPerWorker: 32, MaxQueuedTiles: 1})
	jobs := []maskqc.TileJob{
		{ID: "demo-blocked", Pixels: 1, Run: func(context.Context) error { started <- struct{}{}; <-release; return nil }},
		{ID: "demo-waiting", Pixels: 1, Run: func(context.Context) error { return nil }},
	}
	done := make(chan struct{})
	go func() { _ = sched.Run(r.Context(), jobs); close(done) }()
	<-started
	close(release)
	<-done
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": "ok", "budget": sched.Snapshot()})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"project":       s.project.Name,
		"icon":          s.iconID,
		"budget":        s.engine.SchedulerSnapshot(),
		"invalidations": s.invalidations,
		"three_error":   fmt.Sprint(s.threeErr),
	})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	version := r.URL.Query().Get("schema")
	if version == "" {
		version = maskqc.SchemaV3
	}
	s.mu.RLock()
	data, report, err := maskqc.ExportProject(s.project, version)
	s.mu.RUnlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+version+".json\"")
	_ = json.NewEncoder(w).Encode(map[string]any{"report": report, "document": json.RawMessage(data)})
}

func metaForServer(p *maskqc.Project) maskqc.RecoveryMeta {
	return maskqc.RecoveryMeta{ProjectHash: maskqc.ProjectIdentityHash(p), OrderVersion: maskqc.OrderRulesVersion, ConversionVersion: maskqc.ConversionParamsVersion, AlgorithmVersion: maskqc.AlgorithmVersion, TopologyVersion: maskqc.AlgorithmVersion}
}

func urlFlash(s string) string {
	return url.QueryEscape(s)
}
