package dsp

import "math"

// MarkerTemplate 生成对齐标记模板，中心位于索引 Width，长度 2*Width+1。
// shape:
//
//	"pulse": Hann 加窗 sinc 脉冲（带限、零均值化处理）
//	"chirp": Hann 加窗线性扫频（频率在窗内线性扫过 0.06~0.30 归一化频率）
func MarkerTemplate(shape string, width int) []float64 {
	if width < 2 {
		width = 2
	}
	tpl := make([]float64, 2*width+1)
	switch shape {
	case "chirp":
		const f0, f1 = 0.06, 0.30
		phase := 0.0
		for k := 0; k <= 2*width; k++ {
			u := float64(k-width) / float64(width) // [-1,1]
			f := f0 + (f1-f0)*(u+1)*0.5
			phase += 2 * math.Pi * f
			w := 0.5 * (1 + math.Cos(math.Pi*u))
			tpl[k] = w * math.Sin(phase)
		}
	default: // pulse
		for k := 0; k <= 2*width; k++ {
			u := float64(k - width)
			x := math.Pi * u / (0.85 * float64(width))
			var s float64
			if math.Abs(u) < 1e-12 {
				s = 1
			} else {
				s = math.Sin(x) / x
			}
			w := 0.5 * (1 + math.Cos(math.Pi*u/float64(width)))
			tpl[k] = w * s
		}
	}
	normalizePeak(tpl)
	return tpl
}

// normalizePeak 把模板峰值归一到 1，保证叠加幅度可控、匹配滤波仍具相干增益。
func normalizePeak(a []float64) {
	var mx float64
	for _, v := range a {
		if av := math.Abs(v); av > mx {
			mx = av
		}
	}
	if mx == 0 {
		return
	}
	for i := range a {
		a[i] /= mx
	}
}

// HannWindow 返回长度 n 的 Hann 窗。
func HannWindow(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = 0.5 * (1 - math.Cos(2*math.Pi*float64(i)/float64(n-1)))
	}
	return w
}
