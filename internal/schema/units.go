package schema

import (
	"terrainkernel/internal/fixed"
	"terrainkernel/internal/kerr"
)

const (
	degRadNum int64 = 3_141_592_653_590
	degRadDen int64 = 180_000_000_000
	radDegNum int64 = 180_000_000_000
	radDegDen int64 = 3_141_592_653_590
)

// HeightToMM converts the on-wire height representation to millimetres.
//
// Version 3 uses "m" but stores fixed-point metres with six fractional digits.
// Therefore one metre is 1_000_000 wire units and 1_000 millimetres.
func HeightToMM(v int64, unit, path string) (int64, error) {
	switch unit {
	case "mm":
		return v, nil
	case "cm":
		return fixed.MulDiv(v, 10, 1, path)
	case "m":
		return fixed.MulDiv(v, 1_000, 1_000_000, path)
	default:
		return 0, kerr.New(kerr.CodeUnit, path, "unrecognized height unit "+quote(unit))
	}
}

func MMToHeight(mm int64, unit, path string) (int64, error) {
	switch unit {
	case "mm":
		return mm, nil
	case "cm":
		return fixed.MulDiv(mm, 1, 10, path)
	case "m":
		return fixed.MulDiv(mm, 1_000_000, 1_000, path)
	default:
		return 0, kerr.New(kerr.CodeUnit, path, "unrecognized height unit "+quote(unit))
	}
}

// SlopeToURad converts degrees to microradians. "deg" is a millidegree
// representation, which keeps the value integral at terrain precision.
func SlopeToURad(v int64, unit, path string) (int64, error) {
	switch unit {
	case "rad":
		return v, nil
	case "deg":
		return fixed.MulDivRounded(v, degRadNum, degRadDen, path, fixed.Nearest)
	default:
		return 0, kerr.New(kerr.CodeUnit, path, "unrecognized slope unit "+quote(unit))
	}
}

func URadToSlope(uraD int64, unit, path string) (int64, error) {
	switch unit {
	case "rad":
		return uraD, nil
	case "deg":
		return fixed.MulDivRounded(uraD, radDegNum, radDegDen, path, fixed.Nearest)
	default:
		return 0, kerr.New(kerr.CodeUnit, path, "unrecognized slope unit "+quote(unit))
	}
}
