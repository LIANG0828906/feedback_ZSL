package fog

import "math"

type RayBudgetError struct {
	StepBudget            int     `json:"step_budget"`
	ShadowBudget          int     `json:"shadow_budget"`
	MaxTransmittanceError float64 `json:"max_transmittance_error"`
	MaxScatteringError    float64 `json:"max_scattering_error"`
	MaxRadianceError      float64 `json:"max_radiance_error"`
	NormalizedDeviation   float64 `json:"normalized_deviation"`
}

type PrecisionReport struct {
	FullBudget      RayBudgetError   `json:"full_budget"`
	BudgetCurve     []RayBudgetError `json:"budget_curve"`
	Monotonic       bool             `json:"monotonic"`
	TolerancePassed bool             `json:"tolerance_passed"`
	Failure         *Error           `json:"failure,omitempty"`
}

func EvaluateRayBudget(scene *Scene, truth *TruthBundle, stepBudget, shadowBudget int) RayBudgetError {
	report := RayBudgetError{StepBudget: stepBudget, ShadowBudget: shadowBudget}
	for _, rayTruth := range truth.Rays {
		ray := RayForPixel(scene.ActiveSample(), scene.Viewport, rayTruth.Pixel.X, rayTruth.Pixel.Y)
		result, err := IntegrateRay(scene, ray, IntegrationOptions{StepBudget: stepBudget, ShadowBudget: shadowBudget})
		if err != nil {
			report.MaxTransmittanceError = math.Max(report.MaxTransmittanceError, 1)
			report.NormalizedDeviation = math.Max(report.NormalizedDeviation, 2)
			continue
		}
		report.MaxTransmittanceError = math.Max(report.MaxTransmittanceError, math.Abs(result.FinalTransmittance-rayTruth.FinalTransmittance))
		report.MaxScatteringError = math.Max(report.MaxScatteringError, math.Abs(result.Scattering-rayTruth.Scattering))
		for c := 0; c < 3; c++ {
			report.MaxRadianceError = math.Max(report.MaxRadianceError, math.Abs(result.Radiance[c]-rayTruth.Radiance[c]))
		}
		for i, segmentTruth := range rayTruth.Segments {
			if i >= len(result.Segments) {
				report.NormalizedDeviation = math.MaxFloat64
				continue
			}
			segment := result.Segments[i]
			tError := math.Abs(segment.Transmittance - segmentTruth.Transmittance)
			sError := math.Abs(segment.CumulativeScattering - segmentTruth.CumulativeScattering)
			report.MaxTransmittanceError = math.Max(report.MaxTransmittanceError, tError)
			report.MaxScatteringError = math.Max(report.MaxScatteringError, sError)
			normalized := math.Max(tError/scene.Tolerances.SegmentTransmittance, sError/scene.Tolerances.SegmentScattering)
			report.NormalizedDeviation = math.Max(report.NormalizedDeviation, normalized)
		}
		finalNormalized := math.Max(
			report.MaxTransmittanceError/scene.Tolerances.SegmentTransmittance,
			report.MaxScatteringError/scene.Tolerances.SegmentScattering,
		)
		report.NormalizedDeviation = math.Max(report.NormalizedDeviation, finalNormalized)
	}
	return report
}

func AssertRayTruthWithinTolerance(scene *Scene, rayTruth RayTruth, options IntegrationOptions) error {
	ray := RayForPixel(scene.ActiveSample(), scene.Viewport, rayTruth.Pixel.X, rayTruth.Pixel.Y)
	result, err := IntegrateRay(scene, ray, options)
	if err != nil {
		return err
	}
	deviation := 0.0
	for i, expected := range rayTruth.Segments {
		if i >= len(result.Segments) {
			e := newError(CodeIter, "ray produced %d segments, truth has %d", len(result.Segments), len(rayTruth.Segments))
			e.RayID = ray.ID
			e.Deviation = float64(len(rayTruth.Segments) - len(result.Segments))
			return e
		}
		actual := result.Segments[i]
		tError := math.Abs(actual.Transmittance - expected.Transmittance)
		sError := math.Abs(actual.CumulativeScattering - expected.CumulativeScattering)
		deviation = math.Max(deviation, math.Max(tError/scene.Tolerances.SegmentTransmittance, sError/scene.Tolerances.SegmentScattering))
		if tError > scene.Tolerances.SegmentTransmittance || sError > scene.Tolerances.SegmentScattering {
			e := newError(CodeIter, "segment %d exceeds truth tolerance", i)
			e.RayID = ray.ID
			e.Deviation = math.Max(tError, sError)
			e.Field = "segment"
			e.Value = map[string]any{
				"segment":              i,
				"transmittance_error":  tError,
				"scattering_error":     sError,
				"transmittance_limit":  scene.Tolerances.SegmentTransmittance,
				"scattering_limit":     scene.Tolerances.SegmentScattering,
				"normalized_deviation": deviation,
			}
			return e
		}
	}
	return nil
}

func VerifyPrecision(sample *GeneratedSample, budgets []int) *PrecisionReport {
	scene := &sample.Scene
	if len(budgets) == 0 {
		budgets = []int{8, 12, 18, 32, 48, scene.Recipe.MaxStepsPerRay}
	}
	report := &PrecisionReport{BudgetCurve: make([]RayBudgetError, 0, len(budgets))}
	for _, budget := range budgets {
		point := EvaluateRayBudget(scene, &sample.Truth, budget, scene.Recipe.ShadowBudgetPerRay)
		report.BudgetCurve = append(report.BudgetCurve, point)
	}
	if len(report.BudgetCurve) > 0 {
		report.FullBudget = report.BudgetCurve[len(report.BudgetCurve)-1]
		report.TolerancePassed = report.FullBudget.NormalizedDeviation <= 1.0
	}
	report.Monotonic = true
	previous := math.MaxFloat64
	for _, point := range report.BudgetCurve {
		if point.NormalizedDeviation > previous+1e-9 {
			report.Monotonic = false
		}
		previous = point.NormalizedDeviation
	}
	if !report.TolerancePassed {
		e := newError(CodeIter, "full budget still exceeds truth tolerance")
		e.Deviation = report.FullBudget.NormalizedDeviation
		report.Failure = e
	}
	return report
}
