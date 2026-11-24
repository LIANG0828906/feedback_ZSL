package audiocal

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
)

// FirmwareMagic 与 Version 构成声明的固件格式。
const (
	FirmwareMagic   = "ACALFW01"
	FirmwareVersion = uint16(1)
	HeaderSize      = 64
)

// Segment 是一个固件系数分段。
type Segment struct {
	Index        int       `json:"index"`
	ChannelID    int       `json:"channel_id"`
	CoeffStart   int       `json:"coeff_start"` // 在该通道 FIR 中的起始抽头
	CoeffCount   int       `json:"coeff_count"`
	Coeffs       []float64 `json:"coeffs"`
	EnvelopeDB   []float64 `json:"envelope_db"` // 该段负责频点上的增益包络
	EventStartUS int64     `json:"event_start_us"`
	EventEndUS   int64     `json:"event_end_us"`
	Merged       bool      `json:"merged"` // 是否因分段超上限在窗内合并产生
	ContentHash  string    `json:"content_hash"`
}

// MergeDiag 记录分段合并诊断。
type MergeDiag struct {
	RequestedSegments int      `json:"requested_segments"`
	MaxSegments       int      `json:"max_segments"`
	MergedPairs       [][2]int `json:"merged_pairs"`
}

// FirmwareConfig 是完整的设备可加载固件配置。
type FirmwareConfig struct {
	Segments     []Segment        `json:"segments"`
	ChannelMap   []ChannelMapping `json:"channel_map"`
	Grid         []float64        `json:"grid_hz"`
	SampleRate   int              `json:"sample_rate_hz"`
	WindowUS     int64            `json:"window_us"`
	TargetHash   string           `json:"target_hash"`
	ParamVersion int              `json:"param_version"`
	AlgorithmID  string           `json:"algorithm_id"`
	Diagnostics  *MergeDiag       `json:"diagnostics,omitempty"`
	encoded      []byte
}

// ChannelMapping 是扬声器->麦克风->固件通道的确定性映射。
type ChannelMapping struct {
	LogicalChannel int `json:"logical_channel"`
	SpeakerID      int `json:"speaker_id"`
	MicID          int `json:"mic_id"`
}

// PlanSegments 按 MaxSegments / MaxCoeffsPerSeg 切分系数段，并强制时序窗口约束。
func PlanSegments(model *CalibrationModel) ([]Segment, *MergeDiag, error) {
	cfg := model.Config
	if cfg.WindowUS <= 0 || cfg.MaxSegments <= 0 {
		return nil, nil, &BizError{
			Code:    CodeWin,
			Message: "时间窗或分段数为非法值",
			Field:   "window_us/max_segments",
			Detail:  map[string]any{"window_us": cfg.WindowUS, "max_segments": cfg.MaxSegments},
		}
	}

	// 先按“每通道若干段”生成自然分段（每段至多 MaxCoeffsPerSeg 个系数）。
	var raw []Segment
	segIdx := 0
	eventCursor := int64(0)
	stepUS := cfg.WindowUS / 4 // 自然事件步长，保证单事件远小于窗口
	if stepUS <= 0 {
		stepUS = 1
	}
	for _, ch := range model.Channels {
		taps := ch.FIRTaps
		for start := 0; start < len(taps); start += cfg.MaxCoeffsPerSeg {
			end := start + cfg.MaxCoeffsPerSeg
			if end > len(taps) {
				end = len(taps)
			}
			coeffs := append([]float64(nil), taps[start:end]...)
			env := segmentEnvelope(ch.Predicted)
			seg := Segment{
				Index:        segIdx,
				ChannelID:    ch.ChannelID,
				CoeffStart:   start,
				CoeffCount:   end - start,
				Coeffs:       coeffs,
				EnvelopeDB:   env,
				EventStartUS: eventCursor,
				EventEndUS:   eventCursor + stepUS,
			}
			seg.ContentHash = hashSegment(seg)
			raw = append(raw, seg)
			segIdx++
			eventCursor += stepUS
		}
	}

	// 分段超上限：在时序窗内合并，不静默截断，并给出诊断。
	var diag *MergeDiag
	if len(raw) > cfg.MaxSegments {
		diag = &MergeDiag{RequestedSegments: len(raw), MaxSegments: cfg.MaxSegments}
		// 设备时序总预算 = 窗宽 × 最大分段数；自然事件跨度放不下则 E_WIN，
		// 明确指出实测段数与窗宽，而不是静默截断。
		naturalSpan := raw[len(raw)-1].EventEndUS - raw[0].EventStartUS
		budget := cfg.WindowUS * int64(cfg.MaxSegments)
		if naturalSpan > budget {
			return nil, diag, &BizError{
				Code:    CodeWin,
				Message: "自然分段事件跨度超出固件窗口总预算，无法在窗内合并",
				Field:   "actual_segments=" + itoa(len(raw)),
				Detail: map[string]any{
					"actual_segments": len(raw),
					"max_segments":    cfg.MaxSegments,
					"window_us":       cfg.WindowUS,
					"natural_span_us": naturalSpan,
					"budget_us":       budget,
				},
			}
		}
		raw, diag = mergeWithinWindow(raw, cfg, diag)
		// 通道数本身超过段上限等不可满足场景：合并后仍超限，返回 E_WIN。
		if len(raw) > cfg.MaxSegments {
			return nil, diag, &BizError{
				Code:    CodeWin,
				Message: "通道数超过最大分段数且无法在窗内进一步合并",
				Field:   "actual_segments=" + itoa(len(raw)),
				Detail: map[string]any{
					"actual_segments": len(raw),
					"max_segments":    cfg.MaxSegments,
					"window_us":       cfg.WindowUS,
				},
			}
		}
		// 合并后仍超窗（窗宽放不下合并事件）则 E_WIN，指出实测段数与窗宽。
		for _, seg := range raw {
			if seg.EventEndUS-seg.EventStartUS > cfg.WindowUS || seg.EventEndUS > cfg.WindowUS*int64(cfg.MaxSegments) {
				return nil, diag, &BizError{
					Code:    CodeWin,
					Message: "合并后事件时序超出固件时间窗",
					Field:   "segment=" + itoa(seg.Index),
					Detail: map[string]any{
						"actual_segments": len(raw),
						"window_us":       cfg.WindowUS,
						"event_start_us":  seg.EventStartUS,
						"event_end_us":    seg.EventEndUS,
					},
				}
			}
		}
	}

	// 最终窗口合法性校验。
	for _, seg := range raw {
		if seg.EventStartUS < 0 || seg.EventEndUS <= seg.EventStartUS ||
			seg.EventEndUS-seg.EventStartUS > cfg.WindowUS {
			return nil, diag, &BizError{
				Code:    CodeWin,
				Message: "段事件时序落在固件声明时间窗之外",
				Field:   "segment=" + itoa(seg.Index),
				Detail:  map[string]any{"actual_segments": len(raw), "window_us": cfg.WindowUS},
			}
		}
	}
	return raw, diag, nil
}

// mergeWithinWindow 把多余段合并到上限内，合并事件重新落在等宽窗槽中。
func mergeWithinWindow(raw []Segment, cfg FitConfig, diag *MergeDiag) ([]Segment, *MergeDiag) {
	// 按通道分组（raw 本就按通道顺序生成），只在同一通道内合并相邻段，
	// 杜绝跨通道混合导致的 FIR 重组错误。
	type chanGroup struct {
		channelID int
		segments  []int
	}
	var groups []chanGroup
	cur := -1
	for i, seg := range raw {
		if seg.ChannelID != cur {
			groups = append(groups, chanGroup{channelID: seg.ChannelID})
			cur = seg.ChannelID
		}
		groups[len(groups)-1].segments = append(groups[len(groups)-1].segments, i)
	}

	// 每个通道至少需要一个段；通道数超上限属于不可满足约束。
	if len(groups) > cfg.MaxSegments {
		return raw, diag
	}

	// 把可用段槽分配给各通道（至少 1，多余槽按自然段比例分配）。
	slots := make([]int, len(groups))
	extra := cfg.MaxSegments - len(groups)
	for i := range slots {
		slots[i] = 1
	}
	for extra > 0 {
		// 找自然段最多的通道继续给槽。
		best := 0
		for i := range groups {
			if len(groups[i].segments) > len(groups[best].segments) {
				best = i
			}
		}
		if slots[best] >= len(groups[best].segments) {
			break
		}
		slots[best]++
		extra--
	}

	out := make([]Segment, 0, cfg.MaxSegments)
	globalIdx := 0
	slot := int64(0)
	for gi, grp := range groups {
		n := len(grp.segments)
		k := slots[gi]
		if k > n {
			k = n
		}
		for q := 0; q < k; q++ {
			lo := q * n / k
			hi := (q + 1) * n / k
			if hi <= lo {
				hi = lo + 1
			}
			base := raw[grp.segments[lo]]
			for _, j := range grp.segments[lo+1 : hi] {
				base.Coeffs = append(base.Coeffs, raw[j].Coeffs...)
				base.CoeffCount += raw[j].CoeffCount
				diag.MergedPairs = append(diag.MergedPairs, [2]int{raw[grp.segments[lo]].Index, raw[j].Index})
			}
			base.Index = globalIdx
			globalIdx++
			base.Merged = hi-lo > 1
			base.EventStartUS = slot * cfg.WindowUS
			base.EventEndUS = base.EventStartUS + cfg.WindowUS/2
			base.ContentHash = hashSegment(base)
			out = append(out, base)
			slot++
		}
	}
	return out, diag
}

// segmentEnvelope 给出该段携带的增益包络。
// 长度固定等于网格频点数并携带该通道完整预测增益，设备端无需任何跨段状态即可复算。
func segmentEnvelope(pred []float64) []float64 {
	return append([]float64(nil), pred...)
}

// BuildFirmware 生成含通道映射与时序字段的固件配置。
func BuildFirmware(model *CalibrationModel) (*FirmwareConfig, error) {
	segs, diag, err := PlanSegments(model)
	if err != nil {
		return nil, err
	}
	// 通道映射按逻辑通道号排序，杜绝 map 遍历顺序依赖。
	cm := make([]ChannelMapping, 0, len(model.Channels))
	for _, ch := range model.Channels {
		cm = append(cm, ChannelMapping{
			LogicalChannel: ch.ChannelID,
			SpeakerID:      ch.ChannelID,
			MicID:          ch.ChannelID,
		})
	}
	sortMappings(cm)
	fw := &FirmwareConfig{
		Segments:     segs,
		ChannelMap:   cm,
		Grid:         append([]float64(nil), model.Grid...),
		SampleRate:   model.SampleRate,
		WindowUS:     model.Config.WindowUS,
		TargetHash:   TargetFingerprint(model.Target),
		ParamVersion: model.Config.ParamVersion,
		AlgorithmID:  model.AlgorithmID,
		Diagnostics:  diag,
	}
	return fw, nil
}

func sortMappings(cm []ChannelMapping) {
	for i := 1; i < len(cm); i++ {
		for j := i; j > 0 && cm[j-1].LogicalChannel > cm[j].LogicalChannel; j-- {
			cm[j-1], cm[j] = cm[j], cm[j-1]
		}
	}
}

// TargetFingerprint 计算目标曲线的确定性指纹。
func TargetFingerprint(target []TargetPoint) string {
	h := sha256.New()
	var buf [8]byte
	for _, p := range target {
		binary.BigEndian.PutUint64(buf[:], math.Float64bits(p.FrequencyHz))
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], math.Float64bits(p.GainDB))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashSegment(s Segment) string {
	h := sha256.New()
	var buf [8]byte
	put := func(v float64) {
		binary.BigEndian.PutUint64(buf[:], math.Float64bits(v))
		h.Write(buf[:])
	}
	putI := func(v int64) {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}
	putI(int64(s.Index))
	putI(int64(s.ChannelID))
	putI(int64(s.CoeffStart))
	putI(int64(s.CoeffCount))
	for _, c := range s.Coeffs {
		put(c)
	}
	for _, e := range s.EnvelopeDB {
		put(e)
	}
	putI(s.EventStartUS)
	putI(s.EventEndUS)
	return hex.EncodeToString(h.Sum(nil))
}

// Encode 输出确定性固件字节：纯大端序、排序后的映射、无 map、无 goroutine ID、无时间戳。
func (f *FirmwareConfig) Encode() []byte {
	if f.encoded != nil {
		return append([]byte(nil), f.encoded...)
	}
	var b []byte
	b = append(b, []byte(FirmwareMagic)...)
	b = binary.BigEndian.AppendUint16(b, FirmwareVersion)
	b = binary.BigEndian.AppendUint32(b, uint32(f.SampleRate))
	b = binary.BigEndian.AppendUint32(b, uint32(len(f.ChannelMap)))
	b = binary.BigEndian.AppendUint32(b, uint32(len(f.Segments)))
	b = binary.BigEndian.AppendUint32(b, uint32(len(f.Grid)))
	b = binary.BigEndian.AppendUint64(b, uint64(f.WindowUS))
	b = binary.BigEndian.AppendUint32(b, uint32(f.ParamVersion))
	b = append(b, []byte(f.AlgorithmID)...)
	for len(b) < HeaderSize {
		b = append(b, 0)
	}

	for _, m := range f.ChannelMap {
		b = binary.BigEndian.AppendUint32(b, uint32(m.LogicalChannel))
		b = binary.BigEndian.AppendUint32(b, uint32(m.SpeakerID))
		b = binary.BigEndian.AppendUint32(b, uint32(m.MicID))
	}
	for _, g := range f.Grid {
		b = binary.BigEndian.AppendUint64(b, math.Float64bits(g))
	}
	for _, s := range f.Segments {
		b = binary.BigEndian.AppendUint32(b, uint32(s.Index))
		b = binary.BigEndian.AppendUint32(b, uint32(s.ChannelID))
		b = binary.BigEndian.AppendUint32(b, uint32(s.CoeffStart))
		b = binary.BigEndian.AppendUint32(b, uint32(s.CoeffCount))
		b = binary.BigEndian.AppendUint64(b, uint64(s.EventStartUS))
		b = binary.BigEndian.AppendUint64(b, uint64(s.EventEndUS))
		flags := uint8(0)
		if s.Merged {
			flags = 1
		}
		b = append(b, flags)
		for _, c := range s.Coeffs {
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(c))
		}
		for _, e := range s.EnvelopeDB {
			b = binary.BigEndian.AppendUint64(b, math.Float64bits(e))
		}
		hb, _ := hex.DecodeString(s.ContentHash)
		b = append(b, hb...)
	}
	th, _ := hex.DecodeString(f.TargetHash)
	b = append(b, th...)
	overall := sha256.Sum256(b)
	b = append(b, overall[:]...)
	f.encoded = append([]byte(nil), b...)
	return append([]byte(nil), b...)
}

// ByteSummary 返回固件字节摘要，供面板展示。
func (f *FirmwareConfig) ByteSummary() (int, string) {
	b := f.Encode()
	sum := sha256.Sum256(b)
	return len(b), hex.EncodeToString(sum[:])
}
