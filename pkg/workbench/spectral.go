package workbench

import (
	"fmt"
	"math"
)

const (
	spectralMin  = 380.0
	spectralMax  = 780.0
	spectralStep = 5.0
)

type spectralBasis struct {
	wavelengths []float64
	illuminant  []float64
	responses   [][]float64
	cmf         [][3]float64
	bandXYZ     [][3]float64
}

func measurementGeometry() []float64 {
	n := int(math.Round((spectralMax-spectralMin)/spectralStep)) + 1
	out := make([]float64, n)
	for i := range out {
		out[i] = spectralMin + float64(i)*spectralStep
	}
	return out
}

// cie1931 uses the Wyman-Sloan-Shirley multi-lobe analytic approximation of
// the CIE 1931 2-degree observer. It is deterministic and avoids external data.
func cie1931(lambda float64) [3]float64 {
	g := func(x, mu, sigmaLow, sigmaHigh float64) float64 {
		sigma := sigmaHigh
		if x < mu {
			sigma = sigmaLow
		}
		t := (x - mu) / sigma
		return math.Exp(-0.5 * t * t)
	}
	x := 1.056*g(lambda, 599.8, 37.9, 31.0) +
		0.362*g(lambda, 442.0, 16.0, 26.7) -
		0.065*g(lambda, 501.1, 20.4, 26.2)
	y := 0.821*g(lambda, 568.8, 46.9, 40.5) +
		0.286*g(lambda, 530.9, 16.3, 31.1)
	z := 1.217*g(lambda, 437.0, 11.8, 36.0) +
		0.681*g(lambda, 459.0, 26.0, 13.8)
	return [3]float64{x, y, z}
}

func planckSpectrum(lambda, temperature float64) float64 {
	const (
		h = 6.62607015e-34
		c = 2.99792458e8
		k = 1.380649e-23
	)
	lm := lambda * 1e-9
	return 1.0 / (math.Pow(lm, 5) * (math.Exp(h*c/(lm*k*temperature)) - 1))
}

func buildSpectralBasis(bandMeta []BandMeta) spectralBasis {
	wavelengths := measurementGeometry()
	ill := make([]float64, len(wavelengths))
	rawIll := make([]float64, len(wavelengths))
	for i, wl := range wavelengths {
		rawIll[i] = planckSpectrum(wl, 6504)
	}
	// Normalize the illuminant at 560 nm so integration uses radiance-like units.
	ref := planckSpectrum(560, 6504)
	for i := range rawIll {
		ill[i] = rawIll[i] / ref
	}
	cmf := make([][3]float64, len(wavelengths))
	var yNorm float64
	for i, wl := range wavelengths {
		cmf[i] = cie1931(wl)
		yNorm += ill[i] * cmf[i][1]
	}
	if yNorm <= 0 {
		yNorm = 1
	}
	responses := make([][]float64, len(bandMeta))
	for b, meta := range bandMeta {
		fwhm := meta.FWHM
		if fwhm <= 0 {
			fwhm = 35
		}
		sigma := fwhm / (2 * math.Sqrt(2*math.Log(2)))
		responses[b] = make([]float64, len(wavelengths))
		for i, wl := range wavelengths {
			t := (wl - meta.CenterWavelength) / sigma
			responses[b][i] = math.Exp(-0.5 * t * t)
		}
	}
	bandXYZ := make([][3]float64, len(bandMeta))
	for i := range wavelengths {
		var sumResp float64
		for b := range responses {
			sumResp += responses[b][i]
		}
		if sumResp <= 1e-12 {
			continue
		}
		scale := ill[i] / (yNorm * sumResp)
		for b := range responses {
			r := responses[b][i] * scale
			bandXYZ[b][0] += r * cmf[i][0]
			bandXYZ[b][1] += r * cmf[i][1]
			bandXYZ[b][2] += r * cmf[i][2]
		}
	}
	return spectralBasis{
		wavelengths: wavelengths,
		illuminant:  ill,
		responses:   responses,
		cmf:         cmf,
		bandXYZ:     bandXYZ,
	}
}

type mat3 [3][3]float64

var srgbToXYZ = mat3{
	{0.4124564, 0.3575761, 0.1804375},
	{0.2126729, 0.7151522, 0.0721750},
	{0.0193339, 0.1191920, 0.9503041},
}

var xyzToSRGB = mat3{
	{3.2404542, -1.5371385, -0.4985314},
	{-0.9692660, 1.8760108, 0.0415560},
	{0.0556434, -0.2040259, 1.0572252},
}

var displayP3ToXYZ = mat3{
	{0.4865709, 0.2656677, 0.1982173},
	{0.2289746, 0.6917385, 0.0792869},
	{0.0000000, 0.0451134, 1.0439444},
}

var xyzToDisplayP3 = mat3{
	{2.4934969, -0.9313836, -0.4027108},
	{-0.8294890, 1.7626641, 0.0236247},
	{0.0358458, -0.0761724, 0.9568845},
}

var rec2020ToXYZ = mat3{
	{0.6369580, 0.1446169, 0.1688810},
	{0.2627002, 0.6779981, 0.0593017},
	{0.0000000, 0.0280727, 1.0609851},
}

var xyzToRec2020 = mat3{
	{1.7166512, -0.3556708, -0.2533663},
	{-0.6666844, 1.6164812, 0.0157685},
	{0.0176399, -0.0427706, 0.9421031},
}

var bradford = mat3{
	{0.8951, 0.2664, -0.1614},
	{-0.7502, 1.7135, 0.0367},
	{0.0389, -0.0685, 1.0296},
}

var bradfordInv = mat3{
	{0.9869929, -0.1470543, 0.1599627},
	{0.4323053, 0.5183603, 0.0492912},
	{-0.0085287, 0.0400428, 0.9684867},
}

func mulMatVec(m mat3, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

func mulMatMat(a, b mat3) mat3 {
	var out mat3
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			for k := 0; k < 3; k++ {
				out[r][c] += a[r][k] * b[k][c]
			}
		}
	}
	return out
}

func invertMat(m mat3) (mat3, bool) {
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	if math.Abs(det) < 1e-12 {
		return mat3{}, false
	}
	invDet := 1 / det
	var out mat3
	out[0][0] = (m[1][1]*m[2][2] - m[1][2]*m[2][1]) * invDet
	out[0][1] = (m[0][2]*m[2][1] - m[0][1]*m[2][2]) * invDet
	out[0][2] = (m[0][1]*m[1][2] - m[0][2]*m[1][1]) * invDet
	out[1][0] = (m[1][2]*m[2][0] - m[1][0]*m[2][2]) * invDet
	out[1][1] = (m[0][0]*m[2][2] - m[0][2]*m[2][0]) * invDet
	out[1][2] = (m[0][2]*m[1][0] - m[0][0]*m[1][2]) * invDet
	out[2][0] = (m[1][0]*m[2][1] - m[1][1]*m[2][0]) * invDet
	out[2][1] = (m[0][1]*m[2][0] - m[0][0]*m[2][1]) * invDet
	out[2][2] = (m[0][0]*m[1][1] - m[0][1]*m[1][0]) * invDet
	return out, true
}

func gamutMatrices(name string) (toXYZ, fromXYZ mat3, ok bool) {
	switch normalizeGamutName(name) {
	case "sRGB":
		return srgbToXYZ, xyzToSRGB, true
	case "DisplayP3":
		return displayP3ToXYZ, xyzToDisplayP3, true
	case "Rec2020":
		return rec2020ToXYZ, xyzToRec2020, true
	default:
		return mat3{}, mat3{}, false
	}
}

func adaptXYZ(xyz [3]float64, src, dst WhitePoint) [3]float64 {
	if math.Abs(src.X-dst.X) < 1e-9 &&
		math.Abs(src.Y-dst.Y) < 1e-9 &&
		math.Abs(src.Z-dst.Z) < 1e-9 {
		return xyz
	}
	srcCone := mulMatVec(bradford, [3]float64{src.X, src.Y, src.Z})
	dstCone := mulMatVec(bradford, [3]float64{dst.X, dst.Y, dst.Z})
	out := mulMatVec(bradford, xyz)
	for i := 0; i < 3; i++ {
		if math.Abs(srcCone[i]) < 1e-12 {
			continue
		}
		out[i] *= dstCone[i] / srcCone[i]
	}
	return mulMatVec(bradfordInv, out)
}

func whitePointFromXY(x, y float64) (WhitePoint, error) {
	if !finite(x) || !finite(y) || x <= 0 || y <= 0 {
		return WhitePoint{}, fmt.Errorf("invalid white chromaticity")
	}
	return WhitePoint{Name: "custom", X: x / y, Y: 1, Z: (1 - x - y) / y}, nil
}

func xyzToXY(xyz [3]float64) XY {
	sum := xyz[0] + xyz[1] + xyz[2]
	if math.Abs(sum) < 1e-12 {
		return XY{}
	}
	return XY{X: xyz[0] / sum, Y: xyz[1] / sum}
}

func gamutBoundary(name string) []XY {
	var primaries mat3
	switch normalizeGamutName(name) {
	case "DisplayP3":
		primaries = displayP3ToXYZ
	case "Rec2020":
		primaries = rec2020ToXYZ
	default:
		primaries = srgbToXYZ
	}
	points := make([]XY, 3)
	for i := 0; i < 3; i++ {
		points[i] = xyzToXY([3]float64{primaries[0][i], primaries[1][i], primaries[2][i]})
	}
	return points
}

// gamutMap preserves hue direction and compresses out-of-range code values
// toward the gamut boundary. It never per-channel clips, so an out-of-gamut
// color remains observable as a boundary color rather than a clipped triplet.
func gamutMap(rgb [3]float64) ([3]float64, bool, int) {
	badChannel := -1
	inGamut := true
	for i, v := range rgb {
		if !finite(v) {
			return rgb, false, i
		}
		if v < 0 || v > 1 {
			inGamut = false
			if badChannel < 0 {
				badChannel = i
			}
		}
	}
	if inGamut {
		return rgb, true, -1
	}
	lum := 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	if !finite(lum) {
		return rgb, false, badChannel
	}
	gray := softUnit(lum)
	lo, hi := 0.0, 1.0
	var best [3]float64
	for i := 0; i < 32; i++ {
		mid := (lo + hi) / 2
		candidate := [3]float64{
			gray + mid*(rgb[0]-gray),
			gray + mid*(rgb[1]-gray),
			gray + mid*(rgb[2]-gray),
		}
		valid := true
		for _, v := range candidate {
			if !finite(v) || v < -1e-9 || v > 1+1e-9 {
				valid = false
				break
			}
		}
		if valid {
			lo = mid
			best = candidate
		} else {
			hi = mid
		}
	}
	for i := range best {
		if !finite(best[i]) {
			return rgb, false, i
		}
		best[i] = clamp(best[i], 0, 1)
	}
	return best, true, -1
}

// softUnit maps non-negative values into [0,1) with a smooth shoulder. Values
// already inside the unit interval are unchanged.
func softUnit(v float64) float64 {
	if !finite(v) {
		return 0
	}
	if v <= 1 {
		return math.Max(0, v)
	}
	return 1 - math.Exp(-(v-1))/(1+v-1)
}

func encodeDisplay(v float64, exposure float64) uint8 {
	x := math.Max(0, v*exposure)
	mapped := x / (1 + x)
	encoded := math.Pow(mapped, 1/2.2)
	return uint8(math.Round(clamp(encoded, 0, 1) * 255))
}
