package physics

import (
	"math"

	"volumekernel/internal/model"
)

func Phase(kind model.PhaseType, g, cosTheta float64) float64 {
	switch kind {
	case model.PhaseIsotropic:
		return 1 / (4 * math.Pi)
	case model.PhaseHG:
		if g > 0.999 {
			g = 0.999
		}
		if g < -0.999 {
			g = -0.999
		}
		den := 1 + g*g - 2*g*cosTheta
		if den < 1e-12 {
			den = 1e-12
		}
		return (1 - g*g) / (4 * math.Pi * math.Pow(den, 1.5))
	default:
		return 0
	}
}

func CosTheta(viewDir, lightDir model.Vec3) float64 {
	c := viewDir.Normalize().Dot(lightDir.Normalize())
	if c < -1 {
		return -1
	}
	if c > 1 {
		return 1
	}
	return c
}

func ShadowReference(light model.DirectionalLight) float64 {
	meanProfile := 1 + light.ShadowProfileGain/3
	return math.Exp(-light.ShadowTau * meanProfile)
}

func EstimateShadow(light model.DirectionalLight, samples int) float64 {
	if samples <= 0 {
		return 1
	}
	sum := 0.0
	for i := 0; i < samples; i++ {
		u := (float64(i) + 0.5) / float64(samples)
		sum += 1 + light.ShadowProfileGain*u*u
	}
	return math.Exp(-light.ShadowTau * sum / float64(samples))
}
