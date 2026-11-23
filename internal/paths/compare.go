package paths

import (
	"fmt"

	"haptickernel/internal/packetize"
	"haptickernel/internal/validate"
)

// DiffPoint 定位首个不一致位置。
type DiffPoint struct {
	PathA       PathName `json:"path_a"`
	PathB       PathName `json:"path_b"`
	Channel     int      `json:"channel"`
	GridOffset  int      `json:"grid_offset"`
	PacketSeq   int      `json:"packet_seq"`
	ValueA      int      `json:"value_a"`
	ValueB      int      `json:"value_b"`
	LogicalTime string   `json:"logical_time"`
}

// CompareReport 是三路径比对报告。
type CompareReport struct {
	Passed       bool             `json:"passed"`
	SampleTotals map[PathName]int `json:"sample_totals"`
	PacketCounts map[PathName]int `json:"packet_counts"`
	FirstDiff    *DiffPoint       `json:"first_diff,omitempty"`
	PathOrder    []PathName       `json:"path_order"`
}

// Compare 对任意数量路径做逐采样点与逐包比对。
func Compare(results ...*RunResult) (*CompareReport, error) {
	if len(results) < 2 {
		return nil, validate.NewBiz(validate.EPara, "至少需要两条路径才能比对", "paths", -1, -1)
	}
	report := &CompareReport{
		Passed:       true,
		SampleTotals: map[PathName]int{},
		PacketCounts: map[PathName]int{},
	}
	base := results[0]
	for _, r := range results {
		report.PathOrder = append(report.PathOrder, r.Path)
		report.SampleTotals[r.Path] = r.SampleTotal
		report.PacketCounts[r.Path] = len(r.Packets)
	}

	// 1) 逐采样点比对（按通道、网格偏移）。
	for ci := range base.Curves {
		bc := base.Curves[ci]
		for _, other := range results[1:] {
			oc := other.Curves[ci]
			if len(bc.Frames) != len(oc.Frames) {
				report.Passed = false
				off := len(bc.Frames)
				if len(oc.Frames) < off {
					off = len(oc.Frames)
				}
				report.FirstDiff = &DiffPoint{
					PathA: base.Path, PathB: other.Path, Channel: ci,
					GridOffset:  off,
					LogicalTime: "frame-count-mismatch",
				}
				return report, diffErr(report.FirstDiff)
			}
			for k := range bc.Frames {
				if bc.Frames[k].Value != oc.Frames[k].Value {
					report.Passed = false
					report.FirstDiff = &DiffPoint{
						PathA: base.Path, PathB: other.Path, Channel: ci,
						GridOffset: bc.Frames[k].GridOffset,
						PacketSeq:  packetSeqAt(other.Packets, ci, bc.Frames[k].GridOffset),
						ValueA:     bc.Frames[k].Value, ValueB: oc.Frames[k].Value,
						LogicalTime: fmt.Sprintf("channel=%d grid=%d", ci, bc.Frames[k].GridOffset),
					}
					return report, diffErr(report.FirstDiff)
				}
			}
		}
	}

	// 2) 逐包切分边界与编码字节比对。
	for _, other := range results[1:] {
		if len(base.Packets) != len(other.Packets) {
			report.Passed = false
			report.FirstDiff = &DiffPoint{
				PathA: base.Path, PathB: other.Path,
				LogicalTime: "packet-count-mismatch",
			}
			return report, diffErr(report.FirstDiff)
		}
		for i, bp := range base.Packets {
			op := other.Packets[i]
			if bp.StartOffset != op.StartOffset || bp.Channel != op.Channel ||
				bp.SampleCount != op.SampleCount || string(bp.Encode()) != string(op.Encode()) {
				report.Passed = false
				report.FirstDiff = &DiffPoint{
					PathA: base.Path, PathB: other.Path, Channel: bp.Channel,
					GridOffset: bp.StartOffset, PacketSeq: bp.Seq,
					LogicalTime: fmt.Sprintf("seq=%d channel=%d offset=%d", bp.Seq, bp.Channel, bp.StartOffset),
				}
				return report, diffErr(report.FirstDiff)
			}
		}
	}
	return report, nil
}

func packetSeqAt(packets []*packetize.Packet, channel, offset int) int {
	for _, pkt := range packets {
		if pkt.Channel == channel &&
			offset >= pkt.StartOffset && offset < pkt.StartOffset+pkt.SampleCount {
			return pkt.Seq
		}
	}
	return -1
}

func diffErr(d *DiffPoint) error {
	return validate.NewBiz(validate.EDiff,
		fmt.Sprintf("路径 %s 与 %s 首次不一致: %s valueA=%d valueB=%d",
			d.PathA, d.PathB, d.LogicalTime, d.ValueA, d.ValueB),
		"frames", d.Channel, -1)
}
