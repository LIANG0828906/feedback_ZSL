package terrainbake

import (
	"context"
	"errors"
	"testing"
)

func TestGenerateProjectFullBakeAndTruth(t *testing.T) {
	p, err := GenerateProject(42)
	if err != nil {
		t.Fatalf("GenerateProject: %v", err)
	}
	opts := DefaultEngineOptions(t.TempDir())
	e, err := NewEngine(p, opts)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	res, err := e.FullBake(context.Background())
	if err != nil {
		t.Fatalf("FullBake: %v", err)
	}
	if res.Paused || len(res.Baked) != len(p.Blocks) {
		t.Fatalf("unexpected result: %+v", res)
	}
	checked, err := e.VerifyTruth()
	if err != nil {
		t.Fatalf("VerifyTruth: %v", err)
	}
	if checked == 0 {
		t.Fatal("no truth samples checked")
	}
}

func TestThreePathsWithEdits(t *testing.T) {
	p, err := GenerateProject(7)
	if err != nil {
		t.Fatalf("GenerateProject: %v", err)
	}
	edits := []Edit{
		{Kind: EditHeight, BlockID: "tl", X: 5, Y: 4, Value: 4.75},
		{Kind: EditMaterial, MaterialID: "grass", Field: "normal_scale", Value: 1.35},
	}
	opts := DefaultEngineOptions(t.TempDir())
	opts.MaxWorkers = 3
	opts.NormalMapMemoryBytes = 16 * 1024 * 1024
	report, err := VerifyThreePaths(context.Background(), p, edits, t.TempDir(), opts)
	if err != nil {
		if be, ok := AsError(err); ok {
			t.Fatalf("VerifyThreePaths: %s block=%s xy=(%d,%d): %v", be.Code, be.BlockID, be.X, be.Y, err)
		}
		t.Fatalf("VerifyThreePaths: %v", err)
	}
	if !report.Equal {
		t.Fatal("three paths differ")
	}
}

func TestParamErrorCode(t *testing.T) {
	p, err := GenerateProject(1)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultEngineOptions(t.TempDir())
	opts.Params.Operator = "bad"
	_, err = NewEngine(p, opts)
	if err == nil {
		t.Fatal("expected error")
	}
	be, ok := AsError(err)
	if !ok || be.Code != CodeParam || be.Field != "operator" {
		t.Fatalf("unexpected error: %#v", err)
	}
	if !errors.Is(err, err) {
		t.Fatal("unreachable")
	}
}
