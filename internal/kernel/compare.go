package kernel

import (
	"sort"

	"inputkernel/internal/fixed"
	"inputkernel/internal/model"
)

type CompareResult struct {
	Equal              bool        `json:"equal"`
	FirstDiff          *model.Diff `json:"first_diff,omitempty"`
	ActionsCompared    int         `json:"actions_compared"`
	AxisCompared       int         `json:"axis_samples_compared"`
	IncrementalActions int         `json:"incremental_actions"`
	FullActions        int         `json:"full_actions"`
}

func CompareRuntimes(incremental, full *Runtime) CompareResult {
	incOutputs := incremental.ActionOutputs()
	fullOutputs := full.ActionOutputs()
	incSamples := incremental.AxisSamples()
	fullSamples := full.AxisSamples()
	result := CompareResult{
		ActionsCompared: len(incOutputs), AxisCompared: len(incSamples),
		IncrementalActions: len(incOutputs), FullActions: len(fullOutputs),
	}
	limit := len(incOutputs)
	if len(fullOutputs) < limit {
		limit = len(fullOutputs)
	}
	for i := 0; i < limit; i++ {
		if diff := diffAction(incOutputs[i], fullOutputs[i]); diff != nil {
			result.FirstDiff = diff
			result.Equal = false
			return result
		}
	}
	if len(incOutputs) != len(fullOutputs) {
		seq := 0
		if limit < len(incOutputs) {
			seq = incOutputs[limit].EventSeq
		} else if limit < len(fullOutputs) {
			seq = fullOutputs[limit].EventSeq
		}
		result.FirstDiff = &model.Diff{
			EventSeq: seq, Field: "action_count", Stream: "actions",
			Expected: len(fullOutputs), Actual: len(incOutputs),
		}
		return result
	}
	sampleLimit := len(incSamples)
	if len(fullSamples) < sampleLimit {
		sampleLimit = len(fullSamples)
	}
	for i := 0; i < sampleLimit; i++ {
		if diff := diffAxisSample(incSamples[i], fullSamples[i]); diff != nil {
			result.FirstDiff = diff
			return result
		}
	}
	if len(incSamples) != len(fullSamples) {
		seq := 0
		if sampleLimit < len(incSamples) {
			seq = incSamples[sampleLimit].EventSeq
		} else if sampleLimit < len(fullSamples) {
			seq = fullSamples[sampleLimit].EventSeq
		}
		result.FirstDiff = &model.Diff{
			EventSeq: seq, Field: "axis_sample_count", Stream: "axis_samples",
			Expected: len(fullSamples), Actual: len(incSamples),
		}
		return result
	}
	result.Equal = true
	return result
}

func CompareRuntimesChecked(incremental, full *Runtime) (CompareResult, error) {
	result := CompareRuntimes(incremental, full)
	if !result.Equal {
		if result.FirstDiff == nil {
			result.FirstDiff = &model.Diff{EventSeq: 0, Field: "unknown", Stream: "actions"}
		}
		return result, model.DiffError(result.FirstDiff)
	}
	return result, nil
}

func diffAction(actual, expected ActionOutput) *model.Diff {
	if actual.EventSeq != expected.EventSeq {
		return &model.Diff{EventSeq: minSeq(actual.EventSeq, expected.EventSeq), Field: "event_seq", Stream: "actions", Expected: expected.EventSeq, Actual: actual.EventSeq}
	}
	if actual.BindingID != expected.BindingID {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "binding_id", Stream: "actions", Expected: expected.BindingID, Actual: actual.BindingID}
	}
	if actual.TriggerTimeMS != expected.TriggerTimeMS {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "trigger_time_ms", Stream: "actions", Expected: expected.TriggerTimeMS, Actual: actual.TriggerTimeMS}
	}
	if actual.State != expected.State {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "state", Stream: "actions", Expected: expected.State, Actual: actual.State}
	}
	if !equalInt64Ptr(actual.AxisValue, expected.AxisValue) {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "axis_value", Stream: "actions", Expected: ptrValue(expected.AxisValue), Actual: ptrValue(actual.AxisValue)}
	}
	if !equalFractionPtr(actual.Residual, expected.Residual) {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "quantization_residual", Stream: "actions", Expected: ptrFraction(expected.Residual), Actual: ptrFraction(actual.Residual)}
	}
	return nil
}

func diffAxisSample(actual, expected AxisSample) *model.Diff {
	if actual.EventSeq != expected.EventSeq {
		return &model.Diff{EventSeq: minSeq(actual.EventSeq, expected.EventSeq), Field: "event_seq", Stream: "axis_samples", Expected: expected.EventSeq, Actual: actual.EventSeq}
	}
	if actual.BindingID != expected.BindingID {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "binding_id", Stream: "axis_samples", Expected: expected.BindingID, Actual: actual.BindingID}
	}
	if actual.Normalized != expected.Normalized {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "axis_normalized", Stream: "axis_samples", Expected: expected.Normalized, Actual: actual.Normalized}
	}
	if actual.Residual != expected.Residual {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "quantization_residual", Stream: "axis_samples", Expected: expected.Residual, Actual: actual.Residual}
	}
	if actual.DeadzoneInside != expected.DeadzoneInside {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "deadzone_inside", Stream: "axis_samples", Expected: expected.DeadzoneInside, Actual: actual.DeadzoneInside}
	}
	if actual.CurveError != expected.CurveError {
		return &model.Diff{EventSeq: actual.EventSeq, Field: "curve_linearization_error", Stream: "axis_samples", Expected: expected.CurveError, Actual: actual.CurveError}
	}
	return nil
}

type TruthTolerance struct {
	AxisFixed int64 `json:"axis_fixed"`
	TimeMS    int64 `json:"time_ms"`
}

func DefaultTruthTolerance() TruthTolerance {
	return TruthTolerance{AxisFixed: 1, TimeMS: 0}
}

type TruthMismatch struct {
	EventSeq int         `json:"event_seq"`
	Stream   string      `json:"stream"`
	Field    string      `json:"field"`
	Expected interface{} `json:"expected"`
	Actual   interface{} `json:"actual"`
}

type TruthResult struct {
	Pass            bool            `json:"pass"`
	ActionsChecked  int             `json:"actions_checked"`
	AxisChecked     int             `json:"axis_samples_checked"`
	UnmappedChecked int             `json:"unmapped_checked"`
	Mismatches      []TruthMismatch `json:"mismatches,omitempty"`
	FirstMismatch   *TruthMismatch  `json:"first_mismatch,omitempty"`
	MaxAxisDelta    int64           `json:"max_axis_delta_fixed"`
}

func (rt *Runtime) CompareTruth(tolerance TruthTolerance) TruthResult {
	s := rt.Scenario()
	outputs := rt.ActionOutputs()
	samples := rt.AxisSamples()
	unmapped := rt.UnmappedInputs()
	result := TruthResult{Pass: true}

	usedOutputs := make([]bool, len(outputs))
	for _, expected := range s.Truth.Actions {
		found := -1
		for i, actual := range outputs {
			if usedOutputs[i] {
				continue
			}
			if actual.EventSeq == expected.EventSeq && actual.BindingID == expected.BindingID &&
				actual.State == expected.State && abs64(actual.TriggerTimeMS-expected.TriggerTimeMS) <= tolerance.TimeMS {
				if expected.AxisValue != nil && actual.AxisValue != nil && abs64(*actual.AxisValue-*expected.AxisValue) > tolerance.AxisFixed {
					continue
				}
				if (expected.AxisValue == nil) != (actual.AxisValue == nil) {
					continue
				}
				found = i
				break
			}
		}
		result.ActionsChecked++
		if found < 0 {
			result.addMismatch(TruthMismatch{
				EventSeq: expected.EventSeq, Stream: "actions", Field: "missing_action",
				Expected: expected, Actual: nil,
			})
			continue
		}
		usedOutputs[found] = true
		actual := outputs[found]
		if actual.ActionID != expected.ActionID {
			result.addMismatch(TruthMismatch{
				EventSeq: expected.EventSeq, Stream: "actions", Field: "action_id",
				Expected: expected.ActionID, Actual: actual.ActionID,
			})
		}
		if expected.AxisValue != nil && actual.AxisValue != nil {
			delta := abs64(*actual.AxisValue - *expected.AxisValue)
			if delta > result.MaxAxisDelta {
				result.MaxAxisDelta = delta
			}
			if delta > tolerance.AxisFixed {
				result.addMismatch(TruthMismatch{
					EventSeq: expected.EventSeq, Stream: "actions", Field: "axis_value",
					Expected: *expected.AxisValue, Actual: *actual.AxisValue,
				})
			}
		}
	}
	for i, actual := range outputs {
		if !usedOutputs[i] {
			result.addMismatch(TruthMismatch{
				EventSeq: actual.EventSeq, Stream: "actions", Field: "unexpected_action",
				Expected: nil, Actual: actual,
			})
		}
	}

	usedSamples := make([]bool, len(samples))
	for _, expected := range s.Truth.AxisSamples {
		found := -1
		for i, actual := range samples {
			if usedSamples[i] {
				continue
			}
			if actual.EventSeq == expected.EventSeq && actual.BindingID == expected.BindingID {
				found = i
				break
			}
		}
		result.AxisChecked++
		if found < 0 {
			result.addMismatch(TruthMismatch{
				EventSeq: expected.EventSeq, Stream: "axis_samples", Field: "missing_axis_sample",
				Expected: expected, Actual: nil,
			})
			continue
		}
		usedSamples[found] = true
		actual := samples[found]
		delta := abs64(actual.Normalized - expected.Normalized)
		if delta > result.MaxAxisDelta {
			result.MaxAxisDelta = delta
		}
		if delta > tolerance.AxisFixed {
			result.addMismatch(TruthMismatch{
				EventSeq: expected.EventSeq, Stream: "axis_samples", Field: "axis_normalized",
				Expected: expected.Normalized, Actual: actual.Normalized,
			})
		}
		if actual.DeadzoneInside != expected.DeadzoneInside {
			result.addMismatch(TruthMismatch{
				EventSeq: expected.EventSeq, Stream: "axis_samples", Field: "deadzone_inside",
				Expected: expected.DeadzoneInside, Actual: actual.DeadzoneInside,
			})
		}
		if !fractionClose(actual.Residual, expected.Residual) {
			result.addMismatch(TruthMismatch{
				EventSeq: expected.EventSeq, Stream: "axis_samples", Field: "quantization_residual",
				Expected: expected.Residual, Actual: actual.Residual,
			})
		}
	}
	for i, actual := range samples {
		if !usedSamples[i] {
			result.addMismatch(TruthMismatch{
				EventSeq: actual.EventSeq, Stream: "axis_samples", Field: "unexpected_axis_sample",
				Expected: nil, Actual: actual,
			})
		}
	}

	result.UnmappedChecked = len(s.Truth.Unmapped)
	if len(unmapped) != len(s.Truth.Unmapped) {
		seq := 0
		if len(unmapped) > 0 {
			seq = unmapped[0].EventSeq
		} else if len(s.Truth.Unmapped) > 0 {
			seq = s.Truth.Unmapped[0].EventSeq
		}
		result.addMismatch(TruthMismatch{
			EventSeq: seq, Stream: "unmapped", Field: "unmapped_count",
			Expected: len(s.Truth.Unmapped), Actual: len(unmapped),
		})
	} else {
		for i := range unmapped {
			if unmapped[i] != s.Truth.Unmapped[i] {
				result.addMismatch(TruthMismatch{
					EventSeq: unmapped[i].EventSeq, Stream: "unmapped", Field: "unmapped_input",
					Expected: s.Truth.Unmapped[i], Actual: unmapped[i],
				})
			}
		}
	}
	if len(result.Mismatches) == 0 {
		result.Pass = true
	} else {
		result.Pass = false
		sort.SliceStable(result.Mismatches, func(i, j int) bool {
			if result.Mismatches[i].EventSeq != result.Mismatches[j].EventSeq {
				return result.Mismatches[i].EventSeq < result.Mismatches[j].EventSeq
			}
			return result.Mismatches[i].Field < result.Mismatches[j].Field
		})
		first := result.Mismatches[0]
		result.FirstMismatch = &first
	}
	return result
}

func (r *TruthResult) addMismatch(m TruthMismatch) {
	r.Mismatches = append(r.Mismatches, m)
	r.Pass = false
}

func fractionClose(a, b model.Fraction) bool {
	if a.Den == 0 || b.Den == 0 {
		return a == b
	}
	diff := fixed.FractionAbs(model.Fraction{
		Num: a.Num*b.Den - b.Num*a.Den,
		Den: a.Den * b.Den,
	})
	// Residuals only need to agree well below one fixed-point quantum.
	return diff.Num <= diff.Den/1000000
}

func equalInt64Ptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalFractionPtr(a, b *model.Fraction) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ptrValue(v *int64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func ptrFraction(v *model.Fraction) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func minSeq(a, b int) int {
	if a < b {
		return a
	}
	return b
}
