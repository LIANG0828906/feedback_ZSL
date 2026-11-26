package queryopt

import (
	"reflect"
	"testing"
)

func assertIncrementalEqualsFull(t *testing.T, engine *IncrementalEngine) {
	t.Helper()
	fullEst, fullPlans, err := FullRecompute(engine.Project(), engine.bindings, engine.opts)
	if err != nil {
		t.Fatalf("full recompute: %v", err)
	}
	for id, got := range engine.estimates {
		want := fullEst.Predicates[id]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("incremental estimate %s mismatch:\ngot=%+v\nwant=%+v", id, got, want)
		}
	}
	for qid, got := range engine.plans {
		want := fullPlans[qid]
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("incremental plan %s mismatch:\ngot=%+v\nwant=%+v", qid, got, want)
		}
	}
}

func TestHistogramEditRecomputesOnlyDependencyChain(t *testing.T) {
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(sample.Project, sample.Bindings, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}
	unaffectedBefore := engine.Plan("q2_account_txn")

	bucketPtrs := engine.Project().Indexes.BucketsByColumn["col_order_date"]
	buckets := make([]Bucket, 0, len(bucketPtrs))
	for _, b := range bucketPtrs {
		buckets = append(buckets, *b)
	}
	buckets[0].Rows = 30_000
	report, err := engine.EditHistogram("col_order_date", buckets)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(report.RecomputedQueries, []string{"q1_sales_slice"}) {
		t.Fatalf("recomputed=%v", report.RecomputedQueries)
	}
	if len(report.CacheHitQueries) != 4 {
		t.Fatalf("cache hits=%v", report.CacheHitQueries)
	}
	if engine.Plan("q2_account_txn") != unaffectedBefore {
		t.Fatal("unaffected q2 plan pointer was not reused")
	}
	var q1Result *IncrementalQueryResult
	for i := range report.Results {
		if report.Results[i].QueryID == "q1_sales_slice" {
			q1Result = &report.Results[i]
		}
	}
	if q1Result == nil || q1Result.CacheHit {
		t.Fatalf("q1 result not marked recomputed: %+v", report.Results)
	}
	if !containsString(q1Result.InvalidationSources, "hist:col_order_date") {
		t.Fatalf("sources=%v", q1Result.InvalidationSources)
	}
	if !containsString(q1Result.PropagationClosure, "pred:pred_q1_date_range") ||
		!containsString(q1Result.PropagationClosure, "query:q1_sales_slice") {
		t.Fatalf("closure missing dependency nodes: %v", q1Result.PropagationClosure)
	}
	assertIncrementalEqualsFull(t, engine)
}

func TestPredicateAndRowEditsPropagateAndMatchFull(t *testing.T) {
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewIncrementalEngine(sample.Project, sample.Bindings, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}

	pred := *engine.Project().Indexes.Predicates["pred_q2_skew"]
	pred.Value = PredicateArg{Literal: int64Ptr(1)}
	report, err := engine.EditPredicate(pred)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.RecomputedQueries, []string{"q2_account_txn"}) {
		t.Fatalf("predicate edit recomputed=%v", report.RecomputedQueries)
	}
	assertIncrementalEqualsFull(t, engine)

	report, err = engine.EditTableRowCount("tbl_customer", 12_345)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"q1_sales_slice", "q3_cycle", "q4_cartesian"}
	if !reflect.DeepEqual(report.RecomputedQueries, want) {
		t.Fatalf("row edit recomputed=%v want %v", report.RecomputedQueries, want)
	}
	if containsString(report.CacheHitQueries, "q1_sales_slice") {
		t.Fatalf("q1 should not be cache hit: %v", report.CacheHitQueries)
	}
	assertIncrementalEqualsFull(t, engine)
}

func TestParallelBudgetAndCandidateBackpressure(t *testing.T) {
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	est, err := EstimateProject(sample.Project, sample.Bindings)
	if err != nil {
		t.Fatal(err)
	}
	report, err := RunParallel(sample.Project, est, sample.Bindings, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != len(sample.Project.Queries) {
		t.Fatalf("parallel results=%d", len(report.Results))
	}
	for _, r := range report.Results {
		if r.QueryID == "q5_unreachable" {
			if r.ErrorCode != CodeDisconnectedPlan {
				t.Fatalf("q5 parallel code=%s", r.ErrorCode)
			}
			continue
		}
		if r.ErrorCode != "" || r.Plan == nil || len(r.Plan.SelectedOrder) == 0 {
			t.Fatalf("query %s failed: %+v", r.QueryID, r)
		}
	}

	sched, err := NewBoundedScheduler(2, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !sched.TryAcquire() || !sched.TryAcquire() {
		t.Fatal("failed to fill worker budget")
	}
	backpressured := sched.TryPlan(sample.Project, est, "q1_sales_slice", sample.Bindings)
	if backpressured.Admitted || backpressured.ErrorCode != CodeWorkerBackpressure {
		t.Fatalf("worker backpressure=%+v", backpressured)
	}
	sched.Release()
	sched.Release()

	small, err := NewBoundedScheduler(4, 1)
	if err != nil {
		t.Fatal(err)
	}
	candidateLimit := small.TryPlan(sample.Project, est, "q1_sales_slice", sample.Bindings)
	if candidateLimit.ErrorCode != CodeCandidateBackpressure {
		t.Fatalf("candidate backpressure code=%s plan=%+v", candidateLimit.ErrorCode, candidateLimit.Plan)
	}

	if _, err := NewBoundedScheduler(0, 64); errorCode(err) != CodeBudgetInvalid {
		t.Fatalf("worker budget err=%v", err)
	}
	if _, err := NewBoundedScheduler(4, 0); errorCode(err) != CodeBudgetInvalid {
		t.Fatalf("buffer budget err=%v", err)
	}
}
