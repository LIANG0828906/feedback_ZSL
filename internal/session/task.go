package session

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
	"stitchworkbench/internal/stitch"
)

type Task struct {
	mu        sync.Mutex
	ID        string
	SetID     string
	Version   int64
	Status    string
	Phase     string
	Progress  int
	StartedAt time.Time
	UpdatedAt time.Time
	Err       *errs.Error
	Result    *model.ExactResult
	cancel    context.CancelFunc
}

type TaskView struct {
	ID        string             `json:"id"`
	SetID     string             `json:"set_id"`
	Version   int64              `json:"version"`
	Status    string             `json:"status"`
	Phase     string             `json:"phase"`
	Progress  int                `json:"progress"`
	StartedAt time.Time          `json:"started_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	Error     *errs.Error        `json:"error,omitempty"`
	Result    *model.ExactResult `json:"result,omitempty"`
}

func (t *Task) view() TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TaskView{
		ID: t.ID, SetID: t.SetID, Version: t.Version, Status: t.Status,
		Phase: t.Phase, Progress: t.Progress, StartedAt: t.StartedAt,
		UpdatedAt: t.UpdatedAt, Error: t.Err, Result: t.Result,
	}
}

func (t *Task) set(phase string, progress int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Phase, t.Progress, t.UpdatedAt = phase, progress, time.Now().UTC()
}

func (t *Task) finish(status string, err error, result *model.ExactResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status, t.Result, t.UpdatedAt = status, result, time.Now().UTC()
	if err != nil {
		t.Err = errs.As(err)
	}
	if status == "paused" {
		t.Phase = "paused"
	}
}

func (s *SetSession) StartExact(version int64, autoCommit bool) (*TaskView, error) {
	latest := s.snapshots.Latest()
	if version == 0 {
		version = latest.Version
	}
	if version != latest.Version {
		return nil, &errs.Error{
			Code: errs.EVersion, Message: "exact task must lock the latest snapshot",
			Actual: version, Threshold: latest.Version, Field: "version",
		}
	}
	snap, ok := s.snapshots.Get(version)
	if !ok {
		return nil, &errs.Error{Code: errs.EVersion, Message: "parameter version not found", Actual: version, Field: "version"}
	}
	s.mu.Lock()
	s.nextTask++
	id := fmt.Sprintf("task-%d", s.nextTask)
	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID: id, SetID: s.ID, Version: version, Status: "running", Phase: "queued",
		Progress: 0, StartedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), cancel: cancel,
	}
	s.tasks[id] = t
	committed := make(map[int]model.PairStats, len(s.commits))
	for k, v := range s.commits {
		committed[k] = v
	}
	s.mu.Unlock()
	go s.runExact(ctx, t, snap.Params, version, autoCommit, committed)
	v := t.view()
	return &v, nil
}

func (s *SetSession) Task(id string) (TaskView, bool) {
	s.mu.Lock()
	t := s.tasks[id]
	s.mu.Unlock()
	if t == nil {
		return TaskView{}, false
	}
	return t.view(), true
}

func (s *SetSession) PauseTask(id string) (TaskView, error) {
	s.mu.Lock()
	t := s.tasks[id]
	s.mu.Unlock()
	if t == nil {
		return TaskView{}, errs.New(errs.EDecode, "task not found")
	}
	t.mu.Lock()
	if t.Status == "running" {
		t.cancel()
		t.Phase = "pausing"
		t.UpdatedAt = time.Now().UTC()
	}
	t.mu.Unlock()
	return t.view(), nil
}

func (s *SetSession) runExact(ctx context.Context, task *Task, params model.Params, version int64, autoCommit bool, committed map[int]model.PairStats) {
	start := time.Now()
	task.set("preview", 5)
	preview, err := s.getOrCreatePreview(version, params)
	if err != nil {
		task.finish("error", err, nil)
		return
	}
	if ctx.Err() != nil {
		task.finish("paused", nil, nil)
		return
	}
	task.set("registration", 20)
	reg, err := stitch.Register(stitch.RegisterOptions{
		Set: s.Set, Params: params, Version: version, Committed: committed, Context: ctx,
	})
	if err != nil {
		if ctx.Err() != nil {
			task.finish("paused", nil, nil)
			return
		}
		task.finish("error", err, nil)
		return
	}
	if ctx.Err() != nil {
		task.finish("paused", nil, nil)
		return
	}
	task.set("consistency", 45)
	div, err := comparePreview(preview, reg, params, committed, start)
	if err != nil {
		task.finish("error", err, nil)
		return
	}
	if reg.Canvas != s.Set.Manifest.Canvas {
		detail := map[string]any{
			"actual": reg.Canvas, "expected": s.Set.Manifest.Canvas,
			"registered_pairs": len(reg.Pairs),
		}
		task.finish("error", &errs.Error{Code: errs.EDiverge, Message: "panorama canvas differs from theoretical canvas", Details: detail}, nil)
		return
	}
	if ctx.Err() != nil {
		task.finish("paused", nil, nil)
		return
	}
	task.set("fusion", 60)
	rendered, err := stitch.RenderContext(ctx, reg, params, 1)
	if err != nil {
		if ctx.Err() != nil {
			task.finish("paused", nil, nil)
			return
		}
		task.finish("error", err, nil)
		return
	}
	tol := s.Set.Manifest.SeamTol
	if tol <= 0 {
		tol = 120
	}
	if rendered.SeamP95Grad > tol {
		task.finish("error", &errs.Error{
			Code: errs.EDiverge, Message: "seam gradient exceeds declared threshold",
			Actual: rendered.SeamP95Grad, Threshold: tol, Field: "seam_gradient_p95",
		}, nil)
		return
	}
	if ctx.Err() != nil {
		task.finish("paused", nil, nil)
		return
	}
	task.set("artifacts", 75)
	prefix := task.ID
	s.putArtifact(prefix+"-panorama.png", rendered.PNG)
	s.putArtifact(prefix+"-seam.png", stitch.SeamOverlayPNG(rendered))
	s.putArtifact(prefix+"-heatmap.png", stitch.HeatmapPNG(reg.Pairs, 800, 360))
	s.putArtifact(prefix+"-matches.png", stitch.MatchOverlayPNG(s.Set, reg, firstUncommitted(reg.Pairs, committed)))
	s.putArtifact(prefix+"-overlay.png", stitch.RegistrationOverlayPNG(s.Set, reg, firstUncommitted(reg.Pairs, committed)))
	pairs := copyPairs(reg.Pairs)
	s.mu.Lock()
	s.exactPairs[version] = pairMap(pairs)
	s.mu.Unlock()
	if autoCommit {
		task.set("commit", 85)
		s.mu.Lock()
		for i := range pairs {
			if ctx.Err() != nil {
				break
			}
			p := pairs[i]
			if p.PairIndex <= 0 || p.Committed || !p.Decision || p.ErrorCode != "" {
				continue
			}
			if err := s.commitStatsLocked(p); err != nil {
				s.mu.Unlock()
				task.finish("error", err, nil)
				return
			}
			pairs[i].Committed = true
			pairs[i].CommittedVersion = p.ParamVersion
			s.exactPairs[version][p.PairIndex] = pairs[i]
		}
		s.mu.Unlock()
		if ctx.Err() != nil {
			task.finish("paused", nil, nil)
			return
		}
	}
	committedCount := 0
	cumulative := 0.0
	for _, p := range pairs {
		if p.Committed {
			committedCount++
		}
		if p.Decision && p.HomographyErr > cumulative {
			cumulative = p.HomographyErr
		}
	}
	result := &model.ExactResult{
		SetID: s.ID, TaskID: task.ID, ParamVersion: version, ParamsHash: params.Hash(),
		ContentHash: s.Set.ContentHash, Canvas: reg.Canvas, Pairs: pairs, Divergence: div,
		PanoramaURL:     fmt.Sprintf("/api/sets/%s/artifact/%s-panorama.png", s.ID, prefix),
		DiagnosticsURL:  fmt.Sprintf("/api/sets/%s/artifact/%s-overlay.png", s.ID, prefix),
		HeatmapURL:      fmt.Sprintf("/api/sets/%s/artifact/%s-heatmap.png", s.ID, prefix),
		SeamOverlayURL:  fmt.Sprintf("/api/sets/%s/artifact/%s-seam.png", s.ID, prefix),
		MatchOverlayURL: fmt.Sprintf("/api/sets/%s/artifact/%s-matches.png", s.ID, prefix),
		DurationMs:      time.Since(start).Milliseconds(), CommittedCount: committedCount,
		MaxParallelism: params.MaxWorkers, SeamGradientP95: rendered.SeamP95Grad,
		SeamGradientTol: tol, CumulativeError: cumulative,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	s.mu.Lock()
	s.exactPairs[version] = pairMap(pairs)
	s.mu.Unlock()
	task.set("complete", 100)
	task.finish("completed", nil, result)
}

func comparePreview(preview *previewCache, exact *stitch.Registration, params model.Params, committed map[int]model.PairStats, start time.Time) (model.DivergenceReport, error) {
	rep := model.DivergenceReport{
		CoverageTolerance: params.DivergenceCoverageTol,
		DecisionTolerance: params.DivergenceDecisionTol,
		PreviewImageHash:  preview.Result.ContentHash, ExactImageHash: exact.ContentHash,
		PreviewParamVersion: preview.Result.ParamVersion, ExactParamVersion: exact.Version,
		PreviewMs: preview.Result.DurationMs, ExactMs: time.Since(start).Milliseconds(),
	}
	exactMap := pairMap(exact.Pairs)
	for idx, pv := range preview.Pairs {
		if idx <= 0 {
			continue
		}
		if _, frozen := committed[idx]; frozen {
			continue
		}
		ex, ok := exactMap[idx]
		if !ok {
			rep.DecisionDiff++
			continue
		}
		rep.ComparedPairs++
		if ex.Decision {
			rep.ExactAccepted = append(rep.ExactAccepted, ex.PairID)
		}
		if pv.Decision {
			rep.PreviewAccepted = append(rep.PreviewAccepted, pv.PairID)
		}
		if pv.Decision != ex.Decision {
			rep.DecisionDiff++
		}
	}
	for idx, ex := range exactMap {
		if idx <= 0 {
			continue
		}
		if _, frozen := committed[idx]; frozen {
			continue
		}
		pv, ok := preview.Pairs[idx]
		if !ok {
			continue
		}
		d := mathAbs(pv.OverlapCoverage - ex.OverlapCoverage)
		if d > rep.MaxCoverageDelta {
			rep.MaxCoverageDelta = d
		}
	}
	sort.Strings(rep.PreviewAccepted)
	sort.Strings(rep.ExactAccepted)
	rep.Consistent = rep.PreviewImageHash == rep.ExactImageHash &&
		rep.PreviewParamVersion == rep.ExactParamVersion &&
		rep.DecisionDiff <= params.DivergenceDecisionTol &&
		rep.MaxCoverageDelta <= params.DivergenceCoverageTol
	if !rep.Consistent {
		return rep, &errs.Error{
			Code: errs.EDiverge, Message: "preview and exact registration diverge",
			Actual: rep, Threshold: map[string]any{
				"decision": params.DivergenceDecisionTol,
				"coverage": params.DivergenceCoverageTol,
			},
		}
	}
	return rep, nil
}

func mathAbs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func firstUncommitted(pairs []model.PairStats, committed map[int]model.PairStats) int {
	for _, p := range pairs {
		if p.PairIndex > 0 {
			if _, ok := committed[p.PairIndex]; !ok {
				return p.PairIndex
			}
		}
	}
	for _, p := range pairs {
		if p.PairIndex > 0 {
			return p.PairIndex
		}
	}
	return 0
}

func (s *SetSession) CommitPair(version int64, pairIndex int) (model.PairStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pairs := s.exactPairs[version]
	if pairs == nil {
		return model.PairStats{}, &errs.Error{Code: errs.EVersion, Message: "no exact result exists for this parameter version", Actual: version, Field: "version"}
	}
	st, ok := pairs[pairIndex]
	if !ok {
		return model.PairStats{}, errs.Field(errs.EConflict, "pair is not part of the locked exact result", "pair_index")
	}
	if err := s.commitStatsLocked(st); err != nil {
		return model.PairStats{}, err
	}
	if frozen, ok := s.commits[pairIndex]; ok {
		st = frozen
	}
	st.Committed = true
	st.CommittedVersion = st.ParamVersion
	s.exactPairs[version][pairIndex] = st
	return st, nil
}

func (s *SetSession) ResetCommits() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits = make(map[int]model.PairStats)
	s.previews = make(map[int64]*previewCache)
	s.exactPairs = make(map[int64]map[int]model.PairStats)
	s.sequence++
	header := model.CheckpointHeader{
		Format: "stitchworkbench.checkpoint/v1", SetID: s.ID,
		ParamVersion: s.snapshots.Latest().Version, ParamsHash: s.snapshots.Latest().Hash,
		BaseContentHash: s.Set.ContentHash, AlgorithmVersion: model.AlgorithmVersion,
		ContentHashes: make(map[string]string), Sequence: s.sequence,
	}
	for _, im := range s.Set.Images {
		header.ContentHashes[im.Truth.File] = im.ContentHash
	}
	return s.store.Save(model.CheckpointSnapshot{Header: header, Records: nil})
}
