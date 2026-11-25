package engine_test

import (
	"reflect"
	"testing"

	"credrot/internal/engine"
	"credrot/internal/hashutil"
	"credrot/internal/model"
)

func TestIncrementalCredentialEditMatchesFullRecompute(t *testing.T) {
	_, p := loadBundle(t, 42)
	snapshot, err := engine.BuildSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	before, err := hashutil.JSONSHA256(snapshot.Project)
	if err != nil {
		t.Fatal(err)
	}
	edited := p.Clone()
	edited.Credentials[0].TTLMillis += 2000
	edited.Sort()
	got, err := engine.Incremental(snapshot, edited)
	if err != nil {
		t.Fatal(err)
	}
	solver, err := engine.NewSolver(edited)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		t.Fatal(err)
	}
	propagation, err := engine.Propagate(edited)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(edited, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Snapshot.Validity, validity) {
		t.Fatal("incremental validity differs from full recomputation")
	}
	if !reflect.DeepEqual(got.Snapshot.Propagation, propagation) {
		t.Fatal("incremental propagation differs from full recomputation")
	}
	if !reflect.DeepEqual(got.Snapshot.Schedule, schedule) {
		t.Fatal("incremental schedule differs from full recomputation")
	}
	if got.ReusedValidity == 0 || len(got.Invalidated) == 0 {
		t.Fatalf("edit did not exercise partial reuse: reused=%d invalidated=%d", got.ReusedValidity, len(got.Invalidated))
	}
	if got.ScheduleRecomputed {
		t.Fatal("credential TTL edit should reuse a schedule that only depends on propagation")
	}
	after, err := hashutil.JSONSHA256(snapshot.Project)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("incremental edit mutated cached source project")
	}
}

func TestIncrementalReloadEditRecomputesScheduleOnly(t *testing.T) {
	_, p := loadBundle(t, 42)
	snapshot, err := engine.BuildSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	edited := p.Clone()
	edited.Reload.MaxPerBatch = 2
	edited.Sort()
	got, err := engine.Incremental(snapshot, edited)
	if err != nil {
		t.Fatal(err)
	}
	fullCells := len(snapshot.Validity)
	if got.ReusedValidity != fullCells || got.RecomputedValidity != 0 {
		t.Fatalf("reload edit should reuse every validity cell: reused=%d total=%d recomputed=%d", got.ReusedValidity, fullCells, got.RecomputedValidity)
	}
	if !got.ScheduleRecomputed || got.ScheduleSource != "reload:config" {
		t.Fatalf("reload edit did not recompute schedule: %+v", got)
	}
	solver, err := engine.NewSolver(edited)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		t.Fatal(err)
	}
	propagation, err := engine.Propagate(edited)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(edited, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Snapshot.Validity, validity) ||
		!reflect.DeepEqual(got.Snapshot.Propagation, propagation) ||
		!reflect.DeepEqual(got.Snapshot.Schedule, schedule) {
		t.Fatal("reload incremental result differs from full recomputation")
	}
}

func TestIncrementalReferenceAndTopologyEdits(t *testing.T) {
	_, p := loadBundle(t, 42)
	snapshot, err := engine.BuildSnapshot(p)
	if err != nil {
		t.Fatal(err)
	}
	sub := p.SubscriptionByID()["sub-api"]
	nextRefs := make([]model.CredentialRef, 0, len(sub.Refs))
	for _, ref := range sub.Refs {
		if ref.ExpectedVersion == 1 {
			nextRefs = append(nextRefs, ref)
		}
	}
	sub.Refs = nextRefs
	edited, err := engine.ApplyEdit(p, engine.ProjectEdit{Kind: engine.EditSubscription, Subscription: &sub})
	if err != nil {
		t.Fatal(err)
	}
	got, err := engine.Incremental(snapshot, edited)
	if err != nil {
		t.Fatal(err)
	}
	solver, err := engine.NewSolver(edited)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		t.Fatal(err)
	}
	propagation, err := engine.Propagate(edited)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(edited, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Snapshot.Validity, validity) ||
		!reflect.DeepEqual(got.Snapshot.Propagation, propagation) ||
		!reflect.DeepEqual(got.Snapshot.Schedule, schedule) {
		t.Fatal("reference/topology incremental result differs from full recomputation")
	}
	if got.ScheduleSource != "reload:propagation" {
		t.Fatalf("propagation-affecting edit used unexpected schedule source: %s", got.ScheduleSource)
	}
}
