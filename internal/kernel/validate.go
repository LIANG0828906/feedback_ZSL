package kernel

import "fmt"

func ValidateProject(p *Project) error {
	if p == nil {
		return NewBizError(CodeReferenceNotFound, "project is nil").WithPath("$")
	}
	if p.ProjectID == "" {
		return NewBizError(CodeReferenceNotFound, "project_id is required").WithPath("$.project_id")
	}
	if err := p.Policy.Validate(); err != nil {
		return err
	}
	if p.TickMS <= 0 {
		return NewBizError(CodeTimeBasis, "tick_ms must be positive, got %d", p.TickMS).WithPath("$.tick_ms")
	}

	nodeIDs := map[ID]int{}
	for i, n := range p.Nodes {
		path := fmt.Sprintf("$.nodes[%d]", i)
		if n.ID == "" {
			return NewBizError(CodeReferenceNotFound, "node id is required").WithPath(path + ".id")
		}
		if old, ok := nodeIDs[n.ID]; ok {
			return NewBizError(CodeDuplicateID, "duplicate node id %q", n.ID).
				WithPath(path+".id").WithNode(n.ID).WithDetail("first_index", old)
		}
		nodeIDs[n.ID] = i
		if n.CapacityBytes < 0 {
			return NewBizError(CodeNegativeCapacity, "capacity must be non-negative, got %d", n.CapacityBytes).
				WithPath(path + ".capacity").WithNode(n.ID)
		}
	}

	tenantIDs := map[ID]int{}
	for i, t := range p.Tenants {
		path := fmt.Sprintf("$.tenants[%d]", i)
		if t.ID == "" {
			return NewBizError(CodeReferenceNotFound, "tenant id is required").WithPath(path + ".id")
		}
		if old, ok := tenantIDs[t.ID]; ok {
			return NewBizError(CodeDuplicateID, "duplicate tenant id %q", t.ID).
				WithPath(path+".id").WithTenant(t.ID).WithDetail("first_index", old)
		}
		tenantIDs[t.ID] = i
		if t.QuotaBytes < 0 {
			return NewBizError(CodeNegativeQuota, "quota must be non-negative, got %d", t.QuotaBytes).
				WithPath(path + ".quota").WithTenant(t.ID)
		}
		if t.GuaranteeBytes < 0 {
			return NewBizError(CodeNegativeQuota, "guarantee must be non-negative, got %d", t.GuaranteeBytes).
				WithPath(path + ".guarantee").WithTenant(t.ID)
		}
		if t.BurstLimitBytes < 0 {
			return NewBizError(CodeNegativeQuota, "burst limit must be non-negative, got %d", t.BurstLimitBytes).
				WithPath(path + ".burst_limit").WithTenant(t.ID)
		}
		if t.GuaranteeBytes > t.QuotaBytes {
			return NewBizError(CodeGuaranteeOverQuota, "guarantee %d exceeds quota %d", t.GuaranteeBytes, t.QuotaBytes).
				WithPath(path+".guarantee").WithTenant(t.ID).
				WithDetail("guarantee_bytes", t.GuaranteeBytes).WithDetail("quota_bytes", t.QuotaBytes)
		}
		if t.WeightPermyriad < 0 || t.WeightPermyriad > 10000 {
			return NewBizError(CodeBurstPercentRange, "weight permyriad must be [0,10000], got %d", t.WeightPermyriad).
				WithPath(path + ".weight").WithTenant(t.ID)
		}
		if t.GuaranteeNodeID != "" {
			n, ok := p.Node(t.GuaranteeNodeID)
			if !ok {
				return NewBizError(CodeReferenceNotFound, "guarantee node %q not found", t.GuaranteeNodeID).
					WithPath(path + ".guarantee_node_id").WithTenant(t.ID).WithNode(t.GuaranteeNodeID)
			}
			if !containsID(n.AllowedTenants, t.ID) {
				return NewBizError(CodeNodeAffinity, "tenant %q is not allowed on guarantee node %q", t.ID, n.ID).
					WithPath(path + ".guarantee_node_id").WithTenant(t.ID).WithNode(n.ID)
			}
		}
	}

	for _, n := range p.Nodes {
		for _, tid := range n.AllowedTenants {
			if _, ok := tenantIDs[tid]; !ok {
				return NewBizError(CodeReferenceNotFound, "node %q allows unknown tenant %q", n.ID, tid).
					WithPath("$.nodes.allowed_tenants").WithNode(n.ID).WithTenant(tid)
			}
		}
	}

	reserved := map[ID]int64{}
	for _, t := range p.Tenants {
		if t.GuaranteeNodeID != "" {
			reserved[t.GuaranteeNodeID] += t.GuaranteeBytes
		}
	}
	for nid, total := range reserved {
		n, _ := p.Node(nid)
		if total > n.CapacityBytes {
			return NewBizError(CodeGuaranteeOvercommit, "guarantees reserve %d > node capacity %d", total, n.CapacityBytes).
				WithNode(nid).WithDetail("reserved_bytes", total).WithDetail("capacity_bytes", n.CapacityBytes).
				WithConstraint(ConstraintGuaranteeFloor)
		}
	}

	eventIDs := map[string]int{}
	var lastTick int64 = -1
	for i, e := range p.Events {
		path := fmt.Sprintf("$.events[%d]", i)
		if e.ID == "" {
			return NewBizError(CodeReferenceNotFound, "event id is required").WithPath(path + ".id")
		}
		if old, ok := eventIDs[e.ID]; ok {
			return NewBizError(CodeDuplicateID, "duplicate event id %q", e.ID).
				WithPath(path+".id").WithEvent(e.ID).WithDetail("first_index", old)
		}
		eventIDs[e.ID] = i
		if e.Tick < 0 {
			return NewBizError(CodeEventOrder, "tick must be non-negative, got %d", e.Tick).
				WithPath(path + ".tick").WithEvent(e.ID)
		}
		if e.Tick < lastTick {
			return NewBizError(CodeEventOrder, "tick %d precedes previous tick %d", e.Tick, lastTick).
				WithPath(path+".tick").WithEvent(e.ID).
				WithDetail("tick", e.Tick).WithDetail("previous_tick", lastTick)
		}
		lastTick = e.Tick
		if e.BudgetMS < 0 {
			return NewBizError(CodeDecisionBudget, "decision budget must be non-negative").
				WithPath(path + ".decision_budget_ms").WithEvent(e.ID)
		}
		switch e.Type {
		case EventTypeApply, EventTypeRelease:
			if e.AmountBytes < 0 {
				code := CodeNegativeAmount
				return NewBizError(code, "amount must be non-negative, got %d", e.AmountBytes).
					WithPath(path + ".amount").WithEvent(e.ID).WithTenant(e.TenantID).WithNode(e.NodeID)
			}
			t, ok := p.Tenant(e.TenantID)
			if !ok {
				return NewBizError(CodeReferenceNotFound, "event tenant %q not found", e.TenantID).
					WithPath(path + ".tenant_id").WithEvent(e.ID).WithTenant(e.TenantID)
			}
			n, ok := p.Node(e.NodeID)
			if !ok {
				return NewBizError(CodeReferenceNotFound, "event node %q not found", e.NodeID).
					WithPath(path + ".node_id").WithEvent(e.ID).WithNode(e.NodeID)
			}
			if !containsID(n.AllowedTenants, t.ID) {
				return NewBizError(CodeNodeAffinity, "tenant %q is not allowed on node %q", t.ID, n.ID).
					WithPath(path).WithEvent(e.ID).WithTenant(t.ID).WithNode(n.ID)
			}
		case EventTypeWeight:
			if _, ok := p.Tenant(e.TenantID); !ok {
				return NewBizError(CodeReferenceNotFound, "weight event tenant %q not found", e.TenantID).
					WithPath(path + ".tenant_id").WithEvent(e.ID).WithTenant(e.TenantID)
			}
			if e.WeightPermyriad < 0 || e.WeightPermyriad > 10000 {
				return NewBizError(CodeBurstPercentRange, "weight permyriad must be [0,10000], got %d", e.WeightPermyriad).
					WithPath(path + ".weight").WithEvent(e.ID).WithTenant(e.TenantID)
			}
		default:
			return NewBizError(CodeUnknownField, "unsupported event type %q", e.Type).
				WithPath(path + ".type").WithEvent(e.ID)
		}
		for _, ref := range e.Refs {
			if _, ok := eventIDs[ref]; !ok {
				// Forward references are allowed for DAG support, but any
				// reference must eventually resolve. Defer to graph check.
				if !eventExists(p.Events, ref) {
					return NewBizError(CodeReferenceNotFound, "event %q references missing event %q", e.ID, ref).
						WithPath(path+".causal_refs").WithEvent(e.ID).WithDetail("reference", ref)
				}
			}
		}
	}

	if cycle := CausalCycle(p.Events); cycle != nil {
		return NewBizError(CodeCausalCycle, "causal reference cycle: %v", *cycle).
			WithPath("$.events.causal_refs").WithDetail("cycle", *cycle)
	}
	return nil
}

func eventExists(events []Event, id string) bool {
	for _, e := range events {
		if e.ID == id {
			return true
		}
	}
	return false
}

func containsID(ids []ID, want ID) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// CausalCycle returns a locatable cycle path when refs contain a directed cycle.
func CausalCycle(events []Event) *[]string {
	byID := map[string]Event{}
	for _, e := range events {
		byID[e.ID] = e
	}
	state := map[string]int{}
	stack := []string{}
	var cycle []string
	var dfs func(string) bool
	dfs = func(id string) bool {
		state[id] = 1
		stack = append(stack, id)
		for _, ref := range byID[id].Refs {
			if _, ok := byID[ref]; !ok {
				continue
			}
			if state[ref] == 0 {
				if dfs(ref) {
					return true
				}
			} else if state[ref] == 1 {
				start := 0
				for i, v := range stack {
					if v == ref {
						start = i
						break
					}
				}
				cycle = append(append([]string{}, stack[start:]...), ref)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return false
	}
	for _, e := range events {
		if state[e.ID] == 0 && dfs(e.ID) {
			return &cycle
		}
	}
	return nil
}
