package device

import (
	"testing"

	"audiocal"
)

func TestMergedFirmwareEchoConsistency(t *testing.T) {
	sample := audiocal.GenerateSamples(1, 1)[0]
	cfg := audiocal.DefaultFitConfig()
	// 4 通道自然产生 4 段，压到 3 槽强制在同一通道内合并，同时保留全部通道。
	cfg.MaxCoeffsPerSeg = 32 // 让单通道 FIR(64 抽头) 自然拆成 2 段，制造可合并空间
	cfg.MaxSegments = 4      // 8 个自然段在每通道内合并回 1 段
	model, err := audiocal.Fit(sample, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fw, err := audiocal.BuildFirmware(model)
	if err != nil {
		t.Fatal(err)
	}
	if fw.Diagnostics == nil {
		t.Fatal("预期存在合并诊断")
	}

	srv, err := Listen(0)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	resp, err := NewClient(srv.Addr()).Upload(fw.Encode(), model.Grid, model.SampleRate, 0.01, FaultNone)
	if err != nil {
		t.Fatal(err)
	}
	for chID, applied := range resp.AppliedDB {
		if _, err := audiocal.CompareEcho(model, chID, applied, 0.01); err != nil {
			t.Fatalf("合并固件通道 %d 回显不一致: %v", chID, err)
		}
	}
}
