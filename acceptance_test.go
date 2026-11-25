package maskqa_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"maskqa/internal/checkpoint"
	"maskqa/internal/deps"
	"maskqa/internal/engine"
	"maskqa/internal/model"
	"maskqa/internal/runner"
	"maskqa/internal/sample"
	"maskqa/internal/schemaio"
	"maskqa/internal/topology"
)

func TestSchemaVersionsAndRoundTrip(t *testing.T) {
	assets := sample.GenerateAll(20261126)
	if len(assets) != 3 {
		t.Fatalf("generated %d assets, want 3", len(assets))
	}
	dir := t.TempDir()
	if _, err := sample.WriteWorkspace(filepath.Join(dir, "data"), 20261126, assets); err != nil {
		t.Fatal(err)
	}

	for _, asset := range assets {
		p := asset.Project
		eng := &schemaio.Engineering{ProjectID: p.ID, Seed: p.Seed, Width: p.Width, Height: p.Height, ChunkSize: p.ChunkSize, Strokes: p.Strokes}
		for _, version := range []string{schemaio.V1, schemaio.V2, schemaio.V3} {
			raw, err := schemaio.MarshalDocument(eng, version)
			if err != nil {
				t.Fatalf("%s marshal: %v", p.ID, err)
			}
			loaded, err := schemaio.LoadEngineering(raw)
			if err != nil {
				t.Fatalf("%s load %s: %v", p.ID, version, err)
			}
			if len(loaded.Strokes) != len(p.Strokes) {
				t.Fatalf("%s %s stroke count=%d want %d", p.ID, version, len(loaded.Strokes), len(p.Strokes))
			}
			for _, original := range p.Strokes {
				rep := schemaio.VerifyRoundTrip(original, version, *eng)
				if !rep.OK {
					t.Fatalf("%s round trip %s stroke=%s: %v", p.ID, version, original.ID, rep.Error)
				}
			}
		}
	}

	if _, err := schemaio.LoadEngineering([]byte(`{"schema_version":"v9","id":"x"}`)); err == nil {
		t.Fatal("unknown schema version unexpectedly accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodeSchema || me.Field != "schema_version" {
			t.Fatalf("unexpected unknown-version error: %v", err)
		}
	}

	missing := []byte(`{"schema":"icon-project/1.0","project_id":"x","width":8,"height":8,"strokes":[{"stroke_id":"s","stroke_no":1,"process_priority":1,"start_x":0,"start_y":0,"end_x":1,"end_y":1}]}`)
	_, err := schemaio.LoadEngineering(missing)
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.CodeSchema || me.Field != "$.strokes[0].coverage_pct" {
		t.Fatalf("expected missing coverage_pct E_SCHEMA path, got %v", err)
	}

	badWeight := []byte(`{"schema_version":"v3","id":"x","width":8,"height":8,"strokes":[{"id":"s","stroke_no":1,"priority":1,"start_xy":[0,0],"end_xy":[1,1],"weight_bp":10001,"hardness_bp":5000}]}`)
	_, err = schemaio.LoadEngineering(badWeight)
	if !errors.As(err, &me) || me.Code != model.CodePara || me.StrokeID != "s" || me.Field != "weight_bp" {
		t.Fatalf("expected E_PARA weight_bp/s, got %v", err)
	}
}

func TestRasterDeterminismAndParameterIsolation(t *testing.T) {
	c := model.Chunk{ID: "c00-00", X0: 0, Y0: 0, X1: 12, Y1: 12}
	s := model.Stroke{ID: "s", No: 1, Priority: 1, From: model.Point{X: 1, Y: 1}, To: model.Point{X: 10, Y: 9}, WidthPx: 3, WeightBP: 8000, HardnessBP: 6000}
	a, err := engine.RasterizeStrokeChunk(s, c, 1024)
	if err != nil {
		t.Fatal(err)
	}
	b, err := engine.RasterizeStrokeChunk(s, c, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("rasterized component is not deterministic")
	}
	bad := s
	bad.HardnessBP = 10001
	if err := engine.ValidateStroke(bad); err == nil {
		t.Fatal("hardness out of range accepted")
	} else {
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.CodePara || me.Field != "hardness_bp" || me.StrokeID != "s" {
			t.Fatalf("unexpected hardness error: %v", err)
		}
	}

	strokes := []model.Stroke{
		{ID: "declared-a", No: 2, Priority: 20},
		{ID: "declared-b", No: 1, Priority: 10},
	}
	ordered := engine.OrderedStrokes(strokes)
	if ordered[0].ID != "declared-b" || ordered[1].ID != "declared-a" {
		t.Fatalf("adjudicated order=%v", []string{ordered[0].ID, ordered[1].ID})
	}
}

func TestThreePathsPixelExact(t *testing.T) {
	assets := sample.GenerateAll(20261126)
	for _, asset := range assets {
		p := asset.Project
		dir := filepath.Join(t.TempDir(), "checkpoints")
		cfg := runner.DefaultConfig("acceptance")
		cfg.MaxParallel = 4
		cfg.QueueBudget = 2
		cfg.KillAfterCommits = 7
		report, err := runner.ExecuteThreePaths(context.Background(), p, dir, cfg)
		if err != nil {
			t.Fatalf("%s three paths: %v", p.ID, err)
		}
		if !report.Comparison.Equal {
			t.Fatalf("%s mismatch: %+v", p.ID, report.Comparison)
		}
		if report.Resume.Topology.DomainCount != p.Truth.DomainCount {
			t.Fatalf("%s domain count=%d want %d", p.ID, report.Resume.Topology.DomainCount, p.Truth.DomainCount)
		}
		if !report.Resume.FinalMask.Equal(p.Reference) {
			x, y, _ := firstMaskDifference(report.Resume.FinalMask, p.Reference)
			t.Fatalf("%s final mask differs from truth at %d,%d", p.ID, x, y)
		}
		if len(report.Resume.CommitSequence) != len(p.Strokes) {
			t.Fatalf("%s commit sequence len=%d want %d", p.ID, len(report.Resume.CommitSequence), len(p.Strokes))
		}
		if report.Killed == nil || len(report.Killed.CommitSequence) != 7 {
			t.Fatalf("kill boundary sequence=%v", report.Killed)
		}
	}
}

func TestIncrementalInvalidationReusesChunks(t *testing.T) {
	p := sample.GenerateAll(20261126)[0].Project
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "incremental.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := runner.DefaultConfig("incremental-full")
	cfg.MaxParallel = 2
	full, err := runner.Run(ctx, p, store, cfg, nil)
	if err != nil {
		t.Fatalf("initial full run: %v", err)
	}

	edited := *p
	edited.Strokes = append([]model.Stroke(nil), p.Strokes...)
	edited.Strokes[0].WeightBP = 2200
	edited.Strokes[0].ContentHash = schemaio.StrokeHash(edited.Strokes[0])
	plan := deps.PlanEdit(p, &edited)
	if len(plan.AffectedStrokes) != 1 || plan.AffectedStrokes[0] != edited.Strokes[0].ID {
		t.Fatalf("affected strokes=%v", plan.AffectedStrokes)
	}
	if len(plan.AffectedChunks) == 0 || len(plan.ReusedChunks) == 0 {
		t.Fatalf("affected=%v reused=%v", plan.AffectedChunks, plan.ReusedChunks)
	}

	inc, runErr := runner.Run(ctx, &edited, store, cfg, &plan)
	if runErr != nil {
		var te *model.Error
		if !errors.As(runErr, &te) || te.Code != model.CodeTop {
			t.Fatalf("incremental run failed before cache validation: %v", runErr)
		}
	}
	reused := map[string]bool{}
	for _, c := range inc.Chunks {
		if c.Reused {
			reused[c.ChunkID] = true
		}
	}
	for _, id := range plan.ReusedChunks {
		if !reused[id] {
			t.Fatalf("chunk %s was not reused; plan reused=%v actual=%v", id, plan.ReusedChunks, reused)
		}
	}
	if len(reused) != len(plan.ReusedChunks) {
		t.Fatalf("reused count=%d want %d", len(reused), len(plan.ReusedChunks))
	}
	_ = full
	_ = store.Close()
}

func firstMaskDifference(a, b model.Mask) (int, int, byte) {
	for i := 0; i < len(a.Pix) && i < len(b.Pix); i++ {
		if a.Pix[i] != b.Pix[i] {
			return i % a.Width, i / a.Width, a.Pix[i]
		}
	}
	return -1, -1, 0
}

func TestDeclarationOrderCanChangeIntermediateCoverage(t *testing.T) {
	c := model.Chunk{ID: "c00-00", X0: 0, Y0: 0, X1: 5, Y1: 5}
	low1 := model.Stroke{ID: "low-1", No: 3, Priority: 30, From: model.Point{X: 0, Y: 2}, To: model.Point{X: 4, Y: 2}, WidthPx: 1, WeightBP: 40, HardnessBP: 10000}
	low2 := model.Stroke{ID: "low-2", No: 1, Priority: 10, From: model.Point{X: 2, Y: 0}, To: model.Point{X: 2, Y: 4}, WidthPx: 1, WeightBP: 40, HardnessBP: 10000}
	high := model.Stroke{ID: "high", No: 2, Priority: 20, From: model.Point{X: 0, Y: 0}, To: model.Point{X: 4, Y: 4}, WidthPx: 1, WeightBP: 2589, HardnessBP: 10000}

	compose := func(order []model.Stroke) byte {
		dst := make([]byte, 25)
		for _, s := range order {
			comp, err := engine.RasterizeStrokeChunk(s, c, 1024)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.ApplyComponent(dst, c, comp); err != nil {
				t.Fatal(err)
			}
		}
		return dst[2*5+2]
	}
	lowFirst := compose([]model.Stroke{low1, low2, high})
	highFirst := compose([]model.Stroke{high, low1, low2})
	if lowFirst == highFirst {
		t.Fatalf("expected integer alpha rounding order sensitivity, both=%d", lowFirst)
	}

	shuffledA := []model.Stroke{low1, high, low2}
	shuffledB := []model.Stroke{high, low2, low1}
	orderedA := engine.OrderedStrokes(shuffledA)
	orderedB := engine.OrderedStrokes(shuffledB)
	if orderedA[0].ID != "low-2" || orderedA[1].ID != "high" || orderedA[2].ID != "low-1" {
		t.Fatalf("unexpected adjudication: %v", orderedA)
	}
	if compose(orderedA) != compose(orderedB) {
		t.Fatal("adjudicated composition depends on input declaration order")
	}
}

func TestRoundAndTopologyErrorsContainRequiredFields(t *testing.T) {
	err := schemaio.CheckWeightRoundTrip("s-round", 8080, 8100, 0)
	var re *model.Error
	if !errors.As(err, &re) || re.Code != model.CodeRound || re.Field != "weight_bp" || re.StrokeID != "s-round" {
		t.Fatalf("unexpected round error: %v", err)
	}
	if re.Original != 8080 || re.Residual != 20 {
		t.Fatalf("round error original/residual=%v/%v", re.Original, re.Residual)
	}

	p := sample.GenerateAll(20261126)[0].Project
	badTruth := p.Truth
	badTruth.Domains = append([]model.DomainReport(nil), p.Truth.Domains...)
	badTruth.Domains[0].Holes++
	terr := topology.VerifyAgainstTruth(badTruth, p.Truth, topology.DefaultTolerances())
	var te *model.Error
	if !errors.As(terr, &te) || te.Code != model.CodeTop || te.Field != "holes" {
		t.Fatalf("unexpected topology error: %v", terr)
	}
	if te.Details["domain"] != 1 || te.Details["measured_holes"] != p.Truth.Domains[0].Holes+1 {
		t.Fatalf("topology error details missing domain/measured values: %#v", te.Details)
	}
}

func TestSchedulerIsolatesInvalidParameterTask(t *testing.T) {
	p := sample.GenerateAll(20261126)[0].Project
	p.Strokes = append([]model.Stroke(nil), p.Strokes...)
	p.Strokes[0].HardnessBP = 10001
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "isolation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	result, runErr := runner.Run(context.Background(), p, store, runner.DefaultConfig("isolation"), nil)
	var pe *model.Error
	if !errors.As(runErr, &pe) || pe.Code != model.CodePara || pe.StrokeID != p.Strokes[0].ID {
		t.Fatalf("expected isolated E_PARA, got result=%v err=%v", result != nil, runErr)
	}
	if result == nil || len(result.IsolationEvents) == 0 {
		t.Fatalf("expected isolation events, got %#v", result)
	}
}
