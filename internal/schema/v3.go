package schema

import (
	"fmt"

	"quotaflow/internal/domain"
)

type v3Project struct {
	Version      string     `json:"version"`
	ProjectID    string     `json:"projectId"`
	TenantList   []v3Tenant `json:"tenantList"`
	RackTree     []v3Rack   `json:"rackTree"`
	FaultDomains []v3Domain `json:"faultDomains,omitempty"`
	Timeline     []v3Event  `json:"timeline"`
}

type v3Tenant struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Priority  int    `json:"priority"`
	Weight    int64  `json:"weight"`
	QuotaBP   int64  `json:"quotaBp"`
	ReserveBP int64  `json:"reserveBp"`
}

type v3Rack struct {
	RackID        string   `json:"rackId"`
	FaultDomainID *string  `json:"faultDomainId,omitempty"`
	Nodes         []v3Node `json:"nodes"`
}

type v3Node struct {
	NodeID             string   `json:"nodeId"`
	CapacityMillicores int64    `json:"capacityMillicores"`
	AllowedTenantIDs   []string `json:"allowedTenantIds"`
	FaultDomainID      *string  `json:"faultDomainId,omitempty"`
}

type v3Domain struct {
	DomainID string `json:"domainId"`
	ParentID string `json:"parentId,omitempty"`
}

type v3Event struct {
	At         int64  `json:"at"`
	EventID    string `json:"eventId"`
	Type       string `json:"type"`
	TenantID   string `json:"tenantId"`
	NodeID     string `json:"nodeId"`
	Millicores int64  `json:"millicores"`
	Weight     int64  `json:"weight"`
}

func loadV3(raw []byte) (*domain.Project, error) {
	var src v3Project
	if err := decodeStrict(raw, &src, "$"); err != nil {
		return nil, err
	}
	project := &domain.Project{ID: src.ProjectID}
	for _, item := range src.TenantList {
		project.Tenants = append(project.Tenants, domain.Tenant{
			ID: item.Key, Name: item.Label, Priority: item.Priority, Weight: item.Weight,
			QuotaBasisPoints: item.QuotaBP, ReserveBasisPoints: item.ReserveBP,
		})
	}
	project.Tenants = normalizeTenants(project.Tenants)
	for _, item := range src.FaultDomains {
		project.FaultDomains = append(project.FaultDomains, domain.FaultDomain{ID: item.DomainID, Parent: item.ParentID})
	}
	for rackIndex, rack := range src.RackTree {
		for nodeIndex, item := range rack.Nodes {
			fd := rack.RackID
			if item.FaultDomainID != nil {
				fd = *item.FaultDomainID
			} else if rack.FaultDomainID != nil {
				fd = *rack.FaultDomainID
			}
			if fd == "" {
				return nil, domain.NewBizError(domain.ESchemaRequired,
					fmt.Sprintf("$.rackTree[%d].nodes[%d].faultDomainId", rackIndex, nodeIndex), "missing fault domain")
			}
			project.Nodes = append(project.Nodes, domain.Node{
				ID: item.NodeID, Rack: rack.RackID, CapacityMilli: item.CapacityMillicores,
				AllowedTenants: item.AllowedTenantIDs, FaultDomain: fd,
			})
		}
	}
	project.Nodes = normalizeNodes(project.Nodes)
	if err := validateRackTree(project); err != nil {
		return nil, err
	}
	project.FaultDomains = ensureRackDomains(project.Nodes, project.FaultDomains)
	rawEvents := make([]rawEvent, 0, len(src.Timeline))
	for index, item := range src.Timeline {
		if err := timeKeyRequired(item.At, fmt.Sprintf("$.timeline[%d].at", index)); err != nil {
			return nil, err
		}
		kind, err := eventKind(item.Type, fmt.Sprintf("$.timeline[%d].type", index))
		if err != nil {
			return nil, err
		}
		rawEvents = append(rawEvents, rawEvent{
			TimeKey: item.At, InputIndex: index,
			Event: domain.Event{
				ID: item.EventID, Kind: kind, Tenant: item.TenantID, Node: item.NodeID,
				AmountMilli: item.Millicores, Weight: item.Weight,
			},
		})
	}
	events, err := canonicalizeEvents(rawEvents, "$.timeline")
	if err != nil {
		return nil, err
	}
	project.Events = events
	return project, nil
}
