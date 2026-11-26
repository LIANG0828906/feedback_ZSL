package bonerig

import (
	"context"
	"testing"
)

func TestPreciseInvalidationPlan(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	a := sample.Asset
	activity := BuildActivityIndex(a)

	idx := a.Skeleton.Index()
	oldC := *a.Skeleton.Joints[idx["hand_l"]].Constraint
	newC := oldC
	newC.MaxAngleMicroRad = 2_500_000
	constraintPlan := PlanConstraintEdit(a, activity, "hand_l", oldC, newC)
	if len(constraintPlan) != 1 || constraintPlan[0].IntervalID != "frames-0008-0012" {
		t.Fatalf("constraint plan = %+v", constraintPlan)
	}
	if constraintPlan[0].HitBasis == "" || constraintPlan[0].Downstream == "" {
		t.Fatalf("hit basis/downstream required: %+v", constraintPlan[0])
	}

	coefPlan := PlanCoefficientEdit(a, activity, mustMap(a, "HandL"), "coefficient")
	if len(coefPlan) != 1 || coefPlan[0].IntervalID != "frames-0008-0012" {
		t.Fatalf("coefficient plan = %+v", coefPlan)
	}

	global := PlanGlobalEdit(a, "precision_edited", "precision_version")
	if len(global) != 3 {
		t.Fatalf("global plan length = %d", len(global))
	}
}

func TestEngineReusesUnaffectedIntervals(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	a := sample.Asset
	store, err := OpenCheckpointStore(t.TempDir() + "/checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Asset: a, Budget: Budget{MaxWorkers: 2, MaxJointBufferPerWorker: 4, MaxPendingFrames: 1}, Store: store}
	ctx := context.Background()
	if _, err := engine.Run(ctx); err != nil {
		t.Fatal(err)
	}
	idx := a.Skeleton.Index()
	c := *a.Skeleton.Joints[idx["hand_l"]].Constraint
	c.MaxAngleMicroRad = 2_500_000
	a.Skeleton.Joints[idx["hand_l"]].Constraint = &c
	engine.Asset = a
	report, rerr := engine.Run(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	status := map[string]IntervalRun{}
	for _, run := range report.Intervals {
		status[run.IntervalID] = run
	}
	if !status["frames-0000-0004"].Reused || !status["frames-0004-0008"].Reused {
		t.Fatalf("unaffected intervals not reused: %+v", report.Intervals)
	}
	if status["frames-0008-0012"].Reused {
		t.Fatal("affected interval was incorrectly reused")
	}
}

func TestIntervalCacheKeyChangesWithParameter(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	a := sample.Asset
	_, key1 := BuildIntervalDependencies(a, "frames-0000-0004", []string{"root", "spine"})
	a.Precision.Version = "prec-changed"
	_, key2 := BuildIntervalDependencies(a, "frames-0000-0004", []string{"root", "spine"})
	if key1 == key2 {
		t.Fatal("precision version did not change cache key")
	}
}

func TestPrecisionEditRecomputesAllIntervals(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	a := sample.Asset
	store, err := OpenCheckpointStore(t.TempDir() + "/checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	engine := &Engine{Asset: a, Budget: DefaultBudget(), Store: store}
	if _, err := engine.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Precision.Version = "prec-tightened-test"
	engine.Asset = a
	report, err := engine.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range report.Intervals {
		if run.Reused {
			t.Fatalf("precision edit must recompute %s", run.IntervalID)
		}
	}
}

func TestTopologyEditChangesCacheKey(t *testing.T) {
	sample, _ := NewDeterministicSample(DefaultSeed)
	a := sample.Asset
	active := []string{"root", "spine", "arm_l", "hand_l"}
	_, before := BuildIntervalDependencies(a, "frames-0008-0012", active)
	idx := a.Skeleton.Index()
	a.Skeleton.Joints[idx["hand_l"]].ParentID = "spine"
	_, after := BuildIntervalDependencies(a, "frames-0008-0012", active)
	if before == after {
		t.Fatal("topology edit did not change interval cache key")
	}
	if TopologyHash(a.Skeleton) == sample.Asset.TopologyVersion {
		t.Fatal("topology hash did not change")
	}
}
