// Package analyzer 对多通道脉冲响应做分帧声学分析：
// 直达延迟检峰、早期反射能量积分、尾音指数拟合，并按帧触发输入/参数注入的异常。
package analyzer

import (
	"math"

	"spatialir/internal/model"
)

const (
	// PeakThreshold 是检峰阈值（相对帧内最大幅值），低于该值视为无离散事件。
	PeakThreshold = 0.08
	// ClipLevel 是 PCM 归一化满量程，叠加结果越界即 E_CLIP。
	ClipLevel = 1.0
)

// Frame 描述一个不补零的完整帧的样本区间 [Start, End)。
type Frame struct {
	Index int
	Start int
	End   int
}

// BuildFrames 按帧长/帧移切分，只保留完整落在信号内的帧，起止边界不补零。
func BuildFrames(length int, p model.FrameParams) []Frame {
	var frames []Frame
	for start := 0; start+p.FrameLength <= length; start += p.FrameHop {
		frames = append(frames, Frame{Index: len(frames), Start: start, End: start + p.FrameLength})
	}
	return frames
}

// AnalyzeFrame 对单帧所有通道做估计。返回的错误均为 *model.AppError，
// 帧内异常只使该帧失败（E_ORDER/E_CLIP/E_FRAME），不影响其他帧。
func AnalyzeFrame(ds *Dataset, fr Frame, p model.FrameParams) ([]model.ChannelEstimate, error) {
	faults := ds.Manifest.Faults
	for _, bad := range faults.BadFrames {
		if bad == fr.Index {
			return nil, model.NewError(model.CodeFrame, "injected bad frame").WithFrame(fr.Index)
		}
	}
	out := make([]model.ChannelEstimate, 0, ds.Manifest.Layout.Channels)
	for ch, name := range ds.Manifest.Layout.Names {
		est, err := analyzeChannel(ds, fr, p, ch, name)
		if err != nil {
			return nil, err
		}
		out = append(out, est)
	}
	// E_ORDER：反射到达点倒置，按帧+通道标识。
	if faults.OrderFrame == fr.Index && faults.OrderChannel != "" {
		if hasChannel(ds.Manifest, faults.OrderChannel) {
			return nil, model.NewError(model.CodeOrder, "injected reflection arrival inversion").
				WithFrame(fr.Index).WithChannel(faults.OrderChannel)
		}
	}
	return out, nil
}

func analyzeChannel(ds *Dataset, fr Frame, p model.FrameParams, ch int, name string) (model.ChannelEstimate, error) {
	sig := ds.Signals[ch]
	est := model.ChannelEstimate{Channel: name}

	// E_CLIP：注入增益后帧内采样越过满量程。
	gain := 1.0
	if ds.Manifest.Faults.ClipFrame == fr.Index && ds.Manifest.Faults.ClipChannel == name {
		gain = ds.Manifest.Faults.ClipGain
	}
	if gain != 1.0 {
		for i := fr.Start; i < fr.End; i++ {
			if math.Abs(sig[i]*gain) > ClipLevel {
				return est, model.NewError(model.CodeClip, "injected reflection gain overflow").
					WithFrame(fr.Index).WithChannel(name)
			}
		}
	}

	peakAmp := 0.0
	for i := fr.Start; i < fr.End; i++ {
		if a := math.Abs(sig[i]); a > peakAmp {
			peakAmp = a
		}
	}
	thr := PeakThreshold * peakAmp
	var events []model.Event
	var prevSample int
	first := true
	for i := fr.Start; i < fr.End; i++ {
		if math.Abs(sig[i]) >= thr && sig[i] > 0 && isLocalPeak(sig, i, fr.Start, fr.End) {
			ev := model.Event{Sample: i, Amplitude: sig[i], Energy: sig[i] * sig[i]}
			if first {
				ev.IsDirect = true
				est.DirectSample = i
				est.DirectEnergy = ev.Energy
				first = false
			} else if i < prevSample {
				// 自然信号不会触发；保留防御性倒置检查。
				return est, model.NewError(model.CodeOrder, "reflection arrivals not monotonic").
					WithFrame(fr.Index).WithChannel(name)
			}
			events = append(events, ev)
			prevSample = i
		}
	}
	est.Events = events

	// 早期反射能量：以估计窗长做窗内积分；窗内/全帧两口径都保留，
	// 截断误差由二者之差在 errors 包中量化。
	earlyFull, earlyWin := 0.0, 0.0
	for _, ev := range events {
		if ev.IsDirect {
			continue
		}
		earlyFull += ev.Energy
		if ev.Sample < fr.Start+p.WindowLen {
			earlyWin += ev.Energy
		}
	}
	est.EarlyEnergy = earlyWin

	// 尾音区域能量与指数拟合（log 线性最小二乘）。
	tailOnset, tailAmp, tailRate, reverbEnergy := fitTail(sig, fr)
	est.TailOnset = tailOnset
	est.TailAmp = tailAmp
	est.TailRate = tailRate
	est.ReverbEnergy = reverbEnergy

	// 供误差模块使用的窗截断参考量挂在 ReverbEnergy 之外由统计层重算，
	// 这里同时保留全帧早期能量到 DryReverbRatio 的稳定分母。
	if reverbEnergy > 0 {
		est.DryReverbRatio = est.DirectEnergy / reverbEnergy
	} else {
		est.DryReverbRatio = est.DirectEnergy
	}
	// earlyFull 通过事件可在统计层重算，无需额外字段。
	_ = earlyFull
	return est, nil
}

func isLocalPeak(sig []float64, i, start, end int) bool {
	if i > start && sig[i] < sig[i-1] {
		return false
	}
	if i < end-1 && sig[i] < sig[i+1] {
		return false
	}
	return true
}

// fitTail 在帧内尾音候选区（越过最后离散峰之后）拟合 exp 衰减。
func fitTail(sig []float64, fr Frame) (onset int, amp, rate, energy float64) {
	lastPeak := fr.Start
	for i := fr.Start; i < fr.End; i++ {
		if sig[i] > 0.02 && isLocalPeak(sig, i, fr.Start, fr.End) {
			lastPeak = i
		}
	}
	onset = lastPeak + 1
	if onset >= fr.End || fr.End-onset < 8 {
		return onset, 0, 0, tailEnergy(sig, onset, fr.End)
	}
	var sx, sy, sxx, sxy float64
	k := 0
	for i := onset; i < fr.End; i++ {
		v := sig[i]
		if v <= 1e-9 {
			continue
		}
		x := float64(i - onset)
		y := math.Log(v)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
		k++
	}
	if k < 8 {
		return onset, 0, 0, tailEnergy(sig, onset, fr.End)
	}
	den := float64(k)*sxx - sx*sx
	if math.Abs(den) < 1e-12 {
		return onset, 0, 0, tailEnergy(sig, onset, fr.End)
	}
	slope := (float64(k)*sxy - sx*sy) / den
	intercept := (sy - slope*sx) / float64(k)
	rate = -slope
	if rate < 0 {
		rate = 0
	}
	amp = math.Exp(intercept)
	return onset, amp, rate, tailEnergy(sig, onset, fr.End)
}

func tailEnergy(sig []float64, start, end int) float64 {
	e := 0.0
	for i := start; i < end; i++ {
		e += sig[i] * sig[i]
	}
	return e
}

func hasChannel(m *model.Manifest, name string) bool {
	for _, n := range m.Layout.Names {
		if n == name {
			return true
		}
	}
	return false
}
