package audiofp

import (
	"fmt"
	"math"
	"sort"
)

type EventComparison struct {
	EventNo             int     `json:"event_no"`
	ManifestID          string  `json:"manifest_id,omitempty"`
	AnnotationID        string  `json:"annotation_id,omitempty"`
	Matched             bool    `json:"matched"`
	CategoryExpected    string  `json:"category_expected,omitempty"`
	CategoryActual      string  `json:"category_actual,omitempty"`
	CategoryMatch       bool    `json:"category_match"`
	StartExpected       int     `json:"start_expected"`
	StartActual         int     `json:"start_actual,omitempty"`
	StartDelta          int     `json:"start_delta,omitempty"`
	StartAbsError       int     `json:"start_abs_error,omitempty"`
	EndExpected         int     `json:"end_expected"`
	EndActual           int     `json:"end_actual,omitempty"`
	EndDelta            int     `json:"end_delta,omitempty"`
	EndAbsError         int     `json:"end_abs_error,omitempty"`
	ToleranceSamples    int     `json:"tolerance_samples"`
	WithinTolerance     bool    `json:"within_tolerance"`
	Passed              bool    `json:"passed"`
	BoundaryAggregation bool    `json:"boundary_aggregation"`
	BoundaryReason      string  `json:"boundary_reason,omitempty"`
	Confidence          float64 `json:"confidence,omitempty"`
}

type QualityMismatch struct {
	EventNo      int    `json:"event_no,omitempty"`
	AnnotationNo int    `json:"annotation_no,omitempty"`
	AnnotationID string `json:"annotation_id,omitempty"`
	Kind         string `json:"kind"`
	Field        string `json:"field"`
	Expected     any    `json:"expected"`
	Actual       any    `json:"actual"`
}

type ConfidenceDistribution struct {
	Count   int     `json:"count"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Mean    float64 `json:"mean"`
	Median  float64 `json:"median"`
	Buckets []int   `json:"buckets"`
}

type QualityReport struct {
	ToleranceSamples     int                    `json:"tolerance_samples"`
	QuantumStep          int                    `json:"quantum_step"`
	TruthCount           int                    `json:"truth_count"`
	AnnotationCount      int                    `json:"annotation_count"`
	MatchedCount         int                    `json:"matched_count"`
	CategoryCorrectCount int                    `json:"category_correct_count"`
	CategoryAccuracy     float64                `json:"category_accuracy"`
	PassedCount          int                    `json:"passed_count"`
	Passed               bool                   `json:"passed"`
	MeanAbsStartError    float64                `json:"mean_abs_start_error"`
	MeanAbsEndError      float64                `json:"mean_abs_end_error"`
	MaxAbsStartError     int                    `json:"max_abs_start_error"`
	MaxAbsEndError       int                    `json:"max_abs_end_error"`
	Comparisons          []EventComparison      `json:"comparisons"`
	Extra                []EventComparison      `json:"extra_annotations"`
	BoundaryDeviations   []EventComparison      `json:"boundary_deviations"`
	Mismatches           []QualityMismatch      `json:"mismatches"`
	Confidence           ConfidenceDistribution `json:"confidence_distribution"`
}

// EvaluateQuality performs one-to-one assignment and tolerance checking.
func EvaluateQuality(manifest Manifest, intervals []Interval, params Params, toleranceSamples int) (*QualityReport, error) {
	if toleranceSamples < 0 {
		return nil, newError(CodeRange, "tolerance_samples", "tolerance must be non-negative")
	}
	truth := append([]Event(nil), manifest.Events...)
	sort.SliceStable(truth, func(i, j int) bool { return truth[i].StartSample < truth[j].StartSample })

	quantum := params.QuantumStep()
	if quantum <= 0 {
		quantum = 1
	}
	report := &QualityReport{
		ToleranceSamples:   toleranceSamples,
		QuantumStep:        quantum,
		TruthCount:         len(truth),
		AnnotationCount:    len(intervals),
		Comparisons:        []EventComparison{},
		Extra:              []EventComparison{},
		BoundaryDeviations: []EventComparison{},
		Mismatches:         []QualityMismatch{},
	}
	report.Confidence = confidenceDistribution(intervals)

	assignment := assignEvents(truth, intervals)
	assignedAnnotations := make(map[int]bool)
	var startErrors, endErrors []int

	for i, ev := range truth {
		no := ev.Index
		if no <= 0 {
			no = i + 1
		}
		j := assignment[i]
		if j < 0 || j >= len(intervals) {
			cmp := EventComparison{
				EventNo: no, ManifestID: ev.ID, Matched: false,
				CategoryExpected: ev.Category,
				StartExpected:    ev.StartSample, EndExpected: ev.EndSample,
				ToleranceSamples: toleranceSamples,
			}
			report.Comparisons = append(report.Comparisons, cmp)
			report.Mismatches = append(report.Mismatches, QualityMismatch{
				EventNo: no, Kind: "missing", Field: "interval",
				Expected: [4]any{ev.Category, ev.StartSample, ev.EndSample, ev.Tone.FrequencyHz},
				Actual:   nil,
			})
			continue
		}
		assignedAnnotations[j] = true
		interval := intervals[j]
		startDelta := interval.StartSample - ev.StartSample
		endDelta := interval.EndSample - ev.EndSample
		startAbs := absInt(startDelta)
		endAbs := absInt(endDelta)
		categoryMatch := interval.Category == ev.Category
		within := startAbs <= toleranceSamples && endAbs <= toleranceSamples
		passed := categoryMatch && within
		boundary := startDelta != 0 || endDelta != 0
		cmp := EventComparison{
			EventNo: no, ManifestID: ev.ID, AnnotationID: interval.ID, Matched: true,
			CategoryExpected: ev.Category, CategoryActual: interval.Category,
			CategoryMatch: categoryMatch,
			StartExpected: ev.StartSample, StartActual: interval.StartSample,
			StartDelta: startDelta, StartAbsError: startAbs,
			EndExpected: ev.EndSample, EndActual: interval.EndSample,
			EndDelta: endDelta, EndAbsError: endAbs,
			ToleranceSamples: toleranceSamples, WithinTolerance: within, Passed: passed,
			BoundaryAggregation: boundary,
			Confidence:          interval.Confidence,
		}
		if boundary {
			cmp.BoundaryReason = fmt.Sprintf("predicted endpoint is quantized to %d-sample frame grid", quantum)
			report.BoundaryDeviations = append(report.BoundaryDeviations, cmp)
		}
		report.Comparisons = append(report.Comparisons, cmp)
		report.MatchedCount++
		if categoryMatch {
			report.CategoryCorrectCount++
		} else {
			report.Mismatches = append(report.Mismatches, QualityMismatch{
				EventNo: no, AnnotationID: interval.ID, Kind: "category_mismatch", Field: "category",
				Expected: ev.Category, Actual: interval.Category,
			})
		}
		if startAbs > toleranceSamples {
			report.Mismatches = append(report.Mismatches, QualityMismatch{
				EventNo: no, AnnotationID: interval.ID, Kind: "start_out_of_tolerance", Field: "start_sample",
				Expected: fmt.Sprintf("|actual-%d|<=%d", ev.StartSample, toleranceSamples),
				Actual:   interval.StartSample,
			})
		}
		if endAbs > toleranceSamples {
			report.Mismatches = append(report.Mismatches, QualityMismatch{
				EventNo: no, AnnotationID: interval.ID, Kind: "end_out_of_tolerance", Field: "end_sample",
				Expected: fmt.Sprintf("|actual-%d|<=%d", ev.EndSample, toleranceSamples),
				Actual:   interval.EndSample,
			})
		}
		if passed {
			report.PassedCount++
		}
		startErrors = append(startErrors, startAbs)
		endErrors = append(endErrors, endAbs)
		report.MaxAbsStartError = maxInt(report.MaxAbsStartError, startAbs)
		report.MaxAbsEndError = maxInt(report.MaxAbsEndError, endAbs)
	}

	for j, interval := range intervals {
		if assignedAnnotations[j] {
			continue
		}
		extra := EventComparison{
			AnnotationID: interval.ID, Matched: false,
			CategoryActual: interval.Category, StartActual: interval.StartSample, EndActual: interval.EndSample,
			Confidence: interval.Confidence, ToleranceSamples: toleranceSamples,
		}
		report.Extra = append(report.Extra, extra)
		report.Mismatches = append(report.Mismatches, QualityMismatch{
			AnnotationNo: j + 1, AnnotationID: interval.ID, Kind: "extra_annotation", Field: "interval",
			Expected: nil, Actual: [3]any{interval.Category, interval.StartSample, interval.EndSample},
		})
	}

	if report.MatchedCount > 0 {
		report.CategoryAccuracy = math.Round(float64(report.CategoryCorrectCount)/float64(report.MatchedCount)*1e6) / 1e6
		report.MeanAbsStartError = roundFloat(meanInt(startErrors), 3)
		report.MeanAbsEndError = roundFloat(meanInt(endErrors), 3)
	}
	report.Passed = len(report.Mismatches) == 0 && report.PassedCount == report.TruthCount
	if !report.Passed {
		return report, qualityError(report)
	}
	return report, nil
}

func qualityError(report *QualityReport) *Error {
	e := &Error{
		Code: CodeQual, Message: fmt.Sprintf("quality check failed: %d mismatch(es), passed=%d/%d",
			len(report.Mismatches), report.PassedCount, report.TruthCount),
		Expected: map[string]any{"tolerance_samples": report.ToleranceSamples, "all_events_passed": true},
		Actual:   map[string]any{"passed_events": report.PassedCount, "truth_events": report.TruthCount},
		Details:  report.Mismatches,
	}
	if len(report.Mismatches) > 0 && report.Mismatches[0].EventNo > 0 {
		e.EventNo = report.Mismatches[0].EventNo
		e.EventIndex = e.EventNo - 1
		e.Field = report.Mismatches[0].Field
	}
	return e
}

func assignEvents(truth []Event, intervals []Interval) []int {
	n := len(truth)
	m := len(intervals)
	result := make([]int, n)
	for i := range result {
		result[i] = -1
	}
	if n == 0 || m == 0 {
		return result
	}
	size := maxInt(n, m)
	const dummyCost int64 = 1 << 40
	cost := make([][]int64, size)
	for i := 0; i < size; i++ {
		cost[i] = make([]int64, size)
		for j := 0; j < size; j++ {
			realRow := i < n
			realCol := j < m
			switch {
			case realRow && realCol:
				ev := truth[i]
				in := intervals[j]
				c := int64(absInt(in.StartSample-ev.StartSample) + absInt(in.EndSample-ev.EndSample))
				if in.Category != ev.Category {
					c += 1
				}
				cost[i][j] = c
			case realRow || realCol:
				cost[i][j] = dummyCost
			default:
				cost[i][j] = 0
			}
		}
	}
	assignment := hungarian(cost)
	for i := 0; i < n; i++ {
		j := assignment[i]
		if j >= 0 && j < m {
			result[i] = j
		}
	}
	return result
}

// hungarian returns row->column minimum-cost assignment for a square matrix.
func hungarian(a [][]int64) []int {
	n := len(a)
	if n == 0 {
		return nil
	}
	const inf int64 = math.MaxInt64 / 4
	u := make([]int64, n+1)
	v := make([]int64, n+1)
	p := make([]int, n+1)
	way := make([]int, n+1)

	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		minv := make([]int64, n+1)
		for j := range minv {
			minv[j] = inf
		}
		used := make([]bool, n+1)
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := 0
			for j := 1; j <= n; j++ {
				if used[j] {
					continue
				}
				cur := a[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= n; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}

	assignment := make([]int, n)
	for i := range assignment {
		assignment[i] = -1
	}
	for j := 1; j <= n; j++ {
		if p[j] != 0 {
			assignment[p[j]-1] = j - 1
		}
	}
	return assignment
}

func confidenceDistribution(intervals []Interval) ConfidenceDistribution {
	d := ConfidenceDistribution{Buckets: make([]int, 10)}
	if len(intervals) == 0 {
		d.Min = 0
		d.Max = 0
		return d
	}
	values := make([]float64, 0, len(intervals))
	var sum float64
	d.Min = 1
	for _, in := range intervals {
		v := math.Max(0, math.Min(1, in.Confidence))
		values = append(values, v)
		sum += v
		d.Min = math.Min(d.Min, v)
		d.Max = math.Max(d.Max, v)
		bucket := int(v * 10)
		if bucket == 10 {
			bucket = 9
		}
		d.Buckets[bucket]++
	}
	sort.Float64s(values)
	d.Count = len(values)
	d.Mean = roundFloat(sum/float64(len(values)), 6)
	if len(values)%2 == 1 {
		d.Median = values[len(values)/2]
	} else {
		d.Median = (values[len(values)/2-1] + values[len(values)/2]) / 2
	}
	d.Median = roundFloat(d.Median, 6)
	d.Min = roundFloat(d.Min, 6)
	d.Max = roundFloat(d.Max, 6)
	return d
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func meanInt(values []int) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0
	for _, v := range values {
		sum += v
	}
	return float64(sum) / float64(len(values))
}

func roundFloat(v float64, digits int) float64 {
	pow := math.Pow10(digits)
	return math.Round(v*pow) / pow
}
