package generator

import (
	"fmt"
	"math/rand"

	"maintenanceorchestrator/internal/domain"
)

const DefaultSeed int64 = 424242

func Generate(seed int64) domain.Project {
	rng := rand.New(rand.NewSource(seed))
	p := domain.Project{
		ProjectID:  fmt.Sprintf("hw-maint-%08x", rng.Uint32()),
		Seed:       seed,
		TimeBase:   "canonical_logical_tick",
		ForceArmed: true,
		Window:     domain.Window{Start: 10, End: 22},
		Policies: domain.Policies{
			MaxConcurrentMigrations: 2,
			MaxMigrationTicks:       3,
			ThreadBudget:            4,
			PerNodeQueueLimit:       8,
			CheckpointVersion:       1,
			RecallRule:              "lowest_priority_first",
		},
	}
	for t := p.Window.Start; t <= p.Window.End; t++ {
		p.ProbeTicks = append(p.ProbeTicks, t)
	}

	addNode := func(id string, used int64, borrowCap int64) {
		p.Nodes = append(p.Nodes, domain.Node{
			ID: id, CapacityMilli: 8000, InitialUsedMilli: used,
			WatermarkBasisPoints: 7500, BorrowCapMilli: borrowCap,
			State: domain.StateOnline,
		})
	}
	addNode("m-fast", 2000, 0)
	addNode("m-slot", 1000, 0)
	addNode("m-wave2", 1000, 0)
	addNode("m-block", 5000, 0)
	addNode("t-1", 5000, 2000)
	addNode("t-2", 5000, 2000)
	addNode("t-3", 6000, 2000)
	addNode("t-4", 5000, 2000)
	addNode("t-over", 5000, 1000)

	addInstance := func(id, node string, cores int64, priority, order int, degrade ...int64) {
		var deg int64
		if len(degrade) > 0 {
			deg = degrade[0]
		}
		p.Instances = append(p.Instances, domain.Instance{
			ID: id, NodeID: node, MilliCores: cores, Priority: priority,
			DeclaredOrder: order, MigrationTicks: 1, DegradeMilliCores: deg,
		})
	}

	addInstance("i-fast-1", "m-fast", 1000, 10, 1)
	addInstance("i-slot-1", "m-slot", 1000, 10, 2)
	addInstance("i-fast-2", "m-fast", 1000, 20, 3)
	addInstance("i-wave2-1", "m-wave2", 1000, 10, 4)
	for i := 1; i <= 5; i++ {
		addInstance(fmt.Sprintf("i-block-%d", i), "m-block", 1000, 10, 100+i)
	}

	for _, target := range []string{"t-1", "t-2", "t-4"} {
		for i := 1; i <= 5; i++ {
			addInstance(fmt.Sprintf("w-%s-%d", target, i), target, 1000, 500, 200+i)
		}
	}
	for i := 1; i <= 4; i++ {
		addInstance(fmt.Sprintf("w-t3-%d", i), "t-3", 1000, 500, 300+i)
	}
	addInstance("w-t3-five", "t-3", 1000, 500, 310)
	addInstance("w-t3-degrade", "t-3", 1000, 1000, 399, 1000)
	for i := 1; i <= 5; i++ {
		addInstance(fmt.Sprintf("w-tover-%d", i), "t-over", 1000, 500, 400+i)
	}

	p.Batches = []domain.Batch{
		{ID: "batch-wave1", Wave: 1, NodeIDs: []string{"m-fast", "m-slot"}, Priority: 10, DeclaredOrder: 1, ForceDeclared: true, Forced: true},
		{ID: "batch-wave2", Wave: 2, NodeIDs: []string{"m-wave2"}, Priority: 10, DeclaredOrder: 2},
		{ID: "batch-wave3", Wave: 3, NodeIDs: []string{"m-block"}, Priority: 10, DeclaredOrder: 3},
	}
	p.Waves = []domain.Wave{
		{Seq: 1, BatchIDs: []string{"batch-wave1"}},
		{Seq: 2, BatchIDs: []string{"batch-wave2"}},
		{Seq: 3, BatchIDs: []string{"batch-wave3"}},
	}

	p.Events = []domain.MaintenanceEvent{
		{ID: "evt-mark-fast", NodeID: "m-fast", Kind: domain.EventMarkPending, Tick: 10},
		{ID: "evt-mark-slot", NodeID: "m-slot", Kind: domain.EventMarkPending, Tick: 10},
		{ID: "evt-mark-wave2", NodeID: "m-wave2", Kind: domain.EventMarkPending, Tick: 10},
		{ID: "evt-mark-block", NodeID: "m-block", Kind: domain.EventMarkPending, Tick: 10},
		{ID: "evt-return-wave2", NodeID: "m-wave2", Kind: domain.EventReturned, Tick: 14},
		{ID: "evt-return-fast", NodeID: "m-fast", Kind: domain.EventReturned, Tick: 18},
		{ID: "evt-return-slot", NodeID: "m-slot", Kind: domain.EventReturned, Tick: 20},
	}

	p.Loans = []domain.LoanSpec{
		{
			ID: "loan-fast-t3", LotID: "lot-m-fast", LenderNodeID: "m-fast",
			BorrowerNodeID: "t-3", AmountMilli: 2000, StartTick: 11, ExpiryTick: 17,
			Priority: 10, DeclaredOrder: 1, RecallRule: "expiry_then_policy",
		},
		{
			ID: "loan-slot-over", LotID: "lot-m-slot", LenderNodeID: "m-slot",
			BorrowerNodeID: "t-over", AmountMilli: 1000, StartTick: 11, ExpiryTick: 19,
			Priority: 100, DeclaredOrder: 1, RecallRule: "lowest_priority_first",
		},
		{
			ID: "loan-wave2-over", LotID: "lot-m-wave2", LenderNodeID: "m-wave2",
			BorrowerNodeID: "t-over", AmountMilli: 1000, StartTick: 13, ExpiryTick: 20,
			Priority: 200, DeclaredOrder: 2, RecallRule: "lowest_priority_first",
		},
	}

	return p
}
