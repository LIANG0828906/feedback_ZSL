package acceptance_test

import (
	"encoding/json"
	"testing"

	"coldkernel/internal/gen"
	"coldkernel/internal/loader"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
)

func loadSpec(t *testing.T, spec *gen.Spec) *model.Project {
	t.Helper()
	raw, err := json.Marshal(gen.BuildV1(spec))
	if err != nil {
		t.Fatal(err)
	}
	p, err := loader.Load(raw)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return p
}

func TestColdTimeoutRejection(t *testing.T) {
	spec := &gen.Spec{
		Seed: 1, TickMs: 100, HorizonTicks: 10, ConversionWidth: 64,
		FixedTolerance: 1, Tenants: []string{"t"},
		Functions: []gen.FuncSpec{{
			ID: "fn", ConcurrencyLimit: 2, Reserved: 1, WarmPoolSize: 0,
			// min > timeout guarantees every cold creation overflows.
			ColdMinTicks: 5, ColdMaxTicks: 5, ColdTimeoutTicks: 3,
			BudgetPermyriad: 10000, QueueLimit: 4, WaitLimitTicks: 9,
		}},
		Requests: []gen.ReqSpec{{
			ID: "r1", FunctionID: "fn", ArriveTick: 0, Slots: 1,
			Priority: model.PriorityShared, Tenant: "t", HoldTicks: 2,
		}},
	}
	res := oracle.Simulate(loadSpec(t, spec))
	oc := res.ByRequest["r1"]
	if oc.Decision != model.DecisionReject || oc.ViolatedRule != model.ReasonColdTimeout {
		t.Fatalf("want cold timeout reject, got %s/%s", oc.Decision, oc.ViolatedRule)
	}
}

func TestQueueFullRejection(t *testing.T) {
	spec := &gen.Spec{
		Seed: 1, TickMs: 100, HorizonTicks: 10, ConversionWidth: 64,
		FixedTolerance: 1, Tenants: []string{"t"},
		Functions: []gen.FuncSpec{{
			ID: "fn", ConcurrencyLimit: 1, Reserved: 0, WarmPoolSize: 0,
			ColdMinTicks: 2, ColdMaxTicks: 2, ColdTimeoutTicks: 9,
			BudgetPermyriad: 10000, QueueLimit: 1, WaitLimitTicks: 9,
		}},
	}
	// Five single-slot requests arrive together; one is admitted, one queued,
	// the rest must be rejected with E_QUEUE_FULL.
	for i := 0; i < 5; i++ {
		spec.Requests = append(spec.Requests, gen.ReqSpec{
			ID: "r" + string(rune('0'+i)), FunctionID: "fn", ArriveTick: 0,
			Slots: 1, Priority: model.PriorityShared, Tenant: "t", HoldTicks: 8,
		})
	}
	res := oracle.Simulate(loadSpec(t, spec))
	var queueFull int
	for _, oc := range res.Ticks[0].Rejected {
		if oc.ViolatedRule == model.ReasonQueueFull {
			queueFull++
		}
	}
	if queueFull != 3 {
		t.Fatalf("queue-full rejections=%d want 3", queueFull)
	}
}

func TestBudgetExhaustedRejection(t *testing.T) {
	spec := &gen.Spec{
		Seed: 1, TickMs: 100, HorizonTicks: 40, ConversionWidth: 64,
		FixedTolerance: 1, Tenants: []string{"t"},
		Functions: []gen.FuncSpec{{
			ID: "fn", ConcurrencyLimit: 1, Reserved: 0, WarmPoolSize: 0,
			ColdMinTicks: 1, ColdMaxTicks: 1, ColdTimeoutTicks: 9,
			// Zero budget: every cold start is refused immediately.
			BudgetPermyriad: 0, QueueLimit: 1, WaitLimitTicks: 9,
		}},
		Requests: []gen.ReqSpec{{
			ID: "r1", FunctionID: "fn", ArriveTick: 0, Slots: 1,
			Priority: model.PriorityShared, Tenant: "t", HoldTicks: 2,
		}},
	}
	res := oracle.Simulate(loadSpec(t, spec))
	oc := res.ByRequest["r1"]
	if oc.Decision != model.DecisionReject || oc.ViolatedRule != model.ReasonBudgetExhausted {
		t.Fatalf("want budget rejection, got %s/%s", oc.Decision, oc.ViolatedRule)
	}
}
