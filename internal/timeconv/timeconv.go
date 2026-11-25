package timeconv

import (
	"math"

	"replaybench/internal/eerr"
	"replaybench/internal/model"
)

const (
	Floor = "floor"
	Ceil  = "ceil"
)

func NormalizeRounding(v string) (string, error) {
	if v == "" || v == Floor {
		return Floor, nil
	}
	if v == Ceil {
		return Ceil, nil
	}
	return "", eerr.Schema("$.time.rounding", "rounding must be floor or ceil, got %q", v)
}

func RoundToTimestamp(round int, basis model.TimeBasis, field string) (int64, error) {
	if round < 1 {
		return 0, eerr.Schema(field, "internal round must be >= 1, got %d", round)
	}
	if basis.MsPerRound <= 0 {
		return 0, eerr.Schema("$.ms_per_round", "ms_per_round must be positive, got %d", basis.MsPerRound)
	}
	index := int64(round - 1)
	product, err := mulChecked(index, basis.MsPerRound, field, index, basis.MsPerRound)
	if err != nil {
		return 0, err
	}
	if basis.EpochMs > 0 && product > math.MaxInt64-basis.EpochMs {
		return 0, eerr.Ovf(field, product, basis.EpochMs)
	}
	if basis.EpochMs < 0 && product < math.MinInt64-basis.EpochMs {
		return 0, eerr.Ovf(field, product, basis.EpochMs)
	}
	return product + basis.EpochMs, nil
}

func TimestampToRound(ms int64, basis model.TimeBasis, field string) (int, error) {
	rounding, err := NormalizeRounding(basis.Rounding)
	if err != nil {
		return 0, err
	}
	if basis.MsPerRound <= 0 {
		return 0, eerr.Schema("$.ms_per_round", "ms_per_round must be positive, got %d", basis.MsPerRound)
	}
	elapsed, err := subChecked(ms, basis.EpochMs, field)
	if err != nil {
		return 0, err
	}
	if elapsed < 0 {
		return 0, eerr.Schema(field, "timestamp %d precedes epoch %d", ms, basis.EpochMs)
	}
	q, err := divideRounded(elapsed, basis.MsPerRound, rounding, field)
	if err != nil {
		return 0, err
	}
	if q > math.MaxInt-1 {
		return 0, eerr.Ovf(field, q, 1)
	}
	return int(q) + 1, nil
}

func RoundTrip(round int, basis model.TimeBasis, field string) (int64, int, error) {
	ms, err := RoundToTimestamp(round, basis, field)
	if err != nil {
		return 0, 0, err
	}
	back, err := TimestampToRound(ms, basis, field)
	return ms, back, err
}

func mulChecked(a, b int64, field string, errOperands ...any) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		ops := errOperands
		if len(ops) < 2 {
			ops = []any{a, b}
		}
		return 0, eerr.Ovf(field, ops[0], ops[1])
	}
	return a * b, nil
}

func subChecked(a, b int64, field string) (int64, error) {
	if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
		return 0, eerr.Ovf(field, a, b)
	}
	return a - b, nil
}

func divideRounded(a, b int64, rounding, field string) (int64, error) {
	q := a / b
	rem := a % b
	if rounding == Ceil && rem > 0 {
		if q == math.MaxInt64 {
			return 0, eerr.Ovf(field, q, 1)
		}
		q++
	}
	return q, nil
}
