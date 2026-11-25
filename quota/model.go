package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	RatioScalePermille    int64 = 1000
	RatioScaleBasisPoints int64 = 10000
	MilliPerCore          int64 = 1000
	MillisPerSecond       int64 = 1000

	DefaultToleranceMilli int64 = 0
	MaxWorkers                  = 64
	MaxQueuePerTenant           = 4096
	MaxWeightPermille     int64 = 4000
	CheckpointVersion           = "quota-checkpoint-v1"
)

const (
	ConstraintTenantHardLimit  = "C1_TENANT_HARD_LIMIT"
	ConstraintNodeCapacity     = "C2_NODE_CAPACITY"
	ConstraintGuaranteeReserve = "C3_GUARANTEE_RESERVE"
)

type EventType string

const (
	EventRequest EventType = "request"
	EventRelease EventType = "release"
	EventWeight  EventType = "weight"
)

type Tenant struct {
	ID          string `json:"id"`
	QuotaBP     int64  `json:"quota_basis_points"`
	GuaranteeBP int64  `json:"guarantee_basis_points"`
	BurstBP     int64  `json:"burst_basis_points"`
	Priority    int    `json:"priority"`
}

type Node struct {
	ID             string   `json:"id"`
	CapacityMilli  int64    `json:"capacity_millicores"`
	RackID         string   `json:"rack_id"`
	AllowedTenants []string `json:"allowed_tenants"`
}

type Event struct {
	Seq            int       `json:"seq"`
	TimeMilli      int64     `json:"time_millis"`
	Type           EventType `json:"type"`
	TenantID       string    `json:"tenant_id"`
	NodeID         string    `json:"node_id,omitempty"`
	AmountMilli    int64     `json:"amount_millicores,omitempty"`
	WeightPermille int64     `json:"weight_permille,omitempty"`
}

// Project is the only model consumed by the ledger and persistence layers.
// Schema names, units and topology nesting are erased by the loader.
type Project struct {
	SchemaVersion string   `json:"schema_version"`
	Seed          int64    `json:"seed"`
	Tenants       []Tenant `json:"tenants"`
	Nodes         []Node   `json:"nodes"`
	Events        []Event  `json:"events"`
}

type Rack struct {
	ID    string   `json:"id"`
	Nodes []string `json:"nodes"`
}

func (p *Project) Topology() []Rack {
	byRack := make(map[string][]string)
	for _, n := range p.Nodes {
		byRack[n.RackID] = append(byRack[n.RackID], n.ID)
	}
	racks := make([]Rack, 0, len(byRack))
	for id, nodes := range byRack {
		sort.Strings(nodes)
		racks = append(racks, Rack{ID: id, Nodes: nodes})
	}
	sort.Slice(racks, func(i, j int) bool { return racks[i].ID < racks[j].ID })
	return racks
}

func (p *Project) Fingerprint() (string, error) {
	normalized, err := p.NormalizedClone()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// NormalizedClone sorts declarations and event identities while retaining the
// event sequence. It is used for cross-schema comparison and checkpoint IDs.
func (p *Project) NormalizedClone() (*Project, error) {
	if p == nil {
		return nil, NewError(CodeSchemaUnknown, "normalize", "", "nil project")
	}
	cp := *p
	cp.Tenants = append([]Tenant(nil), p.Tenants...)
	cp.Nodes = append([]Node(nil), p.Nodes...)
	cp.Events = append([]Event(nil), p.Events...)
	for i := range cp.Nodes {
		cp.Nodes[i].AllowedTenants = append([]string(nil), p.Nodes[i].AllowedTenants...)
		sort.Strings(cp.Nodes[i].AllowedTenants)
	}
	sort.Slice(cp.Tenants, func(i, j int) bool { return cp.Tenants[i].ID < cp.Tenants[j].ID })
	sort.Slice(cp.Nodes, func(i, j int) bool { return cp.Nodes[i].ID < cp.Nodes[j].ID })
	sort.Slice(cp.Events, func(i, j int) bool {
		if cp.Events[i].TimeMilli != cp.Events[j].TimeMilli {
			return cp.Events[i].TimeMilli < cp.Events[j].TimeMilli
		}
		return cp.Events[i].Seq < cp.Events[j].Seq
	})
	return &cp, nil
}

// Equivalent compares semantic content, ignoring the encoding version.
func Equivalent(a, b *Project) (bool, error) {
	ac, err := a.NormalizedClone()
	if err != nil {
		return false, err
	}
	bc, err := b.NormalizedClone()
	if err != nil {
		return false, err
	}
	ac.SchemaVersion, bc.SchemaVersion = "", ""
	aj, err := json.Marshal(ac)
	if err != nil {
		return false, err
	}
	bj, err := json.Marshal(bc)
	if err != nil {
		return false, err
	}
	return string(aj) == string(bj), nil
}

func (p *Project) Validate() error {
	if p == nil {
		return NewError(CodeSchemaUnknown, "validate", "", "nil project")
	}
	tenantByID := make(map[string]Tenant, len(p.Tenants))
	for i, t := range p.Tenants {
		path := fmt.Sprintf("tenants[%d]", i)
		if strings.TrimSpace(t.ID) == "" {
			return NewError(CodeSchemaField, "validate", path, "empty tenant id")
		}
		if _, ok := tenantByID[t.ID]; ok {
			return NewError(CodeSchemaField, "validate", path, "duplicate tenant id "+t.ID)
		}
		if t.QuotaBP < 0 || t.GuaranteeBP < 0 || t.BurstBP < 0 {
			e := NewError(CodeNegativeQuota, "validate", path, "quota, guarantee and burst must be non-negative")
			e.TenantID = t.ID
			return e
		}
		if t.QuotaBP > RatioScaleBasisPoints || t.GuaranteeBP > RatioScaleBasisPoints || t.BurstBP > RatioScaleBasisPoints {
			e := NewError(CodeQuotaRange, "validate", path, "ratio exceeds 10000 basis points")
			e.TenantID = t.ID
			return e
		}
		if t.GuaranteeBP > t.QuotaBP {
			e := NewError(CodeQuotaRange, "validate", path, "guarantee exceeds quota")
			e.TenantID = t.ID
			return e
		}
		if t.Priority < 0 {
			e := NewError(CodeSchemaField, "validate", path, "negative priority")
			e.TenantID = t.ID
			return e
		}
		tenantByID[t.ID] = t
	}

	nodeByID := make(map[string]Node, len(p.Nodes))
	for i, n := range p.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		if strings.TrimSpace(n.ID) == "" {
			return NewError(CodeSchemaField, "validate", path, "empty node id")
		}
		if _, ok := nodeByID[n.ID]; ok {
			return NewError(CodeSchemaField, "validate", path, "duplicate node id "+n.ID)
		}
		if strings.TrimSpace(n.RackID) == "" {
			return NewError(CodeSchemaField, "validate", path, "empty rack id")
		}
		if n.CapacityMilli <= 0 {
			return NewError(CodeSchemaField, "validate", path, "node capacity must be positive")
		}
		seen := make(map[string]bool)
		var guaranteeBP int64
		for j, id := range n.AllowedTenants {
			if seen[id] {
				return NewError(CodeSchemaField, "validate", fmt.Sprintf("%s.allowed_tenants[%d]", path, j), "duplicate allowed tenant")
			}
			seen[id] = true
			t, ok := tenantByID[id]
			if !ok {
				e := NewError(CodeSchemaField, "validate", fmt.Sprintf("%s.allowed_tenants[%d]", path, j), "unknown tenant")
				e.NodeID = n.ID
				return e
			}
			if guaranteeBP > RatioScaleBasisPoints-t.GuaranteeBP {
				e := NewError(CodeGuaranteeOverCapacity, "validate", path, "guaranteed shares exceed node capacity")
				e.NodeID = n.ID
				return e
			}
			guaranteeBP += t.GuaranteeBP
		}
		nodeByID[n.ID] = n
	}

	eventSeen := make(map[int]bool, len(p.Events))
	for i, ev := range p.Events {
		path := fmt.Sprintf("events[%d]", i)
		e := &BusinessError{Code: CodeInvalidEvent, Op: "validate", Path: path, TimeMilli: ev.TimeMilli, TenantID: ev.TenantID, NodeID: ev.NodeID}
		if ev.Seq < 0 {
			e.Detail = "negative sequence"
			return e
		}
		if eventSeen[ev.Seq] {
			e.Detail = "duplicate event sequence"
			return e
		}
		eventSeen[ev.Seq] = true
		if ev.TimeMilli < 0 {
			e.Detail = "negative logical time"
			return e
		}
		if _, ok := tenantByID[ev.TenantID]; !ok {
			e.Detail = "unknown tenant"
			return e
		}
		switch ev.Type {
		case EventRequest, EventRelease:
			if ev.AmountMilli <= 0 {
				e.Detail = "request/release amount must be positive"
				return e
			}
			n, ok := nodeByID[ev.NodeID]
			if !ok {
				e.Detail = "unknown node"
				return e
			}
			if !contains(n.AllowedTenants, ev.TenantID) {
				e.Detail = "tenant is not allowed on node"
				return e
			}
		case EventWeight:
			if ev.WeightPermille <= 0 || ev.WeightPermille > MaxWeightPermille {
				e.Detail = fmt.Sprintf("weight must be in (0,%d] permille", MaxWeightPermille)
				return e
			}
		default:
			e.Detail = "unknown event type " + string(ev.Type)
			return e
		}
	}
	return nil
}

func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func (p *Project) Tenant(id string) (Tenant, bool) {
	for _, t := range p.Tenants {
		if t.ID == id {
			return t, true
		}
	}
	return Tenant{}, false
}

func (p *Project) Node(id string) (Node, bool) {
	for _, n := range p.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}
