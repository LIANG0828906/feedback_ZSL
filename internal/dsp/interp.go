package dsp

import "math"

// SincTable 是离散化的加窗 sinc 核，按小数相位查表，避免热路径重复三角函数。
type SincTable struct {
	halfWin int
	order   float64
	taps    int
	sub     int
	kernel  []float64 // [sub][taps]，taps=2*halfWin
}

// NewSincTable 构建插值核表，sub 为小数相位量化档数（常用 256）。
func NewSincTable(halfWin int, order float64, sub int) *SincTable {
	if sub < 64 {
		sub = 256
	}
	taps := 2 * halfWin
	t := &SincTable{halfWin: halfWin, order: order, taps: taps, sub: sub,
		kernel: make([]float64, sub*taps)}
	for q := 0; q < sub; q++ {
		frac := float64(q) / float64(sub)
		for k := 0; k < taps; k++ {
			d := float64(k-halfWin+1) - frac
			arg := math.Pi * d / order
			var s float64
			if math.Abs(arg) < 1e-12 {
				s = 1
			} else {
				s = math.Sin(arg) / arg
			}
			w := 0.5 * (1 + math.Cos(math.Pi*d/float64(halfWin)))
			t.kernel[q*taps+k] = s * w
		}
	}
	return t
}

// Shift 用查表核在分数位置 pos 处插值。调用方负责边界保证。
func (t *SincTable) Shift(x []float64, pos float64) float64 {
	center := int(math.Floor(pos))
	frac := pos - float64(center)
	q := int(frac * float64(t.sub))
	if q >= t.sub {
		q = t.sub - 1
	}
	base := q * t.taps
	off := center - t.halfWin + 1
	var acc float64
	for k := 0; k < t.taps; k++ {
		acc += x[off+k] * t.kernel[base+k]
	}
	return acc
}

// HalfWindow 返回单边窗长。
func (t *SincTable) HalfWindow() int { return t.halfWin }

// SincShift 保留无表版本用于少量调用与测试。
func SincShift(x []float64, pos float64, halfWin int, order float64) float64 {
	center := int(math.Floor(pos))
	frac := pos - float64(center)
	var acc float64
	for k := -halfWin + 1; k <= halfWin; k++ {
		idx := center + k
		d := float64(k) - frac
		arg := math.Pi * d / order
		var s float64
		if math.Abs(arg) < 1e-12 {
			s = 1
		} else {
			s = math.Sin(arg) / arg
		}
		w := 0.5 * (1 + math.Cos(math.Pi*d/float64(halfWin)))
		acc += x[idx] * s * w
	}
	return acc
}

// InBounds 判断给定分数位置是否能在不越界、不补零的情况下完成插值。
func InBounds(length int64, pos float64, halfWin int) bool {
	return pos-float64(halfWin) >= 0 && pos+float64(halfWin) <= float64(length-1)
}
