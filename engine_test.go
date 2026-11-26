package slikernel

import (
	"context"
	"testing"
)

func TestEngineMatchesDeterministicTruth(t *testing.T) {
	data := fixture(t)
	report := compute(t, data.Project, testOptions())
	assertReportCoreEqual(t, &data.Truth, report)
	if len(report.Results) == 0 {
		t.Fatal("expected evaluation windows")
	}
}

func TestThresholdEqualityAndConsecutiveWindows(t *testing.T) {
	const origin int64 = 1_700_100_000_000
	project := Project{
		ProjectID: "boundary-test", TimeOriginMs: origin,
		EvaluationStartMs: origin, EvaluationEndMs: origin + 2*60_000,
		Services: []Service{{
			Name: "svc",
			SLIs: []SLI{{
				ID: "avail", Kind: KindAvailability, Route: "/r", StatusCodes: []int{200, 500},
				WindowMs: 60_000, TargetPPM: 990_000, RollingWindows: 1,
				Thresholds: []Threshold{
					{ID: "WARN", BurnPPM: 500_000, ConsecutiveWindows: 2},
					{ID: "CRIT", BurnPPM: 1_000_000, ConsecutiveWindows: 1},
				},
				GapPolicy: GapIncludeEmpty,
			}},
		}},
		Observations: []Observation{
			{Seq: 1, Service: "svc", Route: "/r", StatusCode: 200, TimestampMs: origin, Events: 100, Errors: 1, ThroughputEvents: 100},
			{Seq: 2, Service: "svc", Route: "/r", StatusCode: 200, TimestampMs: origin + 60_000, Events: 100, Errors: 1, ThroughputEvents: 100},
		},
	}
	report := compute(t, project, testOptions())
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}
	first := report.Results[0]
	second := report.Results[1]
	if first.Budget.ConsumedPPM != 1_000_000 {
		t.Fatalf("boundary consumption = %d, want exactly 1000000", first.Budget.ConsumedPPM)
	}
	if first.Alert.Thresholds[1].Matched != true || first.Alert.Thresholds[1].Fired != true {
		t.Fatalf("equality threshold must trigger CRIT on first window: %+v", first.Alert)
	}
	if first.Alert.Thresholds[0].Fired {
		t.Fatalf("WARN must not fire before consecutive requirement: %+v", first.Alert)
	}
	if !second.Alert.Thresholds[0].Fired || second.Alert.Thresholds[0].ConsecutiveObserved != 2 {
		t.Fatalf("WARN consecutive state mismatch: %+v", second.Alert)
	}
	if first.StartMs != origin || first.EndMs != origin+60_000 {
		t.Fatalf("half-open boundary mismatch: %+v", first)
	}
}

func TestInteriorGapKeepsQuantileAndThroughputExpectation(t *testing.T) {
	const origin int64 = 1_700_200_000_000
	project := Project{
		ProjectID: "gap-test", TimeOriginMs: origin,
		EvaluationStartMs: origin, EvaluationEndMs: origin + 3*60_000,
		Services: []Service{{
			Name: "svc",
			SLIs: []SLI{
				{
					ID: "latency", Kind: KindLatency, Route: "/r", StatusCodes: []int{200},
					WindowMs: 60_000, TargetPPM: 900_000, RollingWindows: 3,
					QuantilePPM: 500_000, LatencyThresholdUs: 1_000, GapPolicy: GapIncludeEmpty,
				},
				{
					ID: "throughput", Kind: KindThroughput, Route: "/r", StatusCodes: []int{200},
					WindowMs: 60_000, TargetPPM: 900_000, RollingWindows: 3,
					ExpectedEventsPerWindow: 10, GapPolicy: GapIncludeEmpty,
				},
			},
		}},
		Observations: []Observation{
			{
				Seq: 1, Service: "svc", Route: "/r", StatusCode: 200,
				TimestampMs: origin + 1, Events: 3, LatencySamplesUs: []int64{3, 1, 2},
				ThroughputEvents: 3,
			},
			{
				Seq: 2, Service: "svc", Route: "/r", StatusCode: 200,
				TimestampMs: origin + 2*60_000 + 1, Events: 3,
				LatencySamplesUs: []int64{300, 100, 200}, ThroughputEvents: 3,
			},
		},
	}
	report := compute(t, project, testOptions())
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}
	latency := report.Results[0]
	if latency.QuantileUs != 3 || latency.Total != 6 {
		t.Fatalf("empty middle window shifted quantile: %+v", latency)
	}
	throughput := report.Results[1]
	if throughput.Total != 30 || throughput.Failures != 24 || throughput.SLIValuePPM != 200_000 {
		t.Fatalf("empty window must remain in throughput expectation: %+v", throughput)
	}
}

func TestConcurrencyAndSubmissionOrderDeterminism(t *testing.T) {
	data := fixture(t)
	options := []Options{
		{Workers: 1, PerWorkerBuffer: 1, BatchSize: 1, SubmissionOrder: "forward"},
		{Workers: 2, PerWorkerBuffer: 3, SubmissionOrder: "reverse"},
		{Workers: 4, PerWorkerBuffer: 8, SubmissionOrder: "shuffle"},
		{Workers: 8, PerWorkerBuffer: 4, BatchSize: 2, SubmissionOrder: "forward"},
	}
	base := compute(t, data.Project, options[0])
	for index, option := range options[1:] {
		report := compute(t, data.Project, option)
		assertReportCoreEqual(t, base, report)
		if index == 0 && len(report.Diagnostics.Backpressure) == 0 {
			t.Fatalf("expected observable backpressure when capacity is 2 and work is larger")
		}
	}
}

func TestBackpressureCodeAndStateSafety(t *testing.T) {
	data := fixture(t)
	engine, err := NewEngine(data.Project, Options{
		Workers: 1, PerWorkerBuffer: 1, BatchSize: 1,
		SubmissionOrder: "forward", FailOnBackpressure: true,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	_, err = engine.ComputeAll(context.Background())
	if code := ErrorCode(err); code != CodeBackpressure {
		t.Fatalf("expected %s, got %v", CodeBackpressure, err)
	}
	normalEngine, err := NewEngine(data.Project, Options{Workers: 1, PerWorkerBuffer: 1, BatchSize: 1})
	if err != nil {
		t.Fatalf("NewEngine normal: %v", err)
	}
	report, err := normalEngine.ComputeAll(context.Background())
	if err != nil {
		t.Fatalf("normal compute after backpressure: %v", err)
	}
	if len(report.Results) == 0 {
		t.Fatal("expected results after backpressure")
	}
}

func TestThreadAndBufferBudgetErrors(t *testing.T) {
	data := fixture(t)
	_, err := NewEngine(data.Project, Options{Workers: MaxWorkers + 1, PerWorkerBuffer: 1})
	if code := ErrorCode(err); code != CodeThreadBudget {
		t.Fatalf("expected %s, got %v", CodeThreadBudget, err)
	}
	_, err = NewEngine(data.Project, Options{Workers: 1, PerWorkerBuffer: MaxPerWorkerBuffer + 1})
	if code := ErrorCode(err); code != CodeBufferBudget {
		t.Fatalf("expected %s, got %v", CodeBufferBudget, err)
	}
}
