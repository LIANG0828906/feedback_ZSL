// Package compare 自动比对两条流水线输出，定位首个不一致采样位置、
// 所属帧编号与差异字段名，不一致时由调用方包装为 E_DIVERGE。
package compare

import (
	"fmt"
	"math"

	"spatialir/internal/analyzer"
	"spatialir/internal/model"
)

const epsilon = 1e-12

// Compare 深度比对两份报告，返回首个差异；完全一致返回 nil。
func Compare(a, b *model.RunReport, frames []analyzer.Frame) *model.Divergence {
	if a.Frames != b.Frames {
		return &model.Divergence{Field: "frames", ValueA: fmt.Sprint(a.Frames), ValueB: fmt.Sprint(b.Frames)}
	}
	if len(a.CommitSeq) != len(b.CommitSeq) {
		return &model.Divergence{Field: "commit_seq_length",
			ValueA: fmt.Sprint(len(a.CommitSeq)), ValueB: fmt.Sprint(len(b.CommitSeq))}
	}
	for i := range a.CommitSeq {
		if a.CommitSeq[i] != b.CommitSeq[i] {
			return &model.Divergence{Frame: i, Field: "commit_seq",
				ValueA: fmt.Sprint(a.CommitSeq[i]), ValueB: fmt.Sprint(b.CommitSeq[i])}
		}
	}
	for fi := range a.FrameReports {
		if d := compareSnapshot(a.FrameReports[fi], b.FrameReports[fi]); d != nil {
			d.Frame = fi
			return d
		}
	}
	// 波形逐样本比对，并把样本位置映射回所属帧与声道。
	for ch := range a.Rendered {
		name := a.Layout.Names[ch]
		for i := 0; i < len(a.Rendered[ch]); i++ {
			if math.Abs(a.Rendered[ch][i]-b.Rendered[ch][i]) > epsilon {
				frame := locateFrame(frames, i)
				return &model.Divergence{
					Frame:       frame,
					Channel:     name,
					SampleIndex: i,
					Field:       "rendered_sample",
					ValueA:      fmt.Sprint(a.Rendered[ch][i]),
					ValueB:      fmt.Sprint(b.Rendered[ch][i]),
				}
			}
		}
	}
	return nil
}

func compareSnapshot(a, b model.FrameSnapshot) *model.Divergence {
	if a.Index != b.Index || a.Status != b.Status {
		return &model.Divergence{Field: "status"}
	}
	if !metricsEqual(a.Measured, b.Measured) {
		return &model.Divergence{Field: "measured"}
	}
	if !metricsEqual(a.Bound.PerFrame, b.Bound.PerFrame) {
		return &model.Divergence{Field: "bound_per_frame"}
	}
	if !metricsEqual(a.Bound.Cumulative, b.Bound.Cumulative) {
		return &model.Divergence{Field: "bound_cumulative"}
	}
	if math.Abs(a.Energy.OriginalEnergy-b.Energy.OriginalEnergy) > epsilon ||
		math.Abs(a.Energy.RenderEnergy-b.Energy.RenderEnergy) > epsilon {
		return &model.Divergence{Field: "energy"}
	}
	for ch := range a.Estimates {
		ae, be := a.Estimates[ch], b.Estimates[ch]
		if ae.DirectSample != be.DirectSample {
			return &model.Divergence{Channel: ae.Channel, Field: "direct_sample"}
		}
		if math.Abs(ae.EarlyEnergy-be.EarlyEnergy) > epsilon {
			return &model.Divergence{Channel: ae.Channel, Field: "early_energy"}
		}
		if math.Abs(ae.DryReverbRatio-be.DryReverbRatio) > epsilon {
			return &model.Divergence{Channel: ae.Channel, Field: "dry_reverb_ratio"}
		}
	}
	for k, v := range a.ITD {
		if math.Abs(v-b.ITD[k]) > epsilon {
			return &model.Divergence{Channel: k, Field: "itd"}
		}
	}
	return nil
}

func metricsEqual(a, b model.ErrorMetric) bool {
	return math.Abs(a.PeakQuantization-b.PeakQuantization) <= epsilon &&
		math.Abs(a.EnergyTruncation-b.EnergyTruncation) <= epsilon &&
		math.Abs(a.ITDEstimation-b.ITDEstimation) <= epsilon &&
		math.Abs(a.RenderInterp-b.RenderInterp) <= epsilon
}

func locateFrame(frames []analyzer.Frame, sample int) int {
	for _, fr := range frames {
		if sample >= fr.Start && sample < fr.End {
			return fr.Index
		}
	}
	return -1
}

// AsError 把差异包装为 E_DIVERGE 业务错误。
func AsError(d *model.Divergence) error {
	if d == nil {
		return nil
	}
	ae := model.NewError(model.CodeDiverge,
		"first divergence at field "+d.Field).WithFrame(d.Frame).WithChannel(d.Channel)
	ae.Metric = d.Field
	return ae
}
