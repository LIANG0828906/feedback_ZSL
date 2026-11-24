package render

import "math"

// MaxParams bounds the differentiable parameter vector. The generated tasks use
// 11 parameters; the larger bound leaves room for modest multi-object scenes.
const MaxParams = 32

// ad is a small fixed-capacity forward-mode dual number. A fixed capacity keeps
// pixel evaluation allocation-free and deterministic.
type ad struct {
	v float64
	n uint8
	d [MaxParams]float64
}

func adConst(v float64, n int) ad {
	return ad{v: v, n: uint8(n)}
}

func adVar(v float64, idx, n int) ad {
	out := ad{v: v, n: uint8(n)}
	tape := out.d[:]
	tape[idx] = 1
	return out
}

func (a ad) param(i int) float64 { return a.d[i] }

func adAdd(a, b ad) ad {
	out := ad{v: a.v + b.v, n: max8(a.n, b.n)}
	for i := 0; i < int(out.n); i++ {
		out.d[i] = a.d[i] + b.d[i]
	}
	return out
}

func adSub(a, b ad) ad {
	out := ad{v: a.v - b.v, n: max8(a.n, b.n)}
	for i := 0; i < int(out.n); i++ {
		out.d[i] = a.d[i] - b.d[i]
	}
	return out
}

func adNeg(a ad) ad {
	out := ad{v: -a.v, n: a.n}
	for i := 0; i < int(a.n); i++ {
		out.d[i] = -a.d[i]
	}
	return out
}

func adMul(a, b ad) ad {
	out := ad{v: a.v * b.v, n: max8(a.n, b.n)}
	for i := 0; i < int(out.n); i++ {
		out.d[i] = a.d[i]*b.v + a.v*b.d[i]
	}
	return out
}

func adScale(a ad, s float64) ad {
	out := ad{v: a.v * s, n: a.n}
	for i := 0; i < int(a.n); i++ {
		out.d[i] = a.d[i] * s
	}
	return out
}

func adDiv(a, b ad) ad {
	out := ad{v: a.v / b.v, n: max8(a.n, b.n)}
	inv := 1 / b.v
	inv2 := inv * inv
	for i := 0; i < int(out.n); i++ {
		out.d[i] = a.d[i]*inv - a.v*b.d[i]*inv2
	}
	return out
}

func adSqrt(a ad) ad {
	if a.v <= 0 {
		return adConst(0, int(a.n))
	}
	out := ad{v: math.Sqrt(a.v), n: a.n}
	scale := 0.5 / out.v
	for i := 0; i < int(a.n); i++ {
		out.d[i] = a.d[i] * scale
	}
	return out
}

func adMax(a, b ad) ad {
	if a.v >= b.v {
		return a
	}
	return b
}

func adMin(a, b ad) ad {
	if a.v <= b.v {
		return a
	}
	return b
}

// adClamp has a zero derivative on the clamped plateaus.
func adClamp(a ad, lo, hi float64) ad {
	if a.v < lo {
		return adConst(lo, int(a.n))
	}
	if a.v > hi {
		return adConst(hi, int(a.n))
	}
	return a
}

func adSmoothstep(edge0, edge1 float64, x ad) ad {
	t := adClamp(adDiv(adSub(x, adConst(edge0, int(x.n))), adConst(edge1-edge0, int(x.n))), 0, 1)
	return adMul(adMul(t, t), adSub(adConst(3, int(t.n)), adScale(t, 2)))
}

func adPow(a ad, exponent float64) ad {
	if a.v <= 0 {
		return adConst(0, int(a.n))
	}
	v := math.Pow(a.v, exponent)
	factor := exponent * math.Pow(a.v, exponent-1)
	out := ad{v: v, n: a.n}
	for i := 0; i < int(a.n); i++ {
		out.d[i] = a.d[i] * factor
	}
	return out
}

type adVec struct {
	x, y, z ad
}

func adDot(a, b adVec) ad {
	return adAdd(adAdd(adMul(a.x, b.x), adMul(a.y, b.y)), adMul(a.z, b.z))
}

func adCross(a, b adVec) adVec {
	return adVec{
		x: adSub(adMul(a.y, b.z), adMul(a.z, b.y)),
		y: adSub(adMul(a.z, b.x), adMul(a.x, b.z)),
		z: adSub(adMul(a.x, b.y), adMul(a.y, b.x)),
	}
}

func adVecAdd(a, b adVec) adVec {
	return adVec{adAdd(a.x, b.x), adAdd(a.y, b.y), adAdd(a.z, b.z)}
}

func adVecSub(a, b adVec) adVec {
	return adVec{adSub(a.x, b.x), adSub(a.y, b.y), adSub(a.z, b.z)}
}

func adVecScale(a adVec, s float64) adVec {
	return adVec{adScale(a.x, s), adScale(a.y, s), adScale(a.z, s)}
}

func adVecMulVec(a, b adVec) adVec {
	return adVec{adMul(a.x, b.x), adMul(a.y, b.y), adMul(a.z, b.z)}
}

func adVecMulAd(a adVec, s ad) adVec {
	return adVec{adMul(a.x, s), adMul(a.y, s), adMul(a.z, s)}
}

func adLength(a adVec) ad { return adSqrt(adDot(a, a)) }

func adNormalize(a adVec) adVec {
	l := adLength(a)
	return adVec{
		adDiv(a.x, l),
		adDiv(a.y, l),
		adDiv(a.z, l),
	}
}

func max8(a, b uint8) uint8 {
	if a > b {
		return a
	}
	return b
}
