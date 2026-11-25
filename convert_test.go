package vpcompose_test

import (
	"math"
	"testing"

	"vpcompose"
	"vpcompose/sample"
)

func TestIntegerTimebaseRoundTripAndOverflow(t *testing.T) {
	for ms := int64(0); ms <= 1300; ms += 50 {
		ticks, err := vpcompose.MillisecondsToTicks(ms, sample.TickRate, vpcompose.RoundHalfUp, "time")
		if err != nil {
			t.Fatal(err)
		}
		back, err := vpcompose.TicksToMilliseconds(ticks, sample.TickRate, vpcompose.RoundHalfUp, "time")
		if err != nil {
			t.Fatal(err)
		}
		if back != ms {
			t.Fatalf("round trip %d -> %d -> %d", ms, ticks, back)
		}
	}
	if _, err := vpcompose.MillisecondsToTicks(math.MaxInt64, sample.TickRate, vpcompose.RoundHalfUp, "layers[0].visible_until_ms"); !vpcompose.IsCode(err, vpcompose.CodeOverflow) {
		t.Fatalf("expected E_OVF, got %v", err)
	} else {
		ee := err.(*vpcompose.EngineError)
		if ee.Path != "layers[0].visible_until_ms" || len(ee.Operands) != 2 {
			t.Fatalf("overflow details = %+v", ee)
		}
	}
}

func TestRoundTripV3ToV2RestoresLayerState(t *testing.T) {
	generated, err := sample.Generate(9)
	if err != nil {
		t.Fatal(err)
	}
	original, err := vpcompose.LoadProject(generated.Files["project-v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	down, err := vpcompose.ExportProject(original, vpcompose.Version2)
	if err != nil {
		t.Fatalf("v3->v2 should be lossless: %v", err)
	}
	middle, err := vpcompose.LoadProject(down.Data)
	if err != nil {
		t.Fatal(err)
	}
	up, err := vpcompose.ExportProject(middle, vpcompose.Version3)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := vpcompose.LoadProject(up.Data)
	if err != nil {
		t.Fatal(err)
	}
	assertLayerStateEqual(t, original, restored)
	if restored.Layers[0].SourceAlphaMode != vpcompose.AlphaPremultiplied {
		t.Fatalf("alpha state not restored: %s", restored.Layers[0].SourceAlphaMode)
	}
	if err := vpcompose.VerifyTruth(restored); err != nil {
		t.Fatalf("round-tripped project truth: %v", err)
	}
}

func TestPremultipliedChannelConversionIsIntegerExact(t *testing.T) {
	if got := vpcompose.PremultiplyChannel(65535, 32768); got != 32768 {
		t.Fatalf("premultiply = %d", got)
	}
	if got := vpcompose.UnpremultiplyChannel(32768, 32768); got != 65535 {
		t.Fatalf("unpremultiply = %d", got)
	}
	if got := vpcompose.PremultiplyChannel(12345, 0); got != 0 {
		t.Fatalf("zero alpha premultiply = %d", got)
	}
	rgba, trace := vpcompose.ConvertAlphaMode([4]uint16{65535, 32768, 0, 32768}, vpcompose.AlphaStraight, vpcompose.AlphaPremultiplied)
	if rgba[0] != 32768 || rgba[1] != 16384 || trace.Rule == "" {
		t.Fatalf("conversion rgba=%v trace=%+v", rgba, trace)
	}
}

func TestV3ToV1ReportsLossesButReadsBackOrderAndVisibility(t *testing.T) {
	generated, err := sample.Generate(10)
	if err != nil {
		t.Fatal(err)
	}
	original, err := vpcompose.LoadProject(generated.Files["project-v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	result, err := vpcompose.ExportProject(original, vpcompose.Version1)
	if !vpcompose.IsCode(err, vpcompose.CodeLossy) {
		t.Fatalf("expected E_LOSSY, got %v", err)
	}
	if len(result.Losses) == 0 {
		t.Fatal("expected loss list")
	}
	reloaded, loadErr := vpcompose.LoadProject(result.Data)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !equalStrings(reloaded.LayerOrder(), original.LayerOrder()) {
		t.Fatalf("layer order changed: %v vs %v", reloaded.LayerOrder(), original.LayerOrder())
	}
	for i := range original.Layers {
		if original.Layers[i].VisibleUntil != reloaded.Layers[i].VisibleUntil {
			t.Fatalf("visibility end changed for %s", original.Layers[i].ID)
		}
	}
	if err := vpcompose.VerifyTruth(reloaded); err != nil {
		t.Fatalf("lossy v1 readback no longer matches shared truth: %v", err)
	}
}

func assertLayerStateEqual(t *testing.T, a, b *vpcompose.Project) {
	t.Helper()
	if !equalStrings(a.LayerOrder(), b.LayerOrder()) {
		t.Fatalf("layer order %v != %v", a.LayerOrder(), b.LayerOrder())
	}
	for i := range a.Layers {
		left, right := a.Layers[i], b.Layers[i]
		if left.ID != right.ID || left.Opacity != right.Opacity ||
			left.Visible != right.Visible || left.VisibleUntil != right.VisibleUntil ||
			left.BlendMode != right.BlendMode || left.ContentHash != right.ContentHash {
			t.Fatalf("layer %d lost state: %+v != %+v", i, left, right)
		}
	}
}
