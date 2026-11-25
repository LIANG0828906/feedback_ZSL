package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	Tenant string      `json:"tenant,omitempty"`
	Node   string      `json:"node,omitempty"`
	Cores  json.Number `json:"cores,omitempty"`
	Weight int64       `json:"weight,omitempty"`
}

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
	FaultDomainID      string   `json:"faultDomainId"`
}

type v2Domain struct {
	DomainID string `json:"domainId"`
	ParentID string `json:"parentId,omitempty"`
}

type v2Event struct {
	EventID     string `json:"eventId"`
	TimestampMs int64  `json:"timestampMs"`
	Event       string `json:"event"`
	TenantID    string `json:"tenantId,omitempty"`
	NodeID      string `json:"nodeId,omitempty"`
	Millicores  int64  `json:"millicores,omitempty"`
	Weight      int64  `json:"weight,omitempty"`
}

type v3Project struct {
	Version    string     `json:"version"`
	ProjectID  string     `json:"projectId"`
	TenantList []v3Tenant `json:"tenantList"`
	RackTree   []v3Rack   `json:"rackTree"`
	Timeline   []v3Event  `json:"timeline"`
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
	RackID string   `json:"rackId"`
	Nodes  []v3Node `json:"nodes"`
}

type v3Node struct {
	NodeID             string   `json:"nodeId"`
	CapacityMillicores int64    `json:"capacityMillicores"`
	AllowedTenantIDs   []string `json:"allowedTenantIds"`
}

type v3Event struct {
	At         int64  `json:"at"`
	EventID    string `json:"eventId"`
	Type       string `json:"type"`
	TenantID   string `json:"tenantId,omitempty"`
	NodeID     string `json:"nodeId,omitempty"`
	Millicores int64  `json:"millicores,omitempty"`
	Weight     int64  `json:"weight,omitempty"`
}

type Manifest struct {
	Seed               int64    `json:"seed"`
	ProjectID          string   `json:"project_id"`
	ProjectFingerprint string   `json:"project_fingerprint"`
	Files              []string `json:"files"`
}

func Generate(seed int64, outDir string) (*domain.Project, *domain.Truth, error) {
	project := Scenario(seed)
	truth, err := BuildTruth(project, seed, 64)
	if err != nil {
		return nil, nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, nil, domain.NewBizError(domain.ESchemaInvalid, outDir, "create output directory: %v", err)
	}
	files := map[string]any{}
	v1, err := encodeV1(project)
	if err != nil {
		return nil, nil, err
	}
	v2, err := encodeV2(project)
	if err != nil {
		return nil, nil, err
	}
	v3, err := encodeV3(project)
	if err != nil {
		return nil, nil, err
	}
	files["project-v1.json"] = v1
	files["project-v2.json"] = v2
	files["project-v3.json"] = v3
	files["truth.json"] = truth
	for _, name := range []string{"project-v1.json", "project-v2.json", "project-v3.json", "truth.json"} {
		if err := writeJSON(filepath.Join(outDir, name), files[name]); err != nil {
			return nil, nil, err
		}
	}
	manifest := Manifest{
		Seed: seed, ProjectID: project.ID, ProjectFingerprint: project.CanonicalFingerprint(),
		Files: []string{"project-v1.json", "project-v2.json", "project-v3.json", "truth.json"},
	}
	if err := writeJSON(filepath.Join(outDir, "manifest.json"), manifest); err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

func encodeV1(project *domain.Project) (*v1Project, error) {
	out := &v1Project{SchemaVersion: "v1", ProjectID: project.ID}
	for _, tenant := range project.Tenants {
		out.Tenants = append(out.Tenants, v1Tenant{
			ID: tenant.ID, Name: tenant.Name, Priority: tenant.Priority, Weight: tenant.Weight,
			QuotaPercent: tenant.QuotaBasisPoints / 100, ReservePercent: tenant.ReserveBasisPoints / 100,
		})
	}
	for _, node := range project.Nodes {
		out.Nodes = append(out.Nodes, v1Node{
			ID: node.ID, Rack: node.Rack, CoreCapacity: json.Number(domain.MillicoresToCores(node.CapacityMilli)),
			AllowedTenants: node.AllowedTenants,
		})
	}
	for _, event := range project.Events {
		item := v1Event{Seq: event.Tick, ID: event.ID, Tenant: event.Tenant, Node: event.Node, Weight: event.Weight}
		switch event.Kind {
		case domain.EventRequest:
			item.Kind = "request"
			item.Cores = json.Number(domain.MillicoresToCores(event.AmountMilli))
		case domain.EventRelease:
			item.Kind = "release"
			item.Cores = json.Number(domain.MillicoresToCores(event.AmountMilli))
		case domain.EventWeight:
			item.Kind = "weight"
		case domain.EventNodeFailure:
			item.Kind = "node_failure"
		case domain.EventNodeRecovery:
			item.Kind = "node_recovery"
		}
		out.Events = append(out.Events, item)
	}
	return out, nil
}

func encodeV2(project *domain.Project) (*v2Project, error) {
	out := &v2Project{Schema: "v2", ProjectID: project.ID}
	for _, tenant := range project.Tenants {
		out.Tenants = append(out.Tenants, v2Tenant{
			TenantID: tenant.ID, Name: tenant.Name, Priority: tenant.Priority, Weight: tenant.Weight,
			QuotaBasisPoints: tenant.QuotaBasisPoints, ReserveBasisPoints: tenant.ReserveBasisPoints,
		})
	}
	for _, node := range project.Nodes {
		out.Nodes = append(out.Nodes, v2Node{
			NodeID: node.ID, RackID: node.Rack, CapacityMillicores: node.CapacityMilli,
			AllowedTenantIDs: node.AllowedTenants, FaultDomainID: node.FaultDomain,
		})
	}
	for _, fd := range project.FaultDomains {
		out.FaultDomains = append(out.FaultDomains, v2Domain{DomainID: fd.ID, ParentID: fd.Parent})
	}
	base := int64(1700000000000)
	for _, event := range project.Events {
		item := v2Event{
			EventID: event.ID, TimestampMs: base + event.Tick*1000, TenantID: event.Tenant,
			NodeID: event.Node, Millicores: event.AmountMilli, Weight: event.Weight,
		}
		switch event.Kind {
		case domain.EventRequest:
			item.Event = "allocate"
		case domain.EventRelease:
			item.Event = "deallocate"
		case domain.EventWeight:
			item.Event = "reweight"
		case domain.EventNodeFailure:
			item.Event = "node_down"
		case domain.EventNodeRecovery:
			item.Event = "node_up"
		}
		out.Events = append(out.Events, item)
	}
	return out, nil
}

func encodeV3(project *domain.Project) (*v3Project, error) {
	out := &v3Project{Version: "3.0", ProjectID: project.ID}
	for _, tenant := range project.Tenants {
		out.TenantList = append(out.TenantList, v3Tenant{
			Key: tenant.ID, Label: tenant.Name, Priority: tenant.Priority, Weight: tenant.Weight,
			QuotaBP: tenant.QuotaBasisPoints, ReserveBP: tenant.ReserveBasisPoints,
		})
	}
	racks := map[string]*v3Rack{}
	for _, node := range project.Nodes {
		rack := racks[node.Rack]
		if rack == nil {
			rack = &v3Rack{RackID: node.Rack}
			racks[node.Rack] = rack
		}
		rack.Nodes = append(rack.Nodes, v3Node{
			NodeID: node.ID, CapacityMillicores: node.CapacityMilli, AllowedTenantIDs: node.AllowedTenants,
		})
	}
	rackIDs := make([]string, 0, len(racks))
	for rackID := range racks {
		rackIDs = append(rackIDs, rackID)
	}
	sort.Strings(rackIDs)
	for _, rackID := range rackIDs {
		rack := racks[rackID]
		sort.SliceStable(rack.Nodes, func(i, j int) bool { return rack.Nodes[i].NodeID < rack.Nodes[j].NodeID })
		out.RackTree = append(out.RackTree, *rack)
	}
	for _, event := range project.Events {
		item := v3Event{
			At: event.Tick, EventID: event.ID, TenantID: event.Tenant, NodeID: event.Node,
			Millicores: event.AmountMilli, Weight: event.Weight,
		}
		switch event.Kind {
		case domain.EventRequest:
			item.Type = "allocate"
		case domain.EventRelease:
			item.Type = "deallocate"
		case domain.EventWeight:
			item.Type = "reweight"
		case domain.EventNodeFailure:
			item.Type = "node_down"
		case domain.EventNodeRecovery:
			item.Type = "node_up"
		}
		out.Timeline = append(out.Timeline, item)
	}
	return out, nil
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return domain.NewBizError(domain.ESchemaInvalid, path, "encode JSON: %v", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return domain.NewBizError(domain.ESchemaInvalid, path, "write file: %v", err)
	}
	return nil
}

func FileNames() []string {
	return []string{"project-v1.json", "project-v2.json", "project-v3.json", "truth.json", "manifest.json"}
}

func ProjectPath(dir string, version string) string {
	return filepath.Join(dir, fmt.Sprintf("project-%s.json", version))
}
