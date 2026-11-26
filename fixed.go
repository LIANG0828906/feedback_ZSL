package silhouette

import (
	"fmt"
	"math"
	"strconv"
)

// RoundingMode controls integer truncation. TowardZero is used for signed
// coordinates; Floor is used when converting a positive scalar to a coarser
// unit where preserving the direction matters.
type RoundingMode string

const (
	RoundTowardZero RoundingMode = "toward_zero"
	RoundFloor      RoundingMode = "floor"
	RoundCeil       RoundingMode = "ceil"
	RoundNearest    RoundingMode = "nearest"
)

type FixedOp struct {
	Field       string       `json:"field"`
	Operation   string       `json:"operation"`
	Unit        string       `json:"unit"`
	OperandA    int64        `json:"operand_a"`
	OperandB    int64        `json:"operand_b,omitempty"`
	Numerator   int64        `json:"numerator,omitempty"`
	Denominator int64        `json:"denominator,omitempty"`
	Mode        RoundingMode `json:"mode,omitempty"`
	Result      int64        `json:"result"`
	Remainder   int64        `json:"remainder,omitempty"`
	Truncated   bool         `json:"truncated,omitempty"`
}

func (o FixedOp) Trace(stage string, source, target int) ConversionTrace {
	return ConversionTrace{
		Stage: stage, Path: o.Field, Action: o.Operation,
		SourceVersion: source, TargetVersion: target, Unit: o.Unit,
		OldValue:  strconv.FormatInt(o.OperandA, 10),
		NewValue:  strconv.FormatInt(o.Result, 10),
		OperandA:  strconv.FormatInt(o.OperandA, 10),
		OperandB:  strconv.FormatInt(o.OperandB, 10),
		Remainder: strconv.FormatInt(o.Remainder, 10),
		Truncated: o.Truncated,
	}
}

func checkAdd(a, b int64, field string) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, overflowError(field, "add", a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, overflowError(field, "add", a, b)
	}
	return a + b, nil
}

func checkSub(a, b int64, field string) (int64, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, overflowError(field, "sub", a, b)
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, overflowError(field, "sub", a, b)
	}
	return a - b, nil
}

func checkMul(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a == -1 && b == math.MinInt64 || b == -1 && a == math.MinInt64 {
		return 0, overflowError(field, "mul", a, b)
	}
	p := a * b
	if p/b != a {
		return 0, overflowError(field, "mul", a, b)
	}
	return p, nil
}

func overflowError(field, operation string, a, b int64) *DomainError {
	return (&DomainError{Code: CodeOverflow, Path: field, Message: "integer conversion overflow"}).
		WithDetail("operation", operation).
		WithDetail("operand_a", strconv.FormatInt(a, 10)).
		WithDetail("operand_b", strconv.FormatInt(b, 10))
}

// ScaleInt performs exact integer scaling. Remainder is always reported when
// the result is not exact, including for signed coordinates.
func ScaleInt(value, numerator, denominator int64, mode RoundingMode, field string) (FixedOp, error) {
	if denominator == 0 {
		return FixedOp{}, (&DomainError{Code: CodeOverflow, Path: field, Message: "zero denominator"}).
			WithDetail("operand_a", strconv.FormatInt(value, 10)).
			WithDetail("operand_b", "0")
	}
	if denominator < 0 {
		if denominator == math.MinInt64 || numerator == math.MinInt64 {
			return FixedOp{}, overflowError(field, "scale_normalize", value, denominator)
		}
		denominator = -denominator
		numerator = -numerator
	}
	op := FixedOp{
		Field: field, Operation: "scale", Unit: "fixed", OperandA: value,
		Numerator: numerator, Denominator: denominator, Mode: mode,
	}
	n, err := checkMul(value, numerator, field)
	if err != nil {
		return op, err
	}
	q := n / denominator
	r := n % denominator
	if r != 0 {
		op.Truncated = true
		switch mode {
		case RoundFloor:
			if r < 0 {
				q--
			}
		case RoundCeil:
			if r > 0 {
				q++
			}
		case RoundNearest:
			absR := r
			if absR < 0 {
				absR = -absR
			}
			threshold := denominator / 2
			if denominator%2 != 0 {
				threshold++
			}
			if absR >= threshold {
				if n < 0 {
					q--
				} else {
					q++
				}
			}
		case RoundTowardZero, "":
			// q already truncated toward zero.
		default:
			return op, NewDomainError(CodeInternal, field, "unknown rounding mode")
		}
	}
	op.Result = q
	op.Remainder = r
	return op, nil
}

func AddFixed(a, b int64, field string) (FixedOp, error) {
	r, err := checkAdd(a, b, field)
	if err != nil {
		return FixedOp{}, err
	}
	return FixedOp{
		Field: field, Operation: "add", Unit: "fixed",
		OperandA: a, OperandB: b, Result: r,
	}, nil
}

func SubFixed(a, b int64, field string) (FixedOp, error) {
	r, err := checkSub(a, b, field)
	if err != nil {
		return FixedOp{}, err
	}
	return FixedOp{
		Field: field, Operation: "sub", Unit: "fixed",
		OperandA: a, OperandB: b, Result: r,
	}, nil
}

func LogicalToFixed(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, FixedScale, 1, RoundTowardZero, field)
}

func FixedToLogical(v int64, mode RoundingMode, field string) (FixedOp, error) {
	return ScaleInt(v, 1, FixedScale, mode, field)
}

// Pixel units deliberately use a declared 1:1 logical-to-pixel relation. The
// conversion still goes through explicit integer operations so residual/truncation
// evidence is available when a caller changes that declaration.
func LogicalToPixel(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1, 1, RoundTowardZero, field)
}

func PixelToLogical(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1, 1, RoundTowardZero, field)
}

func MicroLogicalToFixed(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1, 1, RoundTowardZero, field)
}

func MilliPixelToFixed(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1000, 1, RoundTowardZero, field)
}

func FixedToMilliPixel(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1, 1000, RoundFloor, field)
}

func MicroPixelToFixed(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1, 1, RoundTowardZero, field)
}

func FixedToMicroPixel(v int64, field string) (FixedOp, error) {
	return ScaleInt(v, 1, 1, RoundTowardZero, field)
}

func MicroLogicalToFixedVec(v Vec2, path string) (Vec2, []FixedOp, error) {
	x, err := MicroLogicalToFixed(v.X, path+".x")
	if err != nil {
		return Vec2{}, nil, err
	}
	y, err := MicroLogicalToFixed(v.Y, path+".y")
	if err != nil {
		return Vec2{}, nil, err
	}
	return Vec2{X: x.Result, Y: y.Result}, []FixedOp{x, y}, nil
}

func addVec2(a, b Vec2, path string) (Vec2, []FixedOp, error) {
	x, err := AddFixed(a.X, b.X, path+".x")
	if err != nil {
		return Vec2{}, nil, err
	}
	y, err := AddFixed(a.Y, b.Y, path+".y")
	if err != nil {
		return Vec2{}, nil, err
	}
	return Vec2{X: x.Result, Y: y.Result}, []FixedOp{x, y}, nil
}

func strFixed(v int64) string { return fmt.Sprintf("%d", v) }
