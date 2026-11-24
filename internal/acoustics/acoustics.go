package acoustics

import (
	"math"

	"github.com/gameaudio/workbench/internal/model"
)

// 采样网格与物理常量（算法版本的一部分，调整必须提升 AlgorithmVersion）。
const (
	SampleCount      = 16
	SampleStep       = 0.05 // 每采样点的时间步长（秒）
	ReferenceSPL     = 90.0 // 参考距离 1m 处声压级 dB
	DistanceRolloff  = 20.0 // 距离每翻倍衰减 dB
	OcclusionPenalty = 18.0 // 遮挡固定附加衰减 dB
)

// Input 是单对声源-听者的声学合成输入，仅包含确定性参数。
type Input struct {
	Region         model.Region
	EffectiveLen   float64 // 直达或最短绕射长度
	Occluded       bool
	DiffractionLen float64
}

// Output 是合成结果。
type Output struct {
	Curve []float64
	SPL   float64
}

// DecayRate 由混响时间推导：每秒衰减量 = 60 / RT60 (dB/s)。
func DecayRate(reverbTime float64) float64 {
	return 60.0 / reverbTime
}

// Synthesize 合成衰减曲线与期望声压级。
// 曲线只依赖区域参数、有效长度与遮挡结果，与线程标识、缓存状态无关。
func Synthesize(in Input) Output {
	rate := DecayRate(in.Region.ReverbTime)
	curve := make([]float64, SampleCount)
	for k := 0; k < SampleCount; k++ {
		t := float64(k) * SampleStep
		// 区域增益（线性）换算为 dB 偏移，叠加 RT60 指数衰减。
		gainDB := 20.0 * math.Log10(in.Region.Gain)
		curve[k] = gainDB - rate*t
	}

	// 声压级：参考级 - 距离衰减（对数距离律）- 遮挡惩罚，
	// 再叠加 t=0 的区域增益与首个曲线采样保持口径一致。
	distanceDB := DistanceRolloff * math.Log2(math.Max(in.EffectiveLen, 1.0))
	spl := ReferenceSPL - distanceDB + curve[0]
	if in.Occluded {
		spl -= OcclusionPenalty
	}
	return Output{Curve: curve, SPL: spl}
}

// EqualCurve 逐采样比较两条曲线，返回首个不一致下标（-1 表示全等）。
func EqualCurve(a, b []float64, eps float64) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if math.Abs(a[i]-b[i]) > eps {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}
