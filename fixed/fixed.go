// Package fixed provides overflow-checked integer arithmetic and the
// fixed-point scale conversions used by the economy kernel.
package fixed

import (
	"errors"
	"math"
	"math/big"
)

const (
	// RateScale is the internal scale for production rates. 10000 means one
	// batch per logical cycle.
	RateScale int64 = 10000
	// WorkerScale is the internal scale for per-worker yield. 10000 means one
	// nominal worker contributes 1.0.
	WorkerScale int64 = 10000
	// UnitScale is used for resource-per-cycle rates in public snapshots.
	UnitScale int64 = 10000
)

// RoundMode controls integer division when a value is represented at a
// coarser scale or a shorter period.
type RoundMode string

const (
	RoundFloor   RoundMode = "floor"
	RoundCeil    RoundMode = "ceil"
	RoundNearest RoundMode = "nearest"
)

var ErrOverflow = errors.New("fixed: integer overflow")

// ParseRoundMode parses a schema-declared rounding direction. Empty input
// defaults to floor.
func ParseRoundMode(s string) (RoundMode, error) {
	switch RoundMode(s) {
	case "", RoundFloor:
		return RoundFloor, nil
	case RoundCeil:
		return RoundCeil, nil
	case RoundNearest:
		return RoundNearest, nil
	default:
		return "", errors.New("fixed: unknown rounding mode " + s)
	}
}

// Add returns a+b and reports overflow.
func Add(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, ErrOverflow
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}

// Sub returns a-b and reports overflow.
func Sub(a, b int64) (int64, error) {
	if b < 0 && a > math.MaxInt64+b {
		return 0, ErrOverflow
	}
	if b > 0 && a < math.MinInt64+b {
		return 0, ErrOverflow
	}
	return a - b, nil
}

// Mul returns a*b and reports overflow.
func Mul(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !product.IsInt64() {
		return 0, ErrOverflow
	}
	return product.Int64(), nil
}

// MulDiv computes (a*b)/den with the requested rounding direction. It uses
// big.Int for the intermediate product so the operation is exact unless the
// final result itself does not fit in int64.
func MulDiv(a, b, den int64, mode RoundMode) (int64, error) {
	if den == 0 {
		return 0, errors.New("fixed: division by zero")
	}
	numerator := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	denominator := big.NewInt(den)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)

	if remainder.Sign() != 0 {
		negative := numerator.Sign()*denominator.Sign() < 0
		switch mode {
		case "", RoundFloor:
			if negative {
				quotient.Sub(quotient, big.NewInt(1))
			}
		case RoundCeil:
			if !negative {
				quotient.Add(quotient, big.NewInt(1))
			}
		case RoundNearest:
			absRem := new(big.Int).Abs(remainder)
			absDen := new(big.Int).Abs(denominator)
			twice := new(big.Int).Lsh(absRem, 1)
			if twice.Cmp(absDen) >= 0 {
				if negative {
					quotient.Sub(quotient, big.NewInt(1))
				} else {
					quotient.Add(quotient, big.NewInt(1))
				}
			}
		default:
			return 0, errors.New("fixed: unknown rounding mode " + string(mode))
		}
	}
	if !quotient.IsInt64() {
		return 0, ErrOverflow
	}
	return quotient.Int64(), nil
}

// ScaleTo converts an integer measured in from units into to units.
func ScaleTo(value, from, to int64, mode RoundMode) (int64, error) {
	if from <= 0 || to <= 0 {
		return 0, errors.New("fixed: non-positive scale")
	}
	return MulDiv(value, to, from, mode)
}

// PeriodToMs converts a duration measured in unit into milliseconds.
func PeriodToMs(value int64, millisecondsPerUnit int64) (int64, error) {
	if value < 0 || millisecondsPerUnit <= 0 {
		return 0, errors.New("fixed: invalid period")
	}
	return Mul(value, millisecondsPerUnit)
}

// PeriodFromMs converts milliseconds to a period unit using mode.
func PeriodFromMs(ms, millisecondsPerUnit int64, mode RoundMode) (int64, error) {
	if ms < 0 || millisecondsPerUnit <= 0 {
		return 0, errors.New("fixed: invalid period")
	}
	return MulDiv(ms, 1, millisecondsPerUnit, mode)
}

// Split applies a basis-point percentage to a value, e.g. 1000bp is 10%.
func Split(value, basisPoints int64, mode RoundMode) (int64, error) {
	if basisPoints < 0 || basisPoints > RateScale {
		return 0, errors.New("fixed: basis points out of range")
	}
	return MulDiv(value, basisPoints, RateScale, mode)
}

// ApplyDiscount returns value reduced by discount basis points.
func ApplyDiscount(value, discountBasisPoints int64) (int64, error) {
	if discountBasisPoints < 0 || discountBasisPoints > RateScale {
		return 0, errors.New("fixed: discount out of range")
	}
	discount, err := Split(value, discountBasisPoints, RoundFloor)
	if err != nil {
		return 0, err
	}
	return Sub(value, discount)
}
