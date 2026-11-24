package audiocal

import (
	"bytes"
	"path/filepath"
	"testing"
)

func TestThreeExportPathsByteIdentical(t *testing.T) {
	s := GenerateSamples(20261124, 2)[1]
	m, err := Fit(s, DefaultFitConfig())
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()

	r1, err := ExportInteractive(m, NewCheckpointStore(filepath.Join(base, "i")))
	if err != nil {
		t.Fatal(err)
	}

	// 模拟中断：只提交前两段，再走断点恢复。
	cpDir := filepath.Join(base, "r")
	partial, err := NewExporter(m, NewCheckpointStore(cpDir))
	if err != nil {
		t.Fatal(err)
	}
	_, total := partial.Progress()
	cut := total / 2
	for i := 0; i < cut; i++ {
		if _, err := partial.ExportSegment(i); err != nil {
			t.Fatal(err)
		}
	}
	r2, err := ExportResumable(m, NewCheckpointStore(cpDir))
	if err != nil {
		t.Fatal(err)
	}

	r3, err := ExportAllAtOnce(m, NewCheckpointStore(filepath.Join(base, "f")))
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(r1.Bytes, r2.Bytes) || !bytes.Equal(r1.Bytes, r3.Bytes) {
		t.Fatal("三条导出路径固件字节不一致")
	}
	if err := CompareResults(r1, r2); err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(r1, r3); err != nil {
		t.Fatal(err)
	}
	for i := range r1.CommitSequence {
		if r1.CommitSequence[i] != r2.CommitSequence[i] || r1.CommitSequence[i] != r3.CommitSequence[i] {
			t.Fatal("段提交序列不一致")
		}
	}
}

func TestCompareDetectsFirstDiff(t *testing.T) {
	s := GenerateSamples(1, 1)[0]
	m, _ := Fit(s, DefaultFitConfig())
	base := t.TempDir()
	r1, _ := ExportInteractive(m, NewCheckpointStore(filepath.Join(base, "a")))
	r2, _ := ExportInteractive(m, NewCheckpointStore(filepath.Join(base, "b")))
	// 在第一个段体内部人为破坏一个字节，验证段号定位。
	seg0Start := HeaderSize + len(r1.Firmware.ChannelMap)*12 + len(r1.Firmware.Grid)*8 + 4
	r2.Bytes[seg0Start] ^= 0xFF
	err := CompareResults(r1, r2)
	be, ok := AsBizError(err)
	if !ok || be.Code != CodeDiff {
		t.Fatalf("期望 E_DIFF，得到 %v", err)
	}
	if be.Detail["left_byte"] == be.Detail["right_byte"] {
		t.Fatal("E_DIFF 未给出两侧不同字节")
	}
	if _, has := be.Detail["offset"]; !has {
		t.Fatal("E_DIFF 未给出字节偏移")
	}
	if be.Detail["segment_index"] != 0 {
		t.Fatalf("期望段号 0，得到 %v", be.Detail["segment_index"])
	}
}

func TestPoolBoundedParallelism(t *testing.T) {
	samples := GenerateSamples(9, 4)
	cfg := DefaultFitConfig()
	models, errs := ParallelFit(samples, cfg, 2, len(cfg.GridFrequencies)+100)
	if len(models) != 4 {
		t.Fatal("并行拟合结果数量错误")
	}
	for i, e := range errs {
		if e != nil {
			t.Fatalf("样本 %d 拟合错误: %v", i, e)
		}
	}

	pool := NewPool(2, 4)
	err := pool.Submit(5, func() error { return nil })
	assertCode(t, err, CodePara, "buffer_slots")
}

func TestPanelRenders(t *testing.T) {
	m := buildTestModel(t)
	exp, err := NewExporter(m, NewCheckpointStore(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exp.ExportSegment(0); err != nil {
		t.Fatal(err)
	}
	panel := NewPanel(m, exp)
	out := panel.Render()
	if !contains(out, "声学校准可视化面板") || !contains(out, "已提交 1/") {
		t.Fatal("面板渲染缺少关键内容")
	}
}
