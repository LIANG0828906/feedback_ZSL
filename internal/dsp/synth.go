package dsp

import "math"

// ToneAmplitude 特征音调的标准振幅，样本生成与模板合成共用同一常量，
// 保证“素材中嵌入的类别特征音”与“类别模板”来自同一确定性合成路径。
const ToneAmplitude = 0.6

// RenderTone 把一个含多阶谐波的特征音调确定性渲染进 buf 的指定区间
// （start 闭、end 开），并叠加 Hann 包络避免边界爆音。
func RenderTone(buf []float64, start, end int, sampleRate int, baseFreqHz, stepHz float64, partials int) {
	if partials <= 0 {
		partials = 1
	}
	length := end - start
	for n := 0; n < length; n++ {
		env := HannWindow(n, length)
		sample := 0.0
		for p := 0; p < partials; p++ {
			freq := baseFreqHz + stepHz*float64(p)
			sample += math.Sin(2*math.Pi*freq*float64(start+n)/float64(sampleRate)) / float64(partials)
		}
		buf[start+n] += ToneAmplitude * env * sample
	}
}

// HannWindow 返回位置 n（0..length-1）的 Hann 窗系数。
func HannWindow(n, length int) float64 {
	if length <= 1 {
		return 1
	}
	return 0.5 * (1 - math.Cos(2*math.Pi*float64(n)/float64(length-1)))
}

// MeanEnergy 计算采样区间的平均平方能量（越界自动裁剪）。
func MeanEnergy(samples []float64, start, end int) float64 {
	if start < 0 {
		start = 0
	}
	if end > len(samples) {
		end = len(samples)
	}
	if end <= start {
		return 0
	}
	sum := 0.0
	for i := start; i < end; i++ {
		sum += samples[i] * samples[i]
	}
	return sum / float64(end-start)
}
