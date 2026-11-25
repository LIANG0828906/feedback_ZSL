package engine_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"credrot/internal/anomaly"
	"credrot/internal/bizerr"
	"credrot/internal/engine"
	"credrot/internal/generator"
	"credrot/internal/model"
	"credrot/internal/project"
)

func TestValidityMatchesOracle(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 20261126} {
		t.Run(jsonNumber(seed), func(t *testing.T) {
			bundle, p := loadBundle(t, seed)
			solver, err := engine.NewSolver(p)
			if err != nil {
				t.Fatal(err)
			}
			got, err := solver.ValidityAll()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, bundle.Oracle.Validity) {
				t.Fatal("validity differs from independent oracle")
			}
			for _, value := range got {
				if len(value.Available) > 0 && value.Available[0].RemainingMillis <= 0 {
					t.Fatalf("available credential has non-positive remaining lifetime at tick %d", value.Tick)
				}
			}
		})
	}
}

func TestPropagationMatchesOracleAndTopologyChangesRecompute(t *testing.T) {
	bundle, p := loadBundle(t, 42)
	got, err := engine.Propagate(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, bundle.Oracle.Propagation) {
		t.Fatal("propagation differs from oracle")
	}

	withoutSearch, err := engine.ApplyEdit(p, engine.ProjectEdit{
		Kind:           engine.EditRemoveSubscription,
		SubscriptionID: "sub-search",
	})
	if err != nil {
		t.Fatal(err)
	}
	changed, err := engine.Propagate(withoutSearch)
	if err != nil {
		t.Fatal(err)
	}
	for _, suspended := range changed.Suspended {
		for _, arrival := range suspended.Arrivals {
			if arrival.SubscriptionID == "sub-search" || arrival.SubscriptionID == "sub-archive" {
				t.Fatalf("removed topology still reaches %s via %s", arrival.SubscriptionID, suspended.CredentialID)
			}
		}
	}

	addedSub := model.Subscription{
		ID:        "sub-new",
		Active:    true,
		DependsOn: []string{"sub-api"},
		Refs: []model.CredentialRef{{
			CredentialID:    "cred-sub-audit-v1",
			ExpectedVersion: 1,
			Role:            "primary",
		}},
	}
	withNew, err := engine.ApplyEdit(p, engine.ProjectEdit{Kind: engine.EditAddSubscription, Subscription: &addedSub})
	if err != nil {
		t.Fatal(err)
	}
	changed, err = engine.Propagate(withNew)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, suspended := range changed.Suspended {
		for _, arrival := range suspended.Arrivals {
			if arrival.SubscriptionID == "sub-new" {
				found = true
				if arrival.Depth == 0 {
					t.Fatalf("new descendant should have positive depth: %+v", arrival)
				}
			}
		}
	}
	if !found {
		t.Fatal("new topology descendant was not included in propagation")
	}
}

func TestGateFallbackRejectAndAllow(t *testing.T) {
	bundle, p := loadBundle(t, 42)
	gates, err := engine.GateAll(p, bundle.Oracle.Gates.AtTick, bundle.Oracle.GateRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gates, bundle.Oracle.Gates) {
		t.Fatal("gates differ from oracle")
	}
	seen := map[string]bool{}
	for _, decision := range gates.Decisions {
		seen[decision.Decision] = true
		if decision.Source == "" {
			t.Fatalf("decision has no state source: %+v", decision)
		}
	}
	for _, want := range []string{"allow_new", "fallback_old", "reject"} {
		if !seen[want] {
			t.Fatalf("generator did not exercise %s (seen=%v)", want, seen)
		}
	}
}

func TestGateCombinedAnomalyPrecedence(t *testing.T) {
	requested := &model.Credential{ID: "requested-v1", Version: 1}
	committed := &model.Credential{ID: "committed-v2", Version: 2}
	base := engine.GateConditions{
		SubscriptionID:     "sub-api",
		AtTick:             6,
		Requested:          requested,
		Committed:          committed,
		RequestedBatch:     9,
		LastCommittedBatch: 2,
		CheckBatchGap:      true,
		WatermarkKnown:     false,
		RequireWatermark:   true,
		CommittedValid:     true,
	}
	if got := engine.EvaluateGate(base); got.Code != bizerr.CodeVersionRegression {
		t.Fatalf("version regression must win precedence, got %+v", got)
	}
	base.Requested.Version = 3
	if got := engine.EvaluateGate(base); got.Code != bizerr.CodeBatchGap {
		t.Fatalf("batch gap should win after version regression, got %+v", got)
	}
	base.CheckBatchGap = false
	if got := engine.EvaluateGate(base); got.Code != bizerr.CodeWatermarkMissing {
		t.Fatalf("missing watermark should be reported, got %+v", got)
	}
	base.RequireWatermark = false
	base.WatermarkKnown = true
	base.Arrived = false
	if got := engine.EvaluateGate(base); got.Decision != "fallback_old" {
		t.Fatalf("unarrived reload should fall back to valid committed credential, got %+v", got)
	}
	base.CommittedValid = false
	if got := engine.EvaluateGate(base); got.Decision != "reject" || got.Code != bizerr.CodeGateNoFallback {
		t.Fatalf("expired fallback must reject, got %+v", got)
	}
}

func TestScheduleCapRetryAndCoverage(t *testing.T) {
	bundle, p := loadBundle(t, 42)
	propagation, err := engine.Propagate(p)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(p, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(schedule, bundle.Oracle.Schedule) {
		t.Fatal("schedule differs from oracle")
	}
	if !schedule.CoverageMatch {
		t.Fatal("actual + failed does not cover the one-shot full reload set")
	}
	if !reflect.DeepEqual(schedule.FullSet, schedule.OneShotSet) {
		t.Fatal("one-shot set differs from full set")
	}
	deferred := 0
	failed := 0
	for _, batch := range schedule.Batches {
		if got := len(batch.Planned); got > p.Reload.MaxPerBatch {
			t.Fatalf("batch %d exceeds cap: %d > %d", batch.Batch, got, p.Reload.MaxPerBatch)
		}
		deferred += len(batch.Deferred)
		failed += len(batch.Failed)
		for _, failure := range batch.Failed {
			if failure.TriggerSource == "" || failure.FailureMillis < 0 {
				t.Fatalf("failure lacks observable trigger/time: %+v", failure)
			}
		}
	}
	if deferred == 0 {
		t.Fatal("generator did not exercise batch-cap spillover")
	}
	if failed == 0 {
		t.Fatal("generator did not exercise terminal retry failure")
	}
}

func TestScheduleCapGreaterThanOneSpillsDeterministically(t *testing.T) {
	p := &model.Project{
		ProjectID:                   "cap-test",
		ManifestVersion:             "manifest-cap-test",
		TickMillis:                  1000,
		HorizonTicks:                8,
		LastRotationWatermarkMillis: 1000,
		Reload: model.ReloadConfig{
			ScheduleStartTick: 0,
			MaxPerBatch:       2,
			RetryLimit:        1,
			RetryStepMillis:   1000,
		},
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("sub-%d", i)
		v1 := model.Credential{ID: "cred-" + id + "-v1", Version: 1, IssuedAtMillis: 1000, TTLMillis: 20000}
		v2 := model.Credential{ID: "cred-" + id + "-v2", Version: 2, IssuedAtMillis: 2000, TTLMillis: 20000, ReloadBatch: 1}
		p.Credentials = append(p.Credentials, v1, v2)
		p.Subscriptions = append(p.Subscriptions, model.Subscription{
			ID:     id,
			Active: true,
			Refs: []model.CredentialRef{
				{CredentialID: v1.ID, ExpectedVersion: 1},
				{CredentialID: v2.ID, ExpectedVersion: 2},
			},
		})
		p.Rotations = append(p.Rotations, model.Rotation{CredentialID: v2.ID, FromVersion: 1, ToVersion: 2})
	}
	p.Sort()
	propagation, err := engine.Propagate(p)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(p, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if !schedule.CoverageMatch || len(schedule.ActualUnion) != 5 {
		t.Fatalf("cap spill lost reloads: %+v", schedule)
	}
	if len(schedule.Batches) != 3 {
		t.Fatalf("expected three cap slices, got %d", len(schedule.Batches))
	}
	for _, batch := range schedule.Batches {
		if len(batch.Planned) > 2 {
			t.Fatalf("batch %d exceeds cap 2: %v", batch.Batch, batch.Planned)
		}
	}
	deferred := 0
	for _, batch := range schedule.Batches {
		deferred += len(batch.Deferred)
	}
	if len(schedule.Batches[1].Deferred) != 2 || len(schedule.Batches[2].Deferred) != 2 || deferred != 4 {
		t.Fatalf("unexpected deferred evidence: %+v", schedule.Batches)
	}
}

func TestParallelInvarianceAndBackpressure(t *testing.T) {
	_, p := loadBundle(t, 42)
	solver, err := engine.NewSolver(p)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		t.Fatal(err)
	}
	propagation, err := engine.Propagate(p)
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	events := 0
	for _, workers := range []int{1, 3, 8} {
		for _, buffer := range []int{1, 2, 10} {
			result, err := engine.SolveParallel(context.Background(), p, engine.ParallelOptions{
				Workers:                      workers,
				PendingBufferPerSubscription: buffer,
			})
			if err != nil {
				t.Fatal(err)
			}
			events += len(result.Backpressure)
			if digest == "" {
				digest = result.Digest
			} else if result.Digest != digest {
				t.Fatalf("digest changed at workers=%d buffer=%d", workers, buffer)
			}
			if !reflect.DeepEqual(result.Validity, validity) {
				t.Fatalf("parallel validity changed at workers=%d buffer=%d", workers, buffer)
			}
			if !reflect.DeepEqual(result.Propagation, propagation) {
				t.Fatalf("parallel propagation changed at workers=%d buffer=%d", workers, buffer)
			}
		}
	}
	if events == 0 {
		t.Fatal("no backpressure events were emitted")
	}
	if _, err := engine.SolveParallel(context.Background(), p, engine.ParallelOptions{Workers: 0, PendingBufferPerSubscription: 1}); !bizerr.IsCode(err, bizerr.CodeWorkerBudget) {
		t.Fatalf("zero worker budget returned %v", err)
	}
	if _, err := engine.SolveParallel(context.Background(), p, engine.ParallelOptions{Workers: 1, PendingBufferPerSubscription: 0}); !bizerr.IsCode(err, bizerr.CodeBufferBudget) {
		t.Fatalf("zero buffer budget returned %v", err)
	}
}

func TestSubscriptionOrderInvariance(t *testing.T) {
	bundle, p := loadBundle(t, 42)
	reordered := p.Clone()
	reverseSubscriptions(reordered.Subscriptions)
	reverseCredentials(reordered.Credentials)
	for i := range reordered.Subscriptions {
		refs := reordered.Subscriptions[i].Refs
		for left, right := 0, len(refs)-1; left < right; left, right = left+1, right-1 {
			refs[left], refs[right] = refs[right], refs[left]
		}
		deps := reordered.Subscriptions[i].DependsOn
		for left, right := 0, len(deps)-1; left < right; left, right = left+1, right-1 {
			deps[left], deps[right] = deps[right], deps[left]
		}
	}
	solver, err := engine.NewSolver(reordered)
	if err != nil {
		t.Fatal(err)
	}
	validity, err := solver.ValidityAll()
	if err != nil {
		t.Fatal(err)
	}
	propagation, err := engine.Propagate(reordered)
	if err != nil {
		t.Fatal(err)
	}
	schedule, err := engine.Schedule(reordered, propagation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(validity, bundle.Oracle.Validity) ||
		!reflect.DeepEqual(propagation, bundle.Oracle.Propagation) ||
		!reflect.DeepEqual(schedule, bundle.Oracle.Schedule) {
		t.Fatal("input slice ordering changed semantic results")
	}
}

func TestCycleAndMissingReferenceCodes(t *testing.T) {
	_, p := loadBundle(t, 42)
	cycled, err := anomaly.Inject(p, anomaly.SubscriptionCycle)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Validate(cycled); !bizerr.IsCode(err, bizerr.CodeSubCycle) {
		t.Fatalf("cycle returned %v", err)
	}
	missing, err := anomaly.Inject(p, anomaly.MissingCredential)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Validate(missing); !bizerr.IsCode(err, bizerr.CodeMissingCredential) {
		t.Fatalf("missing reference returned %v", err)
	}
}

func reverseSubscriptions(values []model.Subscription) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseCredentials(values []model.Credential) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func loadBundle(t *testing.T, seed int64) (*generator.Bundle, *model.Project) {
	t.Helper()
	bundle, err := generator.Generate(seed)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := project.Load(bundle.Files["project_v1.json"])
	if err != nil {
		t.Fatal(err)
	}
	return bundle, loaded.Project
}

func jsonNumber(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	negative := v < 0
	if negative {
		v = -v
	}
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
