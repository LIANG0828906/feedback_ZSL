package schema_test

import (
	"bytes"
	"testing"

	"cadpipe/internal/cad"
	"cadpipe/internal/sample"
	"cadpipe/internal/schema"
)

func TestThreeVersionRoundTripAndDeterministicExport(t *testing.T) {
	bundle, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{bundle.SchemaV1, bundle.SchemaV2, bundle.SchemaV3} {
		loaded, err := schema.Load(data)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if loaded.Scene.Schema == "" {
			t.Fatal("source version not preserved")
		}
		for _, target := range []string{schema.V1, schema.V2, schema.V3} {
			first, loss, err := schema.Export(loaded.Scene, target)
			if err != nil {
				t.Fatal(err)
			}
			second, _, err := schema.Export(loaded.Scene, target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("non-deterministic export for %s", target)
			}
			reloaded, err := schema.Load(first)
			if err != nil {
				t.Fatalf("reload %s: %v", target, err)
			}
			assertPatchesEqual(t, loaded.Scene, reloaded.Scene)
			if target != schema.V3 && !loss.HasLoss() {
				t.Fatalf("expected E_LOSSY entries for %s", target)
			}
			for _, entry := range loss.Entries {
				if entry.Code != cad.CodeLossy {
					t.Fatalf("loss entry code = %q", entry.Code)
				}
			}
		}
	}
}

func TestUnitRoundTripKeepsCanonicalLengths(t *testing.T) {
	bundle, _ := sample.Generate(sample.DefaultSeed)
	loaded, err := schema.Load(bundle.SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	v1, _, err := schema.Export(loaded.Scene, schema.V1)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := schema.Load(v1)
	if err != nil {
		t.Fatal(err)
	}
	for i := range loaded.Scene.Patches {
		a := loaded.Scene.Patches[i]
		b := reloaded.Scene.Patches[i]
		for u := range a.ControlPoints {
			for v := range a.ControlPoints[u] {
				if a.ControlPoints[u][v] != b.ControlPoints[u][v] {
					t.Fatalf("length mismatch %s [%d][%d]: %+v != %+v",
						a.ID, u, v, a.ControlPoints[u][v], b.ControlPoints[u][v])
				}
			}
		}
	}
}

func TestUnknownVersionAndMissingFieldAreEVer(t *testing.T) {
	_, err := schema.Load([]byte(`{"schema":"cad.scene/v9"}`))
	ce, ok := cad.AsError(err)
	if !ok || ce.Code != cad.CodeVersion || ce.Version != "cad.scene/v9" || ce.Path != "$.schema" {
		t.Fatalf("unknown version error = %v", err)
	}

	_, err = schema.Load([]byte(`{"schema":"cad.scene/v1","scene_id":"s","unit":"mm","patches":[]}`))
	ce, ok = cad.AsError(err)
	if !ok || ce.Code != cad.CodeVersion || ce.Version != schema.V1 {
		t.Fatalf("missing field error = %v", err)
	}
	if ce.Path != "$.patches" {
		t.Fatalf("path = %q", ce.Path)
	}
}

func TestLowVersionUpgradeTranscribesFilledFields(t *testing.T) {
	bundle, _ := sample.Generate(sample.DefaultSeed)
	loaded, err := schema.Load(bundle.SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Transcript == nil || len(loaded.Transcript.Entries) == 0 {
		t.Fatal("expected transcription entries")
	}
	found := false
	for _, entry := range loaded.Transcript.Entries {
		if entry.Path == "$.patches[0].material" && entry.Original == nil {
			found = true
		}
	}
	if !found {
		t.Fatalf("material fill trace missing: %+v", loaded.Transcript.Entries)
	}
}

func assertPatchesEqual(t *testing.T, a, b *cad.Scene) {
	t.Helper()
	if len(a.Patches) != len(b.Patches) {
		t.Fatalf("patch count %d != %d", len(a.Patches), len(b.Patches))
	}
	for i := range a.Patches {
		pa, pb := a.Patches[i], b.Patches[i]
		if pa.ID != pb.ID || pa.DegreeU != pb.DegreeU || pa.DegreeV != pb.DegreeV {
			t.Fatalf("patch header mismatch: %+v vs %+v", pa, pb)
		}
		if len(pa.KnotsU) != len(pb.KnotsU) {
			t.Fatal("knot U length mismatch")
		}
		for k := range pa.KnotsU {
			if pa.KnotsU[k] != pb.KnotsU[k] {
				t.Fatalf("knot U[%d] %v != %v", k, pa.KnotsU[k], pb.KnotsU[k])
			}
		}
		if cad.HashJSON(pa.ControlPoints) != cad.HashJSON(pb.ControlPoints) ||
			cad.HashJSON(pa.Weights) != cad.HashJSON(pb.Weights) ||
			cad.HashJSON(pa.Clip) != cad.HashJSON(pb.Clip) {
			t.Fatalf("geometry differs after round trip for %s", pa.ID)
		}
	}
}
