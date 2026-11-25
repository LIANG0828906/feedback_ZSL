package combatjudge_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"combatjudge/internal/conv"
	"combatjudge/internal/engine"
	"combatjudge/internal/model"
	"combatjudge/internal/sample"
	"combatjudge/internal/schema"
)

func TestExportRoundTripAndLossyManifest(t *testing.T) {
	ctx := context.Background()
	p := sample.BuildProject()
	v1Data, losses, err := schema.Export(p, model.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(losses) == 0 {
		t.Fatal("expected E_LOSSY notices when exporting high version to v1")
	}
	foundLossy := false
	for _, l := range losses {
		if l.Code == model.CodeLossy {
			foundLossy = true
		}
	}
	if !foundLossy {
		t.Fatalf("loss notices missing E_LOSSY: %+v", losses)
	}
	loadedV1, err := schema.Load(v1Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, wantSweep := range p.Sweeps {
		got := loadedV1.SweepByID(wantSweep.ID)
		if got == nil || got.From != wantSweep.From || got.To != wantSweep.To || got.StartFrame != wantSweep.StartFrame || got.EndFrame != wantSweep.EndFrame {
			t.Fatalf("v1 round trip sweep mismatch: %+v vs %+v", got, wantSweep)
		}
	}

	v3Data, _, err := schema.Export(loadedV1, model.SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	recoveredV3, err := schema.Load(v3Data)
	if err != nil {
		t.Fatal(err)
	}
	full, err := engine.Run(ctx, p, engine.Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	roundTripReport, err := engine.Run(ctx, recoveredV3, engine.Options{DBPath: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	if mm := engine.CompareReports(full, roundTripReport); mm != nil {
		t.Fatalf("round-trip report mismatch: %+v", mm)
	}

	v2Data, _, err := schema.Export(p, model.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	loadedV2, err := schema.Load(v2Data)
	if err != nil {
		t.Fatal(err)
	}
	_, v2ToV1Losses, err := schema.Export(loadedV2, model.SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	hasDeltaLoss := false
	for _, l := range v2ToV1Losses {
		if strings.Contains(l.FieldPath, "EndPosQ") && strings.Contains(l.Disposition, "flatten") {
			hasDeltaLoss = true
		}
	}
	if !hasDeltaLoss {
		t.Fatalf("expected incremental endpoint flatten loss: %+v", v2ToV1Losses)
	}

	recoveredQ, raw, remainder, stable, err := conv.RequantizeRoundTrip(1_234_567, convScalePermilleForTest())
	if err != nil {
		t.Fatal(err)
	}
	if recoveredQ != 1_234_000 || raw != 1234 || remainder != 567_000 || !stable {
		t.Fatalf("quantization round trip = q%d raw%d rem%d stable%v", recoveredQ, raw, remainder, stable)
	}
}

func convScalePermilleForTest() int64 { return 1000 }

func TestOverflowReturnsFieldAndOperands(t *testing.T) {
	_, err := conv.CheckedAdd("$.sweeps[0].toQ.x", model.SchemaV3, 9_223_372_036_854_775_807, 1)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeOVF || me.FieldPath == "" || me.Op1 == "" || me.Op2 == "" {
		t.Fatalf("overflow error incomplete: %+v", err)
	}
}

func TestFrameTimeAndDeltaIntegerRoundTrip(t *testing.T) {
	ms, err := conv.FrameToMS(3, 16)
	if err != nil || ms != 32 {
		t.Fatalf("FrameToMS = %d,%v", ms, err)
	}
	frame, residual, err := conv.MSToFrame("$.at_ms", model.SchemaV2, ms, 16, conv.RoundFloor)
	if err != nil || frame != 3 || residual != 0 {
		t.Fatalf("MSToFrame = %d rem%d err%v", frame, residual, err)
	}
	points, traces, err := conv.ExpandDeltas("$.advance_delta_q", model.SchemaV2, model.Vec3{X: 1}, []model.Vec3{{X: 2}, {X: 3}})
	if err != nil || len(points) != 3 || points[2].X != 6 || len(traces) != 2 {
		t.Fatalf("delta expansion = %+v traces=%d err=%v", points, len(traces), err)
	}
}
