package loudness

import (
	"fmt"
	"sort"
)

// ComparisonMismatch identifies the first differing clip and frame.
type ComparisonMismatch struct {
	ClipID     int    `json:"clipId"`
	FrameIndex int    `json:"frameIndex"`
	Field      string `json:"field"`
	Left       string `json:"left"`
	Right      string `json:"right"`
}

func (m *ComparisonMismatch) Error() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("first mismatch clip=%d frame=%d field=%s left=%s right=%s", m.ClipID, m.FrameIndex, m.Field, m.Left, m.Right)
}

// CompareBuildResults compares every acceptance-relevant value and returns the
// first mismatch. nil means byte-for-byte integer equivalence.
func CompareBuildResults(left, right *BuildResult) *ComparisonMismatch {
	if left == nil || right == nil {
		return &ComparisonMismatch{ClipID: -1, FrameIndex: -1, Field: "result", Left: fmt.Sprint(left != nil), Right: fmt.Sprint(right != nil)}
	}
	if mismatch := compareIntSlices("commitSequence", left.CommitSequence, right.CommitSequence); mismatch != "" {
		return &ComparisonMismatch{ClipID: -1, FrameIndex: -1, Field: "commitSequence", Left: mismatch, Right: "different sequence"}
	}
	if mismatch := compareInvalidations(left.Invalidations, right.Invalidations); mismatch != nil {
		return mismatch
	}
	leftByID := make(map[int]ClipResult, len(left.Clips))
	rightByID := make(map[int]ClipResult, len(right.Clips))
	for _, clip := range left.Clips {
		leftByID[clip.ClipID] = clip
	}
	for _, clip := range right.Clips {
		rightByID[clip.ClipID] = clip
	}
	ids := make([]int, 0, len(leftByID))
	for id := range leftByID {
		ids = append(ids, id)
	}
	for id := range rightByID {
		if _, exists := leftByID[id]; !exists {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	for _, id := range ids {
		leftClip, leftOK := leftByID[id]
		rightClip, rightOK := rightByID[id]
		if !leftOK || !rightOK {
			return &ComparisonMismatch{ClipID: id, FrameIndex: -1, Field: "clipPresence", Left: fmt.Sprint(leftOK), Right: fmt.Sprint(rightOK)}
		}
		if leftClip.GainAdjustmentQ12 != rightClip.GainAdjustmentQ12 {
			return &ComparisonMismatch{ClipID: id, FrameIndex: -1, Field: "gainAdjustmentQ12", Left: fmt.Sprint(leftClip.GainAdjustmentQ12), Right: fmt.Sprint(rightClip.GainAdjustmentQ12)}
		}
		if leftClip.CalibratedSegmentDBQ8 != rightClip.CalibratedSegmentDBQ8 {
			return &ComparisonMismatch{ClipID: id, FrameIndex: -1, Field: "calibratedSegmentDbQ8", Left: fmt.Sprint(leftClip.CalibratedSegmentDBQ8), Right: fmt.Sprint(rightClip.CalibratedSegmentDBQ8)}
		}
		if len(leftClip.Frames) != len(rightClip.Frames) {
			return &ComparisonMismatch{ClipID: id, FrameIndex: minInt(len(leftClip.Frames), len(rightClip.Frames)), Field: "frameCount", Left: fmt.Sprint(len(leftClip.Frames)), Right: fmt.Sprint(len(rightClip.Frames))}
		}
		for frameIndex := range leftClip.Frames {
			leftFrame := leftClip.Frames[frameIndex]
			rightFrame := rightClip.Frames[frameIndex]
			checks := []struct {
				field string
				left  int64
				right int64
			}{
				{"startSample", leftFrame.StartSample, rightFrame.StartSample},
				{"endSample", leftFrame.EndSample, rightFrame.EndSample},
				{"energyQ32", leftFrame.EnergyQ32, rightFrame.EnergyQ32},
				{"rmsQ16", leftFrame.RmsQ16, rightFrame.RmsQ16},
				{"measuredDbQ8", leftFrame.MeasuredDBQ8, rightFrame.MeasuredDBQ8},
				{"peakSample", leftFrame.PeakSample, rightFrame.PeakSample},
				{"peakOffset", leftFrame.PeakOffset, rightFrame.PeakOffset},
				{"calibratedEnergyQ32", leftFrame.CalibratedEnergyQ32, rightFrame.CalibratedEnergyQ32},
				{"calibratedDbQ8", leftFrame.CalibratedDBQ8, rightFrame.CalibratedDBQ8},
			}
			for _, check := range checks {
				if check.left != check.right {
					return &ComparisonMismatch{
						ClipID: id, FrameIndex: frameIndex, Field: check.field,
						Left: fmt.Sprint(check.left), Right: fmt.Sprint(check.right),
					}
				}
			}
			if leftFrame.CacheKey != rightFrame.CacheKey {
				return &ComparisonMismatch{
					ClipID: id, FrameIndex: frameIndex, Field: "cacheKey",
					Left: leftFrame.CacheKey, Right: rightFrame.CacheKey,
				}
			}
		}
	}
	return nil
}

func compareInvalidations(left, right []Invalidation) *ComparisonMismatch {
	if len(left) != len(right) {
		return &ComparisonMismatch{ClipID: firstDifferentInvalidationClip(left, right), FrameIndex: -1, Field: "invalidationCount", Left: fmt.Sprint(len(left)), Right: fmt.Sprint(len(right))}
	}
	for i := range left {
		if left[i] != right[i] {
			return &ComparisonMismatch{
				ClipID: left[i].ClipID, FrameIndex: -1, Field: "invalidation",
				Left: fmt.Sprintf("%+v", left[i]), Right: fmt.Sprintf("%+v", right[i]),
			}
		}
	}
	return nil
}

func firstDifferentInvalidationClip(left, right []Invalidation) int {
	limit := minInt(len(left), len(right))
	for i := 0; i < limit; i++ {
		if left[i] != right[i] {
			return left[i].ClipID
		}
	}
	return -1
}

func compareIntSlices(field string, left, right []int) string {
	if len(left) != len(right) {
		return fmt.Sprintf("%s length %d != %d", field, len(left), len(right))
	}
	for i := range left {
		if left[i] != right[i] {
			return fmt.Sprintf("%s[%d] %d != %d", field, i, left[i], right[i])
		}
	}
	return ""
}

// VerifyTruth compares an independently generated truth list against a fresh
// measurement/calibration. Loudness comparisons use the declared Q8 tolerance.
func VerifyTruth(project *Project, truth *TruthDocument) (*ComparisonMismatch, error) {
	if truth == nil {
		return nil, &DomainError{Code: ESchema, Path: "$.truth", Detail: "truth is nil"}
	}
	if truth.ProjectID != project.ID || truth.Version != project.FormatVersion {
		return &ComparisonMismatch{ClipID: -1, FrameIndex: -1, Field: "projectIdentity", Left: truth.ProjectID + "/" + string(truth.Version), Right: project.ID + "/" + string(project.FormatVersion)}, nil
	}
	report, err := MeasureAndCalibrate(project)
	if err != nil {
		return nil, err
	}
	truthClips := make(map[int]ClipTruth, len(truth.Clips))
	for _, clip := range truth.Clips {
		truthClips[clip.ClipID] = clip
	}
	for _, clip := range project.Clips {
		expected, ok := truthClips[clip.ID]
		if !ok {
			return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: -1, Field: "truthPresence", Left: "missing", Right: "present"}, nil
		}
		actual, ok := report.Clips[clip.ID]
		if !ok {
			return nil, &DomainError{Code: ESchema, Path: "$.clips", Detail: fmt.Sprintf("missing actual clip %d", clip.ID)}
		}
		if actual.GainAdjustmentQ12 != expected.GainAdjustmentQ12 {
			return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: -1, Field: "gainAdjustmentQ12", Left: fmt.Sprint(actual.GainAdjustmentQ12), Right: fmt.Sprint(expected.GainAdjustmentQ12)}, nil
		}
		if absInt64(actual.CalibratedSegmentDBQ8-expected.CalibratedSegmentDBQ8) > truth.ToleranceQ8 {
			return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: -1, Field: "calibratedSegmentDbQ8", Left: fmt.Sprint(actual.CalibratedSegmentDBQ8), Right: fmt.Sprint(expected.CalibratedSegmentDBQ8)}, nil
		}
		if len(actual.Frames) != len(expected.Frames) {
			return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: minInt(len(actual.Frames), len(expected.Frames)), Field: "frameCount", Left: fmt.Sprint(len(actual.Frames)), Right: fmt.Sprint(len(expected.Frames))}, nil
		}
		for i := range actual.Frames {
			frame := actual.Frames[i]
			want := expected.Frames[i]
			checks := []struct {
				field string
				left  int64
				right int64
			}{
				{"startSample", frame.StartSample, want.StartSample},
				{"endSample", frame.EndSample, want.EndSample},
				{"energyQ32", frame.EnergyQ32, want.EnergyQ32},
				{"rmsQ16", frame.RmsQ16, want.RmsQ16},
				{"peakSample", frame.PeakSample, want.PeakSample},
				{"peakOffset", frame.PeakOffset, want.PeakOffset},
				{"calibratedEnergyQ32", frame.CalibratedEnergyQ32, want.CalibratedEnergyQ32},
			}
			for _, check := range checks {
				if check.left != check.right {
					return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: i, Field: check.field, Left: fmt.Sprint(check.left), Right: fmt.Sprint(check.right)}, nil
				}
			}
			if absInt64(frame.MeasuredDBQ8-want.MeasuredDBQ8) > truth.ToleranceQ8 {
				return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: i, Field: "measuredDbQ8", Left: fmt.Sprint(frame.MeasuredDBQ8), Right: fmt.Sprint(want.MeasuredDBQ8)}, nil
			}
			if absInt64(frame.CalibratedDBQ8-want.CalibratedDBQ8) > truth.ToleranceQ8 {
				return &ComparisonMismatch{ClipID: clip.ID, FrameIndex: i, Field: "calibratedDbQ8", Left: fmt.Sprint(frame.CalibratedDBQ8), Right: fmt.Sprint(want.CalibratedDBQ8)}, nil
			}
		}
	}
	return nil, nil
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
