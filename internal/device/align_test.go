package device

import (
	"testing"

	"haptickernel/internal/packetize"
	"haptickernel/internal/validate"
)

func pkts() []*packetize.Packet {
	mk := func(seq, ch, off int, us int64) *packetize.Packet {
		return &packetize.Packet{Seq: seq, Channel: ch, ChannelIndex: off / 4,
			StartOffset: off, SampleCount: 4, TimingUS: us, Samples: []int{1, 2, 3, 4}}
	}
	return []*packetize.Packet{mk(0, 0, 0, 0), mk(1, 0, 4, 500), mk(2, 0, 8, 1000)}
}

func TestAlignMissDupTimeout(t *testing.T) {
	local := pkts()
	echoes := []Echo{
		{Seq: 0, Channel: 0, StartOffset: 0, Samples: []int{1, 2, 3, 4}},
		// seq1 重发两次
		{Seq: 1, Channel: 0, StartOffset: 4, Samples: []int{1, 2, 3, 4}},
		{Seq: 1, Channel: 0, StartOffset: 4, Samples: []int{1, 2, 3, 4}},
		// seq2 超时，不放进 echoes
	}
	r1 := Align(local, echoes, []int{2})
	codes := map[string]int{}
	for _, a := range r1.Anomalies {
		codes[a.Code]++
	}
	if codes[validate.EDup] != 1 || codes[validate.ETimeout] != 1 {
		t.Fatalf("异常分类错误: %+v", r1.Anomalies)
	}
	// 重复对齐必须得到相同结论与相同首个异常定位。
	r2 := Align(local, echoes, []int{2})
	if r1.FirstDevice.Seq != r2.FirstDevice.Seq || r1.FirstDevice.Code != r2.FirstDevice.Code {
		t.Fatal("重复对齐结论不稳定")
	}
}

func TestAlignMissOnly(t *testing.T) {
	local := pkts()
	echoes := []Echo{
		{Seq: 0, StartOffset: 0, Samples: []int{1, 2, 3, 4}},
		{Seq: 2, StartOffset: 8, Samples: []int{1, 2, 3, 4}},
	}
	r := Align(local, echoes, nil)
	if r.OK || r.FirstDevice == nil || r.FirstDevice.Code != validate.EMiss || r.FirstDevice.Seq != 1 {
		t.Fatalf("缺包定位错误: %+v", r)
	}
}

func TestLocalCheckSeparatesKernelSide(t *testing.T) {
	local := pkts()
	// 制造边界不首尾相接。
	local[2].StartOffset = 99
	a := LocalCheck(local, 4, 0, 2000)
	if a == nil || a.Code != validate.ELocal || a.Side != "kernel" {
		t.Fatalf("内核侧违规未被单独归类: %+v", a)
	}
}
