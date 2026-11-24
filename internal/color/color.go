// Package color contains deterministic linear-light color math. No GPU,
// platform API or floating point reduction order is used.
package color

import (
	"math"

	"hdrproof/internal/model"
)

const (
	BT709 = iota
	DisplayP3
	BT2020
)

var (
	bt709 = model.Primaries{
		Red:   model.Chromaticity{X: 0.64, Y: 0.33},
		Green: model.Chromaticity{X: 0.30, Y: 0.60},
		Blue:  model.Chromaticity{X: 0.15, Y: 0.06},
		White: model.Chromaticity{X: 0.3127, Y: 0.3290},
	}
	p3 = model.Primaries{
		Red:   model.Chromaticity{X: 0.680, Y: 0.320},
		Green: model.Chromaticity{X: 0.265, Y: 0.690},
		Blue:  model.Chromaticity{X: 0.150, Y: 0.060},
		White: model.Chromaticity{X: 0.3127, Y: 0.3290},
	}
	bt2020 = model.Primaries{
		Red:   model.Chromaticity{X: 0.708, Y: 0.292},
		Green: model.Chromaticity{X: 0.170, Y: 0.797},
		Blue:  model.Chromaticity{X: 0.131, Y: 0.046},
		White: model.Chromaticity{X: 0.3127, Y: 0.3290},
	}
)

func StandardPrimaries(name string) model.Primaries {
	switch name {
	case "display-p3":
		return p3
	case "bt2020":
		return bt2020
	default:
		return bt709
	}
}

func TargetPrimaries() model.Primaries { return bt709 }

type mat3 [3][3]float64

func matMul(a, b mat3) mat3 {
	var out mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			out[i][j] = a[i][0]*b[0][j] + a[i][1]*b[1][j] + a[i][2]*b[2][j]
		}
	}
	return out
}

func matVec(m mat3, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

func inverse(m mat3) (mat3, bool) {
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	if math.Abs(det) < 1e-15 {
		return mat3{}, false
	}
	inv := 1 / det
	var out mat3
	out[0][0] = (m[1][1]*m[2][2] - m[1][2]*m[2][1]) * inv
	out[0][1] = (m[0][2]*m[2][1] - m[0][1]*m[2][2]) * inv
	out[0][2] = (m[0][1]*m[1][2] - m[0][2]*m[1][1]) * inv
	out[1][0] = (m[1][2]*m[2][0] - m[1][0]*m[2][2]) * inv
	out[1][1] = (m[0][0]*m[2][2] - m[0][2]*m[2][0]) * inv
	out[1][2] = (m[0][2]*m[1][0] - m[0][0]*m[1][2]) * inv
	out[2][0] = (m[1][0]*m[2][1] - m[1][1]*m[2][0]) * inv
	out[2][1] = (m[0][1]*m[2][0] - m[0][0]*m[2][1]) * inv
	out[2][2] = (m[0][0]*m[1][1] - m[0][1]*m[1][0]) * inv
	return out, true
}

func xyzFromPrimaries(p model.Primaries) mat3 {
	red := p.Red
	green := p.Green
	blue := p.Blue
	white := p.White
	toXYZ := mat3{
		{red.X / red.Y, green.X / green.Y, blue.X / blue.Y},
		{1, 1, 1},
		{(1 - red.X - red.Y) / red.Y, (1 - green.X - green.Y) / green.Y, (1 - blue.X - blue.Y) / blue.Y},
	}
	whiteXYZ := [3]float64{white.X / white.Y, 1, (1 - white.X - white.Y) / white.Y}
	scale, ok := inverse(toXYZ)
	if !ok {
		return mat3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	}
	s := matVec(scale, whiteXYZ)
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			toXYZ[i][j] *= s[j]
		}
	}
	return toXYZ
}

func PrimariesToTarget(p model.Primaries) mat3 {
	src := xyzFromPrimaries(p)
	dst := xyzFromPrimaries(bt709)
	invDst, ok := inverse(dst)
	if !ok {
		return mat3{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	}
	return matMul(invDst, src)
}

func TargetToPrimaries(p model.Primaries, target [3]float64) [3]float64 {
	m := PrimariesToTarget(p)
	inv, ok := inverse(m)
	if !ok {
		return target
	}
	return matVec(inv, target)
}

// ACES returns the Naka-Rushton style ACES fitted tone curve, clipped to [0,1].
func ACES(x float64) float64 {
	if x <= 0 {
		return 0
	}
	const a, b, c, d, e = 2.51, 0.03, 2.43, 0.59, 0.14
	y := (x * (a*x + b)) / (x*(c*x+d) + e)
	return math.Max(0, math.Min(1, y))
}

func Reinhard(x float64) float64 {
	if x <= 0 {
		return 0
	}
	return x / (1 + x)
}

// InverseACES numerically inverts ACES with a fixed bisection count. The fixed
// iteration count and interval make the result deterministic across machines.
func InverseACES(y float64) float64 {
	y = math.Max(0, math.Min(1, y))
	if y == 0 {
		return 0
	}
	if y == 1 {
		return 32
	}
	lo, hi := 0.0, 32.0
	for i := 0; i < 80; i++ {
		mid := (lo + hi) * 0.5
		if ACES(mid) < y {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) * 0.5
}

func InverseReinhard(y float64) float64 {
	y = math.Max(0, math.Min(0.999999, y))
	return y / (1 - y)
}

// Desaturate moves a target-linear RGB value toward its luminance until every
// component is in [0,1].
func Desaturate(rgb [3]float64) [3]float64 {
	if rgb[0] >= 0 && rgb[0] <= 1 && rgb[1] >= 0 && rgb[1] <= 1 && rgb[2] >= 0 && rgb[2] <= 1 {
		return rgb
	}
	y := 0.2126*rgb[0] + 0.7152*rgb[1] + 0.0722*rgb[2]
	y = math.Max(0, math.Min(1, y))
	lo, hi := 0.0, 1.0
	for i := 0; i < 32; i++ {
		t := (lo + hi) * 0.5
		v := [3]float64{
			y + (rgb[0]-y)*t,
			y + (rgb[1]-y)*t,
			y + (rgb[2]-y)*t,
		}
		if v[0] >= 0 && v[0] <= 1 && v[1] >= 0 && v[1] <= 1 && v[2] >= 0 && v[2] <= 1 {
			lo = t
		} else {
			hi = t
		}
	}
	t := lo
	return [3]float64{
		y + (rgb[0]-y)*t,
		y + (rgb[1]-y)*t,
		y + (rgb[2]-y)*t,
	}
}

func EncodeSRGBByte(v float64) byte {
	v = math.Max(0, math.Min(1, v))
	var s float64
	if v <= 0.0031308 {
		s = 12.92 * v
	} else {
		s = 1.055*math.Pow(v, 1/2.4) - 0.055
	}
	return byte(math.Round(math.Max(0, math.Min(1, s)) * 255))
}

func DecodeSRGBByte(v byte) float64 {
	s := float64(v) / 255
	if s <= 0.04045 {
		return s / 12.92
	}
	return math.Pow((s+0.055)/1.055, 2.4)
}

func TransferForward(name string, x float64) float64 {
	switch name {
	case "reinhard":
		return Reinhard(x)
	default:
		return ACES(x)
	}
}

func TransferInverse(name string, y float64) float64 {
	switch name {
	case "reinhard":
		return InverseReinhard(y)
	default:
		return InverseACES(y)
	}
}
