package kernel

import (
	"encoding/json"
	"syscall"
	"testing"
)

func TestLongHorizonAdmissionRemainsStable(t *testing.T) {
	p := &Project{
		ProjectID: "long-horizon", TickMS: 10, Policy: DefaultFixedPolicy(),
		Nodes:   []Node{{ID: "n1", CapacityBytes: 1 << 20, AllowedTenants: []ID{"t1"}}},
		Tenants: []Tenant{{ID: "t1", Priority: 1, WeightPermyriad: 5000, QuotaBytes: 1 << 20, GuaranteeBytes: 4096}},
	}
	p.Events = append(p.Events, Event{ID: "apply-1", Tick: 1, Type: EventTypeApply, TenantID: "t1", NodeID: "n1", AmountBytes: 4096, BudgetMS: 100})
	for tick := int64(2); tick <= 300; tick++ {
		p.Events = append(p.Events,
			Event{ID: "release-" + itoa(tick), Tick: tick, Type: EventTypeRelease, TenantID: "t1", NodeID: "n1", AmountBytes: 4096, BudgetMS: 100},
			Event{ID: "apply-" + itoa(tick), Tick: tick, Type: EventTypeApply, TenantID: "t1", NodeID: "n1", AmountBytes: 4096, BudgetMS: 100},
		)
	}
	eq, err := VerifyPathEquivalence(p, Budgets{Workers: 4, PerTenantQueue: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !eq.AllEqual {
		t.Fatalf("long horizon path mismatch: %+v", eq)
	}
	r, err := RunReference(p, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	last := r.Ticks[len(r.Ticks)-1]
	for _, l := range last.Ledgers {
		if l.UsedBytes != 4096 {
			t.Fatalf("integer ledger drifted after long horizon: used=%d", l.UsedBytes)
		}
	}
}

func TestGateSinglePreDeductionNeverCrossesHardCap(t *testing.T) {
	gate := NewGate(PathPreDeduct, DefaultBudgets())
	d := gate.DecideSingle(GateRequest{
		TenantID: "t1", NodeID: "n1", WantBytes: 300, UsedBytes: 0, HeldBytes: 0,
		HardCapBytes: 1000, NodeAvailable: 50, BudgetMS: 1,
	})
	if d.PreHoldBytes != 50 || d.GrantBytes != 50 || d.Decision != DecisionQueue {
		t.Fatalf("unexpected gate decision: %+v", d)
	}
	if !d.WithinBudget || d.DelayMS != 1 {
		t.Fatalf("latency budget not respected: %+v", d)
	}
}

func TestVersionMismatchRecoveryRecomputesFromStart(t *testing.T) {
	bundle, err := GenerateBundle(42)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := dir + "/mismatch.sqlite"
	cp := BuildCheckpoint(bundle.Project, DefaultBudgets(), bundle.Truth.Ticks[3])
	cp.FormatVersion = "old"
	payload, _ := json.Marshal(cp)
	native, err := BuildNativeSQLite(checkpointTables(cp, string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(path, native); err != nil {
		t.Fatal(err)
	}
	full, _, recovery, err := RecoverOrRun(bundle.Project, DefaultBudgets(), path)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Action != "full_recompute" || recovery.ReasonCode != CodeCheckpointVersion {
		t.Fatalf("unexpected recovery: %+v", recovery)
	}
	if len(full.Ticks) != len(bundle.Truth.Ticks) {
		t.Fatalf("full recompute ticks=%d want=%d", len(full.Ticks), len(bundle.Truth.Ticks))
	}
}

func TestSignalClassification(t *testing.T) {
	if ClassifyTermination(syscall.SIGTERM) != SignalGraceful {
		t.Fatal("SIGTERM should be graceful")
	}
	if ClassifyTermination(syscall.SIGKILL) != SignalForced {
		t.Fatal("SIGKILL should be forced")
	}
}
