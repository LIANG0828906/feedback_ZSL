package engine_test

import (
	"context"
	"testing"

	"alertkernel/engine"
	"alertkernel/errs"
	"alertkernel/model"
	"alertkernel/sample"
	"alertkernel/schema"
)

func TestCanceledContextDoesNotAdvanceLogicalStep(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	runner, err := engine.NewRunner(bundle.Project, bundle.Events, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Step(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Step(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runner.Step(canceled, 2)
	if !errs.IsCode(err, errs.EInterrupted) {
		t.Fatalf("expected %s, got %v", errs.EInterrupted, err)
	}
	if runner.BasisSeq() != 2 {
		t.Fatal("expected committed step 0 to retain deterministic basis sequence")
	}
}

func TestWorkerCountsProduceSameLogicalReport(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	one := *bundle.Project
	one.Budget.Workers = 1
	one.Budget.PerRuleBuffer = 1
	eight := *bundle.Project
	eight.Budget.Workers = 8
	eight.Budget.PerRuleBuffer = 2

	r1, err := engine.RunAll(context.Background(), &one, bundle.Events)
	if err != nil {
		t.Fatal(err)
	}
	r8, err := engine.RunAll(context.Background(), &eight, bundle.Events)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Times) != len(r8.Times) {
		t.Fatalf("time length %d != %d", len(r1.Times), len(r8.Times))
	}
	for timeIndex := range r1.Times {
		a := r1.Times[timeIndex]
		b := r8.Times[timeIndex]
		if len(a.States) != len(b.States) || len(a.Visible) != len(b.Visible) || len(a.Notifications) != len(b.Notifications) {
			t.Fatalf("t=%d cardinality differs", timeIndex)
		}
	}
}

func TestMissingMetricIsExplicitNoData(t *testing.T) {
	bundle, err := sample.Generate()
	if err != nil {
		t.Fatal(err)
	}
	project := *bundle.Project
	for _, rule := range project.Rules {
		if rule.ID == "R_CPU" {
			rule.Predicate.Metric = "metric.does.not.exist"
		}
	}
	report, err := engine.RunAll(context.Background(), &project, bundle.Events)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, obs := range report.Times[2].Observations {
		if obs.RuleID == "R_CPU" && obs.NoDataReason == "missing_metric" && !obs.HasData {
			found = true
		}
	}
	if !found {
		t.Fatal("missing metric was not reported as explicit no-data")
	}
}

func TestPredicateFixedPointSaturationThroughEngine(t *testing.T) {
	add := int64(28)
	project := &model.Project{
		ProjectID: "fixed-point",
		Clock:     model.Clock{BaseEpochMS: 1764000000000, StepMS: 1000, Steps: 1},
		Numeric:   model.NumericConfig{Scale: 1, Bits: 8, OverflowPolicy: "saturate"},
		Budget:    model.Budget{Workers: 1, PerRuleBuffer: 1},
		Receivers: []model.Receiver{{Name: "log"}},
		Series: []*model.Series{{
			ID: "s1", Metric: "m", Labels: model.Labels{"host": "a"},
			Samples: []model.Sample{{Index: 0, AtMS: 1764000000000, Raw: 100}},
		}},
		Rules: []*model.Rule{{
			ID: "R1", Receiver: "log", GroupBy: []string{"host"},
			Predicate: model.Predicate{Metric: "m", Op: ">=", Threshold: 127, AddRaw: &add},
		}},
	}
	if err := schema.Canonicalize(project); err != nil {
		t.Fatal(err)
	}
	report, err := engine.RunAll(context.Background(), project, model.EventBundle{})
	if err != nil {
		t.Fatal(err)
	}
	obs := report.Times[0].Observations[0]
	if !obs.HasData || !obs.Matched || !obs.Saturated || obs.ValueRaw == nil || *obs.ValueRaw != 127 {
		t.Fatalf("unexpected saturated observation: %+v", obs)
	}
}
