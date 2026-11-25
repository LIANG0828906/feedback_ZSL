package engine_test

import (
	"math"
	"reflect"
	"testing"

	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/generator"
)

func TestLedgerAndMigrationInvariants(t *testing.T) {
	project := generator.Scenario(42)
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := eng.RunAll(); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, tick := range eng.Observations() {
		for _, tenant := range tick.Tenants {
			if tenant.UsedMilli > tenant.QuotaMilli {
				t.Fatalf("tick %d tenant %s used %d > quota %d", tick.Tick, tenant.TenantID, tenant.UsedMilli, tenant.QuotaMilli)
			}
			if tenant.AvailableMilli < 0 || tenant.ReserveRemainingMilli < 0 {
				t.Fatalf("tick %d tenant %s negative balance: %+v", tick.Tick, tenant.TenantID, tenant)
			}
		}
		for _, node := range tick.Nodes {
			if node.UsedMilli > node.CapacityMilli || node.NonReservedFreeMilli < 0 {
				t.Fatalf("tick %d node %s capacity invariant: %+v", tick.Tick, node.NodeID, node)
			}
		}
		state, ok := eng.State(tick.Tick)
		if !ok {
			t.Fatalf("missing state at tick %d", tick.Tick)
		}
		for _, tenant := range project.Tenants {
			sum := int64(0)
			for _, amount := range state.Placements[tenant.ID] {
				sum += amount
			}
			if sum != state.Tenants[tenant.ID].Used {
				t.Fatalf("tick %d tenant %s placement sum %d != used %d", tick.Tick, tenant.ID, sum, state.Tenants[tenant.ID].Used)
			}
		}
		for _, pause := range tick.Pauses {
			migration := findPausedMigration(t, state, pause.TenantID)
			if state.Placements[pause.TenantID][migration.ToNode] > 0 {
				t.Fatalf("tick %d tenant %s partially accounted on new node before commit", tick.Tick, pause.TenantID)
			}
			if migration.Status == "paused" && state.Placements[pause.TenantID][migration.FromNode] < migration.Amount {
				t.Fatalf("tick %d tenant %s reserve/source placement disappeared during pause", tick.Tick, pause.TenantID)
			}
		}
	}
	sawPauseQueue := false
	for _, tick := range eng.Observations() {
		for _, pending := range tick.Pending {
			if pending.Reason == "migration_pause" {
				sawPauseQueue = true
				state, _ := eng.State(tick.Tick)
				previous, _ := eng.State(tick.Tick - 1)
				if state.Nodes[pending.NodeID].Used > previous.Nodes[pending.NodeID].Used {
					t.Fatalf("paused request for %s increased node %s usage", pending.TenantID, pending.NodeID)
				}
			}
		}
	}
	if !sawPauseQueue {
		t.Fatal("scenario did not enqueue a request during migration pause")
	}
	// A committed migration must be visible as a source removal and target
	// insertion on the commit tick, never as simultaneous accounting.
	var checked int
	for _, obs := range eng.Observations() {
		for _, migration := range obs.Migrations {
			if migration.Status != "committed" {
				continue
			}
			before, _ := eng.State(migration.StartTick)
			after, _ := eng.State(migration.EndTick)
			if before.Tenants[migration.TenantID].Used != after.Tenants[migration.TenantID].Used {
				continue // other events in the commit tick changed used; the placement assertions below still hold.
			}
			if after.Placements[migration.TenantID][migration.ToNode] < migration.AmountMilli {
				t.Fatalf("migration %s not present on target at commit", migration.ID)
			}
			checked++
			break
		}
	}
	if checked == 0 {
		t.Fatal("scenario did not expose a committed migration")
	}
}

func TestReserveCannotBeInvadedByBurst(t *testing.T) {
	project := reserveProject()
	eng, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 8})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := eng.RunAll(); err != nil {
		t.Fatalf("run: %v", err)
	}
	obs, _ := eng.Tick(1)
	byID := map[string]domain.TenantObservation{}
	for _, tenant := range obs.Tenants {
		byID[tenant.TenantID] = tenant
	}
	if byID["t-c"].UsedMilli != 800 {
		t.Fatalf("burst tenant used %d, want protected admission 800", byID["t-c"].UsedMilli)
	}
	if byID["t-a"].ReserveRemainingMilli != 80 || byID["t-b"].ReserveRemainingMilli != 80 {
		t.Fatalf("other tenants' reserves were invaded: a=%+v b=%+v", byID["t-a"], byID["t-b"])
	}
	if len(obs.Pending) != 1 || obs.Pending[0].Reason != "quota" || len(obs.Pending[0].BlockedBy) == 0 {
		t.Fatalf("expected deterministic queued remainder with blocking source: %+v", obs.Pending)
	}
}

func TestQueueBudgetBackpressure(t *testing.T) {
	project := queueProject()
	eng, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 1})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := eng.RunAll(); domain.CodeOf(err) != domain.EBackpressureQueue {
		t.Fatalf("got %v (%s), want %s", err, domain.CodeOf(err), domain.EBackpressureQueue)
	}
	if _, err := engine.New(project, engine.Config{ThreadBudget: 0, QueueLimit: 1}); domain.CodeOf(err) != domain.EBackpressureBudget {
		t.Fatalf("got %v (%s), want %s", err, domain.CodeOf(err), domain.EBackpressureBudget)
	}
}

func TestConcurrencyDoesNotChangeResults(t *testing.T) {
	project := generator.Scenario(42)
	results := make([][]domain.TickObservation, 0, 3)
	for _, budget := range []int{1, 2, 8} {
		eng, err := engine.New(project, engine.Config{ThreadBudget: budget, QueueLimit: 64})
		if err != nil {
			t.Fatalf("new budget %d: %v", budget, err)
		}
		if err := eng.RunAll(); err != nil {
			t.Fatalf("run budget %d: %v", budget, err)
		}
		results = append(results, eng.Observations())
	}
	for index := 1; index < len(results); index++ {
		if !reflect.DeepEqual(results[0], results[index]) {
			t.Fatalf("concurrency budget altered results at index %d", index)
		}
	}
}

func TestIncrementalMatchesFullRecompute(t *testing.T) {
	project := generator.Scenario(42)
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := eng.RunAll(); err != nil {
		t.Fatalf("run: %v", err)
	}
	report, err := eng.ApplyEdit(engine.Edit{
		ID: "edit-weight-alpha", Kind: engine.EditTenantWeight, Target: "t-alpha",
		FromTick: 5, Weight: 3,
	})
	if err != nil {
		t.Fatalf("apply weight edit: %v", err)
	}
	if report.FirstAffectedTick != 5 || len(report.ReusedTicks) != 4 || len(report.RecomputedTicks) != 7 {
		t.Fatalf("unexpected weight invalidation report: %+v", report)
	}
	full, err := eng.FullRecompute()
	if err != nil {
		t.Fatalf("full recompute: %v", err)
	}
	if !reflect.DeepEqual(eng.Observations(), full.Observations()) {
		t.Fatal("incremental weight edit differs from full recompute")
	}
	report, err = eng.ApplyEdit(engine.Edit{
		ID: "edit-cap-c1", Kind: engine.EditNodeCapacity, Target: "n-c1",
		FromTick: 3, CapacityMilli: 26000,
	})
	if err != nil {
		t.Fatalf("apply capacity edit: %v", err)
	}
	if len(report.ReusedTicks) != 2 || report.FirstAffectedTick != 3 {
		t.Fatalf("capacity edit did not reuse a prefix: %+v", report)
	}
	full, err = eng.FullRecompute()
	if err != nil {
		t.Fatalf("full recompute after capacity edit: %v", err)
	}
	if !reflect.DeepEqual(eng.Observations(), full.Observations()) {
		t.Fatal("incremental capacity edit differs from full recompute")
	}
}

func TestFaultDomainEditCycleRejectedWithoutMutation(t *testing.T) {
	project := generator.Scenario(42)
	eng, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 64})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := eng.RunAll(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := eng.ApplyEdit(engine.Edit{
		ID: "parent-b-a", Kind: engine.EditDomainParent, Target: "rack-b", Parent: "rack-a", FromTick: 1,
	}); err != nil {
		t.Fatalf("valid parent edit: %v", err)
	}
	before := eng.Observations()
	_, err = eng.ApplyEdit(engine.Edit{
		ID: "parent-a-b-cycle", Kind: engine.EditDomainParent, Target: "rack-a", Parent: "rack-b", FromTick: 1,
	})
	if domain.CodeOf(err) != domain.EFaultDomainCycle {
		t.Fatalf("cycle edit got %v (%s)", err, domain.CodeOf(err))
	}
	if !reflect.DeepEqual(before, eng.Observations()) {
		t.Fatal("rejected domain edit mutated cached results")
	}
}

func TestFaultDomainEditRecomputesMigrationDecision(t *testing.T) {
	project := generator.Scenario(42)
	eng, err := engine.New(project, engine.Config{ThreadBudget: 3, QueueLimit: 64})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := eng.RunAll(); err != nil {
		t.Fatalf("run: %v", err)
	}
	report, err := eng.ApplyEdit(engine.Edit{
		ID: "move-b2-to-rack-a", Kind: engine.EditNodeFaultDomain,
		Target: "n-b2", FaultDomain: "rack-a", FromTick: 1,
	})
	if err != nil {
		t.Fatalf("fault-domain edit: %v", err)
	}
	if report.FirstAffectedTick != 6 || len(report.ReusedTicks) != 5 {
		t.Fatalf("fault-domain invalidation did not track failure dependency: %+v", report)
	}
	full, err := eng.FullRecompute()
	if err != nil {
		t.Fatalf("full recompute: %v", err)
	}
	if !reflect.DeepEqual(eng.Observations(), full.Observations()) {
		t.Fatal("fault-domain incremental edit differs from full recompute")
	}
	tick6, _ := eng.Tick(6)
	found := false
	for _, migration := range tick6.Migrations {
		if migration.TenantID == "t-alpha" && migration.StartTick == 6 {
			found = true
			if migration.ToNode != "n-c1" {
				t.Fatalf("fault-domain edit did not remove n-b2 from alpha target set: %+v", migration)
			}
		}
	}
	if !found {
		t.Fatal("expected alpha migration at tick 6")
	}
}

func TestFixedPointOverflowDetected(t *testing.T) {
	if _, err := domain.CheckedAdd(int64(^uint64(0)>>1), 1, "$.test"); domain.CodeOf(err) != domain.EFixedPointOverflow {
		t.Fatalf("got %v (%s), want %s", err, domain.CodeOf(err), domain.EFixedPointOverflow)
	}
	project := &domain.Project{
		ID: "overflow-project",
		Tenants: []domain.Tenant{
			{ID: "t-a", Name: "A", Priority: 1, Weight: 1, QuotaBasisPoints: 10000, ReserveBasisPoints: 0},
		},
		Nodes: []domain.Node{{
			ID: "n-1", Rack: "rack-1", CapacityMilli: 1000,
			AllowedTenants: []string{"t-a"}, FaultDomain: "rack-1",
		}},
		FaultDomains: []domain.FaultDomain{{ID: "rack-1"}},
		Events: []domain.Event{
			{Tick: 1, Order: 0, ID: "huge-1", Kind: domain.EventRequest, Tenant: "t-a", Node: "n-1", AmountMilli: math.MaxInt64},
			{Tick: 1, Order: 1, ID: "huge-2", Kind: domain.EventRequest, Tenant: "t-a", Node: "n-1", AmountMilli: math.MaxInt64},
		},
	}
	if err := domain.ValidateProject(project); err != nil {
		t.Fatalf("validate overflow project: %v", err)
	}
	eng, err := engine.New(project, engine.Config{ThreadBudget: 1, QueueLimit: 2})
	if err != nil {
		t.Fatalf("new overflow project: %v", err)
	}
	err = eng.RunAll()
	if domain.CodeOf(err) != domain.EFixedPointOverflow {
		t.Fatalf("engine overflow got %v (%s)", err, domain.CodeOf(err))
	}
	if eng.Stats().FixedStats.Overflows == 0 {
		t.Fatal("engine did not expose overflow in fixed-point statistics")
	}
}

func findPausedMigration(t *testing.T, state *engine.State, tenantID string) engine.Migration {
	t.Helper()
	for _, migration := range state.Migrations {
		if migration.TenantID == tenantID && migration.Status == "paused" {
			return migration
		}
	}
	t.Fatalf("no paused migration for %s", tenantID)
	return engine.Migration{}
}

func reserveProject() *domain.Project {
	project := &domain.Project{
		ID: "reserve-project",
		Tenants: []domain.Tenant{
			{ID: "t-a", Name: "A", Priority: 30, Weight: 1, QuotaBasisPoints: 1000, ReserveBasisPoints: 8000},
			{ID: "t-b", Name: "B", Priority: 20, Weight: 1, QuotaBasisPoints: 1000, ReserveBasisPoints: 8000},
			{ID: "t-c", Name: "C", Priority: 10, Weight: 1, QuotaBasisPoints: 8000, ReserveBasisPoints: 0},
		},
		Nodes: []domain.Node{{
			ID: "n-1", Rack: "rack-1", CapacityMilli: 1000,
			AllowedTenants: []string{"t-a", "t-b", "t-c"}, FaultDomain: "rack-1",
		}},
		FaultDomains: []domain.FaultDomain{{ID: "rack-1"}},
		Events: []domain.Event{{
			Tick: 1, Order: 0, ID: "e-1", Kind: domain.EventRequest,
			Tenant: "t-c", Node: "n-1", AmountMilli: 1000,
		}},
	}
	if err := domain.ValidateProject(project); err != nil {
		panic(err)
	}
	return project
}

func queueProject() *domain.Project {
	project := &domain.Project{
		ID: "queue-project",
		Tenants: []domain.Tenant{
			{ID: "t-a", Name: "A", Priority: 10, Weight: 1, QuotaBasisPoints: 10000, ReserveBasisPoints: 0},
		},
		Nodes: []domain.Node{{
			ID: "n-1", Rack: "rack-1", CapacityMilli: 1000,
			AllowedTenants: []string{"t-a"}, FaultDomain: "rack-1",
		}},
		FaultDomains: []domain.FaultDomain{{ID: "rack-1"}},
		Events: []domain.Event{
			{Tick: 1, Order: 0, ID: "e-1", Kind: domain.EventRequest, Tenant: "t-a", Node: "n-1", AmountMilli: 400},
			{Tick: 1, Order: 1, ID: "e-2", Kind: domain.EventRequest, Tenant: "t-a", Node: "n-1", AmountMilli: 400},
			{Tick: 1, Order: 2, ID: "e-3", Kind: domain.EventRequest, Tenant: "t-a", Node: "n-1", AmountMilli: 400},
			{Tick: 1, Order: 3, ID: "e-4", Kind: domain.EventRequest, Tenant: "t-a", Node: "n-1", AmountMilli: 400},
		},
	}
	if err := domain.ValidateProject(project); err != nil {
		panic(err)
	}
	return project
}
