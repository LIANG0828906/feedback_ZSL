package engine

import (
	"sort"

	"quotaflow/internal/domain"
)

func stateNodeReserveSum(project *domain.Project, state *State, nodeID string) (int64, error) {
	node, ok := project.NodeByID(nodeID)
	if !ok {
		return 0, domain.NewBizError(domain.ENodeUnknown, "$.nodes."+nodeID, "unknown node")
	}
	sum := int64(0)
	for _, allowed := range node.AllowedTenants {
		tenant, ok := state.Tenants[allowed]
		if !ok {
			return 0, domain.NewBizError(domain.ETenantUnknown, "$.nodes."+nodeID, "unknown tenant %q", allowed)
		}
		var err error
		sum, err = domain.CheckedAdd(sum, tenant.Reserve, "$.nodes."+nodeID)
		if err != nil {
			return 0, err
		}
	}
	return sum, nil
}

func stateNodeNonReservedCapacity(project *domain.Project, state *State, nodeID string) (int64, error) {
	reserveSum, err := stateNodeReserveSum(project, state, nodeID)
	if err != nil {
		return 0, err
	}
	node := state.Nodes[nodeID]
	if reserveSum > node.Capacity {
		return 0, domain.NewBizError(domain.EReserveExceedsNode, "$.nodes."+nodeID,
			"reserves %d exceed capacity %d", reserveSum, node.Capacity)
	}
	return node.Capacity - reserveSum, nil
}

func stateNodeNonReservedUsed(project *domain.Project, state *State, nodeID string) (int64, error) {
	node, ok := project.NodeByID(nodeID)
	if !ok {
		return 0, domain.NewBizError(domain.ENodeUnknown, "$.nodes."+nodeID, "unknown node")
	}
	used := int64(0)
	for _, tenantID := range node.AllowedTenants {
		tenant := state.Tenants[tenantID]
		burst := state.virtual(tenantID, nodeID) - tenant.Reserve
		if burst <= 0 {
			continue
		}
		var err error
		used, err = domain.CheckedAdd(used, burst, "$.nodes."+nodeID)
		if err != nil {
			return 0, err
		}
	}
	return used, nil
}

func (e *Engine) admissionCapacity(state *State, tenantID, nodeID string) (int64, error) {
	nresCap, err := stateNodeNonReservedCapacity(e.project, state, nodeID)
	if err != nil {
		return 0, err
	}
	nresUsed, err := stateNodeNonReservedUsed(e.project, state, nodeID)
	if err != nil {
		return 0, err
	}
	if nresUsed > nresCap {
		return 0, domain.NewBizError(domain.ELedgerInvariant, "$.nodes."+nodeID,
			"non-reserved usage %d exceeds capacity %d", nresUsed, nresCap)
	}
	free := nresCap - nresUsed
	tenant := state.Tenants[tenantID]
	ownGap := tenant.Reserve - state.virtual(tenantID, nodeID)
	if ownGap < 0 {
		ownGap = 0
	}
	return domain.CheckedAdd(free, ownGap, "$.nodes."+nodeID+".admission")
}

func (e *Engine) request(state *State, event domain.Event) error {
	if _, paused := state.Pauses[event.Tenant]; paused {
		return e.enqueue(state, Pending{
			ID: event.ID, TenantID: event.Tenant, NodeID: event.Node, Amount: event.AmountMilli,
			Reason: "migration_pause", BlockedBy: []string{event.Tenant},
		})
	}
	admitted, reason, blocked, err := e.tryAdmit(state, event.Tenant, event.Node, event.AmountMilli)
	if err != nil {
		return err
	}
	if admitted > 0 {
		if err := e.addPlacement(state, event.Tenant, event.Node, admitted, true); err != nil {
			return err
		}
	}
	if admitted == event.AmountMilli {
		return nil
	}
	remainder := event.AmountMilli - admitted
	tenant := state.Tenants[event.Tenant]
	encroached, err := domain.CheckedAdd(tenant.Encroached, remainder, "$.tenants."+event.Tenant+".encroached")
	if err != nil {
		e.stats.FixedStats.Overflows++
		return err
	}
	tenant.Encroached = encroached
	state.Tenants[event.Tenant] = tenant
	return e.enqueue(state, Pending{
		ID: event.ID, TenantID: event.Tenant, NodeID: event.Node, Amount: remainder,
		Reason: reason, BlockedBy: blocked,
	})
}

func (e *Engine) enqueue(state *State, pending Pending) error {
	tenant := state.Tenants[pending.TenantID]
	if len(tenant.Pending) >= e.cfg.QueueLimit {
		return domain.NewBizError(domain.EBackpressureQueue, "$.pending."+pending.TenantID,
			"tenant queue limit %d reached at tick %d", e.cfg.QueueLimit, state.Tick)
	}
	pending.BlockedBy = domain.SortedCopy(pending.BlockedBy)
	tenant.Pending = append(tenant.Pending, pending)
	state.Tenants[pending.TenantID] = tenant
	return nil
}

func (e *Engine) tryAdmit(state *State, tenantID, nodeID string, amount int64) (int64, string, []string, error) {
	node := state.Nodes[nodeID]
	if node.Failed {
		return 0, "node_failed", nil, nil
	}
	tenant := state.Tenants[tenantID]
	quotaRemaining := tenant.Quota - tenant.Used
	if quotaRemaining < 0 {
		return 0, "", nil, domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+tenantID, "negative quota remainder")
	}
	nodeCapacity, err := e.admissionCapacity(state, tenantID, nodeID)
	if err != nil {
		return 0, "", nil, err
	}
	admitted := amount
	if admitted > quotaRemaining {
		admitted = quotaRemaining
	}
	if admitted > nodeCapacity {
		admitted = nodeCapacity
	}
	if admitted < 0 {
		return 0, "", nil, domain.NewBizError(domain.ELedgerInvariant, "$.admission", "negative admission")
	}
	if admitted == amount {
		return admitted, "", nil, nil
	}
	reason := "quota"
	var blocked []string
	if quotaRemaining <= nodeCapacity {
		blocked = e.reserveBlockers(state, tenantID, nodeID, false)
	} else {
		reason = "node_capacity"
		blocked = e.reserveBlockers(state, tenantID, nodeID, true)
	}
	if len(blocked) == 0 {
		blocked = e.nodeUsers(state, tenantID, nodeID)
	}
	if len(blocked) == 0 && reason == "quota" {
		blocked = []string{tenantID}
	}
	return admitted, reason, blocked, nil
}

func (e *Engine) retryPending(state *State) error {
	var tenantIDs []string
	for _, tenant := range e.project.Tenants {
		if len(state.Tenants[tenant.ID].Pending) > 0 {
			tenantIDs = append(tenantIDs, tenant.ID)
		}
	}
	probes, err := e.parallelRetryProbes(state, tenantIDs)
	if err != nil {
		return err
	}
	for _, tenantID := range tenantIDs {
		if _, paused := state.Pauses[tenantID]; paused {
			continue
		}
		probe := probes[tenantID]
		if probe.quotaRemaining <= 0 && probe.admission <= 0 {
			continue
		}
		tenant := state.Tenants[tenantID]
		queue := tenant.Pending
		kept := queue[:0]
		for _, pending := range queue {
			admitted, reason, blocked, err := e.tryAdmit(state, pending.TenantID, pending.NodeID, pending.Amount)
			if err != nil {
				return err
			}
			if admitted > 0 {
				if err := e.addPlacement(state, pending.TenantID, pending.NodeID, admitted, true); err != nil {
					return err
				}
				pending.Amount -= admitted
			}
			if pending.Amount == 0 {
				continue
			}
			pending.Reason = reason
			pending.BlockedBy = domain.SortedCopy(blocked)
			kept = append(kept, pending)
		}
		tenant = state.Tenants[tenantID]
		tenant.Pending = kept
		state.Tenants[tenantID] = tenant
	}
	return nil
}

func (e *Engine) addPlacement(state *State, tenantID, nodeID string, amount int64, updateTenant bool) error {
	if amount <= 0 {
		return domain.NewBizError(domain.ELedgerInvariant, "$.placement", "non-positive placement %d", amount)
	}
	node := state.Nodes[nodeID]
	if node.Failed {
		return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+nodeID, "cannot place on failed node")
	}
	current := state.actual(tenantID, nodeID)
	nextCurrent, err := domain.CheckedAdd(current, amount, "$.placement."+tenantID+"."+nodeID)
	if err != nil {
		e.stats.FixedStats.Overflows++
		return err
	}
	if nextCurrent > node.Capacity {
		return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+nodeID,
			"placement would use %d of %d", nextCurrent, node.Capacity)
	}
	nextNodeUsed, err := domain.CheckedAdd(node.Used, amount, "$.nodes."+nodeID+".used")
	if err != nil {
		e.stats.FixedStats.Overflows++
		return err
	}
	if nextNodeUsed > node.Capacity {
		return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+nodeID,
			"node usage %d would exceed capacity %d", nextNodeUsed, node.Capacity)
	}
	state.Placements[tenantID][nodeID] = nextCurrent
	node.Used = nextNodeUsed
	state.Nodes[nodeID] = node
	if updateTenant {
		tenant := state.Tenants[tenantID]
		nextUsed, err := domain.CheckedAdd(tenant.Used, amount, "$.tenants."+tenantID+".used")
		if err != nil {
			e.stats.FixedStats.Overflows++
			return err
		}
		tenant.Used = nextUsed
		if tenant.Used > tenant.Quota {
			return domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+tenantID,
				"used %d exceeds quota %d", tenant.Used, tenant.Quota)
		}
		state.Tenants[tenantID] = tenant
	}
	return nil
}

func (e *Engine) removePlacement(state *State, tenantID, nodeID string, amount int64, updateTenant bool) error {
	if amount <= 0 {
		return domain.NewBizError(domain.ELedgerInvariant, "$.placement", "non-positive removal %d", amount)
	}
	current := state.actual(tenantID, nodeID)
	if current < amount {
		return domain.NewBizError(domain.EReleaseExceedsUsed, "$.placement."+tenantID+"."+nodeID,
			"release %d exceeds placement %d", amount, current)
	}
	state.Placements[tenantID][nodeID] -= amount
	node := state.Nodes[nodeID]
	node.Used -= amount
	state.Nodes[nodeID] = node
	if updateTenant {
		tenant := state.Tenants[tenantID]
		if tenant.Used < amount {
			return domain.NewBizError(domain.EReleaseExceedsUsed, "$.tenants."+tenantID, "release exceeds used")
		}
		tenant.Used -= amount
		state.Tenants[tenantID] = tenant
	}
	return nil
}

func (e *Engine) reserveBlockers(state *State, requester, nodeID string, nodeScoped bool) []string {
	type candidate struct {
		id       string
		priority int
	}
	var candidates []candidate
	for _, tenant := range e.project.Tenants {
		if tenant.ID == requester {
			continue
		}
		if nodeScoped && !allowedOn(e.project, tenant.ID, nodeID) {
			continue
		}
		if state.Tenants[tenant.ID].Reserve-state.virtual(tenant.ID, nodeID) > 0 {
			candidates = append(candidates, candidate{id: tenant.ID, priority: tenant.Priority})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].priority != candidates[j].priority {
			return candidates[i].priority > candidates[j].priority
		}
		return candidates[i].id < candidates[j].id
	})
	out := make([]string, 0, len(candidates))
	for _, item := range candidates {
		out = append(out, item.id)
	}
	return out
}

func (e *Engine) nodeUsers(state *State, requester, nodeID string) []string {
	type candidate struct {
		id   string
		used int64
	}
	var candidates []candidate
	for _, tenant := range e.project.Tenants {
		if tenant.ID == requester {
			continue
		}
		if used := state.virtual(tenant.ID, nodeID); used > 0 {
			candidates = append(candidates, candidate{id: tenant.ID, used: used})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].used != candidates[j].used {
			return candidates[i].used > candidates[j].used
		}
		return candidates[i].id < candidates[j].id
	})
	out := make([]string, 0, len(candidates))
	for _, item := range candidates {
		out = append(out, item.id)
	}
	return out
}

func allowedOn(project *domain.Project, tenantID, nodeID string) bool {
	node, ok := project.NodeByID(nodeID)
	if !ok {
		return false
	}
	for _, allowed := range node.AllowedTenants {
		if allowed == tenantID {
			return true
		}
	}
	return false
}
