package convert

import (
	"math"
	"strconv"

	"battlekernel/internal/errs"
)

type Direction string

const (
	RoundFloor Direction = "floor"
	RoundCeil  Direction = "ceil"
)

func checkedMul(a, b int64, field string, original any) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a == -1 && b == math.MinInt64 {
		return 0, overflow(field, a, b)
	}
	if b == -1 && a == math.MinInt64 {
		return 0, overflow(field, a, b)
	}
	if (a == math.MinInt64 && b != 1) || (b == math.MinInt64 && a != 1) {
		return 0, overflow(field, a, b)
	}
	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return 0, overflow(field, a, b)
	}
	if a < 0 && b < 0 {
		aa := -a
		bb := -b
		if aa > math.MaxInt64/bb {
			return 0, overflow(field, a, b)
		}
	}
	if a > 0 && b < 0 {
		if b < -math.MaxInt64/a {
			return 0, overflow(field, a, b)
		}
	}
	if a < 0 && b > 0 {
		if a < -math.MaxInt64/b {
			return 0, overflow(field, a, b)
		}
	}
	product := a * b
	if product == 0 && a != 0 && b != 0 {
		return 0, (&errs.Error{
			Code:    errs.EOVF,
			Field:   field,
			Values:  []string{intToString(a), intToString(b)},
			Message: "integer multiplication overflow",
		})
	}
	return product, nil
}

func checkedAdd(a, b int64, field string) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, overflow(field, a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, overflow(field, a, b)
	}
	return a + b, nil
}

func overflow(field string, a, b int64) error {
	return &errs.Error{
		Code:    errs.EOVF,
		Field:   field,
		Values:  []string{intToString(a), intToString(b)},
		Message: "integer conversion overflow",
	}
}

func intToString(v int64) string {
	return strconv.FormatInt(v, 10)
}

func RoundIndexToMillis(round, startRound, epochMs, tickMs int64, field string) (int64, error) {
	if tickMs <= 0 {
		return 0, errs.New(errs.ESchema, field, "tick_ms must be positive, got %d", tickMs)
	}
	delta, err := checkedSub(round, startRound, field)
	if err != nil {
		return 0, err
	}
	offset, err := checkedMul(delta, tickMs, field, nil)
	if err != nil {
		return 0, err
	}
	return checkedAdd(epochMs, offset, field)
}

func checkedSub(a, b int64, field string) (int64, error) {
	if b > 0 && a < math.MinInt64+b {
		return 0, overflow(field, a, b)
	}
	if b < 0 && a > math.MaxInt64+b {
		return 0, overflow(field, a, b)
	}
	return a - b, nil
}

func MillisToRoundIndex(timestampMs, startRound, epochMs, tickMs int64, dir Direction, field string) (int64, error) {
	if tickMs <= 0 {
		return 0, errs.New(errs.ESchema, field, "tick_ms must be positive, got %d", tickMs)
	}
	elapsed, err := checkedSub(timestampMs, epochMs, field)
	if err != nil {
		return 0, err
	}
	quot, rem := floorDivMod(elapsed, tickMs)
	if dir == RoundCeil && rem != 0 {
		quot++
		if quot < 0 && startRound > math.MaxInt64+quot {
			return 0, overflow(field, startRound, quot)
		}
	}
	return checkedAdd(startRound, quot, field)
}

func floorDivMod(a, b int64) (int64, int64) {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
		r += b
	}
	return q, r
}

func CanonicalRoundTrip(timestampMs, startRound, epochMs, tickMs int64, dir Direction, field string) (round int64, canonicalMs int64, err error) {
	round, err = MillisToRoundIndex(timestampMs, startRound, epochMs, tickMs, dir, field)
	if err != nil {
		return 0, 0, err
	}
	canonicalMs, err = RoundIndexToMillis(round, startRound, epochMs, tickMs, field)
	return round, canonicalMs, err
}
