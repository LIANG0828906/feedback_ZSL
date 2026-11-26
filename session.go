package downsample

import "context"

type Session struct {
	Engine     *Engine
	Checkpoint *Checkpoint
}

func NewSession(project *Project, budget Budget, checkpointPath string) (*Session, *Error) {
	if err := budget.Validate(); err != nil {
		return nil, err
	}
	cp, err := OpenCheckpoint(checkpointPath)
	if err != nil {
		return nil, err
	}
	engine := NewEngine(project, budget)
	report, rerr := engine.RecomputeAll(InvalidationSource{Kind: "session_start"})
	if rerr != nil {
		_ = cp.Close()
		return nil, rerr
	}
	points := allProjectPoints(project)
	if cerr := cp.SaveCommitBatch(project.ConfigVersion, engine.GlobalSequence(), reportWindowResults(report), points); cerr != nil {
		_ = cp.Close()
		return nil, cerr
	}
	return &Session{Engine: engine, Checkpoint: cp}, nil
}

func allProjectPoints(project *Project) []Point {
	out := []Point{}
	for _, metric := range project.Metrics {
		out = append(out, project.SortedPoints(metric.ID)...)
	}
	return out
}

func reportWindowResults(report *RecomputeReport) []WindowResult {
	out := make([]WindowResult, 0, len(report.Recomputed))
	for _, key := range report.Recomputed {
		if result, ok := report.Results[key]; ok {
			out = append(out, result)
		}
	}
	return out
}

func (s *Session) Close() error {
	if s == nil || s.Checkpoint == nil {
		return nil
	}
	return s.Checkpoint.Close()
}

// AppendAndCheckpoint commits one logical sample at a time. A cancellation
// between samples leaves the checkpoint at the last complete commit point.
func (s *Session) AppendAndCheckpoint(ctx context.Context, points ...Point) (*RecomputeReport, *Error) {
	var last *RecomputeReport
	for i := range points {
		if err := ctx.Err(); err != nil {
			seq := s.Engine.GlobalSequence()
			coded := NewError(CodeStreamInterrupted, "sample stream stopped at last complete commit point").
				WithContext("committed_seq", seq).
				WithContext("remaining_samples", len(points)-i)
			return last, coded
		}
		report, err := s.Engine.AppendPoints(points[i])
		if err != nil {
			return last, err
		}
		persisted := reportWindowResults(report)
		if cerr := s.Checkpoint.SaveCommitBatch(report.ConfigVersion, s.Engine.GlobalSequence(), persisted, []Point{points[i]}); cerr != nil {
			return report, cerr
		}
		last = report
	}
	return last, nil
}

func (s *Session) PersistConfigReport(report *RecomputeReport) *Error {
	if report == nil {
		return NewError(CodeConfigEditInvalid, "missing config report")
	}
	// Computation only touched report.Recomputed. Persist the full snapshot
	// under the new config version so recovery can see reused windows too.
	results := make([]WindowResult, 0, len(report.Results))
	for _, result := range report.Results {
		results = append(results, result)
	}
	return s.Checkpoint.SaveCommitBatch(report.ConfigVersion, s.Engine.GlobalSequence(), results, nil)
}

// RecoverOrInitialize loads a version-matching checkpoint. A version mismatch
// is returned as a coded notice after the checkpoint has been reset and a full
// recompute has been persisted.
func RecoverOrInitialize(project *Project, budget Budget, checkpointPath string) (*Session, *Recovery, *Error) {
	if err := budget.Validate(); err != nil {
		return nil, nil, err
	}
	cp, err := OpenCheckpoint(checkpointPath)
	if err != nil {
		return nil, nil, err
	}
	recovery, rerr := cp.Recover(project.ConfigVersion)
	if rerr != nil && !IsCode(rerr, CodeCheckpointVersionMismatch) {
		_ = cp.Close()
		return nil, recovery, rerr
	}

	if rerr != nil && IsCode(rerr, CodeCheckpointVersionMismatch) {
		if resetErr := cp.Reset(); resetErr != nil {
			_ = cp.Close()
			return nil, recovery, resetErr
		}
		engine := NewEngine(project, budget)
		report, recomputeErr := engine.RecomputeAll(InvalidationSource{Kind: "version_mismatch_full_recompute"})
		if recomputeErr != nil {
			_ = cp.Close()
			return nil, recovery, recomputeErr
		}
		if saveErr := cp.SaveCommitBatch(project.ConfigVersion, engine.GlobalSequence(), reportWindowResults(report), allProjectPoints(project)); saveErr != nil {
			_ = cp.Close()
			return nil, recovery, saveErr
		}
		return &Session{Engine: engine, Checkpoint: cp}, recovery, rerr
	}

	if recovery == nil || !recovery.Found {
		engine := NewEngine(project, budget)
		report, recomputeErr := engine.RecomputeAll(InvalidationSource{Kind: "fresh_checkpoint"})
		if recomputeErr != nil {
			_ = cp.Close()
			return nil, nil, recomputeErr
		}
		if saveErr := cp.SaveCommitBatch(project.ConfigVersion, engine.GlobalSequence(), reportWindowResults(report), allProjectPoints(project)); saveErr != nil {
			_ = cp.Close()
			return nil, nil, saveErr
		}
		return &Session{Engine: engine, Checkpoint: cp}, recovery, nil
	}

	if len(recovery.Points) > 0 {
		series := map[string][]Point{}
		for _, p := range recovery.Points {
			series[p.MetricID] = append(series[p.MetricID], p)
		}
		project.Series = series
	}
	engine := NewEngine(project, budget)
	engine.LoadRecovery(recovery)
	return &Session{Engine: engine, Checkpoint: cp}, recovery, nil
}

func (e *Engine) LoadRecovery(recovery *Recovery) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if recovery == nil {
		return
	}
	e.globalSeq = recovery.GlobalSeq
	for key, result := range recovery.Results {
		if e.cache[key.MetricID] == nil {
			e.cache[key.MetricID] = map[int64]WindowResult{}
		}
		result.Invalidation.Kind = "checkpoint"
		result.Invalidation.Reason = "restored_from_committed_sqlite_state"
		e.cache[key.MetricID][key.StartMS] = result
	}
}
