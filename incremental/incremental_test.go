package incremental_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/liang/collector-backpressure/bpderr"
	"github.com/liang/collector-backpressure/engine"
	"github.com/liang/collector-backpressure/incremental"
	"github.com/liang/collector-backpressure/model"
	"github.com/liang/collector-backpressure/sample"
	"github.com/liang/collector-backpressure/testsupport"
)

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	ctx := context.Background()
	budget := engine.Budget{Workers: 3, PerAgentQueue: 64, SchedulerQueue: 64}
	original := sample.GenerateProject(sample.DefaultSeed)
	sourceBefore, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := incremental.NewCache(ctx, original, budget)
	if err != nil {
		t.Fatal(err)
	}

	edits := []incremental.EditSpec{
		incremental.EditAgentBuffer("a3", 180),
		incremental.EditPartitionRate("p3", 90),
		incremental.EditPartitionBacklogLimit("p2", 200),
		incremental.EditKeyRule(model.KeyRule{
			ID: "r-globex-all", Tenant: "globex", Key: "*", PartitionID: "p3", Priority: 1,
		}),
		incremental.EditConsumerWatermark("p3", nil, false),
	}

	for _, edit := range edits {
		t.Run(edit.Source(), func(t *testing.T) {
			inc, report, err := cache.Apply(ctx, edit)
			if err != nil {
				t.Fatal(err)
			}
			edited, err := incremental.CloneProject(original)
			if err != nil {
				t.Fatal(err)
			}
			if err := edit.Apply(edited); err != nil {
				t.Fatal(err)
			}
			full, err := incremental.FullRecompute(ctx, edited, budget)
			if err != nil {
				t.Fatal(err)
			}
			if !testsupport.EqualSemanticTicks(inc.Ticks, full.Result.Ticks) {
				t.Fatalf("incremental result differs from full recompute")
			}
			if len(report.PropagationClosure) < 2 {
				t.Fatalf("expected non-trivial propagation closure: %v", report.PropagationClosure)
			}
			for _, tick := range inc.Ticks {
				if tick.Invalidation == nil || len(tick.Invalidation.Sources) == 0 {
					t.Fatalf("tick %d missing invalidation source", tick.Tick)
				}
			}
			if report.FromTick == 1 {
				if len(report.ReusedTicks) != 1 || report.ReusedTicks[0] != 0 {
					t.Fatalf("expected only tick0 reused, got %+v", report.ReusedTicks)
				}
				if inc.Ticks[0].Invalidation.Recomputed {
					t.Fatal("tick0 should be marked reused")
				}
			}
		})
	}

	p3Edit := incremental.EditPartitionRate("p3", 75)
	_, p3Report, err := cache.Apply(ctx, p3Edit)
	if err != nil {
		t.Fatal(err)
	}
	if p3Report.FromTick != 1 {
		t.Fatalf("expected p3 dependency to start at tick1, got %d", p3Report.FromTick)
	}
	assertContains(t, p3Report.PropagationClosure, "partition:p3")
	assertContains(t, p3Report.PropagationClosure, "agent:a3")
	assertContains(t, p3Report.PropagationClosure, "agent:a1")
	assertContains(t, p3Report.PropagationClosure, "partition:p1")

	sourceAfter, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if string(sourceBefore) != string(sourceAfter) {
		t.Fatal("source project was modified by incremental edit/recompute")
	}
}

func TestIncrementalInvalidEditsDoNotMutateCache(t *testing.T) {
	ctx := context.Background()
	budget := engine.DefaultBudget()
	cache, err := incremental.NewCache(ctx, sample.GenerateProject(sample.DefaultSeed), budget)
	if err != nil {
		t.Fatal(err)
	}

	zero := incremental.EditAgentBuffer("a1", 0)
	if _, _, err := cache.Apply(ctx, zero); !hasCode(err, bpderr.EBufferLimitZero) {
		t.Fatalf("expected zero buffer code, got %v", err)
	}

	cycle := incremental.EditKeyRule(model.KeyRule{
		ID: "r-acme-all", Tenant: "acme", Key: "*", PartitionID: "p3",
		ParentID: "r-acme-click", Priority: 0,
	})
	if _, _, err := cache.Apply(ctx, cycle); !hasCode(err, bpderr.EKeyRuleCycle) {
		t.Fatalf("expected cycle code, got %v", err)
	}

	if cache.Full.Result.Ticks[0].Agents[0].BufferMaxBytes != 260 {
		t.Fatal("cache base mutated by invalid edit")
	}
}

func hasCode(err error, code bpderr.Code) bool {
	biz, ok := bpderr.As(err)
	return ok && biz.Code == code
}

func assertContains(t *testing.T, values []string, want string) {
	t.Helper()
	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("closure %v does not contain %s", values, want)
}
