package gen

import (
	"encoding/json"
	"fmt"
	"sort"

	"quotaengine/quota"
)

type Documents struct {
	V1 []byte
	V2 []byte
	V3 []byte
}

type tenantV1 struct {
	ID                string `json:"tenant_id"`
	QuotaPermille     int64  `json:"quota_permille"`
	GuaranteePermille int64  `json:"guarantee_permille"`
	BurstPermille     *int64 `json:"burst_permille,omitempty"`
	Priority          int    `json:"priority"`
}

type nodeV1 struct {
	ID            string   `json:"node_id"`
	CapacityCores int64    `json:"capacity_cores"`
	Allowed       []string `json:"allowed_tenants"`
	Rack          string   `json:"rack_id"`
}

type eventV1 struct {
	Seq            int             `json:"seq"`
	AtSeconds      int64           `json:"at_seconds"`
	Type           quota.EventType `json:"type"`
	TenantID       string          `json:"tenant_id"`
	NodeID         string          `json:"node_id,omitempty"`
	AmountCores    int64           `json:"amount_cores,omitempty"`
	WeightPermille int64           `json:"weight_permille,omitempty"`
}

type projectV1 struct {
	SchemaVersion string     `json:"schema_version"`
	Seed          int64      `json:"seed"`
	Tenants       []tenantV1 `json:"tenants"`
	Nodes         []nodeV1   `json:"nodes"`
	Events        []eventV1  `json:"events"`
}

type tenantV2 struct {
	ID               string `json:"tenant_id"`
	QuotaBasisPoints int64  `json:"quota_basis_points"`
	GuaranteeBP      int64  `json:"guarantee_basis_points"`
	BurstBP          *int64 `json:"burst_basis_points,omitempty"`
	Priority         int    `json:"priority"`
}

type nodeV2 struct {
	ID            string   `json:"node_id"`
	CapacityMilli int64    `json:"capacity_millicores"`
	Allowed       []string `json:"allowed_tenants"`
	Rack          string   `json:"rack_id"`
}

type eventV2 struct {
	Seq            int             `json:"seq"`
	AtMillis       int64           `json:"at_millis"`
	Type           quota.EventType `json:"type"`
	TenantID       string          `json:"tenant_id"`
	NodeID         string          `json:"node_id,omitempty"`
	AmountMilli    int64           `json:"amount_millicores,omitempty"`
	WeightPermille int64           `json:"weight_permille,omitempty"`
}

type projectV2 struct {
	SchemaVersion string     `json:"schema_version"`
	Seed          int64      `json:"seed"`
	Tenants       []tenantV2 `json:"tenants"`
	Nodes         []nodeV2   `json:"nodes"`
	Events        []eventV2  `json:"events"`
}

type tenantV3 struct {
	ID          string `json:"id"`
	QuotaBP     int64  `json:"quota_bp"`
	GuaranteeBP int64  `json:"guarantee_bp"`
	BurstBP     *int64 `json:"burst_bp,omitempty"`
	Priority    int    `json:"priority"`
}

type nodeV3 struct {
	ID         string   `json:"id"`
	CapacityMC int64    `json:"capacity_mc"`
	Tenants    []string `json:"tenants"`
}

type rackV3 struct {
	ID    string   `json:"id"`
	Nodes []nodeV3 `json:"nodes"`
}

type eventV3 struct {
	Seq      int             `json:"seq"`
	TimeMS   int64           `json:"time_ms"`
	Type     quota.EventType `json:"type"`
	TenantID string          `json:"tenant_id"`
	NodeID   string          `json:"node_id,omitempty"`
	DeltaMC  int64           `json:"delta_mc,omitempty"`
	Weight   int64           `json:"weight"`
}

type projectV3 struct {
	SchemaVersion string     `json:"schema_version"`
	Seed          int64      `json:"seed"`
	Tenants       []tenantV3 `json:"tenants"`
	Racks         []rackV3   `json:"racks"`
	Events        []eventV3  `json:"events"`
}

func renderDocuments(p *quota.Project) (Documents, error) {
	for _, t := range p.Tenants {
		if t.QuotaBP%10 != 0 || t.GuaranteeBP%10 != 0 || t.BurstBP%10 != 0 {
			return Documents{}, fmt.Errorf("schema v1 permille encoding is not exact for tenant %s", t.ID)
		}
	}
	v1 := projectV1{SchemaVersion: "v1", Seed: p.Seed}
	v2 := projectV2{SchemaVersion: "v2", Seed: p.Seed}
	v3 := projectV3{SchemaVersion: "v3", Seed: p.Seed}
	for _, t := range p.Tenants {
		var b1, b2, b3 *int64
		if t.BurstBP > 0 {
			v := t.BurstBP / 10
			b1 = &v
			v2v := t.BurstBP
			b2 = &v2v
			v3v := t.BurstBP
			b3 = &v3v
		}
		v1.Tenants = append(v1.Tenants, tenantV1{
			ID: t.ID, QuotaPermille: t.QuotaBP / 10, GuaranteePermille: t.GuaranteeBP / 10,
			BurstPermille: b1, Priority: t.Priority,
		})
		v2.Tenants = append(v2.Tenants, tenantV2{
			ID: t.ID, QuotaBasisPoints: t.QuotaBP, GuaranteeBP: t.GuaranteeBP,
			BurstBP: b2, Priority: t.Priority,
		})
		v3.Tenants = append(v3.Tenants, tenantV3{
			ID: t.ID, QuotaBP: t.QuotaBP, GuaranteeBP: t.GuaranteeBP,
			BurstBP: b3, Priority: t.Priority,
		})
	}
	for _, n := range p.Nodes {
		v1.Nodes = append(v1.Nodes, nodeV1{
			ID: n.ID, CapacityCores: n.CapacityMilli / quota.MilliPerCore,
			Allowed: append([]string(nil), n.AllowedTenants...), Rack: n.RackID,
		})
		v2.Nodes = append(v2.Nodes, nodeV2{
			ID: n.ID, CapacityMilli: n.CapacityMilli,
			Allowed: append([]string(nil), n.AllowedTenants...), Rack: n.RackID,
		})
	}
	byRack := make(map[string][]nodeV3)
	for _, n := range p.Nodes {
		byRack[n.RackID] = append(byRack[n.RackID], nodeV3{ID: n.ID, CapacityMC: n.CapacityMilli, Tenants: append([]string(nil), n.AllowedTenants...)})
	}
	rackIDs := make([]string, 0, len(byRack))
	for id := range byRack {
		rackIDs = append(rackIDs, id)
	}
	sort.Strings(rackIDs)
	for _, id := range rackIDs {
		nodes := byRack[id]
		sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
		v3.Racks = append(v3.Racks, rackV3{ID: id, Nodes: nodes})
	}
	for _, e := range p.Events {
		amountCores := e.AmountMilli / quota.MilliPerCore
		v1.Events = append(v1.Events, eventV1{
			Seq: e.Seq, AtSeconds: e.TimeMilli / quota.MillisPerSecond, Type: e.Type,
			TenantID: e.TenantID, NodeID: e.NodeID, AmountCores: amountCores, WeightPermille: e.WeightPermille,
		})
		v2.Events = append(v2.Events, eventV2{
			Seq: e.Seq, AtMillis: e.TimeMilli, Type: e.Type, TenantID: e.TenantID,
			NodeID: e.NodeID, AmountMilli: e.AmountMilli, WeightPermille: e.WeightPermille,
		})
		v3.Events = append(v3.Events, eventV3{
			Seq: e.Seq, TimeMS: e.TimeMilli, Type: e.Type, TenantID: e.TenantID,
			NodeID: e.NodeID, DeltaMC: e.AmountMilli, Weight: e.WeightPermille,
		})
	}
	rawV1, err := json.MarshalIndent(v1, "", "  ")
	if err != nil {
		return Documents{}, err
	}
	rawV2, err := json.MarshalIndent(v2, "", "  ")
	if err != nil {
		return Documents{}, err
	}
	rawV3, err := json.MarshalIndent(v3, "", "  ")
	if err != nil {
		return Documents{}, err
	}
	return Documents{V1: rawV1, V2: rawV2, V3: rawV3}, nil
}
