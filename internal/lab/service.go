package lab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/fontlib"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/pipeline"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/synth"
)

type CallResponse struct {
	OK    bool             `json:"ok"`
	Data  json.RawMessage  `json:"data,omitempty"`
	Error *domain.AppError `json:"error,omitempty"`
}

type RunHandle struct {
	JobID    string        `json:"job_id"`
	RunID    int64         `json:"run_id,omitempty"`
	SampleID string        `json:"sample_id"`
	Mode     string        `json:"mode"`
	Status   string        `json:"status"`
	Params   domain.Params `json:"params"`
}

type StartRunRequest struct {
	SampleID string        `json:"sample_id"`
	Params   domain.Params `json:"params"`
	Mode     string        `json:"mode"`
}

type session struct {
	jobID       string
	sampleID    string
	mode        string
	params      domain.Params
	control     *pipeline.Control
	ctx         context.Context
	cancel      context.CancelFunc
	running     bool
	runID       int64
	error       *domain.AppError
	invalidated []string
	startedAt   time.Time
}

type Service struct {
	mu        sync.Mutex
	wg        sync.WaitGroup
	closeOnce sync.Once
	ctx       context.Context
	cancelAll context.CancelFunc
	store     *store.Store
	sampleDir string
	dbPath    string
	seed      int64
	params    domain.Params
	manifest  domain.Manifest
	samples   map[string]domain.SampleMeta
	sessions  map[string]*session
	bySample  map[string]*session
	counter   atomic.Uint64
	desktop   bool
	Emit      func(event string, data any)
}

func New(sampleDir, dbPath string, seed int64, desktop bool) (*Service, *domain.AppError) {
	if seed == 0 {
		seed = domain.DefaultBaseSeed
	}
	manifest, appErr := synth.Ensure(sampleDir, seed)
	if appErr != nil {
		return nil, appErr
	}
	db, err := store.Open(dbPath)
	if err != nil {
		return nil, domain.Errorf(domain.CodeStore, "cannot open checkpoint database: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		ctx:       ctx,
		cancelAll: cancel,
		store:     db,
		sampleDir: sampleDir,
		dbPath:    dbPath,
		seed:      seed,
		params:    domain.DefaultParams(fontlib.Version),
		manifest:  manifest,
		samples:   make(map[string]domain.SampleMeta, len(manifest.Samples)),
		sessions:  map[string]*session{},
		bySample:  map[string]*session{},
		desktop:   desktop,
	}
	for _, sample := range manifest.Samples {
		s.samples[sample.ID] = sample
	}
	return s, nil
}

func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		for _, sess := range s.sessions {
			if sess.cancel != nil {
				sess.cancel()
			}
		}
		s.mu.Unlock()
		s.cancelAll()
		s.wg.Wait()
		_ = s.store.Close()
	})
}

func (s *Service) Bootstrap() (domain.Bootstrap, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bootstrap := domain.Bootstrap{
		Manifest:         s.manifest,
		Defaults:         s.params,
		AlgorithmVersion: domain.AlgorithmVersion,
		TemplateVersion:  fontlib.Version,
		DBPath:           s.dbPath,
		SampleDirectory:  s.sampleDir,
		Desktop:          s.desktop,
	}
	if len(s.manifest.Samples) > 0 {
		latest, found, appErr := s.store.LatestRun(s.manifest.Samples[0].ID)
		if appErr != nil {
			return bootstrap, appErr
		}
		if found {
			sample, loadErr := imaging.LoadSample(s.sampleDir, latest.SampleID)
			if loadErr == nil {
				snapshot, snapErr := s.store.Snapshot(latest.ID)
				if snapErr == nil {
					view := pipeline.BuildRunView(sample, snapshot, nil)
					bootstrap.LatestRun = &view
				}
			}
		}
	}
	return bootstrap, nil
}

func (s *Service) GenerateSamples() (domain.Manifest, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	manifest, appErr := synth.Generate(s.sampleDir, s.seed)
	if appErr != nil {
		return domain.Manifest{}, appErr
	}
	s.manifest = manifest
	s.samples = make(map[string]domain.SampleMeta, len(manifest.Samples))
	for _, sample := range manifest.Samples {
		s.samples[sample.ID] = sample
	}
	return manifest, nil
}

func (s *Service) LatestView(sampleID string) (domain.RunView, bool, *domain.AppError) {
	s.mu.Lock()
	_, ok := s.samples[sampleID]
	active := s.bySample[sampleID]
	activeRunning := active != nil && active.running
	s.mu.Unlock()
	if !ok {
		return domain.RunView{}, false, domain.DecodeError("sample is not present in the manifest", nil)
	}
	record, found, appErr := s.store.LatestRun(sampleID)
	if appErr != nil {
		return domain.RunView{}, false, appErr
	}
	if !found {
		return domain.RunView{}, false, nil
	}
	if record.Status == "running" && !activeRunning {
		if appErr := s.store.SetRunStatus(record.ID, "interrupted", nil); appErr != nil {
			return domain.RunView{}, false, appErr
		}
		record.Status = "interrupted"
	}
	snapshot, appErr := s.store.Snapshot(record.ID)
	if appErr != nil {
		return domain.RunView{}, false, appErr
	}
	sample, appErr := imaging.LoadSample(s.sampleDir, sampleID)
	if appErr != nil {
		return domain.RunView{}, false, appErr
	}
	return pipeline.BuildRunView(sample, snapshot, nil), true, nil
}

func (s *Service) StartRun(req StartRunRequest) (RunHandle, *domain.AppError) {
	if appErr := req.Params.Validate(); appErr != nil {
		return RunHandle{}, appErr
	}
	if req.Mode == "" {
		req.Mode = "live"
	}
	if req.Mode != "live" && req.Mode != "resume" && req.Mode != "reset" {
		return RunHandle{}, domain.ParaError("mode", "live|resume|reset", req.Mode)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.samples[req.SampleID]
	if !ok {
		e := domain.DecodeError("sample is not present in the manifest", nil)
		e.Field = "sample_id"
		e.Actual = req.SampleID
		return RunHandle{}, e
	}
	if existing := s.bySample[req.SampleID]; existing != nil {
		if existing.running {
			return RunHandle{}, domain.StateError("run", -1, -1, "running", "running")
		}
		if req.Mode == "resume" {
			existing.mode = "resume"
			existing.control.Resume()
			return s.launchLocked(existing)
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.counter.Add(1)
	jobID := fmt.Sprintf("%s-%d-%d", req.SampleID, time.Now().UnixMilli(), s.counter.Load())
	control := pipeline.NewControl(req.Params, 0, "")
	sess := &session{
		jobID:     jobID,
		sampleID:  req.SampleID,
		mode:      req.Mode,
		params:    req.Params,
		control:   control,
		ctx:       ctx,
		cancel:    cancel,
		startedAt: time.Now(),
	}
	s.sessions[jobID] = sess
	s.bySample[req.SampleID] = sess
	handle, appErr := s.launchLocked(sess)
	if appErr != nil {
		return RunHandle{}, appErr
	}
	return handle, nil
}

func (s *Service) launchLocked(sess *session) (RunHandle, *domain.AppError) {
	if sess.running {
		return s.handleLocked(sess), nil
	}
	sess.ctx, sess.cancel = context.WithCancel(s.ctx)
	sess.running = true
	sess.error = nil
	handle := s.handleLocked(sess)
	engine := &pipeline.Engine{Store: s.store, SampleDir: s.sampleDir, Seed: s.seed}
	sampleID := sess.sampleID
	params := sess.params
	mode := sess.mode
	onProgress := func(runID int64) {
		s.mu.Lock()
		sess.runID = runID
		current := s.handleLocked(sess)
		s.mu.Unlock()
		s.emit("run:update", current)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		result, appErr := engine.Run(sess.ctx, sampleID, params, mode, sess.control, onProgress)
		s.mu.Lock()
		sess.running = false
		if result != nil {
			sess.runID = result.RunID
			sess.invalidated = result.Invalidated
			sess.params = result.Params
		}
		if appErr != nil {
			sess.error = appErr
		} else {
			sess.error = nil
		}
		updated := s.handleLocked(sess)
		s.mu.Unlock()
		s.emit("run:done", updated)
	}()
	return handle, nil
}

func (s *Service) handleLocked(sess *session) RunHandle {
	status := "starting"
	if sess.running {
		status = "running"
	}
	if sess.runID > 0 {
		if snap, appErr := s.store.Snapshot(sess.runID); appErr == nil {
			status = snap.Run.Status
		}
	}
	if sess.error != nil {
		status = "failed"
	}
	return RunHandle{
		JobID:    sess.jobID,
		RunID:    sess.runID,
		SampleID: sess.sampleID,
		Mode:     sess.mode,
		Status:   status,
		Params:   sess.params,
	}
}

func (s *Service) Snapshot(jobID string) (domain.RunView, *domain.AppError) {
	s.mu.Lock()
	sess := s.sessions[jobID]
	if sess == nil {
		s.mu.Unlock()
		e := domain.Errorf(domain.CodeState, "run handle does not exist")
		e.Unit = "run"
		return domain.RunView{}, e
	}
	runID := sess.runID
	sampleID := sess.sampleID
	invalidated := append([]string(nil), sess.invalidated...)
	params := sess.params
	started := sess.startedAt
	running := sess.running
	sessErr := sess.error
	s.mu.Unlock()

	if runID <= 0 {
		meta, ok := s.samples[sampleID]
		if !ok {
			return domain.RunView{}, domain.DecodeError("sample is missing", nil)
		}
		status := "starting"
		if !running && sessErr != nil {
			status = "failed"
		}
		return domain.RunView{
			SampleID:         sampleID,
			SampleTitle:      meta.Title,
			Status:           status,
			Mode:             "live",
			ImageWidth:       meta.Width,
			ImageHeight:      meta.Height,
			TemplateVersion:  fontlib.Version,
			AlgorithmVersion: domain.AlgorithmVersion,
			Params:           params,
			Error:            sessErr,
			Invalidated:      invalidated,
			ProgressText:     "Preparing deterministic sample…",
			Tiles:            []domain.TileView{},
			Lines:            []domain.LineView{},
			Commits:          []domain.CommitEntry{},
			TruthRegions:     []domain.TruthRegion{},
		}, nil
	}
	snapshot, appErr := s.store.Snapshot(runID)
	if appErr != nil {
		return domain.RunView{}, appErr
	}
	sample, appErr := imaging.LoadSample(s.sampleDir, sampleID)
	if appErr != nil {
		return domain.RunView{}, appErr
	}
	view := pipeline.BuildRunView(sample, snapshot, invalidated)
	view.Params = params
	if view.Error == nil && sessErr != nil {
		view.Error = sessErr
	}
	if running && view.Status == "idle" {
		view.Status = "running"
	}
	if time.Since(started) > 30*time.Minute && view.Status == "starting" {
		view.Status = "interrupted"
	}
	return view, nil
}

func (s *Service) Pause(jobID string) (RunHandle, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[jobID]
	if sess == nil {
		return RunHandle{}, domain.StateError("run", -1, -1, "unknown", "paused")
	}
	if !sess.running {
		return s.handleLocked(sess), nil
	}
	sess.control.Pause()
	return s.handleLocked(sess), nil
}

func (s *Service) Resume(jobID string) (RunHandle, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[jobID]
	if sess == nil {
		return RunHandle{}, domain.StateError("run", -1, -1, "unknown", "running")
	}
	if sess.running {
		sess.control.Resume()
		return s.handleLocked(sess), nil
	}
	sess.mode = "resume"
	sess.error = nil
	sess.control.Resume()
	handle, appErr := s.launchLocked(sess)
	return handle, appErr
}

func (s *Service) Cancel(jobID string) (RunHandle, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[jobID]
	if sess == nil {
		return RunHandle{}, domain.StateError("run", -1, -1, "unknown", "cancelled")
	}
	sess.control.Cancel()
	if !sess.running && sess.runID > 0 {
		_ = s.store.SetRunStatus(sess.runID, "cancelled", nil)
	}
	return s.handleLocked(sess), nil
}

func (s *Service) Tune(jobID string, params domain.Params) (RunHandle, *domain.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[jobID]
	if sess == nil {
		return RunHandle{}, domain.StateError("run", -1, -1, "unknown", "tuned")
	}
	if appErr := sess.control.UpdateParams(params); appErr != nil {
		return RunHandle{}, appErr
	}
	sess.params = params
	if sess.runID > 0 {
		if appErr := s.store.UpdateRunParams(sess.runID, params); appErr != nil {
			return RunHandle{}, appErr
		}
	}
	return s.handleLocked(sess), nil
}

func (s *Service) Retry(jobID string) (RunHandle, *domain.AppError) {
	return s.Resume(jobID)
}

func (s *Service) Compare(req StartRunRequest) (domain.ComparisonResult, *domain.AppError) {
	if req.SampleID == "" {
		req.SampleID = s.manifest.Samples[0].ID
	}
	if appErr := req.Params.Validate(); appErr != nil {
		return domain.ComparisonResult{}, appErr
	}
	result, appErr := pipeline.Compare(s.ctx, s.sampleDir, s.seed, req.SampleID, req.Params)
	return result, appErr
}

func (s *Service) Export(jobID string) (map[string]any, *domain.AppError) {
	view, appErr := s.Snapshot(jobID)
	if appErr != nil {
		return nil, appErr
	}
	return map[string]any{
		"schema_version":    "ocr-workbench-export/1.0",
		"exported_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"algorithm_version": domain.AlgorithmVersion,
		"run":               view,
	}, nil
}

func (s *Service) ExportLatest(sampleID string) (map[string]any, *domain.AppError) {
	view, found, appErr := s.LatestView(sampleID)
	if appErr != nil {
		return nil, appErr
	}
	if !found {
		e := domain.Errorf(domain.CodeState, "no persisted run exists for this sample")
		e.Unit = "run"
		e.Field = "sample_id"
		e.Actual = sampleID
		return nil, e
	}
	truth, appErr := s.SampleTruth(sampleID)
	if appErr != nil {
		return nil, appErr
	}
	return map[string]any{
		"schema_version":    "ocr-workbench-export/1.0",
		"exported_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"algorithm_version": domain.AlgorithmVersion,
		"run":               view,
		"truth":             truth,
	}, nil
}

func (s *Service) SampleImage(sampleID string) (map[string]any, *domain.AppError) {
	s.mu.Lock()
	meta, ok := s.samples[sampleID]
	s.mu.Unlock()
	if !ok {
		return nil, domain.DecodeError("sample is not present in the manifest", nil)
	}
	path := filepath.Join(s.sampleDir, filepath.FromSlash(meta.Image))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, domain.DecodeError("sample image is missing", err)
	}
	return map[string]any{
		"sample_id": sampleID,
		"width":     meta.Width,
		"height":    meta.Height,
		"mime":      "image/png",
		"data_url":  "data:image/png;base64," + base64.StdEncoding.EncodeToString(data),
	}, nil
}

func (s *Service) SampleTruth(sampleID string) (domain.TruthFile, *domain.AppError) {
	s.mu.Lock()
	meta, ok := s.samples[sampleID]
	s.mu.Unlock()
	if !ok {
		return domain.TruthFile{}, domain.DecodeError("sample is not present in the manifest", nil)
	}
	path := filepath.Join(s.sampleDir, filepath.FromSlash(meta.Truth))
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.TruthFile{}, domain.DecodeError("truth file is missing", err)
	}
	var truth domain.TruthFile
	if err := json.Unmarshal(data, &truth); err != nil {
		return domain.TruthFile{}, domain.DecodeError("truth file is invalid", err)
	}
	return truth, nil
}

func (s *Service) Handle(method string, payload json.RawMessage) CallResponse {
	data, appErr := s.dispatch(method, payload)
	if appErr != nil {
		return CallResponse{OK: false, Error: appErr}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return CallResponse{OK: false, Error: domain.Errorf(domain.CodeStore, "cannot encode response: %v", err)}
	}
	return CallResponse{OK: true, Data: raw}
}

func (s *Service) dispatch(method string, payload json.RawMessage) (any, *domain.AppError) {
	switch strings.TrimSpace(method) {
	case "bootstrap":
		return s.Bootstrap()
	case "samples.generate":
		return s.GenerateSamples()
	case "run.start":
		var req StartRunRequest
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.StartRun(req)
	case "run.snapshot":
		var req struct {
			JobID string `json:"job_id"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.Snapshot(req.JobID)
	case "run.latest_view":
		var req struct {
			SampleID string `json:"sample_id"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		view, found, appErr := s.LatestView(req.SampleID)
		if appErr != nil {
			return nil, appErr
		}
		return map[string]any{"found": found, "run": view}, nil
	case "run.pause":
		return s.jobCall(payload, s.Pause)
	case "run.resume":
		return s.jobCall(payload, s.Resume)
	case "run.cancel":
		return s.jobCall(payload, s.Cancel)
	case "run.retry":
		return s.jobCall(payload, s.Retry)
	case "run.tune":
		var req struct {
			JobID  string        `json:"job_id"`
			Params domain.Params `json:"params"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.Tune(req.JobID, req.Params)
	case "run.compare":
		var req StartRunRequest
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.Compare(req)
	case "run.export":
		var req struct {
			JobID string `json:"job_id"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.Export(req.JobID)
	case "run.export_latest":
		var req struct {
			SampleID string `json:"sample_id"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.ExportLatest(req.SampleID)
	case "sample.image":
		var req struct {
			SampleID string `json:"sample_id"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.SampleImage(req.SampleID)
	case "sample.truth":
		var req struct {
			SampleID string `json:"sample_id"`
		}
		if appErr := decodePayload(payload, &req); appErr != nil {
			return nil, appErr
		}
		return s.SampleTruth(req.SampleID)
	default:
		e := domain.Errorf(domain.CodePara, "unknown API method")
		e.Field = "method"
		e.Actual = method
		return nil, e
	}
}

func (s *Service) jobCall(payload json.RawMessage, fn func(string) (RunHandle, *domain.AppError)) (any, *domain.AppError) {
	var req struct {
		JobID string `json:"job_id"`
	}
	if appErr := decodePayload(payload, &req); appErr != nil {
		return nil, appErr
	}
	return fn(req.JobID)
}

func decodePayload(payload json.RawMessage, target any) *domain.AppError {
	if len(payload) == 0 || string(payload) == "null" {
		payload = []byte("{}")
	}
	if err := json.Unmarshal(payload, target); err != nil {
		e := domain.DecodeError("request payload is invalid JSON", err)
		e.Field = "payload"
		return e
	}
	return nil
}

func (s *Service) emit(event string, data any) {
	if s.Emit != nil {
		s.Emit(event, data)
	}
}
