// Package soundfield 由逐通道分帧估计结果重建声场参数：
// 通道间时差（ITD）与直达/混响能量比（DR），并提供逐帧冻结视图。
package soundfield

import "spatialir/internal/model"

// Recompute 基于参考声道计算每个通道的 ITD（单位：样本），
// 无直达事件的通道 ITD 记为 0 并保持逐帧确定性。
func Recompute(estimates []model.ChannelEstimate, reference string) map[string]float64 {
	refSample := 0
	for _, e := range estimates {
		if e.Channel == reference {
			refSample = e.DirectSample
			break
		}
	}
	out := make(map[string]float64, len(estimates))
	for _, e := range estimates {
		if len(e.Events) == 0 {
			out[e.Channel] = 0
			continue
		}
		out[e.Channel] = float64(e.DirectSample - refSample)
	}
	return out
}

// LayoutMap 把清单声道名映射为渲染输出声道名；未配置映射时按恒等映射。
// 映射目标缺失返回 E_CHAN。
func LayoutMap(layout model.ChannelLayout, mapping map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(layout.Names))
	for _, name := range layout.Names {
		target, ok := mapping[name]
		if !ok {
			out[name] = name
			continue
		}
		found := false
		for _, cand := range layout.Names {
			if cand == target {
				found = true
				break
			}
		}
		if !found {
			return nil, model.NewError(model.CodeChan, "layout mapping target not in layout").
				WithChannel(name)
		}
		out[name] = target
	}
	return out, nil
}
