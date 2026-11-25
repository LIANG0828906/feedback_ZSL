package audiofp

import "fmt"

type DiffDetail struct {
	Category   string `json:"category"`
	FrameIndex *int   `json:"frame_index,omitempty"`
	EventIndex *int   `json:"event_index,omitempty"`
	Field      string `json:"field"`
	Expected   any    `json:"expected,omitempty"`
	Actual     any    `json:"actual,omitempty"`
	Message    string `json:"message"`
}

type DiffReport struct {
	Consistent    bool          `json:"consistent"`
	ResultPaths   []string      `json:"result_paths"`
	CheckedFrames int           `json:"checked_frames"`
	CheckedEvents int           `json:"checked_events"`
	FirstDiff     *DiffDetail   `json:"first_diff,omitempty"`
	Diffs         []*DiffDetail `json:"diffs,omitempty"`
}

// EnsureIdentical compares frame fingerprints, event intervals and quality
// statistics. It returns E_DIFF with the first located field on mismatch.
func EnsureIdentical(results ...*AnnotationResult) (*DiffReport, error) {
	report := &DiffReport{Consistent: true, ResultPaths: make([]string, 0, len(results))}
	for _, r := range results {
		if r == nil {
			report.Consistent = false
			report.FirstDiff = &DiffDetail{Category: "result", Field: "result", Message: "nil result"}
			return report, diffError(report.FirstDiff)
		}
		report.ResultPaths = append(report.ResultPaths, r.Path)
	}
	if len(results) < 2 {
		return report, nil
	}

	base := results[0]
	for _, candidate := range results[1:] {
		detail := comparePair(base, candidate, report)
		if detail != nil {
			report.Consistent = false
			report.FirstDiff = detail
			return report, diffError(detail)
		}
	}
	report.CheckedFrames = len(base.Frames)
	report.CheckedEvents = len(base.Intervals)
	return report, nil
}

func diffError(d *DiffDetail) *Error {
	e := newError(CodeDiff, d.Field, "%s", d.Message)
	e.Details = d
	if d.FrameIndex != nil {
		e.FrameIndex = *d.FrameIndex
	}
	if d.EventIndex != nil {
		e.EventIndex = *d.EventIndex
		e.EventNo = *d.EventIndex + 1
	}
	e.Expected = d.Expected
	e.Actual = d.Actual
	return e
}

func comparePair(a, b *AnnotationResult, report *DiffReport) *DiffDetail {
	if d := compareFrames(a, b); d != nil {
		return d
	}
	if len(a.Frames) > report.CheckedFrames {
		report.CheckedFrames = len(a.Frames)
	}
	if d := compareIntervals(a, b); d != nil {
		return d
	}
	if len(a.Intervals) > report.CheckedEvents {
		report.CheckedEvents = len(a.Intervals)
	}
	return compareQuality(a, b)
}

func intPtr(v int) *int { return &v }

func frameDiff(i int, field string, expected, actual any) *DiffDetail {
	return &DiffDetail{
		Category: "frame", FrameIndex: intPtr(i), Field: fmt.Sprintf("frames[%d].%s", i, field),
		Expected: expected, Actual: actual,
		Message: fmt.Sprintf("frame %d field %s differs", i, field),
	}
}

func compareFrames(a, b *AnnotationResult) *DiffDetail {
	if len(a.Frames) != len(b.Frames) {
		return &DiffDetail{
			Category: "frame", Field: "frames.length",
			Expected: len(a.Frames), Actual: len(b.Frames),
			Message: fmt.Sprintf("frame count differs: %s=%d %s=%d", a.Path, len(a.Frames), b.Path, len(b.Frames)),
		}
	}
	for i := range a.Frames {
		x := a.Frames[i]
		y := b.Frames[i]
		checks := []struct {
			field string
			x, y  any
		}{
			{"algorithm_version", x.AlgorithmVersion, y.AlgorithmVersion},
			{"frame_index", x.FrameIndex, y.FrameIndex},
			{"start_sample", x.StartSample, y.StartSample},
			{"frame_length", x.FrameLength, y.FrameLength},
			{"frame_shift", x.FrameShift, y.FrameShift},
			{"sample_rate", x.SampleRate, y.SampleRate},
			{"band_count", x.BandCount, y.BandCount},
			{"energy", x.Energy, y.Energy},
			{"peak_band", x.PeakBand, y.PeakBand},
			{"feature_version", x.FeatureVersion, y.FeatureVersion},
			{"param_version", x.ParamVersion, y.ParamVersion},
		}
		for _, c := range checks {
			if c.x != c.y {
				return frameDiff(i, c.field, c.x, c.y)
			}
		}
		if len(x.Bands) != len(y.Bands) {
			return frameDiff(i, "bands.length", len(x.Bands), len(y.Bands))
		}
		for band := range x.Bands {
			if x.Bands[band] != y.Bands[band] {
				return frameDiff(i, fmt.Sprintf("bands[%d]", band), x.Bands[band], y.Bands[band])
			}
		}
		if x.Digest() != y.Digest() {
			return frameDiff(i, "digest", x.Digest(), y.Digest())
		}
	}
	return nil
}

func eventDiff(i int, field string, expected, actual any) *DiffDetail {
	return &DiffDetail{
		Category: "event", EventIndex: intPtr(i), Field: fmt.Sprintf("events[%d].%s", i, field),
		Expected: expected, Actual: actual,
		Message: fmt.Sprintf("event %d field %s differs", i, field),
	}
}

func compareIntervals(a, b *AnnotationResult) *DiffDetail {
	if len(a.Intervals) != len(b.Intervals) {
		return &DiffDetail{
			Category: "event", Field: "events.length",
			Expected: len(a.Intervals), Actual: len(b.Intervals),
			Message: fmt.Sprintf("event count differs: %s=%d %s=%d", a.Path, len(a.Intervals), b.Path, len(b.Intervals)),
		}
	}
	for i := range a.Intervals {
		x := a.Intervals[i]
		y := b.Intervals[i]
		checks := []struct {
			field string
			x, y  any
		}{
			{"id", x.ID, y.ID},
			{"category", x.Category, y.Category},
			{"start_sample", x.StartSample, y.StartSample},
			{"end_sample", x.EndSample, y.EndSample},
			{"first_frame", x.FirstFrame, y.FirstFrame},
			{"last_frame", x.LastFrame, y.LastFrame},
			{"hit_frames", x.HitFrames, y.HitFrames},
			{"span_frames", x.SpanFrames, y.SpanFrames},
			{"gap_frames", x.GapFrames, y.GapFrames},
			{"mean_similarity", x.MeanSimilarity, y.MeanSimilarity},
			{"confidence", x.Confidence, y.Confidence},
		}
		for _, c := range checks {
			if c.x != c.y {
				return eventDiff(i, c.field, c.x, c.y)
			}
		}
	}
	return nil
}

func compareQuality(a, b *AnnotationResult) *DiffDetail {
	if a.Quality == nil || b.Quality == nil {
		if a.Quality != b.Quality {
			return &DiffDetail{Category: "quality", Field: "quality", Expected: a.Quality != nil, Actual: b.Quality != nil, Message: "quality report presence differs"}
		}
		return nil
	}
	x := a.Quality
	y := b.Quality
	checks := []struct {
		field string
		x, y  any
	}{
		{"tolerance_samples", x.ToleranceSamples, y.ToleranceSamples},
		{"truth_count", x.TruthCount, y.TruthCount},
		{"annotation_count", x.AnnotationCount, y.AnnotationCount},
		{"matched_count", x.MatchedCount, y.MatchedCount},
		{"category_correct_count", x.CategoryCorrectCount, y.CategoryCorrectCount},
		{"category_accuracy", x.CategoryAccuracy, y.CategoryAccuracy},
		{"passed_count", x.PassedCount, y.PassedCount},
		{"passed", x.Passed, y.Passed},
		{"mean_abs_start_error", x.MeanAbsStartError, y.MeanAbsStartError},
		{"mean_abs_end_error", x.MeanAbsEndError, y.MeanAbsEndError},
		{"max_abs_start_error", x.MaxAbsStartError, y.MaxAbsStartError},
		{"max_abs_end_error", x.MaxAbsEndError, y.MaxAbsEndError},
		{"confidence.count", x.Confidence.Count, y.Confidence.Count},
		{"confidence.min", x.Confidence.Min, y.Confidence.Min},
		{"confidence.max", x.Confidence.Max, y.Confidence.Max},
		{"confidence.mean", x.Confidence.Mean, y.Confidence.Mean},
		{"confidence.median", x.Confidence.Median, y.Confidence.Median},
	}
	for _, c := range checks {
		if c.x != c.y {
			return &DiffDetail{Category: "quality", Field: "quality." + c.field, Expected: c.x, Actual: c.y, Message: "quality statistic differs: " + c.field}
		}
	}
	if len(x.Comparisons) != len(y.Comparisons) {
		return &DiffDetail{Category: "quality", Field: "quality.comparisons.length", Expected: len(x.Comparisons), Actual: len(y.Comparisons), Message: "quality comparison count differs"}
	}
	for i := range x.Comparisons {
		xc := x.Comparisons[i]
		yc := y.Comparisons[i]
		fields := []struct {
			name string
			a, b any
		}{
			{"event_no", xc.EventNo, yc.EventNo},
			{"category_expected", xc.CategoryExpected, yc.CategoryExpected},
			{"category_actual", xc.CategoryActual, yc.CategoryActual},
			{"start_actual", xc.StartActual, yc.StartActual},
			{"end_actual", xc.EndActual, yc.EndActual},
			{"start_abs_error", xc.StartAbsError, yc.StartAbsError},
			{"end_abs_error", xc.EndAbsError, yc.EndAbsError},
			{"passed", xc.Passed, yc.Passed},
		}
		for _, f := range fields {
			if f.a != f.b {
				idx := i
				return &DiffDetail{
					Category: "quality", EventIndex: &idx,
					Field:    fmt.Sprintf("quality.comparisons[%d].%s", i, f.name),
					Expected: f.a, Actual: f.b,
					Message: fmt.Sprintf("quality event %d field %s differs", i, f.name),
				}
			}
		}
	}
	return nil
}
