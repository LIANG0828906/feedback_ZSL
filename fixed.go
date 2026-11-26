package quantforensics

import (
	"fmt"
	"math"
	"math/bits"
)

// ScaleUnit identifies a fixed-point denominator. The internal canonical unit
// is parts-per-million because both declared external units map exactly to it.
type ScaleUnit string

const (
	ScalePermille  ScaleUnit = "permille"  // value / 1_000
	ScalePermyriad ScaleUnit = "permyriad" // value / 10_000
	ScalePPM       ScaleUnit = "ppm"       // value / 1_000_000
)

// Residual records one integer truncation point. Remainder is always the
// non-negative Euclidean remainder of Input divided by Divisor before the
// declared rounding is applied.
type Residual struct {
	Field         string `json:"field"`
	Operation     string `json:"operation"`
	Position      string `json:"position"`
	Input         int64  `json:"input"`
	Divisor       int64  `json:"divisor"`
	FloorQuotient int64  `json:"floorQuotient"`
	OutputValue   int64  `json:"outputValue"`
	Remainder     int64  `json:"remainder"`
	RemainderUnit string `json:"remainderUnit"`
	Rounding      string `json:"rounding"`
}

func scaleFactor(unit ScaleUnit) int64 {
	switch unit {
	case ScalePermille:
		return 1000
	case ScalePermyriad:
		return 100
	case ScalePPM:
		return 1
	default:
		return 0
	}
}

func checkedMulNonNegative(field string, a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, overflowError(field, a, b, "checked multiplication requires non-negative operands")
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, overflowError(field, a, b, "fixed-point multiplication overflows int64")
	}
	return int64(lo), nil
}

func checkedAdd(field string, a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, overflowError(field, a, b, "checked addition overflows int64")
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, overflowError(field, a, b, "checked addition underflows int64")
	}
	return a + b, nil
}

func checkedSub(field string, a, b int64) (int64, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, overflowError(field, a, b, "checked subtraction underflows int64")
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, overflowError(field, a, b, "checked subtraction overflows int64")
	}
	return a - b, nil
}

func floorDivMod(a, b int64) (int64, int64) {
	q := a / b
	r := a % b
	if r < 0 {
		r += b
		q--
	}
	return q, r
}

// ScaleToPPM maps an external fixed-point value to the internal PPM unit using
// integer multiplication only.
func ScaleToPPM(value int64, from ScaleUnit, field string) (int64, Residual, error) {
	factor := scaleFactor(from)
	if factor == 0 {
		return 0, Residual{}, schemaError(field, "unknown scale unit %q", from)
	}
	ppm, err := checkedMulNonNegative(field, value, factor)
	if err != nil {
		return 0, Residual{}, err
	}
	return ppm, Residual{
		Field:         field,
		Operation:     string(from) + "_to_ppm",
		Position:      field + ":denominator*" + fmt.Sprint(factor),
		Input:         value,
		Divisor:       1,
		FloorQuotient: ppm,
		OutputValue:   ppm,
		Remainder:     0,
		RemainderUnit: string(ScalePPM),
		Rounding:      "exact",
	}, nil
}

// PPMToScale exports the internal PPM value using the declared rounding.
func PPMToScale(ppm int64, to ScaleUnit, rounding, field string) (int64, Residual, error) {
	divisor := scaleFactor(to)
	if divisor == 0 {
		return 0, Residual{}, schemaError(field, "unknown target scale unit %q", to)
	}
	if ppm < 0 {
		return 0, Residual{}, overflowError(field, ppm, divisor, "scale value must be non-negative")
	}
	floorQ, rem := floorDivMod(ppm, divisor)
	out := floorQ
	switch rounding {
	case "", "floor", "trunc":
		rounding = "floor"
	case "ceil":
		if rem != 0 {
			var err error
			out, err = checkedAdd(field, floorQ, 1)
			if err != nil {
				return 0, Residual{}, err
			}
		}
	default:
		return 0, Residual{}, schemaError(field, "unsupported rounding direction %q", rounding)
	}
	return out, Residual{
		Field:         field,
		Operation:     "ppm_to_" + string(to),
		Position:      field + ":integer_division_by_" + fmt.Sprint(divisor),
		Input:         ppm,
		Divisor:       divisor,
		FloorQuotient: floorQ,
		OutputValue:   out,
		Remainder:     rem,
		RemainderUnit: string(ScalePPM),
		Rounding:      rounding,
	}, nil
}

// AbsoluteToBlockIndex derives a zero-based block index from an absolute pixel
// coordinate. The residual is the in-block pixel offset, always non-negative.
func AbsoluteToBlockIndex(abs, origin, tile int64, axis, field string) (int64, int64, Residual, error) {
	if tile <= 0 {
		return 0, 0, Residual{}, schemaError(field, "%s tile size must be positive", axis)
	}
	shifted, err := checkedSub(field+".origin", abs, origin)
	if err != nil {
		return 0, 0, Residual{}, err
	}
	index, offset := floorDivMod(shifted, tile)
	if index < 0 {
		return 0, 0, Residual{}, overflowError(field, abs, origin, "coordinate is before declared origin")
	}
	return index, offset, Residual{
		Field:         field,
		Operation:     "absolute_pixel_to_block_index",
		Position:      field + ":" + axis + ":floor((" + fmt.Sprint(abs) + "-origin)/tile)",
		Input:         abs,
		Divisor:       tile,
		FloorQuotient: index,
		OutputValue:   index,
		Remainder:     offset,
		RemainderUnit: "pixel",
		Rounding:      "floor",
	}, nil
}

// BlockIndexToAbsolute derives the upper-left pixel coordinate. Multiplication
// and origin addition are range checked.
func BlockIndexToAbsolute(index, origin, tile int64, axis, field string) (int64, Residual, error) {
	if index < 0 || tile <= 0 {
		return 0, Residual{}, schemaError(field, "index must be non-negative and tile positive")
	}
	offset, err := checkedMulNonNegative(field+".index*tile", index, tile)
	if err != nil {
		return 0, Residual{}, err
	}
	abs, err := checkedAdd(field, origin, offset)
	if err != nil {
		return 0, Residual{}, err
	}
	return abs, Residual{
		Field:         field,
		Operation:     "block_index_to_absolute_pixel",
		Position:      field + ":" + axis + ":origin+index*tile",
		Input:         index,
		Divisor:       1,
		FloorQuotient: abs,
		OutputValue:   abs,
		Remainder:     0,
		RemainderUnit: "pixel",
		Rounding:      "exact",
	}, nil
}
