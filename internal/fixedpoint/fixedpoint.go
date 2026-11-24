package fixedpoint

import (
	"math"
	"math/bits"

	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/model"
)

// 合法定点小数位区间。
const (
	MinScaleBits = 1
	MaxScaleBits = 8
)

// Fix 是一个定点整数，实际值 = Raw / scale。
type Fix struct {
	Raw int64
}

// Converter 在 float64 与定点数之间转换，scale = 10^bits。
type Converter struct {
	bits  int
	scale int64
}

// NewConverter 校验精度参数并构造转换器；越界返回 E_GEO 并指出字段。
func NewConverter(cfg model.GeoConfig) (*Converter, error) {
	if cfg.ScaleBits < MinScaleBits || cfg.ScaleBits > MaxScaleBits {
		return nil, errors.New(errors.EGeo, "scaleBits",
			"scaleBits 超出声明区间 [1,8]")
	}
	scale := int64(1)
	for i := 0; i < cfg.ScaleBits; i++ {
		scale *= 10
	}
	return &Converter{bits: cfg.ScaleBits, scale: scale}, nil
}

// Bits 返回定点小数位。
func (c *Converter) Bits() int { return c.bits }

// Scale 返回缩放系数 10^bits。
func (c *Converter) Scale() int64 { return c.scale }

// FromFloat 把 float64 量化为定点数（半值向上取整，确定性行为）。
func (c *Converter) FromFloat(f float64) Fix {
	if f >= 0 {
		return Fix{Raw: int64(f*float64(c.scale) + 0.5)}
	}
	return Fix{Raw: int64(f*float64(c.scale) - 0.5)}
}

// ToFloat 把定点数还原为 float64。
func (c *Converter) ToFloat(f Fix) float64 {
	return float64(f.Raw) / float64(c.scale)
}

// Add 两个同标度定点数相加。
func Add(a, b Fix) Fix { return Fix{Raw: a.Raw + b.Raw} }

// Sub 两个同标度定点数相减。
func Sub(a, b Fix) Fix { return Fix{Raw: a.Raw - b.Raw} }

// Cmp 比较两个定点数：-1 / 0 / 1。
func Cmp(a, b Fix) int {
	switch {
	case a.Raw < b.Raw:
		return -1
	case a.Raw > b.Raw:
		return 1
	default:
		return 0
	}
}

// i128 是有符号 128 位整数，用于定点乘法与叉积，避免 int64 溢出。
type i128 struct {
	Hi int64
	Lo uint64
}

// I128 是导出的 128 位有符号整数类型别名。
type I128 = i128

func sign64(v int64) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	default:
		return 0
	}
}

// mul128 计算有符号 a*b 的 128 位结果。
func mul128(a, b int64) i128 {
	neg := sign64(a)*sign64(b) < 0
	ua := uint64(a)
	if a < 0 {
		ua = uint64(-a)
	}
	ub := uint64(b)
	if b < 0 {
		ub = uint64(-b)
	}
	hi, lo := bits.Mul64(ua, ub)
	r := i128{Hi: int64(hi), Lo: lo}
	if neg {
		r = neg128(r)
	}
	return r
}

func neg128(v i128) i128 {
	v.Lo = ^v.Lo
	v.Hi = ^v.Hi
	// 加一
	carry := v.Lo
	v.Lo++
	if v.Lo < carry {
		v.Hi++
	}
	return v
}

// sub128 计算 a-b。
func sub128(a, b i128) i128 {
	lo := a.Lo - b.Lo
	borrow := uint64(0)
	if a.Lo < b.Lo {
		borrow = 1
	}
	return i128{Hi: a.Hi - b.Hi - int64(borrow), Lo: lo}
}

// sign 返回 i128 的符号 -1/0/1。
func (v i128) sign() int {
	if v.Hi < 0 {
		return -1
	}
	if v.Hi > 0 || v.Lo > 0 {
		return 1
	}
	return 0
}

// Sign 暴露 i128 符号给几何层使用。
func (v i128) Sign() int { return v.sign() }

// Mul 计算两个定点数的乘积并按标度还原。
func (c *Converter) Mul(a, b Fix) Fix {
	p := mul128(a.Raw, b.Raw)
	// 除以 scale（scale 为 10 的幂，正数且不超过 1e8）
	q := div128ByU64(p, uint64(c.scale))
	return Fix{Raw: q}
}

func div128ByU64(v i128, d uint64) int64 {
	neg := v.sign() < 0
	if neg {
		v = neg128(v)
	}
	// 非负 128 位 / 64 位：先除高 64 位，余数与低 64 位合并再除。
	qHi := uint64(v.Hi) / d
	rHi := uint64(v.Hi) % d
	hi, _ := bits.Div64(uint64(rHi), v.Lo, d)
	q := hi + qHi
	if neg {
		return -int64(q)
	}
	return int64(q)
}

// Cross 计算向量 (b-a) 与 (c-a) 的叉积，返回 128 位有符号结果。
func Cross(ax, ay, bx, by, cx, cy Fix) i128 {
	abx := Sub(bx, ax).Raw
	aby := Sub(by, ay).Raw
	acx := Sub(cx, ax).Raw
	acy := Sub(cy, ay).Raw
	return sub128(mul128(abx, acy), mul128(aby, acx))
}

// Mul128 暴露 128 位有符号乘法，供无除法的比较运算使用。
func Mul128(a, b int64) i128 { return mul128(a, b) }

// Hypot 返回两个定点坐标差对应的欧氏长度（float64，IEEE 确定性）。
func (c *Converter) Hypot(dx, dy Fix) float64 {
	return math.Hypot(c.ToFloat(dx), c.ToFloat(dy))
}
