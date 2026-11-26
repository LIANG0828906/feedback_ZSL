package mirror

import (
	"bytes"
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func mustGenerate(t *testing.T, seed int64) *GeneratedSample {
	t.Helper()
	sample, err := Generate(seed)
	if err != nil {
		t.Fatalf("Generate(%d): %v", seed, err)
	}
	return sample
}

func mutateDoc(t *testing.T, data []byte, fn func(root map[string]any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("decode mutation target: %v", err)
	}
	fn(root)
	out, err := json.Marshal(root)
	if err != nil {
		t.Fatalf("encode mutation target: %v", err)
	}
	return out
}

func TestGeneratedSchemasAreDeterministicAndEquivalent(t *testing.T) {
	a := mustGenerate(t, 42)
	b := mustGenerate(t, 42)
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		if !bytes.Equal(a.Documents[version], b.Documents[version]) {
			t.Fatalf("schema %s is not deterministic", version)
		}
		if a.Hashes[version] != b.Hashes[version] {
			t.Fatalf("schema %s hash changed", version)
		}
		loaded, err := LoadVersion(a.Documents[version], version)
		if err != nil {
			t.Fatalf("LoadVersion(%s): %v", version, err)
		}
		if loaded.SourceVersion != version {
			t.Fatalf("source version = %q, want %q", loaded.SourceVersion, version)
		}
		if path, av, bv, diff := FirstCoreDifference(a.Session, loaded); diff {
			t.Fatalf("schema %s mismatch at %s: %v != %v", version, path, av, bv)
		}
		if len(loaded.Traces) == 0 {
			t.Fatalf("schema %s produced no conversion traces", version)
		}
		if _, _, _, err := RoundTripSession(loaded, version); err != nil {
			t.Fatalf("round-trip %s: %v", version, err)
		}
	}
	truthA, _ := json.Marshal(a.Truth)
	truthB, _ := json.Marshal(b.Truth)
	if !bytes.Equal(truthA, truthB) {
		t.Fatal("truth list is not deterministic")
	}
}

func TestV1UpgradeFillsDeclaredDefaultsWithTrace(t *testing.T) {
	sample := mustGenerate(t, 7)
	loaded, err := LoadVersion(sample.Documents[SchemaV1], SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var defaultScaleTrace, defaultChromaTrace *ConversionTrace
	for i := range loaded.Traces {
		tr := &loaded.Traces[i]
		if strings.HasSuffix(tr.Path, ".default_scale") && strings.Contains(tr.Rule, "fill from defaults.scale") {
			defaultScaleTrace = tr
		}
		if strings.HasSuffix(tr.Path, ".default_chroma") && strings.Contains(tr.Rule, "fill from defaults.chroma") {
			defaultChromaTrace = tr
		}
	}
	if defaultScaleTrace == nil || defaultChromaTrace == nil {
		t.Fatalf("missing default-fill traces: %+v", loaded.Traces)
	}
	if defaultScaleTrace.OldValue != nil {
		t.Fatalf("default fill old value = %#v, want nil", defaultScaleTrace.OldValue)
	}
}

func TestSchemaErrorsCarryPaths(t *testing.T) {
	sample := mustGenerate(t, 42)
	tests := []struct {
		name string
		data []byte
		path string
	}{
		{
			name: "unknown version",
			data: []byte(`{"schema_version":"9.0"}`),
			path: "schema_version",
		},
		{
			name: "missing required",
			data: mutateDoc(t, sample.Documents[SchemaV1], func(root map[string]any) {
				delete(root, "session_id")
			}),
			path: "session_id",
		},
		{
			name: "v1 enum out of range",
			data: mutateDoc(t, sample.Documents[SchemaV1], func(root map[string]any) {
				root["screens"].([]any)[0].(map[string]any)["scale_profiles"] = []any{"150%"}
			}),
			path: "scale_profiles",
		},
		{
			name: "v2 enum out of range",
			data: mutateDoc(t, sample.Documents[SchemaV2], func(root map[string]any) {
				root["windows"].([]any)[0].(map[string]any)["chroma"] = json.Number("9")
			}),
			path: "chroma",
		},
		{
			name: "v3 enum out of range",
			data: mutateDoc(t, sample.Documents[SchemaV3], func(root map[string]any) {
				root["windows"].([]any)[0].(map[string]any)["scale"] = "S175"
			}),
			path: "scale",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(tc.data)
			if err == nil {
				t.Fatal("expected error")
			}
			e, ok := AsError(err)
			if !ok || e.Code != CodeSchema {
				t.Fatalf("error = %v, want E_SCHEMA", err)
			}
			if !strings.Contains(e.Path, tc.path) {
				t.Fatalf("path = %q, want contains %q", e.Path, tc.path)
			}
		})
	}
}

func TestLossyExportsListFirstUnrepresentableField(t *testing.T) {
	sample := mustGenerate(t, 42)

	t.Run("v1 scale", func(t *testing.T) {
		s := cloneSession(sample.Session)
		s.Screens[0].ScaleCapabilities = append(s.Screens[0].ScaleCapabilities, Scale150)
		data, losses, err := Export(s, SchemaV1)
		if err == nil || data != nil {
			t.Fatal("expected lossy export failure")
		}
		if !IsCode(err, CodeLossy) || len(losses) == 0 {
			t.Fatalf("error = %v losses=%v", err, losses)
		}
		if !strings.Contains(losses[0].Path, "scale_profiles") {
			t.Fatalf("first loss path = %q", losses[0].Path)
		}
	})

	t.Run("v1 chroma", func(t *testing.T) {
		s := cloneSession(sample.Session)
		s.Screens[0].ChromaCapabilities = append(s.Screens[0].ChromaCapabilities, Chroma422)
		_, losses, err := Export(s, SchemaV1)
		if err == nil || !IsCode(err, CodeLossy) {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(losses[0].Path, "chroma_modes") {
			t.Fatalf("first loss path = %q", losses[0].Path)
		}
	})

	t.Run("v2 offset", func(t *testing.T) {
		s := cloneSession(sample.Session)
		s.Windows[0].Region.X = 1 << 40
		_, losses, err := Export(s, SchemaV2)
		if err == nil || !IsCode(err, CodeLossy) {
			t.Fatalf("error = %v", err)
		}
		if !strings.Contains(losses[0].Path, "region") {
			t.Fatalf("first loss path = %q", losses[0].Path)
		}
	})
}

func TestHigherVersionEnumsRoundTrip(t *testing.T) {
	sample := mustGenerate(t, 42)
	s := cloneSession(sample.Session)
	s.Screens[0].ScaleCapabilities = append(s.Screens[0].ScaleCapabilities, Scale150)
	s.Screens[0].ChromaCapabilities = append(s.Screens[0].ChromaCapabilities, Chroma422)
	s.Windows[0].Scale = Scale150
	s.Windows[0].Chroma = Chroma422
	if err := NormalizeSession(s); err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{SchemaV2, SchemaV3} {
		loaded, _, _, err := RoundTripSession(s, version)
		if err != nil {
			t.Fatalf("round-trip %s: %v", version, err)
		}
		if loaded.Windows[0].Scale != Scale150 || loaded.Windows[0].Chroma != Chroma422 {
			t.Fatalf("%s did not restore scale/chroma", version)
		}
	}
	if _, _, _, err := RoundTripSession(s, SchemaV1); !IsCode(err, CodeLossy) {
		t.Fatalf("v1 round-trip error = %v, want E_LOSSY", err)
	}
}

func TestV3OnlyEnumsAreLossyForLowerVersions(t *testing.T) {
	sample := mustGenerate(t, 42)
	s := cloneSession(sample.Session)
	s.Screens[0].ScaleCapabilities = append(s.Screens[0].ScaleCapabilities, Scale200)
	s.Screens[0].ChromaCapabilities = append(s.Screens[0].ChromaCapabilities, Chroma411)
	s.Windows[0].Scale = Scale200
	s.Windows[0].Chroma = Chroma411
	if err := NormalizeSession(s); err != nil {
		t.Fatal(err)
	}
	loaded, _, _, err := RoundTripSession(s, SchemaV3)
	if err != nil {
		t.Fatalf("v3 round-trip: %v", err)
	}
	if loaded.Windows[0].Scale != Scale200 || loaded.Windows[0].Chroma != Chroma411 {
		t.Fatalf("v3 enums not restored: %+v", loaded.Windows[0])
	}
	if _, losses, err := Export(s, SchemaV2); !IsCode(err, CodeLossy) || len(losses) == 0 {
		t.Fatalf("v2 export error=%v losses=%v", err, losses)
	}
}

func TestMillisecondTimeRoundingAndResidualTrace(t *testing.T) {
	sample := mustGenerate(t, 42)
	base := mutateDoc(t, sample.Documents[SchemaV2], func(root map[string]any) {
		root["rounding"].(map[string]any)["time"] = "floor"
		root["timebase"].(map[string]any)["negotiationTimeoutMillis"] = json.Number("805")
		events := root["events"].([]any)
		events[0].(map[string]any)["atMillis"] = json.Number("65")
		windows := root["windows"].([]any)
		windows[0].(map[string]any)["startMillis"] = json.Number("205")
		windows[0].(map[string]any)["endMillis"] = json.Number("306")
	})
	floor, err := LoadVersion(base, SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	ev := eventByID(t, floor, "ev-0000")
	if ev.AtTick != 6 {
		t.Fatalf("floor tick = %d, want 6", ev.AtTick)
	}
	if floor.NegotiationTimeoutTicks != 80 {
		t.Fatalf("floor timeout = %d, want 80", floor.NegotiationTimeoutTicks)
	}
	if floor.Windows[0].SourceTimeResidualMs != 5+6 {
		t.Fatalf("residual ms = %d, want 11", floor.Windows[0].SourceTimeResidualMs)
	}
	if !hasTraceResidual(floor, "events[0].atMillis", 5) {
		t.Fatal("missing atMillis residual trace")
	}

	ceilDoc := mutateDoc(t, base, func(root map[string]any) {
		root["rounding"].(map[string]any)["time"] = "ceil"
	})
	ceil, err := LoadVersion(ceilDoc, SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	ev = eventByID(t, ceil, "ev-0000")
	if ev.AtTick != 7 {
		t.Fatalf("ceil tick = %d, want 7", ev.AtTick)
	}
	if ceil.NegotiationTimeoutTicks != 81 {
		t.Fatalf("ceil timeout = %d, want 81", ceil.NegotiationTimeoutTicks)
	}
	if !hasTraceResidual(ceil, "events[0].atMillis", -5) {
		t.Fatal("missing ceil residual trace")
	}

	v3Doc := mutateDoc(t, sample.Documents[SchemaV3], func(root map[string]any) {
		root["rounding"].(map[string]any)["time"] = "ceil"
		windows := root["windows"].([]any)
		windows[0].(map[string]any)["time"].(map[string]any)["start"] = map[string]any{
			"sec": json.Number("0"), "ms": json.Number("205"),
		}
	})
	v3, err := LoadVersion(v3Doc, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if v3.Windows[0].StartTick != 21 {
		t.Fatalf("v3 ceil start tick = %d, want 21", v3.Windows[0].StartTick)
	}
}

func TestCrossVersionRoundTripRestoresCoordinatesTimesAndEnums(t *testing.T) {
	sample := mustGenerate(t, 42)
	low, _, err := Export(sample.Session, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := LoadVersion(low, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	high, _, err := Export(v1, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	v3, err := LoadVersion(high, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if path, av, bv, diff := FirstCoreDifference(sample.Session, v3); diff {
		t.Fatalf("V3 -> V1 -> V3 mismatch at %s: %v != %v", path, av, bv)
	}

	v2Data, _, err := Export(sample.Session, SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := LoadVersion(v2Data, SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	back, _, err := Export(v2, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	v3Again, err := LoadVersion(back, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if path, av, bv, diff := FirstCoreDifference(sample.Session, v3Again); diff {
		t.Fatalf("V3 -> V2 -> V3 mismatch at %s: %v != %v", path, av, bv)
	}
}

func eventByID(t *testing.T, s *Session, id string) Event {
	t.Helper()
	for _, ev := range s.Events {
		if ev.ID == id {
			return ev
		}
	}
	t.Fatalf("event %s not found", id)
	return Event{}
}

func hasTraceResidual(s *Session, path string, residual int64) bool {
	for _, tr := range s.Traces {
		if tr.Path == path && tr.Residual == residual {
			return true
		}
	}
	return false
}

func TestIntegerConversionsAndOverflow(t *testing.T) {
	if got := FloorDiv(-5, 2); got != -3 {
		t.Fatalf("FloorDiv(-5,2) = %d", got)
	}
	floor, r, err := MulDivChecked("x", -5, 1, 2, RoundFloor)
	if err != nil || floor != -3 || r != 1 {
		t.Fatalf("floor = %d residual=%d err=%v", floor, r, err)
	}
	ceil, r, err := MulDivChecked("x", -5, 1, 2, RoundCeil)
	if err != nil || ceil != -2 || r != -1 {
		t.Fatalf("ceil = %d residual=%d err=%v", ceil, r, err)
	}
	_, _, err = MulDivChecked("window.region.x", math.MaxInt64, 2, 1, RoundFloor)
	if !IsCode(err, CodeOverflow) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
	e, _ := AsError(err)
	if len(e.Operands) != 2 || e.Operands[0] != math.MaxInt64 || e.Operands[1] != 2 {
		t.Fatalf("overflow operands = %v", e.Operands)
	}
	if _, err := AddChecked("x", math.MaxInt64, 1); !IsCode(err, CodeOverflow) {
		t.Fatalf("AddChecked overflow = %v", err)
	}
	if _, err := SubChecked("x", math.MinInt64, 1); !IsCode(err, CodeOverflow) {
		t.Fatalf("SubChecked overflow = %v", err)
	}
}
