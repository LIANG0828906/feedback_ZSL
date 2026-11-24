// Package renderer 按分帧重建参数把脉冲响应渲染回多声道信号。
// 渲染输出与原信号采样点数严格一致，帧拥有区间之外不写任何采样（不补零越界）。
package renderer

import (
	"math"

	"spatialir/internal/analyzer"
	"spatialir/internal/model"
)

// RenderFrame 渲染单帧拥有区间 [fr.Start, fr.End) 的贡献。
// 返回 channels 个长度等于原信号长度的向量，非拥有区间保持 0，
// 便于多帧按帧序做确定性叠加。
func RenderFrame(ds *analyzer.Dataset, fr analyzer.Frame, estimates []model.ChannelEstimate,
	p model.FrameParams, layoutMap map[string]string) ([][]float64, error) {
	renderBlock := p.RenderBlock
	if renderBlock <= 0 {
		renderBlock = fr.End - fr.Start
	}
	ch := ds.Manifest.Layout.Channels
	n := ds.Manifest.LengthSamples
	out := make([][]float64, ch)
	for i := range out {
		out[i] = make([]float64, n)
	}
	for srcIdx, est := range estimates {
		dstIdx := srcIdx
		if target, ok := layoutMap[est.Channel]; ok && target != "" {
			for idx, name := range ds.Manifest.Layout.Names {
				if name == target {
					dstIdx = idx
					break
				}
			}
		}
		dry := ds.Dry[srcIdx]
		// 离散事件（直达+反射）：以干声片段为卷积核，窗长受 ConvWindow 限制。
		// 写入按 RenderBlock 分块切片（块大小越界已在参数校验阶段返回 E_PARA），
		// 事件只驱动一次卷积，块仅决定写入批次，保证结果与块大小无关。
		for _, ev := range est.Events {
			if ev.Sample < fr.Start || ev.Sample >= fr.End {
				continue
			}
			for blockStart := fr.Start; blockStart < fr.End; blockStart += renderBlock {
				blockEnd := min(fr.End, blockStart+renderBlock)
				convolveEventBlock(out[dstIdx], dry, ev.Sample, ev.Amplitude,
					fr, blockStart, blockEnd, p.ConvWindow)
			}
		}
		// 指数衰减尾音：直接按拟合参数在拥有区间内解析渲染。
		if est.TailAmp > 0 {
			start := max(fr.Start, est.TailOnset)
			for blockStart := start; blockStart < fr.End; blockStart += renderBlock {
				blockEnd := min(fr.End, blockStart+renderBlock)
				for i := blockStart; i < blockEnd; i++ {
					out[dstIdx][i] += est.TailAmp * math.Exp(-est.TailRate*float64(i-est.TailOnset))
				}
			}
		}
	}
	return out, nil
}

func convolveEventBlock(dst, dry []float64, eventSample int, amp float64,
	fr analyzer.Frame, blockStart, blockEnd, convWindow int) {
	n := len(dst)
	// 只遍历与当前块相交的卷积核区间，避免逐块重复整个卷积。
	kStart := blockStart - eventSample
	if kStart < 0 {
		kStart = 0
	}
	kEnd := blockEnd - eventSample
	if kEnd > convWindow {
		kEnd = convWindow
	}
	for k := kStart; k < kEnd; k++ {
		di := eventSample + k
		if di >= n {
			break
		}
		if k >= len(dry) || di < fr.Start || di >= fr.End || di < blockStart || di >= blockEnd {
			// 卷积尾巴跨帧时只写拥有区间，其余由事件所在帧之外的确定性归属规则处理：
			// 事件所在帧拥有从事件点到帧尾的全部卷积能量，跨帧部分丢弃以保证
			// 三条路径逐样本一致且边界不补零。
			continue
		}
		if k >= len(dry) {
			break
		}
		dst[di] += amp * dry[k]
	}
}

// InterpolationResidual 度量在非整数延迟 frac（0<=frac<1）下，直接解析采样
// 与选定插值阶数重建之间的最大核近似残差。整数延迟（frac=0）残差为 0。
// 该函数只依赖插值阶数与量化步长，供误差报告引用。
func InterpolationResidual(order int, frac float64) float64 {
	if frac <= 0 || frac >= 1 {
		return 0
	}
	// 以单位冲激核在偏移 frac 处的取值评估：阶数越高，核越逼近理想 sinc。
	switch order {
	case 0: // 最近邻
		return frac
	case 1: // 线性
		return frac * (1 - frac)
	case 3: // 三次：局部误差为线性情形的二阶小量
		return frac * frac * (1 - frac) * (1 - frac)
	default:
		return frac
	}
}

// Interpolate 按配置阶数把干声核放置到（可能非整数的）延迟位置。
// 当前样本生成器只产出整数到达点，因此常规路径 frac=0，残差结构性为 0；
// 预留非整数路径供外部参数化场景使用。
func Interpolate(dst, dry []float64, delaySamples float64, amp float64, fr analyzer.Frame, convWindow, order int) {
	base := int(math.Floor(delaySamples))
	frac := delaySamples - float64(base)
	n := len(dst)
	for k := 0; k < convWindow; k++ {
		di := base + k
		if di < fr.Start || di >= fr.End || di >= n || k >= len(dry) {
			continue
		}
		v := amp * dry[k]
		switch {
		case frac == 0 || order == 0:
			dst[di] += v
		case order == 1 && di+1 < n && di+1 < fr.End:
			dst[di] += v * (1 - frac)
			dst[di+1] += v * frac
		case order == 3 && di+1 < n && di+1 < fr.End:
			dst[di] += v * (1 - frac*frac*(3-2*frac))
			dst[di+1] += v * (frac * frac * (3 - 2*frac))
		default:
			dst[di] += v
		}
	}
}

// Merge 按帧序把逐帧贡献叠加为完整信号，加法顺序固定，保证浮点结果路径无关。
func Merge(frames [][][]float64, channels, length int) [][]float64 {
	out := make([][]float64, channels)
	for c := 0; c < channels; c++ {
		out[c] = make([]float64, length)
	}
	for _, frame := range frames {
		for c := 0; c < channels; c++ {
			for i := 0; i < length; i++ {
				out[c][i] += frame[c][i]
			}
		}
	}
	return out
}

// EnergyStats 计算帧拥有区间的原始/渲染能量（总量与逐通道）。
func EnergyStats(original, rendered [][]float64, fr analyzer.Frame) model.FrameEnergy {
	ch := len(original)
	st := model.FrameEnergy{
		PerChannel:     make([]float64, ch),
		PerChannelRend: make([]float64, ch),
	}
	for c := 0; c < ch; c++ {
		for i := fr.Start; i < fr.End; i++ {
			st.PerChannel[c] += original[c][i] * original[c][i]
			st.PerChannelRend[c] += rendered[c][i] * rendered[c][i]
		}
		st.OriginalEnergy += st.PerChannel[c]
		st.RenderEnergy += st.PerChannelRend[c]
	}
	return st
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
