package checkpoint

import (
	"path/filepath"
	"testing"

	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

func TestCommitLoadAndTamperValidation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	pkt := model.Packet{
		GlobalSeq: 0, Channel: "ch_left", PacketIndex: 0, StartSample: 0, EndSample: 2, SampleCount: 2,
		Frames:      []model.Frame{{SampleIndex: 0, FrameQ8: 256, Level: 1}, {SampleIndex: 1, FrameQ8: 512, Level: 2}},
		RuleVersion: model.RuleVersion, AlgorithmVersion: model.AlgorithmVersion, SemanticFingerprint: "fp",
	}
	pkt.FrameDigest = model.DigestFrames(pkt.Frames)
	pkt.ContentHash = model.PacketContentHash(pkt)
	if err := store.CommitPacket(pkt, Meta{Fingerprint: "fp"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPackets()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("load packets=%d err=%v", len(loaded), err)
	}
	if err := Validate(loaded, "fp"); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}

	if _, err := store.db.Exec(`UPDATE packets SET frames_json='[{"sample_index":0,"frame_q8":999,"level":9}]' WHERE global_seq=0`); err != nil {
		t.Fatal(err)
	}
	tampered, err := store.LoadPackets()
	if err != nil {
		t.Fatal(err)
	}
	err = Validate(tampered, "fp")
	if code := errs.CodeOf(err); code != errs.EHash {
		t.Fatalf("tampered checkpoint code=%v err=%v", code, err)
	}
}

func TestContiguousCommitSequence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "seq.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pkt := model.Packet{GlobalSeq: 1, Channel: "x", RuleVersion: model.RuleVersion, AlgorithmVersion: model.AlgorithmVersion}
	if err := store.CommitPacket(pkt, Meta{}); err == nil || errs.CodeOf(err) != errs.EState {
		t.Fatalf("non-contiguous commit must return E_STATE, got %v", err)
	}
}
