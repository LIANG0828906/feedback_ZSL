// Package terrain deterministically derives height, slope and noise from the
// project seed and absolute pixel coordinates. No floating-point state is used.
package terrain

import (
	"terrainkernel/internal/fixed"
)

const (
	piOverTwoURad  int64 = 1_570_796
	pixelSpacingMM int64 = 250
)

func TriWave(v, period, amplitude int64) int64 {
	if period <= 0 {
		return 0
	}
	half := period / 2
	r := v % period
	if r < 0 {
		r += period
	}
	if r <= half {
		return divNearest(amplitude*2*r, half) - amplitude
	}
	return amplitude - divNearest(amplitude*2*(r-half), half)
}

func divNearest(a, b int64) int64 {
	if b == 0 {
		return 0
	}
	if a >= 0 {
		return (a + b/2) / b
	}
	return -((-a + b/2) / b)
}

// HeightMM derives a deterministic height. The fixed constants and wave
// periods deliberately produce varied local gradients at 250 mm pixel pitch.
func HeightMM(seed int64, px, py int) int64 {
	first := TriWave(int64(px)+seed, 16, 20_000)
	second := TriWave(7*seed+int64(py)+3, 12, 10_000)
	h := seed*0x4f9ffb7 ^ int64(px)*0x466f45d ^ int64(py)*0x127409f
	r := h % 17
	if r < 0 {
		r = -r
	}
	return 39_040 + first + second + r*120
}

func SlopeURad(seed int64, px, py int) int64 {
	hHere := HeightMM(seed, px, py)
	hRight := HeightMM(seed, px+1, py)
	hDown := HeightMM(seed, px, py+1)
	dx := hRight - hHere
	dy := hDown - hHere
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	sum := dx*dx + dy*dy
	rise := isqrt(sum)
	ratio, err := fixed.DivScale(rise, pixelSpacingMM, "slope")
	if err != nil {
		return 0
	}
	return AtanURad(ratio)
}

// AtanURad computes atan(x/1e6) in microradians using the kernel's fixed
// polynomial. The inverse and final division use nearest rounding.
func AtanURad(xFixed int64) int64 {
	if xFixed <= 0 {
		return 0
	}
	if xFixed > fixed.Scale {
		inv := divNearest(fixed.Scale*fixed.Scale, xFixed)
		if inv <= 0 {
			return piOverTwoURad
		}
		return piOverTwoURad - atanUnit(inv)
	}
	return atanUnit(xFixed)
}

func atanUnit(xFixed int64) int64 {
	x2 := xFixed * xFixed / fixed.Scale
	bx2 := 271_560 * x2 / fixed.Scale
	numExpr := 999_250 + bx2
	num := xFixed * numExpr
	cx2 := 1_146_420 * x2 / fixed.Scale
	den := fixed.Scale + cx2
	return divNearest(num, den)
}

func isqrt(v int64) int64 {
	if v <= 0 {
		return 0
	}
	bits := 0
	for n := v; n > 0; n >>= 1 {
		bits++
	}
	x := int64(1) << (bits / 2)
	for i := 0; i < 64; i++ {
		next := (x + v/x) / 2
		if x > next {
			x = next
			continue
		}
		return x
	}
	return x
}

// NoiseOffset produces a deterministic value-noise offset in weight units.
func NoiseOffset(seed, amplitude, scaleMM int64, px, py int) int64 {
	if scaleMM <= 0 || amplitude == 0 {
		return 0
	}
	wx := int64(px) * pixelSpacingMM
	wy := int64(py) * pixelSpacingMM
	gx := wx / scaleMM
	gy := wy / scaleMM
	fx := wx % scaleMM
	fy := wy % scaleMM
	lx := scaleMM - fx
	ly := scaleMM - fy

	h00 := hash2(seed, gx, gy)
	h10 := hash2(seed, gx+1, gy)
	h01 := hash2(seed, gx, gy+1)
	h11 := hash2(seed, gx+1, gy+1)

	top := h00*lx/scaleMM + h10*fx/scaleMM
	bot := h01*lx/scaleMM + h11*fx/scaleMM
	interp := top*ly/scaleMM + bot*fy/scaleMM
	centered := interp - 500_000
	return centered * (2 * amplitude) / 1_000_000
}

func hash2(seed, gx, gy int64) int64 {
	h := seed*0x165667b1 + gx*0x27d4eb2f + gy*0x7fffffff
	h ^= h >> 13
	h *= 0x4bf19f61
	h ^= h >> 16
	if h < 0 {
		h = -h
	}
	return h % 1_000_001
}
