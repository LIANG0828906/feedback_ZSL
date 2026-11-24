package errorsx

import (
	"testing"

	"spatialir/internal/model"
)

func TestStructuralBoundOnlyFromObservedStructure(t *testing.T) {
	p := model.FrameParams{FrameLength: 1024, FrameHop: 512, WindowLen: 768, InterpOrder: 1}
	b48 := StructuralBound(p, 48000)
	b96 := StructuralBound(p, 96000)
	// 上界不依赖任何真值或渲染结果；同样结构在不同采样率下样本域上界一致。
	if b48 != b96 {
		t.Fatalf("sample-domain bounds must not depend on sample rate scaling: %+v %+v", b48, b96)
	}
	if b48.EnergyTruncation != 1024 || b48.PeakQuantization != 0.5 ||
		b48.ITDEstimation != 1 || b48.RenderInterp != 0.25 {
		t.Fatalf("unexpected structural bound: %+v", b48)
	}
	p.InterpOrder = 3
	if StructuralBound(p, 48000).RenderInterp != 0.0625 {
		t.Fatal("cubic bound mismatch")
	}
}

func TestCheckAndAccumulate(t *testing.T) {
	bound := model.ErrorMetric{PeakQuantization: 0.5, EnergyTruncation: 100, ITDEstimation: 1, RenderInterp: 0.25}
	ok := model.ErrorMetric{PeakQuantization: 0.4, EnergyTruncation: 90, ITDEstimation: 0.8, RenderInterp: 0.1}
	cum, err := CheckAndAccumulate(model.ErrorMetric{}, bound, ok, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if cum.PeakQuantization != 0.4 || cum.EnergyTruncation != 90 {
		t.Fatalf("bad accumulation: %+v", cum)
	}
	bad := model.ErrorMetric{PeakQuantization: 0.9}
	_, err = CheckAndAccumulate(cum, bound, bad, 1, 4)
	ae := model.AsAppError(err)
	if ae == nil || ae.Code != model.CodeBound || ae.Frame != 4 || ae.Metric != MetricPeak {
		t.Fatalf("expect E_BOUND with frame/metric, got %v", err)
	}
}
