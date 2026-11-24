package verify

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"audioeval/internal/model"
)

type Violation struct {
	Frame   int    `json:"frame"`
	Block   int    `json:"block"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

type Result struct {
	OK             bool       `json:"ok"`
	CheckedBlocks  int        `json:"checked_blocks"`
	CheckedFrames  int        `json:"checked_frames"`
	FirstViolation *Violation `json:"first_violation,omitempty"`
}

type Options struct {
	SampleDir string
	RunDir    string
}

func Verify(opts Options) (Result, error) {
	result := Result{}
	var manifest model.Manifest
	if err := readJSON(filepath.Join(opts.SampleDir, "manifest.json"), &manifest); err != nil {
		return result, fmt.Errorf("read manifest metadata: %w", err)
	}
	var meta model.SnapshotMeta
	if err := readJSON(filepath.Join(opts.RunDir, "meta.json"), &meta); err != nil {
		return result, fmt.Errorf("read checkpoint metadata: %w", err)
	}
	var reportValue model.QualityReport
	if err := readJSON(filepath.Join(opts.RunDir, "report.json"), &reportValue); err != nil {
		return result, fmt.Errorf("read quality report: %w", err)
	}
	if violation := verifyMeta(manifest, meta, reportValue); violation != nil {
		result.FirstViolation = violation
		return result, nil
	}
	blocks, violation, err := loadBlocks(opts.RunDir, meta)
	if err != nil {
		return result, err
	}
	if violation != nil {
		result.FirstViolation = violation
		return result, nil
	}
	result.CheckedBlocks = len(blocks)
	if len(blocks) != meta.FrameCount*meta.BlocksPerFrame {
		result.FirstViolation = &Violation{Frame: -1, Block: -1, Field: "blocks", Message: "checkpoint block count is incomplete"}
		return result, nil
	}
	if violation := verifyCommitSequence(reportValue, len(blocks)); violation != nil {
		result.FirstViolation = violation
		return result, nil
	}
	for _, block := range blocks {
		if violation := verifyBlock(block, meta); violation != nil {
			result.FirstViolation = violation
			return result, nil
		}
	}
	for frame := 0; frame < meta.FrameCount; frame++ {
		result.CheckedFrames++
		frameBlocks := blocks[frame*meta.BlocksPerFrame : (frame+1)*meta.BlocksPerFrame]
		if violation := verifyFrame(reportValue, frame, frameBlocks, meta); violation != nil {
			result.FirstViolation = violation
			return result, nil
		}
	}
	if violation := verifyErrorPath(reportValue, blocks); violation != nil {
		result.FirstViolation = violation
		return result, nil
	}
	result.OK = true
	return result, nil
}

func verifyMeta(manifest model.Manifest, meta model.SnapshotMeta, report model.QualityReport) *Violation {
	switch {
	case meta.SchemaVersion != model.SchemaVersion:
		return violation(-1, -1, "schema_version", "snapshot schema version mismatch")
	case meta.SampleID != manifest.SampleID:
		return violation(-1, -1, "sample_id", "snapshot sample id mismatch")
	case meta.Quality != manifest.Quality:
		return violation(-1, -1, "quality", "snapshot quality mismatch")
	case meta.InputHash != manifest.InputHash:
		return violation(-1, -1, "input_hash", "snapshot input hash mismatch")
	case meta.TruthFingerprint != manifest.TruthFingerprint:
		return violation(-1, -1, "truth_fingerprint", "snapshot truth fingerprint mismatch")
	case meta.FrameLength != manifest.FrameLength || meta.FrameHop != manifest.FrameHop || meta.Channels != manifest.Channels:
		return violation(-1, -1, "frame_geometry", "snapshot frame geometry mismatch")
	case meta.AlgorithmVersion != model.AlgorithmVersion:
		return violation(-1, -1, "algorithm_version", "snapshot algorithm version mismatch")
	case !report.Complete:
		return violation(-1, -1, "complete", "quality report is not complete")
	case report.InputHash != meta.InputHash:
		return violation(-1, -1, "report.input_hash", "report input hash mismatch")
	case report.ParamVersion != meta.ParamVersion:
		return violation(-1, -1, "report.param_version", "report parameter version mismatch")
	case report.AlgorithmVersion != meta.AlgorithmVersion:
		return violation(-1, -1, "report.algorithm_version", "report algorithm version mismatch")
	case report.TruthFingerprint != meta.TruthFingerprint:
		return violation(-1, -1, "report.truth_fingerprint", "report truth fingerprint mismatch")
	case report.Params.Window != meta.Window || report.Params.WindowHop != meta.WindowHop || report.Params.BlocksPerFrame != meta.BlocksPerFrame:
		return violation(-1, -1, "report.params", "report evaluation geometry mismatch")
	}
	return nil
}

func loadBlocks(runDir string, meta model.SnapshotMeta) ([]model.BlockResult, *Violation, error) {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil, nil, fmt.Errorf("scan checkpoint: %w", err)
	}
	bySequence := map[int]model.BlockResult{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		sequence, ok := model.ParseBlockFileName(entry.Name())
		if !ok {
			continue
		}
		var block model.BlockResult
		if err := readJSON(filepath.Join(runDir, entry.Name()), &block); err != nil {
			return nil, violation(-1, -1, "checkpoint_block", fmt.Sprintf("cannot decode block file %s", entry.Name())), nil
		}
		if block.Sequence != sequence {
			return nil, violation(block.Frame, block.Block, "sequence", "block file sequence does not match its content"), nil
		}
		if block.InputHash != meta.InputHash || block.ParamVersion != meta.ParamVersion || block.AlgorithmVersion != meta.AlgorithmVersion {
			return nil, violation(block.Frame, block.Block, "version", "block version does not match snapshot metadata"), nil
		}
		bySequence[sequence] = block
	}
	total := meta.FrameCount * meta.BlocksPerFrame
	blocks := make([]model.BlockResult, 0, len(bySequence))
	for sequence := 0; sequence < total; sequence++ {
		block, ok := bySequence[sequence]
		if !ok {
			return nil, violation(-1, -1, "blocks", "checkpoint block sequence contains a gap"), nil
		}
		if sequence != block.Frame*meta.BlocksPerFrame+block.Block {
			return nil, violation(block.Frame, block.Block, "sequence", "block coordinate does not match sequence"), nil
		}
		blocks = append(blocks, block)
	}
	return blocks, nil, nil
}

func verifyCommitSequence(report model.QualityReport, total int) *Violation {
	if len(report.CommitSequence) != total {
		return violation(-1, -1, "commit_sequence", "commit sequence length does not match block count")
	}
	for i, sequence := range report.CommitSequence {
		if sequence != i {
			return violation(-1, -1, "commit_sequence", fmt.Sprintf("commit sequence is not canonical at position %d", i))
		}
	}
	if len(report.Frames) < 1 {
		return violation(-1, -1, "frames", "report contains no frames")
	}
	return nil
}

func verifyBlock(block model.BlockResult, meta model.SnapshotMeta) *Violation {
	if block.CoreStart < 0 || block.CoreEnd <= block.CoreStart || block.CoreEnd > meta.FrameLength {
		return violation(block.Frame, block.Block, "core", "block core range is invalid")
	}
	if block.BandLeft < 0 || block.BandRight < 0 || block.RequiredBandRight < 0 || block.BandRight < block.RequiredBandRight {
		return violation(block.Frame, block.Block, "band_right", "block band is smaller than required")
	}
	for _, window := range block.Windows {
		if window.Channel < 0 || window.Length <= 0 || window.Start < block.CoreStart || window.Start >= block.CoreEnd {
			return violation(block.Frame, block.Block, "windows", "window is outside its block core")
		}
		if window.Start+window.Length > block.CoreEnd+block.BandRight {
			return violation(block.Frame, block.Block, "band_right", "window extends beyond the block band")
		}
	}
	var psnrSum, ssimSum int64
	for _, core := range block.CoreStats {
		stat := model.WindowStat{
			Channel: core.Channel,
			Length:  int(core.Count),
			SumX:    core.SumX,
			SumY:    core.SumY,
			SumXX:   core.SumXX,
			SumYY:   core.SumYY,
			SumXY:   core.SumXY,
			SSE:     core.SSE,
		}
		psnrSum += psnrMicroDB(core.SSE, int(core.Count))
		ssimSum += ssimQ16(stat)
	}
	if len(block.CoreStats) > 0 {
		wantPSNR := psnrSum / int64(len(block.CoreStats))
		wantSSIM := ssimSum / int64(len(block.CoreStats))
		if block.Quality.PSNRUDb != wantPSNR {
			return violation(block.Frame, block.Block, "quality.psnr_udb", "block PSNR does not match its integer core moments")
		}
		if block.Quality.SSIMQ16 != wantSSIM {
			return violation(block.Frame, block.Block, "quality.ssim_q16", "block SSIM does not match its integer core moments")
		}
	}
	if block.Frame < 0 || block.Frame >= meta.FrameCount {
		return violation(block.Frame, block.Block, "frame", "block frame is outside snapshot metadata")
	}
	if block.TruthNoiseFloorStdQ16 < 0 {
		return violation(block.Frame, block.Block, "truth_noise_floor_std_q16", "persisted truth noise floor is invalid")
	}
	wantBound := errorUpperBound(block.QuantStep, meta.FrameLength, meta.FrameHop, meta.Window, block.TruthNoiseFloorStdQ16)
	if block.ErrorUpperBound != wantBound {
		return violation(block.Frame, block.Block, "error_upper_bound", "checkpoint error upper bound does not match the declared formula")
	}
	if !validHash(block.OriginalHash) {
		return violation(block.Frame, block.Block, "original_hash", "original core hash is incomplete")
	}
	if !validHash(block.ReconstructionHash) {
		return violation(block.Frame, block.Block, "reconstruction_hash", "reconstruction core hash is incomplete")
	}
	return nil
}

func verifyFrame(report model.QualityReport, frame int, blocks []model.BlockResult, meta model.SnapshotMeta) *Violation {
	if frame >= len(report.Frames) {
		return violation(frame, -1, "frames", "frame report is missing")
	}
	frameReport := report.Frames[frame]
	if len(frameReport.Blocks) != len(blocks) {
		return violation(frame, -1, "blocks", "frame block summary count mismatch")
	}
	windowsByChannel := map[int][]model.WindowStat{}
	residualOriginal := map[int]int64{}
	residualReconstruction := map[int]int64{}
	residualCount := map[int]int64{}
	var totalOriginal, totalReconstruction, totalCount int64
	var framePSNRSum, frameSSIMSum, frameWindows int64
	var frameSampleErrorSSE int64
	for i, block := range blocks {
		summary := frameReport.Blocks[i]
		if summary.Frame != block.Frame || summary.Block != block.Block ||
			summary.CoreStart != block.CoreStart || summary.CoreEnd != block.CoreEnd ||
			summary.BandLeft != block.BandLeft || summary.BandRight != block.BandRight ||
			summary.SampleErrorSSE != block.SampleErrorSSE ||
			summary.ErrorUpperBound != block.ErrorUpperBound ||
			summary.BoundaryJump != block.BoundaryJump ||
			summary.ReconstructionHash != block.ReconstructionHash ||
			summary.OriginalHash != block.OriginalHash ||
			summary.WindowCount != len(block.Windows) {
			return violation(frame, block.Block, "block_summary", "frame block summary does not match checkpoint block")
		}
		for _, window := range block.Windows {
			windowsByChannel[window.Channel] = append(windowsByChannel[window.Channel], window)
			framePSNRSum += psnrMicroDB(window.SSE, window.Length)
			frameSSIMSum += ssimQ16(window)
			frameWindows++
		}
		for _, residual := range block.Residuals {
			residualOriginal[residual.Channel] += residual.OriginalSSE
			residualReconstruction[residual.Channel] += residual.ReconstructionSSE
			residualCount[residual.Channel] += residual.Count
			totalOriginal += residual.OriginalSSE
			totalReconstruction += residual.ReconstructionSSE
			totalCount += residual.Count
		}
		frameSampleErrorSSE += block.SampleErrorSSE
	}
	if frameWindows > 0 {
		if frameReport.PSNRUDb != framePSNRSum/frameWindows {
			return violation(frame, -1, "psnr_udb", "frame PSNR does not match independent window merge")
		}
		if frameReport.SSIMQ16 != frameSSIMSum/frameWindows {
			return violation(frame, -1, "ssim_q16", "frame SSIM does not match independent window merge")
		}
	}
	wantOrigNoise, wantReconNoise := noiseQ16(totalOriginal, totalCount), noiseQ16(totalReconstruction, totalCount)
	if frameReport.OriginalNoiseStdQ16 != wantOrigNoise {
		return violation(frame, -1, "original_noise_std_q16", "frame original noise statistic mismatch")
	}
	if frameReport.ReconstructionNoiseStdQ16 != wantReconNoise {
		return violation(frame, -1, "reconstruction_noise_std_q16", "frame reconstruction noise statistic mismatch")
	}
	wantQuantizationResidual := noiseQ16(frameSampleErrorSSE, int64(meta.FrameLength*meta.Channels))
	if frameReport.QuantizationResidualStdQ16 != wantQuantizationResidual {
		return violation(frame, -1, "quantization_residual_std_q16", "frame quantization residual statistic mismatch")
	}
	wantBound := errorUpperBound(quantStepFromBlock(blocks[0]), meta.FrameLength, meta.FrameHop, meta.Window, blocks[0].TruthNoiseFloorStdQ16)
	if frameReport.ErrorUpperBound != wantBound {
		return violation(frame, -1, "error_upper_bound", "frame error upper bound does not match independent recomputation")
	}
	if frameReport.TruthExpectedSNRUDb != blocks[0].TruthExpectedSNRUDb {
		return violation(frame, -1, "truth_expected_snr_udb", "persisted truth expected SNR mismatch")
	}
	if frameReport.TruthNoiseFloorStdQ16 != blocks[0].TruthNoiseFloorStdQ16 {
		return violation(frame, -1, "truth_noise_floor_std_q16", "persisted truth noise floor mismatch")
	}
	if frameReport.NoiseFloorDeltaQ16 != frameReport.QuantizationResidualStdQ16-frameReport.TruthNoiseFloorStdQ16 {
		return violation(frame, -1, "noise_floor_delta_q16", "noise floor delta is inconsistent")
	}
	if len(frameReport.Channels) != meta.Channels {
		return violation(frame, -1, "channels", "frame channel report count mismatch")
	}
	for _, channelReport := range frameReport.Channels {
		windows := windowsByChannel[channelReport.Channel]
		sort.Slice(windows, func(i, j int) bool { return windows[i].Start < windows[j].Start })
		var psnrSum, ssimSum int64
		for _, window := range windows {
			psnrSum += psnrMicroDB(window.SSE, window.Length)
			ssimSum += ssimQ16(window)
		}
		if len(windows) != channelReport.WindowCount {
			return violation(frame, -1, "channels.window_count", "channel window count mismatch")
		}
		if len(windows) > 0 {
			if channelReport.PSNRUDb != psnrSum/int64(len(windows)) {
				return violation(frame, -1, "channels.psnr_udb", "channel PSNR merge mismatch")
			}
			if channelReport.SSIMQ16 != ssimSum/int64(len(windows)) {
				return violation(frame, -1, "channels.ssim_q16", "channel SSIM merge mismatch")
			}
		}
		wantChannelOriginal := noiseQ16(residualOriginal[channelReport.Channel], residualCount[channelReport.Channel])
		wantChannelReconstruction := noiseQ16(residualReconstruction[channelReport.Channel], residualCount[channelReport.Channel])
		if channelReport.OriginalNoiseStdQ16 != wantChannelOriginal || channelReport.ReconstructionNoiseStdQ16 != wantChannelReconstruction {
			return violation(frame, -1, "channels.noise_std_q16", "channel residual merge mismatch")
		}
	}
	return nil
}

func verifyErrorPath(report model.QualityReport, blocks []model.BlockResult) *Violation {
	expected := len(blocks) - 1
	if len(report.ErrorPath) != expected {
		return violation(-1, -1, "error_path", "error path edge count mismatch")
	}
	for i := 0; i < expected; i++ {
		from := blocks[i]
		to := blocks[i+1]
		edge := report.ErrorPath[i]
		if edge.FromFrame != from.Frame || edge.FromBlock != from.Block ||
			edge.ToFrame != to.Frame || edge.ToBlock != to.Block ||
			edge.FrameBoundary != (from.Frame != to.Frame) ||
			edge.ErrorJump != from.BoundaryJump {
			return violation(from.Frame, from.Block, "error_path", "error propagation edge mismatch")
		}
	}
	return nil
}

func psnrMicroDB(sse int64, count int) int64 {
	if count <= 0 {
		return 0
	}
	if sse <= 0 {
		return 300_000_000
	}
	value := 10 * math.Log10(float64(32767*32767)*float64(count)/float64(sse)) * 1_000_000
	if value > 300_000_000 {
		return 300_000_000
	}
	return int64(math.Floor(value))
}

func ssimQ16(stat model.WindowStat) int64 {
	if stat.Length <= 0 {
		return 0
	}
	n := float64(stat.Length)
	meanX := float64(stat.SumX) / n
	meanY := float64(stat.SumY) / n
	varX := float64(stat.SumXX)/n - meanX*meanX
	varY := float64(stat.SumYY)/n - meanY*meanY
	cov := float64(stat.SumXY)/n - meanX*meanY
	if varX < 0 {
		varX = 0
	}
	if varY < 0 {
		varY = 0
	}
	const (
		c1 = (0.01 * 65535.0) * (0.01 * 65535.0)
		c2 = (0.03 * 65535.0) * (0.03 * 65535.0)
	)
	denominator := (meanX*meanX + meanY*meanY + c1) * (varX + varY + c2)
	if denominator == 0 {
		return 1 << 16
	}
	value := math.Floor((2*meanX*meanY + c1) * (2*cov + c2) / denominator * 65536)
	if value < -65536 {
		return -65536
	}
	if value > 65536 {
		return 65536
	}
	return int64(value)
}

func noiseQ16(sse, count int64) int64 {
	if count <= 0 {
		return 0
	}
	return int64(math.Round(math.Sqrt(float64(sse)/float64(count)) * 65536))
}

func errorUpperBound(quantStep int64, frameLength, frameHop, window int, noiseFloorStdQ16 int64) int64 {
	if quantStep <= 0 || frameLength <= 0 || frameHop <= 0 || window <= 0 {
		return 0
	}
	quantVariance := float64(quantStep*quantStep) * float64(2*frameLength-1) /
		float64(12*frameLength*frameLength)
	noiseStd := float64(noiseFloorStdQ16) / 65536.0
	base := math.Sqrt(float64(window) * (quantVariance + noiseStd*noiseStd))
	hopTerm := float64(quantStep) * float64(max(0, frameLength-frameHop)) / float64(2*frameLength)
	bound := int64(math.Ceil(base + hopTerm))
	if bound < 1 {
		bound = 1
	}
	return bound
}

func quantStepFromBlock(blocks ...model.BlockResult) int64 {
	if len(blocks) == 0 {
		return 0
	}
	return blocks[0].QuantStep
}

func validHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func violation(frame, block int, field, message string) *Violation {
	return &Violation{Frame: frame, Block: block, Field: field, Message: message}
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
