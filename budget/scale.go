package budget

import (
	"math"
)

// Scale identifies a fixed-point denominator.
type Scale string

const (
	ScalePermille  Scale = "permille"  // denominator 1_000
	ScalePermyriad Scale = "permyriad" // denominator 10_000, also basis points
	ScaleInternal  Scale = "permyriad"
)

func (s Scale) denominator() (int64, bool) {
	switch s {
	case ScalePermille:
		return 1_000, true
	case ScalePermyriad:
		return 10_000, true
	default:
		return 0, false
	}
}

// Rounding declares the integer rounding direction used by a conversion.
type Rounding string

const (
	RoundFloor   Rounding = "floor_toward_negative_infinity"
	RoundCeil    Rounding = "ceil_toward_positive_infinity"
	RoundTrunc   Rounding = "trunc_toward_zero"
	RoundNearest Rounding = "nearest_half_up"
	RoundExact   Rounding = "exact_zero_residual"
)

// ScaledValue is a fixed-point integer plus the exact source remainder.
//
// When reducing resolution, Residual retains source-denominator units lost by
// rounding. RestoreHighResolution performs the exact inverse and is used by
// round-trip tests and field conversion traces.
type ScaledValue struct {
	Value         int64         `json:"value"`
	Scale         Scale         `json:"scale"`
	Residual      int64         `json:"residual"`
	ResidualScale Scale         `json:"residual_scale,omitempty"`
	Rounding      Rounding      `json:"rounding,omitempty"`
	Field         string        `json:"field,omitempty"`
	SourceVersion SourceVersion `json:"source_version,omitempty"`
}

// ConvertScaled performs rational fixed-point conversion using integers only.
// The returned residual is always expressed in the source scale.
func ConvertScaled(raw int64, from, to Scale, rounding Rounding, field string) (ScaledValue, error) {
	fd, ok := from.denominator()
	if !ok {
		return ScaledValue{}, newError(ECodeScale, "unknown source scale %q", from).withField(field)
	}
	td, ok := to.denominator()
	if !ok {
		return ScaledValue{}, newError(ECodeScale, "unknown destination scale %q", to).withField(field)
	}

	result := ScaledValue{Scale: to, ResidualScale: from, Rounding: rounding, Field: field}

	switch {
	case fd == td:
		result.Value = raw
		return result, nil
	case td > fd && td%fd == 0:
		// Increasing resolution is an exact checked multiplication.
		factor := td / fd
		value, err := mulChecked(raw, factor, field)
		if err != nil {
			return ScaledValue{}, err
		}
		result.Value = value
		return result, nil
	case fd > td && fd%td == 0:
		// The remainder is retained in source-scale units, making the declared
		// rounding direction and the inverse restoration both explicit.
		factor := fd / td
		quot, rem, err := divRemChecked(raw, factor, rounding, field)
		if err != nil {
			return ScaledValue{}, err
		}
		if rounding == RoundExact && rem != 0 {
			return ScaledValue{}, &Error{
				Code:     ECodeScale,
				Message:  "exact fixed-point conversion required but remainder is non-zero",
				Field:    field,
				Path:     field,
				Operands: []int64{raw, factor},
			}
		}
		result.Value = quot
		result.Residual = rem
		return result, nil
	default:
		return ScaledValue{}, &Error{
			Code: ECodeScale, Message: "fixed-point scales do not have an integral conversion ratio",
			Field: field, Path: field, Operands: []int64{fd, td},
		}
	}
}

// RestoreHighResolution is the exact inverse for a down-converted value.
func (v ScaledValue) RestoreHighResolution(to Scale, field string) (ScaledValue, error) {
	td, ok := to.denominator()
	if !ok {
		return ScaledValue{}, newError(ECodeScale, "unknown target scale %q", to).withField(field)
	}
	cd, ok := v.Scale.denominator()
	if !ok {
		return ScaledValue{}, newError(ECodeScale, "unknown current scale %q", v.Scale).withField(field)
	}
	if td <= cd {
		return ConvertScaled(v.Value, v.Scale, to, RoundExact, field)
	}
	if td%cd != 0 {
		return ScaledValue{}, &Error{
			Code: ECodeScale, Message: "fixed-point scales do not have an integral restoration ratio",
			Field: field, Path: field, Operands: []int64{cd, td},
		}
	}
	factor := td / cd
	base, err := mulChecked(v.Value, factor, field)
	if err != nil {
		return ScaledValue{}, err
	}
	raw, err := addChecked(base, v.Residual, field)
	if err != nil {
		return ScaledValue{}, err
	}
	return ScaledValue{Value: raw, Scale: to, Field: field}, nil
}

// MillisToTicks converts milliseconds to internal ticks. Residual is in
// milliseconds. Sample documents use exact multiples of TickMS so all three
// source versions resolve to identical rules.
func MillisToTicks(ms, tickMS int64, rounding Rounding, field string) (ticks int64, residualMS int64, err error) {
	if tickMS <= 0 {
		return 0, 0, &Error{
			Code:     ECodeScale,
			Message:  "tick duration must be positive",
			Field:    field,
			Path:     field,
			Operands: []int64{ms, tickMS},
		}
	}
	q, r, derr := divRemChecked(ms, tickMS, rounding, field)
	if derr != nil {
		return 0, 0, derr
	}
	if rounding == RoundExact && r != 0 {
		return 0, 0, &Error{
			Code:     ECodeScale,
			Message:  "exact millisecond-to-tick conversion required but remainder is non-zero",
			Field:    field,
			Path:     field,
			Operands: []int64{ms, tickMS},
		}
	}
	return q, r, nil
}

func TicksToMillis(ticks, tickMS int64, field string) (int64, error) {
	return mulChecked(ticks, tickMS, field)
}

func mulChecked(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}

	switch {
	case a > 0 && b > 0:
		if a > math.MaxInt64/b {
			return 0, overflowError(field, a, b)
		}
	case a < 0 && b < 0:
		if a == math.MinInt64 || b == math.MinInt64 {
			return 0, overflowError(field, a, b)
		}
		aa, bb := -a, -b
		if aa > math.MaxInt64/bb {
			return 0, overflowError(field, a, b)
		}
	case a > 0 && b < 0:
		if b < math.MinInt64/a {
			return 0, overflowError(field, a, b)
		}
	case a < 0 && b > 0:
		if a < math.MinInt64/b {
			return 0, overflowError(field, a, b)
		}
	}

	result := a * b
	if result/b != a {
		return 0, overflowError(field, a, b)
	}
	return result, nil
}

func addChecked(a, b int64, field string) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, overflowError(field, a, b)
	}
	return a + b, nil
}

func overflowError(field string, a, b int64) *Error {
	return &Error{
		Code:     ECodeOVF,
		Message:  "integer operation overflow",
		Field:    field,
		Path:     field,
		Operands: []int64{a, b},
	}
}

func divRemChecked(a, b int64, rounding Rounding, field string) (int64, int64, error) {
	if b == 0 {
		return 0, 0, &Error{
			Code:     ECodeEval,
			Message:  "division by zero",
			Field:    field,
			Path:     field,
			Operands: []int64{a, b},
		}
	}
	if a == math.MinInt64 && b == -1 {
		return 0, 0, overflowError(field, a, b)
	}
	q := a / b
	r := a % b
	switch rounding {
	case RoundFloor:
		if r != 0 && ((a < 0) != (b < 0)) {
			q--
			r += b
		}
	case RoundCeil:
		if r != 0 && ((a > 0) == (b > 0)) {
			q++
			r -= b
		}
	case RoundNearest:
		// Samples are non-negative. Half-up is implemented exactly for b>0.
		if b > 0 && ((b%2 == 0 && r >= b/2) || (b%2 == 1 && r > b/2)) {
			q++
			r -= b
		}
	case RoundTrunc, RoundExact:
		// Go division already truncates toward zero; exact checks r later.
	default:
		return 0, 0, newError(ECodeScale, "unsupported rounding direction %q", rounding).withField(field)
	}
	return q, r, nil
}

func (e *Error) withField(field string) *Error {
	e.Field = field
	if e.Path == "" {
		e.Path = field
	}
	return e
}
