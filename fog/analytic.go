package fog

import "math"

// AnalyticHomogeneous evaluates a single homogeneous, unshadowed medium
// segment. It is used to generate and independently verify the truth list.
func AnalyticHomogeneous(scene *Scene, m Medium, ray Ray, t0, t1 float64) (transmittance, scattered float64) {
	if len(m.Density) == 0 || t1 <= t0 {
		return 1, 0
	}
	density := float64(m.Density[0]) / float64(DensityMax)
	scale := float64(scene.Recipe.CoefficientScale)
	sigmaT := (float64(m.AbsorptionFP) + float64(m.ScatteringFP)) / scale * density
	sigmaS := float64(m.ScatteringFP) / scale * density
	cosTheta := ray.Direction.Dot(scene.Recipe.LightDirection.Normalize())
	source := sigmaS * EvalPhase(scene.Recipe.Phase, cosTheta, m.Anisotropy) * scene.Recipe.LightIntensity
	transmittance = math.Exp(-sigmaT * (t1 - t0))
	if sigmaT > 1e-15 {
		scattered = source / sigmaT * (1 - transmittance)
	} else {
		scattered = source * (t1 - t0)
	}
	return transmittance, scattered
}

func UniformDensity(m *Medium) (int32, bool) {
	if len(m.Density) == 0 {
		return 0, false
	}
	v := m.Density[0]
	for _, d := range m.Density[1:] {
		if d != v {
			return 0, false
		}
	}
	return v, true
}
