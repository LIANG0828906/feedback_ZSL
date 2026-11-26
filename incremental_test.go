package slikernel

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func newTestEngine(t *testing.T, project Project, options Options) *Engine {
	t.Helper()
	engine, err := NewEngine(project, options)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if _, err := engine.ComputeAll(context.Background()); err != nil {
		t.Fatalf("initial ComputeAll: %v", err)
	}
	return engine
}

func TestIncrementalTargetEditReusesAggregation(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	target := int64(990_000)
	result, err := engine.ApplyEdits(context.Background(), []Edit{{
		ID: "edit-target", Kind: EditSetSLOTarget, Service: "checkout",
		SLI: "checkout-availability", TargetPPM: &target,
	}})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	full := compute(t, engine.Project(), testOptions())
	assertReportCoreEqual(t, full, result.Report)
	if len(result.RecomputedEval) != 10 {
		t.Fatalf("expected 10 recomputed budget windows, got %d", len(result.RecomputedEval))
	}
	for _, key := range result.RecomputedEval {
		if key.Service != "checkout" || key.SLI != "checkout-availability" {
			t.Fatalf("unexpected recomputed key: %+v", key)
		}
	}
	index := resultIndex(result.Report)
	affected := index[ResultKey{Service: "checkout", SLI: "checkout-availability", EvalIndex: 2}]
	if !affected.Provenance.AggregationFromCache {
		t.Fatalf("target-only edit must reuse aggregation: %+v", affected.Provenance)
	}
	if affected.Provenance.BudgetFromCache || affected.Provenance.AlertFromCache {
		t.Fatalf("target edit must recompute budget and alert: %+v", affected.Provenance)
	}
	if len(affected.Provenance.InvalidationIDs) != 1 || affected.Provenance.InvalidationIDs[0] != "edit-target" {
		t.Fatalf("invalid provenance: %+v", affected.Provenance)
	}
	if len(affected.Provenance.PropagationClosure) != 2 {
		t.Fatalf("expected evaluate+alert closure, got %+v", affected.Provenance.PropagationClosure)
	}
	unaffected := index[ResultKey{Service: "search", SLI: "search-availability", EvalIndex: 2}]
	if !unaffected.Provenance.FromCache {
		t.Fatalf("unrelated service result should be reused: %+v", unaffected.Provenance)
	}
	for _, source := range result.Sources {
		if len(source.PropagationClosure) == 0 {
			t.Fatalf("missing propagation closure for %s", source.EditID)
		}
		for _, node := range source.PropagationClosure {
			if node.Stage == "aggregate" {
				t.Fatalf("target edit must not invalidate aggregate nodes: %+v", node)
			}
		}
	}
}

func TestIncrementalWindowLengthEdit(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	window := int64(120_000)
	result, err := engine.ApplyEdits(context.Background(), []Edit{{
		ID: "edit-window", Kind: EditSetWindowLength, Service: "search",
		SLI: "search-latency", WindowMs: &window,
	}})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	full := compute(t, engine.Project(), testOptions())
	assertReportCoreEqual(t, full, result.Report)
	if len(result.RecomputedEval) != 4 {
		t.Fatalf("expected 4 evaluation windows after 120s change, got %d", len(result.RecomputedEval))
	}
	for _, key := range result.RecomputedEval {
		if key.Service != "search" || key.SLI != "search-latency" {
			t.Fatalf("unexpected recomputed key: %+v", key)
		}
	}
	for _, key := range result.ReusedEval {
		if key.Service == "search" && key.SLI == "search-latency" {
			t.Fatalf("changed SLI result should not be reused: %+v", key)
		}
	}
}

func TestIncrementalLatencyRuleAndThresholdOnly(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	threshold := int64(30_000)
	quantile := int64(900_000)
	result, err := engine.ApplyEdits(context.Background(), []Edit{
		{
			ID: "edit-latency", Kind: EditSetLatencyThreshold, Service: "payments",
			SLI: "payments-latency", LatencyThresholdUs: &threshold,
		},
		{
			ID: "edit-quantile", Kind: EditSetQuantile, Service: "payments",
			SLI: "payments-latency", QuantilePPM: &quantile,
		},
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	assertReportCoreEqual(t, compute(t, engine.Project(), testOptions()), result.Report)
	affected := resultIndex(result.Report)[ResultKey{Service: "payments", SLI: "payments-latency", EvalIndex: 5}]
	if affected.Provenance.AggregationFromCache || affected.Provenance.BudgetFromCache {
		t.Fatalf("latency rule edit must recompute aggregation and budget: %+v", affected.Provenance)
	}

	thresholds := []Threshold{{ID: "WARN", BurnPPM: 400_000, ConsecutiveWindows: 1}}
	alertOnly, err := engine.ApplyEdits(context.Background(), []Edit{{
		ID: "edit-alerts", Kind: EditSetThresholds, Service: "checkout",
		SLI: "checkout-availability", Thresholds: &thresholds,
	}})
	if err != nil {
		t.Fatalf("ApplyEdits alerts: %v", err)
	}
	assertReportCoreEqual(t, compute(t, engine.Project(), testOptions()), alertOnly.Report)
	if len(alertOnly.RecomputedEval) != 0 {
		t.Fatalf("threshold-only edit must not recompute budget windows: %+v", alertOnly.RecomputedEval)
	}
	if len(alertOnly.RecomputedAlerts) != 10 {
		t.Fatalf("expected 10 alert recomputations, got %d", len(alertOnly.RecomputedAlerts))
	}
	alertWindow := resultIndex(alertOnly.Report)[ResultKey{Service: "checkout", SLI: "checkout-availability", EvalIndex: 2}]
	if !alertWindow.Provenance.AggregationFromCache || !alertWindow.Provenance.BudgetFromCache {
		t.Fatalf("alert-only edit reused upstream incorrectly: %+v", alertWindow.Provenance)
	}
	if alertWindow.Provenance.AlertFromCache || alertWindow.Provenance.FromCache {
		t.Fatalf("alert must be recomputed: %+v", alertWindow.Provenance)
	}
}

func TestIncrementalAppendOnlyAffectsOverlappingAndDownstream(t *testing.T) {
	data := fixture(t)
	beforeJSON, _ := json.Marshal(data.Project)
	engine := newTestEngine(t, data.Project, testOptions())
	lastSeq := data.Project.LastObservationSeq()
	timestamp := data.Project.TimeOriginMs + 3*60_000 + 100
	appended := Observation{
		Seq: lastSeq + 1, Service: "checkout", Route: "/checkout", StatusCode: 500,
		TimestampMs: timestamp, Events: 100, Errors: 100,
		LatencySamplesUs: []int64{90_000, 120_000}, ThroughputEvents: 100,
	}
	result, err := engine.ApplyEdits(context.Background(), []Edit{{
		ID: "append-1", Kind: EditAppendObservations, Observations: []Observation{appended},
	}})
	if err != nil {
		t.Fatalf("ApplyEdits append: %v", err)
	}
	afterJSON, _ := json.Marshal(data.Project)
	if !reflect.DeepEqual(beforeJSON, afterJSON) {
		t.Fatal("ApplyEdits mutated the source project")
	}
	assertReportCoreEqual(t, compute(t, engine.Project(), testOptions()), result.Report)
	if len(result.RecomputedEval) != 9 {
		t.Fatalf("expected 3 windows x 3 checkout SLIs recomputed, got %d", len(result.RecomputedEval))
	}
	for _, key := range result.RecomputedEval {
		if key.Service != "checkout" || key.EvalIndex < 3 || key.EvalIndex > 5 {
			t.Fatalf("append affected an unexpected evaluation window: %+v", key)
		}
	}
	index := resultIndex(result.Report)
	direct := index[ResultKey{Service: "checkout", SLI: "checkout-availability", EvalIndex: 4}]
	if direct.Provenance.AggregationFromCache || direct.Provenance.BudgetFromCache {
		t.Fatalf("overlapping window must recompute aggregation and budget: %+v", direct.Provenance)
	}
	downstream := index[ResultKey{Service: "checkout", SLI: "checkout-availability", EvalIndex: 9}]
	if !downstream.Provenance.AggregationFromCache || !downstream.Provenance.BudgetFromCache {
		t.Fatalf("non-overlap downstream window should reuse aggregation/budget: %+v", downstream.Provenance)
	}
	if downstream.Provenance.AlertFromCache || downstream.Provenance.FromCache {
		t.Fatalf("downstream alert must be recomputed: %+v", downstream.Provenance)
	}
	if len(downstream.Provenance.InvalidationIDs) == 0 || downstream.Provenance.InvalidationIDs[0] != "append-1" {
		t.Fatalf("missing downstream provenance: %+v", downstream.Provenance)
	}
	if len(downstream.Provenance.PropagationClosure) != 1 ||
		downstream.Provenance.PropagationClosure[0].Stage != "alert" {
		t.Fatalf("expected alert-only downstream closure: %+v", downstream.Provenance.PropagationClosure)
	}
	unrelated := index[ResultKey{Service: "search", SLI: "search-availability", EvalIndex: 9}]
	if !unrelated.Provenance.FromCache {
		t.Fatalf("unrelated result should be reused: %+v", unrelated.Provenance)
	}
}

func TestIncrementalFailureLeavesEngineUsable(t *testing.T) {
	data := fixture(t)
	engine := newTestEngine(t, data.Project, testOptions())
	badWindow := int64(-1)
	_, err := engine.ApplyEdits(context.Background(), []Edit{{
		ID: "bad", Kind: EditSetWindowLength, Service: "checkout",
		SLI: "checkout-latency", WindowMs: &badWindow,
	}})
	if code := ErrorCode(err); code != CodeWindowLength {
		t.Fatalf("expected %s, got %v", CodeWindowLength, err)
	}
	report, err := engine.ComputeAll(context.Background())
	if err != nil {
		t.Fatalf("engine unusable after failed edit: %v", err)
	}
	truth := compute(t, data.Project, testOptions())
	assertReportCoreEqual(t, truth, report)
}
