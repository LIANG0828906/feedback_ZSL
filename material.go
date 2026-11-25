package fog

import (
	"math"
	"sort"
)

const ShadowRayLength = 12.0

func PhaseFunction(kind string, g, cosTheta float64) float64 {
	switch kind {
	case PhaseIsotropic:
		return 1.0 / (4.0 * math.Pi)
	case PhaseHenyeyGreenstein:
		denom := 1 + g*g - 2*g*cosTheta
		if denom <= 1e-12 {
			denom = 1e-12
		}
		return (1 - g*g) / (4 * math.Pi * math.Pow(denom, 1.5))
	default:
		return 1.0 / (4.0 * math.Pi)
	}
}

func DensityAt(medium *Medium, p Vec3) float64 {
	d := medium.Density
	min := medium.Bounds.Min.Float64()
	size := d.VoxelSize.Float64()
	ix := int(math.Floor((p.X - min.X) / size.X))
	iy := int(math.Floor((p.Y - min.Y) / size.Y))
	iz := int(math.Floor((p.Z - min.Z) / size.Z))
	if ix < 0 {
		ix = 0
	}
	if iy < 0 {
		iy = 0
	}
	if iz < 0 {
		iz = 0
	}
	if ix >= d.Resolution[0] {
		ix = d.Resolution[0] - 1
	}
	if iy >= d.Resolution[1] {
		iy = d.Resolution[1] - 1
	}
	if iz >= d.Resolution[2] {
		iz = d.Resolution[2] - 1
	}
	return float64(d.Values[d.Index(ix, iy, iz)]) * medium.DensityScale
}

type MaterialSample struct {
	SigmaT       float64
	SigmaS       float64
	PhaseScatter float64
	Ambient      float64
}

func SampleMaterials(scene *Scene, ray Ray, t float64, mediumIDs []string) MaterialSample {
	p := ray.Origin.Add(ray.Dir.Scale(t))
	sample := MaterialSample{}
	lightDir := scene.Light.Direction.Normalize()
	cosTheta := ray.Dir.Normalize().Dot(lightDir)
	for _, id := range mediumIDs {
		medium := scene.Medium(id)
		if medium == nil || !medium.Bounds.Contains(p) {
			continue
		}
		density := DensityAt(medium, p)
		sigmaA := medium.Absorption.Float64() * density
		sigmaS := medium.Scattering.Float64() * density
		sample.SigmaT += sigmaA + sigmaS
		sample.SigmaS += sigmaS
		phase := PhaseFunction(scene.Recipe.PhaseFunction, medium.Anisotropy, cosTheta)
		sample.PhaseScatter += sigmaS * phase
		sample.Ambient += sigmaS * 0.32
	}
	return sample
}

func ShadowTransmittanceExact(scene *Scene, origin Vec3) float64 {
	dir := scene.Light.Direction.Normalize()
	ray := Ray{
		ID:     "shadow-exact",
		Origin: origin,
		Dir:    dir,
		Near:   0,
		Far:    FixedFromFloat(ShadowRayLength),
	}
	tau := 0.0
	for _, medium := range scene.Media {
		near, far, ok := SlabIntersection(ray, medium.Bounds)
		if !ok {
			continue
		}
		tau += exactOpticalDepth(ray, medium, near, far)
	}
	return math.Exp(-tau)
}

func exactOpticalDepth(ray Ray, medium *Medium, near, far float64) float64 {
	boundaries := voxelBoundaries(ray, medium, near, far)
	tau := 0.0
	for i := 0; i+1 < len(boundaries); i++ {
		a, b := boundaries[i], boundaries[i+1]
		if b <= a {
			continue
		}
		mid := (a + b) / 2
		p := ray.Origin.Add(ray.Dir.Scale(mid))
		density := DensityAt(medium, p)
		sigma := (medium.Absorption.Float64() + medium.Scattering.Float64()) * density
		tau += sigma * (b - a) * ray.Dir.Len()
	}
	return tau
}

func voxelBoundaries(ray Ray, medium *Medium, near, far float64) []float64 {
	values := []float64{near, far}
	min := medium.Bounds.Min.Float64()
	size := medium.Density.VoxelSize.Float64()
	res := medium.Density.Resolution
	origin := [3]float64{ray.Origin.X, ray.Origin.Y, ray.Origin.Z}
	dir := [3]float64{ray.Dir.X, ray.Dir.Y, ray.Dir.Z}
	lo := [3]float64{min.X, min.Y, min.Z}
	sizes := [3]float64{size.X, size.Y, size.Z}
	counts := [3]int{res[0], res[1], res[2]}
	for axis := 0; axis < 3; axis++ {
		if math.Abs(dir[axis]) < 1e-12 {
			continue
		}
		for k := 0; k <= counts[axis]; k++ {
			plane := lo[axis] + float64(k)*sizes[axis]
			t := (plane - origin[axis]) / dir[axis]
			if t > near+1e-10 && t < far-1e-10 {
				values = append(values, t)
			}
		}
	}
	values = uniqueSortedFloat(values)
	return values
}

func uniqueSortedFloat(values []float64) []float64 {
	if len(values) == 0 {
		return values
	}
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j-1] > values[j]; j-- {
			values[j-1], values[j] = values[j], values[j-1]
		}
	}
	out := values[:0]
	for _, v := range values {
		if len(out) == 0 || math.Abs(v-out[len(out)-1]) > 1e-9 {
			out = append(out, v)
		}
	}
	return out
}

type ShadowBudgetResult struct {
	Transmittance float64
	Used          int
	Exhausted     bool
}

func ShadowTransmittanceBudget(scene *Scene, origin Vec3, budget int) ShadowBudgetResult {
	if budget <= 0 {
		return ShadowBudgetResult{Transmittance: 1, Used: 0, Exhausted: true}
	}
	dir := scene.Light.Direction.Normalize()
	ray := Ray{
		ID:     "shadow-budget",
		Origin: origin,
		Dir:    dir,
		Near:   0,
		Far:    FixedFromFloat(ShadowRayLength),
	}

	type interval struct {
		medium *Medium
		near   float64
		far    float64
	}
	intervals := make([]interval, 0, len(scene.Media))
	for _, medium := range scene.Media {
		near, far, ok := SlabIntersection(ray, medium.Bounds)
		if ok {
			intervals = append(intervals, interval{medium: medium, near: near, far: far})
		}
	}
	valid := make([]interval, 0, len(intervals))
	totalLength := 0.0
	for _, iv := range intervals {
		if iv.far > iv.near {
			valid = append(valid, iv)
			totalLength += iv.far - iv.near
		}
	}
	if len(valid) == 0 {
		return ShadowBudgetResult{Transmittance: 1, Used: 0}
	}

	counts := make([]int, len(valid))
	used := 0
	for i := range valid {
		if used < budget {
			counts[i] = 1
			used++
		}
	}
	type budgetRemainder struct {
		index int
		value float64
	}
	rem := make([]budgetRemainder, 0, len(valid))
	assigned := used
	for i, iv := range valid {
		raw := 0.0
		if totalLength > 0 {
			raw = float64(budget-len(valid)) * (iv.far - iv.near) / totalLength
		}
		extra := int(math.Floor(raw))
		counts[i] += extra
		assigned += extra
		rem = append(rem, budgetRemainder{i, raw - float64(extra)})
	}
	sort.Slice(rem, func(i, j int) bool { return rem[i].value > rem[j].value })
	for _, r := range rem {
		if assigned >= budget {
			break
		}
		counts[r.index]++
		assigned++
	}
	if assigned > budget {
		for i := len(counts) - 1; i >= 0 && assigned > budget; i-- {
			for counts[i] > 1 && assigned > budget {
				counts[i]--
				assigned--
			}
		}
	}

	tau := 0.0
	actualUsed := 0
	for i, iv := range valid {
		count := counts[i]
		if count <= 0 {
			continue
		}
		length := iv.far - iv.near
		for j := 0; j < count; j++ {
			t := iv.near + length*(float64(j)+0.5)/float64(count)
			p := ray.Origin.Add(ray.Dir.Scale(t))
			density := DensityAt(iv.medium, p)
			sigma := (iv.medium.Absorption.Float64() + iv.medium.Scattering.Float64()) * density
			tau += sigma * length / float64(count)
			actualUsed++
		}
	}
	return ShadowBudgetResult{Transmittance: math.Exp(-tau), Used: actualUsed, Exhausted: actualUsed >= budget}
}
