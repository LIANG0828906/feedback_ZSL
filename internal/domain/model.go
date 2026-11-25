package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"
)

type EventKind string

const (
	EventRequest      EventKind = "request"
	EventRelease      EventKind = "release"
	EventWeight       EventKind = "weight"
	EventNodeFailure  EventKind = "node_failure"
	EventNodeRecovery EventKind = "node_recovery"
)

type Tenant struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Priority           int    `json:"priority"`
	Weight             int64  `json:"weight"`
	QuotaBasisPoints   int64  `json:"quota_basis_points"`
	ReserveBasisPoints int64  `json:"reserve_basis_points"`
}

type Node struct {
	ID             string   `json:"id"`
	Rack           string   `json:"rack"`
	CapacityMilli  int64    `json:"capacity_milli"`
	AllowedTenants []string `json:"allowed_tenants"`
	FaultDomain    string   `json:"fault_domain"`
}

type FaultDomain struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
}

type Event struct {
	Tick        int64     `json:"tick"`
	Order       int       `json:"order"`
	ID          string    `json:"id"`
	Kind        EventKind `json:"kind"`
	Tenant      string    `json:"tenant,omitempty"`
	Node        string    `json:"node,omitempty"`
	AmountMilli int64     `json:"amount_milli,omitempty"`
	Weight      int64     `json:"weight,omitempty"`
}

type Project struct {
	ID                 string        `json:"id"`
	SourceSchema       string        `json:"-"`
	SourceName         string        `json:"-"`
	Tenants            []Tenant      `json:"tenants"`
	Nodes              []Node        `json:"nodes"`
	FaultDomains       []FaultDomain `json:"fault_domains"`
	Events             []Event       `json:"events"`
	TotalCapacityMilli int64         `json:"total_capacity_milli"`
	MaxTick            int64         `json:"max_tick"`
}

func (p *Project) TenantByID(id string) (Tenant, bool) {
	for _, tenant := range p.Tenants {
		if tenant.ID == id {
			return tenant, true
		}
	}
	return Tenant{}, false
}

func (p *Project) NodeByID(id string) (Node, bool) {
	for _, node := range p.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

func (p *Project) DomainByID(id string) (FaultDomain, bool) {
	for _, fd := range p.FaultDomains {
		if fd.ID == id {
			return fd, true
		}
	}
	return FaultDomain{}, false
}

func (p *Project) EventsByTick() map[int64][]Event {
	out := make(map[int64][]Event)
	for _, event := range p.Events {
		out[event.Tick] = append(out[event.Tick], event)
	}
	for tick := range out {
		sort.SliceStable(out[tick], func(i, j int) bool {
			if out[tick][i].Order != out[tick][j].Order {
				return out[tick][i].Order < out[tick][j].Order
			}
			return out[tick][i].ID < out[tick][j].ID
		})
	}
	return out
}

func (p *Project) Clone() *Project {
	raw, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(raw, &out)
	out.SourceSchema = p.SourceSchema
	out.SourceName = p.SourceName
	return &out
}

// CanonicalFingerprint excludes representation-specific source metadata but
// includes every normalized model field that can affect behavior.
func (p *Project) CanonicalFingerprint() string {
	canonical := struct {
		ID                 string        `json:"id"`
		Tenants            []Tenant      `json:"tenants"`
		Nodes              []Node        `json:"nodes"`
		FaultDomains       []FaultDomain `json:"fault_domains"`
		Events             []Event       `json:"events"`
		TotalCapacityMilli int64         `json:"total_capacity_milli"`
		MaxTick            int64         `json:"max_tick"`
	}{
		ID:                 p.ID,
		Tenants:            append([]Tenant(nil), p.Tenants...),
		Nodes:              append([]Node(nil), p.Nodes...),
		FaultDomains:       append([]FaultDomain(nil), p.FaultDomains...),
		Events:             append([]Event(nil), p.Events...),
		TotalCapacityMilli: p.TotalCapacityMilli,
		MaxTick:            p.MaxTick,
	}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (p *Project) ValidateTopology() error {
	domains := make(map[string]FaultDomain, len(p.FaultDomains))
	for _, fd := range p.FaultDomains {
		if fd.ID == "" {
			return NewBizError(ESchemaRequired, "$.fault_domains", "empty fault domain id")
		}
		if _, exists := domains[fd.ID]; exists {
			return NewBizError(ESchemaInvalid, "$.fault_domains", "duplicate fault domain %q", fd.ID)
		}
		domains[fd.ID] = fd
	}
	for _, fd := range p.FaultDomains {
		if fd.Parent == "" {
			continue
		}
		if _, ok := domains[fd.Parent]; !ok {
			return NewBizError(EFaultDomainUnknown, "$.fault_domains."+fd.ID+".parent", "unknown parent %q", fd.Parent)
		}
	}
	for _, fd := range p.FaultDomains {
		seen := map[string]bool{}
		current := fd.ID
		for current != "" {
			if seen[current] {
				return NewBizError(EFaultDomainCycle, "$.fault_domains."+fd.ID, "fault domain parent cycle at %q", current)
			}
			seen[current] = true
			next, ok := domains[current]
			if !ok {
				return NewBizError(EFaultDomainUnknown, "$.fault_domains."+fd.ID, "unknown domain %q in parent chain", current)
			}
			current = next.Parent
		}
	}
	for _, node := range p.Nodes {
		if node.FaultDomain == "" {
			return NewBizError(ESchemaRequired, "$.nodes."+node.ID+".fault_domain", "node has no fault domain")
		}
		if _, ok := domains[node.FaultDomain]; !ok {
			return NewBizError(EFaultDomainUnknown, "$.nodes."+node.ID+".fault_domain", "unknown fault domain %q", node.FaultDomain)
		}
	}
	return nil
}

func (p *Project) DomainInSubtree(ancestor, candidate string) bool {
	domains := make(map[string]string, len(p.FaultDomains))
	for _, fd := range p.FaultDomains {
		domains[fd.ID] = fd.Parent
	}
	current := candidate
	for current != "" {
		if current == ancestor {
			return true
		}
		current = domains[current]
	}
	return false
}

func (p *Project) ValidateReserveCapacity() error {
	for _, node := range p.Nodes {
		sum := int64(0)
		for _, tenant := range p.Tenants {
			if !contains(node.AllowedTenants, tenant.ID) {
				continue
			}
			reserve, err := TenantReserve(p.TotalCapacityMilli, tenant, nil, "$.tenants."+tenant.ID)
			if err != nil {
				return err
			}
			sum, err = CheckedAdd(sum, reserve, "$.nodes."+node.ID)
			if err != nil {
				return err
			}
		}
		if sum > node.CapacityMilli {
			return NewBizError(EReserveExceedsNode, "$.nodes."+node.ID,
				"tenant reserves %d exceed node capacity %d", sum, node.CapacityMilli)
		}
	}
	return nil
}

func TenantQuota(totalCapacity int64, tenant Tenant, weightOverride int64, path string) (int64, error) {
	weight := tenant.Weight
	if weightOverride > 0 {
		weight = weightOverride
	}
	base, err := MulDivFloor(totalCapacity, tenant.QuotaBasisPoints, 10000, path+".quota_basis_points")
	if err != nil {
		return 0, err
	}
	return CheckedMul(base, weight, path+".weight")
}

// TenantReserve is deliberately based on the base quota, not the mutable
// weight. A weight edit changes burst quota but cannot silently shrink the
// protected reservation out from under already admitted workloads.
func TenantReserve(totalCapacity int64, tenant Tenant, stats *FixedStats, path string) (int64, error) {
	base, err := MulDivFloor(totalCapacity, tenant.QuotaBasisPoints, 10000, path+".quota_basis_points")
	if err != nil {
		return 0, err
	}
	reserve, err := MulDivFloor(base, tenant.ReserveBasisPoints, 10000, path+".reserve_basis_points")
	if err != nil {
		return 0, err
	}
	return ClampNonNegative(reserve, stats), nil
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func SortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func ValidateProject(p *Project) error {
	if p.ID == "" {
		return NewBizError(ESchemaRequired, "$.project_id", "empty project id")
	}
	if len(p.Tenants) == 0 {
		return NewBizError(ESchemaRequired, "$.tenants", "project has no tenants")
	}
	if len(p.Nodes) == 0 {
		return NewBizError(ESchemaRequired, "$.nodes", "project has no nodes")
	}
	tenantIDs := map[string]bool{}
	quotaSum := int64(0)
	for i := range p.Tenants {
		tenant := &p.Tenants[i]
		if tenant.ID == "" {
			return NewBizError(ESchemaRequired, fmt.Sprintf("$.tenants[%d].id", i), "empty tenant id")
		}
		if tenantIDs[tenant.ID] {
			return NewBizError(ESchemaInvalid, fmt.Sprintf("$.tenants[%d].id", i), "duplicate tenant %q", tenant.ID)
		}
		tenantIDs[tenant.ID] = true
		if tenant.QuotaBasisPoints < 0 {
			return NewBizError(ENegativeQuota, fmt.Sprintf("$.tenants[%d].quota_basis_points", i), "negative quota")
		}
		if tenant.QuotaBasisPoints > 10000 {
			return NewBizError(ESchemaInvalid, fmt.Sprintf("$.tenants[%d].quota_basis_points", i), "quota above 10000 bp")
		}
		if tenant.ReserveBasisPoints < 0 {
			return NewBizError(ENegativeQuota, fmt.Sprintf("$.tenants[%d].reserve_basis_points", i), "negative reserve")
		}
		if tenant.ReserveBasisPoints > 10000 {
			return NewBizError(EReserveExceedsQuota, fmt.Sprintf("$.tenants[%d].reserve_basis_points", i),
				"reserve %d bp exceeds 10000 bp of the tenant quota", tenant.ReserveBasisPoints)
		}
		if tenant.Weight <= 0 || tenant.Weight > 1000 {
			return NewBizError(EInvalidWeight, fmt.Sprintf("$.tenants[%d].weight", i), "weight %d outside 1..1000", tenant.Weight)
		}
		var err error
		quotaSum, err = CheckedAdd(quotaSum, tenant.QuotaBasisPoints, "$.tenants")
		if err != nil {
			return err
		}
	}
	if quotaSum > 10000 {
		return NewBizError(ESchemaInvalid, "$.tenants", "base quota sum %d bp exceeds 10000 bp", quotaSum)
	}
	nodeIDs := map[string]bool{}
	capacitySum := int64(0)
	for i := range p.Nodes {
		node := &p.Nodes[i]
		if node.ID == "" {
			return NewBizError(ESchemaRequired, fmt.Sprintf("$.nodes[%d].id", i), "empty node id")
		}
		if nodeIDs[node.ID] {
			return NewBizError(ESchemaInvalid, fmt.Sprintf("$.nodes[%d].id", i), "duplicate node %q", node.ID)
		}
		nodeIDs[node.ID] = true
		if node.CapacityMilli <= 0 {
			return NewBizError(EInvalidCapacity, fmt.Sprintf("$.nodes[%d].capacity_milli", i), "capacity must be positive")
		}
		node.AllowedTenants = SortedCopy(node.AllowedTenants)
		for _, tenantID := range node.AllowedTenants {
			if !tenantIDs[tenantID] {
				return NewBizError(ETenantUnknown, fmt.Sprintf("$.nodes[%d].allowed_tenants", i), "unknown tenant %q", tenantID)
			}
		}
		var err error
		capacitySum, err = CheckedAdd(capacitySum, node.CapacityMilli, "$.nodes")
		if err != nil {
			return err
		}
	}
	p.TotalCapacityMilli = capacitySum
	if err := p.ValidateTopology(); err != nil {
		return err
	}
	if err := p.ValidateReserveCapacity(); err != nil {
		return err
	}
	eventIDs := map[string]bool{}
	lastTick := int64(0)
	for i := range p.Events {
		event := &p.Events[i]
		if event.Tick < 1 {
			return NewBizError(EEventOrder, fmt.Sprintf("$.events[%d].tick", i), "tick must be >= 1")
		}
		if event.Tick < lastTick {
			return NewBizError(EEventOrder, fmt.Sprintf("$.events[%d].tick", i), "events are not time ordered")
		}
		lastTick = event.Tick
		if event.ID == "" {
			return NewBizError(ESchemaRequired, fmt.Sprintf("$.events[%d].id", i), "empty event id")
		}
		if eventIDs[event.ID] {
			return NewBizError(ESchemaInvalid, fmt.Sprintf("$.events[%d].id", i), "duplicate event %q", event.ID)
		}
		eventIDs[event.ID] = true
		switch event.Kind {
		case EventRequest, EventRelease:
			if !tenantIDs[event.Tenant] {
				return NewBizError(ETenantUnknown, fmt.Sprintf("$.events[%d].tenant", i), "unknown tenant %q", event.Tenant)
			}
			if !nodeIDs[event.Node] {
				return NewBizError(ENodeUnknown, fmt.Sprintf("$.events[%d].node", i), "unknown node %q", event.Node)
			}
			node, _ := p.NodeByID(event.Node)
			if !contains(node.AllowedTenants, event.Tenant) {
				return NewBizError(ENodeNotAllowed, fmt.Sprintf("$.events[%d].node", i),
					"tenant %q is not allowed on node %q", event.Tenant, event.Node)
			}
			if event.AmountMilli <= 0 {
				return NewBizError(EEventInvalid, fmt.Sprintf("$.events[%d].amount_milli", i), "request/release amount must be positive")
			}
		case EventWeight:
			if !tenantIDs[event.Tenant] {
				return NewBizError(ETenantUnknown, fmt.Sprintf("$.events[%d].tenant", i), "unknown tenant %q", event.Tenant)
			}
			if event.Weight <= 0 || event.Weight > 1000 {
				return NewBizError(EInvalidWeight, fmt.Sprintf("$.events[%d].weight", i), "weight %d outside 1..1000", event.Weight)
			}
		case EventNodeFailure, EventNodeRecovery:
			if !nodeIDs[event.Node] {
				return NewBizError(ENodeUnknown, fmt.Sprintf("$.events[%d].node", i), "unknown node %q", event.Node)
			}
		default:
			return NewBizError(EEventInvalid, fmt.Sprintf("$.events[%d].kind", i), "unknown event kind %q", event.Kind)
		}
	}
	if len(p.Events) > 0 {
		p.MaxTick = p.Events[len(p.Events)-1].Tick
	}
	return nil
}
