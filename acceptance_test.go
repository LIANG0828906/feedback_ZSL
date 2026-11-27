package planopt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestThreeSchemaVersionsConverge(t *testing.T) {
	generated := Generate(42)
	formats := []struct {
		name    string
		marshal func(Project) ([]byte, error)
	}{
		{"v1", MarshalV1},
		{"v2", MarshalV2},
		{"v3", MarshalV3},
	}

	base, err := OptimizeProject(generated.Project, DefaultBindings(generated.Project))
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range formats {
		data, err := format.marshal(generated.Project)
		if err != nil {
			t.Fatalf("%s marshal: %v", format.name, err)
		}
		project, err := LoadEngineeringJSON(data)
		if err != nil {
			t.Fatalf("%s load: %v", format.name, err)
		}
		if err := EquivalentProject(generated.Project, project); err != nil {
			t.Fatalf("%s equivalence: %v", format.name, err)
		}
		loaded, err := OptimizeProject(project, DefaultBindings(project))
		if err != nil {
			t.Fatalf("%s optimize: %v", format.name, err)
		}
		assertOptimizationsEqual(t, format.name, base, loaded)
	}
}

func TestSuppliedV1V2ArtifactsLoad(t *testing.T) {
	for _, path := range []string{
		"artifacts/demo/generated/engineering-v1.json",
		"artifacts/demo/generated/engineering-v2.json",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("optional supplied artifact unavailable: %s", path)
		}
		project, err := LoadEngineeringJSON(data)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(project.Plans) != 6 {
			t.Fatalf("%s plans=%d", path, len(project.Plans))
		}
		if _, err := OptimizeProject(project, DefaultBindings(project)); err != nil {
			t.Fatalf("%s optimize: %v", path, err)
		}
	}
}

func TestNormalizationTruthAndPushdownSemantics(t *testing.T) {
	generated := Generate(42)
	plans, err := OptimizeProject(generated.Project, DefaultBindings(generated.Project))
	if err != nil {
		t.Fatal(err)
	}
	bySeq := map[int]OptimizedPlan{}
	for _, plan := range plans {
		bySeq[plan.PlanSeq] = plan
	}

	for _, truthPlan := range generated.Truth.Plans {
		opt := bySeq[truthPlan.PlanSeq]
		if len(opt.Normalized.Operators) != len(truthPlan.Operators) {
			t.Fatalf("plan %d ops=%d truth=%d", truthPlan.PlanSeq, len(opt.Normalized.Operators), len(truthPlan.Operators))
		}
		for i, opTruth := range truthPlan.Operators {
			op := opt.Normalized.Operators[i]
			if op.Key != opTruth.Key {
				t.Fatalf("plan %d op %d key=%s truth=%s", truthPlan.PlanSeq, i, op.Key, opTruth.Key)
			}
			if !reflect.DeepEqual(op.OutputColumns, opTruth.OutputColumns) {
				t.Fatalf("plan %d op %d output columns mismatch", truthPlan.PlanSeq, i)
			}
		}
		if !reflect.DeepEqual(opt.Normalized.RetainedColumns, truthPlan.RetainedColumns) {
			t.Fatalf("plan %d retained mismatch", truthPlan.PlanSeq)
		}
		if !reflect.DeepEqual(opt.JoinOrder, truthPlan.Optimization.JoinOrder) {
			t.Fatalf("plan %d join=%v truth=%v", truthPlan.PlanSeq, opt.JoinOrder, truthPlan.Optimization.JoinOrder)
		}
		for _, estimate := range opt.Estimates {
			if !estimate.WithinTolerance {
				t.Fatalf("plan %d predicate %s error=%d tolerance=%d", truthPlan.PlanSeq, estimate.PredicateID, estimate.AbsoluteErrorPPM, SelTolerancePPM)
			}
		}
		for _, op := range opt.Normalized.Operators {
			for _, col := range op.OutputColumns {
				if stringInSlice(col, opt.Normalized.PrunedColumns) {
					t.Fatalf("plan %d pruned column %s in %s", truthPlan.PlanSeq, col, op.Key)
				}
			}
		}
	}

	p1 := bySeq[1]
	if !stringInSlice("filter:p1_pred_customer_eq", operatorKeys(p1.Normalized)) {
		t.Fatalf("p1 predicate was not pushed: %v", operatorKeys(p1.Normalized))
	}
	p5 := bySeq[5]
	blocked := false
	for _, decision := range p5.Normalized.Pushdowns {
		if decision.PredicateID == "p5_pred_segment_eq" {
			blocked = !decision.Pushed && decision.Reason == "LEFT_OUTER_NULLABLE_RIGHT_SIDE_BLOCKED"
		}
	}
	if !blocked {
		t.Fatalf("left-outer right predicate pushdown was not blocked: %+v", p5.Normalized.Pushdowns)
	}
	if stringInSlice("filter:p5_pred_segment_eq", operatorKeys(p5.Normalized)) {
		t.Fatal("nullable-side predicate incorrectly pushed below LEFT OUTER JOIN")
	}

	boundaryChecks := map[string]struct {
		id       string
		ppm      int64
		boundary bool
	}{
		"p4": {id: "p4_pred_region_eq", ppm: 125000, boundary: true},
		"p5": {id: "p5_pred_segment_eq", ppm: 250000, boundary: true},
		"p6": {id: "p6_pred_order_gt", boundary: true},
	}
	for name, check := range boundaryChecks {
		opt := bySeq[map[string]int{"p4": 4, "p5": 5, "p6": 6}[name]]
		var estimate PredicateEstimate
		for _, candidate := range opt.Estimates {
			if candidate.PredicateID == check.id {
				estimate = candidate
			}
		}
		if estimate.PredicateID == "" || !estimate.BucketBoundary {
			t.Fatalf("%s boundary estimate missing: %+v", name, estimate)
		}
		if check.ppm != 0 && estimate.SelectivityPPM != check.ppm {
			t.Fatalf("%s selectivity=%d want=%d", name, estimate.SelectivityPPM, check.ppm)
		}
	}
}

func TestBindingSensitivityGlobalAndRelative(t *testing.T) {
	project := Generate(42).Project
	global := map[int]int64{7: 1000}
	relative := map[int]map[int]int64{6: {1: 1000}}
	if err := GlobalRelativeBindingEquivalence(project, global, relative); err != nil {
		t.Fatal(err)
	}
	bundle, err := CompareBindings(project, BindingSet{}, BindingSet{Global: global})
	if err != nil {
		t.Fatal(err)
	}
	var plan6 SensitivityReport
	for _, report := range bundle.Reports {
		if report.PlanSeq == 6 {
			plan6 = report
		}
	}
	if plan6.PlanSeq != 6 {
		t.Fatal("missing plan 6 sensitivity report")
	}
	if !plan6.AccessPathChanged || !plan6.Changed {
		t.Fatalf("expected path sensitivity, got %+v", plan6)
	}
	if !stringInSlice("p6_pred_order_gt", plan6.TriggerPredicateIDs) {
		t.Fatalf("trigger mismatch: %v", plan6.TriggerPredicateIDs)
	}
	if plan6.BeforePaths["orders"] != "pk_orders" || plan6.AfterPaths["orders"] != "seq_orders" {
		t.Fatalf("path change %v -> %v", plan6.BeforePaths, plan6.AfterPaths)
	}
	if plan6.CostDifferenceMilli == 0 {
		t.Fatal("expected non-zero cost difference")
	}
}

func TestIncrementalMatchesFullAndRecordsClosure(t *testing.T) {
	generated := Generate(42)
	bindings := DefaultBindings(generated.Project)
	engine, err := NewIncrementalEngine(generated.Project, bindings)
	if err != nil {
		t.Fatal(err)
	}

	customers, _ := generated.Project.Table("customers")
	segment, _ := customers.Column("segment")
	segment.NullPermyriad = 200

	p6, _ := generated.Project.Plan(6)
	predicate := p6.Predicates[0]
	predicate.Op = "GTE"

	edits := []Edit{
		{Kind: EditTableStats, Table: "customers", Column: &segment},
		{Kind: EditPredicate, PlanSeq: 6, PredicateID: predicate.ID, Predicate: &predicate},
		{Kind: EditOperatorParam, PlanSeq: 6, NodeID: "p6_limit", ParamKey: "limit_rows", ParamValue: 37},
	}
	for _, edit := range edits {
		report, err := engine.ApplyEdits(edit)
		if err != nil {
			t.Fatalf("%s: %v", edit.Kind, err)
		}
		step := report.Steps[0]
		if step.InvalidationSource == "" || len(step.PropagationClosure) == 0 {
			t.Fatalf("%s missing source/closure", edit.Kind)
		}
		if len(step.ReusedKeys) == 0 {
			t.Fatalf("%s did not reuse any unaffected cache key", edit.Kind)
		}
		if len(step.OperatorIntervals) == 0 {
			t.Fatalf("%s missing operator interval", edit.Kind)
		}
		full, err := OptimizeProject(engine.ProjectSnapshot(), bindings)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(engine.Results(), full) {
			t.Fatalf("%s incremental result differs from full optimization", edit.Kind)
		}
	}
}

func TestParallelDeterminismAndBackpressure(t *testing.T) {
	project := Generate(42).Project
	bindings := DefaultBindings(project)
	sequential, err := OptimizeProject(project, bindings)
	if err != nil {
		t.Fatal(err)
	}

	for _, workers := range []int{1, 2, 4, 8} {
		scheduler := NewScheduler(workers, 64)
		outcomes := scheduler.RunAll(context.Background(), project, bindings)
		if len(outcomes) != len(project.Plans) {
			t.Fatalf("workers=%d outcomes=%d", workers, len(outcomes))
		}
		results := SuccessfulResults(outcomes)
		assertOptimizationsEqual(t, "parallel", sequential, results)
	}

	shuffled := cloneProject(project)
	for i, j := 0, len(shuffled.Plans)-1; i < j; i, j = i+1, j-1 {
		shuffled.Plans[i], shuffled.Plans[j] = shuffled.Plans[j], shuffled.Plans[i]
	}
	shuffledOutcomes := NewScheduler(3, 64).RunAll(context.Background(), shuffled, bindings)
	assertOptimizationsEqual(t, "shuffled", sequential, SuccessfulResults(shuffledOutcomes))

	bufferScheduler := NewScheduler(2, 1)
	_, err = bufferScheduler.TryProcessPlan(project, project.Plans[0], bindings)
	assertErrorCode(t, err, CodeBackpressureBuffer)

	workerScheduler := NewScheduler(1, 64)
	release, err := workerScheduler.AcquireWorker()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = workerScheduler.TryProcessPlan(project, project.Plans[0], bindings)
	assertErrorCode(t, err, CodeBackpressureWorkers)
}

func TestCheckpointCommitOrderAndInjections(t *testing.T) {
	project := Generate(42).Project
	plans, err := OptimizeProject(project, DefaultBindings(project))
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(project, DefaultBindings(project))
	if err != nil {
		t.Fatal(err)
	}
	cache := engine.CacheSnapshot()

	dir := t.TempDir()
	store := NewFileCheckpoint(dir)
	for _, plan := range plans {
		if err := store.CommitPlan(plan, cache); err != nil {
			t.Fatal(err)
		}
	}
	recovered, err := store.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Manifest.CommittedSeq != 6 || len(recovered.States) != 6 {
		t.Fatalf("seq=%d states=%d", recovered.Manifest.CommittedSeq, len(recovered.States))
	}

	orphanDir := t.TempDir()
	orphan := NewFileCheckpoint(orphanDir)
	if err := orphan.CommitPlan(plans[0], cache); err != nil {
		t.Fatal(err)
	}
	if err := orphan.WritePlanState(plans[1]); err != nil {
		t.Fatal(err)
	}
	orphanRecovered, err := orphan.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if orphanRecovered.Manifest.CommittedSeq != 1 || len(orphanRecovered.States) != 1 {
		t.Fatalf("orphan visible seq=%d states=%d", orphanRecovered.Manifest.CommittedSeq, len(orphanRecovered.States))
	}

	truncatedPlan := filepath.Join(dir, "plans", planStateFileName(1))
	if err := os.WriteFile(truncatedPlan, []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Recover()
	assertErrorCode(t, err, CodeCheckpointTruncated)

	truncatedDir := t.TempDir()
	truncated := NewFileCheckpoint(truncatedDir)
	if err := truncated.InjectTruncatedManifest(); err != nil {
		t.Fatal(err)
	}
	_, err = truncated.Recover()
	assertErrorCode(t, err, CodeCheckpointTruncated)

	versionDir := t.TempDir()
	versioned := NewFileCheckpoint(versionDir)
	if err := versioned.InjectVersionMismatchSnapshot(); err != nil {
		t.Fatal(err)
	}
	_, err = versioned.Recover()
	assertErrorCode(t, err, CodeCheckpointVersionMismatch)
	rebuilt, didRebuild, err := versioned.RecoverOrRebuild(project, DefaultBindings(project))
	if err != nil || !didRebuild || len(rebuilt) != 6 {
		t.Fatalf("version rebuild: err=%v rebuilt=%v len=%d", err, didRebuild, len(rebuilt))
	}
	recoveredAfterRebuild, err := versioned.Recover()
	if err != nil || len(recoveredAfterRebuild.States) != 6 {
		t.Fatalf("rebuilt checkpoint recovery: err=%v states=%d", err, len(recoveredAfterRebuild.States))
	}
}

func TestSchemaExceptionInjectionCodes(t *testing.T) {
	data, err := MarshalV1(Generate(42).Project)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		doc  []byte
		code string
	}{
		{"malformed tree", InjectMalformedNodeTreeV1(data), CodeMalformedNodeTree},
		{"placeholder", InjectPlaceholderOutOfRangeV1(data), CodePlaceholderOutOfRange},
		{"scale", InjectScaleOutOfRangeV1(data), CodeScaleOutOfRange},
		{"unknown combination", InjectUnknownFieldCombinationV1(data), CodeFieldCombination},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadEngineeringJSON(tc.doc)
			assertErrorCode(t, err, tc.code)
		})
	}
	_, err = LoadEngineeringJSON([]byte(`{"schemaVersion":"9.9"}`))
	assertErrorCode(t, err, CodeSchemaUnknown)
}

func assertOptimizationsEqual(t *testing.T, context string, want, got []OptimizedPlan) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: len %d != %d", context, len(got), len(want))
	}
	for i := range want {
		if want[i].PlanSeq != got[i].PlanSeq || want[i].CostMilli != got[i].CostMilli {
			t.Fatalf("%s plan %d cost %d != %d", context, want[i].PlanSeq, got[i].CostMilli, want[i].CostMilli)
		}
		if !reflect.DeepEqual(want[i].JoinOrder, got[i].JoinOrder) {
			t.Fatalf("%s plan %d join %v != %v", context, want[i].PlanSeq, got[i].JoinOrder, want[i].JoinOrder)
		}
		if !reflect.DeepEqual(want[i].SelectedPath, got[i].SelectedPath) {
			t.Fatalf("%s plan %d paths %v != %v", context, want[i].PlanSeq, got[i].SelectedPath, want[i].SelectedPath)
		}
		if !reflect.DeepEqual(operatorKeys(want[i].Normalized), operatorKeys(got[i].Normalized)) {
			t.Fatalf("%s plan %d operators %v != %v", context, want[i].PlanSeq, operatorKeys(got[i].Normalized), operatorKeys(want[i].Normalized))
		}
	}
}

func operatorKeys(n NormalizedPlan) []string {
	out := make([]string, 0, len(n.Operators))
	for _, op := range n.Operators {
		out = append(out, op.Key)
	}
	return out
}

func assertErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var businessError *Error
	if !errors.As(err, &businessError) {
		t.Fatalf("expected code %s, got non-business error: %v", code, err)
	}
	if businessError.Code != code {
		t.Fatalf("code=%s want=%s err=%v", businessError.Code, code, err)
	}
}
