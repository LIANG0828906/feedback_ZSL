package maskqa

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSelectiveInvalidationAndReuse(t *testing.T) {
	d := GenerateDataset()
	svc, err := NewService(d, filepath.Join(t.TempDir(), "inc.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := svc.Run(ctx, RunOptions{Mode: "full"}); err != nil {
		t.Fatal(err)
	}

	edited := svc.Params
	edited.MaxIterations = 6
	items, err := svc.ApplyEdit(ctx, edited)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 60 {
		t.Fatalf("iteration edit invalidations=%d, want 60 open+compose nodes", len(items))
	}
	for _, it := range items {
		if it.Stage != StageOpen && it.Stage != StageCompose {
			t.Fatalf("unexpected invalidated stage %s", it.Stage)
		}
		if it.Reason == "" || len(it.Upstream) == 0 {
			t.Fatalf("invalidation missing reason/upstream: %+v", it)
		}
	}
	rep, err := svc.Run(ctx, RunOptions{Mode: "incremental"})
	if err != nil {
		t.Fatal(err)
	}
	reusedStages := map[Stage]int{}
	for _, sr := range rep.SampleReports {
		for _, ref := range sr.Reused {
			reusedStages[ref.Stage]++
		}
	}
	for _, st := range []Stage{StageGradient, StageEdge, StageRadius, StageClose} {
		if reusedStages[st] != 30 {
			t.Fatalf("stage %s reused=%d want 30", st, reusedStages[st])
		}
	}
	if reusedStages[StageOpen] != 0 || reusedStages[StageCompose] != 0 {
		t.Fatalf("edited downstream stages were reused: %+v", reusedStages)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRadiusAndOperatorInvalidationScopes(t *testing.T) {
	d := GenerateDataset()
	ctx := context.Background()

	svc, err := NewService(d, filepath.Join(t.TempDir(), "radius.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Run(ctx, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	p := svc.Params
	p.RadiusMax = 2
	items := svc.PlanEdit(svc.Params, p)
	stages := map[Stage]bool{}
	for _, it := range items {
		stages[it.Stage] = true
	}
	if !stages[StageRadius] || !stages[StageClose] || !stages[StageOpen] || !stages[StageCompose] || stages[StageGradient] || stages[StageEdge] {
		t.Fatalf("unexpected radius invalidation stages: %+v", stages)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	svc2, err := NewService(d, filepath.Join(t.TempDir(), "op.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	p2 := svc2.Params
	p2.Operator = "prewitt3"
	items2 := svc2.PlanEdit(svc2.Params, p2)
	if len(items2) != 180 {
		t.Fatalf("operator edit invalidations=%d want all 180 nodes", len(items2))
	}
	if err := svc2.Close(); err != nil {
		t.Fatal(err)
	}
}
