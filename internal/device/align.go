package device

import (
	"fmt"

	"haptickernel/internal/packetize"
	"haptickernel/internal/validate"
)

// Anomaly 是一次对齐发现的异常（设备侧或内核侧）。
type Anomaly struct {
	Code        string `json:"code"`
	Side        string `json:"side"` // device | kernel
	Seq         int    `json:"seq"`
	Channel     int    `json:"channel"`
	LogicalTime int64  `json:"logical_time_us"`
	Reason      string `json:"reason"`
}

// AlignReport 是回显对齐结论（重复对齐输入相同则结论相同）。
type AlignReport struct {
	OK           bool      `json:"ok"`
	Anomalies    []Anomaly `json:"anomalies"`
	AlignedCount int       `json:"aligned_count"`
	FirstDevice  *Anomaly  `json:"first_device_anomaly,omitempty"`
	FirstKernel  *Anomaly  `json:"first_kernel_anomaly,omitempty"`
}

// LocalCheck 先做内核侧本地违规检查（E_LOCAL 系列），与设备侧异常严格区分。
func LocalCheck(packets []*packetize.Packet, samplesPerPacket int, minUS, maxUS int64) *Anomaly {
	prevEnd := map[int]int{}
	for _, pkt := range packets {
		if pkt.SampleCount > samplesPerPacket {
			return &Anomaly{Code: validate.ELocal, Side: "kernel", Seq: pkt.Seq,
				Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
				Reason: "sample-count-over-limit"}
		}
		if pkt.TimingUS < minUS || pkt.TimingUS > maxUS {
			return &Anomaly{Code: validate.ELocal, Side: "kernel", Seq: pkt.Seq,
				Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
				Reason: "timing-out-of-window"}
		}
		if end, ok := prevEnd[pkt.Channel]; ok && pkt.StartOffset != end {
			return &Anomaly{Code: validate.ELocal, Side: "kernel", Seq: pkt.Seq,
				Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
				Reason: "boundary-not-contiguous"}
		}
		prevEnd[pkt.Channel] = pkt.StartOffset + pkt.SampleCount
	}
	return nil
}

// Align 把设备回显按包序号与本地记录对齐，输出确定性异常清单。
// echoes 为按提交顺序收集的全部回显；timeouts 为超时未回显的包序号。
func Align(local []*packetize.Packet, echoes []Echo, timeouts []int) *AlignReport {
	report := &AlignReport{OK: true}
	echoBySeq := map[int][]Echo{}
	for _, e := range echoes {
		echoBySeq[e.Seq] = append(echoBySeq[e.Seq], e)
	}
	timeoutSet := map[int]bool{}
	for _, seq := range timeouts {
		timeoutSet[seq] = true
	}

	add := func(a Anomaly) {
		report.Anomalies = append(report.Anomalies, a)
		if a.Side == "device" && report.FirstDevice == nil {
			cp := a
			report.FirstDevice = &cp
		}
		if a.Side == "kernel" && report.FirstKernel == nil {
			cp := a
			report.FirstKernel = &cp
		}
	}

	for _, pkt := range local {
		switch {
		case timeoutSet[pkt.Seq]:
			report.OK = false
			add(Anomaly{Code: validate.ETimeout, Side: "device", Seq: pkt.Seq,
				Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
				Reason: fmt.Sprintf("seq=%d 超时未回显", pkt.Seq)})
		case len(echoBySeq[pkt.Seq]) == 0:
			report.OK = false
			add(Anomaly{Code: validate.EMiss, Side: "device", Seq: pkt.Seq,
				Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
				Reason: fmt.Sprintf("seq=%d 缺包", pkt.Seq)})
		case len(echoBySeq[pkt.Seq]) > 1:
			report.OK = false
			add(Anomaly{Code: validate.EDup, Side: "device", Seq: pkt.Seq,
				Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
				Reason: fmt.Sprintf("seq=%d 重发 %d 次", pkt.Seq, len(echoBySeq[pkt.Seq]))})
		default:
			// 校验回显采样点与本地记录一致，也算内核/数据对齐违规。
			got := echoBySeq[pkt.Seq][0]
			if !sameSamples(got.Samples, pkt.Samples) || got.StartOffset != pkt.StartOffset {
				report.OK = false
				add(Anomaly{Code: validate.ELocal, Side: "kernel", Seq: pkt.Seq,
					Channel: pkt.Channel, LogicalTime: pkt.TimingUS,
					Reason: "echo-sample-mismatch"})
			} else {
				report.AlignedCount++
			}
		}
	}
	return report
}

func sameSamples(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
