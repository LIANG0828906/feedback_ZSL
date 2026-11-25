package generator

import (
	"fmt"

	"quotaflow/internal/domain"
)

func Scenario(seed int64) *domain.Project {
	priorityShift := int(seed % 7)
	project := &domain.Project{
		ID: fmt.Sprintf("quota-project-%d", seed),
		Tenants: []domain.Tenant{
			{ID: "t-alpha", Name: fmt.Sprintf("Alpha-%d", seed), Priority: 30 + priorityShift, Weight: 1, QuotaBasisPoints: 1800, ReserveBasisPoints: 2500},
			{ID: "t-beta", Name: fmt.Sprintf("Beta-%d", seed), Priority: 21 + priorityShift, Weight: 2, QuotaBasisPoints: 1400, ReserveBasisPoints: 2000},
			{ID: "t-gamma", Name: fmt.Sprintf("Gamma-%d", seed), Priority: 12 + priorityShift, Weight: 1, QuotaBasisPoints: 1100, ReserveBasisPoints: 1500},
		},
		Nodes: []domain.Node{
			{ID: "n-a1", Rack: "rack-a", CapacityMilli: 12500, AllowedTenants: []string{"t-alpha", "t-beta"}, FaultDomain: "rack-a"},
			{ID: "n-a2", Rack: "rack-a", CapacityMilli: 10500, AllowedTenants: []string{"t-beta", "t-gamma"}, FaultDomain: "rack-a"},
			{ID: "n-b1", Rack: "rack-b", CapacityMilli: 14500, AllowedTenants: []string{"t-alpha", "t-beta", "t-gamma"}, FaultDomain: "rack-b"},
			{ID: "n-b2", Rack: "rack-b", CapacityMilli: 9500, AllowedTenants: []string{"t-alpha", "t-gamma"}, FaultDomain: "rack-b"},
			{ID: "n-c1", Rack: "rack-c", CapacityMilli: 24000, AllowedTenants: []string{"t-alpha", "t-beta", "t-gamma"}, FaultDomain: "rack-c"},
		},
		FaultDomains: []domain.FaultDomain{
			{ID: "rack-a"},
			{ID: "rack-b"},
			{ID: "rack-c"},
		},
	}
	type eventSpec struct {
		tick   int64
		id     string
		kind   domain.EventKind
		tenant string
		node   string
		amount int64
		weight int64
	}
	specs := []eventSpec{
		{1, "e-001", domain.EventRequest, "t-alpha", "n-a1", 3500, 0},
		{1, "e-002", domain.EventRequest, "t-beta", "n-a1", 4000, 0},
		{2, "e-003", domain.EventRequest, "t-alpha", "n-b1", 4000, 0},
		{2, "e-004", domain.EventRequest, "t-beta", "n-a2", 3000, 0},
		{2, "e-005", domain.EventRequest, "t-gamma", "n-b1", 2500, 0},
		{3, "e-006", domain.EventWeight, "t-alpha", "", 0, 2},
		{3, "e-007", domain.EventRequest, "t-alpha", "n-b1", 3000, 0},
		{4, "e-008", domain.EventRequest, "t-gamma", "n-b1", 4000, 0},
		{4, "e-009", domain.EventRequest, "t-beta", "n-a1", 5000, 0},
		{5, "e-010", domain.EventRelease, "t-gamma", "n-b1", 1000, 0},
		{5, "e-011", domain.EventRequest, "t-beta", "n-b1", 6000, 0},
		{6, "e-012", domain.EventNodeFailure, "", "n-a1", 0, 0},
		{6, "e-019", domain.EventRequest, "t-alpha", "n-c1", 500, 0},
		{6, "e-020", domain.EventRequest, "t-beta", "n-b1", 500, 0},
		{7, "e-013", domain.EventNodeRecovery, "", "n-a1", 0, 0},
		{8, "e-014", domain.EventNodeFailure, "", "n-b1", 0, 0},
		{9, "e-015", domain.EventNodeRecovery, "", "n-b1", 0, 0},
		{10, "e-016", domain.EventRelease, "t-beta", "n-c1", 2000, 0},
		{10, "e-017", domain.EventRelease, "t-gamma", "n-a2", 1000, 0},
		{11, "e-018", domain.EventNodeRecovery, "", "n-c1", 0, 0},
	}
	orderByTick := map[int64]int{}
	for _, spec := range specs {
		event := domain.Event{
			Tick: spec.tick, Order: orderByTick[spec.tick], ID: spec.id, Kind: spec.kind,
			Tenant: spec.tenant, Node: spec.node, AmountMilli: spec.amount, Weight: spec.weight,
		}
		orderByTick[spec.tick]++
		project.Events = append(project.Events, event)
	}
	if err := domain.ValidateProject(project); err != nil {
		panic(err)
	}
	return project
}
