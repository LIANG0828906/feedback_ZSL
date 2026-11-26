package quotakernel_test

import (
	"testing"

	"quotakernel/engine"
	"quotakernel/generate"
	"quotakernel/incremental"
	"quotakernel/model"
)

func TestIncrementalConvergedClosureBoundary(t *testing.T) {
	project := mustMainProject(t)
	manager, err := incremental.NewManager(project)
	if err != nil {
		t.Fatal(err)
	}
	edit := incremental.Edit{Kind: incremental.EditWeightEvent, Target: "evt-08-bravo-weight-up", NewWeight: 300}
	result, err := manager.Apply(edit)
	if err != nil {
		t.Fatal(err)
	}
	full, err := manager.FullRecompute(edit)
	if err != nil {
		t.Fatal(err)
	}
	if err := incremental.EqualReports(result.Reports, full); err != nil {
		t.Fatal(err)
	}
	if !result.Proof.Closure.Converged || result.Proof.Closure.FirstReusedTick != 5 {
		t.Fatalf("expected closure through tick 4 and reuse from 5: %+v", result.Proof.Closure)
	}
}

func mustMainProject(t *testing.T) *model.Project {
	t.Helper()
	bundle, err := generate.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range bundle.Projects {
		if p.Name == "cluster-main" {
			return p.Canonical
		}
	}
	t.Fatal("cluster-main missing")
	return nil
}

func TestFastReserveReconciliationDeltaBothDirections(t *testing.T) {
	bundle, err := generate.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	var project *model.Project
	for _, p := range bundle.Projects {
		if p.Name == "gate-reconcile" {
			project = p.Canonical
		}
	}
	if project == nil {
		t.Fatal("gate-reconcile project missing")
	}
	result, err := engine.New(project).Run()
	if err != nil {
		t.Fatal(err)
	}
	tick1 := result.Reports[0]
	byEvent := map[string]model.ReserveRecord{}
	for _, record := range tick1.Reconciliation.Records {
		byEvent[record.EventID] = record
	}
	const MiB = int64(1024 * 1024)
	low := byEvent["rec-01-low-fast-hold"]
	if low.ReservedBytes != 60*MiB || low.SettledBytes != 40*MiB || low.DeltaBytes != -20*MiB {
		t.Fatalf("unexpected low-priority reconciliation: %+v", low)
	}
	high := byEvent["rec-02-high-priority"]
	if high.ReservedBytes != 0 || high.SettledBytes != 60*MiB || high.DeltaBytes != 60*MiB {
		t.Fatalf("unexpected high-priority catch-up reconciliation: %+v", high)
	}
	if !tick1.Reconciliation.Closed {
		t.Fatalf("reconciliation not closed: %+v", tick1.Reconciliation)
	}
}

func TestExistingBurstIsAttributedAsGuaranteeEncroacher(t *testing.T) {
	project := encroachmentProject()
	eng := engine.New(project)
	eng.BeforeTick = func(state *engine.State, tick model.Tick) error {
		if tick == 2 {
			state.Tenants["vic-a"].GuaranteeBytes = 50
		}
		return nil
	}
	result, err := eng.Run()
	if err != nil {
		t.Fatal(err)
	}
	var item *model.QueueItem
	for i := range result.Reports[1].Queue {
		if result.Reports[1].Queue[i].TenantID == "vic-a" {
			item = &result.Reports[1].Queue[i]
		}
	}
	if item == nil {
		t.Fatalf("victim guarantee request was not queued: %+v", result.Reports[1])
	}
	if item.EncroachingTenantID != "bur-b" || item.VictimTenantID != "vic-a" {
		t.Fatalf("encroachment attribution wrong: %+v", item)
	}
	if item.ConstraintID != model.ConstraintGuaranteeReserve {
		t.Fatalf("constraint=%s, want %s", item.ConstraintID, model.ConstraintGuaranteeReserve)
	}
}

func encroachmentProject() *model.Project {
	return &model.Project{
		ProjectID: "encroachment", TickDurationMs: 100, Rules: model.DefaultRules(),
		Nodes: []model.Node{{
			ID: "enc-node", RackID: "r", CapacityBytes: 100,
			AllowedTenants: []string{"vic-a", "bur-b"},
		}},
		Tenants: []model.Tenant{
			{ID: "vic-a", RackID: "r", HomeNodeID: "enc-node", Priority: 100, Weight: 1, QuotaBytes: 100, GuaranteeBytes: 0, MaxWaitTicks: 3},
			{ID: "bur-b", RackID: "r", HomeNodeID: "enc-node", Priority: 1, Weight: 1, QuotaBytes: 0, GuaranteeBytes: 0, BurstEnabled: true, BurstCapBytes: 100, MaxWaitTicks: 3},
		},
		Events: []model.Event{
			{ID: "enc-01-burst", Tick: 1, Kind: model.EventBurst, TenantID: "bur-b", NodeID: "enc-node", AmountBytes: 100},
			{ID: "enc-02-guarantee", Tick: 2, Kind: model.EventApply, TenantID: "vic-a", NodeID: "enc-node", AmountBytes: 50},
		},
	}
}
