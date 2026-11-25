package fog

import "math"

func EvalPhase(kind PhaseType, cosTheta, g float64) float64 {
	cosTheta = clamp(cosTheta, -1, 1)
	switch kind {
	case PhaseHG:
		g = clamp(g, -0.99, 0.99)
		den := 1 + g*g - 2*g*cosTheta
		if den < 1e-12 {
			den = 1e-12
		}
		return (1 - g*g) / (4 * math.Pi * math.Pow(den, 1.5))
	default:
		return 1 / (4 * math.Pi)
	}
}
