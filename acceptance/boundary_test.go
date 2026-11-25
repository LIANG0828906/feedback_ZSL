package acceptance_test

import (
	"encoding/json"
	"testing"

	"coldkernel/internal/gen"
	"coldkernel/internal/loader"
	"coldkernel/internal/model"
	"coldkernel/internal/oracle"
)

// buildBoundaryProject crafts a tiny project where one function has limit 3,
// reserved 2, no warm pool and requests that fill every slot exactly.
func buildBoundaryProject(t *testing.T) *gen.Spec {
	spec := &gen.Spec{
		Seed: 7, TickMs: 100, HorizonTicks: 12, ConversionWidth: 64,
		FixedTolerance: 1, Tenants: []string{"tenant-a"},
		Functions: []gen.FuncSpec{{
			ID: "fn-a", ConcurrencyLimit: 3, Reserved: 2, WarmPoolSize: 0,
			ColdMinTicks: 2, ColdMaxTicks: 2, ColdTimeoutTicks: 9,
			BudgetPermyriad: 10000, QueueLimit: 5, WaitLimitTicks: 8,
		}},
		Requests: []gen.ReqSpec{
			{ID: "r1", FunctionID: "fn-a", ArriveTick: 0, Slots: 2,
				Priority: model.PriorityReserved, Tenant: "tenant-a", HoldTicks: 3},
			{ID: "r2", FunctionID: "fn-a", ArriveTick: 0, Slots: 1,
				Priority: model.PriorityShared, Tenant: "tenant-a", HoldTicks: 3},
			// Third request would exceed the absolute limit -> queued.
			{ID: "r3", FunctionID: "fn-a", ArriveTick: 0, Slots: 1,
				Priority: model.PriorityShared, Tenant: "tenant-a", HoldTicks: 2},
			// A reserved request while the shared slot is occupied must still
			// fit once a reserved slot frees, and must never be squeezed by
			// the shared request.
			{ID: "r4", FunctionID: "fn-a", ArriveTick: 1, Slots: 1,
				Priority: model.PriorityReserved, Tenant: "tenant-a", HoldTicks: 2},
		},
	}
	return spec
}

func TestLastSlotBoundaryConsistentAcrossSchemas(t *testing.T) {
	spec := buildBoundaryProject(t)
	load := func(snap any) *model.Project {
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		p, err := loader.Load(raw)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return p
	}
	p1 := load(gen.BuildV1(spec))
	p2 := load(gen.BuildV2(spec))
	p3 := load(gen.BuildV3(spec))

	r1 := oracle.Simulate(p1)
	r2 := oracle.Simulate(p2)
	r3 := oracle.Simulate(p3)

	// At tick 0: r1 (reserved, 2 slots) and r2 (shared, 1 slot) exactly fill
	// the 3-slot limit; r3 must be queued against C3.
	var oc *model.RequestOutcome
	for _, q := range r1.Ticks[0].Queued {
		if q.RequestID == "r3" {
			oc = q
		}
	}
	if oc == nil {
		t.Fatal("r3 must appear in tick0 queued set")
	}
	if oc.ViolatedRule != model.ReasonTotalFull || oc.QueueRank != 1 {
		t.Fatalf("r3 want QUEUE/C3 rank1, got %s/%s rank%d",
			oc.Decision, oc.ViolatedRule, oc.QueueRank)
	}

	for i, rr := range []*model.RunResult{r2, r3} {
		var found bool
		for _, q := range rr.Ticks[0].Queued {
			if q.RequestID == "r3" {
				found = true
				if q.ViolatedRule != model.ReasonTotalFull || q.QueueRank != 1 {
					t.Fatalf("schema v%d boundary divergence: %+v", i+2, q)
				}
			}
		}
		if !found {
			t.Fatalf("schema v%d missing r3 queued verdict", i+2)
		}
	}
}

func TestReservedNotSqueezedByShared(t *testing.T) {
	spec := buildBoundaryProject(t)
	raw, _ := json.Marshal(gen.BuildV1(spec))
	p, err := loader.Load(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := oracle.Simulate(p)
	// r4 is RESERVED while 2 reserved + 1 shared slots are busy. It must wait
	// until a reserved slot frees at tick 6, rather than taking shared space.
	oc := res.ByRequest["r4"]
	if oc.Decision == model.DecisionReject {
		t.Fatalf("reserved request must not be rejected: %+v", oc)
	}
	state := res.Ticks[0].Functions["fn-a"]
	if state.ReservedUsed != 2 || state.SharedUsed != 1 {
		t.Fatalf("tick0 class accounting wrong: %+v", state)
	}
}
