package fixed

import (
	"fmt"
	"math"
	"math/big"

	"skeletonretarget/internal/model"
)

type Truncation struct {
	Stage     string
	Component string
	Remainder int64
	Divisor   int64
	Mode      model.RoundMode
}

type NormalizeResult struct {
	Q               model.Quat
	NormResidualPPM int64
	Truncations     []Truncation
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// DivRound performs integer division while exposing the remainder. This keeps
// every quantization decision inspectable.
func DivRound(numerator, denominator int64, mode model.RoundMode) (int64, int64) {
	if denominator <= 0 {
		return 0, 0
	}
	q := numerator / denominator
	r := numerator % denominator
	switch mode {
	case model.RoundFloor:
		if r < 0 {
			q--
		}
	case model.RoundCeil:
		if r > 0 {
			q++
		}
	case model.RoundHalfUp:
		if abs64(r)*2 >= denominator {
			if numerator >= 0 {
				q++
			} else {
				q--
			}
		}
	}
	return q, r
}

func Rescale(value, from, to int64, mode model.RoundMode) (int64, Truncation) {
	if from <= 0 || to <= 0 {
		return 0, Truncation{Stage: "rescale", Remainder: 0, Divisor: 0, Mode: mode}
	}
	q, r := DivRound(value*to, from, mode)
	return q, Truncation{
		Stage:     "rescale",
		Component: fmt.Sprintf("%d->%d", from, to),
		Remainder: r,
		Divisor:   from,
		Mode:      mode,
	}
}

func RescaleQuat(q model.Quat, scale int64, mode model.RoundMode) (model.Quat, []Truncation) {
	out := model.Quat{Scale: scale}
	values := [4]int64{q.W, q.X, q.Y, q.Z}
	results := [4]int64{}
	truncs := make([]Truncation, 0, 4)
	for i, value := range values {
		var t Truncation
		results[i], t = Rescale(value, q.Scale, scale, mode)
		t.Component = fmt.Sprintf("quat.%d", i)
		truncs = append(truncs, t)
	}
	out.W, out.X, out.Y, out.Z = results[0], results[1], results[2], results[3]
	return out, truncs
}

func RescaleVec(v model.FixedVec3, scale int64, mode model.RoundMode) (model.FixedVec3, []Truncation) {
	out := model.FixedVec3{Scale: scale}
	values := [3]int64{v.X, v.Y, v.Z}
	results := [3]int64{}
	truncs := make([]Truncation, 0, 3)
	for i, value := range values {
		var t Truncation
		results[i], t = Rescale(value, v.Scale, scale, mode)
		t.Component = fmt.Sprintf("vec.%d", i)
		truncs = append(truncs, t)
	}
	out.X, out.Y, out.Z = results[0], results[1], results[2]
	return out, truncs
}

func normSquared(q model.Quat) *big.Int {
	sum := new(big.Int)
	for _, component := range q.Components() {
		term := new(big.Int).SetInt64(component)
		term.Mul(term, term)
		sum.Add(sum, term)
	}
	return sum
}

// NormPPM reports |norm-1| in parts per million without floating point.
func NormPPM(q model.Quat) int64 {
	if q.Scale <= 0 {
		return 1_000_000
	}
	root := new(big.Int).Sqrt(normSquared(q))
	diff := new(big.Int).Sub(root, big.NewInt(q.Scale))
	diff.Abs(diff)
	diff.Mul(diff, big.NewInt(1_000_000))
	diff.Div(diff, big.NewInt(q.Scale))
	if !diff.IsInt64() {
		return 1_000_000
	}
	return diff.Int64()
}

// Normalize converts a fixed quaternion to unit length at its declared scale.
// It rejects components outside [-scale, scale] so corrupt rotations cannot
// masquerade as normalized input.
func Normalize(q model.Quat, mode model.RoundMode) (NormalizeResult, *model.Error) {
	if q.Scale <= 0 {
		return NormalizeResult{}, &model.Error{
			Code: model.CodeComp, Frame: -1, Message: "quaternion scale must be positive",
		}
	}
	for _, component := range q.Components() {
		if abs64(component) > q.Scale {
			return NormalizeResult{}, &model.Error{
				Code: model.CodeComp, Frame: -1,
				Message: fmt.Sprintf("component %d exceeds fixed scale %d", component, q.Scale),
			}
		}
	}
	root := new(big.Int).Sqrt(normSquared(q))
	if root.Sign() == 0 {
		return NormalizeResult{}, &model.Error{
			Code: model.CodeNorm, Frame: -1, Message: "zero-length quaternion",
		}
	}
	rootInt := root.Int64()
	out := model.Quat{Scale: q.Scale}
	values := [4]int64{q.W, q.X, q.Y, q.Z}
	results := [4]int64{}
	truncs := make([]Truncation, 0, 4)
	for i, component := range values {
		var t Truncation
		results[i], t.Remainder = DivRound(component*q.Scale, rootInt, mode)
		t.Stage = "normalize"
		t.Component = fmt.Sprintf("quat.%d", i)
		t.Divisor = rootInt
		t.Mode = mode
		truncs = append(truncs, t)
	}
	out.W, out.X, out.Y, out.Z = results[0], results[1], results[2], results[3]
	return NormalizeResult{
		Q:               out,
		NormResidualPPM: NormPPM(q),
		Truncations:     truncs,
	}, nil
}

func NormalizeChecked(q model.Quat, tolerancePPM int64, mode model.RoundMode) (NormalizeResult, *model.Error) {
	result, err := Normalize(q, mode)
	if err != nil {
		return result, err
	}
	if result.NormResidualPPM > tolerancePPM {
		return result, &model.Error{
			Code: model.CodeNorm, Frame: -1,
			Message: fmt.Sprintf("normalization residual %d ppm exceeds tolerance %d ppm", result.NormResidualPPM, tolerancePPM),
		}
	}
	return result, nil
}

// Mul composes two rotations and normalizes the product. Products are kept in
// fixed point; no floating-point accumulator exists in this path.
func Mul(a, b model.Quat, mode model.RoundMode) (model.Quat, []Truncation, *model.Error) {
	if a.Scale <= 0 || b.Scale <= 0 {
		return model.Quat{}, nil, &model.Error{Code: model.CodeComp, Frame: -1, Message: "quaternion scale must be positive"}
	}
	var truncs []Truncation
	if a.Scale != b.Scale {
		var rescaleTruncs []Truncation
		b, rescaleTruncs = RescaleQuat(b, a.Scale, mode)
		truncs = append(truncs, rescaleTruncs...)
	}
	s := a.Scale
	w := a.W*b.W - a.X*b.X - a.Y*b.Y - a.Z*b.Z
	x := a.W*b.X + a.X*b.W + a.Y*b.Z - a.Z*b.Y
	y := a.W*b.Y - a.X*b.Z + a.Y*b.W + a.Z*b.X
	z := a.W*b.Z + a.X*b.Y - a.Y*b.X + a.Z*b.W
	raw := model.Quat{W: w, X: x, Y: y, Z: z, Scale: s * s}
	// Reduce the homogeneous product back to the component scale.
	scaled := model.Quat{Scale: s}
	values := [4]int64{raw.W, raw.X, raw.Y, raw.Z}
	results := [4]int64{}
	truncs = append(make([]Truncation, 0, 8), truncs...)
	for i, component := range values {
		var t Truncation
		results[i], t.Remainder = DivRound(component, s, mode)
		t.Stage = "multiply"
		t.Component = fmt.Sprintf("quat.%d", i)
		t.Divisor = s
		t.Mode = mode
		truncs = append(truncs, t)
	}
	scaled.W, scaled.X, scaled.Y, scaled.Z = results[0], results[1], results[2], results[3]
	normalized, normErr := Normalize(scaled, mode)
	if normErr != nil {
		return model.Quat{}, truncs, normErr
	}
	truncs = append(truncs, normalized.Truncations...)
	return normalized.Q, truncs, nil
}

func Inverse(q model.Quat) model.Quat {
	return model.Quat{W: q.W, X: -q.X, Y: -q.Y, Z: -q.Z, Scale: q.Scale}
}

func Identity(scale int64) model.Quat {
	return model.Quat{W: scale, Scale: scale}
}

// AdjustRotation keeps the scalar part and rescales the vector part. It is the
// fixed-point interpretation of a retarget rotation coefficient.
func AdjustRotation(q model.Quat, ratio model.Ratio, mode model.RoundMode) (model.Quat, []Truncation, *model.Error) {
	if !ratio.Valid() || q.Scale <= 0 {
		return model.Quat{}, nil, &model.Error{Code: model.CodeComp, Frame: -1, Message: "invalid rotation ratio"}
	}
	adjusted := model.Quat{W: q.W, Scale: q.Scale}
	values := [3]int64{q.X, q.Y, q.Z}
	results := [3]int64{}
	truncs := make([]Truncation, 0, 7)
	for i, component := range values {
		var t Truncation
		results[i], t.Remainder = DivRound(component*ratio.Num, ratio.Den, mode)
		t.Stage = "rotation_scale"
		t.Component = fmt.Sprintf("quat.%d", i+1)
		t.Divisor = ratio.Den
		t.Mode = mode
		truncs = append(truncs, t)
	}
	adjusted.X, adjusted.Y, adjusted.Z = results[0], results[1], results[2]
	normalized, err := Normalize(adjusted, mode)
	if err != nil {
		return model.Quat{}, truncs, err
	}
	truncs = append(truncs, normalized.Truncations...)
	return normalized.Q, truncs, nil
}

func QuatDeltaPPM(a, b model.Quat) int64 {
	if a.Scale <= 0 || b.Scale <= 0 {
		return 1_000_000
	}
	if a.Scale != b.Scale {
		b, _ = RescaleQuat(b, a.Scale, model.RoundHalfUp)
	}
	maxDiff := int64(0)
	for i, component := range a.Components() {
		diff := abs64(component - b.Components()[i])
		if diff > maxDiff {
			maxDiff = diff
		}
	}
	return (maxDiff*1_000_000 + a.Scale/2) / a.Scale
}

func VecDeltaPPM(a, b model.FixedVec3) int64 {
	if a.Scale <= 0 || b.Scale <= 0 {
		return 1_000_000
	}
	if a.Scale != b.Scale {
		b, _ = RescaleVec(b, a.Scale, model.RoundHalfUp)
	}
	maxDiff := int64(0)
	for i, component := range a.Components() {
		diff := abs64(component - b.Components()[i])
		if diff > maxDiff {
			maxDiff = diff
		}
	}
	return (maxDiff*1_000_000 + a.Scale/2) / a.Scale
}

func ScaleVec(v model.FixedVec3, ratios ...model.Ratio) (model.FixedVec3, []Truncation, *model.Error) {
	out := model.FixedVec3{X: v.X, Y: v.Y, Z: v.Z, Scale: v.Scale}
	var truncs []Truncation
	for _, ratio := range ratios {
		if !ratio.Valid() {
			return model.FixedVec3{}, truncs, &model.Error{Code: model.CodeComp, Frame: -1, Message: "invalid translation ratio"}
		}
		values := [3]int64{out.X, out.Y, out.Z}
		results := [3]int64{}
		for i, component := range values {
			var t Truncation
			results[i], t.Remainder = DivRound(component*ratio.Num, ratio.Den, model.RoundHalfUp)
			t.Stage = "vector_scale"
			t.Component = fmt.Sprintf("vec.%d", i)
			t.Divisor = ratio.Den
			t.Mode = model.RoundHalfUp
			truncs = append(truncs, t)
		}
		out.X, out.Y, out.Z = results[0], results[1], results[2]
	}
	return out, truncs, nil
}

// RotateVec rotates a fixed vector by a unit quaternion using a single rational
// expression, then rounds once.
func RotateVec(q model.Quat, v model.FixedVec3) (model.FixedVec3, *model.Error) {
	if q.Scale <= 0 || v.Scale <= 0 {
		return model.FixedVec3{}, &model.Error{Code: model.CodeComp, Frame: -1, Message: "rotation or vector scale must be positive"}
	}
	w, x, y, z := big.NewInt(q.W), big.NewInt(q.X), big.NewInt(q.Y), big.NewInt(q.Z)
	vx, vy, vz := big.NewInt(v.X), big.NewInt(v.Y), big.NewInt(v.Z)
	ww := new(big.Int).Mul(w, w)
	xx := new(big.Int).Mul(x, x)
	yy := new(big.Int).Mul(y, y)
	zz := new(big.Int).Mul(z, z)
	scalar := new(big.Int).Sub(ww, xx)
	scalar.Sub(scalar, yy)
	scalar.Sub(scalar, zz)
	dot := new(big.Int).Mul(x, vx)
	dot.Add(dot, new(big.Int).Mul(y, vy))
	dot.Add(dot, new(big.Int).Mul(z, vz))

	numX := new(big.Int).Mul(vx, scalar)
	numX.Add(numX, new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(x, dot)))
	crossX := new(big.Int).Sub(new(big.Int).Mul(y, vz), new(big.Int).Mul(z, vy))
	numX.Add(numX, new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(w, crossX)))

	numY := new(big.Int).Mul(vy, scalar)
	numY.Add(numY, new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(y, dot)))
	crossY := new(big.Int).Sub(new(big.Int).Mul(z, vx), new(big.Int).Mul(x, vz))
	numY.Add(numY, new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(w, crossY)))

	numZ := new(big.Int).Mul(vz, scalar)
	numZ.Add(numZ, new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(z, dot)))
	crossZ := new(big.Int).Sub(new(big.Int).Mul(x, vy), new(big.Int).Mul(y, vx))
	numZ.Add(numZ, new(big.Int).Mul(big.NewInt(2), new(big.Int).Mul(w, crossZ)))

	den := new(big.Int).Mul(big.NewInt(q.Scale), big.NewInt(q.Scale))
	roundDivBig := func(numerator *big.Int) int64 {
		d := new(big.Int).Mul(numerator, big.NewInt(2))
		d.Add(d, den)
		d.Div(d, new(big.Int).Mul(den, big.NewInt(2)))
		return d.Int64()
	}
	return model.FixedVec3{
		X: roundDivBig(numX), Y: roundDivBig(numY), Z: roundDivBig(numZ), Scale: v.Scale,
	}, nil
}

// AngleRadians uses a single trigonometric evaluation for constraint checks;
// no value from this path is accumulated into the retarget chain.
func AngleRadians(q model.Quat) (float64, *model.Error) {
	if q.Scale <= 0 {
		return 0, &model.Error{Code: model.CodeComp, Frame: -1, Message: "quaternion scale must be positive"}
	}
	root := new(big.Int).Sqrt(normSquared(q))
	if root.Sign() == 0 {
		return 0, &model.Error{Code: model.CodeNorm, Frame: -1, Message: "zero-length quaternion"}
	}
	cosHalf := float64(q.W) / float64(root.Int64())
	if cosHalf > 1 {
		cosHalf = 1
	}
	if cosHalf < -1 {
		cosHalf = -1
	}
	return 2 * math.Acos(cosHalf), nil
}

func ClampConstraint(q model.Quat, constraint model.Constraint) (model.Quat, bool, *model.Error) {
	if constraint.Scale <= 0 || constraint.MaxRad < constraint.MinRad {
		return model.Quat{}, false, &model.Error{Code: model.CodeComp, Frame: -1, Message: "invalid joint constraint"}
	}
	angle, err := AngleRadians(q)
	if err != nil {
		return model.Quat{}, false, err
	}
	minAngle := float64(constraint.MinRad) / float64(constraint.Scale)
	maxAngle := float64(constraint.MaxRad) / float64(constraint.Scale)
	if angle >= minAngle-1e-12 && angle <= maxAngle+1e-12 {
		return q, false, nil
	}
	targetAngle := minAngle
	if angle > maxAngle {
		targetAngle = maxAngle
	}
	vecNorm := normSquared(model.Quat{X: q.X, Y: q.Y, Z: q.Z, Scale: q.Scale})
	vecNorm.Sqrt(vecNorm)
	if vecNorm.Sign() == 0 {
		return q, false, nil
	}
	axisScale := vecNorm.Int64()
	half := targetAngle / 2
	s, c := math.Sincos(half)
	out := model.Quat{Scale: q.Scale}
	values := [3]int64{q.X, q.Y, q.Z}
	out.W = int64(math.Round(c * float64(q.Scale)))
	out.X = int64(math.Round(s * float64(q.Scale) * (float64(values[0]) / float64(axisScale))))
	out.Y = int64(math.Round(s * float64(q.Scale) * (float64(values[1]) / float64(axisScale))))
	out.Z = int64(math.Round(s * float64(q.Scale) * (float64(values[2]) / float64(axisScale))))
	normalized, normErr := Normalize(out, model.RoundHalfUp)
	if normErr != nil {
		return model.Quat{}, false, normErr
	}
	return normalized.Q, true, nil
}

func FrameToMS(frame, fps int64, mode model.RoundMode) (int64, Truncation) {
	ms, remainder := DivRound(frame*1000, fps, mode)
	return ms, Truncation{Stage: "frame_to_ms", Remainder: remainder, Divisor: fps, Mode: mode}
}

func MSToFrame(ms, fps int64, mode model.RoundMode) (int64, Truncation) {
	frame, remainder := DivRound(ms*fps, 1000, mode)
	return frame, Truncation{Stage: "ms_to_frame", Remainder: remainder, Divisor: 1000, Mode: mode}
}

// QuantizeRoundTrip models a high->low->high release conversion.
func QuantizeRoundTrip(q model.Quat, lowScale int64, mode model.RoundMode) (model.Quat, []Truncation) {
	low, first := RescaleQuat(q, lowScale, mode)
	high, second := RescaleQuat(low, q.Scale, mode)
	return high, append(first, second...)
}
