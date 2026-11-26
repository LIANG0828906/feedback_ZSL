package kernel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestThreeSchemaVersionsMapToSameCanonicalProject(t *testing.T) {
	bundle, err := GenerateBundle(42)
	if err != nil {
		t.Fatal(err)
	}
	projects := make([]*Project, 0, 3)
	for _, raw := range []map[string]any{bundle.V1, bundle.V2, bundle.V3} {
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		p, err := LoadJSON(data)
		if err != nil {
			t.Fatalf("schema %v: %v", raw["schema"], err)
		}
		projects = append(projects, p)
	}
	if !reflect.DeepEqual(projects[0], projects[1]) || !reflect.DeepEqual(projects[0], projects[2]) {
		t.Fatalf("canonical projects differ:\nv1=%#v\nv2=%#v\nv3=%#v", projects[0], projects[1], projects[2])
	}
}

func TestLedgerHardCapAndNodeInvariants(t *testing.T) {
	p := GenerateProject(42)
	r, err := RunReference(p, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range r.Ticks {
		for _, l := range tick.Ledgers {
			if l.UsedBytes > l.QuotaBytes+l.BurstLimitBytes {
				t.Fatalf("tick %d tenant %s crosses hard cap", tick.Tick, l.TenantID)
			}
			if l.GuaranteeRemainingBytes < 0 || l.AvailableQuotaBytes < 0 || l.BurstRemainingBytes < 0 {
				t.Fatalf("negative ledger remainder at tick %d %s", tick.Tick, l.TenantID)
			}
		}
		for _, n := range tick.Nodes {
			if n.OccupiedBytes+n.HeldBytes > n.CapacityBytes {
				t.Fatalf("node %s oversold at tick %d", n.NodeID, tick.Tick)
			}
		}
	}
}

func TestFastExactAndParallelPathsMatch(t *testing.T) {
	p := GenerateProject(42)
	eq, err := VerifyPathEquivalence(p, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if !eq.AllEqual {
		t.Fatalf("path equivalence failed: %+v", eq)
	}
}

func TestFairOrderIsStableUnderEnumerationAndParallelism(t *testing.T) {
	p := GenerateProject(42)
	base, err := RunReference(p, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}

	shuffled := p.Clone()
	rng := splitMix64{state: 0xdecafbad}
	shuffle := func(n int, swap func(int, int)) {
		for i := n - 1; i > 0; i-- {
			j := int(rng.next() % uint64(i+1))
			swap(i, j)
		}
	}
	shuffle(len(shuffled.Tenants), func(i, j int) { shuffled.Tenants[i], shuffled.Tenants[j] = shuffled.Tenants[j], shuffled.Tenants[i] })
	shuffle(len(shuffled.Nodes), func(i, j int) { shuffled.Nodes[i], shuffled.Nodes[j] = shuffled.Nodes[j], shuffled.Nodes[i] })
	byTick := map[int64][]int{}
	for i, e := range shuffled.Events {
		byTick[e.Tick] = append(byTick[e.Tick], i)
	}
	for _, indexes := range byTick {
		shuffle(len(indexes), func(a, b int) {
			ia, ib := indexes[a], indexes[b]
			shuffled.Events[ia], shuffled.Events[ib] = shuffled.Events[ib], shuffled.Events[ia]
		})
	}
	shuffledRun, err := RunReference(shuffled, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if !ReportsEqualState(base, shuffledRun) {
		t.Fatal("shuffled enumeration changed admission state")
	}

	for _, workers := range []int{1, 2, 4, 8} {
		budgets := DefaultBudgets()
		budgets.Workers = workers
		parallel, err := RunEngine(p, budgets, PathParallel)
		if err != nil {
			t.Fatal(err)
		}
		high := p.Clone()
		for i := range high.Events {
			high.Events[i].BudgetMS = 100
		}
		want, err := RunEngine(high, budgets, PathExact)
		if err != nil {
			t.Fatal(err)
		}
		got, err := RunEngine(high, budgets, PathParallel)
		if err != nil {
			t.Fatal(err)
		}
		if !ReportsEqualState(want, got) || !ReportsEqualState(base, parallel) {
			t.Fatalf("worker count %d changed result", workers)
		}
	}
}

func TestGuaranteeTranchesAlwaysPrecedeBurstLayer(t *testing.T) {
	r, err := RunReference(GenerateProject(42), DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range r.Ticks {
		maxLayerSeen := -1
		for _, entry := range tick.AdmissionOrder {
			if entry.Layer < maxLayerSeen {
				t.Fatalf("tick %d order crosses layers out of sequence: %+v", tick.Tick, entry)
			}
			maxLayerSeen = entry.Layer
		}
	}
	a, ok := r.Allocation("e-delta-contend")
	if !ok || a.Decision != DecisionQueue || a.InvaderTenantID != "t-delta" {
		t.Fatalf("expected locatable guarantee invasion queue, got ok=%v alloc=%+v", ok, a)
	}
	if a.ConstraintID != ConstraintGuaranteeFloor || len(a.ProtectedTenantIDs) == 0 {
		t.Fatalf("missing constraint/protected tenants: %+v", a)
	}
}

func TestIncrementalEditsMatchFullRecompute(t *testing.T) {
	p := GenerateProject(42)
	checks, err := verifyIncremental(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		if !c.EqualFullRecompute {
			t.Fatalf("incremental edit %s differs", c.Name)
		}
		if len(c.Invalidation.Closure) == 0 || len(c.Invalidation.Provenance) == 0 {
			t.Fatalf("missing closure/provenance for %s", c.Name)
		}
	}
}

func TestSourceProjectIsNotMutatedByIncrementalEdit(t *testing.T) {
	p := GenerateProject(42)
	before, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(p, DefaultBudgets())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.ApplyEdit(Edit{Kind: EditNodeCapacity, NodeID: "node-a", NewValue: mustKiB(9999)}); err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("source project was mutated")
	}

	committedBefore := engine.LastReport()
	_, _, badErr := engine.ApplyEdit(Edit{Kind: EditTenantQuota, TenantID: "t-alpha", NewValue: -1})
	if BizCode(badErr) != CodeNegativeQuota {
		t.Fatalf("expected negative quota injection, got %v", badErr)
	}
	if engine.LastReport() != committedBefore {
		t.Fatal("failed edit replaced the last committed report")
	}
}

func TestCheckpointRoundTripTruncationAndVersionMismatch(t *testing.T) {
	bundle, err := GenerateBundle(42)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "cp.sqlite")
	tick := bundle.Truth.Tick(3)
	cp := BuildCheckpoint(bundle.Project, DefaultBudgets(), *tick)
	if err := SaveCheckpoint(path, cp); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CommittedTick != 3 || loaded.ProjectFingerprint != bundle.Project.Fingerprint() {
		t.Fatalf("loaded checkpoint mismatch: %+v", loaded)
	}
	resumed, err := ResumeFromCheckpoint(bundle.Project, DefaultBudgets(), loaded)
	if err != nil {
		t.Fatal(err)
	}
	full := bundle.Truth
	if !reportMatchesFromTick(resumed, full, 4) {
		t.Fatal("resumed suffix does not match truth")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	trunc := filepath.Join(dir, "trunc.sqlite")
	if err := os.WriteFile(trunc, raw[:64], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCheckpoint(trunc); BizCode(err) != CodeCheckpointTruncated {
		t.Fatalf("expected %s, got %v", CodeCheckpointTruncated, err)
	}

	badPath := filepath.Join(dir, "bad.sqlite")
	bad := *cp
	bad.FormatVersion = "v0"
	payload, _ := json.Marshal(&bad)
	native, err := BuildNativeSQLite(checkpointTables(&bad, string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(badPath, native, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCheckpoint(badPath); BizCode(err) != CodeCheckpointVersion {
		t.Fatalf("expected %s, got %v", CodeCheckpointVersion, err)
	}
}

func TestDiagnosticSuiteAllExpectedCodes(t *testing.T) {
	report, err := RunDiagnostics(42, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range report.Cases {
		if !c.OK {
			t.Errorf("diagnostic %s failed: expected %s got %s (%s)", c.Name, c.ExpectedCode, c.Code, c.Message)
		}
	}
	if !report.AllOK {
		t.Fatal("diagnostic report not all OK")
	}
}

func TestFullVerificationPasses(t *testing.T) {
	report, err := RunFullVerification(42, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllOK {
		data, _ := json.MarshalIndent(report, "", "  ")
		t.Fatalf("verification failed: %s", data)
	}
}
