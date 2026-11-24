package model

import "errors"

// Milli 表示毫单位定点数：1 个游戏单位 = 1000 milli。
// 全部生命/资源/技能数值均以 int64 milli 累加，全程不使用浮点。
const Milli int64 = 1000

// 声明的安全边界与溢出边界。
// SafeLimit 是领域允许的最大绝对值；MaxInt64/MinInt64 是物理溢出边界。
const (
	SafeLimit int64 = 1_000_000_000_000_000 // 10^15 milli
	MaxInt64        = int64(^uint64(0) >> 1)
	MinInt64        = -MaxInt64 - 1
)

var ErrOverflow = errors.New("fixed-point overflow: int64 boundary exceeded")

// AddChecked 执行带物理溢出检查的整数加法。
func AddChecked(a, b int64) (int64, error) {
	if b > 0 && a > MaxInt64-b {
		return 0, ErrOverflow
	}
	if b < 0 && a < MinInt64-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}

// MulChecked 执行带物理溢出检查的整数乘法。
func MulChecked(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	r := a * b
	if r/b != a {
		return 0, ErrOverflow
	}
	return r, nil
}

// DivMilli 计算 (a*b)/Milli，先乘后除并带溢出检查；用于暴击等定点缩放。
func ScaleMilli(a, numerator int64) (int64, error) {
	p, err := MulChecked(a, numerator)
	if err != nil {
		return 0, err
	}
	return p / Milli, nil
}

// Clamp 把值收敛到 [lo, hi]，并校验安全边界声明。
func Clamp(v, lo, hi int64) int64 {
	if lo > hi {
		lo, hi = hi, lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// WithinSafeLimit 声明领域安全边界校验。
func WithinSafeLimit(v int64) bool {
	return v >= -SafeLimit && v <= SafeLimit
}
