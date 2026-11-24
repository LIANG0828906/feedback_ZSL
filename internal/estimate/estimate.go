package estimate

import (
	"math"

	"terrain-occlusion/internal/model"
)

// Components derives every term from observed quantization parameters only.
func Components(q model.QuantParameters) (shadow, depth, projection, sampling float64) {
	r := float64(q.ShadowResolution)
	shadow = 1 / r
	depth = q.FixedPointStep
	projection = q.FixedPointStep * (1 + q.SampleSpacing)
	sampling = q.SampleSpacing / r
	return shadow, depth, projection, sampling
}

func BaseBound(q model.QuantParameters) float64 {
	s, d, p, sample := Components(q)
	return s + d + p + sample
}

// Report accumulates the conservative bound through the current camera sample.
func Report(q model.QuantParameters, sampleIndex, delayedImpact int) model.ErrorReport {
	shadow, depth, projection, sampling := Components(q)
	base := shadow + depth + projection + sampling
	cumulative := float64(sampleIndex+1) * base
	estimated := int(math.Ceil(cumulative*float64(q.ShadowResolution))) + delayedImpact
	report := model.ErrorReport{
		SampleIndex:            sampleIndex,
		ShadowQuantization:     shadow,
		DepthTruncation:        depth,
		ProjectionQuantization: projection,
		SamplingPropagation:    sampling,
		StreamingDelayImpact:   delayedImpact,
		CumulativeUpperBound:   cumulative,
		EstimatedSetDifference: estimated,
		Sufficient:             q.ShadowResolution > 0 && q.SampleSpacing > 0 && q.FixedPointStep > 0,
	}
	return report
}

// CheckSufficiency verifies that an observed set difference does not exceed
// the derived integer upper bound.
func CheckSufficiency(report model.ErrorReport, observedDifference int) error {
	if !report.Sufficient || report.EstimatedSetDifference < observedDifference {
		return &model.Error{
			Code:    model.EEst,
			Field:   "error_report.estimated_set_difference_upper_bound",
			Message: "error upper bound is insufficient at sample %d",
			Details: map[string]any{
				"sample_index": report.SampleIndex,
				"upper_bound":  report.EstimatedSetDifference,
				"observed":     observedDifference,
			},
		}
	}
	return nil
}
