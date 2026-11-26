package pipeline

import (
	"context"
	"testing"

	"iconflow/checkpoint"
	"iconflow/generator"
)

func TestThreePathsConsistent(t *testing.T) {
	project := generator.GenerateProject(generator.FixedSeed)
	report, err := RunConsistency(context.Background(), project, Options{
		Budget: DefaultBudget(),
	})
	if err != nil {
		t.Fatalf("consistency failed: %v", err)
	}
	if !report.Passed {
		t.Fatalf("report not passed: %+v", report.Diff)
	}
	if report.RealtimeBlocks != len(project.SortedBlockCoords()) {
		t.Fatalf("unexpected block count %d", report.RealtimeBlocks)
	}
}

func TestFlakyBlockRetry(t *testing.T) {
	ctx := context.Background()
	project := generator.GenerateProject(generator.FixedSeed)
	coords := project.SortedBlockCoords()
	target := coords[4].Key()
	runner, err := NewRunner(ctx, project, Options{
		Mode: "test", Budget: DefaultBudget(),
		FailFirstAttempt: map[string]bool{target: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, runErr := runner.Run(ctx)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if runner.blocks[target].Stages["gradient"] != "failed" {
		t.Fatalf("expected target failed, got %s", runner.blocks[target].Stages["gradient"])
	}
	if first.Progress.CommittedWindows == first.Progress.TotalWindows {
		t.Fatal("window should not fully commit with failed block")
	}
	second, runErr := runner.Run(ctx)
	if runErr != nil {
		t.Fatal(runErr)
	}
	if runner.blocks[target].Frozen() != true {
		t.Fatalf("target should freeze after retry: %+v", runner.blocks[target].Stages)
	}
	if second.Progress.CommittedWindows != second.Progress.TotalWindows {
		t.Fatalf("all windows should commit: %+v", second.Progress)
	}
}

func TestScopedEditReusesUnaffectedBlocks(t *testing.T) {
	ctx := context.Background()
	project := generator.GenerateProject(generator.FixedSeed)
	store := checkpoint.NewMemoryStore()
	first, err := NewRunner(ctx, project, Options{Mode: "base", Store: store, Budget: DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	if _, runErr := first.Run(ctx); runErr != nil {
		t.Fatal(runErr)
	}

	target := project.SortedBlockCoords()[5].Key()
	if applyErr := ApplyEdit(project, Edit{
		Field: "threshold_scale", Value: 7000, BlockKey: target,
	}); applyErr != nil {
		t.Fatal(applyErr)
	}
	second, err := NewRunner(ctx, project, Options{Mode: "edited", Store: store, Budget: DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	report, runErr := second.Run(ctx)
	if runErr != nil {
		t.Fatal(runErr)
	}
	reused := 0
	recomputed := []string{}
	for _, result := range report.Results {
		if result.Reused {
			reused++
		} else {
			recomputed = append(recomputed, result.Coord.Key())
		}
	}
	if reused != len(project.SortedBlockCoords())-1 {
		t.Fatalf("expected only target recompute, reused=%d recomputed=%v", reused, recomputed)
	}
	if len(recomputed) != 1 || recomputed[0] != target {
		t.Fatalf("unexpected recomputed blocks: %v", recomputed)
	}
}
