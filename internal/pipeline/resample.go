package pipeline

import (
	"mcapture/internal/dsp"
	"mcapture/internal/session"
)

// AlignChannel 按漂移模型把非参考通道重采样到参考核心时间轴。
// 输出长度严格等于 corePoints；边界插值越界返回 E_PARA（定位到对齐区间），
// 不补零、不丢样。
func AlignChannel(data []float64, params DriftParams, margin, core int64, cfg AlgoConfig) ([]float64, error) {
	if len(data) != int(core+2*margin) {
		return nil, session.NewError(session.EPara,
			"input length does not match manifest").With("", "data")
	}
	table := dsp.NewSincTable(cfg.HalfWindow, cfg.SincOrder, 256)
	out := make([]float64, core)
	halfWin := cfg.HalfWindow
	phi := params.phiPrefix(core)
	for n := int64(0); n < core; n++ {
		// 核心采样 n 对应的物理时间在本通道设备坐标上的位置：
		// 设备索引 = margin + n + phi(n)，phi 是该通道相对参考的累计漂移。
		devPos := float64(margin+n) + phi[n]
		if !dsp.InBounds(int64(len(data)), devPos, halfWin) {
			return nil, session.NewError(session.EPara,
				"interpolation position out of bounds (no zero padding allowed)").
				WithSample(n)
		}
		out[n] = table.Shift(data, devPos)
	}
	return out, nil
}

// ResidualStats 汇总对齐后标记残余偏移。
type ResidualStats struct {
	MarkerIndex int
	Residual    float64
	Bound       float64
}

// CheckResidual 在对齐后的通道上重新检测标记，并与参考标记位置核对。
// 超出声明上界返回 E_RESID（带通道与标记序号）。
func CheckResidual(aligned, refAligned []float64, tpl []float64, width int,
	refMarkers []markerObs, bound float64, channelID string) ([]ResidualStats, error) {
	stats := make([]ResidualStats, 0, len(refMarkers))
	for _, rm := range refMarkers {
		center := int(rm.coreRef)
		lo := center - width*2
		hi := center + width*2
		if lo < 0 {
			lo = 0
		}
		if hi > len(aligned)-1 {
			hi = len(aligned) - 1
		}
		pos, _, found := dsp.DetectMarker(aligned, tpl, lo, hi)
		if !found {
			return nil, session.NewError(session.EMark,
				"marker missing after alignment").
				With(channelID, "marker").WithMarker(rm.index)
		}
		res := pos - rm.coreRef
		stats = append(stats, ResidualStats{MarkerIndex: rm.index, Residual: res, Bound: bound})
		if abs(res) > bound {
			return nil, session.NewError(session.EResid,
				"residual offset exceeds declared bound").
				With(channelID, "marker").WithMarker(rm.index).WithSample(int64(center))
		}
	}
	return stats, nil
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
