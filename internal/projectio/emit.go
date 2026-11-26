package projectio

import (
	"encoding/json"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/model"
)

func MarshalV1(p *model.Project) ([]byte, error) {
	doc := docV1{
		SchemaVersion: model.SchemaV1, ProjectID: p.ProjectID, Seed: p.Seed, LogicalTicks: p.LogicalTicks,
		Policy: policyDTOV1{TickMillis: p.Policy.TickMillis, RatioBasisBytes: p.Policy.RatioBasisBytes, ToleranceBytes: p.Policy.ToleranceBytes, DefaultBudgetMS: p.Policy.DefaultBudgetMS, WorkerBudget: p.Policy.WorkerBudget, TaskQueueCapacity: p.Policy.TaskQueueCapacity, TenantQueueLimit: p.Policy.TenantQueueLimit, RetryBackoffTicks: p.Policy.RetryBackoffTicks},
	}
	for _, n := range p.Nodes {
		doc.Nodes = append(doc.Nodes, nodeV1{ID: n.ID, Rack: n.Rack, CapacityBytes: n.CapacityBytes, AllowedTenants: n.AllowedTenants})
	}
	for _, t := range p.Tenants {
		qp, err := bytesToScale(t.QuotaBytes, 1000, p.Policy.RatioBasisBytes, "/quota")
		if err != nil {
			return nil, err
		}
		gp, err := bytesToScale(t.GuaranteeBytes, 1000, p.Policy.RatioBasisBytes, "/guarantee")
		if err != nil {
			return nil, err
		}
		doc.Tenants = append(doc.Tenants, tenantV1{ID: t.ID, Priority: t.Priority, InitialWeight: t.InitialWeight, QuotaPermille: qp, GuaranteePermille: gp, HomeNodeID: t.HomeNodeID})
	}
	for _, e := range p.Events {
		doc.Events = append(doc.Events, eventV1{Sequence: e.Sequence, AtTick: e.Tick, Type: string(e.Kind), RequestID: e.RequestID, NodeID: e.NodeID, TenantID: e.TenantID, AmountBytes: e.AmountBytes, NewWeight: e.Weight, DecisionBudgetMS: e.LatencyBudgetMS, ReleaseRequestID: e.ReleaseRequestID})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func MarshalV2(p *model.Project) ([]byte, error) {
	doc := docV2{
		APIVersion: model.SchemaV2, ProjectName: p.ProjectID, DeterministicSeed: p.Seed, LogicalTickCount: p.LogicalTicks,
		Timing: timingV2{TickDurationMillis: p.Policy.TickMillis}, Scale: scaleV2{RatioBasisBytes: p.Policy.RatioBasisBytes, ToleranceBytes: p.Policy.ToleranceBytes},
		Runtime: runtimeV2{DefaultBudgetMillis: p.Policy.DefaultBudgetMS, WorkerBudget: p.Policy.WorkerBudget, TaskQueueCapacity: p.Policy.TaskQueueCapacity, TenantQueueLimit: p.Policy.TenantQueueLimit, RetryBackoffTicks: p.Policy.RetryBackoffTicks},
	}
	rackIndex := map[string]int{}
	for _, n := range p.Nodes {
		idx, ok := rackIndex[n.Rack]
		if !ok {
			idx = len(doc.Topology.Racks)
			rackIndex[n.Rack] = idx
			doc.Topology.Racks = append(doc.Topology.Racks, rackV2{RackID: n.Rack})
		}
		doc.Topology.Racks[idx].Nodes = append(doc.Topology.Racks[idx].Nodes, nodeV2{NodeID: n.ID, CapacityKiB: n.CapacityBytes / 1024, TenantAllowlist: n.AllowedTenants})
	}
	for _, t := range p.Tenants {
		q, err := bytesToScale(t.QuotaBytes, 10000, p.Policy.RatioBasisBytes, "/quota")
		if err != nil {
			return nil, err
		}
		g, err := bytesToScale(t.GuaranteeBytes, 10000, p.Policy.RatioBasisBytes, "/guarantee")
		if err != nil {
			return nil, err
		}
		burstKiB := t.BurstLimitBytes / 1024
		doc.TenantProfiles = append(doc.TenantProfiles, tenantV2{TenantID: t.ID, PriorityClass: t.Priority, Weight: t.InitialWeight, QuotaBasisPoints: q, GuaranteeBasisPoints: g, BurstLimitKiB: &burstKiB, HomeNodeID: t.HomeNodeID})
	}
	for _, e := range p.Events {
		doc.EventLog = append(doc.EventLog, eventV2{SequenceNumber: e.Sequence, AtMillis: e.Tick * p.Policy.TickMillis, EventType: string(e.Kind), RequestID: e.RequestID, NodeID: e.NodeID, TenantID: e.TenantID, AmountKiB: e.AmountBytes / 1024, NewWeight: e.Weight, DecisionBudgetMillis: e.LatencyBudgetMS, ReleaseRequestID: e.ReleaseRequestID})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func MarshalV3(p *model.Project) ([]byte, error) {
	doc := docV3{
		Spec:          model.SchemaV3,
		Metadata:      metadataV3{ID: p.ProjectID, Seed: p.Seed, LogicalTicks: p.LogicalTicks},
		Configuration: configV3{TickMS: p.Policy.TickMillis, RatioBasisBytes: p.Policy.RatioBasisBytes, ToleranceBytes: p.Policy.ToleranceBytes, DefaultBudgetMS: p.Policy.DefaultBudgetMS, WorkerBudget: p.Policy.WorkerBudget, TaskQueueCapacity: p.Policy.TaskQueueCapacity, TenantQueueLimit: p.Policy.TenantQueueLimit, RetryBackoffTicks: p.Policy.RetryBackoffTicks},
	}
	for _, n := range p.Nodes {
		doc.NodeInventory = append(doc.NodeInventory, nodeV3{NodeRef: n.ID, RackRef: n.Rack, CapacityKiB: n.CapacityBytes / 1024, TenantAllowlist: n.AllowedTenants})
	}
	for i, t := range p.Tenants {
		q, err := bytesToScale(t.QuotaBytes, 10000, p.Policy.RatioBasisBytes, "/quota")
		if err != nil {
			return nil, err
		}
		g, err := bytesToScale(t.GuaranteeBytes, 10000, p.Policy.RatioBasisBytes, "/guarantee")
		if err != nil {
			return nil, err
		}
		tenant := tenantV3{TenantRef: t.ID, PriorityRank: t.Priority, BaseWeight: t.InitialWeight, QuotaPermyriad: q, GuaranteePermyriad: g, HomeNodeRef: t.HomeNodeID}
		if i != 1 {
			kib := t.BurstLimitBytes / 1024
			tenant.BurstCapKiB = &kib
		}
		doc.TenantInventory = append(doc.TenantInventory, tenant)
	}
	for _, e := range p.Events {
		doc.Timeline = append(doc.Timeline, eventV3{Seq: e.Sequence, TimestampMS: e.Tick * p.Policy.TickMillis, Action: string(e.Kind), RequestRef: e.RequestID, NodeRef: e.NodeID, TenantRef: e.TenantID, QuantityKiB: e.AmountBytes / 1024, WeightValue: e.Weight, BudgetMS: e.LatencyBudgetMS, ReleaseRequestRef: e.ReleaseRequestID})
	}
	return json.MarshalIndent(doc, "", "  ")
}

func bytesToScale(bytes, scale, basis int64, path string) (int64, error) {
	if basis == 0 || (bytes*scale)%basis != 0 {
		return 0, bizerror.New(bizerror.CodeToleranceExceeded, path, "canonical bytes %d are not an exact %d-scale fraction of basis %d", bytes, scale, basis)
	}
	return bytes * scale / basis, nil
}
