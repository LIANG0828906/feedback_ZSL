package projectio

import (
	"fmt"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/model"
)

func validateProject(p *model.Project) error {
	if p.Policy.TickMillis <= 0 {
		p.Policy.TickMillis = 10
	}
	if p.Policy.RatioBasisBytes <= 0 {
		return bizerror.New(bizerror.CodeFieldRequired, "/policy/ratio_basis_bytes", "ratio basis is required")
	}
	if p.Policy.ToleranceBytes <= 0 {
		p.Policy.ToleranceBytes = 1
	}
	if p.Policy.DefaultBudgetMS <= 0 {
		p.Policy.DefaultBudgetMS = 5
	}
	if p.Policy.FastCostMS <= 0 {
		p.Policy.FastCostMS = 1
	}
	if p.Policy.PreciseCostMS <= 0 {
		p.Policy.PreciseCostMS = 2
	}
	if p.Policy.WorkerBudget <= 0 {
		p.Policy.WorkerBudget = 2
	}
	if p.Policy.TaskQueueCapacity <= 0 {
		p.Policy.TaskQueueCapacity = p.Policy.WorkerBudget * 4
	}
	if p.Policy.TenantQueueLimit <= 0 {
		p.Policy.TenantQueueLimit = 4
	}
	if p.Policy.RetryBackoffTicks <= 0 {
		p.Policy.RetryBackoffTicks = 1
	}

	nodeIDs := map[string]bool{}
	for i, n := range p.Nodes {
		path := fmt.Sprintf("/nodes/%d", i)
		if nodeIDs[n.ID] {
			return bizerror.New(bizerror.CodeDuplicateID, path+"/id", "duplicate node id %q", n.ID)
		}
		nodeIDs[n.ID] = true
	}
	tenantIDs := map[string]bool{}
	for i, t := range p.Tenants {
		path := fmt.Sprintf("/tenants/%d", i)
		if tenantIDs[t.ID] {
			return bizerror.New(bizerror.CodeDuplicateID, path+"/id", "duplicate tenant id %q", t.ID)
		}
		tenantIDs[t.ID] = true
		if t.GuaranteeBytes > t.QuotaBytes {
			return bizerror.New(bizerror.CodeValueOutOfRange, path+"/guarantee_bytes", "guarantee cannot exceed quota")
		}
		if t.InitialWeight <= 0 {
			p.Tenants[i].InitialWeight = 100
		}
	}

	allowed := map[string]map[string]bool{}
	for _, n := range p.Nodes {
		allowed[n.ID] = map[string]bool{}
		for _, tid := range n.AllowedTenants {
			if !tenantIDs[tid] {
				return bizerror.New(bizerror.CodeReferenceMissing, "/nodes/"+n.ID+"/allowed_tenants", "tenant %q does not exist", tid)
			}
			if allowed[n.ID][tid] {
				return bizerror.New(bizerror.CodeDuplicateID, "/nodes/"+n.ID+"/allowed_tenants", "duplicate allowlist tenant %q", tid)
			}
			allowed[n.ID][tid] = true
		}
	}

	requestIDs := map[string]bool{}
	applyByRequest := map[string]model.Event{}
	for i, e := range p.Events {
		path := fmt.Sprintf("/events/%d", i)
		if e.Tick < 0 {
			return bizerror.New(bizerror.CodeValueOutOfRange, path, "tick cannot be negative")
		}
		if e.Kind == model.EventApply {
			if requestIDs[e.RequestID] {
				return bizerror.New(bizerror.CodeDuplicateID, path+"/request_id", "duplicate request id %q", e.RequestID)
			}
			requestIDs[e.RequestID] = true
			if !nodeIDs[e.NodeID] {
				return bizerror.New(bizerror.CodeReferenceMissing, path+"/node_id", "node %q does not exist", e.NodeID)
			}
			if !tenantIDs[e.TenantID] {
				return bizerror.New(bizerror.CodeReferenceMissing, path+"/tenant_id", "tenant %q does not exist", e.TenantID)
			}
			if !allowed[e.NodeID][e.TenantID] {
				return bizerror.New(bizerror.CodeNotAllowed, path, "tenant %q is not allowed on node %q", e.TenantID, e.NodeID)
			}
			applyByRequest[e.RequestID] = e
		}
		if e.Kind == model.EventRelease {
			src, ok := applyByRequest[e.ReleaseRequestID]
			if !ok {
				return bizerror.New(bizerror.CodeReferenceMissing, path+"/release_request_id", "release references unknown apply %q", e.ReleaseRequestID)
			}
			if e.AmountBytes == 0 {
				p.Events[i].AmountBytes = src.AmountBytes
			}
			if e.AmountBytes > src.AmountBytes {
				return bizerror.New(bizerror.CodeValueOutOfRange, path+"/amount_bytes", "release exceeds requested amount")
			}
			if e.Tick < src.Tick {
				return bizerror.New(bizerror.CodeValueOutOfRange, path+"/tick", "release precedes its apply")
			}
		}
		if e.Kind == model.EventWeight && !tenantIDs[e.TenantID] {
			return bizerror.New(bizerror.CodeReferenceMissing, path+"/tenant_id", "tenant %q does not exist", e.TenantID)
		}
	}
	return nil
}
