package ocrflow

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
)

// Internal scales are basis points (1/10000) and absolute integer pixels.
const (
	InternalScale       = 10000
	PermilleScale       = 1000
	DefaultBlockWidth   = 8
	DefaultBlockHeight  = 8
	ThresholdResidualBP = 5
	CoordResidualPx     = DefaultBlockWidth - 1
)

type RoundingDirection string

const (
	RoundNearestHalfUp RoundingDirection = "nearest_half_up"
	RoundFloor         RoundingDirection = "floor"
	RoundCeil          RoundingDirection = "ceil"
)

func mulNonNegativeChecked(a, b int, field string) (int, error) {
	if a < 0 || b < 0 {
		return 0, overflowError(field, a, b)
	}
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt/b {
		return 0, overflowError(field, a, b)
	}
	return a * b, nil
}

// PermilleToBPS converts 1/1000 units to internal 1/10000 units.
func PermilleToBPS(v int, field string) (int, error) {
	return mulNonNegativeChecked(v, 10, field)
}

// BPSToPermille converts internal basis points to permille using the declared
// nearest-half-up rounding direction.
func BPSToPermille(v int, field string) (int, error) {
	if v < 0 {
		return 0, overflowError(field, v, "negative")
	}
	q, r := v/10, v%10
	if r >= 5 {
		if q == math.MaxInt {
			return 0, overflowError(field, v, RoundNearestHalfUp)
		}
		q++
	}
	return q, nil
}

func FloorDivPositive(v, d int) int { return v / d }

func CeilDivPositive(v, d int) int {
	q, r := v/d, v%d
	if r != 0 {
		q++
	}
	return q
}

// PixelPointToBlock converts absolute pixels to block-derived coordinates.
// Left/top callers use floor and right/bottom callers use ceil so the block
// representation conservatively encloses the source box.
func PixelPointToBlock(p Point, blockW, blockH int, round RoundingDirection) (Point, error) {
	if blockW <= 0 || blockH <= 0 {
		return Point{}, paramError("coordinate_block", []int{1, math.MaxInt}, "block dimensions must be positive")
	}
	if p.X < 0 || p.Y < 0 {
		return Point{}, overflowError("coordinate.point", p.X, p.Y)
	}
	switch round {
	case RoundCeil:
		return Point{X: CeilDivPositive(p.X, blockW), Y: CeilDivPositive(p.Y, blockH)}, nil
	default:
		return Point{X: FloorDivPositive(p.X, blockW), Y: FloorDivPositive(p.Y, blockH)}, nil
	}
}

func BlockPointToPixel(p Point, blockW, blockH int, field string) (Point, error) {
	x, err := mulNonNegativeChecked(p.X, blockW, field+".x")
	if err != nil {
		return Point{}, err
	}
	y, err := mulNonNegativeChecked(p.Y, blockH, field+".y")
	if err != nil {
		return Point{}, err
	}
	return Point{X: x, Y: y}, nil
}

func HashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write(p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func HashString(s string) string { return HashBytes([]byte(s)) }
