package packetize

import (
	"testing"

	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

func TestEncodeDeterministic(t *testing.T) {
	p := &Packet{Seq: 1, Channel: 2, ChannelIndex: 0, StartOffset: 0,
		SampleCount: 2, TimingUS: 100, Samples: []int{10, -10}}
	b1 := p.Encode()
	b2 := p.Encode()
	if string(b1) != string(b2) {
		t.Fatal("重复编码字节不一致")
	}
	if len(b1) != 4+4*5+8+4 {
		t.Fatalf("编码长度异常: %d", len(b1))
	}
	if p.Hash() == "" {
		t.Fatal("哈希为空")
	}
}

func TestInvalidationPropagation(t *testing.T) {
	pkts := []*Packet{
		{Seq: 0, Channel: 0, ChannelIndex: 0, DependEvents: []string{"ev-a"}},
		{Seq: 1, Channel: 0, ChannelIndex: 1, DependEvents: []string{"ev-b"}},
		{Seq: 2, Channel: 0, ChannelIndex: 2, DependEvents: []string{"ev-c"}},
		{Seq: 3, Channel: 1, ChannelIndex: 0, DependEvents: []string{"ev-d"}},
	}
	entries := PropagateInvalidation(pkts, EditSpec{EditedEventIDs: []string{"ev-b"}})
	want := map[int]bool{0: true, 1: false, 2: false, 3: true}
	for _, e := range entries {
		if e.Reused != want[e.Seq] {
			t.Fatalf("seq=%d 复用状态错误: %+v", e.Seq, e)
		}
	}
	if entries[0].Seq != 0 || entries[3].Seq != 3 {
		t.Fatal("清单顺序与提交顺序不一致")
	}
}

func TestLocalViolationClassified(t *testing.T) {
	m := &synth.Manifest{
		SamplesPerPacket: 4, QuantUnit: 1,
		Channels: []synth.ChannelCap{{
			TimingWindow: synth.TimingWindow{MinOffsetUS: 0, MaxOffsetUS: 100},
		}},
	}
	pkt := &Packet{Channel: 0, ChannelIndex: 0, SampleCount: 9,
		StartOffset: 0, TimingUS: 10}
	err := validateLocal(pkt, m, 0)
	biz, ok := err.(*validate.BizError)
	if !ok || biz.Code != validate.ELocal {
		t.Fatalf("期望 E_LOCAL, 实际 %v", err)
	}
}
