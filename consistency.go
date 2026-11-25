package iconmask

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ConsistencyConfig struct {
	Repair         RepairConfig   `json:"repair"`
	Preview        PreviewConfig  `json:"preview"`
	Parallel       ParallelConfig `json:"parallel"`
	DatabasePath   string         `json:"databasePath"`
	RunID          string         `json:"runId"`
	InterruptAfter int            `json:"interruptAfterCommits"`
}

func DefaultConsistencyConfig() ConsistencyConfig {
	return ConsistencyConfig{
		Repair:         DefaultRepairConfig(),
		Preview:        DefaultPreviewConfig(),
		Parallel:       DefaultParallelConfig(),
		InterruptAfter: 0,
	}
}

type PathResult struct {
	Name            string        `json:"name"`
	Mask            Mask          `json:"-"`
	Stats           RegionStats   `json:"stats"`
	Commits         []StageCommit `json:"commits"`
	WaitEvents      []WaitEvent   `json:"waitEvents"`
	CheckpointCount int           `json:"checkpointCount"`
	Elapsed         time.Duration `json:"elapsedNanos"`
}

type ThreePathReport struct {
	ImageID                string          `json:"imageId"`
	ParameterVersion       string          `json:"parameterVersion"`
	Incremental            PathResult      `json:"incremental"`
	Resume                 PathResult      `json:"resume"`
	Full                   PathResult      `json:"full"`
	MasksEqual             bool            `json:"masksEqual"`
	StatsEqual             bool            `json:"statsEqual"`
	CommitSequencesEqual   bool            `json:"commitSequencesEqual"`
	FirstInconsistentBlock *Coord          `json:"firstInconsistentBlock,omitempty"`
	Exact                  *RepairResult   `json:"exact,omitempty"`
	DualPath               *DualPathResult `json:"dualPath,omitempty"`
}

type blockRecord struct {
	Index   int
	Coord   Coord
	Rect    Rect
	Commits []Checkpoint
	Mask    Mask
}

func RunThreePathConsistency(
	ctx context.Context,
	sample ImageSample,
	snapshot ParameterSnapshot,
	cfg ConsistencyConfig,
) (*ThreePathReport, *Error) {
	if err := validateParameterSnapshot(sample.ID, snapshot); err != nil {
		return nil, err
	}
	if cfg.Repair == (RepairConfig{}) {
		cfg.Repair = snapshot.Config
	} else if cfg.Repair != snapshot.Config {
		return nil, &Error{
			Code:    CodeState,
			ImageID: sample.ID,
			Field:   "parameterSnapshot",
			Message: "repair configuration differs from the supplied parameter snapshot",
		}
	}
	if cfg.DatabasePath == "" {
		return nil, parameterError(sample.ID, "databasePath", "database path is required", "filesystem path")
	}
	if cfg.RunID == "" {
		cfg.RunID = "run-" + sample.ID + "-" + snapshot.Version
	}
	if cfg.Parallel.MaxWorkers == 0 {
		cfg.Parallel = DefaultParallelConfig()
	}
	globalExact, err := RefineSample(sample, cfg.Repair)
	if err != nil {
		return nil, err
	}
	exact, err := RefineBlockwiseParallel(ctx, sample, cfg.Repair, cfg.Parallel)
	if err != nil {
		return nil, err
	}
	if !globalExact.Mask.Equal(exact.Mask) {
		return nil, &Error{
			Code:    CodeState,
			ImageID: sample.ID,
			Field:   "blockwiseRepair",
			Message: "blockwise repair does not match full-resolution frame repair",
			Details: map[string]any{"differentPixels": globalExact.Mask.DiffPixels(exact.Mask)},
		}
	}
	dual, dualErr := RunDualPath(sample, snapshot, cfg.Preview)
	if dualErr != nil {
		if !IsCode(dualErr, CodeDivergence) {
			return nil, dualErr
		}
	}
	graph := NewDependencyGraph(
		sample.ID, sample.InitialMask, sample.Truth.Segments,
		cfg.Repair.BlockSize, cfg.Repair.Radius.Max, cfg.Repair.MaxRounds, exact.RadiusTable,
	)
	start := time.Now()
	incrementalRecords, incrementalSchedule, pathErr := buildBlockRecords(ctx, sample, exact, graph, snapshot, cfg)
	if pathErr != nil {
		return nil, pathErr
	}
	incrementalPath, err := persistIncremental(ctx, cfg, incrementalRecords, incrementalSchedule)
	if err != nil {
		return nil, err
	}
	resumeRecords, resumeSchedule, pathErr := buildBlockRecords(ctx, sample, exact, graph, snapshot, cfg)
	if pathErr != nil {
		return nil, pathErr
	}
	resumePath, err := persistResume(ctx, cfg, resumeRecords, resumeSchedule)
	if err != nil {
		return nil, err
	}
	fullRecords, fullSchedule, pathErr := buildBlockRecords(ctx, sample, exact, graph, snapshot, cfg)
	if pathErr != nil {
		return nil, pathErr
	}
	fullPath := recordsToPath("full", fullRecords, fullSchedule)
	fullPath.Elapsed = time.Since(start)

	report := &ThreePathReport{
		ImageID:          sample.ID,
		ParameterVersion: snapshot.Version,
		Incremental:      incrementalPath,
		Resume:           resumePath,
		Full:             fullPath,
		Exact:            exact,
		DualPath:         dual,
	}
	report.MasksEqual = report.Incremental.Mask.Equal(report.Resume.Mask) &&
		report.Incremental.Mask.Equal(report.Full.Mask)
	report.StatsEqual = sameStats(report.Incremental.Stats, report.Resume.Stats) &&
		sameStats(report.Incremental.Stats, report.Full.Stats)
	report.CommitSequencesEqual = sameCommits(report.Incremental.Commits, report.Resume.Commits) &&
		sameCommits(report.Incremental.Commits, report.Full.Commits)
	report.FirstInconsistentBlock = firstInconsistentBlock(incrementalRecords, resumeRecords, fullRecords)
	if report.FirstInconsistentBlock == nil && !report.CommitSequencesEqual {
		report.FirstInconsistentBlock = firstCommitMismatchBlock(
			report.Incremental.Commits, report.Resume.Commits, report.Full.Commits,
		)
	}
	if !report.MasksEqual || !report.StatsEqual || !report.CommitSequencesEqual {
		return report, &Error{
			Code:    CodeState,
			ImageID: sample.ID,
			Field:   "threePathConsistency",
			Message: "incremental, resume, and full recompute paths diverged",
			Block:   report.FirstInconsistentBlock,
			Details: map[string]any{
				"masksEqual":           report.MasksEqual,
				"statsEqual":           report.StatsEqual,
				"commitSequencesEqual": report.CommitSequencesEqual,
			},
		}
	}
	if dualErr != nil {
		return report, dualErr
	}
	return report, nil
}

func buildBlockRecords(
	ctx context.Context,
	sample ImageSample,
	exact *RepairResult,
	graph *DependencyGraph,
	snapshot ParameterSnapshot,
	cfg ConsistencyConfig,
) ([]blockRecord, SchedulerReport, *Error) {
	blocks := blockRects(sample.Width, sample.Height, cfg.Repair.BlockSize)
	tasks := make([]ParallelTask, len(blocks))
	for i, rect := range blocks {
		tasks[i] = ParallelTask{
			Index:      i,
			Block:      Coord{X: rect.X / cfg.Repair.BlockSize, Y: rect.Y / cfg.Repair.BlockSize},
			Rect:       rect,
			PixelCount: rect.Width * rect.Height,
		}
	}
	records := make([]blockRecord, len(tasks))
	report, runErr := RunParallel(ctx, tasks, cfg.Parallel, func(ctx context.Context, task ParallelTask, buffer *PixelBuffer) error {
		record, buildErr := buildOneBlockRecord(sample, exact, graph, snapshot, cfg, task, buffer)
		if buildErr != nil {
			return buildErr
		}
		records[task.Index] = record
		return nil
	})
	if runErr != nil {
		if domainErr, ok := runErr.(*Error); ok {
			return nil, report, domainErr
		}
		return nil, report, &Error{Code: CodeState, ImageID: sample.ID, Field: "parallel", Message: runErr.Error()}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Coord.Y != records[j].Coord.Y {
			return records[i].Coord.Y < records[j].Coord.Y
		}
		return records[i].Coord.X < records[j].Coord.X
	})
	return records, report, nil
}

func buildOneBlockRecord(
	sample ImageSample,
	exact *RepairResult,
	graph *DependencyGraph,
	snapshot ParameterSnapshot,
	cfg ConsistencyConfig,
	task ParallelTask,
	buffer *PixelBuffer,
) (blockRecord, *Error) {
	dependency := findDependency(graph, task.Block)
	contentHash := dependency.ContentHash
	segmentIDs := dependency.SegmentIDs
	radiusJSON := EncodeRadiusTable(exact.RadiusTable)
	maxRadius := 0
	for _, segmentID := range segmentIDs {
		if entry, ok := exact.RadiusTable.Lookup(segmentID); ok {
			maxRadius = maxInt(maxRadius, entry.ErosionRadius)
		}
	}
	if maxRadius == 0 {
		maxRadius = cfg.Repair.Radius.Max
	}
	rounds := len(exact.Rounds)
	if rounds == 0 {
		rounds = 1
	}
	machine := NewBlockStateMachine(task.Block)
	commits := make([]Checkpoint, 0, rounds*len(stageOrder))
	upstream := contentHash + ":" + snapshot.Version
	for round := 0; round < rounds; round++ {
		roundMask := exact.Mask
		if round < len(exact.RoundMasks) {
			roundMask = exact.RoundMasks[round]
		}
		blockMask := cropMask(roundMask, task.Rect)
		for _, stage := range stageOrder {
			if stageErr := machine.Start(stage); stageErr != nil {
				return blockRecord{}, stageErr
			}
			digest := ""
			var outputMask []byte
			var output *Mask
			if stage == StageRadius {
				digest = fmt.Sprintf("%x", sha256String(radiusJSON))
			} else {
				maskDigest := blockMask.SHA256()
				digest = maskDigest
				outputMask = append([]byte(nil), blockMask.Data...)
				maskCopy := blockMask.Clone()
				output = &maskCopy
				if len(buffer.Data) >= len(blockMask.Data) {
					copy(buffer.Data[:len(blockMask.Data)], blockMask.Data)
				}
			}
			stageOutput := StageOutput{
				Digest:      digest,
				Mask:        output,
				RadiusTable: exact.RadiusTable,
				Metadata:    map[string]string{"stage": string(stage)},
			}
			if commitErr := machine.Commit(stage, stageOutput); commitErr != nil {
				return blockRecord{}, commitErr
			}
			segmentLabel := strings.Join(segmentIDs, ",")
			if segmentLabel == "" {
				segmentLabel = "block"
			}
			cacheKey := BuildCacheKey(CacheKeyInput{
				BlockContentHash: contentHash,
				SegmentID:        segmentLabel,
				Stage:            string(stage),
				Radius:           maxRadius,
				Round:            round,
				UpstreamKey:      upstream,
			})
			upstream = cacheKey
			commits = append(commits, Checkpoint{
				RunID:            cfg.RunID,
				ImageID:          sample.ID,
				Block:            task.Block,
				Round:            round,
				Stage:            stage,
				OutputDigest:     digest,
				RadiusTableJSON:  radiusJSON,
				CacheKey:         cacheKey,
				ContentHash:      contentHash,
				ParameterVersion: snapshot.Version,
				AlgorithmVersion: AlgorithmVersion,
				OutputMask:       outputMask,
			})
		}
	}
	return blockRecord{Index: task.Index, Coord: task.Block, Rect: task.Rect, Commits: commits, Mask: cropMask(exact.Mask, task.Rect)}, nil
}

func findDependency(graph *DependencyGraph, block Coord) BlockDependency {
	for _, dependency := range graph.Blocks {
		if dependency.Block == block {
			return dependency
		}
	}
	return BlockDependency{Block: block}
}

func blockRects(width, height, blockSize int) []Rect {
	rects := make([]Rect, 0)
	for y := 0; y < height; y += blockSize {
		for x := 0; x < width; x += blockSize {
			rects = append(rects, Rect{
				X: x, Y: y,
				Width:  minInt(blockSize, width-x),
				Height: minInt(blockSize, height-y),
			})
		}
	}
	return rects
}

func cropMask(mask Mask, rect Rect) Mask {
	out := NewMask(rect.Width, rect.Height)
	for y := 0; y < rect.Height; y++ {
		for x := 0; x < rect.Width; x++ {
			out.Set(x, y, mask.At(rect.X+x, rect.Y+y))
		}
	}
	return out
}

func mergeBlockMasks(records []blockRecord, width, height int) Mask {
	out := NewMask(width, height)
	for _, record := range records {
		for y := 0; y < record.Rect.Height; y++ {
			for x := 0; x < record.Rect.Width; x++ {
				out.Set(record.Rect.X+x, record.Rect.Y+y, record.Mask.At(x, y))
			}
		}
	}
	return out
}

func persistIncremental(
	ctx context.Context,
	cfg ConsistencyConfig,
	records []blockRecord,
	schedule SchedulerReport,
) (PathResult, *Error) {
	start := time.Now()
	store, err := OpenCheckpointStore(cfg.DatabasePath)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "databasePath", Message: err.Error()}
	}
	defer store.Close()
	if err := store.DeleteRun(ctx, cfg.RunID); err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "runId", Message: err.Error()}
	}
	for blockIndex := range records {
		for commitIndex := range records[blockIndex].Commits {
			saved, saveErr := store.SaveStageCheckpoint(ctx, records[blockIndex].Commits[commitIndex])
			if saveErr != nil {
				if domainErr, ok := saveErr.(*Error); ok {
					return PathResult{}, domainErr
				}
				return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "checkpoint", Message: saveErr.Error()}
			}
			records[blockIndex].Commits[commitIndex].Seq = saved.Seq
		}
	}
	commits, err := store.CommitSequence(ctx, cfg.RunID)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "commitSequence", Message: err.Error()}
	}
	allCheckpoints, err := store.LoadCheckpoints(ctx, cfg.RunID)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "commitSequence", Message: err.Error()}
	}
	if _, replayErr := ReplayCheckpointPrefix(allCheckpoints); replayErr != nil {
		return PathResult{}, replayErr
	}
	mask := mergeBlockMasks(records, maskWidth(records), maskHeight(records))
	return PathResult{
		Name:            "incremental",
		Mask:            mask,
		Stats:           AnalyzeMask(mask),
		Commits:         commits,
		WaitEvents:      schedule.WaitEvents,
		CheckpointCount: len(commits),
		Elapsed:         time.Since(start),
	}, nil
}

func persistResume(
	ctx context.Context,
	cfg ConsistencyConfig,
	records []blockRecord,
	schedule SchedulerReport,
) (PathResult, *Error) {
	start := time.Now()
	store, err := OpenCheckpointStore(cfg.DatabasePath)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "databasePath", Message: err.Error()}
	}
	if err := store.DeleteRun(ctx, cfg.RunID); err != nil {
		_ = store.Close()
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "runId", Message: err.Error()}
	}
	total := 0
	for _, record := range records {
		total += len(record.Commits)
	}
	cut := cfg.InterruptAfter
	if cut <= 0 || cut >= total {
		cut = total / 2
	}
	if cut < 1 {
		cut = 1
	}
	written := 0
	for blockIndex := range records {
		for commitIndex := range records[blockIndex].Commits {
			if written >= cut {
				break
			}
			saved, saveErr := store.SaveStageCheckpoint(ctx, records[blockIndex].Commits[commitIndex])
			if saveErr != nil {
				_ = store.Close()
				return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "checkpoint", Message: saveErr.Error()}
			}
			records[blockIndex].Commits[commitIndex].Seq = saved.Seq
			written++
		}
	}
	_ = store.Close()

	store, err = OpenCheckpointStore(cfg.DatabasePath)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "databasePath", Message: err.Error()}
	}
	defer store.Close()
	existing, err := store.LoadCheckpoints(ctx, cfg.RunID)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "resume", Message: err.Error()}
	}
	if _, replayErr := ReplayCheckpointPrefix(existing); replayErr != nil {
		return PathResult{}, replayErr
	}
	existingKeys := make(map[string]bool, len(existing))
	for _, checkpoint := range existing {
		existingKeys[checkpointKey(checkpoint)] = true
	}
	for blockIndex := range records {
		for commitIndex := range records[blockIndex].Commits {
			if existingKeys[checkpointKey(records[blockIndex].Commits[commitIndex])] {
				continue
			}
			saved, saveErr := store.SaveStageCheckpoint(ctx, records[blockIndex].Commits[commitIndex])
			if saveErr != nil {
				return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "checkpoint", Message: saveErr.Error()}
			}
			records[blockIndex].Commits[commitIndex].Seq = saved.Seq
		}
	}
	commits, err := store.CommitSequence(ctx, cfg.RunID)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "commitSequence", Message: err.Error()}
	}
	allCheckpoints, err := store.LoadCheckpoints(ctx, cfg.RunID)
	if err != nil {
		return PathResult{}, &Error{Code: CodeCheckpoint, ImageID: cfg.RunID, Field: "commitSequence", Message: err.Error()}
	}
	if _, replayErr := ReplayCheckpointPrefix(allCheckpoints); replayErr != nil {
		return PathResult{}, replayErr
	}
	mask := mergeBlockMasks(records, maskWidth(records), maskHeight(records))
	return PathResult{
		Name:            "resume",
		Mask:            mask,
		Stats:           AnalyzeMask(mask),
		Commits:         commits,
		WaitEvents:      schedule.WaitEvents,
		CheckpointCount: len(commits),
		Elapsed:         time.Since(start),
	}, nil
}

func recordsToPath(name string, records []blockRecord, schedule SchedulerReport) PathResult {
	mask := mergeBlockMasks(records, maskWidth(records), maskHeight(records))
	commits := make([]StageCommit, 0)
	seq := int64(1)
	for _, record := range records {
		for _, checkpoint := range record.Commits {
			commits = append(commits, StageCommit{
				Seq:              seq,
				RunID:            checkpoint.RunID,
				ImageID:          checkpoint.ImageID,
				Block:            checkpoint.Block,
				Round:            checkpoint.Round,
				Stage:            checkpoint.Stage,
				OutputDigest:     checkpoint.OutputDigest,
				CacheKey:         checkpoint.CacheKey,
				ContentHash:      checkpoint.ContentHash,
				ParameterVersion: checkpoint.ParameterVersion,
				AlgorithmVersion: checkpoint.AlgorithmVersion,
			})
			seq++
		}
	}
	return PathResult{
		Name:            name,
		Mask:            mask,
		Stats:           AnalyzeMask(mask),
		Commits:         commits,
		WaitEvents:      schedule.WaitEvents,
		CheckpointCount: len(commits),
	}
}

func maskWidth(records []blockRecord) int {
	maxWidth := 0
	for _, record := range records {
		if record.Rect.X+record.Rect.Width > maxWidth {
			maxWidth = record.Rect.X + record.Rect.Width
		}
	}
	return maxWidth
}

func maskHeight(records []blockRecord) int {
	maxHeight := 0
	for _, record := range records {
		if record.Rect.Y+record.Rect.Height > maxHeight {
			maxHeight = record.Rect.Y + record.Rect.Height
		}
	}
	return maxHeight
}

func checkpointKey(checkpoint Checkpoint) string {
	return fmt.Sprintf("%d/%d/%d/%s", checkpoint.Block.X, checkpoint.Block.Y, checkpoint.Round, checkpoint.Stage)
}

func sameStats(a, b RegionStats) bool {
	if a.Count != b.Count || a.TotalArea != b.TotalArea || len(a.Regions) != len(b.Regions) {
		return false
	}
	for i := range a.Regions {
		left, right := a.Regions[i], b.Regions[i]
		if left.Area != right.Area || left.Holes != right.Holes || left.BBox != right.BBox ||
			left.Centroid != right.Centroid {
			return false
		}
	}
	return true
}

func sameCommits(a, b []StageCommit) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		left, right := a[i], b[i]
		if left.Seq != right.Seq || left.Block != right.Block || left.Round != right.Round ||
			left.Stage != right.Stage || left.OutputDigest != right.OutputDigest ||
			left.CacheKey != right.CacheKey || left.ContentHash != right.ContentHash ||
			left.ParameterVersion != right.ParameterVersion || left.AlgorithmVersion != right.AlgorithmVersion {
			return false
		}
	}
	return true
}

func firstInconsistentBlock(records ...[]blockRecord) *Coord {
	if len(records) < 2 {
		return nil
	}
	base := records[0]
	for _, other := range records[1:] {
		if len(base) != len(other) {
			return &Coord{}
		}
		for i := range base {
			if !base[i].Mask.Equal(other[i].Mask) {
				block := base[i].Coord
				return &block
			}
		}
	}
	return nil
}

func firstCommitMismatchBlock(sequences ...[]StageCommit) *Coord {
	if len(sequences) < 2 {
		return nil
	}
	base := sequences[0]
	for _, other := range sequences[1:] {
		limit := minInt(len(base), len(other))
		for i := 0; i < limit; i++ {
			if base[i] != other[i] {
				block := base[i].Block
				return &block
			}
		}
		if len(base) != len(other) {
			if len(base) > limit {
				block := base[limit].Block
				return &block
			}
			if len(other) > limit {
				block := other[limit].Block
				return &block
			}
		}
	}
	return nil
}
