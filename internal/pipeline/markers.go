package pipeline

import (
	"mcapture/internal/dsp"
	"mcapture/internal/session"
)

const clipThreshold = 0.949

// Preflight 执行故障链路预检：通道缺失(E_CHAN)、削顶(E_CLIP)。
// 标记缺失(E_MARK) 在实际检测阶段给出，以便附带标记序号。
func Preflight(sess *session.Session) error {
	faults, _ := session.LoadFaults(sess.Dir())
	for _, f := range faults {
		switch f.Kind {
		case session.EChan:
			spec := sess.Manifest.ChannelByID(f.Channel)
			if spec == nil {
				return session.NewError(session.EChan,
					"injected missing channel detected in manifest").
					With(f.Channel, "file")
			}
			if err := session.VerifyChannelPresent(sess.Dir(), *spec); err != nil {
				return err
			}
		case session.EClip:
			for _, ch := range sess.Channels {
				if ch.Spec.ID != f.Channel {
					continue
				}
				if idx, ok := dsp.HasClip(ch.Data, clipThreshold); ok {
					return session.NewError(session.EClip,
						"gain overflow / clipping detected").
						With(f.Channel, "gain").WithSample(idx)
				}
			}
		}
	}
	// 即使没有注入记录，也对所有通道做一次削顶扫描，避免漏检。
	for _, ch := range sess.Channels {
		if idx, ok := dsp.HasClip(ch.Data, clipThreshold); ok {
			return session.NewError(session.EClip, "gain overflow / clipping detected").
				With(ch.Spec.ID, "gain").WithSample(idx)
		}
	}
	return nil
}

// detectAllMarkers 在参考通道上检测全部标记，返回按核心位置排序的观测。
func detectAllMarkers(data []float64, tpl []float64, margin, core int64) ([]markerObs, error) {
	w := len(tpl) / 2
	// 先求全序列匹配滤波，标记峰显著高于场景多音产生的低幅相关。
	raw := make([]float64, len(data))
	var globalMax float64
	for i := w; i+w < len(data); i++ {
		var c float64
		for k := -w; k <= w; k++ {
			c += data[i+k] * tpl[k+w]
		}
		raw[i] = c
		if c > globalMax {
			globalMax = c
		}
	}
	// 自适应阈值：取全局最大峰的 55%，远高于背景假峰（实测背景 < 最大峰的 35%）。
	threshold := globalMax * 0.55
	minGap := w * 2
	type peak struct {
		i int
		c float64
	}
	var kept []peak
	for i := w; i+w < len(data); i++ {
		if raw[i] <= threshold {
			continue
		}
		// 局部极大 + 非极大抑制（最小峰间距）。
		isMax := true
		for d := 1; d <= minGap; d++ {
			if i-d >= 0 && raw[i-d] > raw[i] {
				isMax = false
				break
			}
			if i+d < len(raw) && raw[i+d] > raw[i] {
				isMax = false
				break
			}
		}
		if !isMax {
			continue
		}
		if len(kept) > 0 && i-kept[len(kept)-1].i < minGap {
			if raw[i] > kept[len(kept)-1].c {
				kept[len(kept)-1] = peak{i, raw[i]}
			}
			continue
		}
		kept = append(kept, peak{i, raw[i]})
	}
	var obs []markerObs
	for _, pk := range kept {
		lo := pk.i - w
		hi := pk.i + w
		pos, _, _ := dsp.DetectMarker(data, tpl, lo, hi)
		obs = append(obs, markerObs{
			index:   len(obs),
			coreRef: pos - float64(margin),
			arrival: pos,
		})
	}
	for i := range obs {
		obs[i].index = i
	}
	return obs, nil
}

// detectChannelMarkers 在非参考通道上，围绕参考标记物理时间开窄窗检测。
// 任一标记缺失返回 E_MARK，附带通道与标记序号。
func detectChannelMarkers(data []float64, tpl []float64, margin int64,
	ref []markerObs, channelID string) ([]markerObs, error) {
	// 搜索窗覆盖物理上界：本采集场景漂移率 ≤ 1e-3，全程累计漂移 < 0.1% 采样点，
	// 再加标记检测裕量。窗过宽会在标记缺失时误抓相邻峰，故收紧到 256。
	const searchHalf = 256
	obs := make([]markerObs, 0, len(ref))
	for _, rm := range ref {
		center := int64(rm.coreRef) + margin
		lo := int(center) - searchHalf
		hi := int(center) + searchHalf
		pos, peak, found := dsp.DetectMarker(data, tpl, lo, hi)
		// 标记是高 SNR 相干峰；阈值过低会把噪声/邻峰误判为标记。
		if !found || peak <= 0.25 {
			return nil, session.NewError(session.EMark,
				"alignment marker missing in channel window").
				With(channelID, "marker").WithMarker(rm.index)
		}
		obs = append(obs, markerObs{index: rm.index, coreRef: rm.coreRef, arrival: pos})
	}
	return obs, nil
}
