package bonerig

import (
	"math/bits"
)

// FQ is a work-scale fixed-point quaternion. Its implicit scale is PrecisionParams.WorkScale.
type FQ struct {
	W int64 `json:"w"`
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

// FVec is a work distance-scale fixed-point vector.
type FVec struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

// ComponentTrunc records one component normalization/division trace.
type ComponentTrunc struct {
	Component      string `json:"component"`
	ComponentIndex int    `json:"component_index"`
	InputNumerator int64  `json:"input_numerator"`
	Output         int64  `json:"output"`
	Remainder      int64  `json:"remainder"`
	Denominator    int64  `json:"denominator"`
	CutBitPosition int    `json:"cut_bit_position"`
	Truncated      bool   `json:"truncated"`
}

// QuatNormTrace is the per-frame normalization record required by requirement 3.
type QuatNormTrace struct {
	Joint          string           `json:"joint"`
	Frame          int              `json:"frame"`
	SourceScale    int64            `json:"source_scale"`
	WorkScale      int64            `json:"work_scale"`
	RawNorm        int64            `json:"raw_norm"`
	NormErrorUnits int64            `json:"norm_error_units"`
	AdjustmentL1   int64            `json:"adjustment_l1"`
	Overflow       bool             `json:"overflow"`
	Components     []ComponentTrunc `json:"components"`
}

func safeMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a == -1 && b == -9223372036854775808 {
		return 0, false
	}
	if b == -1 && a == -9223372036854775808 {
		return 0, false
	}
	if a > 0 {
		if b > 0 {
			if a > 9223372036854775807/b {
				return 0, false
			}
		} else if b < -9223372036854775808/a {
			return 0, false
		}
	} else {
		if b > 0 {
			if a < -9223372036854775808/b {
				return 0, false
			}
		} else {
			if a != -1 && b != -1 {
				aa := -a
				bb := -b
				if aa > 9223372036854775807/bb {
					return 0, false
				}
			}
		}
	}
	return a * b, true
}

func safeAdd(a, b int64) (int64, bool) {
	if (b > 0 && a > 9223372036854775807-b) || (b < 0 && a < -9223372036854775808-b) {
		return 0, false
	}
	return a + b, true
}

func isqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := int64(1) << uint((bits.Len64(uint64(n))+1)/2)
	for {
		nx := (x + n/x) / 2
		if nx >= x {
			return x
		}
		x = nx
	}
}

// roundDiv returns rounded quotient and signed remainder n-q*d.
func roundDiv(n, d int64) (int64, int64) {
	if d <= 0 {
		panic("roundDiv requires positive denominator")
	}
	var q int64
	if n >= 0 {
		q = (n + d/2) / d
	} else {
		q = -((-n + d/2) / d)
	}
	return q, n - q*d
}

func roundQ(n, d int64) int64 {
	q, _ := roundDiv(n, d)
	return q
}

func cutBitPosition(den int64) int {
	if den <= 1 {
		return 0
	}
	return bits.Len64(uint64(den - 1))
}

func quatSquares(q FQ) (int64, bool) {
	var sum int64
	for _, v := range []int64{q.W, q.X, q.Y, q.Z} {
		vv, ok := safeMul(v, v)
		if !ok {
			return 0, false
		}
		sum, ok = safeAdd(sum, vv)
		if !ok {
			return 0, false
		}
	}
	return sum, true
}

func quatNorm(q FQ) (int64, bool) {
	sq, ok := quatSquares(q)
	if !ok {
		return 0, false
	}
	return isqrt(sq), true
}

// NormalizeQuat validates and normalizes at the quaternion's declared scale,
// then exactly widens it to the common work scale.
func NormalizeQuat(q Quat, p PrecisionParams, joint string, frame int) (Quat, FQ, QuatNormTrace, *Error) {
	trace := QuatNormTrace{Joint: joint, Frame: frame, SourceScale: q.Scale, WorkScale: p.WorkScale}
	if q.Scale <= 0 || p.WorkScale <= 0 || p.WorkScale%q.Scale != 0 {
		e := errorf(CodeQuat, "illegal fixed-point scale")
		e.Field = "rotation.scale"
		e.OpA = q.Scale
		e.OpB = p.WorkScale
		return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
	}
	mult := p.WorkScale / q.Scale
	raw := [4]int64{q.W, q.X, q.Y, q.Z}
	names := [4]string{"W", "X", "Y", "Z"}
	components := make([]ComponentTrunc, 0, 4)

	for i, v := range raw {
		if v > p.MaxComponent || v < -p.MaxComponent {
			e := errorf(CodeQuat, "quaternion component overflow")
			e.Field = "rotation." + names[i]
			e.OpA = v
			e.OpB = p.MaxComponent
			return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
		}
	}

	var rawFQ FQ
	rawArr := [4]*int64{&rawFQ.W, &rawFQ.X, &rawFQ.Y, &rawFQ.Z}
	for i, v := range raw {
		mv, ok := safeMul(v, v)
		if !ok {
			e := errorf(CodeQuat, "quaternion component overflow")
			e.Field = "rotation." + names[i]
			e.OpA = v
			e.OpB = p.MaxComponent
			return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
		}
		_ = mv
	}
	norm, ok := quatSquares(FQ{W: raw[0], X: raw[1], Y: raw[2], Z: raw[3]})
	if !ok {
		e := errorf(CodeQuat, "quaternion norm overflow")
		e.OpA = q
		e.OpB = p.MaxComponent
		trace.Overflow = true
		return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
	}
	rawNorm := isqrt(norm)
	trace.RawNorm = rawNorm
	normErr := abs64(rawNorm - q.Scale)
	if normErr < 0 {
		normErr = -normErr
	}
	trace.NormErrorUnits = normErr
	declaredTol := p.NormToleranceUnits
	if q.Scale < p.WorkScale {
		declaredTol = (p.NormToleranceUnits*q.Scale + p.WorkScale - 1) / p.WorkScale
		if declaredTol < 2 {
			declaredTol = 2
		}
	}
	if rawNorm == 0 || normErr > declaredTol {
		e := errorf(CodeQuat, "quaternion modulus outside declared tolerance")
		e.Field = "rotation"
		e.OpA = rawNorm
		e.OpB = q.Scale
		e.Expected = map[string]int64{"target": q.Scale, "tolerance": declaredTol}
		return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
	}

	var adjustment int64
	out := [4]int64{}
	namesArr := [4]string{"W", "X", "Y", "Z"}
	for i := 0; i < 4; i++ {
		v := raw[i]
		num, ok := safeMul(v, q.Scale)
		if !ok {
			e := errorf(CodeQuat, "quaternion normalization numerator overflow")
			e.Field = "rotation." + namesArr[i]
			e.OpA = v
			e.OpB = q.Scale
			trace.Overflow = true
			return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
		}
		ov, rem := roundDiv(num, rawNorm)
		out[i] = ov
		if ov > p.MaxComponent || ov < -p.MaxComponent {
			e := errorf(CodeQuat, "normalized quaternion component overflow")
			e.Field = "rotation." + namesArr[i]
			e.OpA = ov
			e.OpB = p.MaxComponent
			return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
		}
		delta := ov - v
		adjustment += abs64(delta)
		components = append(components, ComponentTrunc{
			Component: namesArr[i], ComponentIndex: i, InputNumerator: num, Output: ov,
			Remainder: rem, Denominator: rawNorm, CutBitPosition: cutBitPosition(rawNorm),
			Truncated: rem != 0,
		})
		mv, ok := safeMul(ov, mult)
		if !ok {
			e := errorf(CodeQuat, "quaternion work-scale widening overflow")
			e.OpA = ov
			e.OpB = mult
			return Quat{}, FQ{}, trace, withFrame(e, joint, frame)
		}
		*rawArr[i] = mv
	}
	trace.AdjustmentL1 = adjustment
	trace.Components = components
	normalized := Quat{W: out[0], X: out[1], Y: out[2], Z: out[3], Scale: q.Scale}
	return normalized, rawFQ, trace, nil
}

// NormalizeQuatValue validates and converts a declared-scale quaternion to FQ.
func NormalizeQuatValue(q Quat, p PrecisionParams, joint string, frame int) (FQ, QuatNormTrace, *Error) {
	_, fq, tr, err := NormalizeQuat(q, p, joint, frame)
	return fq, tr, err
}

// QMul multiplies fixed quaternions and returns per-component signed remainders.
func QMul(a, b FQ, scale int64) (FQ, [4]int64, *Error) {
	terms := [4][4]int64{
		{a.W * b.W, -a.X * b.X, -a.Y * b.Y, -a.Z * b.Z},
		{a.W * b.X, a.X * b.W, a.Y * b.Z, -a.Z * b.Y},
		{a.W * b.Y, -a.X * b.Z, a.Y * b.W, a.Z * b.X},
		{a.W * b.Z, a.X * b.Y, -a.Y * b.X, a.Z * b.W},
	}
	// The declared work scale keeps all four-term sums in int64. Re-check to
	// make injected corruption observable as E_QUAT rather than wrapping.
	var out FQ
	o := [4]*int64{&out.W, &out.X, &out.Y, &out.Z}
	rem := [4]int64{}
	for i := 0; i < 4; i++ {
		var sum int64
		for _, term := range terms[i] {
			var ok bool
			sum, ok = safeAdd(sum, term)
			if !ok {
				e := errorf(CodeQuat, "quaternion multiplication overflow")
				e.OpA = a
				e.OpB = b
				return FQ{}, rem, e
			}
		}
		q, r := roundDiv(sum, scale)
		*o[i] = q
		rem[i] = r
	}
	return out, rem, nil
}

func QConj(q FQ) FQ { return FQ{W: q.W, X: -q.X, Y: -q.Y, Z: -q.Z} }

// QNormalize normalizes an already work-scale blended/scaled quaternion.
// The returned bound is the conservative L1 rounding residual in work units.
func QNormalize(q FQ, scale int64, joint string, frame int) (FQ, int64, *Error) {
	norm, ok := quatNorm(q)
	if !ok || norm == 0 {
		e := errorf(CodeQuat, "normalization failed")
		e.OpA = q
		e.OpB = scale
		return FQ{}, 0, withFrame(e, joint, frame)
	}
	arr := [4]int64{q.W, q.X, q.Y, q.Z}
	out := [4]int64{}
	var remSum int64
	for i, v := range arr {
		num, mok := safeMul(v, scale)
		if !mok {
			e := errorf(CodeQuat, "normalization overflow")
			e.OpA = v
			e.OpB = scale
			return FQ{}, 0, withFrame(e, joint, frame)
		}
		qv, rem := roundDiv(num, norm)
		out[i] = qv
		remSum += abs64(rem)
	}
	roundBound := (remSum + norm - 1) / norm
	if roundBound < 0 {
		roundBound = 0
	}
	return FQ{W: out[0], X: out[1], Y: out[2], Z: out[3]}, roundBound, nil
}

// QScaleVectorPart applies a declared rotation-scale coefficient to xyz.
func QScaleVectorPart(q FQ, num, den, scale int64, joint string, frame int) (FQ, [3]int64, *Error) {
	if den <= 0 || num < 0 {
		e := errorf(CodeQuat, "illegal rotation scale rational")
		e.OpA = num
		e.OpB = den
		return FQ{}, [3]int64{}, withFrame(e, joint, frame)
	}
	vs := [3]int64{q.X, q.Y, q.Z}
	var rem [3]int64
	out := [3]int64{}
	for i, v := range vs {
		n, ok := safeMul(v, num)
		if !ok {
			e := errorf(CodeQuat, "rotation scale overflow")
			e.OpA = v
			e.OpB = num
			return FQ{}, rem, withFrame(e, joint, frame)
		}
		out[i], rem[i] = roundDiv(n, den)
	}
	return FQ{W: q.W, X: out[0], Y: out[1], Z: out[2]}, rem, nil
}

// WeightedQ performs fixed-point weighted quaternion redistribution.
func WeightedQ(qs []FQ, weights []int64, weightScale int64) (FQ, int64, *Error) {
	if len(qs) == 0 || len(qs) != len(weights) {
		return FQ{}, 0, errorf(CodeMap, "weighted quaternion requires matching non-empty inputs")
	}
	reference := qs[0]
	var sum FQ
	parts := [4]*int64{&sum.W, &sum.X, &sum.Y, &sum.Z}
	for i, q := range qs {
		if quatL1(reference, q) > quatL1(reference, FQ{W: -q.W, X: -q.X, Y: -q.Y, Z: -q.Z}) {
			q = FQ{W: -q.W, X: -q.X, Y: -q.Y, Z: -q.Z}
		}
		vals := [4]int64{q.W, q.X, q.Y, q.Z}
		for j, v := range vals {
			mv, ok := safeMul(v, weights[i])
			if !ok {
				e := errorf(CodeMap, "mapping weight overflow")
				e.OpA = v
				e.OpB = weights[i]
				return FQ{}, 0, e
			}
			*parts[j] += mv
		}
	}
	arr := [4]int64{sum.W, sum.X, sum.Y, sum.Z}
	out := [4]int64{}
	var remSum int64
	for i, n := range arr {
		q, rem := roundDiv(n, weightScale)
		out[i] = q
		remSum += abs64(rem)
	}
	roundBound := (remSum + weightScale - 1) / weightScale
	return FQ{W: out[0], X: out[1], Y: out[2], Z: out[3]}, roundBound, nil
}

func quatL1(a, b FQ) int64 {
	direct := abs64(a.W-b.W) + abs64(a.X-b.X) + abs64(a.Y-b.Y) + abs64(a.Z-b.Z)
	flipped := abs64(a.W+b.W) + abs64(a.X+b.X) + abs64(a.Y+b.Y) + abs64(a.Z+b.Z)
	if flipped < direct {
		return flipped
	}
	return direct
}

func vecL1(a, b FVec) int64 {
	return abs64(a.X-b.X) + abs64(a.Y-b.Y) + abs64(a.Z-b.Z)
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func vecToWork(v Vec3, scale int64) FVec {
	mult := scale / v.Scale
	return FVec{X: v.X * mult, Y: v.Y * mult, Z: v.Z * mult}
}

func vecFromWork(v FVec, scale int64) Vec3 {
	return Vec3{X: v.X, Y: v.Y, Z: v.Z, Scale: scale}
}
