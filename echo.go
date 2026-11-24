package audiocal

import "math"

// EchoPoint 是逐频点回显比对结果。
type EchoPoint struct {
	FrequencyHz float64
	LocalDB     float64
	EchoDB      float64
	DeltaDB     float64
}

// EchoReport 汇总回环回显校验。
type EchoReport struct {
	ChannelID int
	Points    []EchoPoint
	MaxDelta  float64
	Tolerance float64
	Pass      bool
}

// LocalExpectedResponse 由本地 FIR 系数计算设备应施加的频响（与设备端同一确定性算法）。
func LocalExpectedResponse(model *CalibrationModel) map[int][]float64 {
	out := make(map[int][]float64, len(model.Channels))
	for _, ch := range model.Channels {
		out[ch.ChannelID] = FIRResponseDB(ch.FIRTaps, model.SampleRate, model.Grid)
	}
	return out
}

// CompareEcho 按网格频点对齐比对回显与本地拟合频响（业务要求 4）。
func CompareEcho(model *CalibrationModel, channelID int, echoDB []float64, toleranceDB float64) (*EchoReport, error) {
	if len(echoDB) != len(model.Grid) {
		return nil, newErrf(CodeEcho, "channel="+itoa(channelID),
			"回显频点数 %d 与本地网格 %d 不一致", len(echoDB), len(model.Grid))
	}
	var local []float64
	for _, ch := range model.Channels {
		if ch.ChannelID == channelID {
			local = FIRResponseDB(ch.FIRTaps, model.SampleRate, model.Grid)
			break
		}
	}
	if local == nil {
		return nil, newErrf(CodeEcho, "channel="+itoa(channelID), "本地模型不存在该通道")
	}
	rep := &EchoReport{ChannelID: channelID, Tolerance: toleranceDB, Pass: true}
	for i, f := range model.Grid {
		delta := math.Abs(echoDB[i] - local[i])
		rep.Points = append(rep.Points, EchoPoint{
			FrequencyHz: f, LocalDB: local[i], EchoDB: echoDB[i], DeltaDB: delta,
		})
		if delta > rep.MaxDelta {
			rep.MaxDelta = delta
		}
	}
	for _, p := range rep.Points {
		if p.DeltaDB > toleranceDB {
			rep.Pass = false
			return rep, &BizError{
				Code:    CodeEcho,
				Message: "回显频响逐点偏差超出声明容差",
				Field:   "channel=" + itoa(channelID) + ",point=" + freqLabel(rep.Points, p),
				Detail: map[string]any{
					"channel_id":   channelID,
					"frequency_hz": p.FrequencyHz,
					"local_db":     p.LocalDB,
					"echo_db":      p.EchoDB,
					"delta_db":     p.DeltaDB,
					"tolerance_db": toleranceDB,
				},
			}
		}
	}
	return rep, nil
}

func freqLabel(pts []EchoPoint, target EchoPoint) string {
	for i, p := range pts {
		if p.FrequencyHz == target.FrequencyHz {
			return itoa(i)
		}
	}
	return "?"
}
