package terrainbake

import (
	"fmt"
	"math"
	"strings"
)

// AddChecked and SubChecked keep all coordinate transformations in int64 and
// surface both operands on overflow.
func AddChecked(field string, a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, errOvf(field, a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, errOvf(field, a, b)
	}
	return a + b, nil
}

func SubChecked(field string, a, b int64) (int64, error) {
	if b < 0 && a > math.MaxInt64+b {
		return 0, errOvf(field, a, b)
	}
	if b > 0 && a < math.MinInt64+b {
		return 0, errOvf(field, a, b)
	}
	return a - b, nil
}

func AbsoluteToRelative(field string, abs, origin int64) (int64, error) {
	return SubChecked(field, abs, origin)
}

func RelativeToAbsolute(field string, rel, origin int64) (int64, error) {
	return AddChecked(field, rel, origin)
}

func toleranceFactor(scale string) (int64, bool) {
	switch normalizeScaleName(scale) {
	case "permille":
		return 1000, true // one per-mille of a metre is 1 mm = 1000 um
	case "basis_points":
		return 100, true // one basis point of a metre is 0.1 mm = 100 um
	default:
		return 0, false
	}
}

func divRound(n, d int64, rounding string) int64 {
	if d == 0 {
		return 0
	}
	switch strings.ToLower(rounding) {
	case "ceil", "up":
		if n >= 0 {
			return (n + d - 1) / d
		}
		return n / d
	case "floor", "down", "trunc":
		if n >= 0 {
			return n / d
		}
		return (n - d + 1) / d
	case "half_up", "round", "":
		if n >= 0 {
			return (n + d/2) / d
		}
		return (n - d/2) / d
	default:
		return (n + d/2) / d
	}
}

// ToleranceToUM converts the schema's integer tolerance scale to the
// internal micrometre representation and reports the residual caused by
// integer rounding.
func ToleranceToUM(value int64, scale, rounding string) (um int64, residual int64, err error) {
	factor, ok := toleranceFactor(scale)
	if !ok {
		return 0, 0, errSchema("chord_tolerance.scale", "unsupported tolerance scale "+scale)
	}
	if value <= 0 {
		return 0, 0, errPara("chord_tolerance", ">0", "chord tolerance must be positive")
	}
	if value > math.MaxInt64/factor {
		return 0, 0, errOvf("chord_tolerance", value, factor)
	}
	um = value * factor
	return um, residual, nil
}

// UMToTolerance performs the inverse conversion using the declared rounding
// direction. Residual is in micrometres.
func UMToTolerance(um int64, scale, rounding string) (value int64, residual int64, err error) {
	factor, ok := toleranceFactor(scale)
	if !ok {
		return 0, 0, errSchema("chord_tolerance.scale", "unsupported tolerance scale "+scale)
	}
	if um <= 0 {
		return 0, 0, errPara("chord_tolerance_um", ">0", "internal tolerance must be positive")
	}
	value = divRound(um, factor, rounding)
	residual = um - value*factor
	return value, residual, nil
}

// ConvertTolerance converts directly between permille and basis-point values
// without floating point arithmetic.
func ConvertTolerance(value int64, fromScale, toScale, rounding string) (int64, int64, error) {
	um, _, err := ToleranceToUM(value, fromScale, rounding)
	if err != nil {
		return 0, 0, err
	}
	return UMToTolerance(um, toScale, rounding)
}

type Interval struct {
	Min         int64
	Max         int64
	Truncation  string
	ResidualMin int64
	ResidualMax int64
}

func normalizeCoverageRule(rule string) (string, bool) {
	r := strings.ToLower(strings.TrimSpace(rule))
	switch r {
	case "[]", "closed", "closed_closed", "inclusive":
		return "[]", true
	case "[)", "half_open", "closed_open", "min_inclusive_max_exclusive":
		return "[)", true
	case "(]", "open_closed", "min_exclusive_max_inclusive":
		return "(]", true
	case "()", "open", "open_open":
		return "()", true
	default:
		return "", false
	}
}

// BoundaryToHalfOpen converts a declared endpoint convention to the internal
// half-open convention. Every endpoint adjustment is recorded with its
// residual so callers can explain a one-unit truncation.
func BoundaryToHalfOpen(field string, min, max int64, rule string) (Interval, error) {
	r, ok := normalizeCoverageRule(rule)
	if !ok {
		return Interval{}, errSchema(field+".boundary", "unsupported coverage rule "+rule)
	}
	out := Interval{Min: min, Max: max}
	var err error
	switch r {
	case "[]":
		out.Max, err = AddChecked(field+".max", max, 1)
		if err != nil {
			return Interval{}, err
		}
		out.Truncation = "max+1"
		out.ResidualMax = 1
	case "[)":
		out.Truncation = "none"
	case "()":
		out.Min, err = AddChecked(field+".min", min, 1)
		if err != nil {
			return Interval{}, err
		}
		out.Truncation = "min+1"
		out.ResidualMin = 1
	case "(]":
		out.Min, err = AddChecked(field+".min", min, 1)
		if err != nil {
			return Interval{}, err
		}
		out.Max, err = AddChecked(field+".max", max, 1)
		if err != nil {
			return Interval{}, err
		}
		out.Truncation = "min+1,max+1"
		out.ResidualMin = 1
		out.ResidualMax = 1
	}
	if out.Max <= out.Min {
		return Interval{}, &Error{
			Code:    CodeSchema,
			Path:    field,
			Field:   "coverage",
			Message: "coverage becomes empty after normalization",
		}
	}
	return out, nil
}

// HalfOpenToBoundary serializes an internal rectangle into the requested
// endpoint convention. The inverse returns the exact internal interval for
// all supported conventions.
func HalfOpenToBoundary(field string, min, max int64, rule string) (int64, int64, error) {
	r, ok := normalizeCoverageRule(rule)
	if !ok {
		return 0, 0, errSchema(field+".boundary", "unsupported coverage rule "+rule)
	}
	switch r {
	case "[]":
		maxOut, err := AddChecked(field+".max", max, -1)
		return min, maxOut, err
	case "[)":
		return min, max, nil
	case "()":
		minOut, err := AddChecked(field+".min", min, -1)
		return minOut, max, err
	case "(]":
		minOut, err := AddChecked(field+".min", min, -1)
		if err != nil {
			return 0, 0, err
		}
		maxOut, err := AddChecked(field+".max", max, -1)
		return minOut, maxOut, err
	default:
		return 0, 0, errSchema(field+".boundary", "unsupported coverage rule "+rule)
	}
}

func TraceBoundary(v SchemaVersion, path string, rule string, in Interval) ConversionTrace {
	return ConversionTrace{
		SourceVersion: v,
		Path:          path,
		Operation:     "coverage_to_half_open",
		From:          rule,
		To:            "[min,max)",
		Residual:      in.ResidualMin + in.ResidualMax,
		Truncation:    in.Truncation,
		Note:          fmt.Sprintf("[%d,%d) normalized", in.Min, in.Max),
	}
}

func TraceTolerance(v SchemaVersion, path, scale, rounding string, value, um, residual int64) ConversionTrace {
	return ConversionTrace{
		SourceVersion: v,
		Path:          path,
		Operation:     "tolerance_to_micrometres",
		From:          fmt.Sprintf("%d %s", value, scale),
		To:            fmt.Sprintf("%d um", um),
		Rounding:      rounding,
		Residual:      residual,
		Note:          "integer scaling at 1000 or 100 um per declared unit",
	}
}
