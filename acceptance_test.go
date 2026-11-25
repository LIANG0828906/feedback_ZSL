package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"maintenanceorchestrator/internal/app"
	"maintenanceorchestrator/internal/domain"
	"maintenanceorchestrator/internal/generator"
	"maintenanceorchestrator/internal/incremental"
	"maintenanceorchestrator/internal/schema"
	"maintenanceorchestrator/internal/solver"
	"maintenanceorchestrator/internal/store"
)

func TestGeneratedSampleVerifiesAndIsByteDeterministic(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	if _, _, _, err := generator.WriteSample(dir1, generator.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := generator.WriteSample(dir2, generator.DefaultSeed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project.v1.json", "project.v2.json", "project.v3.json", "truth.json"} {
		b1 := readFile(t, filepath.Join(dir1, name))
		b2 := readFile(t, filepath.Join(dir2, name))
		if !reflect.DeepEqual(b1, b2) {
			t.Fatalf("%s is not byte-identical for same seed", name)
		}
	}
	report, err := app.VerifyDirectory(dir1)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK {
		for _, c := range report.Checks {
			if !c.OK {
				t.Errorf("check %s failed: %s", c.Name, c.Detail)
			}
		}
		t.Fatal("verification report failed")
	}
}

func TestThreeSchemaLoadingEquivalence(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	loaded := make([]*domain.Project, 0, 3)
	for _, version := range []string{domain.SchemaV1, domain.SchemaV2, domain.SchemaV3} {
		data, err := schema.Encode(p, version)
		if err != nil {
			t.Fatal(err)
		}
		got, err := schema.Load(data)
		if err != nil {
			t.Fatal(err)
		}
		loaded = append(loaded, got)
	}
	base := canonical(t, loaded[0])
	for i, p := range loaded[1:] {
		if !reflect.DeepEqual(base, canonical(t, p)) {
			t.Fatalf("schema %d canonical mismatch", i+2)
		}
	}

	unarmed := generator.Generate(generator.DefaultSeed)
	unarmed.ForceArmed = false
	data, err := schema.EncodeV1(unarmed)
	if err != nil {
		t.Fatal(err)
	}
	got, err := schema.Load(data)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Batches[0].ForceDeclared || got.Batches[0].Forced {
		t.Fatalf("conditional force marker not gated: %+v", got.Batches[0])
	}
}

func TestWatermarkScaleAndFeasibilityReason(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	report := solver.EvaluateTick(solver.FeasibilityOptions{Project: p, Tick: 10})
	block := nodeReport(t, report.Nodes, "m-block")
	if block.EvacuableCount != 4 {
		t.Fatalf("evacuable count = %d, want 4", block.EvacuableCount)
	}
	decision := instanceDecision(t, block.Decisions, "i-block-5")
	if decision.Movable {
		t.Fatal("i-block-5 should be watermark-blocked")
	}
	if decision.ReasonCode != solver.ReasonTargetWatermark || decision.ConstraintID != domain.EConstraintWatermark {
		t.Fatalf("reason=%s constraint=%s", decision.ReasonCode, decision.ConstraintID)
	}

	v1, err := schema.EncodeV1(p)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := schema.Load(v1)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range loaded.Nodes {
		if n.WatermarkBasisPoints != 7500 {
			t.Fatalf("permille conversion failed for %s: %d", n.ID, n.WatermarkBasisPoints)
		}
	}
}

func TestFeasibilityAdvancesWithCommittedPlacement(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	placement := map[string]string{"i-fast-1": "t-1", "i-slot-1": "t-2"}
	used := solver.UsageFromPlacements(p, placement)
	report := solver.EvaluateTick(solver.FeasibilityOptions{
		Project: p, Tick: 11, Used: used, CurrentNode: placement,
	})
	fast := nodeReport(t, report.Nodes, "m-fast")
	if fast.TotalInstances != 1 || fast.EvacuableCount != 1 {
		t.Fatalf("m-fast after wave1 total=%d evacuable=%d", fast.TotalInstances, fast.EvacuableCount)
	}
	if got := fast.Decisions[0].SelectedTargetID; got != "t-4" {
		t.Fatalf("remaining instance target=%q want t-4", got)
	}
	slot := nodeReport(t, report.Nodes, "m-slot")
	if slot.TotalInstances != 0 || slot.EvacuableCount != 0 {
		t.Fatalf("m-slot should be fully evacuated: %+v", slot)
	}
}

func TestCheckpointResumeAndVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	p := generator.Generate(generator.DefaultSeed)
	a, err := app.Analyze(p)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "checkpoint.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	cp := app.CheckpointForWave(p, a, 1, 11)
	if err := st.SaveCommit(cp); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, found, err := reopened.LoadRecovery()
	if err != nil || !found {
		t.Fatalf("recovery found=%v err=%v", found, err)
	}
	resumed, err := app.ResumePlan(p, loaded)
	if err != nil {
		t.Fatal(err)
	}
	wantMigrations := mustJSON(t, a.Plan.Migrations)
	gotMigrations := mustJSON(t, resumed.Migrations)
	if !reflect.DeepEqual(wantMigrations, gotMigrations) {
		t.Fatalf("resumed migrations mismatch\nwant=%s\n got=%s", wantMigrations, gotMigrations)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	badPath := filepath.Join(dir, "bad-version.db")
	bad, err := store.Open(badPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := bad.SaveCommit(store.Checkpoint{StateVersion: 999, ProjectID: "x", CommittedWave: 1}); err != nil {
		t.Fatal(err)
	}
	if err := bad.Close(); err != nil {
		t.Fatal(err)
	}
	badOpen, err := store.Open(badPath)
	if err != nil {
		t.Fatal(err)
	}
	defer badOpen.Close()
	_, _, err = badOpen.LoadRecovery()
	if !domain.IsCode(err, domain.EStateVersionMismatch) {
		t.Fatalf("err=%v, want E_STATE_VERSION_MISMATCH", err)
	}

	fullPath := filepath.Join(dir, "full.db")
	fullStore, err := store.Open(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	fullCP := app.FullCheckpoint(p, a)
	if err := fullStore.SaveCommit(fullCP); err != nil {
		t.Fatal(err)
	}
	if err := fullStore.Close(); err != nil {
		t.Fatal(err)
	}
	fullOpen, err := store.Open(fullPath)
	if err != nil {
		t.Fatal(err)
	}
	defer fullOpen.Close()
	loadedFull, _, err := fullOpen.LoadRecovery()
	if err != nil {
		t.Fatal(err)
	}
	completedReturns := 0
	for _, in := range loadedFull.Instances {
		if in.ReturnStatus == "completed" {
			completedReturns++
		}
	}
	if completedReturns != len(a.Plan.Migrations) {
		t.Fatalf("completed returns=%d want %d", completedReturns, len(a.Plan.Migrations))
	}
}

func TestFaultCodes(t *testing.T) {
	cases := map[string]string{
		"negative_watermark":   domain.ENegativeWatermark,
		"time_conflict":        domain.ETimeBaseConflict,
		"unknown_field":        domain.ESchemaUnknownFieldCombo,
		"migration_window":     domain.EMigrationExceedsWindow,
		"loan_order":           domain.ELoanStartAfterExpiry,
		"loan_maintenance":     domain.ELoanTargetMaintenance,
		"thread_budget":        domain.EThreadBudgetInvalid,
		"queue_budget":         domain.EQueueBudgetInvalid,
		"backpressure_queue":   domain.EBackpressureQueue,
		"backpressure_thread":  domain.EBackpressureThread,
		"double_booking":       domain.ELoanDoubleBooked,
		"truncated_checkpoint": domain.ECheckpointTruncated,
		"version_mismatch":     domain.EStateVersionMismatch,
	}
	for name, code := range cases {
		t.Run(name, func(t *testing.T) {
			err := app.DiagnoseFault(name, t.TempDir())
			if !domain.IsCode(err, code) {
				t.Fatalf("DiagnoseFault(%s) = %v, want code %s", name, err, code)
			}
		})
	}
}

func TestParallelBudgetsDoNotChangeResults(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	want := mustJSON(t, parallelReportsOrFail(t, p))
	for _, threads := range []int{1, 2, 8} {
		cp := domain.CloneProject(p)
		cp.Policies.ThreadBudget = threads
		gotReports, errs := app.FeasibilityParallel(cp)
		if len(errs) != 0 {
			t.Fatalf("threads=%d errors: %v", threads, errs)
		}
		if got := mustJSON(t, gotReports); !reflect.DeepEqual(want, got) {
			t.Fatalf("threads=%d changed feasibility output", threads)
		}
	}
}

func TestIncrementalInvalidationPropagatesDependencyChain(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	a, err := app.Analyze(p)
	if err != nil {
		t.Fatal(err)
	}
	eng := incremental.NewEngine()
	deps := map[string][]string{
		"feasibility:10": {"node:t-1", "node:t-2", "node:t-3", "node:t-4", "node:t-over"},
		"wave:2":         {"node:t-3", "lease:loan-fast-t3"},
		"lease:fast":     {"lease:loan-fast-t3", "wave:2", "node:t-3"},
		"gate:m-fast":    {"lease:loan-fast-t3", "wave:2", "instance:i-fast-2"},
		"unrelated":      {"node:unrelated"},
	}
	values := map[string]any{
		"feasibility:10": a.Feasibility[0], "wave:2": a.Plan,
		"lease:fast": a.LeaseTimeline, "gate:m-fast": a.Gate, "unrelated": "cached",
	}
	for key, value := range values {
		if _, err := eng.GetOrCompute(key, deps[key], func() (any, error) { return value, nil }); err != nil {
			t.Fatal(err)
		}
	}
	eng.ResetRecords()
	invalidated := eng.Invalidate(incremental.Change{Source: "lease:loan-fast-t3", Path: "$.loans[0].expiry_tick"})
	if len(invalidated) != 3 {
		t.Fatalf("invalidated=%v, want wave/lease/gate", invalidated)
	}
	for key, value := range values {
		if _, err := eng.GetOrCompute(key, deps[key], func() (any, error) { return value, nil }); err != nil {
			t.Fatal(err)
		}
	}
	records := map[string]incremental.Record{}
	for _, r := range eng.Records() {
		records[r.Key] = r
	}
	for _, key := range []string{"wave:2", "lease:fast", "gate:m-fast"} {
		if records[key].Status != "recomputed" || records[key].InvalidationSource != "lease:loan-fast-t3" {
			t.Fatalf("%s record=%+v", key, records[key])
		}
	}
	if records["unrelated"].Status != "reused" {
		t.Fatalf("unrelated should be reused: %+v", records["unrelated"])
	}
}

func canonical(t *testing.T, p *domain.Project) any {
	t.Helper()
	cp := domain.CloneProject(*p)
	cp.SchemaVersion = ""
	data, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func nodeReport(t *testing.T, in []solver.NodeFeasibility, id string) solver.NodeFeasibility {
	for _, n := range in {
		if n.NodeID == id {
			return n
		}
	}
	t.Fatalf("node %s not found", id)
	return solver.NodeFeasibility{}
}

func instanceDecision(t *testing.T, in []solver.InstanceFeasibility, id string) solver.InstanceFeasibility {
	for _, d := range in {
		if d.InstanceID == id {
			return d
		}
	}
	t.Fatalf("instance %s not found", id)
	return solver.InstanceFeasibility{}
}

func parallelReportsOrFail(t *testing.T, p domain.Project) []solver.TickReport {
	t.Helper()
	reports, errs := app.FeasibilityParallel(p)
	if len(errs) != 0 {
		t.Fatalf("parallel feasibility errors: %v", errs)
	}
	return reports
}

func TestSchedulingOrderDoesNotChangeOutcomeAndSourceRemainsImmutable(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	before := mustJSON(t, p)
	a1, err := app.Analyze(p)
	if err != nil {
		t.Fatal(err)
	}

	shuffled := domain.CloneProject(p)
	shuffled.Nodes = append(append([]domain.Node(nil), shuffled.Nodes[1:]...), shuffled.Nodes[0])
	shuffled.Instances = append(append([]domain.Instance(nil), shuffled.Instances[5:]...), shuffled.Instances[:5]...)
	for i := range shuffled.Batches {
		ids := append([]string(nil), shuffled.Batches[i].NodeIDs...)
		for j, k := 0, len(ids)-1; j < k; j, k = j+1, k-1 {
			ids[j], ids[k] = ids[k], ids[j]
		}
		shuffled.Batches[i].NodeIDs = ids
	}
	shuffled.Loans = append([]domain.LoanSpec(nil), p.Loans...)
	for j, k := 0, len(shuffled.Loans)-1; j < k; j, k = j+1, k-1 {
		shuffled.Loans[j], shuffled.Loans[k] = shuffled.Loans[k], shuffled.Loans[j]
	}
	a2, err := app.Analyze(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	for name, x := range map[string][]byte{
		"plan":  mustJSON(t, a1.Plan),
		"lease": mustJSON(t, a1.LeaseTimeline),
		"gate":  mustJSON(t, a1.Gate),
	} {
		var y []byte
		switch name {
		case "plan":
			y = mustJSON(t, a2.Plan)
		case "lease":
			y = mustJSON(t, a2.LeaseTimeline)
		case "gate":
			y = mustJSON(t, a2.Gate)
		}
		if !bytes.Equal(x, y) {
			t.Fatalf("%s changed after input ordering permutation", name)
		}
	}
	after := mustJSON(t, p)
	if !bytes.Equal(before, after) {
		t.Fatal("Analyze modified the source project")
	}
}

func TestRecoverOrRecomputeOnVersionMismatch(t *testing.T) {
	p := generator.Generate(generator.DefaultSeed)
	path := filepath.Join(t.TempDir(), "version.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveCommit(store.Checkpoint{StateVersion: 999, ProjectID: p.ProjectID, CommittedWave: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	outcome, err := app.RecoverOrRecompute(p, reopened)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Recomputed || outcome.Analysis == nil || outcome.Reason != domain.EStateVersionMismatch {
		t.Fatalf("unexpected outcome: %+v", outcome)
	}
}
