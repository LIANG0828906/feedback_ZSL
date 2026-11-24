package fixedpoint

import "math"

const (
	MinScale int64 = 256
	MaxScale int64 = 1 << 30
)

func DivModFloor(a, b int64) (q, r int64) {
	if b <= 0 {
		panic("fixedpoint: denominator must be positive")
	}
	q = a / b
	r = a % b
	if r < 0 {
		q--
		r += b
	}
	return q, r
}

func DivFloor(a, b int64) int64 {
	q, _ := DivModFloor(a, b)
	return q
}

func RoundNearest(a, den int64) int64 {
	if a >= 0 {
		return DivFloor(2*a+den, 2*den)
	}
	negQ := RoundNearest(-a, den)
	return -negQ
}

func Abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func MaxCode(bitDepth int) int64 {
	if bitDepth >= 63 {
		return math.MaxInt64
	}
	return int64((uint64(1) << bitDepth) - 1)
}

func MilliMean(sumAbs int64, count int) int64 {
	if count == 0 {
		return 0
	}
	return sumAbs * 1000 / int64(count)
}
