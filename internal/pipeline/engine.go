package pipeline

import (
	"context"
	"fmt"
	"time"

	"ocrworkbench/internal/domain"
	"ocrworkbench/internal/imaging"
	"ocrworkbench/internal/ocr"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/synth"
)

type Engine struct {
	Store     *store.Store
	SampleDir string
	Seed      int64
}

type Result struct {
	RunID       int64
	Sample      *domain.LoadedSample
	Snapshot    store.Snapshot
	Params      domain.Params
	BandWidth   int
	Invalidated []string
}

func (e *Engine) Run(ctx context.Context, sampleID string, params domain.Params, mode string, control *Control, onProgress func(int64)) (*Result, *domain.AppError) {
	if control == nil {
		control = NewControl(params, 0, "")
	}
	if appErr := params.Validate(); appErr != nil {
		return nil, appErr
	}
	if _, appErr := synth.Ensure(e.SampleDir, e.Seed); appErr != nil {
		return nil, appErr
	}
	sample, appErr := imaging.LoadSample(e.SampleDir, sampleID)
	if appErr != nil {
		return nil, appErr
	}
	recognizer, appErr := ocr.NewRecognizer(params.TemplateVersion, params.Split)
	if appErr != nil {
		return nil, appErr
	}
	detector := ocr.NewDetector(sample.Gray)
	band, appErr := params.EffectiveBand(detector.AvgStroke)
	if appErr != nil {
		return nil, appErr
	}
	paramVersion := params.StructuralVersion(band)
	if control.paramVersion == "" {
		control.band = band
		control.paramVersion = paramVersion
	}
	identity := domain.SnapshotIdentity{
		SampleID:         sampleID,
		ImageHash:        sample.ContentHash,
		TemplateVersion:  params.TemplateVersion,
		ParamVersion:     paramVersion,
		AlgorithmVersion: domain.AlgorithmVersion,
		BandWidth:        band,
	}
	if mode == "reset" {
		if appErr := e.Store.DeleteSampleRuns(sampleID); appErr != nil {
			return nil, appErr
		}
	}
	run, invalidated, appErr := e.Store.OpenOrCreateRun(identity, sampleID, params, detector.AvgStroke)
	if appErr != nil {
		return nil, appErr
	}
	tiles, appErr := imaging.BuildTiles(sample.Width, sample.Height, params.TileSize, band)
	if appErr != nil {
		return nil, appErr
	}
	if appErr := e.Store.EnsureTiles(run.ID, tiles); appErr != nil {
		return nil, appErr
	}
	if appErr := e.Store.UpdateRunParams(run.ID, control.Params()); appErr != nil {
		return nil, appErr
	}
	if appErr := e.recoverTransient(run.ID); appErr != nil {
		return nil, appErr
	}
	if appErr := e.Store.SetRunStatus(run.ID, "running", nil); appErr != nil {
		return nil, appErr
	}
	if onProgress != nil {
		onProgress(run.ID)
	}

	goal := func() (*Result, *domain.AppError) {
		return e.result(run.ID, sample, control.Params(), band, invalidated)
	}
	stop, stopErr := control.Checkpoint(ctx)
	if stopErr != nil {
		return goal()
	}
	if stop != "running" {
		status := "paused"
		if stop == "cancelled" {
			status = "cancelled"
		}
		_ = e.Store.SetRunStatus(run.ID, status, nil)
		return goal()
	}

	tileErr := e.runTiles(ctx, run.ID, detector, sample.ContentHash, control, onProgress)
	if tileErr == errStop {
		return goal()
	}
	if tileErr != nil {
		res, resErr := goal()
		if resErr != nil {
			return nil, resErr
		}
		return res, tileErr
	}

	snapshot, appErr := e.Store.Snapshot(run.ID)
	if appErr != nil {
		return goal()
	}
	lines, appErr := e.ensureLines(run.ID, sample, snapshot, control.Params())
	if appErr != nil {
		_ = e.Store.SetRunStatus(run.ID, "failed", appErr)
		res, resErr := goal()
		if resErr != nil {
			return nil, resErr
		}
		return res, appErr
	}
	lineErr := e.runLines(ctx, run.ID, sample, recognizer, lines, control, paramVersion, onProgress)
	if lineErr == errStop {
		return goal()
	}
	if lineErr != nil {
		res, resErr := goal()
		if resErr != nil {
			return nil, resErr
		}
		return res, lineErr
	}

	finalSnapshot, appErr := e.Store.Snapshot(run.ID)
	if appErr != nil {
		return goal()
	}
	committed := 0
	for _, line := range finalSnapshot.Lines {
		if line.State == domain.LineCommitted {
			committed++
		}
	}
	if committed != len(finalSnapshot.Lines) {
		stateErr := domain.Errorf(domain.CodeState, "run finished with uncommitted text lines")
		stateErr.Field = "line_state"
		_ = e.Store.SetRunStatus(run.ID, "failed", stateErr)
		res, resErr := goal()
		if resErr != nil {
			return nil, resErr
		}
		return res, stateErr
	}
	if appErr := e.Store.SetRunStatus(run.ID, "completed", nil); appErr != nil {
		return goal()
	}
	if onProgress != nil {
		onProgress(run.ID)
	}
	return e.result(run.ID, sample, control.Params(), band, invalidated)
}

var errStop = &domain.AppError{Code: "STOP"}

func (e *Engine) runTiles(ctx context.Context, runID int64, detector *ocr.Detector, imageHash string, control *Control, onProgress func(int64)) *domain.AppError {
	for {
		snapshot, appErr := e.Store.Snapshot(runID)
		if appErr != nil {
			return appErr
		}
		nextIndex := -1
		for _, tile := range snapshot.Tiles {
			if tile.State != domain.TileCommitted {
				nextIndex = tile.Tile.Index
				break
			}
		}
		if nextIndex < 0 {
			return nil
		}
		stop, stopErr := control.Checkpoint(ctx)
		if stopErr != nil {
			return stopErr
		}
		if stop != "running" {
			status := "paused"
			if stop == "cancelled" {
				status = "cancelled"
			}
			_ = e.Store.SetRunStatus(runID, status, nil)
			return errStop
		}
		stored := snapshot.Tiles[nextIndex]
		if stored.State == domain.TilePending || stored.State == domain.TileFailed {
			if appErr := e.Store.TransitionTile(runID, stored.Tile.Index, domain.TileDetecting); appErr != nil {
				_ = e.Store.SetRunStatus(runID, "failed", appErr)
				return appErr
			}
			if onProgress != nil {
				onProgress(runID)
			}
		}
		params := control.Params()
		candidates, appErr := detector.ScanTile(stored.Tile, params)
		if appErr != nil {
			_ = e.Store.FailTile(runID, stored.Tile.Index, appErr)
			_ = e.Store.SetRunStatus(runID, "failed", appErr)
			return appErr
		}
		payload := domain.TileCommitPayload{
			Tile:              stored.Tile,
			Candidates:        candidates,
			ImageHash:         imageHash,
			TemplateVersion:   params.TemplateVersion,
			ParamVersion:      params.StructuralVersion(stored.Tile.BandWidth),
			TuningVersion:     params.TuningVersion(),
			AlgorithmVersion:  domain.AlgorithmVersion,
			CommittedUnixNano: time.Now().UnixNano(),
		}
		payloadHash := tilePayloadHash(payload)
		if _, appErr := e.Store.CommitTile(runID, payload, payloadHash); appErr != nil {
			_ = e.Store.FailTile(runID, stored.Tile.Index, appErr)
			_ = e.Store.SetRunStatus(runID, "failed", appErr)
			return appErr
		}
		control.MarkCommit()
		if onProgress != nil {
			onProgress(runID)
		}
	}
}

func (e *Engine) ensureLines(runID int64, sample *domain.LoadedSample, snapshot store.Snapshot, params domain.Params) ([]store.StoredLine, *domain.AppError) {
	if len(snapshot.Lines) > 0 {
		return snapshot.Lines, nil
	}
	candidates := make([]domain.Candidate, 0)
	for _, tile := range snapshot.Tiles {
		if tile.State != domain.TileCommitted {
			e := domain.Errorf(domain.CodeState, "cannot merge text lines while tiles are uncommitted")
			e.Unit = "tile"
			e.TileIndex = tile.Tile.Index
			return nil, e
		}
		candidates = append(candidates, tile.Candidates...)
	}
	lines, appErr := ocr.MergeCandidates(candidates, params)
	if appErr != nil {
		return nil, appErr
	}
	deviation := len(lines) - len(sample.Truth.Regions)
	if absInt(deviation) > params.CountTolerance {
		e := domain.Errorf(domain.CodeDet, "merged text-line count exceeds the declared truth tolerance")
		e.Field = "count_tolerance"
		e.Expected = fmt.Sprintf("truth±%d", params.CountTolerance)
		e.Actual = fmt.Sprintf("detected=%d truth=%d", len(lines), len(sample.Truth.Regions))
		return nil, e
	}
	if len(lines) == 0 {
		e := domain.Errorf(domain.CodeDet, "no text lines were detected")
		e.Field = "merged_lines"
		return nil, e
	}
	if appErr := e.Store.EnsureLines(runID, lines); appErr != nil {
		return nil, appErr
	}
	snapshot, appErr = e.Store.Snapshot(runID)
	if appErr != nil {
		return nil, appErr
	}
	return snapshot.Lines, nil
}

func (e *Engine) runLines(ctx context.Context, runID int64, sample *domain.LoadedSample, recognizer *ocr.Recognizer, lines []store.StoredLine, control *Control, paramVersion string, onProgress func(int64)) *domain.AppError {
	for {
		snapshot, appErr := e.Store.Snapshot(runID)
		if appErr != nil {
			return appErr
		}
		nextIndex := -1
		for _, line := range snapshot.Lines {
			if line.State != domain.LineCommitted {
				nextIndex = line.Line.ReadingIndex
				break
			}
		}
		if nextIndex < 0 {
			return nil
		}
		stop, stopErr := control.Checkpoint(ctx)
		if stopErr != nil {
			return stopErr
		}
		if stop != "running" {
			status := "paused"
			if stop == "cancelled" {
				status = "cancelled"
			}
			_ = e.Store.SetRunStatus(runID, status, nil)
			return errStop
		}
		stored := snapshot.Lines[nextIndex]
		if stored.State == domain.LinePending || stored.State == domain.LineFailed {
			if appErr := e.Store.TransitionLine(runID, stored.Line.ReadingIndex, domain.LineRecognizing); appErr != nil {
				_ = e.Store.SetRunStatus(runID, "failed", appErr)
				return appErr
			}
			if onProgress != nil {
				onProgress(runID)
			}
		}
		params := control.Params()
		recognition, appErr := recognizer.RecognizeLine(sample.Gray, stored.Line, params.ConfidenceThreshold)
		if appErr != nil {
			_ = e.Store.FailLine(runID, stored.Line.ReadingIndex, appErr)
			_ = e.Store.SetRunStatus(runID, "failed", appErr)
			return appErr
		}
		commitSnapshot := domain.LineCommitSnapshot{
			Line:              stored.Line,
			Recognition:       recognition,
			Params:            params,
			ImageHash:         sample.ContentHash,
			ParamVersion:      paramVersion,
			TuningVersion:     params.TuningVersion(),
			TemplateVersion:   params.TemplateVersion,
			AlgorithmVersion:  domain.AlgorithmVersion,
			CommittedUnixNano: time.Now().UnixNano(),
		}
		payloadHash := linePayloadHash(commitSnapshot)
		if _, appErr := e.Store.CommitLine(runID, commitSnapshot, payloadHash); appErr != nil {
			_ = e.Store.FailLine(runID, stored.Line.ReadingIndex, appErr)
			_ = e.Store.SetRunStatus(runID, "failed", appErr)
			return appErr
		}
		control.MarkCommit()
		if onProgress != nil {
			onProgress(runID)
		}
	}
}

func (e *Engine) recoverTransient(runID int64) *domain.AppError {
	snapshot, appErr := e.Store.Snapshot(runID)
	if appErr != nil {
		return appErr
	}
	for _, tile := range snapshot.Tiles {
		if tile.State == domain.TileDetecting {
			if appErr := e.Store.TransitionTile(runID, tile.Tile.Index, domain.TileFailed); appErr != nil {
				return appErr
			}
		}
	}
	for _, line := range snapshot.Lines {
		if line.State == domain.LineRecognizing {
			if appErr := e.Store.TransitionLine(runID, line.Line.ReadingIndex, domain.LineFailed); appErr != nil {
				return appErr
			}
		}
	}
	return nil
}

func (e *Engine) result(runID int64, sample *domain.LoadedSample, params domain.Params, band int, invalidated []string) (*Result, *domain.AppError) {
	snapshot, appErr := e.Store.Snapshot(runID)
	if appErr != nil {
		return nil, appErr
	}
	return &Result{
		RunID:       runID,
		Sample:      sample,
		Snapshot:    snapshot,
		Params:      params,
		BandWidth:   band,
		Invalidated: invalidated,
	}, nil
}

func tilePayloadHash(payload domain.TileCommitPayload) string {
	return domain.HashJSON(struct {
		Tile             domain.Tile        `json:"tile"`
		Candidates       []domain.Candidate `json:"candidates"`
		ImageHash        string             `json:"image_hash"`
		TemplateVersion  string             `json:"template_version"`
		ParamVersion     string             `json:"param_version"`
		TuningVersion    string             `json:"tuning_version"`
		AlgorithmVersion string             `json:"algorithm_version"`
	}{
		Tile:             payload.Tile,
		Candidates:       payload.Candidates,
		ImageHash:        payload.ImageHash,
		TemplateVersion:  payload.TemplateVersion,
		ParamVersion:     payload.ParamVersion,
		TuningVersion:    payload.TuningVersion,
		AlgorithmVersion: payload.AlgorithmVersion,
	})
}

func linePayloadHash(snapshot domain.LineCommitSnapshot) string {
	return domain.HashJSON(struct {
		Line             domain.DetectedLine    `json:"line"`
		Recognition      domain.LineRecognition `json:"recognition"`
		ImageHash        string                 `json:"image_hash"`
		ParamVersion     string                 `json:"param_version"`
		TuningVersion    string                 `json:"tuning_version"`
		TemplateVersion  string                 `json:"template_version"`
		AlgorithmVersion string                 `json:"algorithm_version"`
	}{
		Line:             snapshot.Line,
		Recognition:      snapshot.Recognition,
		ImageHash:        snapshot.ImageHash,
		ParamVersion:     snapshot.ParamVersion,
		TuningVersion:    snapshot.TuningVersion,
		TemplateVersion:  snapshot.TemplateVersion,
		AlgorithmVersion: snapshot.AlgorithmVersion,
	})
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
