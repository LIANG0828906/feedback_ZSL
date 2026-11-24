package volume

import (
	"math"
	"sort"

	"volumediag/internal/model"
)

const LightSupportSigmas = 4.0

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func smoothstep01(v float64) float64 {
	v = clamp01(v)
	return v * v * (3 - 2*v)
}

// LightRadiance evaluates the deterministic emitter kernel used by both the
// workbench renderer and the independent truth generator. Point and area
// lights use a bounded Gaussian spatial kernel; spot lights additionally use a
// smooth annular cone term. No shadow visibility is applied here.
func LightRadiance(light *model.Light, p model.Vec3) model.Color {
	delta := p.Sub(light.Position)
	r2 := delta.Length2()
	sigma := light.Radius
	support := LightSupportSigmas * sigma
	if r2 > support*support {
		return model.Color{}
	}
	kernel := math.Exp(-r2 / (2 * sigma * sigma))
	factor := kernel
	dir, dirOK := light.Direction.Normalize()
	if dirOK {
		switch light.Type {
		case "spot":
			toP, ok := delta.Normalize()
			if !ok {
				factor = kernel
				break
			}
			cosTheta := toP.Dot(dir)
			cosInner := math.Cos(light.ConeInnerDeg * math.Pi / 180)
			cosOuter := math.Cos(light.ConeOuterDeg * math.Pi / 180)
			if cosTheta <= cosOuter {
				return model.Color{}
			}
			if cosTheta < cosInner && cosInner > cosOuter {
				factor *= smoothstep01((cosTheta - cosOuter) / (cosInner - cosOuter))
			}
		case "area":
			toP, ok := delta.Normalize()
			if ok {
				orientation := clamp01(toP.Dot(dir))
				factor *= 0.35 + 0.65*orientation
			}
		}
	}
	return light.Intensity.Mul(factor)
}

func HGPhase(cosTheta, g float64) float64 {
	if g >= model.MaxAnisotropy {
		g = model.MaxAnisotropy
	} else if g <= -model.MaxAnisotropy {
		g = -model.MaxAnisotropy
	}
	denom := 1 + g*g - 2*g*cosTheta
	if denom < 1e-12 {
		denom = 1e-12
	}
	return (1 - g*g) / (4 * math.Pi * math.Pow(denom, 1.5))
}

func PhaseMax(g float64) float64 {
	if g >= 0 {
		return (1 + g) / (4 * math.Pi * (1 - g))
	}
	return (1 - g) / (4 * math.Pi * (1 + g))
}

func SampleEmission(scene *model.Scene, light *model.Light, p, viewDir model.Vec3, scattering model.Color) model.Color {
	radiance := LightRadiance(light, p)
	if radiance.Max() <= 0 {
		return model.Color{}
	}
	if SceneOccluded(scene, light, p) {
		return model.Color{}
	}
	toLight := light.Position.Sub(p)
	dirToLight, ok := toLight.Normalize()
	if !ok {
		return model.Color{}
	}
	cosTheta := dirToLight.Dot(viewDir)
	// The per-light anisotropy is the primary artist control; the medium phase
	// parameter supplies a global baseline used by the diagnostic model.
	effectiveG := 0.75*light.Anisotropy + 0.25*scene.Medium.PhaseG
	phase := HGPhase(cosTheta, effectiveG)
	return model.Color{
		scattering[0] * radiance[0] * phase,
		scattering[1] * radiance[1] * phase,
		scattering[2] * radiance[2] * phase,
	}
}

func ExpTransmittance(sigmaT model.Color, distance float64) model.Color {
	if distance < 0 {
		distance = 0
	}
	return model.Color{
		math.Exp(-sigmaT[0] * distance),
		math.Exp(-sigmaT[1] * distance),
		math.Exp(-sigmaT[2] * distance),
	}
}

// LightCanInfluenceRay is used for block-level invalidation. It performs an
// analytic support-sphere test and, for spot lights, a cone intersection test.
// It is intentionally independent of render traversal order.
func LightCanInfluenceRay(light *model.Light, origin, dir model.Vec3, tMax float64) bool {
	if light.Intensity.Max() <= 0 {
		return false
	}
	dir, ok := dir.Normalize()
	if !ok {
		return false
	}
	radius := LightSupportSigmas * light.Radius
	if light.Type == "area" {
		radius += math.Hypot(light.HalfExtents[0], light.HalfExtents[1])
	}
	lo, hi, hit := raySphereInterval(origin, dir, light.Position, radius, tMax)
	if !hit {
		return false
	}
	if light.Type != "spot" {
		return true
	}
	axis, ok := light.Direction.Normalize()
	if !ok {
		return false
	}
	cosOuter := math.Cos(light.ConeOuterDeg * math.Pi / 180)
	w := origin.Sub(light.Position)
	a := dir.Dot(axis)
	b := w.Dot(axis)
	e := w.Dot(dir)
	f := w.Length2()
	c2 := cosOuter * cosOuter
	candidates := []float64{lo, hi, (lo + hi) / 2}
	qa := a*a - c2
	qb := 2*a*b - 2*c2*e
	qc := b*b - c2*f
	if math.Abs(qa) < 1e-12 {
		if math.Abs(qb) > 1e-12 {
			candidates = append(candidates, -qc/qb)
		}
	} else {
		disc := qb*qb - 4*qa*qc
		if disc >= 0 {
			sq := math.Sqrt(disc)
			candidates = append(candidates, (-qb-sq)/(2*qa), (-qb+sq)/(2*qa))
		}
	}
	if math.Abs(a) > 1e-12 {
		candidates = append(candidates, -b/a)
	}
	sort.Float64s(candidates)
	var filtered []float64
	for _, t := range candidates {
		if t >= lo && t <= hi {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		filtered = []float64{lo, hi, (lo + hi) / 2}
	}
	inSpotCone := func(t float64) bool {
		p := origin.Add(dir.Mul(t)).Sub(light.Position)
		dot := p.Dot(axis)
		if dot < 0 {
			return false
		}
		return dot*dot >= c2*p.Length2()-1e-9
	}
	for i := 0; i < len(filtered); i++ {
		if inSpotCone(filtered[i]) {
			return true
		}
		if i+1 < len(filtered) {
			if inSpotCone((filtered[i] + filtered[i+1]) / 2) {
				return true
			}
		}
	}
	return false
}

func raySphereInterval(origin, dir, center model.Vec3, radius, tMax float64) (float64, float64, bool) {
	oc := origin.Sub(center)
	b := oc.Dot(dir)
	c := oc.Length2() - radius*radius
	disc := b*b - c
	if disc < 0 {
		return 0, 0, false
	}
	sqrt := math.Sqrt(disc)
	lo := -b - sqrt
	hi := -b + sqrt
	if hi < 0 || lo > tMax {
		return 0, 0, false
	}
	if lo < 0 {
		lo = 0
	}
	if hi > tMax {
		hi = tMax
	}
	if hi < lo {
		return 0, 0, false
	}
	return lo, hi, true
}
