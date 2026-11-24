package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mediaforge/internal/codec"
	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/paths"
	"mediaforge/internal/project"
	"mediaforge/internal/runner"
	"mediaforge/internal/samplegen"
	"mediaforge/internal/scale"
	"mediaforge/internal/state"
	"mediaforge/internal/store"
	"mediaforge/internal/verifier"
)

//go:embed static/*
var staticFiles embed.FS

type Config struct {
	DataDir string
	DBRoot  string
	Addr    string
}

type Server struct {
	cfg  Config
	mu   sync.Mutex
	opMu sync.RWMutex

	projects   []projectEntry
	current    *selected
	running    bool
	runCancel  context.CancelFunc
	lastRun    *runner.Report
	lastRunErr *errs.Error
	events     []EventRecord

	compareRunning bool
	compareReport  *paths.Comparison
	compareErr     *errs.Error

	verifyRunning bool
	verifyReport  *verifier.Report
	verifyErr     *errs.Error
}

type projectEntry struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

type selected struct {
	Path        string
	Dir         string
	Project     *domain.Project
	Resolved    *domain.ResolvedProject
	Fingerprint string
	Store       *store.Store
}

type EventRecord struct {
	Time    string      `json:"time"`
	Variant string      `json:"variant,omitempty"`
	State   state.State `json:"state,omitempty"`
	Message string      `json:"message"`
}

type ErrorEnvelope struct {
	OK    bool        `json:"ok"`
	Error *errs.Error `json:"error"`
}

func New(cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		cfg.DataDir = "data"
	}
	if cfg.DBRoot == "" {
		cfg.DBRoot = "work"
	}
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8787"
	}
	if err := os.MkdirAll(cfg.DBRoot, 0o755); err != nil {
		return nil, errs.Wrap(errs.EIO, "create database root", err)
	}
	s := &Server{cfg: cfg}
	s.projects = scanProjects(cfg.DataDir)
	if len(s.projects) > 0 {
		if err := s.selectProject(s.projects[0].Path); err != nil {
			s.events = append(s.events, EventRecord{
				Time: now(), Message: "auto-select failed: " + err.Error(),
			})
		}
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFiles, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/projects", s.handleProjects)
	mux.HandleFunc("/api/select", s.handleSelect)
	mux.HandleFunc("/api/state", s.handleState)
	mux.HandleFunc("/api/run", s.handleRun)
	mux.HandleFunc("/api/pause", s.handlePause)
	mux.HandleFunc("/api/recipe", s.handleRecipe)
	mux.HandleFunc("/api/retry", s.handleRetry)
	mux.HandleFunc("/api/export", s.handleExport)
	mux.HandleFunc("/api/source", s.handleSource)
	mux.HandleFunc("/api/verify", s.handleVerify)
	mux.HandleFunc("/api/compare-paths", s.handleCompare)
	return logMiddleware(mux)
}

func (s *Server) ListenAndServe() error {
	return http.ListenAndServe(s.cfg.Addr, s.Handler())
}

func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "GET required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"projects": s.projects})
}

func (s *Server) handleSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errs.Wrap(errs.EDecode, "invalid JSON body", err))
		return
	}
	if err := s.selectProject(body.Path); err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) selectProject(path string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errs.New(errs.EState, "pause or cancel the active batch before switching projects")
	}
	if s.compareRunning || s.verifyRunning {
		s.mu.Unlock()
		return errs.New(errs.EState, "wait for verification or comparison before switching projects")
	}
	s.mu.Unlock()
	abs, err := filepath.Abs(path)
	if err != nil {
		return errs.Wrap(errs.EIO, "resolve project path", err)
	}
	root, _ := filepath.Abs(s.cfg.DataDir)
	if rel, err := filepath.Rel(root, abs); err != nil || strings.HasPrefix(rel, "..") {
		return errs.Field(errs.EPara, "path", "project must be inside the configured data directory")
	}
	p, err := project.Load(abs)
	if err != nil {
		return err
	}
	dir := filepath.Dir(abs)
	rp, err := project.Validate(p, dir)
	if err != nil {
		return err
	}
	fp := project.Fingerprint(p, rp)
	dbPath := filepath.Join(s.cfg.DBRoot, safeName(p.ID)+".sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	if _, err := st.Ensure(context.Background(), p, rp, fp, false); err != nil {
		st.Close()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		_ = s.current.Store.Close()
	}
	s.current = &selected{
		Path: abs, Dir: dir, Project: p, Resolved: rp, Fingerprint: fp, Store: st,
	}
	s.compareReport = nil
	s.compareErr = nil
	s.verifyReport = nil
	s.verifyErr = nil
	s.lastRun = nil
	s.lastRunErr = nil
	s.events = appendEvent(s.events, EventRecord{Time: now(), Message: "selected project " + p.Name})
	return nil
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "GET required"))
		return
	}
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	s.mu.Lock()
	current := s.current
	running := s.running
	lastRun := s.lastRun
	lastRunErr := s.lastRunErr
	events := append([]EventRecord(nil), s.events...)
	compareRunning := s.compareRunning
	compareReport := s.compareReport
	compareErr := s.compareErr
	verifyRunning := s.verifyRunning
	verifyReport := s.verifyReport
	verifyErr := s.verifyErr
	projects := append([]projectEntry(nil), s.projects...)
	s.mu.Unlock()
	stateMap := map[string]any{
		"projects":     projects,
		"running":      running,
		"selected":     nil,
		"lastRun":      lastRun,
		"lastRunError": lastRunErr,
		"events":       events,
		"compare": map[string]any{
			"running": compareRunning, "report": compareReport, "error": compareErr,
		},
		"verify": map[string]any{
			"running": verifyRunning, "report": verifyReport, "error": verifyErr,
		},
	}
	if current == nil {
		writeJSON(w, http.StatusOK, stateMap)
		return
	}
	recs, err := current.Store.Variants(r.Context())
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	progress, err := current.Store.Progress(r.Context())
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	stalls, _ := current.Store.Stalls(r.Context(), 30)
	invalidations, _ := current.Store.Invalidations(r.Context())
	decisions := scale.Decide(current.Resolved)
	stateMap["selected"] = buildSelectedView(current, recs, progress, stalls, invalidations, decisions)
	writeJSON(w, http.StatusOK, stateMap)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	var body struct {
		Mode       runner.Mode `json:"mode"`
		Workers    int         `json:"workers"`
		PauseAfter int         `json:"pauseAfter"`
		ForcePath  string      `json:"forcePath"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errs.Wrap(errs.EDecode, "invalid JSON body", err))
		return
	}
	if body.Mode == "" {
		body.Mode = runner.ModeRealtime
	}
	err := s.startRun(runner.Options{
		Mode: body.Mode, Workers: body.Workers, PauseAfter: body.PauseAfter, ForcePath: body.ForcePath,
	})
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "accepted": true})
}

func (s *Server) startRun(opts runner.Options) error {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.startRunLocked(opts)
}

func (s *Server) startRunLocked(opts runner.Options) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errs.New(errs.EState, "a batch is already running")
	}
	current := s.current
	if current == nil {
		s.mu.Unlock()
		return errs.New(errs.EDecode, "no project selected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.running = true
	s.runCancel = cancel
	s.lastRun = nil
	s.lastRunErr = nil
	s.events = appendEvent(s.events, EventRecord{Time: now(), Message: "batch " + string(opts.Mode) + " started"})
	s.mu.Unlock()
	r := runner.New(current.Store, current.Project, current.Resolved, current.Fingerprint, current.Dir)
	r.Emit = func(ev runner.Event) {
		s.mu.Lock()
		s.events = appendEvent(s.events, EventRecord{
			Time: now(), Variant: ev.Variant, State: ev.State, Message: ev.Message,
		})
		s.mu.Unlock()
	}
	go func() {
		rep, err := r.Run(ctx, opts)
		cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		s.running = false
		s.runCancel = nil
		s.lastRun = rep
		if err != nil {
			s.lastRunErr = errs.As(err)
		}
		msg := "batch finished"
		if rep != nil && rep.Paused {
			msg = "batch paused with committed checkpoints"
		} else if err != nil {
			msg = "batch failed: " + err.Error()
		}
		s.events = appendEvent(s.events, EventRecord{Time: now(), Message: msg})
	}()
	return nil
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	s.mu.Lock()
	if !s.running || s.runCancel == nil {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, errs.New(errs.EState, "no batch is running"))
		return
	}
	cancel := s.runCancel
	s.events = appendEvent(s.events, EventRecord{Time: now(), Message: "pause requested"})
	s.mu.Unlock()
	cancel()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleRecipe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	var body struct {
		Variant string          `json:"variant"`
		Field   string          `json:"field"`
		Value   json.RawMessage `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errs.Wrap(errs.EDecode, "invalid JSON body", err))
		return
	}
	if body.Variant == "" {
		writeError(w, http.StatusBadRequest, errs.Field(errs.EDecode, "variant", "variant is required"))
		return
	}
	if body.Field == "" || len(body.Value) == 0 {
		writeError(w, http.StatusBadRequest, errs.Field(errs.EDecode, "field", "field and value are required"))
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, errs.New(errs.EState, "pause the batch before editing a recipe"))
		return
	}
	current := s.current
	s.mu.Unlock()
	if current == nil {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "no project selected"))
		return
	}
	rec, err := current.Store.Get(r.Context(), body.Variant)
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	if rec.State == state.Committed {
		writeError(w, http.StatusConflict, errs.New(errs.EState, "committed variant is frozen"))
		return
	}
	idx := -1
	for i := range current.Resolved.Variants {
		if current.Resolved.Variants[i].ID == body.Variant {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "variant not found"))
		return
	}
	v := current.Resolved.Variants[idx]
	recipe := v.Recipe.Clone()
	if domain.NormalizeRecipeField(body.Field) == "" {
		writeError(w, http.StatusBadRequest, errs.Field(errs.EPara, "field", "unknown recipe field"))
		return
	}
	if err := domain.ApplyRecipeField(&recipe, body.Field, body.Value); err != nil {
		writeError(w, http.StatusBadRequest, errs.Wrap(errs.EPara, "invalid recipe value", err))
		return
	}
	if err := project.ValidateRecipe(recipe, "recipe"); err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	img := current.Resolved.Sources[v.ImageSourceID]
	aud := current.Resolved.Sources[v.AudioSourceID]
	out, err := codec.Transcode(codec.Input{
		ImagePath:     filepath.Join(current.Dir, filepath.FromSlash(img.Path)),
		AudioPath:     filepath.Join(current.Dir, filepath.FromSlash(aud.Path)),
		ImageChannels: img.Channels, AudioChannels: aud.AudioChannels,
		ImageMetadata: img.Metadata, AudioMetadata: aud.Metadata,
	}, recipe, codec.Options{Path: codec.PathFine})
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	v.Recipe = recipe
	v.RecipeVersion = project.RecipeVersion(recipe)
	v.InputHash = project.VariantInputHash(v, current.Resolved.Sources)
	truth := domain.TruthVariant{
		ID: v.ID, Width: out.Header.Width, Height: out.Header.Height,
		Channels: out.Header.ImageChannels, PixelCount: out.Header.PixelCount,
		AudioSamples:   out.Header.AudioSamples,
		MetadataFields: append([]string(nil), out.MetadataFields...),
		Fingerprint:    out.SHA256, SizeBytes: int64(len(out.Bytes)),
	}
	if err := current.Store.UpdatePending(r.Context(), v.ID, recipe, v.RecipeVersion, v.InputHash, truth); err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	s.mu.Lock()
	current.Resolved.Variants[idx] = v
	replaceTruth(&current.Project.Truth, truth)
	s.events = appendEvent(s.events, EventRecord{
		Time: now(), Variant: v.ID, Message: fmt.Sprintf("recipe field %s updated for pending variant", body.Field),
	})
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "variant": v.ID, "recipe": recipe, "truth": truth,
	})
}

func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	var body struct {
		Variant string `json:"variant"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errs.Wrap(errs.EDecode, "invalid JSON body", err))
		return
	}
	if body.Variant == "" {
		writeError(w, http.StatusBadRequest, errs.Field(errs.EDecode, "variant", "variant is required"))
		return
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, errs.New(errs.EState, "batch is already running"))
		return
	}
	current := s.current
	s.mu.Unlock()
	if current == nil {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "no project selected"))
		return
	}
	if err := current.Store.Retry(r.Context(), body.Variant); err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	if err := s.startRunLocked(runner.Options{Mode: runner.ModeResume, Only: []string{body.Variant}}); err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "retrying": body.Variant})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if current == nil {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "no project selected"))
		return
	}
	id := r.URL.Query().Get("variant")
	rec, err := current.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, errStatus(err), err)
		return
	}
	if rec.State != state.Committed || len(rec.OutputBlob) == 0 {
		writeError(w, http.StatusConflict, errs.New(errs.EState, "variant has no committed output"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", safeName(rec.ID)+".mfx"))
	w.Header().Set("X-Content-SHA256", rec.OutputSHA256)
	_, _ = w.Write(rec.OutputBlob)
}

func (s *Server) handleSource(w http.ResponseWriter, r *http.Request) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	s.mu.Lock()
	current := s.current
	s.mu.Unlock()
	if current == nil {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "no project selected"))
		return
	}
	id := r.URL.Query().Get("id")
	src, ok := current.Resolved.Sources[id]
	if !ok {
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "source not found"))
		return
	}
	path := filepath.Join(current.Dir, filepath.FromSlash(src.Path))
	rel, err := filepath.Rel(current.Dir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		writeError(w, http.StatusBadRequest, errs.New(errs.EPara, "invalid source path"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if ct := mime.TypeByExtension(filepath.Ext(path)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeFile(w, r, path)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	s.mu.Lock()
	if s.verifyRunning {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, errs.New(errs.EState, "verification is already running"))
		return
	}
	current := s.current
	if current == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "no project selected"))
		return
	}
	s.verifyRunning = true
	s.verifyReport = nil
	s.verifyErr = nil
	s.mu.Unlock()
	go func() {
		v := &verifier.Verifier{Store: current.Store}
		report, err := v.Verify(context.Background())
		s.mu.Lock()
		defer s.mu.Unlock()
		s.verifyRunning = false
		s.verifyReport = report
		if err != nil {
			s.verifyErr = errs.As(err)
		}
		s.events = appendEvent(s.events, EventRecord{Time: now(), Message: "independent verification completed"})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleCompare(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errs.New(errs.EPara, "POST required"))
		return
	}
	var body struct {
		Workers    int `json:"workers"`
		PauseAfter int `json:"pauseAfter"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	s.mu.Lock()
	if s.compareRunning {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, errs.New(errs.EState, "comparison is already running"))
		return
	}
	current := s.current
	if current == nil {
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, errs.New(errs.EDecode, "no project selected"))
		return
	}
	s.compareRunning = true
	s.compareReport = nil
	s.compareErr = nil
	s.mu.Unlock()
	go func() {
		rep, err := paths.Compare(context.Background(), current.Path, body.Workers, body.PauseAfter)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.compareRunning = false
		s.compareReport = rep
		if err != nil {
			s.compareErr = errs.As(err)
		}
		s.events = appendEvent(s.events, EventRecord{Time: now(), Message: "three-path comparison completed"})
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

func buildSelectedView(current *selected, recs []store.VariantRecord, progress store.Progress,
	stalls []store.StallRecord, invalidations []map[string]string, decisions scale.BatchDecision) map[string]any {
	variants := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		var recipe any
		var decision any
		var truth any
		var summary any
		_ = json.Unmarshal([]byte(rec.RecipeJSON), &recipe)
		_ = json.Unmarshal([]byte(rec.DecisionJSON), &decision)
		_ = json.Unmarshal([]byte(rec.TruthJSON), &truth)
		_ = json.Unmarshal([]byte(rec.SummaryJSON), &summary)
		variants = append(variants, map[string]any{
			"id": rec.ID, "ordinal": rec.Ordinal, "state": rec.State,
			"errorCode": rec.ErrorCode, "errorMessage": rec.ErrorMessage,
			"path": rec.Path, "blockRows": rec.BlockRows, "downsampleFactor": rec.DownsampleFactor,
			"decision": decision, "recipe": recipe, "truth": truth, "summary": summary,
			"outputSha256": rec.OutputSHA256, "outputSize": rec.OutputSize,
			"metadataFields": rec.MetadataFields, "commitSeq": rec.CommitSeq,
			"committedAt": rec.CommittedAt, "recipeVersion": rec.RecipeVersion,
			"inputHash": rec.InputHash, "algorithmVersion": rec.AlgorithmVersion,
		})
	}
	return map[string]any{
		"path": current.Path, "dir": current.Dir, "project": current.Project,
		"fingerprint": current.Fingerprint, "variants": variants, "progress": progress,
		"scale": decisions, "stalls": stalls, "invalidations": invalidations,
		"sourceMetadata": collectSourceMetadata(current.Resolved),
	}
}

func collectSourceMetadata(rp *domain.ResolvedProject) map[string]map[string]string {
	out := map[string]map[string]string{}
	for id, s := range rp.Sources {
		out[id] = s.Metadata
	}
	return out
}

func replaceTruth(t *domain.TruthManifest, truth domain.TruthVariant) {
	for i := range t.Variants {
		if t.Variants[i].ID == truth.ID {
			t.Variants[i] = truth
			return
		}
	}
	t.Variants = append(t.Variants, truth)
	sort.Slice(t.Variants, func(i, j int) bool { return t.Variants[i].ID < t.Variants[j].ID })
}

func scanProjects(root string) []projectEntry {
	files, _ := samplegen.FindProjects(root)
	var out []projectEntry
	for _, path := range files {
		p, err := project.Load(path)
		if err != nil {
			continue
		}
		out = append(out, projectEntry{ID: p.ID, Name: p.Name, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func appendEvent(events []EventRecord, ev EventRecord) []EventRecord {
	events = append(events, ev)
	if len(events) > 200 {
		events = events[len(events)-200:]
	}
	return events
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, ErrorEnvelope{OK: false, Error: errs.As(err)})
}

func errStatus(err error) int {
	switch errs.CodeOf(err) {
	case errs.EPara, errs.EDecode:
		return http.StatusBadRequest
	case errs.EState:
		return http.StatusConflict
	case errs.EDiff, errs.EScale, errs.EVerify:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = io.NopCloser(io.LimitReader(r.Body, 32<<20))
		}
		next.ServeHTTP(w, r)
	})
}
