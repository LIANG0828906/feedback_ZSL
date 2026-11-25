package schema_test

import (
	"encoding/json"
	"errors"
	"math"
	"testing"

	"obsproj/internal/model"
	"obsproj/internal/runner"
	"obsproj/internal/scenario"
	"obsproj/internal/schema"
)

func TestThreeSchemasLoadToSameModelAndReplay(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	artifacts := map[string][]byte{
		schema.Version1: bundle.V1,
		schema.Version2: bundle.V2,
		schema.Version3: bundle.V3,
	}
	wantHash := bundle.Session.Fingerprint()
	results := map[string]model.RunResult{}
	for version, data := range artifacts {
		loaded, decodeErr := schema.Decode(data)
		if decodeErr != nil {
			t.Fatalf("%s decode: %v", version, decodeErr)
		}
		if loaded.SourceVersion != version {
			t.Fatalf("%s source version=%q", version, loaded.SourceVersion)
		}
		if got := loaded.Fingerprint(); got != wantHash {
			t.Fatalf("%s fingerprint=%s want=%s", version, got, wantHash)
		}
		result, runErr := runner.RunFullReplay(loaded, runner.DefaultOptions())
		if runErr != nil {
			t.Fatalf("%s replay: %v", version, runErr)
		}
		results[version] = result
	}
	if diff := runner.CompareResults(results[schema.Version1], results[schema.Version2]); diff != nil {
		t.Fatalf("v1/v2 replay diff: %v", diff)
	}
	if diff := runner.CompareResults(results[schema.Version1], results[schema.Version3]); diff != nil {
		t.Fatalf("v1/v3 replay diff: %v", diff)
	}
}

func TestV1UpgradeDefaultsAreTraced(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	loaded, decodeErr := schema.Decode(bundle.V1)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if len(loaded.UpgradeTraces) != len(loaded.Screens) {
		t.Fatalf("traces=%d screens=%d", len(loaded.UpgradeTraces), len(loaded.Screens))
	}
	for _, trace := range loaded.UpgradeTraces {
		if trace.OldValue != "<missing>" {
			t.Fatalf("old value=%q", trace.OldValue)
		}
		if trace.Source != "schema:v1.defaultMaxPacketSize" {
			t.Fatalf("source=%q", trace.Source)
		}
	}
}

func TestV2OptionalMaxPacketDefaultIsTraced(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	mutated := mutateJSON(t, bundle.V2, func(root map[string]any) {
		screen := root["screens"].([]any)[0].(map[string]any)
		delete(screen, "maxPacketSize")
	})
	loaded, decodeErr := schema.Decode(mutated)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	if len(loaded.UpgradeTraces) != 1 {
		t.Fatalf("traces=%d", len(loaded.UpgradeTraces))
	}
	trace := loaded.UpgradeTraces[0]
	if trace.Source != "schema:v2.defaultMaxPacketSize" || trace.DefaultValue != "32768" {
		t.Fatalf("unexpected trace: %+v", trace)
	}
	if loaded.Screens[0].Capability.MaxPacketSize != model.DefaultMaxPacketSize {
		t.Fatal("default packet size was not applied")
	}
}

func TestSchemaDiagnostics(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	t.Run("unknown version", func(t *testing.T) {
		_, decodeErr := schema.Decode([]byte(`{"schema_version":"v9"}`))
		assertCodeFieldVersion(t, decodeErr, model.CodeSchema, "schema_version", "v9")
	})
	t.Run("decode", func(t *testing.T) {
		_, decodeErr := schema.Decode([]byte(`{"schema_version":`))
		assertCode(t, decodeErr, model.CodeDecode)
	})
	t.Run("missing required", func(t *testing.T) {
		mutated := mutateJSON(t, bundle.V2, func(root map[string]any) {
			screen := root["screens"].([]any)[0].(map[string]any)
			delete(screen, "originX")
		})
		_, decodeErr := schema.Decode(mutated)
		assertCodeFieldVersion(t, decodeErr, model.CodeSchema, "screens[0].originX", schema.Version2)
	})
	t.Run("overflow", func(t *testing.T) {
		mutated := mutateJSON(t, bundle.V2, func(root map[string]any) {
			screen := root["screens"].([]any)[0].(map[string]any)
			screen["originX"] = int64(math.MaxInt64)
		})
		_, decodeErr := schema.Decode(mutated)
		assertCodeFieldVersion(t, decodeErr, model.CodeOverflow, "screens[0].originX", schema.Version2)
		kerr, _ := model.AsKernelError(decodeErr)
		if len(kerr.Operands) != 2 {
			t.Fatalf("overflow operands=%v", kerr.Operands)
		}
	})
	t.Run("unknown enum", func(t *testing.T) {
		mutated := mutateJSON(t, bundle.V3, func(root map[string]any) {
			screen := root["screens"].([]any)[0].(map[string]any)
			screen["sc"] = 99
		})
		_, decodeErr := schema.Decode(mutated)
		assertCodeFieldVersion(t, decodeErr, model.CodeSchema, "screens[0].sc", schema.Version3)
	})
	t.Run("v1 unrepresentable enum", func(t *testing.T) {
		mutated := mutateJSON(t, bundle.V1, func(root map[string]any) {
			screen := root["screens"].([]any)[0].(map[string]any)
			screen["scale"] = "1.25x"
		})
		_, decodeErr := schema.Decode(mutated)
		assertCodeFieldVersion(t, decodeErr, model.CodeSchema, "screens[0].scale", schema.Version1)
	})
	t.Run("v3 max packet required", func(t *testing.T) {
		mutated := mutateJSON(t, bundle.V3, func(root map[string]any) {
			screen := root["screens"].([]any)[0].(map[string]any)
			capability := screen["cap"].(map[string]any)
			delete(capability, "mps")
		})
		_, decodeErr := schema.Decode(mutated)
		assertCodeFieldVersion(t, decodeErr, model.CodeSchema, "screens[0].cap.mps", schema.Version3)
	})
}

func TestExportsReloadToSameModel(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{schema.Version1, schema.Version2, schema.Version3} {
		exported, exportErr := schema.Export(bundle.Session, version)
		if exportErr != nil {
			t.Fatalf("%s export: %v", version, exportErr)
		}
		loaded, decodeErr := schema.Decode(exported.Data)
		if decodeErr != nil {
			t.Fatalf("%s reload: %v", version, decodeErr)
		}
		if loaded.Fingerprint() != bundle.Session.Fingerprint() {
			t.Fatalf("%s round trip changed semantic model", version)
		}
	}
}

func TestHighToLowToHighKeepsMappedFrames(t *testing.T) {
	bundle, err := scenario.Generate(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	high, decodeErr := schema.Decode(bundle.V3)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	highResult, runErr := runner.RunFullReplay(high, runner.DefaultOptions())
	if runErr != nil {
		t.Fatal(runErr)
	}
	for _, target := range []string{schema.Version1, schema.Version2} {
		low, exportErr := schema.Export(high, target)
		if exportErr != nil {
			t.Fatalf("%s export: %v", target, exportErr)
		}
		roundTripped, lowErr := schema.Decode(low.Data)
		if lowErr != nil {
			t.Fatalf("%s decode: %v", target, lowErr)
		}
		if roundTripped.Fingerprint() != high.Fingerprint() {
			t.Fatalf("%s round trip changed mapping parameters", target)
		}
		lowResult, replayErr := runner.RunFullReplay(roundTripped, runner.DefaultOptions())
		if replayErr != nil {
			t.Fatalf("%s replay: %v", target, replayErr)
		}
		if diff := runner.CompareResults(highResult, lowResult); diff != nil {
			t.Fatalf("%s mapped-frame diff: %v", target, diff)
		}
	}
}

func TestLossyExportIsLoadableAndTraced(t *testing.T) {
	bundle, err := scenario.GenerateLossy(scenario.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.V1Err == nil || bundle.V1Err.Code != model.CodeLossy {
		t.Fatalf("expected E_LOSSY, got %v", bundle.V1Err)
	}
	if len(bundle.V1Err.Losses) == 0 {
		t.Fatal("lossy export has no loss list")
	}
	loaded, decodeErr := schema.Decode(bundle.V1.Data)
	if decodeErr != nil {
		t.Fatalf("lossy v1 artifact must load: %v", decodeErr)
	}
	screen := loaded.Screens[0]
	if screen.Scale != model.Scale100 || screen.Chroma != model.Chroma422 {
		t.Fatalf("degradation not applied: scale=%v chroma=%v", screen.Scale, screen.Chroma)
	}
	for _, loss := range bundle.V1Err.Losses {
		if loss.Path == "" || loss.OldValue == "" || loss.NewValue == "" || loss.Disposition == "" {
			t.Fatalf("incomplete loss trace: %+v", loss)
		}
	}
}

func TestIntegerConversionsAndOverflow(t *testing.T) {
	internalRate, err := model.InternalTickRate(1200)
	if err != nil {
		t.Fatal(err)
	}
	ticks, ovf := model.MillisToInternal("at_ms", 125, internalRate)
	if ovf != nil || ticks != 750 {
		t.Fatalf("ms conversion ticks=%d err=%v", ticks, ovf)
	}
	ms, residual, convErr := model.InternalToMillis(751, internalRate)
	if convErr != nil || ms != 125 || residual != 1 {
		t.Fatalf("floor conversion ms=%d residual=%d err=%v", ms, residual, convErr)
	}
	offset, offsetErr := model.AbsoluteFromOffset("region.offsetX", 320000, -420)
	if offsetErr != nil || offset != -100000 {
		t.Fatalf("offset conversion=%d err=%v", offset, offsetErr)
	}
	units, coordinateResidual, coordinateErr := model.OffsetFromAbsolute("region.offsetX", -1, 0)
	if coordinateErr != nil || units != -1 || coordinateResidual != 999 {
		t.Fatalf("floor offset=%d residual=%d err=%v", units, coordinateResidual, coordinateErr)
	}
	_, overflowErr := model.UnitsToInternal("origin_x", math.MaxInt64)
	assertCode(t, overflowErr, model.CodeOverflow)
}

func mutateJSON(t *testing.T, data []byte, mutate func(map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	mutate(root)
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertCode(t *testing.T, err error, code model.Code) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("expected %s, got %T: %v", code, err, err)
	}
	if kernelErr.Code != code {
		t.Fatalf("code=%s want=%s (%v)", kernelErr.Code, code, err)
	}
}

func assertCodeFieldVersion(t *testing.T, err error, code model.Code, field, version string) {
	t.Helper()
	var kernelErr *model.KernelError
	if !errors.As(err, &kernelErr) {
		t.Fatalf("expected %s, got %T: %v", code, err, err)
	}
	if kernelErr.Code != code {
		t.Fatalf("code=%s want=%s (%v)", kernelErr.Code, code, err)
	}
	if kernelErr.Field != field {
		t.Fatalf("field=%q want=%q (%v)", kernelErr.Field, field, err)
	}
	if kernelErr.Version != version {
		t.Fatalf("version=%q want=%q (%v)", kernelErr.Version, version, err)
	}
}
