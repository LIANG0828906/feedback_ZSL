package paths

import (
	"path/filepath"
	"testing"

	"haptickernel/internal/sample"
	"haptickernel/internal/store"
	"haptickernel/internal/synth"
	"haptickernel/internal/validate"
)

func loadFixture(t *testing.T) (string, string) {
	dir := t.TempDir()
	mp, ep, err := sample.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	return mp, ep
}

func TestThreePathsIdentical(t *testing.T) {
	mp, ep := loadFixture(t)
	m, err := validate.LoadManifest(mp)
	if err != nil {
		t.Fatal(err)
	}
	events, err := validate.LoadEvents(ep, m)
	if err != nil {
		t.Fatal(err)
	}
	cpRoot := filepath.Join(t.TempDir(), "cp")
	rtStore, _ := store.NewCheckpointStore(filepath.Join(cpRoot, "realtime"))
	opt := Options{Store: rtStore, MaxWorkers: 1, MaxBufferFrames: 256, WindowPackets: 8}

	rt, err := RunRealtime(events, m, opt)
	if err != nil {
		t.Fatal(err)
	}
	resumeStore, _ := store.NewCheckpointStore(filepath.Join(cpRoot, "realtime"))
	resume, err := RunResume(events, m, Options{Store: resumeStore, WindowPackets: 8})
	if err != nil {
		t.Fatal(err)
	}
	replayStore, _ := store.NewCheckpointStore(filepath.Join(cpRoot, "realtime"))
	replay, err := RunReplay(events, m, Options{Store: replayStore, WindowPackets: 8})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Compare(rt, resume, replay)
	if err != nil {
		t.Fatalf("三路径应一致: %v", err)
	}
	if !report.Passed {
		t.Fatalf("比对未通过: %+v", report.FirstDiff)
	}
	if rt.SampleTotal != resume.SampleTotal || rt.SampleTotal != replay.SampleTotal {
		t.Fatal("采样点总数不一致")
	}
}

func TestResumeFallsBackToFullReplayOnInvalidSnapshot(t *testing.T) {
	mp, ep := loadFixture(t)
	m, _ := validate.LoadManifest(mp)
	events, _ := validate.LoadEvents(ep, m)
	dir := filepath.Join(t.TempDir(), "cp", "realtime")
	s, _ := store.NewCheckpointStore(dir)
	opt := Options{Store: s, WindowPackets: 8}
	if _, err := RunRealtime(events, m, opt); err != nil {
		t.Fatal(err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	// 空快照下恢复路径应等价全量重算，不报错。
	resume, err := RunResume(events, m, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(resume.Packets) == 0 {
		t.Fatal("恢复路径未产出包")
	}
}

func TestCompareReturnsEDiff(t *testing.T) {
	mp, ep := loadFixture(t)
	m, _ := validate.LoadManifest(mp)
	events, _ := validate.LoadEvents(ep, m)
	s, _ := store.NewCheckpointStore(filepath.Join(t.TempDir(), "cp"))
	rt, err := RunRealtime(events, m, Options{Store: s, WindowPackets: 8})
	if err != nil {
		t.Fatal(err)
	}
	// 人为制造一条不一致路径。
	tampered := *rt
	tampered.Path = PathReplay
	tampered.Curves = make([]*synth.ChannelCurve, len(rt.Curves))
	for i, c := range rt.Curves {
		cp := *c
		cp.Frames = append([]synth.Frame(nil), c.Frames...)
		tampered.Curves[i] = &cp
	}
	tampered.Curves[0].Frames[0].Value++
	report, err := Compare(rt, &tampered)
	if err == nil {
		t.Fatal("期望 E_DIFF 错误")
	}
	biz, ok := err.(*validate.BizError)
	if !ok || biz.Code != validate.EDiff {
		t.Fatalf("期望 E_DIFF, 实际 %v", err)
	}
	if report.Passed || report.FirstDiff == nil {
		t.Fatal("应记录首个不一致位置")
	}
}
