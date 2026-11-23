package packetize

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

// Packet 是一个设备包（按通道、按网格顺序切分）。
type Packet struct {
	Seq          int    `json:"seq"` // 全局提交序号（跨通道稳定顺序）
	Channel      int    `json:"channel"`
	ChannelIndex int    `json:"channel_index"` // 通道内包序号
	StartOffset  int    `json:"start_offset"`  // 起始量化网格偏移
	SampleCount  int    `json:"sample_count"`  // 包内采样点数
	TimingUS     int64  `json:"timing_us"`     // 时序标记（微秒，逻辑时间）
	Samples      []int  `json:"samples"`       // 定点包络帧值
	ContentHash  string `json:"content_hash"`

	// 依赖图信息
	DependEvents []string `json:"depend_events"`
	Template     string   `json:"template_refs"` // 参与叠加的模板名（去重，逗号连接）
	QuantUnit    int      `json:"quant_unit"`
	PrevOffset   int      `json:"prev_offset"` // 前一包占位采样偏移
}

// packetMagic 与格式版本保证编码字节稳定、可识别。
var packetMagic = [4]byte{'H', 'P', 'K', '1'}

// Encode 按确定性格式编码设备包，不依赖任何 map 遍历或线程标识。
func (p *Packet) Encode() []byte {
	buf := make([]byte, 0, 32+len(p.Samples)*2)
	buf = append(buf, packetMagic[:]...)
	buf = binary.BigEndian.AppendUint32(buf, uint32(p.Seq))
	buf = binary.BigEndian.AppendUint32(buf, uint32(p.Channel))
	buf = binary.BigEndian.AppendUint32(buf, uint32(p.ChannelIndex))
	buf = binary.BigEndian.AppendUint32(buf, uint32(p.StartOffset))
	buf = binary.BigEndian.AppendUint32(buf, uint32(p.SampleCount))
	buf = binary.BigEndian.AppendUint64(buf, uint64(p.TimingUS))
	for _, v := range p.Samples {
		buf = binary.BigEndian.AppendUint16(buf, uint16(int16(v)))
	}
	return buf
}

// Hash 对编码字节做内容哈希。
func (p *Packet) Hash() string {
	sum := sha256.Sum256(p.Encode())
	return hex.EncodeToString(sum[:])
}

// FrameSummary 是写入检查点的定点包络帧摘要（用于重启完整性校验）。
type FrameSummary struct {
	Channel     int    `json:"channel"`
	StartOffset int    `json:"start_offset"`
	SampleCount int    `json:"sample_count"`
	Hash        string `json:"hash"`
}

// SliceResult 是一次切分的全部包（全局提交顺序）与帧摘要。
type SliceResult struct {
	Packets   []*Packet      `json:"packets"`
	Summaries []FrameSummary `json:"frame_summaries"`
}

// Slice 把各通道包络帧按 SamplesPerPacket 切分为有序设备包。
func Slice(curves []*synth.ChannelCurve, m *synth.Manifest) (*SliceResult, error) {
	perChannel := make([][]*Packet, m.ChannelCount)

	for c, curve := range curves {
		frames := curve.Frames
		if len(frames) == 0 {
			continue
		}
		idx := 0
		prevEnd := 0
		for off := 0; off < len(frames); off += m.SamplesPerPacket {
			end := off + m.SamplesPerPacket
			if end > len(frames) {
				end = len(frames)
			}
			samples := make([]int, end-off)
			eventSet := map[string]bool{}
			tplSet := map[string]bool{}
			for k := off; k < end; k++ {
				samples[k-off] = frames[k].Value
				for _, ct := range curve.Decisions[k].Contributors {
					eventSet[ct.EventID] = true
					tplSet[ct.Template] = true
				}
			}
			evs := mapKeys(eventSet)
			tpls := mapKeys(tplSet)

			pkt := &Packet{
				Channel:      c,
				ChannelIndex: idx,
				StartOffset:  frames[off].GridOffset,
				SampleCount:  end - off,
				TimingUS:     m.GridUS(frames[off].GridOffset),
				Samples:      samples,
				DependEvents: evs,
				Template:     joinCSV(tpls),
				QuantUnit:    m.QuantUnit,
				PrevOffset:   prevEnd,
			}
			if err := validateLocal(pkt, m, prevEnd); err != nil {
				return nil, err
			}
			pkt.ContentHash = pkt.Hash()
			perChannel[c] = append(perChannel[c], pkt)
			prevEnd = pkt.StartOffset + pkt.SampleCount
			idx++
		}
	}

	// 全局提交顺序：按通道内包序号轮询、再按通道，保证确定性与稳定排序。
	var all []*Packet
	maxLen := 0
	for _, pkts := range perChannel {
		if len(pkts) > maxLen {
			maxLen = len(pkts)
		}
	}
	for i := 0; i < maxLen; i++ {
		for c := 0; c < m.ChannelCount; c++ {
			if i < len(perChannel[c]) {
				all = append(all, perChannel[c][i])
			}
		}
	}
	// seq 进入编码，最后统一编号并重算哈希。
	for seq, pkt := range all {
		pkt.Seq = seq
		pkt.ContentHash = pkt.Hash()
	}

	res := &SliceResult{Packets: all}
	for _, pkt := range all {
		sum := sha256.Sum256(encodeSamples(pkt.Samples))
		res.Summaries = append(res.Summaries, FrameSummary{
			Channel:     pkt.Channel,
			StartOffset: pkt.StartOffset,
			SampleCount: pkt.SampleCount,
			Hash:        hex.EncodeToString(sum[:]),
		})
	}
	return res, nil
}

// validateLocal 归类内核侧本地违规（E_LOCAL），与设备侧异常严格分开。
func validateLocal(pkt *Packet, m *synth.Manifest, prevEnd int) error {
	if pkt.SampleCount > m.SamplesPerPacket {
		return validate.NewBiz(validate.ELocal,
			fmt.Sprintf("包内采样点 %d 超过上限 %d", pkt.SampleCount, m.SamplesPerPacket),
			"sample_count", pkt.Channel, -1)
	}
	tw := m.Channels[pkt.Channel].TimingWindow
	if pkt.TimingUS < tw.MinOffsetUS || pkt.TimingUS > tw.MaxOffsetUS {
		return validate.NewBiz(validate.ELocal,
			fmt.Sprintf("时序标记 %d 越出窗口 [%d,%d]", pkt.TimingUS, tw.MinOffsetUS, tw.MaxOffsetUS),
			"timing_us", pkt.Channel, -1)
	}
	if pkt.ChannelIndex > 0 && pkt.StartOffset != prevEnd {
		return validate.NewBiz(validate.ELocal,
			fmt.Sprintf("切分边界不首尾相接: start=%d prevEnd=%d", pkt.StartOffset, prevEnd),
			"start_offset", pkt.Channel, -1)
	}
	return nil
}

func encodeSamples(samples []int) []byte {
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.BigEndian.PutUint16(buf[i*2:], uint16(int16(v)))
	}
	return buf
}

func mapKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinCSV(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}
