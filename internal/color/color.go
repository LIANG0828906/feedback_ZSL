package color

import (
	"math"

	"ms-workbench/internal/scene"
)

// Matrices are in row-major order and are intentionally fixed so generated
// truth, live rendering, and batch rendering share one exact pipeline.
var bradford = [9]float64{
	0.8951, 0.2664, -0.1614,
	-0.7502, 1.7135, 0.0367,
	0.0389, -0.0685, 1.0296,
}

var bradfordInv = [9]float64{
	0.9869929, -0.1470543, 0.1599627,
	0.4323053, 0.5183603, 0.0492912,
	-0.0085287, 0.0400428, 0.9684867,
}

var xyzToSRGB = [9]float64{
	3.2404542, -1.5371385, -0.4985314,
	-0.9692660, 1.8760108, 0.0415560,
	0.0556434, -0.2040259, 1.0572252,
}

func mul3(m [9]float64, v scene.Vec3) scene.Vec3 {
	return scene.Vec3{
		m[0]*v[0] + m[1]*v[1] + m[2]*v[2],
		m[3]*v[0] + m[4]*v[1] + m[5]*v[2],
		m[6]*v[0] + m[7]*v[1] + m[8]*v[2],
	}
}

func mul33(a, b [9]float64) [9]float64 {
	var out [9]float64
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			out[r*3+c] = a[r*3]*b[c] + a[r*3+1]*b[3+c] + a[r*3+2]*b[6+c]
		}
	}
	return out
}

// BradfordAdapt converts XYZ from srcWhite to dstWhite.
func BradfordAdapt(xyz, srcWhite, dstWhite scene.Vec3) scene.Vec3 {
	if srcWhite == dstWhite {
		return xyz
	}
	srcLMS := mul3(bradford, srcWhite)
	dstLMS := mul3(bradford, dstWhite)
	scale := [9]float64{
		dstLMS[0] / srcLMS[0], 0, 0,
		0, dstLMS[1] / srcLMS[1], 0,
		0, 0, dstLMS[2] / srcLMS[2],
	}
	return mul3(mul33(bradfordInv, scale), mul3(bradford, xyz))
}

// ReconstructXYZ integrates band radiance against the declared band response.
func ReconstructXYZ(radiance []float64, responses []scene.Vec3) scene.Vec3 {
	var out scene.Vec3
	for i, r := range radiance {
		if i >= len(responses) {
			break
		}
		out[0] += r * responses[i][0]
		out[1] += r * responses[i][1]
		out[2] += r * responses[i][2]
	}
	return out
}

// Target describes the display-referred output transform.
type Target struct {
	Name       string
	WhitePoint scene.Vec3
	Matrix     [9]float64
}

func SRGB() Target {
	return Target{
		Name:       "sRGB",
		WhitePoint: scene.Vec3{0.95047, 1.0, 1.08883},
		Matrix:     xyzToSRGB,
	}
}

// MapIntent applies a declared gamut mapping intent. Every supported intent
// compresses rather than clips out-of-gamut colors.
func MapIntent(raw scene.Vec3, intent string) scene.Vec3 {
	if InGamut(raw) {
		return raw
	}
	switch intent {
	case "perceptual":
		return compressHighlight(raw)
	case "saturation":
		return compressChroma(raw)
	case "", "relative-colorimetric":
		return compressRelative(raw)
	default:
		return compressRelative(raw)
	}
}

func InGamut(v scene.Vec3) bool {
	const eps = 1e-9
	for _, c := range v {
		if c < -eps || c > 1+eps {
			return false
		}
	}
	return true
}

func luminance(v scene.Vec3) float64 {
	return 0.2126*v[0] + 0.7152*v[1] + 0.0722*v[2]
}

func clampUnit(v scene.Vec3) scene.Vec3 {
	for i, c := range v {
		if c < 0 {
			v[i] = 0
		} else if c > 1 {
			v[i] = 1
		}
	}
	return v
}

func compressRelative(raw scene.Vec3) scene.Vec3 {
	gray := luminance(raw)
	if gray > 1 {
		// Luminance above display white cannot be recovered by desaturation
		// alone. Apply a perceptual scale compression before chromatic
		// compression, never a hard per-channel crop.
		raw = raw.Scale(1 / gray)
		gray = 1
	} else if gray < 0 {
		raw = scene.Vec3{}
		gray = 0
	}
	if gray < 0 {
		gray = 0
	} else if gray > 1 {
		gray = 1
	}
	neutral := scene.Vec3{gray, gray, gray}
	lo, hi := 0.0, 1.0
	best := neutral
	for i := 0; i < 40; i++ {
		a := (lo + hi) * 0.5
		candidate := neutral.Add(raw.Sub(neutral).Scale(a))
		if InGamut(candidate) {
			best = candidate
			lo = a
		} else {
			hi = a
		}
	}
	return clampUnit(best)
}

func compressHighlight(raw scene.Vec3) scene.Vec3 {
	m := math.Max(raw[0], math.Max(raw[1], raw[2]))
	if m > 1 {
		raw = raw.Scale(1 / m)
	}
	if raw[0] < 0 || raw[1] < 0 || raw[2] < 0 {
		return compressRelative(raw)
	}
	return clampUnit(raw)
}

func compressChroma(raw scene.Vec3) scene.Vec3 {
	out := compressRelative(raw)
	gray := luminance(out)
	if gray < 0 || gray > 1 {
		return out
	}
	neutral := scene.Vec3{gray, gray, gray}
	// Preserve relative chroma as far as possible after hue-safe compression.
	lo, hi := 0.0, 1.0
	best := neutral
	for i := 0; i < 32; i++ {
		a := (lo + hi) * 0.5
		candidate := neutral.Add(raw.Sub(neutral).Scale(a))
		if InGamut(candidate) {
			best = candidate
			lo = a
		} else {
			hi = a
		}
	}
	return clampUnit(best)
}

// ReconstructLinearRGB performs integration, white-point adaptation, and
// target gamut mapping. The raw RGB value is returned for observability.
func ReconstructLinearRGB(radiance []float64, responses []scene.Vec3, sceneWhite scene.Vec3, intent string) (mapped, raw scene.Vec3, outOfGamut bool) {
	xyz := ReconstructXYZ(radiance, responses)
	target := SRGB()
	xyz = BradfordAdapt(xyz, sceneWhite, target.WhitePoint)
	raw = mul3(target.Matrix, xyz)
	outOfGamut = !InGamut(raw)
	return MapIntent(raw, intent), raw, outOfGamut
}
