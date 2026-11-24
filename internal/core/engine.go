package core

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"time"
)

type RunMode string

const (
	RunRealtime RunMode = "realtime"
	RunResume   RunMode = "resume"
	RunFull     RunMode = "full"
)

type EngineEvent struct {
	Type     string     `json:"type"`
	Stage    string     `json:"stage,omitempty"`
	Block    int        `json:"block,omitempty"`
	State    BlockState `json:"state,omitempty"`
	Reason   string     `json:"reason,omitempty"`
	AtUnixMS int64      `json:"atUnixMs"`
}

type RunOptions struct {
	Mode            RunMode
	Grid            Grid
	Workers         int
	PerWorkerPixels int
	Checkpoint      *CheckpointStore
	Faults          map[CommitKey]string
	OnEvent         func(EngineEvent)
	Control         *RunControl
}

type RunResult struct {
	Mode                  RunMode            `json:"mode"`
	Status                string             `json:"status"`
	ImageFingerprint      string             `json:"imageFingerprint"`
	ParamVersion          string             `json:"paramVersion"`
	AlgorithmVersion      string             `json:"algorithmVersion"`
	Params                Params             `json:"params"`
	Final                 Image              `json:"-"`
	Stages                []StageResult      `json:"stages"`
	Stats                 Geometry           `json:"stats"`
	TruthDeviation        float64            `json:"truthDeviation"`
	CommitSequence        []CommitKey        `json:"commitSequence"`
	BlockStates           []StageBlockMatrix `json:"blockStates"`
	FailedBlocks          []CommitKey        `json:"failedBlocks"`
	CheckpointInvalidated bool               `json:"checkpointInvalidated"`
	CheckpointReason      string             `json:"checkpointReason,omitempty"`
	RestoredCommits       int                `json:"restoredCommits"`
	PoolStats             PoolStats          `json:"poolStats"`
	ElapsedMS             float64            `json:"elapsedMs"`
}

type blockResult struct {
	stage string
	block int
	hash  string
	err   error
}

func RunPipeline(ctx context.Context, src Image, params Params, truth Truth, opts RunOptions) (RunResult, error) {
	start := time.Now()
	if err := src.Validate(); err != nil {
		return RunResult{}, err
	}
	if err := params.Validate(); err != nil {
		return RunResult{}, err
	}
	switch opts.Mode {
	case RunRealtime, RunResume, RunFull:
	default:
		return RunResult{}, NewError(CodeParam, "mode", "run mode must be realtime, resume, or full")
	}
	if opts.Workers == 0 {
		opts.Workers = min(4, max(1, runtime.GOMAXPROCS(0)))
	}
	if opts.PerWorkerPixels == 0 {
		opts.PerWorkerPixels = 512 * 1024
	}
	if opts.Grid.BlockW == 0 {
		opts.Grid = BuildGrid(src.Width, src.Height, 96, 96)
	}
	if len(opts.Grid.Blocks) == 0 {
		opts.Grid = BuildGrid(src.Width, src.Height, 96, 96)
	}
	pool, err := NewWorkerPool(opts.Workers, opts.PerWorkerPixels)
	if err != nil {
		return RunResult{}, err
	}
	defer pool.Close()

	imageFP := FingerprintImage(src)
	result := RunResult{
		Mode:             opts.Mode,
		ImageFingerprint: imageFP,
		ParamVersion:     params.Version(),
		AlgorithmVersion: AlgorithmVersion,
		Params:           params,
	}
	if opts.Mode == RunRealtime && opts.Checkpoint != nil {
		// A realtime run is a fresh canonical sequence for this image. Removing
		// any previous parameter/algorithm version prevents mixed snapshots from
		// surviving on disk after interactive parameter changes.
		if err := opts.Checkpoint.WipeImage(ctx, imageFP); err != nil {
			return RunResult{}, err
		}
	}
	var snapshot *Snapshot
	if opts.Mode == RunResume && opts.Checkpoint != nil {
		snapshot, err = opts.Checkpoint.LoadSnapshot(ctx, imageFP, params, AlgorithmVersion, src.Width, src.Height, src.Channels, opts.Grid)
		if err != nil {
			return RunResult{}, err
		}
		if snapshot != nil && snapshot.Invalidated {
			result.CheckpointInvalidated = true
			result.CheckpointReason = snapshot.Reason
		}
	}

	computed, err := ComputeStages(src, params)
	if err != nil {
		return RunResult{}, err
	}
	machine := NewStateMachine(opts.Grid)
	stageImages := make([]Image, 0, len(computed))
	result.Stages = make([]StageResult, 0, len(computed))
	result.CommitSequence = make([]CommitKey, 0, len(StageOrder)*len(opts.Grid.Blocks))
	result.FailedBlocks = []CommitKey{}
	failedAny := false

	for stageIdx, stageResult := range computed {
		if opts.Control != nil {
			if err := opts.Control.Wait(ctx); err != nil {
				return result, err
			}
		}
		stage := stageResult.Stage
		useRestored := false
		if opts.Mode == RunResume && snapshot != nil && snapshot.Valid && snapshot.CompleteSet[stage] {
			snapImage := snapshot.Stages[stage]
			if FingerprintImage(snapImage) == stageResult.Fingerprint {
				stageImages = append(stageImages, snapImage)
				result.Stages = append(result.Stages, makeStageResult(stage, snapImage, 0))
				for _, block := range opts.Grid.Blocks {
					machine.RestoreCommitted(stage, block.Index)
					result.CommitSequence = append(result.CommitSequence, CommitKey{Stage: stage, Block: block.Index})
					result.RestoredCommits++
					emit(opts.OnEvent, EngineEvent{Type: "restored", Stage: stage, Block: block.Index, State: BlockCommitted})
				}
				useRestored = true
			} else {
				if opts.Checkpoint != nil {
					_ = opts.Checkpoint.WipeImage(context.Background(), imageFP)
				}
				result.CheckpointInvalidated = true
				result.CheckpointReason = "reconstructed stage fingerprint mismatch"
			}
		}
		if useRestored {
			continue
		}
		stageImage := stageResult.Image()
		stageImages = append(stageImages, stageImage)
		result.Stages = append(result.Stages, stageResult)
		hasFailure := processStageBlocks(ctx, opts, pool, machine, imageFP, params, stage, stageImage, &result)
		if hasFailure {
			failedAny = true
		}
		_ = stageIdx
	}

	final := stageImages[len(stageImages)-1]
	result.Final = final
	result.Stats = AnalyzeGeometry(final, truth)
	result.TruthDeviation = GeometryTrueDeviation(result.Stats)
	result.BlockStates = machine.Matrix()
	result.PoolStats = pool.Stats()
	if failedAny || len(result.FailedBlocks) > 0 {
		result.Status = "failed"
	} else {
		result.Status = "complete"
	}
	result.ElapsedMS = float64(time.Since(start).Microseconds()) / 1000.0
	return result, nil
}

func processStageBlocks(ctx context.Context, opts RunOptions, pool *WorkerPool, machine *StateMachine, imageFP string, params Params, stage string, stageImage Image, result *RunResult) bool {
	n := len(opts.Grid.Blocks)
	results := make(chan blockResult, n)
	submitted := 0
	for _, block := range opts.Grid.Blocks {
		block := block
		crop := cropImage(stageImage, block)
		key := CommitKey{Stage: stage, Block: block.Index}
		label := fmt.Sprintf("%s:%d", stage, block.Index)
		err := pool.Submit(ctx, label, crop.PixelCount(), func(buffer []byte) error {
			res := blockResult{stage: stage, block: block.Index}
			defer func() {
				if recovered := recover(); recovered != nil {
					res.err = NewError(CodeState, "panic", fmt.Sprintf("block stage panic: %v", recovered))
					if state, ok := machine.State(stage, block.Index); ok && state == BlockProcessing {
						_ = machine.Fail(stage, block.Index, res.err.Error())
					}
					results <- res
				}
			}()
			if opts.Control != nil {
				if err := opts.Control.Wait(ctx); err != nil {
					res.err = err
					if state, ok := machine.State(stage, block.Index); ok && state == BlockPending {
						_ = machine.Transition(stage, block.Index, BlockProcessing)
					}
					_ = machine.Fail(stage, block.Index, err.Error())
					results <- res
					return nil
				}
			}
			if err := machine.Transition(stage, block.Index, BlockProcessing); err != nil {
				res.err = err
				results <- res
				return nil
			}
			emit(opts.OnEvent, EngineEvent{Type: "transition", Stage: stage, Block: block.Index, State: BlockProcessing})
			if reason, faulted := opts.Faults[key]; faulted {
				if strings.HasPrefix(reason, "panic:") {
					panic(reason)
				}
				res.err = NewError(CodeState, "fault-injection", reason)
				_ = machine.Fail(stage, block.Index, reason)
				emit(opts.OnEvent, EngineEvent{Type: "transition", Stage: stage, Block: block.Index, State: BlockFailed, Reason: reason})
				results <- res
				return nil
			}
			res.hash = fingerprintWithBuffer(crop, buffer)
			results <- res
			return nil
		})
		if err != nil {
			results <- blockResult{stage: stage, block: block.Index, err: err}
			break
		}
		submitted++
	}
	collected := make([]blockResult, n)
	received := make([]bool, n)
	for i := 0; i < submitted; i++ {
		r := <-results
		collected[r.block] = r
		received[r.block] = true
	}
	for blockIdx := 0; blockIdx < n; blockIdx++ {
		if !received[blockIdx] {
			result.FailedBlocks = append(result.FailedBlocks, CommitKey{Stage: stage, Block: blockIdx})
			if state, ok := machine.State(stage, blockIdx); ok && state == BlockPending {
				_ = machine.Transition(stage, blockIdx, BlockProcessing)
				_ = machine.Fail(stage, blockIdx, "not submitted")
			}
			continue
		}
		r := collected[blockIdx]
		if r.err != nil {
			result.FailedBlocks = append(result.FailedBlocks, CommitKey{Stage: r.stage, Block: r.block})
			if state, ok := machine.State(r.stage, r.block); ok && state == BlockProcessing {
				_ = machine.Fail(r.stage, r.block, r.err.Error())
			}
			continue
		}
		if opts.Checkpoint != nil && opts.Mode != RunFull {
			crop := cropImage(stageImage, opts.Grid.Blocks[r.block])
			if err := opts.Checkpoint.SaveBlock(ctx, imageFP, params, AlgorithmVersion, stage, opts.Grid.Blocks[r.block], crop, r.hash); err != nil {
				_ = machine.Fail(stage, r.block, err.Error())
				result.FailedBlocks = append(result.FailedBlocks, CommitKey{Stage: stage, Block: r.block})
				continue
			}
		}
		if err := machine.Transition(stage, r.block, BlockCommitted); err != nil {
			result.FailedBlocks = append(result.FailedBlocks, CommitKey{Stage: stage, Block: r.block})
			continue
		}
		result.CommitSequence = append(result.CommitSequence, CommitKey{Stage: stage, Block: r.block})
		emit(opts.OnEvent, EngineEvent{Type: "transition", Stage: stage, Block: r.block, State: BlockCommitted})
	}
	return len(result.FailedBlocks) > 0
}

func cropImage(src Image, block Block) Image {
	out := NewImage(block.Width, block.Height, src.Channels)
	for y := 0; y < block.Height; y++ {
		srcStart := ((block.Y+y)*src.Width + block.X) * src.Channels
		dstStart := y * block.Width * src.Channels
		copy(out.Pix[dstStart:dstStart+block.Width*src.Channels], src.Pix[srcStart:srcStart+block.Width*src.Channels])
	}
	return out
}

func fingerprintWithBuffer(im Image, buffer []byte) string {
	h := sha256.New()
	var header [12]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(im.Width))
	binary.LittleEndian.PutUint32(header[4:8], uint32(im.Height))
	binary.LittleEndian.PutUint32(header[8:12], uint32(im.Channels))
	_, _ = h.Write(header[:])
	if len(buffer) == 0 {
		_, _ = h.Write(im.Pix)
	} else {
		for offset := 0; offset < len(im.Pix); {
			n := min(len(buffer), len(im.Pix)-offset)
			copy(buffer[:n], im.Pix[offset:offset+n])
			_, _ = h.Write(buffer[:n])
			offset += n
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func emit(fn func(EngineEvent), ev EngineEvent) {
	if fn == nil {
		return
	}
	ev.AtUnixMS = time.Now().UnixMilli()
	fn(ev)
}

func CompareRuns(a, b RunResult) error {
	if a.ImageFingerprint != b.ImageFingerprint {
		return NewError(CodeDiverge, "imageFingerprint", "image fingerprints differ")
	}
	if a.ParamVersion != b.ParamVersion {
		return NewError(CodeDiverge, "paramVersion", "parameter versions differ")
	}
	if a.AlgorithmVersion != b.AlgorithmVersion {
		return NewError(CodeDiverge, "algorithmVersion", "algorithm versions differ")
	}
	if a.Final.Width != b.Final.Width || a.Final.Height != b.Final.Height || a.Final.Channels != b.Final.Channels {
		return NewError(CodeDiverge, "finalShape", "final mask shape differs")
	}
	firstStage := ""
	firstPix := -1
	for i := range a.Stages {
		if i >= len(b.Stages) {
			return NewError(CodeDiverge, "stages", "stage result counts differ")
		}
		if pix := firstDifferingPixel(a.Stages[i].Image(), b.Stages[i].Image()); pix >= 0 {
			firstStage = a.Stages[i].Stage
			firstPix = pix
			break
		}
	}
	if firstPix < 0 {
		firstPix = firstDifferingPixel(a.Final, b.Final)
		if firstPix >= 0 {
			firstStage = StageOrder[len(StageOrder)-1]
		}
	}
	if firstPix >= 0 {
		total := a.Final.PixelCount()
		pixel := firstPix / a.Final.Channels
		details := map[string]any{
			"stage":   firstStage,
			"x":       pixel % a.Final.Width,
			"y":       pixel / a.Final.Width,
			"channel": firstPix % a.Final.Channels,
			"index":   firstPix,
			"total":   total,
		}
		return &AppError{Code: CodeDiverge, Field: "pixels", Message: "run results differ pixel-by-pixel", Details: details}
	}
	if GeometryDeviation(a.Stats, b.Stats) != 0 {
		return &AppError{Code: CodeDiverge, Field: "geometry", Message: "geometry statistics differ", Details: map[string]any{"deviation": GeometryDeviation(a.Stats, b.Stats)}}
	}
	if len(a.CommitSequence) != len(b.CommitSequence) {
		return NewError(CodeDiverge, "commitSequence", "commit sequence lengths differ")
	}
	for i := range a.CommitSequence {
		if a.CommitSequence[i] != b.CommitSequence[i] {
			return &AppError{Code: CodeDiverge, Field: "commitSequence", Message: "commit sequences differ", Details: map[string]any{"index": i, "left": a.CommitSequence[i], "right": b.CommitSequence[i]}}
		}
	}
	return nil
}

func firstDifferingPixel(a, b Image) int {
	if a.Width != b.Width || a.Height != b.Height || a.Channels != b.Channels {
		return 0
	}
	for i := range a.Pix {
		if a.Pix[i] != b.Pix[i] {
			return i
		}
	}
	return -1
}

func SortedCommitKeys(keys []CommitKey) []CommitKey {
	out := append([]CommitKey(nil), keys...)
	sort.Slice(out, func(i, j int) bool {
		si, sj := stageIndex(out[i].Stage), stageIndex(out[j].Stage)
		if si != sj {
			return si < sj
		}
		return out[i].Block < out[j].Block
	})
	return out
}
