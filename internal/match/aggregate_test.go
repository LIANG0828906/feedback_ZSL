package match

import (
	"path/filepath"
	"testing"

	"audiobench/internal/dsp"
	"audiobench/internal/ingest"
	"audiobench/internal/model"
	"audiobench/internal/sample"
)

func loadMain(t *testing.T) (*model.Material, []float64, model.Params) {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.GenerateAll(sample.Options{
		OutDir: dir, Seed: 3, SampleRate: 16000,
		MaterialSecs: 8, EventCount: 6, WindowSize: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	mat, err := ingest.LoadMaterial(filepath.Join(dir, "mat_main.json"))
	if err != nil {
		t.Fatal(err)
	}
	samples, err := ingest.ReadPCM(mat.PCMFile)
	if err != nil {
		t.Fatal(err)
	}
	return mat, samples, model.DefaultParams()
}

func TestAggregateDetectsDeclaredEvents(t *testing.T) {
	mat, samples, p := loadMain(t)
	frames := dsp.ExtractAll(samples, p)
	templates := BuildTemplates(mat.Categories, mat.SampleRate, p)
	events := Aggregate(frames, templates, p)
	if len(events) != len(mat.Events) {
		t.Fatalf("期望检出 %d 个事件，实际 %d", len(mat.Events), len(events))
	}
	for i, ev := range events {
		if ev.Label != mat.Events[i].Label {
			t.Fatalf("事件 %d 类别期望 %s 实际 %s", i, mat.Events[i].Label, ev.Label)
		}
		if ev.Confidence <= 0 || ev.Confidence > 1 {
			t.Fatalf("事件 %d 置信度越界: %f", i, ev.Confidence)
		}
		if ev.EndSample <= ev.StartSample {
			t.Fatalf("事件 %d 区间非法", i)
		}
	}
	// 区间不得跨类别重叠。
	for i := 1; i < len(events); i++ {
		if events[i].StartSample < events[i-1].EndSample &&
			events[i].Label != events[i-1].Label {
			t.Fatalf("不同类别事件重叠: %+v %+v", events[i-1], events[i])
		}
	}
}

func TestQualityPassesWithinTolerance(t *testing.T) {
	mat, samples, p := loadMain(t)
	frames := dsp.ExtractAll(samples, p)
	templates := BuildTemplates(mat.Categories, mat.SampleRate, p)
	events := Aggregate(frames, templates, p)
	report, err := ValidateQuality(mat.Events, events, mat.SampleTolerance)
	if err != nil {
		t.Fatalf("容差内不应返回 E_QUAL: %v", err)
	}
	if report.LabelAccuracy != 1.0 || report.Matched != len(mat.Events) {
		t.Fatalf("质量统计异常: %+v", report)
	}
}

func TestQualityReturnsEqualOnMismatch(t *testing.T) {
	mat, samples, p := loadMain(t)
	frames := dsp.ExtractAll(samples, p)
	templates := BuildTemplates(mat.Categories, mat.SampleRate, p)
	events := Aggregate(frames, templates, p)
	// 故意把首个检测事件改成错误类别。
	events[0].Label = "alarm"
	report, err := ValidateQuality(mat.Events, events, mat.SampleTolerance)
	if err == nil || model.ErrCode(err) != model.EQuality {
		t.Fatalf("期望 E_QUAL，实际 %v", err)
	}
	if len(report.Mismatches) == 0 || report.Mismatches[0].DeclLabel == "" {
		t.Fatalf("E_QUAL 必须列出错配事件与两侧类别: %+v", report.Mismatches)
	}
}

func TestGapToleranceMergesAdjacentHits(t *testing.T) {
	mat, samples, p := loadMain(t)
	p.GapToleranceFrames = 0
	frames := dsp.ExtractAll(samples, p)
	templates := BuildTemplates(mat.Categories, mat.SampleRate, p)
	strict := Aggregate(frames, templates, p)

	p.GapToleranceFrames = 4
	merged := Aggregate(frames, templates, p)
	if len(merged) > len(strict) {
		t.Fatalf("增大间隙容忍不应产生更多区间: %d > %d", len(merged), len(strict))
	}
}
