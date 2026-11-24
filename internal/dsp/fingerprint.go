package dsp

import (
	"math"

	"audiobench/internal/model"
)

// 参数边界，越界统一返回 E_PARA 并指出字段名。
const (
	minFrameLength = 64
	maxFrameLength = 16384
	minFrameShift  = 1
	minBands       = 4
	maxBands       = 128
	maxGapTol      = 64
)

// ValidateParams 校验分帧/频带/聚合参数。帧移大于帧长按越界处理。
func ValidateParams(p model.Params) error {
	check := func(ok bool, field string, actual any) error {
		if !ok {
			return model.NewErr(model.EPara, "参数字段 %s 越界，实测值 %v", field, actual).
				With("field", field).With("actual", actual)
		}
		return nil
	}
	if err := check(p.FrameLength >= minFrameLength && p.FrameLength <= maxFrameLength &&
		p.FrameLength&(p.FrameLength-1) == 0, "frame_length", p.FrameLength); err != nil {
		return err
	}
	if err := check(p.FrameShift >= minFrameShift && p.FrameShift <= p.FrameLength,
		"frame_shift", p.FrameShift); err != nil {
		return err
	}
	if err := check(p.Bands >= minBands && p.Bands <= maxBands && p.Bands <= p.FrameLength/2,
		"bands", p.Bands); err != nil {
		return err
	}
	if err := check(p.GapToleranceFrames >= 0 && p.GapToleranceFrames <= maxGapTol,
		"gap_tolerance_frames", p.GapToleranceFrames); err != nil {
		return err
	}
	if err := check(p.MinHitFrames >= 1, "min_hit_frames", p.MinHitFrames); err != nil {
		return err
	}
	if err := check(p.MatchThreshold > 0 && p.MatchThreshold < 1,
		"match_threshold", p.MatchThreshold); err != nil {
		return err
	}
	if err := check(p.QuietRatio > 0 && p.QuietRatio < 1,
		"quiet_ratio_threshold", p.QuietRatio); err != nil {
		return err
	}
	return nil
}

// FrameCount 返回给定总采样点数下的帧数（完整帧 + 至少一个采样的尾帧）。
func FrameCount(totalSamples, frameLength, frameShift int) int {
	if totalSamples < frameLength {
		return 0
	}
	return 1 + (totalSamples-frameLength)/frameShift
}

// ExtractFrame 提取单帧指纹。纯函数：相同采样与参数 → 逐位相同输出，
// 不读取任何 goroutine 本地状态或全局随机源。
func ExtractFrame(samples []float64, frame, frameLength, frameShift, bands int) model.Fingerprint {
	offset := frame * frameShift
	real := make([]float64, frameLength)
	imag := make([]float64, frameLength)
	for n := 0; n < frameLength; n++ {
		real[n] = samples[offset+n] * HannWindow(n, frameLength)
	}
	FFT(real, imag)

	binCount := frameLength/2 + 1
	bandEnergy := make([]float64, bands)
	bandBins := binCount / bands
	for band := 0; band < bands; band++ {
		lo := band * bandBins
		hi := lo + bandBins
		if band == bands-1 {
			hi = binCount
		}
		sum := 0.0
		for bin := lo; bin < hi; bin++ {
			sum += real[bin]*real[bin] + imag[bin]*imag[bin]
		}
		bandEnergy[band] = sum / float64(hi-lo)
	}

	total := 0.0
	for _, e := range bandEnergy {
		total += e
	}
	relative := make([]float64, bands)
	quantized := make([]int, bands)
	if total > 0 {
		for b := 0; b < bands; b++ {
			relative[b] = bandEnergy[b] / total
			quantized[b] = int(math.Round(relative[b] * 1000))
		}
	}
	return model.Fingerprint{
		Frame:          frame,
		OffsetSample:   offset,
		TotalEnergy:    total / float64(frameLength),
		RelativeEnergy: relative,
		Quantized:      quantized,
	}
}

// ExtractAll 顺序提取整段素材的全部帧指纹（全量重算路径使用）。
func ExtractAll(samples []float64, p model.Params) []model.Fingerprint {
	count := FrameCount(len(samples), p.FrameLength, p.FrameShift)
	out := make([]model.Fingerprint, count)
	for f := 0; f < count; f++ {
		out[f] = ExtractFrame(samples, f, p.FrameLength, p.FrameShift, p.Bands)
	}
	return out
}

// BuildTemplate 用与素材合成完全相同的确定性渲染器生成类别特征音，
// 再走同一套分帧/FFT/量化路径得到模板的平均相对能量分布。
func BuildTemplate(cat model.Category, sampleRate int, p model.Params) []float64 {
	length := p.FrameLength * 4
	buf := make([]float64, length)
	RenderTone(buf, 0, length, sampleRate, cat.BaseFreqHz, cat.StepHz, cat.Partials)
	frames := FrameCount(length, p.FrameLength, p.FrameShift)
	avg := make([]float64, p.Bands)
	used := 0
	for f := 0; f < frames; f++ {
		fp := ExtractFrame(buf, f, p.FrameLength, p.FrameShift, p.Bands)
		if fp.TotalEnergy <= 0 {
			continue
		}
		for b := 0; b < p.Bands; b++ {
			avg[b] += fp.RelativeEnergy[b]
		}
		used++
	}
	if used > 0 {
		for b := range avg {
			avg[b] /= float64(used)
		}
	}
	return avg
}

// NoiseFloorAmplitude 与样本生成器的本底噪声振幅（±0.01）对齐的语音门限。
// 仅当采样振幅显著高于本底噪声时才计入声能量，避免噪声被误判为语音。
const NoiseFloorAmplitude = 0.01

// VoiceEnergyRatio 实测声能量占比：超过本底噪声门限的采样能量 / 总能量。
// 纯本底噪声素材该值趋近 0，含特征音调素材趋近 1，阈值比较稳定可复现。
func VoiceEnergyRatio(samples []float64) (ratio float64, noiseFloor float64) {
	noiseFloor = NoiseFloorAmplitude * 3
	threshold := noiseFloor * noiseFloor
	total := 0.0
	voice := 0.0
	for _, v := range samples {
		e := v * v
		total += e
		if e > threshold {
			voice += e
		}
	}
	if total == 0 {
		return 0, noiseFloor
	}
	return voice / total, noiseFloor
}
