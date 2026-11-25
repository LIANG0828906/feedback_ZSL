package schema

import (
	"fmt"

	"quotaflow/internal/domain"
)

type v2Project struct {
	Schema       string     `json:"schema"`
	ProjectID    string     `json:"projectId"`
	Tenants      []v2Tenant `json:"tenants"`
	Nodes        []v2Node   `json:"nodes"`
	FaultDomains []v2Domain `json:"faultDomains"`
	Events       []v2Event  `json:"events"`
}

type v2Tenant struct {
	TenantID           string `json:"tenantId"`
	Name               string `json:"name"`
	Priority           int    `json:"priority"`
	Weight             int64  `json:"weight"`
	QuotaBasisPoints   int64  `json:"quotaBasisPoints"`
	ReserveBasisPoints int64  `json:"reserveBasisPoints"`
}

type v2Node struct {
	NodeID             string   `json:"nodeId"`
	RackID             string   `json:"rackId"`
	CapacityMillicores int64    `json:"capacityMillicores"`
	AllowedTenantIDs   []string `json:"allowedTenantIds"`
	FaultDomainID      *string  `json:"faultDomainId,omitempty"`
}

type v2Domain struct {
	DomainID string `json:"domainId"`
	ParentID string `json:"parentId,omitempty"`
}

type v2Event struct {
	EventID     string `json:"eventId"`
	TimestampMs int64  `json:"timestampMs"`
	Event       string `json:"event"`
	TenantID    string `json:"tenantId"`
	NodeID      string `json:"nodeId"`
	Millicores  int64  `json:"millicores"`
	Weight      int64  `json:"weight"`
}

func loadV2(raw []byte) (*domain.Project, error) {
	var src v2Project
	if err := decodeStrict(raw, &src, "$"); err != nil {
		return nil, err
	}
	project := &domain.Project{ID: src.ProjectID}
	for _, item := range src.Tenants {
		project.Tenants = append(project.Tenants, domain.Tenant{
			ID: item.TenantID, Name: item.Name, Priority: item.Priority, Weight: item.Weight,
			QuotaBasisPoints: item.QuotaBasisPoints, ReserveBasisPoints: item.ReserveBasisPoints,
		})
	}
	project.Tenants = normalizeTenants(project.Tenants)
	for _, item := range src.FaultDomains {
		project.FaultDomains = append(project.FaultDomains, domain.FaultDomain{ID: item.DomainID, Parent: item.ParentID})
	}
	for index, item := range src.Nodes {
		fd := ""
		if item.FaultDomainID != nil {
			fd = *item.FaultDomainID
			if fd == "" {
				return nil, domain.NewBizError(domain.ESchemaInvalid, fmt.Sprintf("$.nodes[%d].faultDomainId", index), "empty fault domain id")
			}
		} else {
			fd = item.RackID
		}
		project.Nodes = append(project.Nodes, domain.Node{
			ID: item.NodeID, Rack: item.RackID, CapacityMilli: item.CapacityMillicores,
			AllowedTenants: item.AllowedTenantIDs, FaultDomain: fd,
		})
	}
	project.Nodes = normalizeNodes(project.Nodes)
	project.FaultDomains = ensureRackDomains(project.Nodes, project.FaultDomains)
	rawEvents := make([]rawEvent, 0, len(src.Events))
	for index, item := range src.Events {
		if err := timeKeyRequired(item.TimestampMs, fmt.Sprintf("$.events[%d].timestampMs", index)); err != nil {
			return nil, err
		}
		kind, err := eventKind(item.Event, fmt.Sprintf("$.events[%d].event", index))
		if err != nil {
			return nil, err
		}
		rawEvents = append(rawEvents, rawEvent{
			TimeKey: item.TimestampMs, InputIndex: index,
			Event: domain.Event{
				ID: item.EventID, Kind: kind, Tenant: item.TenantID, Node: item.NodeID,
				AmountMilli: item.Millicores, Weight: item.Weight,
			},
		})
	}
	events, err := canonicalizeEvents(rawEvents, "$.events")
	if err != nil {
		return nil, err
	}
	project.Events = events
	return project, nil
}
