package project

import (
	"fmt"

	"loudnessworkbench/apperr"
)

type Mismatch struct {
	Path  string `json:"path"`
	Left  any    `json:"left"`
	Right any    `json:"right"`
}

type EquivalenceReport struct {
	Equivalent    bool      `json:"equivalent"`
	LeftVersion   string    `json:"left_version"`
	RightVersion  string    `json:"right_version"`
	FirstMismatch *Mismatch `json:"first_mismatch,omitempty"`
	Trace         []string  `json:"degradation_trace"`
}

func ComparePrograms(left, right *Program) (*EquivalenceReport, error) {
	if left == nil || right == nil {
		return nil, apperr.New(apperr.CodeLossy, "both programs are required")
	}
	report := &EquivalenceReport{
		Equivalent:   true,
		LeftVersion:  left.SourceVersion,
		RightVersion: right.SourceVersion,
		Trace:        append(traceFor(left), traceFor(right)...),
	}
	mismatch := func(path string, l, r any) *Mismatch {
		return &Mismatch{Path: path, Left: l, Right: r}
	}
	var found *Mismatch
	checkNumber := func(path string, l, r float64) {
		if found == nil && !nearlyEqual(l, r, 1e-9) {
			found = mismatch(path, l, r)
		}
	}
	checkInt := func(path string, l, r int) {
		if found == nil && l != r {
			found = mismatch(path, l, r)
		}
	}
	checkString := func(path, l, r string) {
		if found == nil && l != r {
			found = mismatch(path, l, r)
		}
	}
	checkString("program_id", left.ProjectID, right.ProjectID)
	checkInt("sample_rate_hz", left.SampleRateHz, right.SampleRateHz)
	checkNumber("target_lufs", left.TargetLUFS, right.TargetLUFS)
	checkNumber("frame_ms", left.FrameMs, right.FrameMs)
	checkNumber("hop_ms", left.HopMs, right.HopMs)
	checkNumber("gate.absolute_lufs", left.Gate.AbsoluteLUFS, right.Gate.AbsoluteLUFS)
	checkNumber("gate.relative_lu", left.Gate.RelativeLU, right.Gate.RelativeLU)
	checkNumber("compressor.threshold_dbfs", left.Compressor.ThresholdDBFS, right.Compressor.ThresholdDBFS)
	checkNumber("compressor.ratio", left.Compressor.Ratio, right.Compressor.Ratio)
	checkNumber("compressor.attack_ms", left.Compressor.AttackMs, right.Compressor.AttackMs)
	checkNumber("compressor.release_ms", left.Compressor.ReleaseMs, right.Compressor.ReleaseMs)
	checkNumber("limiter.ceiling_dbtp", left.Limiter.CeilingDBTP, right.Limiter.CeilingDBTP)
	checkNumber("limiter.lookahead_ms", left.Limiter.LookaheadMs, right.Limiter.LookaheadMs)
	checkInt("processing_stages", len(left.StageOrder), len(right.StageOrder))
	for i := 0; found == nil && i < len(left.StageOrder) && i < len(right.StageOrder); i++ {
		checkString(fmt.Sprintf("processing_stages[%d]", i), left.StageOrder[i], right.StageOrder[i])
	}
	checkInt("segments", len(left.Segments), len(right.Segments))
	for i := 0; found == nil && i < len(left.Segments) && i < len(right.Segments); i++ {
		base := fmt.Sprintf("segments[%d]", i)
		l, r := left.Segments[i], right.Segments[i]
		checkString(base+".id", l.ID, r.ID)
		checkInt(base+".channels", l.Channels, r.Channels)
		checkInt(base+".sample_rate_hz", l.Source.SampleRateHz, r.Source.SampleRateHz)
		checkInt(base+".frame_count", l.Frames(), r.Frames())
		checkString(base+".content_sha256", l.ContentSHA256, r.ContentSHA256)
	}
	if found != nil {
		report.Equivalent = false
		report.FirstMismatch = found
		err := apperr.New(apperr.CodeLossy, "program representations are not equivalent").
			WithPath(found.Path).WithField(found.Path).WithSides(found.Left, found.Right).
			WithTrace(report.Trace)
		return report, err
	}
	return report, nil
}

func MustBeEquivalent(left, right *Program) error {
	_, err := ComparePrograms(left, right)
	return err
}

func traceFor(p *Program) []string {
	switch p.SourceVersion {
	case SourceV1:
		return []string{
			"v1.gate.relative_lu -> gate.relative_lu (direct)",
			"v1.compression_chain[compress,limit] -> processing_stages[compressor,limiter]",
			"v1.segments[].source.path+offset_samples+sample_count -> decoded content hash",
		}
	case SourceV2:
		return []string{
			"v2.gating.relativeDropLu -> gate.relative_lu (direct)",
			"v2.stageOrder -> processing_stages",
			"v2.assets[assetId] -> segments[].content_sha256",
		}
	case SourceV3:
		return []string{
			"v3.analysis.gates[relative,energy_ratio] -> gate.relative_lu = -10*log10(ratio)",
			"v3.processingStages[].type -> processing_stages",
			"v3.media.clips + segments[].clip -> segments[].content_sha256",
		}
	default:
		return []string{fmt.Sprintf("source version %q uses canonical mapping", p.SourceVersion)}
	}
}
