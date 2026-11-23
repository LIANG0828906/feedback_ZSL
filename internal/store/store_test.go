package store

import (
	"os"
	"path/filepath"
	"testing"

	"haptickernel/internal/packetize"
	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

func TestStateMachineLegalAndIllegal(t *testing.T) {
	sm := NewStateMachine()
	if err := sm.Transition(StateQuantizing); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(StateSliced); err != nil {
		t.Fatal(err)
	}
	if err := sm.Transition(StateCommitted); err != nil {
		t.Fatal(err)
	}
	// 已提交是冻结态，任何迁出非法且保持原状态。
	err := sm.Transition(StateQuantizing)
	biz, ok := err.(*validate.BizError)
	if !ok || biz.Code != validate.EState {
		t.Fatalf("期望 E_STATE, 实际 %v", err)
	}
	if sm.State() != StateCommitted {
		t.Fatalf("非法迁移改变了状态: %s", sm.State())
	}
}

func testManifest() *synth.Manifest {
	return &synth.Manifest{ManifestVersion: "m1", ChannelCount: 1,
		SampleRateHz: 8000, QuantUnit: 1, SamplesPerPacket: 4}
}

func TestCheckpointCommitLoadAndInvalidate(t *testing.T) {
	dir := t.TempDir()
	s, err := NewCheckpointStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	pkt := &packetize.Packet{Seq: 0, Channel: 0, ChannelIndex: 0,
		StartOffset: 0, SampleCount: 2, TimingUS: 1, Samples: []int{1, 2}}
	pkt.ContentHash = pkt.Hash()
	w := &WindowRecord{
		WindowIndex: 0, ManifestVersion: "m1", AlgoVersion: synth.AlgoVersion,
		QuantUnit: 1, SamplesPerPacket: 4, Packets: []*packetize.Packet{pkt},
	}
	if err := s.CommitWindow(w); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Load(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || len(snap.Windows) != 1 {
		t.Fatalf("有效快照加载失败: %+v", snap)
	}

	// 篡改窗口文件模拟内容哈希不匹配，应整体作废。
	files, _ := os.ReadDir(dir)
	var target string
	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			target = filepath.Join(dir, f.Name())
		}
	}
	raw, _ := os.ReadFile(target)
	// 破坏 JSON 结构，模拟损坏快照。
	raw = append([]byte("{broken,"), raw...)
	if err := os.WriteFile(target, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	snap2, err := s.Load(testManifest())
	if err != nil {
		t.Fatal(err)
	}
	if snap2.Valid {
		t.Fatal("被篡改的快照应当整体作废")
	}
}

func TestVersionMismatchInvalidates(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewCheckpointStore(dir)
	pkt := &packetize.Packet{Seq: 0, Channel: 0, Samples: []int{1}}
	pkt.ContentHash = pkt.Hash()
	w := &WindowRecord{WindowIndex: 0, ManifestVersion: "m1",
		AlgoVersion: synth.AlgoVersion, QuantUnit: 1, SamplesPerPacket: 4,
		Packets: []*packetize.Packet{pkt}}
	if err := s.CommitWindow(w); err != nil {
		t.Fatal(err)
	}
	m2 := testManifest()
	m2.ManifestVersion = "m2-changed"
	snap, err := s.Load(m2)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Valid || snap.Reason == "" {
		t.Fatalf("版本不匹配应作废并给出原因: %+v", snap)
	}
}
