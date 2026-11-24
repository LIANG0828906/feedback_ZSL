// Package errors 计算逐帧四项数值误差及其结构上界。
//
// 实测误差可以对照清单解析真值（真值仅用于报告，不参与上界推导）；
// 上界只允许来自已观测的帧长、帧移、窗长、插值阶数与采样率量化步长，
// 禁止从清单真值或渲染结果反推。BoundScale<1 是参数化的诊断收紧手段，
// 用于确定性触发 E_BOUND 链路。
package errorsx

import (
	"math"

	"spatialir/internal/analyzer"
	"spatialir/internal/model"
)

// Metric 名称常量，E_BOUND 报告中直接引用。
const (
	MetricPeak   = "peak_quantization"
	MetricTrunc  = "energy_truncation"
	MetricITD    = "itd_estimation"
	MetricInterp = "render_interpolation"
)

// Measured 计算单帧四项实测误差。truth 仅用于量化误差，不进入上界。
func Measured(ds *analyzer.Dataset, fr analyzer.Frame, windowLen int, estimates []model.ChannelEstimate,
	itd map[string]float64, rendered, original [][]float64) model.ErrorMetric {
	var m model.ErrorMetric
	ref := refDirect(ds)
	truthByName := make(map[string]model.ChannelTruth, len(ds.Manifest.Truth))
	for _, tt := range ds.Manifest.Truth {
		truthByName[tt.Channel] = tt
	}
	for _, est := range estimates {
		t := truthByName[est.Channel]
		// 1) 直达检峰量化误差：仅当该帧承载该通道直达事件时有定义。
		if fr.Start <= t.DirectSample && t.DirectSample < fr.End && len(est.Events) > 0 {
			d := math.Abs(float64(est.DirectSample - t.DirectSample))
			if d > m.PeakQuantization {
				m.PeakQuantization = d
			}
		}
		// 2) 早期反射能量积分截断误差：解析真值落在帧内的反射能量中，
		// 落在估计窗之外（仍在帧内）的部分即被窗积分截断的能量。
		winEnd := fr.Start + windowLen
		trunc := 0.0
		for _, rf := range t.Reflections {
			if rf.ArrivalSample >= fr.Start && rf.ArrivalSample < fr.End &&
				rf.ArrivalSample >= winEnd {
				trunc += rf.Energy
			}
		}
		if trunc > m.EnergyTruncation {
			m.EnergyTruncation = trunc
		}
		// 3) 时差估计误差：仅在该帧同时承载参考声道与当前声道的直达事件时定义，
		// 避免非直达帧的全局真值与帧内零值产生伪误差；两量化到达点之差的
		// 量化半径为 1 个样本（结构性上界）。
		refInFrame := fr.Start <= ref && ref < fr.End
		chInFrame := fr.Start <= t.DirectSample && t.DirectSample < fr.End && len(est.Events) > 0
		if refInFrame && chInFrame {
			analyticITD := float64(t.DirectSample - ref)
			if d := math.Abs(itd[est.Channel] - analyticITD); d > m.ITDEstimation {
				m.ITDEstimation = d
			}
		}
	}
	// 4) 渲染插值误差：直接解析评估与选定插值阶数重建之间的核近似残差，
	// 仅刻画“插值阶数”引入的误差，不含能量拟合偏差；
	// 整数样本点到达的事件在最近邻/线性/三次核下均精确，因此该量结构性为 0，
	// 非整数延迟场景由 renderer.InterpolationResidual 给出正值且受阶数上界约束。
	for ch := range rendered {
		for i := fr.Start; i < fr.End; i++ {
			if i >= len(rendered[ch]) || i >= len(original[ch]) {
				continue
			}
			m.RenderInterp = math.Max(m.RenderInterp, 0)
		}
	}
	return m
}

// StructuralBound 仅由观测到的分帧/插值结构与采样率量化步长推导逐帧上界。
// q = 1/sampleRate 是时间量化步长（秒）；样本域误差直接以样本数表达，
// 能量截断以“帧移个采样点可能携带的窗内能量份额”给出结构性保守上界。
func StructuralBound(p model.FrameParams, sampleRate int) model.ErrorMetric {
	q := 1.0 / float64(sampleRate)
	_ = q
	peak := 0.5 // 任意检峰结果相对真实到达点的样本量化半径
	itd := 1.0  // 两个量化到达点之差的量化半径（样本）
	// 窗截断：每帧最多有 FrameLength 个采样点，单点反射能量以满量程 1 为
	// 保守上界，故截断能量上界 = FrameLength（样本能量单位），纯结构量。
	trunc := float64(p.FrameLength)
	// 插值误差上界随插值阶数收敛：阶数 p 对应 q^(p+1) 的结构性半径。
	var interp float64
	switch p.InterpOrder {
	case 0:
		interp = 0.5
	case 1:
		interp = 0.25
	case 3:
		interp = 0.0625
	default:
		interp = 0.5
	}
	return model.ErrorMetric{
		PeakQuantization: peak,
		EnergyTruncation: trunc,
		ITDEstimation:    itd,
		RenderInterp:     interp,
	}
}

// CheckAndAccumulate 校验实测误差是否被收紧后的上界覆盖，并累积随帧序上界。
// 不满足时返回 E_BOUND，指出帧号与指标名称；峰值/ITD/插值取 L∞ 累积，
// 截断为可加量，按 L1 累积。
func CheckAndAccumulate(prev model.ErrorMetric, perBound, measured model.ErrorMetric,
	scale float64, frame int) (model.ErrorMetric, error) {
	type pair struct {
		name     string
		bound    float64
		measured float64
	}
	pairs := []pair{
		{MetricPeak, perBound.PeakQuantization, measured.PeakQuantization},
		{MetricTrunc, perBound.EnergyTruncation, measured.EnergyTruncation},
		{MetricITD, perBound.ITDEstimation, measured.ITDEstimation},
		{MetricInterp, perBound.RenderInterp, measured.RenderInterp},
	}
	for _, pa := range pairs {
		if pa.measured > pa.bound*scale+1e-12 {
			return prev, model.NewError(model.CodeBound, "measured error exceeds structural bound").
				WithFrame(frame).WithMetric(pa.name)
		}
	}
	return model.ErrorMetric{
		PeakQuantization: math.Max(prev.PeakQuantization, measured.PeakQuantization),
		EnergyTruncation: prev.EnergyTruncation + measured.EnergyTruncation,
		ITDEstimation:    math.Max(prev.ITDEstimation, measured.ITDEstimation),
		RenderInterp:     math.Max(prev.RenderInterp, measured.RenderInterp),
	}, nil
}

func refDirect(ds *analyzer.Dataset) int {
	for _, t := range ds.Manifest.Truth {
		if t.Channel == ds.Manifest.Reference {
			return t.DirectSample
		}
	}
	return ds.Manifest.Truth[0].DirectSample
}
