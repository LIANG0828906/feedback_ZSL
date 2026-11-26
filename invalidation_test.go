package strokelab

import (
	"context"
	"testing"
)

func TestEditInvalidatesOnlyAffectedBlocksAndReusesCache(t *testing.T) {
	suite, err := GenerateSuite(99173)
	if err != nil {
		t.Fatal(err)
	}
	project := suite.Samples[2].Project
	engine, _ := NewEngine(project)
	base, err := engine.Run(context.Background(), RunOptions{Mode: RunFull})
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	if base.Cache == nil {
		t.Fatal("full run did not emit a block cache")
	}
	newWeight := project.Strokes[0].Weight - 700
	edited, err := ApplyEdit(project, Edit{StrokeID: project.Strokes[0].ID, Weight: &newWeight})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanInvalidation(project, edited, Edit{StrokeID: project.Strokes[0].ID, Weight: &newWeight})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Affected) == 0 || len(plan.Downstream) == 0 {
		t.Fatalf("expected invalidation and downstream nodes: %+v", plan)
	}
	if plan.Dependencies == nil || len(plan.Dependencies.Nodes) == 0 || len(plan.Dependencies.Edges) == 0 {
		t.Fatalf("dependency graph not materialized")
	}
	editedEngine, _ := NewEngine(edited)
	reusedEval, runErr := editedEngine.Run(context.Background(), RunOptions{
		Mode: RunFull, ReuseCache: base.Cache,
	})
	if runErr != nil && ErrorCode(runErr) != CodeTopology {
		t.Fatalf("edited reuse run: %v", runErr)
	}
	recomputed := make(map[int]bool)
	for _, id := range reusedEval.RecomputedBlocks {
		recomputed[id] = true
	}
	for _, id := range plan.Affected {
		if !recomputed[id] {
			t.Fatalf("affected block %d was not recomputed", id)
		}
	}
	for _, id := range reusedEval.ReusedBlocks {
		if plan.Contains(id) {
			t.Fatalf("affected block %d was reused", id)
		}
	}
	cleanEngine, _ := NewEngine(edited)
	clean, cleanErr := cleanEngine.Run(context.Background(), RunOptions{Mode: RunFull})
	if cleanErr != nil && ErrorCode(cleanErr) != CodeTopology {
		t.Fatalf("clean edited run: %v", cleanErr)
	}
	cmp := CompareEvaluations(reusedEval, clean)
	if !cmp.Equal {
		t.Fatalf("cache reuse changed output: %+v", cmp.Mismatches)
	}
}
