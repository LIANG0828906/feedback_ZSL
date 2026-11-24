package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/fsm"
	"ocrworkbench/internal/loader"
	"ocrworkbench/internal/store"
	"ocrworkbench/internal/vision"
)

const AlgorithmVersion = "ocr-pipeline-v3"

type Faults struct {
	Tile                  func(imageID string, tileIndex int) error
	Line                  func(imageID string, lineIndex int) error
	CrashAfterTileCommits int
	CrashAfterLineCommits int
}

type Runner struct {
	Store            *store.Store
	Detector         core.DetectorParams
	Segment          core.SegmentParams
	Control          *Control
	Faults           Faults
	TemplateVersion  string
	AlgorithmVersion string
}

type UnitIssue struct {
	Kind    string
	ImageID string
	Index   int
	Err     error
}

type Issues []UnitIssue

func (i Issues) Error() string {
	parts := make([]string, 0, len(i))
	for _, issue := range i {
		parts = append(parts, fmt.Sprintf("%s %s[%d]: %v", issue.Kind, issue.ImageID, issue.Index, issue.Err))
	}
	return strings.Join(parts, "; ")
}

func (r *Runner) Run(ctx context.Context, manifest core.Manifest, datasetDir, kind string) (core.RunResult, error) {
	if r.TemplateVersion == "" {
		r.TemplateVersion = vision.TemplateVersion()
	}
	if r.AlgorithmVersion == "" {
		r.AlgorithmVersion = AlgorithmVersion
	}
	if err := core.ValidateDetectorParams(r.Detector); err != nil {
		return core.RunResult{}, err
	}
	if err := core.ValidateSegmentParams(r.Segment); err != nil {
		return core.RunResult{}, err
	}
	if manifest.TemplateVersion != "" && manifest.TemplateVersion != r.TemplateVersion {
		return core.RunResult{}, core.FieldError(core.CodeMeta, "template_version", "",
			fmt.Sprintf("manifest template %s does not match binary template %s", manifest.TemplateVersion, r.TemplateVersion))
	}
	truthHash := manifest.TruthFingerprint
	if truthHash == "" {
		var err error
		truthHash, err = core.TruthFingerprint(manifest)
		if err != nil {
			return core.RunResult{}, err
		}
	}
	parameterHash, err := core.HashAny(struct {
		Detector core.DetectorParams `json:"detector"`
		Segment  core.SegmentParams  `json:"segment"`
	}{r.Detector, r.Segment})
	if err != nil {
		return core.RunResult{}, err
	}
	result := core.RunResult{
		RunKind:          kind,
		Seed:             manifest.Seed,
		TemplateVersion:  r.TemplateVersion,
		ParameterVersion: parameterHash,
		AlgorithmVersion: r.AlgorithmVersion,
	}
	var allIssues Issues
	for _, truth := range manifest.Images {
		imageResult, issues, runErr := r.runImage(ctx, datasetDir, truth, truthHash, parameterHash, kind)
		result.Images = append(result.Images, imageResult)
		allIssues = append(allIssues, issues...)
		if runErr != nil {
			if errors.Is(runErr, ErrInjectedCrash) || errors.Is(runErr, ErrCanceled) ||
				errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
				return result, runErr
			}
			return result, runErr
		}
	}
	if len(allIssues) > 0 {
		return result, allIssues
	}
	return result, nil
}

func (r *Runner) runImage(
	ctx context.Context,
	datasetDir string,
	truth core.ImageTruth,
	truthHash string,
	parameterHash string,
	kind string,
) (core.ImageResult, Issues, error) {
	control := r.Control
	if control == nil {
		control = NewControl()
	}
	if err := control.Wait(ctx); err != nil {
		return core.ImageResult{ImageID: truth.ID}, nil, err
	}
	loaded, err := loader.Load(datasetDir, truth)
	if err != nil {
		return core.ImageResult{ImageID: truth.ID}, nil, err
	}
	key := store.Key{
		ImageID:          truth.ID,
		ContentHash:      loaded.Hash,
		TruthHash:        truthHash,
		TemplateVersion:  r.TemplateVersion,
		ParameterVersion: parameterHash,
		AlgorithmVersion: r.AlgorithmVersion,
	}
	var (
		snapshotID int64
		snapshot   *store.Snapshot
	)
	if r.Store != nil && kind != "full" {
		var invalidated bool
		snapshotID, snapshot, invalidated, err = r.Store.Prepare(ctx, key)
		if err != nil {
			return core.ImageResult{ImageID: truth.ID}, nil, err
		}
		_ = invalidated
	}

	gray := vision.ToGray(loaded.Image)
	tilePlan := vision.PlanTiles(loaded.Width, loaded.Height, r.Detector)
	tileResults := make([]core.TileResult, len(tilePlan))
	tileStates := make([]*fsm.TileState, len(tilePlan))
	tileLoaded := make([]bool, len(tilePlan))
	var knownTiles []core.TileResult
	if snapshot != nil {
		knownTiles = snapshot.Tiles
	}
	loadedTiles := map[int]core.TileResult{}
	for _, tile := range knownTiles {
		loadedTiles[tile.Index] = tile
	}
	for i, tile := range tilePlan {
		state := fsm.NewTile(i)
		tileStates[i] = state
		if loadedTile, ok := loadedTiles[i]; ok {
			_ = state.Transition(fsm.TileDetecting)
			_ = state.Transition(fsm.TileCommitted)
			tileResults[i] = loadedTile
			tileLoaded[i] = true
			continue
		}
		tileResults[i] = core.TileResult{
			ImageID:  truth.ID,
			Index:    tile.Index,
			Row:      tile.Row,
			Col:      tile.Col,
			Core:     tile.Core,
			Expanded: tile.Expanded,
			State:    string(fsm.TilePending),
		}
	}

	var issues Issues
	pendingTiles := make([]int, 0, len(tilePlan))
	tileCosts := make([]int64, 0, len(tilePlan))
	for i := range tilePlan {
		if tileLoaded[i] {
			continue
		}
		pendingTiles = append(pendingTiles, i)
		tileCosts = append(tileCosts, int64(tilePlan[i].Expanded.Pixels())*2+int64(loaded.Width*loaded.Height)*18)
	}
	buffer := NewBufferPool(r.Detector.MaxWorkers, int64(r.Detector.BufferBytesPerWorker))
	tileErrors := RunIndexed(ctx, len(pendingTiles), r.Detector.MaxWorkers, tileCosts, buffer, func(job int) (runErr error) {
		index := pendingTiles[job]
		state := tileStates[index]
		if err := control.Wait(ctx); err != nil {
			return err
		}
		if err := state.Transition(fsm.TileDetecting); err != nil {
			return err
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				_ = state.Transition(fsm.TileFailed)
				tileResults[index].State = string(fsm.TileFailed)
				tileResults[index].Error = fmt.Sprintf("tile panic: %v", recovered)
				runErr = fmt.Errorf("tile %d panic: %v", index, recovered)
			}
		}()
		if r.Faults.Tile != nil {
			if err := r.Faults.Tile(truth.ID, index); err != nil {
				_ = state.Transition(fsm.TileFailed)
				tileResults[index].State = string(fsm.TileFailed)
				tileResults[index].Error = err.Error()
				return err
			}
		}
		candidates, err := vision.DetectTile(gray, tilePlan[index], r.Detector)
		if err != nil {
			_ = state.Transition(fsm.TileFailed)
			tileResults[index].State = string(fsm.TileFailed)
			tileResults[index].Error = err.Error()
			return err
		}
		tileResults[index].Candidates = candidates
		return nil
	})
	for job, index := range pendingTiles {
		if err := tileErrors[job]; err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrCanceled) {
				return r.finalizeImage(truth, loaded, key, kind, tileResults, nil, parameterHash, truthHash), issues, err
			}
			issues = append(issues, UnitIssue{Kind: "tile", ImageID: truth.ID, Index: index, Err: err})
		}
	}
	committedTileCount := 0
	for i := range tilePlan {
		if tileLoaded[i] {
			committedTileCount++
			continue
		}
		if tileStates[i].PhaseValue() == fsm.TileFailed {
			continue
		}
		if err := control.Wait(ctx); err != nil {
			return r.finalizeImage(truth, loaded, key, kind, tileResults, nil, parameterHash, truthHash), issues, err
		}
		tileResults[i].State = string(fsm.TileCommitted)
		tileResults[i].CommittedSeq = i
		tileResults[i].ContentHash = store.TileHash(key, tileResults[i])
		if r.Store != nil && kind != "full" {
			if err := r.Store.SaveTile(ctx, snapshotID, key, tileResults[i]); err != nil {
				return core.ImageResult{ImageID: truth.ID}, issues, err
			}
		}
		if err := tileStates[i].Transition(fsm.TileCommitted); err != nil {
			return core.ImageResult{ImageID: truth.ID}, issues, err
		}
		committedTileCount++
		if r.Faults.CrashAfterTileCommits > 0 && committedTileCount >= r.Faults.CrashAfterTileCommits {
			return r.finalizeImage(truth, loaded, key, kind, tileResults, nil, parameterHash, truthHash), issues, ErrInjectedCrash
		}
	}

	var candidates []core.TextLine
	for _, tile := range tileResults {
		if tile.State == string(fsm.TileCommitted) {
			candidates = append(candidates, tile.Candidates...)
		}
	}
	merged, err := vision.MergeTileCandidates(candidates, r.Detector)
	if err != nil {
		return core.ImageResult{ImageID: truth.ID}, issues, err
	}
	lineStates := make([]*fsm.LineState, len(merged))
	lineResults := make([]core.LineResult, len(merged))
	lineLoaded := make([]bool, len(merged))
	loadedLines := map[int]core.LineResult{}
	if snapshot != nil {
		for _, line := range snapshot.Lines {
			loadedLines[line.Number] = line
		}
	}
	for i, line := range merged {
		state := fsm.NewLine(i)
		lineStates[i] = state
		if loadedLine, ok := loadedLines[i]; ok {
			if core.BoxIoU(loadedLine.Box, line.Box) < 1-1e-9 {
				if r.Store != nil {
					_ = r.Store.Invalidate(ctx, snapshotID, "redetected line box does not match checkpoint")
				}
				return core.ImageResult{ImageID: truth.ID}, issues, core.NewError(core.CodeState, "checkpoint line geometry mismatch; invalidate and recompute")
			}
			_ = state.Transition(fsm.LineRecognizing)
			_ = state.Transition(fsm.LineCommitted)
			lineResults[i] = loadedLine
			lineLoaded[i] = true
			continue
		}
		lineResults[i] = core.LineResult{
			ImageID:       truth.ID,
			Number:        i,
			ReadingIndex:  line.ReadingIndex,
			TileIndex:     firstSourceTile(line.SourceTiles),
			Box:           line.Box,
			State:         string(fsm.LinePending),
			StrokeWidth:   line.StrokeWidth,
			SegmentParams: r.Segment,
			ParameterHash: parameterHash,
			Characters:    []core.CharResult{},
		}
	}
	pendingLines := make([]int, 0, len(merged))
	lineCosts := make([]int64, 0, len(merged))
	for i, line := range merged {
		if lineLoaded[i] {
			continue
		}
		pendingLines = append(pendingLines, i)
		roi := line.AxisAligned()
		lineCosts = append(lineCosts, int64(roi.Pixels())*2+int64(loaded.Width*loaded.Height)*18)
	}
	recognizer := vision.Recognizer{
		Params:        r.Segment,
		ImageID:       truth.ID,
		ParameterHash: parameterHash,
		Fault: func(lineIndex int) error {
			if r.Faults.Line == nil {
				return nil
			}
			return r.Faults.Line(truth.ID, lineIndex)
		},
	}
	lineErrors := RunIndexed(ctx, len(pendingLines), r.Detector.MaxWorkers, lineCosts, buffer, func(job int) (runErr error) {
		index := pendingLines[job]
		state := lineStates[index]
		if err := control.Wait(ctx); err != nil {
			return err
		}
		if err := state.Transition(fsm.LineRecognizing); err != nil {
			return err
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				_ = state.Transition(fsm.LineFailed)
				lineResults[index].State = string(fsm.LineFailed)
				lineResults[index].Error = fmt.Sprintf("line panic: %v", recovered)
				runErr = fmt.Errorf("line %d panic: %v", index, recovered)
			}
		}()
		result, err := recognizer.Recognize(loaded.Image, merged[index])
		if err != nil {
			_ = state.Transition(fsm.LineFailed)
			lineResults[index].State = string(fsm.LineFailed)
			lineResults[index].Error = err.Error()
			return err
		}
		lineResults[index] = result
		lineResults[index].State = string(fsm.LineRecognizing)
		return nil
	})
	for job, index := range pendingLines {
		if err := lineErrors[job]; err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrCanceled) {
				return r.finalizeImage(truth, loaded, key, kind, tileResults, lineResults, parameterHash, truthHash), issues, err
			}
			issues = append(issues, UnitIssue{Kind: "line", ImageID: truth.ID, Index: index, Err: err})
		}
	}
	committedLineCount := 0
	for i := range merged {
		if lineLoaded[i] {
			committedLineCount++
			continue
		}
		if lineStates[i].PhaseValue() == fsm.LineFailed {
			continue
		}
		if err := control.Wait(ctx); err != nil {
			return r.finalizeImage(truth, loaded, key, kind, tileResults, lineResults, parameterHash, truthHash), issues, err
		}
		lineResults[i].State = string(fsm.LineCommitted)
		lineResults[i].CommittedSeq = i
		lineResults[i].ContentHash = store.LineHash(key, lineResults[i])
		if r.Store != nil && kind != "full" {
			if err := r.Store.SaveLine(ctx, snapshotID, key, lineResults[i]); err != nil {
				return core.ImageResult{ImageID: truth.ID}, issues, err
			}
		}
		if err := lineStates[i].Transition(fsm.LineCommitted); err != nil {
			return core.ImageResult{ImageID: truth.ID}, issues, err
		}
		committedLineCount++
		if r.Faults.CrashAfterLineCommits > 0 && committedLineCount >= r.Faults.CrashAfterLineCommits {
			return r.finalizeImage(truth, loaded, key, kind, tileResults, lineResults, parameterHash, truthHash), issues, ErrInjectedCrash
		}
	}
	result := r.finalizeImage(truth, loaded, key, kind, tileResults, lineResults, parameterHash, truthHash)
	if len(issues) == 0 {
		if r.Store != nil && kind != "full" {
			if err := r.Store.MarkComplete(ctx, snapshotID); err != nil {
				return result, issues, err
			}
		}
	}
	return result, issues, nil
}

func (r *Runner) finalizeImage(
	truth core.ImageTruth,
	loaded loader.LoadedImage,
	key store.Key,
	kind string,
	tileResults []core.TileResult,
	lineResults []core.LineResult,
	parameterHash, truthHash string,
) core.ImageResult {
	tiles := append([]core.TileResult(nil), tileResults...)
	lines := append([]core.LineResult(nil), lineResults...)
	for i := range lines {
		if lines[i].State == string(fsm.LineRecognizing) {
			lines[i].State = string(fsm.LinePending)
		}
	}
	result := core.ImageResult{
		ImageID:          truth.ID,
		TruthFingerprint: truthHash,
		ImageHash:        loaded.Hash,
		TemplateVersion:  r.TemplateVersion,
		ParameterVersion: parameterHash,
		AlgorithmVersion: r.AlgorithmVersion,
		Tiles:            tiles,
		Lines:            lines,
	}
	for i := range result.Tiles {
		if result.Tiles[i].ContentHash == "" {
			result.Tiles[i].ContentHash = store.TileHash(key, result.Tiles[i])
		}
	}
	for i := range result.Lines {
		if result.Lines[i].ContentHash == "" {
			result.Lines[i].ContentHash = store.LineHash(key, result.Lines[i])
		}
	}
	AttachTruth(&result, truth)
	result.Stats, result.Progress = BuildStats(result.Tiles, result.Lines)
	result.Commits = CommitSequence(result.Tiles, result.Lines)
	_ = kind
	return result
}

func firstSourceTile(tiles []int) int {
	if len(tiles) == 0 {
		return 0
	}
	return tiles[0]
}

func RequiredBand(params core.DetectorParams) int {
	return core.RequiredBand(params)
}

func ValidateTileBand(params core.DetectorParams) error {
	return core.ValidateDetectorParams(params)
}
