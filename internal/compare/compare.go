package compare

import (
	"fmt"
	"reflect"

	"audioeval/internal/model"
)

type Mismatch struct {
	Frame    int    `json:"frame"`
	Block    int    `json:"block"`
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("first mismatch frame=%d block=%d field=%s expected=%s actual=%s", m.Frame, m.Block, m.Field, m.Expected, m.Actual)
}

func Reports(expected, actual model.QualityReport) *Mismatch {
	if mismatch := topLevel(expected, actual); mismatch != nil {
		return mismatch
	}
	if len(expected.Frames) != len(actual.Frames) {
		return mismatchAt(-1, -1, "frames", fmt.Sprint(len(expected.Frames)), fmt.Sprint(len(actual.Frames)))
	}
	for frame := range expected.Frames {
		if mismatch := compareFrame(expected.Frames[frame], actual.Frames[frame]); mismatch != nil {
			return mismatch
		}
	}
	if !reflect.DeepEqual(expected.CommitSequence, actual.CommitSequence) {
		return mismatchAt(-1, -1, "commit_sequence", fmt.Sprint(expected.CommitSequence), fmt.Sprint(actual.CommitSequence))
	}
	if len(expected.ErrorPath) != len(actual.ErrorPath) {
		return mismatchAt(-1, -1, "error_path.length", fmt.Sprint(len(expected.ErrorPath)), fmt.Sprint(len(actual.ErrorPath)))
	}
	for i := range expected.ErrorPath {
		if expected.ErrorPath[i] != actual.ErrorPath[i] {
			return mismatchAt(expected.ErrorPath[i].FromFrame, expected.ErrorPath[i].FromBlock, "error_path",
				fmt.Sprintf("%+v", expected.ErrorPath[i]), fmt.Sprintf("%+v", actual.ErrorPath[i]))
		}
	}
	return nil
}

func topLevel(expected, actual model.QualityReport) *Mismatch {
	if expected.SchemaVersion != actual.SchemaVersion {
		return mismatchAt(-1, -1, "schema_version", fmt.Sprint(expected.SchemaVersion), fmt.Sprint(actual.SchemaVersion))
	}
	if expected.SampleID != actual.SampleID {
		return mismatchAt(-1, -1, "sample_id", expected.SampleID, actual.SampleID)
	}
	if expected.Quality != actual.Quality {
		return mismatchAt(-1, -1, "quality", string(expected.Quality), string(actual.Quality))
	}
	if expected.InputHash != actual.InputHash {
		return mismatchAt(-1, -1, "input_hash", expected.InputHash, actual.InputHash)
	}
	if expected.ParamVersion != actual.ParamVersion {
		return mismatchAt(-1, -1, "param_version", expected.ParamVersion, actual.ParamVersion)
	}
	if expected.AlgorithmVersion != actual.AlgorithmVersion {
		return mismatchAt(-1, -1, "algorithm_version", expected.AlgorithmVersion, actual.AlgorithmVersion)
	}
	if expected.TruthFingerprint != actual.TruthFingerprint {
		return mismatchAt(-1, -1, "truth_fingerprint", expected.TruthFingerprint, actual.TruthFingerprint)
	}
	if expected.Params.Window != actual.Params.Window {
		return mismatchAt(-1, -1, "params.window", fmt.Sprint(expected.Params.Window), fmt.Sprint(actual.Params.Window))
	}
	if expected.Params.WindowHop != actual.Params.WindowHop {
		return mismatchAt(-1, -1, "params.window_hop", fmt.Sprint(expected.Params.WindowHop), fmt.Sprint(actual.Params.WindowHop))
	}
	if expected.Params.BlocksPerFrame != actual.Params.BlocksPerFrame {
		return mismatchAt(-1, -1, "params.blocks_per_frame", fmt.Sprint(expected.Params.BlocksPerFrame), fmt.Sprint(actual.Params.BlocksPerFrame))
	}
	if expected.Params.BandWidth != actual.Params.BandWidth {
		return mismatchAt(-1, -1, "params.band_width", fmt.Sprint(expected.Params.BandWidth), fmt.Sprint(actual.Params.BandWidth))
	}
	if expected.Alignment != actual.Alignment {
		return mismatchAt(-1, -1, "alignment", fmt.Sprintf("%+v", expected.Alignment), fmt.Sprintf("%+v", actual.Alignment))
	}
	if !expected.Complete || !actual.Complete {
		return mismatchAt(-1, -1, "complete", fmt.Sprint(expected.Complete), fmt.Sprint(actual.Complete))
	}
	return nil
}

func compareFrame(expected, actual model.FrameReport) *Mismatch {
	frame := expected.Frame
	if expected.Frame != actual.Frame {
		return mismatchAt(frame, -1, "frame", fmt.Sprint(expected.Frame), fmt.Sprint(actual.Frame))
	}
	if expected.PSNRUDb != actual.PSNRUDb {
		return mismatchAt(frame, -1, "psnr_udb", fmt.Sprint(expected.PSNRUDb), fmt.Sprint(actual.PSNRUDb))
	}
	if expected.SSIMQ16 != actual.SSIMQ16 {
		return mismatchAt(frame, -1, "ssim_q16", fmt.Sprint(expected.SSIMQ16), fmt.Sprint(actual.SSIMQ16))
	}
	if expected.OriginalNoiseStdQ16 != actual.OriginalNoiseStdQ16 {
		return mismatchAt(frame, -1, "original_noise_std_q16", fmt.Sprint(expected.OriginalNoiseStdQ16), fmt.Sprint(actual.OriginalNoiseStdQ16))
	}
	if expected.ReconstructionNoiseStdQ16 != actual.ReconstructionNoiseStdQ16 {
		return mismatchAt(frame, -1, "reconstruction_noise_std_q16", fmt.Sprint(expected.ReconstructionNoiseStdQ16), fmt.Sprint(actual.ReconstructionNoiseStdQ16))
	}
	if expected.QuantizationResidualStdQ16 != actual.QuantizationResidualStdQ16 {
		return mismatchAt(frame, -1, "quantization_residual_std_q16", fmt.Sprint(expected.QuantizationResidualStdQ16), fmt.Sprint(actual.QuantizationResidualStdQ16))
	}
	if expected.TruthExpectedSNRUDb != actual.TruthExpectedSNRUDb {
		return mismatchAt(frame, -1, "truth_expected_snr_udb", fmt.Sprint(expected.TruthExpectedSNRUDb), fmt.Sprint(actual.TruthExpectedSNRUDb))
	}
	if expected.TruthNoiseFloorStdQ16 != actual.TruthNoiseFloorStdQ16 {
		return mismatchAt(frame, -1, "truth_noise_floor_std_q16", fmt.Sprint(expected.TruthNoiseFloorStdQ16), fmt.Sprint(actual.TruthNoiseFloorStdQ16))
	}
	if expected.ErrorUpperBound != actual.ErrorUpperBound {
		return mismatchAt(frame, -1, "error_upper_bound", fmt.Sprint(expected.ErrorUpperBound), fmt.Sprint(actual.ErrorUpperBound))
	}
	if expected.InverseErrorSSE != actual.InverseErrorSSE {
		return mismatchAt(frame, -1, "inverse_error_sse", fmt.Sprint(expected.InverseErrorSSE), fmt.Sprint(actual.InverseErrorSSE))
	}
	if expected.BoundaryErrorJump != actual.BoundaryErrorJump {
		return mismatchAt(frame, -1, "boundary_error_jump", fmt.Sprint(expected.BoundaryErrorJump), fmt.Sprint(actual.BoundaryErrorJump))
	}
	if len(expected.Channels) != len(actual.Channels) {
		return mismatchAt(frame, -1, "channels.length", fmt.Sprint(len(expected.Channels)), fmt.Sprint(len(actual.Channels)))
	}
	for ch := range expected.Channels {
		e := expected.Channels[ch]
		a := actual.Channels[ch]
		if e.Channel != a.Channel {
			return mismatchAt(frame, -1, "channels.channel", fmt.Sprint(e.Channel), fmt.Sprint(a.Channel))
		}
		if e.PSNRUDb != a.PSNRUDb {
			return mismatchAt(frame, -1, "channels.psnr_udb", fmt.Sprint(e.PSNRUDb), fmt.Sprint(a.PSNRUDb))
		}
		if e.SSIMQ16 != a.SSIMQ16 {
			return mismatchAt(frame, -1, "channels.ssim_q16", fmt.Sprint(e.SSIMQ16), fmt.Sprint(a.SSIMQ16))
		}
		if e.OriginalNoiseStdQ16 != a.OriginalNoiseStdQ16 || e.ReconstructionNoiseStdQ16 != a.ReconstructionNoiseStdQ16 {
			return mismatchAt(frame, -1, "channels.noise_std_q16", fmt.Sprint(e.OriginalNoiseStdQ16, e.ReconstructionNoiseStdQ16), fmt.Sprint(a.OriginalNoiseStdQ16, a.ReconstructionNoiseStdQ16))
		}
		if e.WindowCount != a.WindowCount {
			return mismatchAt(frame, -1, "channels.window_count", fmt.Sprint(e.WindowCount), fmt.Sprint(a.WindowCount))
		}
		if e.InverseErrorSSE != a.InverseErrorSSE {
			return mismatchAt(frame, -1, "channels.inverse_error_sse", fmt.Sprint(e.InverseErrorSSE), fmt.Sprint(a.InverseErrorSSE))
		}
		if !reflect.DeepEqual(e.QuantCoeffErrorsQ14, a.QuantCoeffErrorsQ14) {
			return mismatchAt(frame, -1, "channels.quant_coeff_errors_q14", "different coefficient error sequence", "different coefficient error sequence")
		}
	}
	if len(expected.Blocks) != len(actual.Blocks) {
		return mismatchAt(frame, -1, "blocks.length", fmt.Sprint(len(expected.Blocks)), fmt.Sprint(len(actual.Blocks)))
	}
	for i := range expected.Blocks {
		e := expected.Blocks[i]
		a := actual.Blocks[i]
		if e.Frame != a.Frame || e.Block != a.Block {
			return mismatchAt(frame, e.Block, "block_coordinate", fmt.Sprintf("%d/%d", e.Frame, e.Block), fmt.Sprintf("%d/%d", a.Frame, a.Block))
		}
		if e.CoreStart != a.CoreStart || e.CoreEnd != a.CoreEnd || e.BandLeft != a.BandLeft || e.BandRight != a.BandRight {
			return mismatchAt(frame, e.Block, "block_geometry", fmt.Sprintf("%d..%d bands %d/%d", e.CoreStart, e.CoreEnd, e.BandLeft, e.BandRight),
				fmt.Sprintf("%d..%d bands %d/%d", a.CoreStart, a.CoreEnd, a.BandLeft, a.BandRight))
		}
		if e.WindowCount != a.WindowCount {
			return mismatchAt(frame, e.Block, "block.window_count", fmt.Sprint(e.WindowCount), fmt.Sprint(a.WindowCount))
		}
		if e.Quality != a.Quality {
			return mismatchAt(frame, e.Block, "block.quality", fmt.Sprintf("%+v", e.Quality), fmt.Sprintf("%+v", a.Quality))
		}
		if e.SampleErrorSSE != a.SampleErrorSSE {
			return mismatchAt(frame, e.Block, "block.sample_error_sse", fmt.Sprint(e.SampleErrorSSE), fmt.Sprint(a.SampleErrorSSE))
		}
		if e.ErrorUpperBound != a.ErrorUpperBound {
			return mismatchAt(frame, e.Block, "block.error_upper_bound", fmt.Sprint(e.ErrorUpperBound), fmt.Sprint(a.ErrorUpperBound))
		}
		if e.BoundaryJump != a.BoundaryJump {
			return mismatchAt(frame, e.Block, "block.boundary_jump", fmt.Sprint(e.BoundaryJump), fmt.Sprint(a.BoundaryJump))
		}
		if e.OriginalHash != a.OriginalHash {
			return mismatchAt(frame, e.Block, "block.original_hash", e.OriginalHash, a.OriginalHash)
		}
		if e.ReconstructionHash != a.ReconstructionHash {
			return mismatchAt(frame, e.Block, "block.reconstruction_hash", e.ReconstructionHash, a.ReconstructionHash)
		}
	}
	return nil
}

func mismatchAt(frame, block int, field, expected, actual string) *Mismatch {
	return &Mismatch{Frame: frame, Block: block, Field: field, Expected: expected, Actual: actual}
}
