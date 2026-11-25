package chaosdiag_test

import (
	"reflect"
	"testing"

	"chaosdiag/gen"
	"chaosdiag/incremental"
	"chaosdiag/model"
)

func TestIncrementalAmplitudeRecomputesOnlyClosure(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := incremental.NewEngine(bundle.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	before := eng.Report()

	newDelay := int64(100)
	out, err := eng.Apply(incremental.Mutation{
		Kind: "instruction_amplitude", InstructionID: "I-DELAY-INV", DelayMS: &newDelay,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertExactIDs(t, out.Recomputed, []string{"R-03-LATENCY-SLO"})
	if len(out.Reused) != len(before.Results)-1 {
		t.Fatalf("reused=%d want %d", len(out.Reused), len(before.Results)-1)
	}
	for _, d := range out.CacheDecisions {
		if d.RequestID == "R-03-LATENCY-SLO" {
			if d.Reused || len(d.Sources) == 0 {
				t.Fatalf("edited request was not invalidated: %+v", d)
			}
		} else if !d.Reused {
			t.Fatalf("unrelated request %s was recomputed", d.RequestID)
		}
	}

	full := incremental.FullReport(eng.Project())
	if !reflect.DeepEqual(out.Report.Results, full.Results) {
		t.Fatal("incremental amplitude results differ from full recompute")
	}
	if !reflect.DeepEqual(out.Report.Verdicts, full.Verdicts) {
		t.Fatal("incremental verdicts differ from full recompute")
	}
	if out.Closure.FirstAffectedHop["R-03-LATENCY-SLO"] != 1 {
		t.Fatalf("first affected hop=%d want 1", out.Closure.FirstAffectedHop["R-03-LATENCY-SLO"])
	}
}

func TestIncrementalEdgeAndSelectorClosuresMatchFullRecompute(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("edge latency", func(t *testing.T) {
		eng, err := incremental.NewEngine(bundle.Canonical)
		if err != nil {
			t.Fatal(err)
		}
		latency := int64(35)
		out, err := eng.Apply(incremental.Mutation{Kind: "edge_latency", EdgeID: "edge-order-inventory", EdgeLatencyMS: &latency})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"R-02-BASE-INV", "R-03-LATENCY-SLO", "R-06-BUDGET-TIMEOUT", "R-11-REORDER", "R-14-RETRY-INV"}
		assertExactIDs(t, out.Recomputed, want)
		full := incremental.FullReport(eng.Project())
		if !reflect.DeepEqual(out.Report.Results, full.Results) {
			t.Fatal("edge incremental results differ from full recompute")
		}
	})

	t.Run("selector move", func(t *testing.T) {
		eng, err := incremental.NewEngine(bundle.Canonical)
		if err != nil {
			t.Fatal(err)
		}
		out, err := eng.Apply(incremental.Mutation{
			Kind: "instruction_selector", InstructionID: "I-FAIL-PAY",
			Selector: &model.Selector{Kind: model.TargetEdge, EdgeID: "edge-order-inventory"},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"R-04-INJECTED-FAIL", "R-06-BUDGET-TIMEOUT"}
		assertExactIDs(t, out.Recomputed, want)
		full := incremental.FullReport(eng.Project())
		if !reflect.DeepEqual(out.Report.Results, full.Results) {
			t.Fatal("selector incremental results differ from full recompute")
		}
	})
}

func TestIncrementalWindowMutationMatchesFullRecompute(t *testing.T) {
	bundle, err := gen.Generate()
	if err != nil {
		t.Fatal(err)
	}
	eng, err := incremental.NewEngine(bundle.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	endTick := 4
	out, err := eng.Apply(incremental.Mutation{
		Kind: "instruction_window", InstructionID: "I-DELAY-INV", EndTick: &endTick,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(out.Recomputed, "R-03-LATENCY-SLO") {
		t.Fatalf("window closure did not invalidate R-03: %v", out.Recomputed)
	}
	full := incremental.FullReport(eng.Project())
	if !reflect.DeepEqual(out.Report.Results, full.Results) {
		t.Fatal("window incremental results differ from full recompute")
	}
	if !reflect.DeepEqual(out.Report.Schedules, full.Schedules) {
		t.Fatal("window incremental schedules differ from full recompute")
	}
}

func containsString(in []string, want string) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

func assertExactIDs(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ids mismatch\n got=%v\nwant=%v", got, want)
	}
}
