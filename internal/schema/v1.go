package schema

import (
	"encoding/json"
	"fmt"

	"quotaflow/internal/domain"
)

type v1Project struct {
	SchemaVersion string     `json:"schema_version"`
	ProjectID     string     `json:"project_id"`
	Tenants       []v1Tenant `json:"tenants"`
	Nodes         []v1Node   `json:"nodes"`
	Events        []v1Event  `json:"events"`
}

type v1Tenant struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Priority       int    `json:"priority"`
	Weight         int64  `json:"weight"`
	QuotaPercent   int64  `json:"quota_percent"`
	ReservePercent int64  `json:"reserve_percent"`
}

type v1Node struct {
	ID             string      `json:"id"`
	Rack           string      `json:"rack"`
	CoreCapacity   json.Number `json:"core_capacity"`
	AllowedTenants []string    `json:"allowed_tenants"`
}

type v1Event struct {
	Seq    int64       `json:"seq"`
	ID     string      `json:"id"`
	Kind   string      `json:"kind"`
	Tenant string      `json:"tenant"`
	Node   string      `json:"node"`
	Cores  json.Number `json:"cores"`
	Weight int64       `json:"weight"`
}

func loadV1(raw []byte) (*domain.Project, error) {
	var src v1Project
	if err := decodeStrict(raw, &src, "$"); err != nil {
		return nil, err
	}
	project := &domain.Project{ID: src.ProjectID}
	for index, item := range src.Tenants {
		quota, err := domain.PercentToBasisPoints(item.QuotaPercent, fmt.Sprintf("$.tenants[%d].quota_percent", index))
		if err != nil {
			return nil, err
		}
		reserve, err := domain.PercentToBasisPoints(item.ReservePercent, fmt.Sprintf("$.tenants[%d].reserve_percent", index))
		if err != nil {
			return nil, err
		}
		project.Tenants = append(project.Tenants, domain.Tenant{
			ID: item.ID, Name: item.Name, Priority: item.Priority, Weight: item.Weight,
			QuotaBasisPoints: quota, ReserveBasisPoints: reserve,
		})
	}
	project.Tenants = normalizeTenants(project.Tenants)
	for index, item := range src.Nodes {
		capacity, err := domain.ParseDecimalCores(item.CoreCapacity.String(), fmt.Sprintf("$.nodes[%d].core_capacity", index))
		if err != nil {
			return nil, err
		}
		project.Nodes = append(project.Nodes, domain.Node{
			ID: item.ID, Rack: item.Rack, CapacityMilli: capacity,
			AllowedTenants: item.AllowedTenants, FaultDomain: item.Rack,
		})
	}
	project.Nodes = normalizeNodes(project.Nodes)
	project.FaultDomains = ensureRackDomains(project.Nodes, nil)
	rawEvents := make([]rawEvent, 0, len(src.Events))
	for index, item := range src.Events {
		if item.Seq < 1 {
			return nil, domain.NewBizError(domain.EEventOrder, fmt.Sprintf("$.events[%d].seq", index), "seq must be >= 1")
		}
		kind, err := eventKind(item.Kind, fmt.Sprintf("$.events[%d].kind", index))
		if err != nil {
			return nil, err
		}
		event := domain.Event{ID: item.ID, Kind: kind, Tenant: item.Tenant, Node: item.Node, Weight: item.Weight}
		if kind == domain.EventRequest || kind == domain.EventRelease {
			amount, err := domain.ParseDecimalCores(item.Cores.String(), fmt.Sprintf("$.events[%d].cores", index))
			if err != nil {
				return nil, err
			}
			event.AmountMilli = amount
		}
		rawEvents = append(rawEvents, rawEvent{TimeKey: item.Seq, InputIndex: index, Event: event})
	}
	events, err := canonicalizeEvents(rawEvents, "$.events")
	if err != nil {
		return nil, err
	}
	project.Events = events
	return project, nil
}
