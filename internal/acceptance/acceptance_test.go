package acceptance

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"samplingkernel/internal/engine"
	"samplingkernel/internal/errs"
	"samplingkernel/internal/exceptions"
	"samplingkernel/internal/loader"
	"samplingkernel/internal/merge"
	"samplingkernel/internal/model"
	"samplingkernel/internal/runtime"
	"samplingkernel/internal/sample"
	"samplingkernel/internal/sampling"
	"samplingkernel/internal/store"
	"samplingkernel/internal/verify"
)

func TestThreeSchemaVersionsMatchTruth(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	canonical := engine.RunFull(bundle.Project)
	if !engine.ReportsSemanticEqual(canonical, bundle.Truth) {
		t.Fatalf("canonical report differs from generator truth: %s", cmp.Diff(bundle.Truth, canonical))
	}

	for _, version := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		t.Run(version, func(t *testing.T) {
			loaded, err := loader.Load(bundle.Docs[version])
			if err != nil {
				t.Fatal(err)
			}
			normalized := *loaded
			normalized.SchemaVersion = ""
			if diff := cmp.Diff(bundle.Project, &normalized); diff != "" {
				t.Fatalf("project mismatch (-truth +loaded):\n%s", diff)
			}
			report := engine.RunFull(loaded)
			if !engine.ReportsSemanticEqual(report, bundle.Truth) {
				t.Fatalf("observation mismatch: %s", cmp.Diff(bundle.Truth, report))
			}
		})
	}
}

func TestOrphanAttachPolicy(t *testing.T) {
	spans := []model.Span{
		{SpanID: "root", TraceID: "tr", ServiceID: "gw", CausalOrder: 1},
		{SpanID: "orphan", TraceID: "tr", ParentSpanID: "missing", ServiceID: "risk", CausalOrder: 9},
	}
	result := merge.MergeTrace("tr", spans, model.OrphanAttach)
	if result.Status != "merged_with_attached_orphans" || len(result.OrderedSpanIDs) != 2 {
		t.Fatalf("orphan should be attached: %+v", result)
	}
	if len(result.UnmergedSpans) != 0 || result.Spans[1].MergeState != "orphan_attached_to_trace_root" {
		t.Fatalf("unexpected attach result: %+v", result)
	}

	chain := merge.MergeTrace("tr", []model.Span{
		{SpanID: "root", TraceID: "tr", CausalOrder: 1},
		{SpanID: "missing-parent", TraceID: "tr", ParentSpanID: "absent", CausalOrder: 2},
		{SpanID: "child-of-orphan", TraceID: "tr", ParentSpanID: "missing-parent", CausalOrder: 3},
	}, model.OrphanQuarantine)
	if len(chain.OrderedSpanIDs) != 1 || len(chain.UnmergedSpans) != 2 {
		t.Fatalf("orphan descendants should be quarantined together: %+v", chain)
	}
}

func TestUnknownFieldHasStableLocation(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(bundle.Docs[model.SchemaV1], &raw); err != nil {
		t.Fatal(err)
	}
	raw["unexpected_test_field"] = true
	data, _ := json.Marshal(raw)
	_, err = loader.LoadVersion(model.SchemaV1, data)
	if errs.CodeOf(err) != errs.SchemaUnknownField {
		t.Fatalf("expected %s, got %v", errs.SchemaUnknownField, err)
	}
	coded, _ := errs.As(err)
	if coded.Path != "$.unexpected_test_field" || coded.Version != model.SchemaV1 {
		t.Fatalf("unexpected error context: %+v", coded)
	}
}

func TestHeadDecisionsAreStableUnderConcurrency(t *testing.T) {
	edge := model.Edge{ID: "e-checkout", No: 1, BaseRatePermyriad: 3500}
	want := sampling.DecideHead("project", edge, "req-stable", "tr-stable")

	var wg sync.WaitGroup
	results := make(chan model.HeadDecision, 1000)
	for batch := 0; batch < 10; batch++ {
		for worker := 0; worker < 20; worker++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 5; i++ {
					results <- sampling.DecideHead("project", edge, "req-stable", "tr-stable")
				}
			}()
		}
	}
	wg.Wait()
	close(results)
	for got := range results {
		if got != want {
			t.Fatalf("non-deterministic decision: %+v != %+v", got, want)
		}
	}

	for _, rate := range []uint32{0, 10000} {
		boundaryEdge := model.Edge{ID: "boundary", BaseRatePermyriad: rate}
		for i := 0; i < 1000; i++ {
			decision := sampling.DecideHead("project", boundaryEdge, "req-boundary", "trace")
			if rate == 0 && decision.Keep {
				t.Fatal("zero rate flipped to keep")
			}
			if rate == 10000 && !decision.Keep {
				t.Fatal("full rate flipped to drop")
			}
		}
	}
}

func TestTailPriorityForcedAndBudgetSemantics(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	report := engine.RunFull(bundle.Project)
	tailByTrace := map[string]model.TailDecision{}
	for _, decision := range report.Tail {
		tailByTrace[decision.TraceID] = decision
	}

	multi := tailByTrace["tr-1001"]
	if multi.SelectedRuleID != "tr-error" || len(multi.MatchedRuleIDs) != 3 {
		t.Fatalf("unexpected priority adjudication: %+v", multi)
	}
	forced := tailByTrace["tr-1004"]
	if !forced.Forced || forced.DecisionType != "FORCED_RETAIN" {
		t.Fatalf("forced trace not retained: %+v", forced)
	}

	var ordinaryRiskBudget, forcedBudget *model.BudgetDecision
	var globalReject, serviceReject *model.BudgetDecision
	for i := range report.Budget {
		decision := &report.Budget[i]
		switch decision.EventSeq {
		case 17:
			ordinaryRiskBudget = decision
		case 28:
			forcedBudget = decision
		case 22:
			globalReject = decision
		case 47:
			serviceReject = decision
		}
	}
	if forcedBudget == nil || !forcedBudget.Accepted || !forcedBudget.Overshot {
		t.Fatalf("forced budget should be accepted with overshoot: %+v", forcedBudget)
	}
	var ordinaryRiskCharged uint64
	for _, service := range ordinaryRiskBudget.Services {
		if service.ServiceID == "risk" {
			ordinaryRiskCharged = service.ChargedSpans
		}
	}
	for _, service := range forcedBudget.Services {
		if service.ServiceID == "risk" {
			if service.ForcedSpans != 1 || service.ChargedSpans != ordinaryRiskCharged || ordinaryRiskCharged != 1 {
				t.Fatalf("forced trace consumed allocatable service quota: %+v", service)
			}
		}
	}
	if globalReject == nil || globalReject.Accepted || globalReject.RejectCode != "E_BUDGET_GLOBAL_EXHAUSTED" {
		t.Fatalf("missing global rejection: %+v", globalReject)
	}
	if serviceReject == nil || serviceReject.Accepted || serviceReject.RejectCode != "E_BUDGET_SERVICE_EXHAUSTED" {
		t.Fatalf("missing service rejection: %+v", serviceReject)
	}
}

func TestMergeOrderDuplicatesAndOrphans(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	report := engine.RunFull(bundle.Project)
	first := report.Merge["tr-1001"]
	wantOrder := []string{"sp-1001-gw", "sp-1001-ord", "sp-1001-pay", "sp-1001-inv"}
	if diff := cmp.Diff(wantOrder, first.OrderedSpanIDs); diff != "" {
		t.Fatalf("causal order mismatch: %s", diff)
	}
	if diff := cmp.Diff([]string{"sp-1001-pay"}, first.DuplicateSpanIDs); diff != "" {
		t.Fatalf("duplicate mismatch: %s", diff)
	}
	second := report.Merge["tr-1002"]
	if len(second.UnmergedSpans) != 1 || second.UnmergedSpans[0].SpanID != "sp-1002-orphan-risk" {
		t.Fatalf("orphan not quarantined: %+v", second.UnmergedSpans)
	}
	if report.Merge["tr-unknown"].Status != "quarantined_orphans" {
		t.Fatalf("unknown trace orphan should be isolated: %+v", report.Merge["tr-unknown"])
	}
}

func TestIncrementalEditsEqualFullRecompute(t *testing.T) {
	report, err := verify.Run()
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range report.Checks {
		if !check.OK {
			t.Fatalf("verification check failed: %s %s", check.Name, check.Detail)
		}
	}
}

func TestSchedulerBackpressureAndParallelMerge(t *testing.T) {
	scheduler, err := runtime.NewScheduler(runtime.Options{Workers: 2, BufferCap: 1})
	if err != nil {
		t.Fatal(err)
	}
	span := model.Span{SpanID: "sp-1", TraceID: "tr-bp", ServiceID: "gw"}
	if err := scheduler.SubmitSpan(span); err != nil {
		t.Fatal(err)
	}
	err = scheduler.SubmitSpan(model.Span{SpanID: "sp-2", TraceID: "tr-bp", ServiceID: "gw"})
	if errs.CodeOf(err) != errs.BackpressureBuffer {
		t.Fatalf("expected buffer backpressure, got %v", err)
	}
	if scheduler.BufferedSpanCount("tr-bp") != 1 {
		t.Fatalf("rejected span must not be buffered, count=%d", scheduler.BufferedSpanCount("tr-bp"))
	}
	if merged, mergeErr := scheduler.TryRunTrace("tr-bp", model.OrphanQuarantine); mergeErr != nil || len(merged.OrderedSpanIDs) != 1 {
		t.Fatalf("committed span must remain mergeable: %+v err=%v", merged, mergeErr)
	}

	oneWorker, _ := runtime.NewScheduler(runtime.Options{Workers: 1, BufferCap: 4})
	release, err := oneWorker.TryAcquireWorker()
	if err != nil {
		t.Fatal(err)
	}
	_, err = oneWorker.TryAcquireWorker()
	if errs.CodeOf(err) != errs.BackpressureWorkers {
		t.Fatalf("expected worker backpressure, got %v", err)
	}
	release()

	parallel, _ := runtime.NewScheduler(runtime.Options{Workers: 4, BufferCap: 32})
	for _, traceID := range []string{"tr-a", "tr-b"} {
		_ = parallel.SubmitSpan(model.Span{SpanID: traceID + "-gw", TraceID: traceID, ServiceID: "gw", CausalOrder: 1})
		_ = parallel.SubmitSpan(model.Span{SpanID: traceID + "-ord", TraceID: traceID, ParentSpanID: traceID + "-gw", ServiceID: "ord", CausalOrder: 2})
	}
	results, err := parallel.RunAllParallel(context.Background(), model.OrphanQuarantine)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || len(results["tr-a"].OrderedSpanIDs) != 2 {
		t.Fatalf("parallel merge failed: %+v", results)
	}

	for _, options := range []runtime.Options{{Workers: 0, BufferCap: 4}, {Workers: 100, BufferCap: 4}} {
		_, err := runtime.NewScheduler(options)
		if errs.CodeOf(err) != errs.RuntimeWorkerBudgetBad {
			t.Fatalf("expected worker budget error for %+v, got %v", options, err)
		}
	}
	for _, options := range []runtime.Options{{Workers: 2, BufferCap: 0}, {Workers: 2, BufferCap: 99999}} {
		_, err := runtime.NewScheduler(options)
		if errs.CodeOf(err) != errs.RuntimeBufferBudgetBad {
			t.Fatalf("expected buffer budget error for %+v, got %v", options, err)
		}
	}
}

func TestCheckpointCrashRecoveryAndVersionMismatch(t *testing.T) {
	ctx := context.Background()
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "checkpoint.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runtime.RunCheckpointed(ctx, bundle.Project, st, runtime.Options{Workers: 2, BufferCap: 32}, 20)
	if errs.CodeOf(err) != runtime.CrashAfterCommitCode {
		t.Fatalf("expected crash injection, got %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := st.CompletedTraceJSON(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 2 {
		t.Fatalf("expected exactly two complete readable traces at seq20, got %d: %v", len(completed), completed)
	}

	result, err := runtime.RunCheckpointed(ctx, bundle.Project, st, runtime.Options{Workers: 2, BufferCap: 32}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recovery.Action != "resume_matching_version" || result.Recovery.LoadedSeq != 20 {
		t.Fatalf("unexpected recovery info: %+v", result.Recovery)
	}
	if !engine.ReportsSemanticEqual(result.Report, bundle.Truth) {
		t.Fatalf("recovered report mismatch: %s", cmp.Diff(bundle.Truth, result.Report))
	}
	_, mismatchErr := st.Load(ctx, model.SchemaV2)
	if errs.CodeOf(mismatchErr) != errs.CheckpointVersionMismatch {
		t.Fatalf("expected version mismatch, got %v", mismatchErr)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	v2Project, err := loader.Load(bundle.Docs[model.SchemaV2])
	if err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := runtime.RunCheckpointed(ctx, v2Project, st, runtime.Options{Workers: 2, BufferCap: 32}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Recovery.Action != "restart_from_start_version_mismatch" {
		t.Fatalf("expected full restart after mismatch, got %+v", restarted.Recovery)
	}
	if !engine.ReportsSemanticEqual(restarted.Report, bundle.Truth) {
		t.Fatal("version-mismatch restart did not reproduce truth")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExceptionInjectionCodesAndSourceUnmodified(t *testing.T) {
	ctx := context.Background()
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	originalRate := bundle.Project.Edges[0].BaseRatePermyriad
	originalFirstWeight := bundle.Project.Allocations[0].WeightPermyriad
	results, err := exceptions.All(ctx, bundle.Project, bundle.Docs[model.SchemaV1], t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]string{
		"rate_out_of_range":           errs.RateOutOfRange,
		"tail_condition_cycle":        errs.TailConditionCycle,
		"allocation_sum_exceeded":     errs.AllocationSumExceeded,
		"edge_service_missing":        errs.EdgeServiceMissing,
		"worker_budget_out_of_bounds": errs.RuntimeWorkerBudgetBad,
		"buffer_budget_out_of_bounds": errs.RuntimeBufferBudgetBad,
		"unknown_schema_field":        errs.SchemaUnknownField,
		"truncated_checkpoint":        errs.CheckpointTruncated,
		"version_mismatch_snapshot":   errs.CheckpointVersionMismatch,
	}
	if len(results) != len(expected) {
		t.Fatalf("got %d exception results, want %d: %+v", len(results), len(expected), results)
	}
	for _, result := range results {
		if expected[result.Name] != result.Code {
			t.Fatalf("exception %s code=%s want %s", result.Name, result.Code, expected[result.Name])
		}
	}
	if bundle.Project.Edges[0].BaseRatePermyriad != originalRate ||
		bundle.Project.Allocations[0].WeightPermyriad != originalFirstWeight {
		t.Fatal("exception injection modified source project")
	}
}

func TestTruncatedCheckpointDirectly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "truncated.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveProgress(ctx, store.Checkpoint{ProjectID: "x", ProjectVersion: model.SchemaV1, LastSeq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 64); err != nil {
		t.Fatal(err)
	}
	_, openErr := store.Open(path)
	if errs.CodeOf(openErr) != errs.CheckpointTruncated {
		st, loadErr := store.Open(path)
		if loadErr == nil {
			defer st.Close()
			_, loadErr = st.Load(ctx, model.SchemaV1)
		}
		if errs.CodeOf(loadErr) != errs.CheckpointTruncated {
			t.Fatalf("expected truncated checkpoint, open=%v load=%v", openErr, loadErr)
		}
	}
}
