package fogkernel

import (
	"math"
	"testing"
)

func TestThreeSchemaDocumentsLoadEquivalent(t *testing.T) {
	bundle, err := GenerateBundle(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	scenes := map[string]*Scene{}
	for _, schema := range []string{SchemaV1, SchemaV2, SchemaV3} {
		scene, err := LoadProject(bundle.Docs[schema])
		if err != nil {
			t.Fatalf("load %s: %v", schema, err)
		}
		scenes[schema] = scene
		if !ScenesEquivalent(bundle.Canonical, scene) {
			t.Fatalf("%s not equivalent to canonical", schema)
		}
	}
	v1 := scenes[SchemaV1]
	v2 := scenes[SchemaV2]
	for i := range v1.Media {
		if v1.Media[i].SigmaAQ != v2.Media[i].SigmaAQ || v1.Media[i].SigmaSQ != v2.Media[i].SigmaSQ {
			t.Fatalf("coefficient Q mismatch")
		}
		if v1.Media[i].MinMicro != v2.Media[i].MinMicro || len(v1.Media[i].Density.Values) != len(v2.Media[i].Density.Values) {
			t.Fatalf("distance/density mismatch")
		}
		for j := range v1.Media[i].Density.Values {
			if v1.Media[i].Density.Values[j] != v2.Media[i].Density.Values[j] {
				t.Fatalf("voxel %d mismatch", j)
			}
		}
	}
}

func TestStableSchemaAndDensityErrors(t *testing.T) {
	if _, err := LoadProject([]byte(`{"Schema":"fog.project/v9"}`)); err == nil {
		t.Fatal("expected unknown schema error")
	} else if biz, ok := AsBizError(err); !ok || biz.Code != CodeSchemaVersion {
		t.Fatalf("expected E_SCHEMA_VERSION, got %v", err)
	}

	bundle, _ := GenerateDefaultBundle()
	_, err := RunInjection(bundle, InjectionRequest{Kind: "density_shape_mismatch"})
	if biz, ok := AsBizError(err); !ok || biz.Code != CodeDensityShape {
		t.Fatalf("expected E_DENSITY_SHAPE, got %v", err)
	}
	_, err = RunInjection(bundle, InjectionRequest{Kind: "density_out_of_range"})
	if biz, ok := AsBizError(err); !ok || biz.Code != CodeDensityRange {
		t.Fatalf("expected E_DENSITY_RANGE, got %v", err)
	}
	_, err = flatDensity([3]int{math.MaxInt64, 1, 1}, nil, 255, "$.huge")
	if biz, ok := AsBizError(err); !ok || biz.Code != CodeDensityShape {
		t.Fatalf("expected safe E_DENSITY_SHAPE for overflow dimensions, got %v", err)
	}
}

func TestFixedConversionsOverflowAndEquivalence(t *testing.T) {
	q1, err := CoefficientToQ(123, ScalePermille, "$.a")
	if err != nil || q1 != 123_000 {
		t.Fatalf("permille Q = %d, %v", q1, err)
	}
	q2, err := CoefficientToQ(1230, ScalePermyriad, "$.b")
	if err != nil || q2 != q1 {
		t.Fatalf("permyriad equivalence failed: %d vs %d", q2, q1)
	}
	if _, err := CoefficientToQ(-1, ScalePermille, "$.neg"); err == nil {
		t.Fatal("expected negative coefficient error")
	} else if biz, ok := AsBizError(err); !ok || biz.Code != CodeCoeffNegative {
		t.Fatalf("expected E_COEFF_NEGATIVE, got %v", err)
	}
	if _, err := CoefficientToQ(math.MaxInt64, ScalePermille, "$.over"); err == nil {
		t.Fatal("expected overflow")
	} else if biz, ok := AsBizError(err); !ok || biz.Code != CodeFixedOverflow {
		t.Fatalf("expected E_FIXED_OVERFLOW, got %v", err)
	}

	mm, err := MMToMicro(1234, "$.mm")
	if err != nil || mm != 1_234_000 {
		t.Fatalf("mm conversion %d %v", mm, err)
	}
	m, err := DecimalMetersToMicro("1.234", "$.m")
	if err != nil || m != mm {
		t.Fatalf("meter equivalence %d vs %d err=%v", m, mm, err)
	}
	if _, err := DecimalMetersToMicro("999999999999999999999", "$.big"); err == nil {
		t.Fatal("expected meter overflow")
	} else if biz, ok := AsBizError(err); !ok || biz.Code != CodeFixedOverflow {
		t.Fatalf("expected E_FIXED_OVERFLOW, got %v", err)
	}
	q, overflow, saturated := CoefficientToQSaturated(math.MaxInt64, ScalePermille)
	if !overflow || !saturated || q != math.MaxInt64 {
		t.Fatalf("coefficient saturation mismatch q=%d overflow=%v saturated=%v", q, overflow, saturated)
	}
	micro, overflow, saturated := MMToMicroSaturated(math.MaxInt64)
	if !overflow || !saturated || micro != math.MaxInt64 {
		t.Fatalf("distance saturation mismatch micro=%d overflow=%v saturated=%v", micro, overflow, saturated)
	}
}

func TestParallelSchedulingDoesNotChangeResults(t *testing.T) {
	bundle, _ := GenerateDefaultBundle()
	scene := bundle.Canonical
	base, err := SolveScene(scene, SolveOptions{Path: PathExact, Workers: 1, Buffer: 8, Order: "forward"})
	if err != nil {
		t.Fatal(err)
	}
	variants := []SolveOptions{
		{Path: PathExact, Workers: 4, Buffer: 64, Order: "reverse", BatchSize: 1},
		{Path: PathExact, Workers: 2, Buffer: 2, Order: "reverse", BatchSize: 3},
		{Path: PathDiagnostic, Workers: 3, Buffer: 4, Order: "forward", BatchSize: 5},
	}
	for _, opts := range variants {
		got, err := SolveScene(scene, opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Rays) != len(base.Rays) {
			t.Fatal("ray count changed")
		}
		for ri := range base.Rays {
			a, b := base.Rays[ri], got.Rays[ri]
			if a.Transmittance != b.Transmittance || a.TerminalBlock != b.TerminalBlock || len(a.Blocks) != len(b.Blocks) {
				t.Fatalf("schedule changed ray %d", ri)
			}
			for bi := range a.Blocks {
				x, y := a.Blocks[bi], b.Blocks[bi]
				if x.Tau != y.Tau || x.TOut != y.TOut || x.Energy != y.Energy {
					t.Fatalf("schedule changed ray %d block %d", ri, bi)
				}
			}
		}
	}
}

func TestBudgetAndBackpressureCodes(t *testing.T) {
	_, err := (Budget{Workers: 0, PerWorkerBuffer: 4}).Validate()
	if err == nil {
		t.Fatal("expected worker budget error")
	} else if biz, ok := AsBizError(err); !ok || biz.Code != CodeBudgetWorkers {
		t.Fatalf("expected E_BUDGET_WORKERS, got %v", err)
	}
	_, err = (Budget{Workers: 2, PerWorkerBuffer: MaxDeclaredBuffer + 1}).Validate()
	if err == nil {
		t.Fatal("expected buffer budget error")
	} else if biz, ok := AsBizError(err); !ok || biz.Code != CodeBudgetBuffer {
		t.Fatalf("expected E_BUDGET_BUFFER, got %v", err)
	}
	err = AdmitBlockBatch(Budget{Workers: 1, PerWorkerBuffer: 1}, 2)
	if biz, ok := AsBizError(err); !ok || biz.Code != CodeBackpressure {
		t.Fatalf("expected E_BACKPRESSURE, got %v", err)
	}
	bundle, _ := GenerateDefaultBundle()
	_, err = SolveScene(bundle.Canonical, SolveOptions{Path: PathExact, Workers: 1, Buffer: 1, StrictBackpressure: true})
	if biz, ok := AsBizError(err); !ok || biz.Code != CodeBackpressure {
		t.Fatalf("expected strict solver backpressure, got %v", err)
	}
}
