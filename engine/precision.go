package engine

import (
	"math"
	"sort"

	"performanceexpressions/workspace/domain"
)

type PrecisionPoint struct {
	SegmentID      string  `json:"segment_id"`
	Tick           int     `json:"tick"`
	Beat           float64 `json:"beat"`
	Target         string  `json:"target"`
	Expected       float64 `json:"expected"`
	Actual         float64 `json:"actual"`
	AbsError       float64 `json:"abs_error"`
	SourceExpected string  `json:"source_expected"`
	SourceActual   string  `json:"source_actual"`
	SourceMatch    bool    `json:"source_match"`
	WithinTol      bool    `json:"within_tolerance"`
}

type PrecisionReport struct {
	Tolerance        float64          `json:"tolerance"`
	TotalPoints      int              `json:"total_points"`
	PassedPoints     int              `json:"passed_points"`
	FailedPoints     int              `json:"failed_points"`
	SourceMismatches int              `json:"source_mismatches"`
	SourceMatchRate  float64          `json:"source_match_rate"`
	Points           []PrecisionPoint `json:"points"`
	OK               bool             `json:"ok"`
}

func ValidatePrecision(project domain.Project, result domain.ResultSet, tolerance float64) (PrecisionReport, error) {
	if tolerance < 0 || math.IsNaN(tolerance) || math.IsInf(tolerance, 0) {
		return PrecisionReport{}, domain.NewError(domain.CodePrecision, "tolerance must be non-negative and finite")
	}
	report := PrecisionReport{Tolerance: tolerance}
	resultSegments := make(map[string]domain.SegmentResult, len(result.Segments))
	for _, seg := range result.Segments {
		resultSegments[seg.SegmentID] = seg
	}
	for _, seg := range project.Segments {
		segResult, ok := resultSegments[seg.ID]
		if !ok {
			return report, domain.NewError(domain.CodePrecision, "missing evaluated segment").WithSegment(seg.ID)
		}
		samples := make(map[int]domain.SampleResult, len(segResult.Samples))
		for _, sample := range segResult.Samples {
			samples[sample.Tick] = sample
		}
		truth := append([]domain.TruthSample(nil), seg.Truth.Samples...)
		sort.Slice(truth, func(i, j int) bool { return truth[i].Tick < truth[j].Tick })
		for _, expected := range truth {
			sample, ok := samples[expected.Tick]
			if !ok {
				return report, domain.NewError(domain.CodePrecision, "evaluation is missing truth sample").
					WithSegment(seg.ID).WithTick(expected.Tick).WithBeat(expected.Beat)
			}
			targets := make([]string, 0, len(expected.Values))
			seen := make(map[string]bool)
			for target := range expected.Values {
				seen[target] = true
				targets = append(targets, target)
			}
			for target := range expected.Sources {
				if !seen[target] {
					targets = append(targets, target)
				}
			}
			sort.Strings(targets)
			for _, target := range targets {
				expValue := expected.Values[target]
				actValue, ok := sample.Values[target]
				if !ok {
					return report, domain.NewError(domain.CodePrecision, "evaluation is missing target value").
						WithSegment(seg.ID).WithTick(expected.Tick).WithBeat(expected.Beat)
				}
				errAbs := math.Abs(actValue - expValue)
				sourceExpected := expected.Sources[target]
				sourceActual := sample.Sources[target].Channel
				p := PrecisionPoint{
					SegmentID:      seg.ID,
					Tick:           expected.Tick,
					Beat:           expected.Beat,
					Target:         target,
					Expected:       expValue,
					Actual:         actValue,
					AbsError:       errAbs,
					SourceExpected: sourceExpected,
					SourceActual:   sourceActual,
					SourceMatch:    sourceExpected == "" || sourceExpected == sourceActual,
					WithinTol:      errAbs <= tolerance,
				}
				report.Points = append(report.Points, p)
				report.TotalPoints++
				if p.WithinTol {
					report.PassedPoints++
				} else {
					report.FailedPoints++
				}
				if !p.SourceMatch {
					report.SourceMismatches++
				}
			}
		}
	}
	report.OK = report.FailedPoints == 0 && report.SourceMismatches == 0
	if report.TotalPoints > 0 {
		report.SourceMatchRate = float64(report.TotalPoints-report.SourceMismatches) / float64(report.TotalPoints)
	} else {
		report.SourceMatchRate = 1
	}
	if report.FailedPoints > 0 {
		for _, p := range report.Points {
			if !p.WithinTol {
				return report, domain.NewError(domain.CodePrecision, "absolute deviation %.12g exceeds tolerance %.12g", p.AbsError, tolerance).
					WithSegment(p.SegmentID).WithTick(p.Tick).WithBeat(p.Beat)
			}
		}
	}
	if report.SourceMismatches > 0 {
		for _, p := range report.Points {
			if !p.SourceMatch {
				return report, domain.NewError(domain.CodePrecision, "source mismatch for target %s: expected %s got %s", p.Target, p.SourceExpected, p.SourceActual).
					WithSegment(p.SegmentID).WithTick(p.Tick).WithBeat(p.Beat)
			}
		}
	}
	return report, nil
}

type PointKey struct {
	SegmentID string
	Tick      int
	Target    string
}

func PassingPointSet(report PrecisionReport) map[PointKey]struct{} {
	set := make(map[PointKey]struct{})
	for _, p := range report.Points {
		if p.WithinTol && p.SourceMatch {
			set[PointKey{SegmentID: p.SegmentID, Tick: p.Tick, Target: p.Target}] = struct{}{}
		}
	}
	return set
}

func SetIsSubset(small, large map[PointKey]struct{}) bool {
	for key := range small {
		if _, ok := large[key]; !ok {
			return false
		}
	}
	return true
}
