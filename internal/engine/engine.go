package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"audioeval/internal/codec"
	"audioeval/internal/metrics"
	"audioeval/internal/model"
	"audioeval/internal/report"
	"audioeval/internal/sample"
)

type Mode string

const (
	ModeRealtime Mode = "realtime"
	ModeResume   Mode = "resume"
	ModeFull     Mode = "full"
)

type RunOptions struct {
	Mode   Mode
	RunDir string
	Params model.EvalParams
}

type RunResult struct {
	Report   model.QualityReport
	Partial  bool
	LoadInfo CheckpointLoad
}

type frameEncodingInfo struct {
	CoeffErrors         [][]int64
	CoeffAbsQ14         int64
	CoeffSqQ28          int64
	InverseSSE          int64
	InverseSSEByChannel []int64
}

type blockGeometry struct {
	Frame             int
	Block             int
	CoreStart         int
	CoreEnd           int
	BandLeft          int
	BandRight         int
	RequiredBandRight int
	WindowStarts      []int
}

func Evaluate(ctx context.Context, loaded sample.Loaded, opts RunOptions) (RunResult, error) {
	if opts.Mode == "" {
		opts.Mode = ModeRealtime
	}
	if opts.RunDir == "" {
		return RunResult{}, model.NewError(model.CodePara, "run_dir", -1, "run directory is required")
	}
	if err := validateParams(opts.Params, loaded.Manifest); err != nil {
		return RunResult{}, err
	}
	if err := os.MkdirAll(opts.RunDir, 0o755); err != nil {
		return RunResult{}, fmt.Errorf("create run directory: %w", err)
	}
	_ = os.Remove(filepath.Join(opts.RunDir, "report.json"))
	_ = os.Remove(filepath.Join(opts.RunDir, "partial-report.json"))
	_ = os.Remove(filepath.Join(opts.RunDir, "heatmap.png"))

	frameEncodings, err := buildFrameEncodings(loaded)
	if err != nil {
		return RunResult{}, err
	}
	paramVersion := opts.Params.ParamVersion()
	meta := model.SnapshotMeta{
		SchemaVersion:    model.SchemaVersion,
		SampleID:         loaded.Manifest.SampleID,
		Quality:          loaded.Manifest.Quality,
		InputHash:        loaded.Manifest.InputHash,
		ParamVersion:     paramVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		TruthFingerprint: loaded.Truth.TruthFingerprint,
		FrameCount:       loaded.Manifest.FrameCount,
		FrameLength:      loaded.Manifest.FrameLength,
		FrameHop:         loaded.Manifest.FrameHop,
		Channels:         loaded.Manifest.Channels,
		BlocksPerFrame:   opts.Params.BlocksPerFrame,
		Window:           opts.Params.Window,
		WindowHop:        opts.Params.WindowHop,
	}
	store, loadInfo, err := OpenCheckpoint(opts.RunDir, meta, opts.Mode == ModeResume)
	if err != nil {
		return RunResult{}, err
	}
	state, err := NewStateMachine(loaded.Manifest.FrameCount, opts.Params.BlocksPerFrame)
	if err != nil {
		return RunResult{}, err
	}
	allBlocks := make(map[int]model.BlockResult, loaded.Manifest.FrameCount*opts.Params.BlocksPerFrame)
	for _, block := range loadInfo.Blocks {
		coord := model.BlockCoord{Frame: block.Frame, Block: block.Block}
		if err := state.MarkProcessing(coord); err != nil {
			return RunResult{}, err
		}
		if err := state.CommitResult(coord, block); err != nil {
			return RunResult{}, err
		}
		allBlocks[block.Sequence] = block
	}
	totalBlocks := loaded.Manifest.FrameCount * opts.Params.BlocksPerFrame
	resumeInfo := model.ResumeInfo{
		RequestedMode:   string(opts.Mode),
		Resumed:         loadInfo.Resumed,
		Invalidated:     loadInfo.Invalidated,
		InvalidReason:   loadInfo.Reason,
		LoadedBlocks:    len(loadInfo.Blocks),
		CommittedBlocks: len(loadInfo.Blocks),
		TotalBlocks:     totalBlocks,
	}

	geometry := make(map[int]blockGeometry, totalBlocks)
	for frame := 0; frame < loaded.Manifest.FrameCount; frame++ {
		for block := 0; block < opts.Params.BlocksPerFrame; block++ {
			geom, err := makeBlockGeometry(frame, block, loaded.Manifest, opts.Params)
			if err != nil {
				return RunResult{}, err
			}
			sequence := frame*opts.Params.BlocksPerFrame + block
			geometry[sequence] = geom
		}
	}

	tasks := make([]Task, 0, totalBlocks-len(loadInfo.Blocks))
	for sequence := len(loadInfo.Blocks); sequence < totalBlocks; sequence++ {
		geom := geometry[sequence]
		requiredSamples := (geom.CoreEnd - geom.CoreStart + geom.BandLeft + geom.BandRight) * loaded.Manifest.Channels
		if requiredSamples > opts.Params.PerThreadBufferSamples {
			err := model.NewError(model.CodePara, "per_thread_buffer_samples", geom.Frame,
				"block requires more samples than the per-thread buffer budget").WithBlock(model.BlockCoord{Frame: geom.Frame, Block: geom.Block})
			return RunResult{}, err
		}
		geomCopy := geom
		coord := model.BlockCoord{Frame: geom.Frame, Block: geom.Block}
		tasks = append(tasks, Task{
			Sequence:        sequence,
			Coord:           coord,
			RequiredSamples: requiredSamples,
			Run: func(runCtx context.Context, buffer *WorkerBuffer) (model.BlockResult, error) {
				if err := state.MarkProcessing(coord); err != nil {
					return model.BlockResult{}, err
				}
				result, err := evaluateBlock(runCtx, loaded, frameEncodings[geomCopy.Frame], geometry[sequence], opts.Params, buffer)
				if err != nil {
					_ = state.MarkFailed(coord)
					return model.BlockResult{}, err
				}
				return result, nil
			},
		})
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	newCommitCount := 0
	scheduler := Scheduler{
		MaxWorkers:             opts.Params.MaxWorkers,
		PerThreadBufferSamples: opts.Params.PerThreadBufferSamples,
		QueueBudget:            opts.Params.QueueBudget,
	}
	summary, runErr := scheduler.Run(runCtx, tasks, func(result model.BlockResult) error {
		if err := store.Commit(result); err != nil {
			return err
		}
		coord := model.BlockCoord{Frame: result.Frame, Block: result.Block}
		if err := state.CommitResult(coord, result); err != nil {
			return err
		}
		allBlocks[result.Sequence] = result
		newCommitCount++
		if opts.Params.CancelAfterCommits > 0 && newCommitCount >= opts.Params.CancelAfterCommits {
			cancel()
		}
		return nil
	})
	resumeInfo.CommittedBlocks = len(allBlocks)
	if runErr != nil {
		cancellation := errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)
		completeDespiteCancellation := false
		if cancellation && len(allBlocks) == totalBlocks {
			committed, total := state.Progress()
			completeDespiteCancellation = committed == total
		}
		if !completeDespiteCancellation {
			if cancellation {
				partial := buildPartialReport(loaded, opts, resumeInfo, allBlocks, summary)
				if err := report.WriteJSON(filepath.Join(opts.RunDir, "partial-report.json"), partial); err != nil {
					return RunResult{Report: partial, Partial: true, LoadInfo: loadInfo}, err
				}
				return RunResult{Report: partial, Partial: true, LoadInfo: loadInfo}, runErr
			}
			return RunResult{Report: model.QualityReport{}, LoadInfo: loadInfo}, runErr
		}
	}
	if len(allBlocks) != totalBlocks {
		return RunResult{}, model.NewError(model.CodeState, "blocks", -1, "scheduler returned without all block commits")
	}
	committed, total := state.Progress()
	if committed != total {
		return RunResult{}, model.NewError(model.CodeState, "state", -1, "not all block stages are committed")
	}
	reportValue, err := buildReport(loaded, opts, resumeInfo, allBlocks, summary, frameEncodings, paramVersion)
	if err != nil {
		return RunResult{}, err
	}
	if err := report.WriteJSON(filepath.Join(opts.RunDir, "report.json"), reportValue); err != nil {
		return RunResult{}, err
	}
	if err := report.WriteHeatmap(filepath.Join(opts.RunDir, "heatmap.png"), reportValue); err != nil {
		return RunResult{}, err
	}
	return RunResult{Report: reportValue, LoadInfo: loadInfo}, nil
}

func validateParams(params model.EvalParams, manifest model.Manifest) error {
	if params.Window <= 0 || params.Window > manifest.FrameLength {
		return model.NewError(model.CodePara, "window", -1, "evaluation window must be positive and not greater than frame length")
	}
	if params.WindowHop <= 0 || params.WindowHop > params.Window {
		return model.NewError(model.CodePara, "window_hop", -1, "window hop must be positive and not greater than window size")
	}
	if params.BlocksPerFrame <= 0 || params.BlocksPerFrame > manifest.FrameLength {
		return model.NewError(model.CodePara, "blocks_per_frame", -1, "blocks per frame must be positive and not greater than frame length")
	}
	if params.BandWidth < 0 {
		return model.NewError(model.CodePara, "band_width", -1, "band width must not be negative")
	}
	if params.MaxWorkers <= 0 {
		return model.NewError(model.CodePara, "max_workers", -1, "maximum worker count must be positive")
	}
	if params.PerThreadBufferSamples <= 0 {
		return model.NewError(model.CodePara, "per_thread_buffer_samples", -1, "per-thread sample buffer must be positive")
	}
	if params.QueueBudget < 0 {
		return model.NewError(model.CodePara, "queue_budget", -1, "queue budget must not be negative")
	}
	return nil
}

func makeBlockGeometry(frame, block int, manifest model.Manifest, params model.EvalParams) (blockGeometry, error) {
	coreStart := block * manifest.FrameLength / params.BlocksPerFrame
	coreEnd := (block + 1) * manifest.FrameLength / params.BlocksPerFrame
	bandWidth := params.BandWidth
	if bandWidth == 0 {
		bandWidth = params.Window + params.WindowHop - 1
	}
	bandLeft := min(bandWidth, coreStart)
	bandRight := min(bandWidth, manifest.FrameLength-coreEnd)
	windowsForBlock := make([]int, 0)
	requiredRight := 0
	for _, start := range metrics.WindowStarts(manifest.FrameLength, params.Window, params.WindowHop) {
		if start >= coreStart && start < coreEnd {
			windowsForBlock = append(windowsForBlock, start)
			need := start + params.Window - coreEnd
			if need > requiredRight {
				requiredRight = need
			}
		}
	}
	if bandRight < requiredRight {
		coord := model.BlockCoord{Frame: frame, Block: block}
		return blockGeometry{}, model.NewError(model.CodeBand, "band_width", frame,
			"block right band is smaller than the deterministic minimum").WithBlock(coord).WithRequiredBand(requiredRight)
	}
	return blockGeometry{
		Frame:             frame,
		Block:             block,
		CoreStart:         coreStart,
		CoreEnd:           coreEnd,
		BandLeft:          bandLeft,
		BandRight:         bandRight,
		RequiredBandRight: requiredRight,
		WindowStarts:      windowsForBlock,
	}, nil
}

func evaluateBlock(ctx context.Context, loaded sample.Loaded, encoding frameEncodingInfo, geom blockGeometry, params model.EvalParams, buffer *WorkerBuffer) (model.BlockResult, error) {
	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, err
	}
	channels := loaded.Manifest.Channels
	frameLength := loaded.Manifest.FrameLength
	frameStart := geom.Frame * loaded.Manifest.FrameHop
	globalStart := frameStart + geom.CoreStart - geom.BandLeft
	globalEnd := frameStart + geom.CoreEnd + geom.BandRight
	samples := (globalEnd - globalStart) * channels
	xScratch, yScratch := buffer.Allocate(samples)
	copy(xScratch, loaded.Original[globalStart*channels:globalEnd*channels])
	copy(yScratch, loaded.Reconstruction[globalStart*channels:globalEnd*channels])

	result := model.BlockResult{
		SchemaVersion:         model.SchemaVersion,
		Sequence:              geom.Frame*params.BlocksPerFrame + geom.Block,
		Frame:                 geom.Frame,
		Block:                 geom.Block,
		CoreStart:             geom.CoreStart,
		CoreEnd:               geom.CoreEnd,
		BandLeft:              geom.BandLeft,
		BandRight:             geom.BandRight,
		RequiredBandRight:     geom.RequiredBandRight,
		QuantCoeffAbsQ14:      encoding.CoeffAbsQ14,
		QuantCoeffSqQ28:       encoding.CoeffSqQ28,
		QuantStep:             loaded.Manifest.QuantStep,
		TruthExpectedSNRUDb:   loaded.Truth.Frames[geom.Frame].ExpectedSNRUDb,
		TruthNoiseFloorStdQ16: loaded.Truth.Frames[geom.Frame].NoiseFloorStdQ16,
		InputHash:             loaded.Manifest.InputHash,
		ParamVersion:          params.ParamVersion(),
		AlgorithmVersion:      model.AlgorithmVersion,
	}
	coreLocalStart := geom.BandLeft
	coreLen := geom.CoreEnd - geom.CoreStart
	var qualityPSNR, qualitySSIM int64
	for ch := 0; ch < channels; ch++ {
		relativeStart := (coreLocalStart - geom.BandLeft) * channels
		coreX := xScratch[relativeStart+ch:]
		coreY := yScratch[relativeStart+ch:]
		coreStat := metrics.CoreStatsStrided(ch, coreX, coreY, channels, coreLen)
		result.CoreStats = append(result.CoreStats, coreStat)
		channelQuality := metrics.BlockQuality(coreStat)
		qualityPSNR += channelQuality.PSNRUDb
		qualitySSIM += channelQuality.SSIMQ16

		for _, windowStart := range geom.WindowStarts {
			windowLocalStart := windowStart - (geom.CoreStart - geom.BandLeft)
			windowX := xScratch[windowLocalStart*channels+ch:]
			windowY := yScratch[windowLocalStart*channels+ch:]
			windowStat := metrics.ComputeWindowStrided(ch, windowStart, windowX, windowY, channels, params.Window)
			result.Windows = append(result.Windows, windowStat)
		}

		var originalSSE, reconstructionSSE, residualCount int64
		if geom.CoreStart > 0 {
			for i := 0; i < coreLen; i++ {
				currentLocal := geom.BandLeft + i
				previousLocal := currentLocal - 1
				xDiff := int64(xScratch[currentLocal*channels+ch]) - int64(xScratch[previousLocal*channels+ch])
				yDiff := int64(yScratch[currentLocal*channels+ch]) - int64(yScratch[previousLocal*channels+ch])
				originalSSE += xDiff * xDiff
				reconstructionSSE += yDiff * yDiff
				residualCount++
			}
		} else {
			for i := 1; i < coreLen; i++ {
				xDiff := int64(xScratch[i*channels+ch]) - int64(xScratch[(i-1)*channels+ch])
				yDiff := int64(yScratch[i*channels+ch]) - int64(yScratch[(i-1)*channels+ch])
				originalSSE += xDiff * xDiff
				reconstructionSSE += yDiff * yDiff
				residualCount++
			}
		}
		result.Residuals = append(result.Residuals, model.ChannelResidual{
			Channel:           ch,
			Count:             residualCount,
			OriginalSSE:       originalSSE,
			ReconstructionSSE: reconstructionSSE,
		})
	}
	if channels > 0 {
		result.Quality = model.BlockQuality{
			PSNRUDb: qualityPSNR / int64(channels),
			SSIMQ16: qualitySSIM / int64(channels),
		}
	}
	for i := 0; i < coreLen; i++ {
		for ch := 0; ch < channels; ch++ {
			diff := int64(xScratch[(coreLocalStart+i)*channels+ch]) - int64(yScratch[(coreLocalStart+i)*channels+ch])
			result.SampleErrorSSE += diff * diff
		}
	}
	result.OriginalHash = hashInterleaved(xScratch[coreLocalStart*channels : (coreLocalStart+coreLen)*channels])
	result.ReconstructionHash = hashInterleaved(yScratch[coreLocalStart*channels : (coreLocalStart+coreLen)*channels])
	result.ErrorUpperBound = metrics.ErrorUpperBound(
		loaded.Manifest.QuantStep,
		frameLength,
		loaded.Manifest.FrameHop,
		params.Window,
		loaded.Truth.Frames[geom.Frame].NoiseFloorStdQ16,
	)
	result.BoundaryJump = boundaryJump(loaded, geom)
	if err := ctx.Err(); err != nil {
		return model.BlockResult{}, err
	}
	return result, nil
}

func boundaryJump(loaded sample.Loaded, geom blockGeometry) int64 {
	frameStart := geom.Frame * loaded.Manifest.FrameHop
	previousGlobal := frameStart + geom.CoreEnd - 1
	var nextGlobal int
	if geom.CoreEnd < loaded.Manifest.FrameLength {
		nextGlobal = frameStart + geom.CoreEnd
	} else if geom.Frame+1 < loaded.Manifest.FrameCount {
		nextGlobal = (geom.Frame + 1) * loaded.Manifest.FrameHop
	} else {
		return 0
	}
	previous := errorAt(loaded, previousGlobal)
	next := errorAt(loaded, nextGlobal)
	diff := previous - next
	if diff < 0 {
		diff = -diff
	}
	return diff
}

func errorAt(loaded sample.Loaded, globalSample int) int64 {
	index := globalSample * loaded.Manifest.Channels
	return int64(loaded.Original[index]) - int64(loaded.Reconstruction[index])
}

func buildFrameEncodings(loaded sample.Loaded) ([]frameEncodingInfo, error) {
	result := make([]frameEncodingInfo, loaded.Manifest.FrameCount)
	for frame := 0; frame < loaded.Manifest.FrameCount; frame++ {
		info := frameEncodingInfo{
			CoeffErrors:         make([][]int64, loaded.Manifest.Channels),
			InverseSSEByChannel: make([]int64, loaded.Manifest.Channels),
		}
		frameStart := frame * loaded.Manifest.FrameHop
		for ch := 0; ch < loaded.Manifest.Channels; ch++ {
			segment := make([]int16, loaded.Manifest.FrameLength)
			for local := 0; local < loaded.Manifest.FrameLength; local++ {
				segment[local] = loaded.Original[(frameStart+local)*loaded.Manifest.Channels+ch]
			}
			encoding, err := codec.EncodeFrame(segment, loaded.Manifest.QuantStep)
			if err != nil {
				return nil, err
			}
			info.CoeffErrors[ch] = encoding.CoeffErrorsQ14
			absSum, squareSum := codec.CoefficientErrorSummary(encoding.CoeffErrorsQ14)
			info.CoeffAbsQ14 += absSum
			info.CoeffSqQ28 += squareSum
			info.InverseSSE += encoding.InverseSSE
			info.InverseSSEByChannel[ch] = encoding.InverseSSE
		}
		result[frame] = info
	}
	return result, nil
}

func buildReport(loaded sample.Loaded, opts RunOptions, resumeInfo model.ResumeInfo, allBlocks map[int]model.BlockResult, summary SchedulerSummary, encodings []frameEncodingInfo, paramVersion string) (model.QualityReport, error) {
	frameCount := loaded.Manifest.FrameCount
	blocksPerFrame := opts.Params.BlocksPerFrame
	totalBlocks := frameCount * blocksPerFrame
	ordered := make([]model.BlockResult, totalBlocks)
	for sequence, block := range allBlocks {
		if sequence < 0 || sequence >= totalBlocks {
			return model.QualityReport{}, model.NewError(model.CodeState, "sequence", block.Frame, "checkpoint block sequence is outside the report")
		}
		ordered[sequence] = block
	}
	reportValue := model.QualityReport{
		SchemaVersion:    model.SchemaVersion,
		SampleID:         loaded.Manifest.SampleID,
		Quality:          loaded.Manifest.Quality,
		Mode:             string(opts.Mode),
		Complete:         true,
		InputHash:        loaded.Manifest.InputHash,
		ParamVersion:     paramVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		TruthFingerprint: loaded.Truth.TruthFingerprint,
		Params:           opts.Params,
		Alignment: model.AlignmentReport{
			Offset:                0,
			OriginalSamples:       len(loaded.Original) / loaded.Manifest.Channels,
			ReconstructionSamples: len(loaded.Reconstruction) / loaded.Manifest.Channels,
			InterleavedSamples:    len(loaded.Original),
			Aligned:               len(loaded.Original) == len(loaded.Reconstruction),
		},
		Precision:      model.DefaultPrecisionPolicy(),
		Resume:         resumeInfo,
		CommitSequence: make([]int, 0, totalBlocks),
		QueueEvents:    summary.QueueEvents,
		Frames:         make([]model.FrameReport, frameCount),
	}
	for sequence, block := range ordered {
		if block.Sequence != sequence {
			return model.QualityReport{}, model.NewError(model.CodeState, "blocks", -1, "block sequence is incomplete or out of order")
		}
		reportValue.CommitSequence = append(reportValue.CommitSequence, sequence)
	}
	for frame := 0; frame < frameCount; frame++ {
		frameReport := model.FrameReport{
			Frame:                 frame,
			TruthExpectedSNRUDb:   loaded.Truth.Frames[frame].ExpectedSNRUDb,
			TruthNoiseFloorStdQ16: loaded.Truth.Frames[frame].NoiseFloorStdQ16,
			Blocks:                make([]model.BlockSummary, 0, blocksPerFrame),
		}
		windowsByChannel := make([][]model.WindowStat, loaded.Manifest.Channels)
		residualOriginal := make([]int64, loaded.Manifest.Channels)
		residualReconstruction := make([]int64, loaded.Manifest.Channels)
		residualCount := make([]int64, loaded.Manifest.Channels)
		var frameSampleErrorSSE int64
		for blockIndex := 0; blockIndex < blocksPerFrame; blockIndex++ {
			block := ordered[frame*blocksPerFrame+blockIndex]
			frameSampleErrorSSE += block.SampleErrorSSE
			frameReport.Blocks = append(frameReport.Blocks, model.BlockSummary{
				Frame:              block.Frame,
				Block:              block.Block,
				CoreStart:          block.CoreStart,
				CoreEnd:            block.CoreEnd,
				BandLeft:           block.BandLeft,
				BandRight:          block.BandRight,
				WindowCount:        len(block.Windows),
				Quality:            block.Quality,
				SampleErrorSSE:     block.SampleErrorSSE,
				ErrorUpperBound:    block.ErrorUpperBound,
				BoundaryJump:       block.BoundaryJump,
				ReconstructionHash: block.ReconstructionHash,
				OriginalHash:       block.OriginalHash,
			})
			for _, window := range block.Windows {
				windowsByChannel[window.Channel] = append(windowsByChannel[window.Channel], window)
			}
			for _, residual := range block.Residuals {
				residualOriginal[residual.Channel] += residual.OriginalSSE
				residualReconstruction[residual.Channel] += residual.ReconstructionSSE
				residualCount[residual.Channel] += residual.Count
			}
		}
		var framePSNRSum, frameSSIMSum int64
		var frameWindowCount int64
		var totalOriginalResidual, totalReconstructionResidual, totalResidualCount int64
		for ch := 0; ch < loaded.Manifest.Channels; ch++ {
			windows := windowsByChannel[ch]
			sort.Slice(windows, func(i, j int) bool { return windows[i].Start < windows[j].Start })
			channelReport := model.ChannelFrameReport{
				Channel:             ch,
				WindowCount:         len(windows),
				QuantCoeffErrorsQ14: encodings[frame].CoeffErrors[ch],
				InverseErrorSSE:     encodings[frame].InverseSSEByChannel[ch],
			}
			for _, window := range windows {
				channelReport.PSNRUDb += metrics.PSNRMicroDB(window.SSE, window.Length)
				channelReport.SSIMQ16 += metrics.SSIMQ16(window)
				framePSNRSum += metrics.PSNRMicroDB(window.SSE, window.Length)
				frameSSIMSum += metrics.SSIMQ16(window)
				frameWindowCount++
			}
			if len(windows) > 0 {
				channelReport.PSNRUDb /= int64(len(windows))
				channelReport.SSIMQ16 /= int64(len(windows))
			}
			channelReport.OriginalNoiseStdQ16, channelReport.ReconstructionNoiseStdQ16 =
				metrics.MergeNoise(residualOriginal[ch], residualReconstruction[ch], residualCount[ch])
			frameReport.Channels = append(frameReport.Channels, channelReport)
			frameReport.InverseErrorSSE += channelReport.InverseErrorSSE
			totalOriginalResidual += residualOriginal[ch]
			totalReconstructionResidual += residualReconstruction[ch]
			totalResidualCount += residualCount[ch]
		}
		if frameWindowCount > 0 {
			frameReport.PSNRUDb = framePSNRSum / frameWindowCount
			frameReport.SSIMQ16 = frameSSIMSum / frameWindowCount
		}
		frameReport.OriginalNoiseStdQ16, frameReport.ReconstructionNoiseStdQ16 =
			metrics.MergeNoise(totalOriginalResidual, totalReconstructionResidual, totalResidualCount)
		frameReport.QuantizationResidualStdQ16 = metrics.NoiseStdQ16(
			frameSampleErrorSSE,
			int64(loaded.Manifest.FrameLength*loaded.Manifest.Channels),
		)
		frameReport.NoiseFloorDeltaQ16 = frameReport.QuantizationResidualStdQ16 - frameReport.TruthNoiseFloorStdQ16
		frameReport.ErrorUpperBound = metrics.ErrorUpperBound(
			loaded.Manifest.QuantStep,
			loaded.Manifest.FrameLength,
			loaded.Manifest.FrameHop,
			opts.Params.Window,
			loaded.Truth.Frames[frame].NoiseFloorStdQ16,
		)
		if len(frameReport.Blocks) > 0 {
			frameReport.BoundaryErrorJump = frameReport.Blocks[len(frameReport.Blocks)-1].BoundaryJump
		}
		reportValue.Frames[frame] = frameReport
	}
	for i := 0; i+1 < len(ordered); i++ {
		from := ordered[i]
		to := ordered[i+1]
		edge := model.ErrorPathEdge{
			FromFrame:     from.Frame,
			FromBlock:     from.Block,
			ToFrame:       to.Frame,
			ToBlock:       to.Block,
			FrameBoundary: from.Frame != to.Frame,
			ErrorJump:     from.BoundaryJump,
		}
		reportValue.ErrorPath = append(reportValue.ErrorPath, edge)
	}
	return reportValue, nil
}

func buildPartialReport(loaded sample.Loaded, opts RunOptions, resumeInfo model.ResumeInfo, allBlocks map[int]model.BlockResult, summary SchedulerSummary) model.QualityReport {
	sequences := make([]int, 0, len(allBlocks))
	for sequence := range allBlocks {
		sequences = append(sequences, sequence)
	}
	sort.Ints(sequences)
	return model.QualityReport{
		SchemaVersion:    model.SchemaVersion,
		SampleID:         loaded.Manifest.SampleID,
		Quality:          loaded.Manifest.Quality,
		Mode:             string(opts.Mode),
		Complete:         false,
		InputHash:        loaded.Manifest.InputHash,
		ParamVersion:     opts.Params.ParamVersion(),
		AlgorithmVersion: model.AlgorithmVersion,
		TruthFingerprint: loaded.Truth.TruthFingerprint,
		Params:           opts.Params,
		Alignment: model.AlignmentReport{
			Offset:                0,
			OriginalSamples:       len(loaded.Original) / loaded.Manifest.Channels,
			ReconstructionSamples: len(loaded.Reconstruction) / loaded.Manifest.Channels,
			InterleavedSamples:    len(loaded.Original),
			Aligned:               len(loaded.Original) == len(loaded.Reconstruction),
		},
		Precision:      model.DefaultPrecisionPolicy(),
		Resume:         resumeInfo,
		CommitSequence: sequences,
		QueueEvents:    summary.QueueEvents,
		Frames:         []model.FrameReport{},
		ErrorPath:      []model.ErrorPathEdge{},
	}
}

func hashInterleaved(samples []int16) string {
	data := make([]byte, len(samples)*2)
	for i, value := range samples {
		binary.LittleEndian.PutUint16(data[i*2:], uint16(value))
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
