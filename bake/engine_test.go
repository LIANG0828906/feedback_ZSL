package bake_test

import (
	"context"
	"math"
	"testing"

	"terrainbake/bake"
	"terrainbake/generator"
	"terrainbake/model"
	"terrainbake/project"
)

func testBundle(t *testing.T) model.SampleBundle {
	t.Helper()
	bundle, err := generator.Generate(424242)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	data, err := generator.MarshalBundle(bundle)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	loaded, err := project.LoadBundle(data)
	if err != nil {
		t.Fatalf("load generated bundle: %v", err)
	}
	return *loaded
}

func TestBakeMatchesTruthAndSeams(t *testing.T) {
	bundle := testBundle(t)
	engine, err := bake.NewEngine(&bundle.Project, nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	result, err := engine.BakeFull(context.Background(), bake.DefaultScheduleOptions(bundle.Project.Params))
	if err != nil {
		t.Fatalf("bake full: %v", err)
	}
	report, err := bake.CompareTruth(result, bundle.Truth, bundle.Project.Params.IrradianceTolerance)
	if err != nil {
		t.Fatalf("truth compare: %v; report=%+v", err, report)
	}
	if report.ShadowMismatches != 0 {
		t.Fatalf("shadow mismatches: %d", report.ShadowMismatches)
	}
	if _, err := bake.AnalyzeSeams(result, engine.Terrain()); err != nil {
		t.Fatalf("seam analysis: %v", err)
	}
}

func TestAnyChunkOrderEqualsWhole(t *testing.T) {
	bundle := testBundle(t)
	engineA, _ := bake.NewEngine(&bundle.Project, nil)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	whole, err := engineA.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("whole: %v", err)
	}
	order := make([]string, 0, len(bundle.Project.Chunks))
	for i := len(bundle.Project.Chunks) - 1; i >= 0; i-- {
		order = append(order, bundle.Project.Chunks[i].ID)
	}
	engineB, _ := bake.NewEngine(&bundle.Project, nil)
	reordered, err := engineB.BakeFullOrder(context.Background(), order, opts)
	if err != nil {
		t.Fatalf("reordered: %v", err)
	}
	if _, err := bake.CompareResults(whole, reordered); err != nil {
		t.Fatalf("order compare: %v", err)
	}
}

func TestDiffAndSeamErrorLocations(t *testing.T) {
	bundle := testBundle(t)
	opts := bake.DefaultScheduleOptions(bundle.Project.Params)
	engine, _ := bake.NewEngine(&bundle.Project, nil)
	result, err := engine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("bake: %v", err)
	}
	otherEngine, _ := bake.NewEngine(&bundle.Project, nil)
	other, err := otherEngine.BakeFull(context.Background(), opts)
	if err != nil {
		t.Fatalf("other bake: %v", err)
	}
	ref := result.CommitSequence[0]
	unit, _ := other.Unit(ref.ChunkID, ref.Time)
	unit.Points[0].IrradianceFixed[1]++
	_, err = bake.CompareResults(result, other)
	coded, ok := model.AsCoded(err)
	if !ok || coded.Code != model.CodeDiff || coded.ChunkID != ref.ChunkID ||
		coded.Point == nil || coded.Field != "irradiance_fixed.g" {
		t.Fatalf("bad E_DIFF context: %+v", err)
	}

	edge := engine.Terrain().Edges()[0]
	sideA, _ := result.Unit(edge.ChunkA, result.CommitSequence[0].Time)
	var mutated *model.PointResult
	for i := range sideA.Points {
		p := &sideA.Points[i]
		onEdge := false
		if edge.Axis == "x" {
			onEdge = math.Abs(p.World.X-edge.Position) < 1e-7 &&
				p.World.Z >= edge.Start-1e-7 && p.World.Z <= edge.End+1e-7
		} else {
			onEdge = math.Abs(p.World.Z-edge.Position) < 1e-7 &&
				p.World.X >= edge.Start-1e-7 && p.World.X <= edge.End+1e-7
		}
		if onEdge {
			mutated = p
			break
		}
	}
	if mutated == nil {
		t.Fatal("no edge point found")
	}
	mutated.DirectShadow = !mutated.DirectShadow
	_, err = bake.AnalyzeSeams(result, engine.Terrain())
	coded, ok = model.AsCoded(err)
	if !ok || coded.Code != model.CodeSeam || coded.EdgeID == "" {
		t.Fatalf("bad E_SEAM context: %+v", err)
	}
}
