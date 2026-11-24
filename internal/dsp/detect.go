package dsp

import "math"

// DetectMarker 在搜索窗 [lo,hi]（整数索引）内用模板匹配定位标记，并做抛物线亚采样细化。
// 返回分数到达位置与归一化相关峰能量；未找到显著峰时 found=false。
func DetectMarker(x, tpl []float64, lo, hi int) (pos float64, peak float64, found bool) {
	w := len(tpl) / 2
	if lo < w {
		lo = w
	}
	if hi > len(x)-w-1 {
		hi = len(x) - w - 1
	}
	if hi-lo < 2 {
		return 0, 0, false
	}
	bestI := lo
	var bestV float64
	for i := lo; i <= hi; i++ {
		var c float64
		for k := -w; k <= w; k++ {
			c += x[i+k] * tpl[k+w]
		}
		if c > bestV {
			bestV = c
			bestI = i
		}
	}
	if bestI <= lo || bestI >= hi {
		return float64(bestI), bestV, bestV > 0
	}
	// 相邻三点抛物线亚采样细化。
	c0 := corrAt(x, tpl, bestI-1)
	c1 := bestV
	c2 := corrAt(x, tpl, bestI+1)
	den := c0 - 2*c1 + c2
	delta := 0.0
	if math.Abs(den) > 1e-18 {
		delta = 0.5 * (c0 - c2) / den
		if delta > 1 || delta < -1 {
			delta = 0
		}
	}
	return float64(bestI) + delta, bestV, bestV > 0
}

func corrAt(x, tpl []float64, i int) float64 {
	w := len(tpl) / 2
	var c float64
	for k := -w; k <= w; k++ {
		c += x[i+k] * tpl[k+w]
	}
	return c
}

// HasClip 判断序列是否存在超过阈值的削顶（相邻采样幅值饱和且接近）。
func HasClip(x []float64, threshold float64) (int64, bool) {
	for i := 1; i < len(x); i++ {
		if math.Abs(x[i]) >= threshold && math.Abs(x[i-1]) >= threshold && math.Abs(x[i]-x[i-1]) < 1e-9 {
			return int64(i), true
		}
	}
	return -1, false
}
