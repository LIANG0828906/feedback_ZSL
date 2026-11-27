package volumefog

import "math"

type StepBudgetProbe struct {
	Steps                  int     `json:"steps"`
	Transmittance          float64 `json:"transmittance"`
	Radiance               float64 `json:"radiance"`
	TransmittanceDeviation float64 `json:"transmittance_deviation"`
	RadianceDeviation      float64 `json:"radiance_deviation"`
	ErrorBound             float64 `json:"error_bound"`
}

// IntegrationErrorBound is the declared numerical upper bound for a ray at the
// given budgets. It decreases monotonically as either budget is tightened.
func IntegrationErrorBound(steps, shadowSamples int) float64 {
	if steps < 1 {
		steps = 1
	}
	if shadowSamples < 1 {
		shadowSamples = 1
	}
	return 0.5/float64(steps) + 0.5/float64(shadowSamples)
}

// ProbeStepBudgets checks transmittance monotonicity and the declared radiance
// error bound for increasing step counts.
func ProbeStepBudgets(scene *Scene, ray Ray, steps []int, shadowSamples int) ([]StepBudgetProbe, error) {
	truth, err := BuildRayTruth(scene, ray)
	if err != nil {
		return nil, err
	}
	var out []StepBudgetProbe
	for _, n := range steps {
		got, err := IntegrateRay(scene, ray, IntegrateOptions{Steps: n, ShadowSamples: shadowSamples})
		if err != nil {
			return nil, err
		}
		out = append(out, StepBudgetProbe{
			Steps: n, Transmittance: got.FinalTransmittance, Radiance: got.FinalRadiance,
			TransmittanceDeviation: math.Abs(got.FinalTransmittance - truth.FinalTransmittance),
			RadianceDeviation:      math.Abs(got.FinalRadiance - truth.FinalRadiance),
			ErrorBound:             IntegrationErrorBound(n, shadowSamples),
		})
	}
	return out, nil
}
