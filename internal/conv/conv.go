package conv

import (
	"fmt"
	"math/big"

	"combatjudge/internal/model"
)

type Rounding int

const (
	RoundFloor Rounding = iota
	RoundNearest
	RoundCeil
)

func (r Rounding) String() string {
	switch r {
	case RoundNearest:
		return "nearest"
	case RoundCeil:
		return "ceil"
	default:
		return "floor"
	}
}

type FixedResult struct {
	Value                int64 `json:"value"`
	RemainderNumerator   int64 `json:"remainder_numerator"`
	RemainderDenominator int64 `json:"remainder_denominator"`
}

func ovf(field, version string, a, b any) error {
	return &model.Error{
		Code: model.CodeOVF, FieldPath: field, SourceVersion: version,
		Message: fmt.Sprintf("integer operation overflow: %v and %v", a, b),
		Op1:     fmt.Sprint(a), Op2: fmt.Sprint(b),
	}
}

func CheckedAdd(field, version string, a, b int64) (int64, error) {
	if (b > 0 && a > maxInt64-b) || (b < 0 && a < minInt64-b) {
		return 0, ovf(field, version, a, b)
	}
	return a + b, nil
}

const (
	maxInt64 = int64(^uint64(0) >> 1)
	minInt64 = -maxInt64 - 1
)

func CheckedMul(field, version string, a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !product.IsInt64() {
		return 0, ovf(field, version, a, b)
	}
	return product.Int64(), nil
}

// MulDiv computes round(a*b/den) without floating point accumulation.
func MulDiv(field, version string, a, b, den int64, rounding Rounding) (int64, int64, error) {
	if den <= 0 {
		return 0, 0, model.NewError(model.CodeUnit, field, version, "division denominator must be positive: %d", den)
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	d := big.NewInt(den)
	q, rem := new(big.Int).QuoRem(num, d, new(big.Int))
	if rem.Sign() != 0 {
		switch rounding {
		case RoundFloor:
			if num.Sign() < 0 {
				q.Sub(q, big.NewInt(1))
			}
		case RoundCeil:
			if num.Sign() > 0 {
				q.Add(q, big.NewInt(1))
			}
		case RoundNearest:
			twice := new(big.Int).Abs(rem)
			twice.Mul(twice, big.NewInt(2))
			if twice.Cmp(d) >= 0 {
				if num.Sign() < 0 {
					q.Sub(q, big.NewInt(1))
				} else {
					q.Add(q, big.NewInt(1))
				}
			}
		}
	}
	if !q.IsInt64() {
		return 0, 0, ovf(field, version, a, b)
	}
	return q.Int64(), rem.Int64(), nil
}

// FromFixed converts a fixed-point raw value to internal Q units.
// raw is denominated in 1/scale units: 500 with scale 1000 means 0.5.
func FromFixed(field, version string, raw, scale int64, rounding Rounding) (FixedResult, error) {
	q, rem, err := MulDiv(field, version, raw, model.InternalScale, scale, rounding)
	if err != nil {
		return FixedResult{}, err
	}
	return FixedResult{Value: q, RemainderNumerator: rem, RemainderDenominator: scale}, nil
}

// ToFixed quantizes internal Q units to a release fixed-point scale.
func ToFixed(field, version string, q, scale int64, rounding Rounding) (int64, int64, error) {
	raw, remainder, err := MulDiv(field, version, q, scale, model.InternalScale, rounding)
	if err != nil {
		return 0, 0, err
	}
	return raw, remainder, nil
}

func FrameToMS(frame, stepMS int64) (int64, error) {
	if frame < 1 {
		return 0, model.NewError(model.CodeStep, "$.frame", "", "logical frame must be one-based, got %d", frame)
	}
	index := frame - 1
	ms, err := CheckedMul("$.frame_ms", "", index, stepMS)
	if err != nil {
		return 0, err
	}
	return ms, nil
}

func MSToFrame(field, version string, ms, stepMS int64, rounding Rounding) (int, int64, error) {
	if stepMS <= 0 {
		return 0, 0, model.NewError(model.CodeStep, "$.frame_step_ms", version, "time step must be positive, got %d", stepMS)
	}
	if ms < 0 {
		return 0, 0, model.NewError(model.CodeSchema, field, version, "millisecond timestamp must be non-negative")
	}
	index, residual, err := MulDiv(field, version, ms, 1, stepMS, rounding)
	if err != nil {
		return 0, 0, err
	}
	return int(index + 1), residual, nil
}

func AddVec(field, version string, a, d model.Vec3) (model.Vec3, error) {
	var out model.Vec3
	var err error
	if out.X, err = CheckedAdd(field+".x", version, a.X, d.X); err != nil {
		return model.Vec3{}, err
	}
	if out.Y, err = CheckedAdd(field+".y", version, a.Y, d.Y); err != nil {
		return model.Vec3{}, err
	}
	if out.Z, err = CheckedAdd(field+".z", version, a.Z, d.Z); err != nil {
		return model.Vec3{}, err
	}
	return out, nil
}

func ExpandDeltas(field, version string, absolute model.Vec3, deltas []model.Vec3) ([]model.Vec3, []model.Trace, error) {
	points := []model.Vec3{absolute}
	traces := []model.Trace{}
	current := absolute
	for i, delta := range deltas {
		next, err := AddVec(fmt.Sprintf("%s[%d]", field, i+1), version, current, delta)
		if err != nil {
			return nil, nil, err
		}
		traces = append(traces, model.Trace{
			SourceVersion: version, FieldPath: fmt.Sprintf("%s[%d]", field, i+1), Op: "delta_expand",
			OldValue: fmt.Sprintf("delta=%+v prev=%+v", delta, current), NewValue: fmt.Sprintf("%+v", next),
			Source: "integer checked addition", RuleVersion: model.TransformRuleVersion,
		})
		current = next
		points = append(points, next)
	}
	return points, traces, nil
}

// RequantizeRoundTrip proves that export/import quantization is stable after
// the first lossy compression: the second residual is always zero.
func RequantizeRoundTrip(q int64, releaseScale int64) (recoveredQ int64, raw int64, firstRemainder int64, stable bool, err error) {
	raw, firstRemainder, err = ToFixed("roundtrip", "", q, releaseScale, RoundFloor)
	if err != nil {
		return 0, 0, 0, false, err
	}
	back, err := FromFixed("roundtrip", "", raw, releaseScale, RoundFloor)
	if err != nil {
		return 0, 0, 0, false, err
	}
	raw2, remainder2, err := ToFixed("roundtrip", "", back.Value, releaseScale, RoundFloor)
	if err != nil {
		return 0, 0, 0, false, err
	}
	return back.Value, raw, firstRemainder, raw2 == raw && remainder2 == 0, nil
}
