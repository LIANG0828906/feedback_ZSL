package engine

import (
	"sort"

	"quotaflow/internal/domain"
)

type Pending struct {
	ID        string   `json:"id"`
	TenantID  string   `json:"tenant_id"`
	NodeID    string   `json:"node_id"`
	Amount    int64    `json:"amount"`
	Reason    string   `json:"reason"`
	BlockedBy []string `json:"blocked_by,omitempty"`
}

type Pause struct {
	TenantID        string `json:"tenant_id"`
	FromNode        string `json:"from_node"`
	ToNode          string `json:"to_node"`
	StartTick       int64  `json:"start_tick"`
	ExpectedEndTick int64  `json:"expected_end_tick"`
	ReserveMilli    int64  `json:"reserve_milli"`
	EnableBasis     string `json:"enable_basis"`
}

type Migration struct {
	ID          string   `json:"id"`
	TenantID    string   `json:"tenant_id"`
	FromNode    string   `json:"from_node"`
	ToNode      string   `json:"to_node"`
	Amount      int64    `json:"amount"`
	StartTick   int64    `json:"start_tick"`
	EndTick     int64    `json:"end_tick"`
	Status      string   `json:"status"`
	Constraints []string `json:"constraints"`
	EnableBasis string   `json:"enable_basis"`
}

type TenantState struct {
	TenantID   string    `json:"tenant_id"`
	Weight     int64     `json:"weight"`
	Quota      int64     `json:"quota"`
	Used       int64     `json:"used"`
	Reserve    int64     `json:"reserve"`
	Encroached int64     `json:"encroached"`
	Pending    []Pending `json:"pending,omitempty"`
}

type NodeState struct {
	NodeID      string `json:"node_id"`
	Capacity    int64  `json:"capacity"`
	Used        int64  `json:"used"`
	Failed      bool   `json:"failed"`
	FaultDomain string `json:"fault_domain"`
}

// State is the complete checkpointable engine state after a tick.
type State struct {
	Tick          int64                         `json:"tick"`
	Tenants       map[string]TenantState        `json:"tenants"`
	Nodes         map[string]NodeState          `json:"nodes"`
	Placements    map[string]map[string]int64   `json:"placements"`
	Domains       map[string]domain.FaultDomain `json:"domains"`
	Migrations    map[string]Migration          `json:"migrations"`
	Pauses        map[string]Pause              `json:"pauses"`
	NextMigration int                           `json:"next_migration"`
	AppliedEdits  map[string]bool               `json:"applied_edits,omitempty"`
}

func InitialState(project *domain.Project) *State {
	state := &State{
		Tick:         0,
		Tenants:      make(map[string]TenantState, len(project.Tenants)),
		Nodes:        make(map[string]NodeState, len(project.Nodes)),
		Placements:   make(map[string]map[string]int64, len(project.Tenants)),
		Domains:      make(map[string]domain.FaultDomain, len(project.FaultDomains)),
		Migrations:   map[string]Migration{},
		Pauses:       map[string]Pause{},
		AppliedEdits: map[string]bool{},
	}
	for _, tenant := range project.Tenants {
		quota, _ := domain.TenantQuota(project.TotalCapacityMilli, tenant, 0, "$.tenants."+tenant.ID)
		reserve, _ := domain.TenantReserve(project.TotalCapacityMilli, tenant, nil, "$.tenants."+tenant.ID)
		state.Tenants[tenant.ID] = TenantState{
			TenantID: tenant.ID, Weight: tenant.Weight, Quota: quota, Reserve: reserve,
		}
		state.Placements[tenant.ID] = map[string]int64{}
	}
	for _, node := range project.Nodes {
		state.Nodes[node.ID] = NodeState{
			NodeID: node.ID, Capacity: node.CapacityMilli, FaultDomain: node.FaultDomain,
		}
	}
	for _, fd := range project.FaultDomains {
		state.Domains[fd.ID] = fd
	}
	return state
}

func (s *State) Clone() *State {
	if s == nil {
		return nil
	}
	out := &State{
		Tick:          s.Tick,
		Tenants:       make(map[string]TenantState, len(s.Tenants)),
		Nodes:         make(map[string]NodeState, len(s.Nodes)),
		Placements:    make(map[string]map[string]int64, len(s.Placements)),
		Domains:       make(map[string]domain.FaultDomain, len(s.Domains)),
		Migrations:    make(map[string]Migration, len(s.Migrations)),
		Pauses:        make(map[string]Pause, len(s.Pauses)),
		NextMigration: s.NextMigration,
		AppliedEdits:  make(map[string]bool, len(s.AppliedEdits)),
	}
	for id, tenant := range s.Tenants {
		tenant.Pending = clonePending(tenant.Pending)
		out.Tenants[id] = tenant
	}
	for id, node := range s.Nodes {
		out.Nodes[id] = node
	}
	for tenantID, placements := range s.Placements {
		cp := make(map[string]int64, len(placements))
		for nodeID, amount := range placements {
			cp[nodeID] = amount
		}
		out.Placements[tenantID] = cp
	}
	for id, fd := range s.Domains {
		out.Domains[id] = fd
	}
	for id, migration := range s.Migrations {
		migration.Constraints = append([]string(nil), migration.Constraints...)
		out.Migrations[id] = migration
	}
	for id, pause := range s.Pauses {
		out.Pauses[id] = pause
	}
	for id, applied := range s.AppliedEdits {
		out.AppliedEdits[id] = applied
	}
	return out
}

func clonePending(values []Pending) []Pending {
	out := make([]Pending, len(values))
	for i, value := range values {
		out[i] = value
		out[i].BlockedBy = append([]string(nil), value.BlockedBy...)
	}
	return out
}

func (s *State) actual(tenantID, nodeID string) int64 {
	return s.Placements[tenantID][nodeID]
}

func (s *State) pausedIncoming(tenantID, nodeID string) int64 {
	sum := int64(0)
	for _, migration := range s.Migrations {
		if migration.Status == "paused" && migration.TenantID == tenantID && migration.ToNode == nodeID {
			sum += migration.Amount
		}
	}
	return sum
}

func (s *State) virtual(tenantID, nodeID string) int64 {
	return s.actual(tenantID, nodeID) + s.pausedIncoming(tenantID, nodeID)
}

func (s *State) nodePausedIncoming(nodeID string) int64 {
	sum := int64(0)
	for _, migration := range s.Migrations {
		if migration.Status == "paused" && migration.ToNode == nodeID {
			sum += migration.Amount
		}
	}
	return sum
}

func (s *State) migrationsSorted() []Migration {
	out := make([]Migration, 0, len(s.Migrations))
	for _, migration := range s.Migrations {
		cp := migration
		cp.Constraints = append([]string(nil), migration.Constraints...)
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartTick != out[j].StartTick {
			return out[i].StartTick < out[j].StartTick
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *State) DomainInSubtree(ancestor, candidate string) bool {
	current := candidate
	seen := map[string]bool{}
	for current != "" {
		if current == ancestor {
			return true
		}
		if seen[current] {
			return false
		}
		seen[current] = true
		fd, ok := s.Domains[current]
		if !ok {
			return false
		}
		current = fd.Parent
	}
	return false
}
