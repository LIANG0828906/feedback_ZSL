package rebuild_test

import (
	"context"
	"path/filepath"
	"testing"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/audit"
	"acousticfield/internal/checkpoint"
	"acousticfield/internal/demo"
	"acousticfield/internal/deps"
	"acousticfield/internal/engine"
	"acousticfield/internal/generator"
	"acousticfield/internal/rebuild"
	"acousticfield/internal/schema"
)

func TestFixedSeedGeneratorDeterministicAndValid(t *testing.T) {
	a, ta := generator.Generate(generator.DefaultSeed)
	b, tb := generator.Generate(generator.DefaultSeed)
	if acoustic.CanonicalJSON(a) == nil || string(acoustic.CanonicalJSON(a)) != string(acoustic.CanonicalJSON(b)) {
		t.Fatal("generator is not deterministic")
	}
	if ta.Fingerprint != tb.Fingerprint {
		t.Fatal("truth fingerprint is not deterministic")
	}
	if err := schema.ValidateLevel(a); err != nil {
		t.Fatal(err)
	}
}

func TestDemoThreePathsAuditAndRecovery(t *testing.T) {
	out := filepath.Join(t.TempDir(), "demo")
	result, err := demo.Run(out)
	if err != nil {
		t.Fatalf("demo run: %v", err)
	}
	if !result.Comparison.OK {
		t.Fatalf("three-path mismatch: %+v", result.Comparison.First)
	}
	for name, a := range result.Audits {
		if !a.OK {
			t.Fatalf("audit %s failed: %+v", name, a.First)
		}
	}
	recoveryReport, err := checkpoint.LoadReport(result.RecoverDir)
	if err != nil {
		t.Fatal(err)
	}
	recovered := 0
	for _, c := range recoveryReport.CommitSequence {
		if c.Source == "recovered" {
			recovered++
		}
	}
	if recovered < 2 {
		t.Fatalf("expected at least two recovered chunks, got %d", recovered)
	}
}

func TestSourceMoveIncrementalMatchesFull(t *testing.T) {
	fullLevel, _ := generator.Generate(generator.DefaultSeed)
	move := func(level *acoustic.Level) deps.Edit {
		edit, err := rebuild.MoveSource(level, "SRC-C04", acoustic.Point{X: 12_500, Y: 13_500}, "")
		if err != nil {
			t.Fatal(err)
		}
		return edit
	}
	move(fullLevel)
	fullReport, err := rebuild.NewService(fullLevel, nil, nil).Run(context.Background(), rebuild.RunOptions{
		RoundID: "full-move", Mode: "full", Full: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	incLevel, _ := generator.Generate(generator.DefaultSeed)
	dir := t.TempDir()
	store, err := checkpoint.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	svc := rebuild.NewService(incLevel, nil, store)
	base, err := svc.Run(context.Background(), rebuild.RunOptions{RoundID: "base", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	edit := move(incLevel)
	invalidations := rebuild.DependencyGraph(base).Plan(edit)
	ids := map[string]bool{}
	for _, inv := range invalidations {
		ids[inv.ChunkID] = true
	}
	if !ids["C04"] || !ids["C01"] || !ids["C07"] || ids["C08"] {
		t.Fatalf("unexpected precise invalidation set: %+v", invalidations)
	}
	incremental, err := svc.Run(context.Background(), rebuild.RunOptions{
		RoundID: "move", Mode: "incremental", Invalidations: invalidations,
	})
	if err != nil {
		t.Fatal(err)
	}
	cmp := audit.CompareReports([]*acoustic.Report{fullReport, incremental}, []string{"full", "incremental"})
	if !cmp.OK {
		t.Fatalf("source move mismatch: %s", cmp.First.String())
	}
	auditResult, err := audit.AuditDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !auditResult.OK {
		t.Fatalf("incremental audit failed: %s", auditResult.First.String())
	}
}

func TestParallelSettingsProduceSameResult(t *testing.T) {
	run := func(parallel int) *acoustic.Report {
		level, truth := generator.Generate(generator.DefaultSeed)
		level.Params.Budget.MaxParallel = parallel
		report, err := rebuild.NewService(level, truth, nil).Run(context.Background(), rebuild.RunOptions{
			RoundID: "p", Full: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	serial := run(1)
	parallel := run(4)
	cmp := audit.CompareReports([]*acoustic.Report{serial, parallel}, []string{"serial", "parallel"})
	if !cmp.OK {
		t.Fatalf("parallel mismatch: %+v", cmp.First)
	}
}

func TestConservationViolationListsBandChunkResidual(t *testing.T) {
	level, _ := generator.Generate(generator.DefaultSeed)
	report, err := rebuild.NewService(level, nil, nil).Run(context.Background(), rebuild.RunOptions{RoundID: "r", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	report.Chunks[0].RetainedQ[1] += 2_000_000
	cons, err := rebuild.CheckConservation(level, report.Chunks)
	if err == nil {
		t.Fatal("expected E_CONS")
	}
	ae, ok := acoustic.AsError(err)
	if !ok || ae.Code != acoustic.CodeConservation {
		t.Fatalf("expected E_CONS, got %v", err)
	}
	if len(cons.Violations) == 0 || cons.Violations[0].BandID != 1 || cons.Violations[0].SourceChunkID != "C00" {
		t.Fatalf("expected band 1 violation: %+v", cons.Violations)
	}
}

func TestPathBufferFailureThenLargerBufferSucceeds(t *testing.T) {
	level, _ := generator.Generate(generator.DefaultSeed)
	chunk := level.Chunks[4]
	_, err := engine.ScreenChunk(chunk, level.Sources, level.Listeners, level.Params, 1)
	if ae, ok := acoustic.AsError(err); !ok || ae.Code != acoustic.CodeBudget {
		t.Fatalf("expected E_BUDGET, got %v", err)
	}
	result, err := engine.ScreenChunk(chunk, level.Sources, level.Listeners, level.Params, 64)
	if err != nil {
		t.Fatalf("retry with larger buffer: %v", err)
	}
	if len(result.Paths) != 2 {
		t.Fatalf("paths = %d", len(result.Paths))
	}
}
