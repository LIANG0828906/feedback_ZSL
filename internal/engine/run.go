package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/checkpoint"
	"maskpipeline/internal/model"
	"maskpipeline/internal/sample"
)

type stagePayload struct {
	stage  model.Stage
	output []byte
	hash   string
}

type tileComputation struct {
	index  int
	stages []stagePayload
	err    error
}

type tileFailure struct {
	Tile  int         `json:"tile"`
	Stage model.Stage `json:"stage"`
	Error string      `json:"error"`
	Code  string      `json:"code"`
}

func (e *Engine) Run(ctx context.Context, smp *sample.Sample, req RunRequest) (*RunResult, error) {
	e.init()
	if e.Store == nil || e.Versions == nil {
		return nil, apperr.New(apperr.CodeInternal, "run", "engine is not initialized")
	}
	if smp == nil || smp.Image == nil || smp.Manifest.Width <= 0 || smp.Manifest.Height <= 0 {
		return nil, apperr.New(apperr.CodeMeta, "run", "valid sample is required")
	}
	if req.SessionID == "" {
		return nil, apperr.New(apperr.CodeMeta, "run", "session id is required").
			WithField("session_id")
	}
	if req.Mode == "" {
		req.Mode = checkpoint.ModeIncremental
	}
	if smp.Manifest.Width != e.Versions.Width() || smp.Manifest.Height != e.Versions.Height() {
		return nil, apperr.New(apperr.CodeMeta, "run", "sample dimensions differ from version store").
			WithField("width").
			WithDetail("sample_width", smp.Manifest.Width).
			WithDetail("pipeline_width", e.Versions.Width())
	}
	unlock, err := e.lockSession(req.SessionID)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return e.runLocked(ctx, smp, req, true)
}

func (e *Engine) runLocked(ctx context.Context, smp *sample.Sample, req RunRequest, allowRecovery bool) (*RunResult, error) {
	var snap versionSnapshot
	if req.SnapshotVersion == 0 {
		latest := e.Versions.Latest()
		snap = versionSnapshot{Version: latest.Version, Params: latest.Params, ParamsHash: latest.ParamsHash, AlgorithmVersion: latest.AlgorithmVersion}
	} else {
		got, err := e.Versions.Get(req.SnapshotVersion)
		if err != nil {
			return nil, err
		}
		snap = versionSnapshot{Version: got.Version, Params: got.Params, ParamsHash: got.ParamsHash, AlgorithmVersion: got.AlgorithmVersion}
	}
	if snap.AlgorithmVersion != smp.Manifest.AlgorithmVersion {
		return nil, apperr.New(apperr.CodeVersion, "run", "sample and pipeline algorithm versions differ").
			WithDetail("left_version", snap.AlgorithmVersion).
			WithDetail("right_version", smp.Manifest.AlgorithmVersion)
	}
	params, err := model.ValidateParams(snap.Params, smp.Manifest.Width, smp.Manifest.Height)
	if err != nil {
		return nil, err
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "run", "encode parameter snapshot", err)
	}
	tiles := model.TileLayout(smp.Manifest.Width, smp.Manifest.Height, smp.Manifest.TileSize)
	if len(tiles) == 0 {
		return nil, apperr.New(apperr.CodeMeta, "run", "tile layout is empty").WithField("tile_size")
	}
	meta := checkpoint.Meta{
		SessionID:        req.SessionID,
		SampleID:         smp.Manifest.SampleID,
		ImageSHA256:      smp.ImageSHA256,
		TruthFingerprint: smp.TruthFingerprint,
		ParamsVersion:    snap.Version,
		ParamsHash:       snap.ParamsHash,
		ParamsJSON:       string(paramsJSON),
		AlgorithmVersion: snap.AlgorithmVersion,
		Width:            smp.Manifest.Width,
		Height:           smp.Manifest.Height,
		TileSize:         smp.Manifest.TileSize,
		TilesX:           (smp.Manifest.Width + smp.Manifest.TileSize - 1) / smp.Manifest.TileSize,
		TilesY:           (smp.Manifest.Height + smp.Manifest.TileSize - 1) / smp.Manifest.TileSize,
	}
	ensured, err := e.Store.EnsureSession(ctx, meta, req.Mode)
	if err != nil {
		return nil, err
	}
	segOpts := e.sampleToSegmentOptions(smp, params, 0)
	seg, err := runSegment(smp.Image, segOpts)
	if err != nil {
		return nil, err
	}
	localSeg := &segmentResult{
		Labels: seg.Labels, GrowthLabels: seg.GrowthLabels, Stats: seg.Stats,
		MaskHash: seg.MaskHash, StatsHash: seg.StatsHash,
	}

	outcome, err := e.processTiles(ctx, smp, meta, snap, localSeg, tiles, req.PauseAfterCommits)
	if err != nil {
		if isDrift(err) && allowRecovery {
			if resetErr := e.Store.ResetSession(ctx, req.SessionID); resetErr != nil {
				return nil, resetErr
			}
			again := req
			again.Mode = checkpoint.ModeFull
			again.PauseAfterCommits = 0
			return e.runLocked(ctx, smp, again, false)
		}
		return nil, err
	}
	commits, err := e.Store.Commits(ctx, req.SessionID)
	if err != nil {
		return nil, err
	}
	result := &RunResult{
		SessionID:        req.SessionID,
		Mode:             req.Mode,
		Width:            smp.Manifest.Width,
		Height:           smp.Manifest.Height,
		ParamVersion:     snap.Version,
		ParamsHash:       snap.ParamsHash,
		AlgorithmVersion: snap.AlgorithmVersion,
		Labels:           append([]byte(nil), seg.Labels...),
		Stats:            seg.Stats,
		MaskHash:         seg.MaskHash,
		StatsHash:        seg.StatsHash,
		Commits:          commits,
		CommitSequence:   checkpoint.ParseStageSequence(commits),
		CommittedNew:     outcome.newCommits,
		Invalidated:      ensured.Reset,
		InvalidateReason: ensured.ResetReason,
		Paused:           outcome.paused,
	}
	if outcome.paused {
		result.Status = "paused"
		return result, nil
	}
	if len(outcome.failures) > 0 {
		result.Status = "failed"
		return result, aggregateBlockError(outcome.failures)
	}
	want := checkpoint.StageSequence(len(tiles))
	if !checkpoint.StringsEqual(want, result.CommitSequence) {
		return nil, apperr.New(apperr.CodeState, "run", "successful run did not produce the canonical stage commit sequence").
			WithDetail("want_len", len(want)).
			WithDetail("got_len", len(result.CommitSequence))
	}
	if err := e.Store.Finalize(ctx, req.SessionID, seg.MaskHash, seg.StatsHash); err != nil {
		return nil, err
	}
	result.Status = "completed"
	return result, nil
}

type versionSnapshot struct {
	Version          uint64
	Params           model.Params
	ParamsHash       string
	AlgorithmVersion string
}

func aggregateBlockError(failures []tileFailure) error {
	first := failures[0]
	detailFailures := make([]map[string]any, 0, len(failures))
	for _, f := range failures {
		detailFailures = append(detailFailures, map[string]any{
			"tile": f.Tile, "stage": f.Stage, "code": f.Code, "error": f.Error,
		})
	}
	return apperr.New(apperr.CodeBlock, "process_blocks", "one or more tile stages failed").
		WithDetail("tile", first.Tile).
		WithDetail("stage", first.Stage).
		WithDetail("failed_count", len(failures)).
		WithDetail("failed_stages", detailFailures)
}

type driftError struct {
	tile  int
	stage model.Stage
	err   error
}

func (e *driftError) Error() string {
	return fmt.Sprintf("checkpoint drift at tile %d stage %s: %v", e.tile, e.stage, e.err)
}
func (e *driftError) Unwrap() error { return e.err }

func isDrift(err error) bool {
	var d *driftError
	return errors.As(err, &d)
}

func (e *Engine) processTiles(ctx context.Context, smp *sample.Sample, meta checkpoint.Meta, snap versionSnapshot, seg *segmentResult, tiles []model.Tile, pauseAfter int) (tileOutcome, error) {
	states, err := e.Store.AllStages(ctx, meta.SessionID)
	if err != nil {
		return tileOutcome{}, err
	}
	stateMap := make(map[[2]int]checkpoint.StageView, len(states))
	for _, st := range states {
		stateMap[[2]int{st.TileIndex, stageIndex(st.Stage)}] = st
	}
	outcome := tileOutcome{}
	for start := 0; start < len(tiles); start += e.Opts.MaxParallel {
		if err := ctx.Err(); err != nil {
			return outcome, err
		}
		end := start + e.Opts.MaxParallel
		if end > len(tiles) {
			end = len(tiles)
		}
		batch := tiles[start:end]
		results := make(chan tileComputation, e.Opts.PerBlockBuffer)
		var wg sync.WaitGroup
		for _, tile := range batch {
			tile := tile
			wg.Add(1)
			go func() {
				defer wg.Done()
				results <- e.computeTile(tile.Index, smp, seg, tile)
			}()
		}
		go func() {
			wg.Wait()
			close(results)
		}()
		ordered := make([]tileComputation, 0, len(batch))
		for tr := range results {
			ordered = append(ordered, tr)
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].index < ordered[j].index })
		for _, current := range ordered {
			n, fail, pause, err := e.processTile(ctx, meta, snap, current, stateMap, pauseAfter, outcome.newCommits)
			if err != nil {
				return outcome, err
			}
			outcome.newCommits += n
			if fail != nil {
				outcome.failures = append(outcome.failures, *fail)
				continue
			}
			if pause {
				outcome.paused = true
				return outcome, nil
			}
		}
	}
	return outcome, nil
}

type tileOutcome struct {
	newCommits int
	paused     bool
	failures   []tileFailure
}

type segmentResult struct {
	Labels       []byte
	GrowthLabels []byte
	Stats        model.Stats
	MaskHash     string
	StatsHash    string
}

func (e *Engine) computeTile(index int, smp *sample.Sample, seg *segmentResult, tile model.Tile) tileComputation {
	tr := tileComputation{index: index}
	build := func(stage model.Stage, output []byte) error {
		if len(output) > e.Opts.MaxStageBytes {
			return blockError(index, stage, "tile stage output exceeds the configured buffer limit").
				WithDetail("bytes", len(output)).
				WithDetail("limit", e.Opts.MaxStageBytes)
		}
		tr.stages = append(tr.stages, stagePayload{stage: stage, output: output, hash: model.HashBytes(output)})
		return nil
	}
	growth := model.CropLabels(seg.GrowthLabels, smp.Manifest.Width, smp.Manifest.Height, tile.X, tile.Y, tile.Width, tile.Height)
	if err := build(model.StageGrowth, growth); err != nil {
		tr.err = err
		return tr
	}
	cleanup := model.CropLabels(seg.Labels, smp.Manifest.Width, smp.Manifest.Height, tile.X, tile.Y, tile.Width, tile.Height)
	if err := build(model.StageCleanup, cleanup); err != nil {
		tr.err = err
		return tr
	}
	partial := model.TilePartialStats(index, cleanup, tile.Width, tile.Height, tile.X, tile.Y)
	statsBytes, err := json.Marshal(partial)
	if err != nil {
		tr.err = blockError(index, model.StageStats, "cannot encode tile statistics")
		return tr
	}
	if err := build(model.StageStats, statsBytes); err != nil {
		tr.err = err
		return tr
	}
	return tr
}

func (e *Engine) processTile(ctx context.Context, meta checkpoint.Meta, snap versionSnapshot, tr tileComputation, states map[[2]int]checkpoint.StageView, pauseAfter, alreadyNew int) (int, *tileFailure, bool, error) {
	if tr.err != nil {
		fail, err := e.failComputation(ctx, meta, snap, tr, states)
		return 0, fail, false, err
	}
	payloadByStage := make(map[model.Stage]stagePayload, len(tr.stages))
	for _, p := range tr.stages {
		payloadByStage[p.stage] = p
	}
	newCommits := 0
	for _, stage := range model.Stages {
		payload := payloadByStage[stage]
		key := [2]int{tr.index, stageIndex(stage)}
		st := states[key]
		if st.State == model.StateCommitted {
			if !bytes.Equal(st.Output, payload.output) || st.ContentHash != payload.hash {
				return 0, nil, false, &driftError{tile: tr.index, stage: stage,
					err: apperr.New(apperr.CodeBlock, "verify_checkpoint", "committed stage differs from deterministic recomputation").
						WithDetail("tile", tr.index).WithDetail("stage", stage)}
			}
			continue
		}
		if st.State == model.StateInProgress {
			if err := e.Store.Transition(ctx, meta.SessionID, tr.index, stage, model.StateFailed, snap.Version, snap.AlgorithmVersion); err != nil {
				return 0, nil, false, err
			}
			st.State = model.StateFailed
		}
		if st.State == model.StateFailed || st.State == model.StatePending {
			if err := e.Store.Transition(ctx, meta.SessionID, tr.index, stage, model.StateInProgress, snap.Version, snap.AlgorithmVersion); err != nil {
				return 0, nil, false, err
			}
		}
		if e.Opts.InjectBadTile == tr.index && e.Opts.InjectBadStage == stage {
			msg := "injected block failure"
			if err := e.Store.FailStage(ctx, meta.SessionID, tr.index, stage, snap.Version, snap.AlgorithmVersion, apperr.CodeBlock, msg); err != nil {
				return 0, nil, false, err
			}
			return newCommits, &tileFailure{Tile: tr.index, Stage: stage, Code: apperr.CodeBlock, Error: msg}, false, nil
		}
		_, err := e.Store.Commit(ctx, checkpoint.CommitInput{
			SessionID:        meta.SessionID,
			TileIndex:        tr.index,
			Stage:            stage,
			Output:           payload.output,
			ContentHash:      payload.hash,
			ParamVersion:     snap.Version,
			AlgorithmVersion: snap.AlgorithmVersion,
		})
		if err != nil {
			return 0, nil, false, err
		}
		st.State = model.StateCommitted
		st.Output = payload.output
		st.ContentHash = payload.hash
		states[key] = st
		newCommits++
		if pauseAfter > 0 && alreadyNew+newCommits >= pauseAfter {
			return newCommits, nil, true, nil
		}
	}
	return newCommits, nil, false, nil
}

func (e *Engine) failComputation(ctx context.Context, meta checkpoint.Meta, snap versionSnapshot, tr tileComputation, states map[[2]int]checkpoint.StageView) (*tileFailure, error) {
	stage := model.StageGrowth
	if ae, ok := apperr.As(tr.err); ok {
		if s, ok := ae.Details["stage"].(model.Stage); ok {
			stage = s
		} else if s, ok := ae.Details["stage"].(string); ok {
			stage = model.Stage(s)
		}
	}
	key := [2]int{tr.index, stageIndex(stage)}
	st := states[key]
	if st.State == model.StateCommitted {
		for _, candidate := range model.Stages {
			k := [2]int{tr.index, stageIndex(candidate)}
			if states[k].State != model.StateCommitted {
				stage = candidate
				st = states[k]
				break
			}
		}
	}
	if st.State == model.StateCommitted {
		return &tileFailure{Tile: tr.index, Stage: stage, Code: apperr.Code(tr.err), Error: tr.err.Error()}, nil
	}
	if st.State == model.StateInProgress {
		if err := e.Store.Transition(ctx, meta.SessionID, tr.index, stage, model.StateFailed, snap.Version, snap.AlgorithmVersion); err != nil {
			return nil, err
		}
		st.State = model.StateFailed
	}
	if st.State == model.StatePending || st.State == model.StateFailed {
		if err := e.Store.Transition(ctx, meta.SessionID, tr.index, stage, model.StateInProgress, snap.Version, snap.AlgorithmVersion); err != nil {
			return nil, err
		}
	}
	if err := e.Store.FailStage(ctx, meta.SessionID, tr.index, stage, snap.Version, snap.AlgorithmVersion, apperr.Code(tr.err), tr.err.Error()); err != nil {
		return nil, err
	}
	states[key] = checkpoint.StageView{SessionID: meta.SessionID, TileIndex: tr.index, Stage: stage, State: model.StateFailed}
	return &tileFailure{Tile: tr.index, Stage: stage, Code: apperr.Code(tr.err), Error: tr.err.Error()}, nil
}

func stageIndex(stage model.Stage) int {
	for i, s := range model.Stages {
		if s == stage {
			return i
		}
	}
	return -1
}

func summarizeCommitSequence(commits []checkpoint.CommitRecord) string {
	parts := make([]string, 0, len(commits))
	for _, c := range commits {
		parts = append(parts, fmt.Sprintf("%d/%s", c.TileIndex, c.Stage))
	}
	return strings.Join(parts, ",")
}
