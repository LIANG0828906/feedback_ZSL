package fixed

import (
	"fmt"
	"math"

	"replaybench/internal/eerr"
)

const (
	Scale     int64 = 1000
	MaxWhole  int64 = 1_000_000_000
	MaxPoints int64 = MaxWhole * Scale
	MaxEffect int64 = 100_000_000 * Scale
	MinPoints int64 = 0
)

type P int64

func FromWhole(v int64, field string) (P, error) {
	if v < 0 || v > MaxWhole {
		return 0, eerr.Ovf(field, v, MaxWhole)
	}
	p, err := MulChecked(v, Scale, field)
	if err != nil {
		return 0, err
	}
	if p > MaxPoints {
		return 0, eerr.Ovf(field, v, Scale)
	}
	return P(p), nil
}

func MustWhole(v int64) P {
	p, err := FromWhole(v, "must_whole")
	if err != nil {
		panic(err)
	}
	return p
}

func MulChecked(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, eerr.Ovf(field, a, b)
	}
	return a * b, nil
}

func AddChecked(a, b P, field string) (P, error) {
	if b > 0 && int64(a) > math.MaxInt64-int64(b) {
		return 0, eerr.Ovf(field, a, b)
	}
	if b < 0 && int64(a) < math.MinInt64-int64(b) {
		return 0, eerr.Ovf(field, a, b)
	}
	r := a + b
	if r < P(-MaxPoints) || r > P(MaxPoints) {
		return 0, eerr.Ovf(field, a, b)
	}
	return r, nil
}

func Clamp(v, lo, hi P) P {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func Whole(p P) int64 {
	return int64(p) / Scale
}

func String(p P) string {
	whole := int64(p) / Scale
	frac := int64(p) % Scale
	if frac < 0 {
		frac = -frac
	}
	if frac == 0 {
		return fmt.Sprintf("%d", whole)
	}
	return fmt.Sprintf("%d.%03d", whole, frac)
}
