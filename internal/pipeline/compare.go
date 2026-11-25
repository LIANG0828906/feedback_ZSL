package pipeline

import (
	"fmt"

	"prosodyalign/internal/errs"
	"prosodyalign/internal/model"
)

func ComparePaths(runs ...*PathRun) *ComparisonReport {
	report := &ComparisonReport{AllEqual: true}
	if len(runs) < 2 {
		return report
	}
	base := runs[0]
	report.ComparedEpochs = len(base.Epochs)
	commitEqual := true
	for _, r := range runs[1:] {
		if !equalStringSlices(base.CommitSequence, r.CommitSequence) {
			commitEqual = false
			m := Mismatch{Epoch: -1, FrameIndex: -1, Field: "commit_sequence", Path: r.Name,
				Expected: base.CommitSequence, Actual: r.CommitSequence}
			report.Mismatches = append(report.Mismatches, m)
			if report.FirstMismatch == nil {
				report.FirstMismatch = &m
			}
		}
	}
	report.CommitSequencesEqual = commitEqual

	for epochIdx := 0; epochIdx < len(base.Epochs); epochIdx++ {
		for runIdx := 1; runIdx < len(runs); runIdx++ {
			other := runs[runIdx]
			if epochIdx >= len(other.Epochs) {
				m := Mismatch{Epoch: epochIdx, FrameIndex: -1, Field: "epoch_count", Path: other.Name, Expected: len(base.Epochs), Actual: len(other.Epochs)}
				addMismatch(report, m)
				continue
			}
			be := base.Epochs[epochIdx]
			oe := other.Epochs[epochIdx]
			for fragIdx := 0; fragIdx < len(be.OrderedFragmentIDs); fragIdx++ {
				fragID := be.OrderedFragmentIDs[fragIdx]
				a := findOutcome(be, fragID)
				b := findOutcome(oe, fragID)
				if a == nil || b == nil {
					m := Mismatch{Epoch: epochIdx, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: "outcome_presence", Path: other.Name}
					addMismatch(report, m)
					continue
				}
				if a.OK != b.OK {
					m := Mismatch{Epoch: epochIdx, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: "outcome.ok", Path: other.Name, Expected: a.OK, Actual: b.OK}
					addMismatch(report, m)
					continue
				}
				if !a.OK {
					ac := errorCode(a.ErrorDetail)
					bc := errorCode(b.ErrorDetail)
					if ac != bc {
						m := Mismatch{Epoch: epochIdx, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: "error_detail.code", Path: other.Name, Expected: ac, Actual: bc}
						addMismatch(report, m)
					}
					continue
				}
				if m := compareResult(epochIdx, fragIdx, fragID, a.Result, b.Result, other.Name); m != nil {
					addMismatch(report, *m)
				}
			}
		}
	}
	report.AllEqual = len(report.Mismatches) == 0
	return report
}

type ComparisonReportShim struct{ FrameIndexUnused int }

func addMismatch(r *ComparisonReport, m Mismatch) {
	r.Mismatches = append(r.Mismatches, m)
	if r.FirstMismatch == nil {
		cp := m
		r.FirstMismatch = &cp
	}
}

func findOutcome(e *EpochOutput, fragmentID string) *Outcome {
	for i := range e.Outcomes {
		if e.Outcomes[i].FragmentID == fragmentID {
			return &e.Outcomes[i]
		}
	}
	return nil
}

func errorCode(d *errs.Detail) string {
	if d == nil {
		return ""
	}
	return d.Code
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func compareResult(epoch, fragIdx int, fragID string, a, b *model.DialogResult, path string) *Mismatch {
	if a == nil || b == nil {
		return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: "result", Path: path, Expected: a != nil, Actual: b != nil}
	}
	if a.EnvelopeDigest != b.EnvelopeDigest {
		if m := compareFrames(epoch, fragIdx, fragID, a.Frames, b.Frames, path); m != nil {
			return m
		}
	}
	if m := compareFrames(epoch, fragIdx, fragID, a.Frames, b.Frames, path); m != nil {
		return m
	}
	if m := compareAccent(epoch, fragIdx, fragID, a.Accent, b.Accent, path); m != nil {
		return m
	}
	return compareFit(epoch, fragIdx, fragID, a.Fit, b.Fit, path)
}

func compareFrames(epoch, fragIdx int, fragID string, a, b []model.FrameResult, path string) *Mismatch {
	if len(a) != len(b) {
		return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: minInt(len(a), len(b)), Field: "frames.length", Path: path, Expected: len(a), Actual: len(b)}
	}
	for i := range a {
		af, bf := a[i], b[i]
		frameNo := af.FrameIndex
		checks := []struct {
			field  string
			av, bv any
		}{
			{"frame_index", af.FrameIndex, bf.FrameIndex},
			{"start_sample", af.StartSample, bf.StartSample},
			{"center_sample", af.CenterSample, bf.CenterSample},
			{"raw_loudness", af.RawLoudness, bf.RawLoudness},
			{"smoothed_loudness", af.SmoothedLoudness, bf.SmoothedLoudness},
			{"envelope_fixed", af.EnvelopeFixed, bf.EnvelopeFixed},
			{"phoneme_index", af.PhonemeIndex, bf.PhonemeIndex},
		}
		for _, c := range checks {
			if c.av != c.bv {
				return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: frameNo, Field: "frames." + c.field, Path: path, Expected: c.av, Actual: c.bv}
			}
		}
	}
	return nil
}

func compareAccent(epoch, fragIdx int, fragID string, a, b model.AccentResult, path string) *Mismatch {
	checks := []struct {
		field  string
		av, bv any
	}{
		{"accent.sample", a.Sample, b.Sample},
		{"accent.phoneme_index", a.PhonemeIndex, b.PhonemeIndex},
		{"accent.loudness_peak", a.LoudnessPeak, b.LoudnessPeak},
		{"accent.pitch_peak", a.PitchPeak, b.PitchPeak},
		{"accent.distance", a.DistanceSamples, b.DistanceSamples},
		{"accent.final_frame_index", a.FinalFrameIndex, b.FinalFrameIndex},
	}
	for _, c := range checks {
		if c.av != c.bv {
			return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: a.FinalFrameIndex, Field: c.field, Path: path, Expected: c.av, Actual: c.bv}
		}
	}
	return nil
}

func compareFit(epoch, fragIdx int, fragID string, a, b model.FitResult, path string) *Mismatch {
	scalar := []struct {
		field  string
		av, bv any
	}{
		{"fit.target_duration", a.TargetDuration, b.TargetDuration},
		{"fit.final_duration", a.FinalDuration, b.FinalDuration},
		{"fit.gap", a.Gap, b.Gap},
		{"fit.allocated", a.Allocated, b.Allocated},
		{"fit.shortfall", a.Shortfall, b.Shortfall},
		{"fit.warped_accent", a.WarpedAccent, b.WarpedAccent},
		{"fit.feasible", a.Feasible, b.Feasible},
	}
	for _, c := range scalar {
		if c.av != c.bv {
			return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: c.field, Path: path, Expected: c.av, Actual: c.bv}
		}
	}
	if len(a.Segments) != len(b.Segments) {
		return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: "fit.segments.length", Path: path, Expected: len(a.Segments), Actual: len(b.Segments)}
	}
	for i := range a.Segments {
		as, bs := a.Segments[i], b.Segments[i]
		checks := []struct {
			field  string
			av, bv any
		}{
			{"original_start", as.OriginalStart, bs.OriginalStart},
			{"original_end", as.OriginalEnd, bs.OriginalEnd},
			{"added_samples", as.AddedSamples, bs.AddedSamples},
			{"new_start", as.NewStart, bs.NewStart},
			{"new_end", as.NewEnd, bs.NewEnd},
			{"new_duration", as.NewDuration, bs.NewDuration},
			{"coefficient_permille", as.CoefficientPermille, bs.CoefficientPermille},
			{"cap_triggered", as.CapTriggered, bs.CapTriggered},
		}
		for _, c := range checks {
			if c.av != c.bv {
				return &Mismatch{Epoch: epoch, FragmentIndex: fragIdx, FragmentID: fragID, FrameIndex: -1, Field: fmt.Sprintf("fit.segments[%d].%s", i, c.field), Path: path, Expected: c.av, Actual: c.bv}
			}
		}
	}
	return nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
