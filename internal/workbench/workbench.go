package workbench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"texlab/internal/apperr"
	"texlab/internal/checkpoint"
	"texlab/internal/engine"
	"texlab/internal/model"
	"texlab/internal/plan"
	"texlab/internal/preview"
	"texlab/internal/runrecord"
	"texlab/internal/sample"
	"texlab/internal/state"
	"texlab/internal/truth"
	"texlab/internal/verify"
)

type Service struct {
	mu         sync.Mutex
	SampleRoot string
	DataDir    string
	Workers    int
	PixelLimit int
	Session    *Session
}

type Session struct {
	Project       sample.Project
	Plan          model.Plan
	Truth         truth.Truth
	Result        *engine.RunResult
	Record        runrecord.Record
	CheckpointDir string
	PlanPath      string
	TruthPath     string
	RecordPath    string
	ReportPath    string
	Verification  VerificationState
	BeforeFinal   model.Image
	CacheAuditLog []engine.ResourceLog
}

type VerificationState struct {
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	Message    string `json:"message,omitempty"`
	ReportPath string `json:"report_path,omitempty"`
	TruthHash  string `json:"truth_fingerprint,omitempty"`
}

type SampleInfo struct {
	Name       string `json:"name"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	Depth      int    `json:"depth"`
	Layout     string `json:"layout"`
	Seed       uint64 `json:"seed"`
	EntryCount int    `json:"entry_count"`
}

type EntryView struct {
	model.Entry
	SourceChannels int    `json:"source_channels"`
	TargetLayout   string `json:"target_layout"`
}

type CellsView struct {
	Cells         []state.Cell `json:"cells"`
	EntryCount    int          `json:"entry_count"`
	TotalSegments int          `json:"total_segments"`
}

type CacheAudit struct {
	EntryIndex   int      `json:"entry_index"`
	EntryID      string   `json:"entry_id"`
	Regions      []string `json:"regions"`
	Segment      int      `json:"segment"`
	State        string   `json:"state"`
	Version      int      `json:"version"`
	Reused       bool     `json:"reused"`
	Invalidated  bool     `json:"invalidated"`
	CacheKey     string   `json:"cache_key,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	SnapshotHash string   `json:"snapshot_hash,omitempty"`
}

type DiffSummary struct {
	ChangedPixels int    `json:"changed_pixels"`
	MaxChannel    uint16 `json:"max_channel_delta"`
	MaxValue      int    `json:"max_channel_value"`
	ComparedTo    string `json:"compared_to"`
}

type State struct {
	Sample            SampleInfo              `json:"sample"`
	Plan              model.Plan              `json:"plan"`
	PlanFingerprint   string                  `json:"plan_fingerprint"`
	IsTruthRevision   bool                    `json:"is_truth_revision"`
	Entries           []EntryView             `json:"entries"`
	Source            preview.View            `json:"source"`
	Final             preview.View            `json:"final"`
	EntryImages       map[string]preview.View `json:"entry_images"`
	Diff              preview.View            `json:"diff"`
	DiffSummary       DiffSummary             `json:"diff_summary"`
	Matrix            CellsView               `json:"matrix"`
	Audit             []CacheAudit            `json:"audit"`
	CommittedSegments int                     `json:"committed_segments"`
	TotalSegments     int                     `json:"total_segments"`
	CommittedCells    int                     `json:"committed_cells"`
	TotalCells        int                     `json:"total_cells"`
	CacheHits         int                     `json:"cache_hits"`
	ReusedCells       int                     `json:"reused_cells"`
	InvalidatedCells  int                     `json:"invalidated_cells"`
	CommitSequence    []int                   `json:"commit_sequence"`
	FinalDigest       string                  `json:"final_digest"`
	Verification      VerificationState       `json:"verification"`
	Logs              []engine.ResourceLog    `json:"logs"`
	CheckpointDir     string                  `json:"checkpoint_dir"`
	PlanPath          string                  `json:"plan_path"`
	TruthPath         string                  `json:"truth_path"`
	RecordPath        string                  `json:"record_path"`
	AlgorithmVersion  string                  `json:"algorithm_version"`
}

type EditRequest struct {
	EntryID        string           `json:"entry_id"`
	TargetDepth    int              `json:"target_depth,omitempty"`
	TargetChannels int              `json:"target_channels,omitempty"`
	Permutation    []int            `json:"permutation,omitempty"`
	Fill           []model.FillSpec `json:"fill,omitempty"`
	Drop           []int            `json:"drop,omitempty"`
	Alpha          string           `json:"alpha,omitempty"`
	Quantize       string           `json:"quantize,omitempty"`
}

type ReorderRequest struct {
	Order []string `json:"order"`
}

type RetryRequest struct {
	Segment int `json:"segment"`
}

type CompareResult struct {
	Equal             bool              `json:"equal"`
	IncrementalDigest string            `json:"incremental_digest"`
	FullDigest        string            `json:"full_digest"`
	CommitSequence    []int             `json:"commit_sequence"`
	Verification      VerificationState `json:"verification"`
}

type ExportResult struct {
	Directory string            `json:"directory"`
	Files     map[string]string `json:"files"`
}

func NewService(sampleRoot, dataDir string, workers, pixelLimit int) (*Service, error) {
	if workers <= 0 {
		workers = 4
	}
	if pixelLimit <= 0 {
		pixelLimit = 262144
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, &apperr.Error{Code: apperr.EIO, Message: "create data directory", Path: dataDir, Details: err.Error()}
	}
	if err := sample.EnsureAll(sampleRoot); err != nil {
		return nil, err
	}
	return &Service{SampleRoot: sampleRoot, DataDir: dataDir, Workers: workers, PixelLimit: pixelLimit}, nil
}

func (s *Service) ListSamples() ([]SampleInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names, err := sample.Names(s.SampleRoot)
	if err != nil {
		return nil, err
	}
	out := make([]SampleInfo, 0, len(names))
	for _, name := range names {
		p, err := sample.Load(s.SampleRoot, name)
		if err != nil {
			return nil, err
		}
		out = append(out, SampleInfo{
			Name: p.Manifest.Name, Width: p.Source.Width, Height: p.Source.Height,
			Depth: p.Source.Depth, Layout: p.Source.Layout, Seed: p.Manifest.Seed,
			EntryCount: len(p.Plan.Entries),
		})
	}
	return out, nil
}

func (s *Service) LoadSample(name string) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := sample.Load(s.SampleRoot, name)
	if err != nil {
		return nil, err
	}
	current := p.Plan
	planPath := filepath.Join(s.DataDir, "plans", name+".json")
	if raw, err := os.ReadFile(planPath); err == nil {
		var persisted model.Plan
		if json.Unmarshal(raw, &persisted) == nil && plan.Validate(persisted, p.Source) == nil &&
			persisted.Revision >= p.Plan.Revision {
			current = persisted
		}
	}
	cpDir := filepath.Join(s.DataDir, "checkpoints", name)
	resume, status, err := checkpoint.LoadAll(cpDir, current, p.SourceFP)
	if err != nil {
		return nil, err
	}
	res, err := s.run(current, p.Source, p.SourceFP, cpDir, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	record := runrecord.Build(res)
	recordPath := filepath.Join(s.DataDir, "runs", name+".json")
	if err := runrecord.Write(recordPath, record); err != nil {
		return nil, err
	}
	if err := writePlanFile(planPath, current); err != nil {
		return nil, err
	}
	currentTruth, truthPath, err := s.ensureTruth(p, current)
	if err != nil {
		return nil, err
	}
	reportPath := filepath.Join(s.DataDir, "reports", name+".json")
	ver := s.verify(p, planPath, truthPath, currentTruth, cpDir, recordPath, reportPath)
	sess := &Session{
		Project: p, Plan: current, Truth: currentTruth, Result: res, Record: record,
		CheckpointDir: cpDir, PlanPath: planPath, TruthPath: truthPath, RecordPath: recordPath, ReportPath: reportPath,
		Verification: ver, BeforeFinal: p.Source.Clone(),
	}
	if status.Discarded {
		sess.CacheAuditLog = append(sess.CacheAuditLog, engine.ResourceLog{
			Reason: "checkpoint_discarded", Detail: status.Reason,
		})
	}
	if status.Valid && len(resume) > 0 {
		sess.CacheAuditLog = append(sess.CacheAuditLog, engine.ResourceLog{
			Reason: "checkpoint_resumed", Detail: fmt.Sprintf("%d committed segments loaded", len(resume)),
		})
	}
	s.Session = sess
	return s.stateLocked(sess), nil
}

func (s *Service) ApplyEdit(req EditRequest) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil, apperr.New(apperr.EState, "no sample is loaded")
	}
	next := s.Session.Plan.Clone()
	idx, ok := plan.FindEntry(next, req.EntryID)
	if !ok {
		return nil, apperr.At(apperr.EPlan, 0, "entry_id", "unknown entry %q", req.EntryID)
	}
	e := &next.Entries[idx]
	sourceChannels := 4
	if idx > 0 {
		sourceChannels = next.Entries[idx-1].TargetChannels
	}
	if req.TargetDepth != 0 {
		e.TargetDepth = req.TargetDepth
	}
	if req.TargetChannels != 0 {
		e.TargetChannels = req.TargetChannels
	}
	if req.Permutation != nil {
		e.Permutation = append([]int(nil), req.Permutation...)
	}
	if req.Fill != nil {
		e.Fill = append([]model.FillSpec(nil), req.Fill...)
	}
	if req.Drop != nil {
		e.Drop = append([]int(nil), req.Drop...)
	}
	if req.Alpha != "" {
		e.Alpha = req.Alpha
	}
	if req.Quantize != "" {
		e.Quantize = req.Quantize
	}
	normalizeChannels(e, sourceChannels)
	next.Revision++
	if err := plan.Validate(next, s.Session.Project.Source); err != nil {
		return nil, err
	}
	return s.applyPlanLocked(next, nil)
}

func normalizeChannels(e *model.Entry, sourceChannels int) {
	if e.TargetChannels < 1 || e.TargetChannels > 4 {
		return
	}
	if len(e.Permutation) != e.TargetChannels {
		if len(e.Permutation) == 0 {
			for i := 0; i < e.TargetChannels && i < sourceChannels; i++ {
				e.Permutation = append(e.Permutation, i)
			}
		}
		for len(e.Permutation) < e.TargetChannels {
			e.Permutation = append(e.Permutation, -1)
		}
		if len(e.Permutation) > e.TargetChannels {
			e.Permutation = e.Permutation[:e.TargetChannels]
		}
	}
	if e.TargetChannels < sourceChannels {
		used := map[int]bool{}
		for _, v := range e.Permutation {
			if v >= 0 {
				used[v] = true
			}
		}
		var drop []int
		for i := 0; i < sourceChannels; i++ {
			if !used[i] {
				drop = append(drop, i)
			}
		}
		e.Drop = drop
		e.Fill = []model.FillSpec{}
	} else if e.TargetChannels > sourceChannels {
		e.Drop = []int{}
		maxValue := 255
		if e.TargetDepth == 16 {
			maxValue = 65535
		}
		fill := map[int]model.FillSpec{}
		for _, f := range e.Fill {
			fill[f.TargetIndex] = f
		}
		for slot, v := range e.Permutation {
			if v == -1 {
				if _, ok := fill[slot]; !ok {
					fill[slot] = model.FillSpec{TargetIndex: slot, Source: "constant", Channel: "A", Value: maxValue}
				}
			}
		}
		e.Fill = e.Fill[:0]
		for i := 0; i < e.TargetChannels; i++ {
			if f, ok := fill[i]; ok {
				e.Fill = append(e.Fill, f)
			}
		}
	} else {
		e.Drop = []int{}
		e.Fill = []model.FillSpec{}
	}
}

func (s *Service) Reorder(req ReorderRequest) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil, apperr.New(apperr.EState, "no sample is loaded")
	}
	next := s.Session.Plan.Clone()
	if len(req.Order) != len(next.Entries) {
		return nil, apperr.At(apperr.EPlan, 0, "order", "order must contain exactly %d entry ids", len(next.Entries))
	}
	byID := map[string]model.Entry{}
	for _, e := range next.Entries {
		byID[e.ID] = e
	}
	entries := make([]model.Entry, 0, len(req.Order))
	seen := map[string]bool{}
	for _, id := range req.Order {
		e, ok := byID[id]
		if !ok || seen[id] {
			return nil, apperr.At(apperr.EPlan, 0, "order", "unknown or duplicate entry %q", id)
		}
		seen[id] = true
		entries = append(entries, e)
	}
	next.Entries = entries
	plan.Reindex(&next)
	next.Revision++
	if err := plan.Validate(next, s.Session.Project.Source); err != nil {
		return nil, err
	}
	return s.applyPlanLocked(next, nil)
}

func (s *Service) RetrySegment(req RetryRequest) (*State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil, apperr.New(apperr.EState, "no sample is loaded")
	}
	next := s.Session.Plan.Clone()
	next.Revision++
	if err := plan.Validate(next, s.Session.Project.Source); err != nil {
		return nil, err
	}
	return s.applyPlanLocked(next, []int{req.Segment})
}

func (s *Service) applyPlanLocked(next model.Plan, force []int) (*State, error) {
	old := s.Session
	before := old.Result.AssembleFinal()
	cpDir := old.CheckpointDir
	res, err := s.run(next, old.Project.Source, old.Project.SourceFP, cpDir, old.Result, force, nil)
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(next, old.Project.Source, old.Project.SourceFP, engine.Options{
		Workers: max(1, s.Workers/2), PixelBufferPerWorker: s.PixelLimit,
	})
	if err != nil {
		return nil, err
	}
	if err := engine.CompareRuns(full, res); err != nil {
		return nil, err
	}
	record := runrecord.Build(res)
	recordPath := filepath.Join(s.DataDir, "runs", old.Project.Manifest.Name+".json")
	if err := runrecord.Write(recordPath, record); err != nil {
		return nil, err
	}
	if err := writePlanFile(old.PlanPath, next); err != nil {
		return nil, err
	}
	nextTruth, truthPath, err := s.ensureTruth(old.Project, next)
	if err != nil {
		return nil, err
	}
	ver := s.verify(old.Project, old.PlanPath, truthPath, nextTruth, cpDir, recordPath, old.ReportPath)
	sess := *old
	sess.Plan = next
	sess.Truth = nextTruth
	sess.Result = res
	sess.Record = record
	sess.TruthPath = truthPath
	sess.RecordPath = recordPath
	sess.BeforeFinal = before
	sess.Verification = ver
	sess.CacheAuditLog = append(sess.CacheAuditLog, res.Logs...)
	if len(sess.CacheAuditLog) > 500 {
		sess.CacheAuditLog = sess.CacheAuditLog[len(sess.CacheAuditLog)-500:]
	}
	s.Session = &sess
	return s.stateLocked(&sess), nil
}

func (s *Service) ComparePaths() (*CompareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil, apperr.New(apperr.EState, "no sample is loaded")
	}
	full, err := engine.Run(s.Session.Plan, s.Session.Project.Source, s.Session.Project.SourceFP, engine.Options{
		Workers: max(1, s.Workers/2), PixelBufferPerWorker: s.PixelLimit,
	})
	if err != nil {
		return nil, err
	}
	out := &CompareResult{
		Equal:             true,
		IncrementalDigest: s.Session.Record.FinalDigest,
		FullDigest:        runrecord.Build(full).FinalDigest,
		CommitSequence:    append([]int(nil), full.CommitSequence...),
		Verification:      s.Session.Verification,
	}
	if err := engine.CompareRuns(full, s.Session.Result); err != nil {
		out.Equal = false
		return out, err
	}
	s.Session.Verification = s.verify(s.Session.Project, s.Session.PlanPath, s.Session.TruthPath, s.Session.Truth,
		s.Session.CheckpointDir, s.Session.RecordPath, s.Session.ReportPath)
	out.Verification = s.Session.Verification
	return out, nil
}

func (s *Service) Export() (*ExportResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return nil, apperr.New(apperr.EState, "no sample is loaded")
	}
	sess := s.Session
	dir := filepath.Join(s.DataDir, "exports", sess.Project.Manifest.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &apperr.Error{Code: apperr.EIO, Message: "create export directory", Path: dir, Details: err.Error()}
	}
	files := map[string]string{}
	planPath := filepath.Join(dir, "plan.json")
	raw, _ := json.MarshalIndent(sess.Plan, "", "  ")
	if err := os.WriteFile(planPath, append(raw, '\n'), 0o644); err != nil {
		return nil, err
	}
	files["plan"] = planPath
	finalPath := filepath.Join(dir, "final.png")
	if err := preview.WritePNG(finalPath, sess.Result.AssembleFinal(), 0, 0); err != nil {
		return nil, err
	}
	files["final_png"] = finalPath
	runPath := filepath.Join(dir, "run-record.json")
	if err := runrecord.Write(runPath, sess.Record); err != nil {
		return nil, err
	}
	files["run_record"] = runPath
	if raw, err := os.ReadFile(sess.TruthPath); err == nil {
		truthPath := filepath.Join(dir, "truth.json")
		if err := os.WriteFile(truthPath, raw, 0o644); err == nil {
			files["truth"] = truthPath
		}
	}
	if sess.Verification.Status == "MATCH" || sess.Verification.Status == "DIFF" {
		reportPath := filepath.Join(dir, "verification-report.json")
		reportRaw, err := os.ReadFile(sess.ReportPath)
		if err == nil {
			_ = os.WriteFile(reportPath, reportRaw, 0o644)
			files["verification_report"] = reportPath
		}
	}
	manifest := map[string]any{
		"sample": sess.Project.Manifest.Name, "plan_fingerprint": model.PlanFingerprint(sess.Plan),
		"final_digest": sess.Record.FinalDigest, "algorithm_version": model.AlgorithmVersion,
		"verification": sess.Verification,
	}
	manifestRaw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "export-manifest.json"), append(manifestRaw, '\n'), 0o644); err != nil {
		return nil, err
	}
	files["manifest"] = filepath.Join(dir, "export-manifest.json")
	return &ExportResult{Directory: dir, Files: files}, nil
}

func (s *Service) run(p model.Plan, source model.Image, sourceFP, cpDir string, previous *engine.RunResult, force []int, resume map[int]*engine.SegmentCommit) (*engine.RunResult, error) {
	store, err := checkpoint.NewStore(cpDir, p, sourceFP)
	if err != nil {
		return nil, err
	}
	if resume == nil {
		if loaded, status, err := checkpoint.LoadAll(cpDir, p, sourceFP); err == nil && status.Valid {
			resume = loaded
		}
	}
	res, err := engine.Run(p, source, sourceFP, engine.Options{
		Workers: s.Workers, PixelBufferPerWorker: s.PixelLimit,
		Previous: previous, ForceSegments: force, Resume: resume, Sink: store,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

func (s *Service) ensureTruth(p sample.Project, plan model.Plan) (truth.Truth, string, error) {
	if model.PlanFingerprint(plan) == p.Truth.PlanFingerprint {
		return p.Truth, filepath.Join(p.Dir, "truth.json"), nil
	}
	path := filepath.Join(s.DataDir, "truths", fmt.Sprintf("%s-rev%d.json", p.Manifest.Name, plan.Revision))
	if stored, err := truth.Load(path); err == nil && stored.PlanFingerprint == model.PlanFingerprint(plan) {
		return stored, path, nil
	}
	generated, err := sample.BuildTruthWithSpec(p.Spec, p.Source, p.SourceFP, plan)
	if err != nil {
		return truth.Truth{}, "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return truth.Truth{}, "", &apperr.Error{Code: apperr.EIO, Message: "create truth directory", Path: path, Details: err.Error()}
	}
	if err := truth.Write(path, generated); err != nil {
		return truth.Truth{}, "", &apperr.Error{Code: apperr.EIO, Message: "write generated truth", Path: path, Details: err.Error()}
	}
	return generated, path, nil
}

func (s *Service) verify(p sample.Project, planPath, truthPath string, t truth.Truth, cpDir, recordPath, reportPath string) VerificationState {
	report, err := verify.Run(verify.Options{
		PlanPath: planPath, TruthPath: truthPath,
		CheckpointDir: cpDir, RecordPath: recordPath, ReportPath: reportPath,
		SourceImage: p.Source, SourceFingerprint: p.SourceFP,
	})
	if err != nil {
		ae := apperr.As(err)
		status := "ERROR"
		if ae.Code == apperr.EDiff {
			status = "DIFF"
		}
		if report != nil && report.Status == "DIFF" {
			status = "DIFF"
		}
		return VerificationState{Status: status, Code: ae.Code, Message: ae.Message, ReportPath: reportPath, TruthHash: t.Fingerprint}
	}
	return VerificationState{Status: "MATCH", ReportPath: reportPath, TruthHash: t.Fingerprint}
}

func (s *Service) stateLocked(sess *Session) *State {
	p := sess.Project
	res := sess.Result
	out := &State{
		Sample: SampleInfo{
			Name: p.Manifest.Name, Width: p.Source.Width, Height: p.Source.Height,
			Depth: p.Source.Depth, Layout: p.Source.Layout, Seed: p.Manifest.Seed,
			EntryCount: len(sess.Plan.Entries),
		},
		Plan: sess.Plan, PlanFingerprint: model.PlanFingerprint(sess.Plan),
		IsTruthRevision:   sess.Truth.PlanFingerprint == model.PlanFingerprint(sess.Plan),
		CommittedSegments: res.CommittedSegments, TotalSegments: res.TotalSegments,
		CommittedCells: res.CommittedEntries, TotalCells: res.TotalEntries,
		CacheHits: res.CacheHits, ReusedCells: res.ReusedCells, InvalidatedCells: res.InvalidatedCells,
		CommitSequence: append([]int(nil), res.CommitSequence...),
		FinalDigest:    sess.Record.FinalDigest,
		Verification:   sess.Verification, Logs: append([]engine.ResourceLog(nil), sess.CacheAuditLog...),
		CheckpointDir: sess.CheckpointDir, PlanPath: sess.PlanPath, TruthPath: sess.TruthPath, RecordPath: sess.RecordPath,
		AlgorithmVersion: model.AlgorithmVersion,
		Matrix:           CellsView{Cells: res.Matrix, EntryCount: len(sess.Plan.Entries), TotalSegments: res.TotalSegments},
		EntryImages:      map[string]preview.View{},
	}
	if len(out.Logs) > 200 {
		out.Logs = out.Logs[len(out.Logs)-200:]
	}
	sourceView, err := preview.DataURL(p.Source, 960, 540)
	if err == nil {
		out.Source = sourceView
	}
	finalView, err := preview.SegmentsDataURL(res, "", true, 960, 540)
	if err == nil {
		out.Final = finalView
	}
	for i, e := range sess.Plan.Entries {
		view := EntryView{Entry: e, SourceChannels: 4}
		if i > 0 {
			view.SourceChannels = sess.Plan.Entries[i-1].TargetChannels
		}
		view.TargetLayout = targetLayout(e)
		out.Entries = append(out.Entries, view)
		if v, err := preview.SegmentsDataURL(res, e.ID, false, 600, 338); err == nil {
			out.EntryImages[e.ID] = v
		}
	}
	before := sess.BeforeFinal
	if len(before.Pixels) == 0 {
		before = p.Source
	}
	after := res.AssembleFinal()
	if heat, err := preview.Heatmap(before, after, 960, 540); err == nil {
		out.Diff = heat
	}
	out.DiffSummary = diffSummary(before, after)
	for _, c := range res.Matrix {
		var regions []string
		if c.EntryIndex > 0 && c.EntryIndex <= len(sess.Plan.Entries) {
			regions = append(regions, sess.Plan.Entries[c.EntryIndex-1].Regions...)
		}
		out.Audit = append(out.Audit, CacheAudit{
			EntryIndex: c.EntryIndex, EntryID: c.EntryID, Segment: c.Segment,
			Regions: regions,
			State:   string(c.State), Version: c.Version, Reused: c.Reused,
			Invalidated: c.Invalidated, CacheKey: c.CacheKey, Reason: c.Reason,
			SnapshotHash: c.SnapshotHash,
		})
	}
	sort.Slice(out.Audit, func(i, j int) bool {
		if out.Audit[i].EntryIndex != out.Audit[j].EntryIndex {
			return out.Audit[i].EntryIndex < out.Audit[j].EntryIndex
		}
		return out.Audit[i].Segment < out.Audit[j].Segment
	})
	return out
}

func writePlanFile(path string, p model.Plan) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "create plan state directory", Path: path, Details: err.Error()}
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "encode plan state", Path: path, Details: err.Error()}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "write plan state", Path: path, Details: err.Error()}
	}
	if err := os.Rename(tmp, path); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "replace plan state", Path: path, Details: err.Error()}
	}
	return nil
}

func targetLayout(e model.Entry) string {
	fill := map[int]model.FillSpec{}
	for _, f := range e.Fill {
		fill[f.TargetIndex] = f
	}
	var b strings.Builder
	for slot, src := range e.Permutation {
		if src >= 0 && src < 4 {
			b.WriteByte("RGBA"[src])
		} else if f, ok := fill[slot]; ok {
			b.WriteString(f.Channel)
		} else {
			b.WriteByte('?')
		}
	}
	return b.String()
}

func diffSummary(a, b model.Image) DiffSummary {
	out := DiffSummary{ComparedTo: "previous run"}
	out.MaxValue = 255
	if b.Depth == 16 {
		out.MaxValue = 65535
	}
	w, h := min(a.Width, b.Width), min(a.Height, b.Height)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pa, pb := a.SemanticPixel(x, y), b.SemanticPixel(x, y)
			changed := false
			for c := 0; c < 4; c++ {
				d := abs16(pa[c], pb[c])
				if d > 0 {
					changed = true
				}
				if d > out.MaxChannel {
					out.MaxChannel = d
				}
			}
			if changed {
				out.ChangedPixels++
			}
		}
	}
	return out
}

func abs16(a, b uint16) uint16 {
	if a > b {
		return a - b
	}
	return b - a
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Service) TruthForSession() (truth.Truth, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Session == nil {
		return truth.Truth{}, false
	}
	return s.Session.Project.Truth, true
}
