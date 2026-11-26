package engine

import (
	"fmt"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/model"
)

func (m *machine) admit(tick int64, cand candidate, eval tenantEvaluation) (model.Decision, error) {
	ts := m.tenants[cand.tenantID]
	ns := m.nodes[cand.nodeID]
	tier := m.guaranteeTier(cand, ts)
	decision := model.Decision{
		RequestID: cand.requestID, Tick: tick, Sequence: cand.sequence,
		TenantID: cand.tenantID, NodeID: cand.nodeID, AskedBytes: cand.ask,
		BudgetMS: cand.budgetMS, LatencyMS: eval.latencyMS,
		OrderKey: fmt.Sprintf("tick=%d/tier=%d/priority=%d/weight=%d/tenant=%s/seq=%d/request=%s", tick, tier, ts.tenant.Priority, ts.weight, cand.tenantID, cand.sequence, cand.requestID),
	}

	if !eval.budgetOK {
		decision.Verdict = model.VerdictReject
		decision.ReasonCode = bizerror.CodeGateBudget
		decision.Constraint = ConstraintGateLatency
		decision.RetryTick = tick + m.project.Policy.RetryBackoffTicks
		decision.SettlementCorrection = 0
		m.removeQueueIfComplete(cand.requestID, false)
		return decision, nil
	}

	nodeFree := max64(0, ns.node.CapacityBytes-ns.used-ns.held)
	preheld := min64(cand.ask, nodeFree)
	ns.held += preheld
	ts.held += preheld
	decision.PreheldBytes = preheld

	tenantLimit := max64(0, ts.tenant.QuotaBytes+ts.tenant.BurstLimitBytes-ts.used)
	reserveOthers := m.guaranteeReserveForOthers(ns, cand.tenantID)
	guaranteeNodeLimit := max64(0, ns.node.CapacityBytes-ns.used-reserveOthers)
	admit := min64(preheld, tenantLimit)
	admit = min64(admit, guaranteeNodeLimit)

	ns.held -= preheld
	ts.held -= preheld
	decision.SettlementCorrection = admit - preheld

	ts.used += admit
	ns.used += admit
	ns.tenantUsed[cand.tenantID] += admit
	if lease := m.leases[cand.requestID]; lease != nil {
		lease.Admitted += admit
	}
	if admit > cand.ask {
		return decision, bizerror.New(bizerror.CodeValueOutOfRange, "/gate/admit", "admitted %d exceeds ask %d", admit, cand.ask)
	}
	if ts.used > ts.tenant.QuotaBytes+ts.tenant.BurstLimitBytes {
		return decision, bizerror.New(bizerror.CodeValueOutOfRange, "/tenant/used", "tenant %s crossed quota+burst", cand.tenantID)
	}
	if ns.used > ns.node.CapacityBytes {
		return decision, bizerror.New(bizerror.CodeValueOutOfRange, "/node/used", "node %s crossed capacity", cand.nodeID)
	}

	remaining := cand.ask - admit
	decision.AdmittedBytes = admit
	decision.QueuedBytes = remaining

	bindingConstraint, reason := m.binding(cand, tenantLimit, guaranteeNodeLimit, reserveOthers, nodeFree)
	if remaining > 0 {
		decision.Constraint = bindingConstraint
		decision.ReasonCode = reason
		decision.EncroacherTenantID = m.encroacher(ns, cand.tenantID)
		decision.RetryTick = m.suggestRetryTick(tick, cand.nodeID)

		if cand.fromQueue {
			m.updateQueueRemaining(cand.requestID, remaining, decision)
			decision.Verdict = model.VerdictQueue
		} else if len(ts.queue) >= m.project.Policy.TenantQueueLimit {
			decision.Verdict = model.VerdictReject
			decision.ReasonCode = bizerror.CodeQueueLimit
			decision.Constraint = ConstraintWaitQueueLimit
			decision.RetryTick = tick + m.project.Policy.RetryBackoffTicks
			decision.QueuedBytes = 0
		} else {
			decision.Verdict = model.VerdictQueue
			ts.queuedBytes += remaining
			ts.queue = append(ts.queue, model.QueueMember{
				RequestID: cand.requestID, TenantID: cand.tenantID, NodeID: cand.nodeID,
				RemainingBytes: remaining, OriginalBytes: cand.ask, EnqueuedTick: tick,
				Position: len(ts.queue) + 1, Priority: ts.tenant.Priority, Weight: ts.weight,
				GuaranteeEligible: tier < 2, EncroacherTenantID: decision.EncroacherTenantID,
				Constraint: decision.Constraint,
			})
		}
	} else {
		decision.Verdict = model.VerdictAllow
		m.removeQueueIfComplete(cand.requestID, true)
	}

	if be := m.checkGuaranteeInvariant(ns); be != nil {
		return decision, be
	}
	return decision, nil
}

func (m *machine) guaranteeReserveForOthers(ns *nodeState, requester string) int64 {
	var reserve int64
	for _, allowedID := range ns.node.AllowedTenants {
		if allowedID == requester {
			continue
		}
		ts := m.tenants[allowedID]
		usedOnNode := ns.tenantUsed[allowedID]
		shortfall := max64(0, ts.tenant.GuaranteeBytes-usedOnNode)
		reserve += shortfall
	}
	return reserve
}

func (m *machine) binding(cand candidate, tenantLimit, guaranteeNodeLimit, reserveOthers, nodeFree int64) (constraint, reason string) {
	ask := cand.ask
	switch {
	case ask > tenantLimit && tenantLimit <= guaranteeNodeLimit:
		return ConstraintTenantCeil, ReasonQuotaExceeded
	case reserveOthers > 0 && ask > guaranteeNodeLimit:
		return ConstraintGuaranteeReserve, ReasonGuaranteeProtected
	case ask > guaranteeNodeLimit:
		return ConstraintNodeCapacity, ReasonNodeCapacity
	case ask > nodeFree:
		return ConstraintNodeCapacity, ReasonNodeCapacity
	default:
		return ConstraintNodeCapacity, ReasonNodeCapacity
	}
}

func (m *machine) encroacher(ns *nodeState, requester string) string {
	bestID := ""
	bestBurst := int64(-1)
	for tenantID, usedOnNode := range ns.tenantUsed {
		if tenantID == requester || usedOnNode <= 0 {
			continue
		}
		guarantee := m.tenants[tenantID].tenant.GuaranteeBytes
		burst := max64(0, usedOnNode-guarantee)
		if burst > bestBurst || (burst == bestBurst && (bestID == "" || tenantID < bestID)) {
			bestBurst = burst
			bestID = tenantID
		}
	}
	if bestBurst <= 0 {
		return ""
	}
	return bestID
}

func (m *machine) suggestRetryTick(tick int64, nodeID string) int64 {
	retry := tick + m.project.Policy.RetryBackoffTicks
	for _, ev := range m.project.Events {
		if ev.Kind != model.EventRelease || ev.Tick <= tick || ev.Tick >= retry {
			continue
		}
		source := m.applyEvents[ev.ReleaseRequestID]
		if source.NodeID == nodeID {
			retry = ev.Tick
		}
	}
	return retry
}

func (m *machine) updateQueueRemaining(requestID string, remaining int64, decision model.Decision) {
	for _, ts := range m.tenants {
		for i := range ts.queue {
			if ts.queue[i].RequestID == requestID {
				old := ts.queue[i].RemainingBytes
				ts.queue[i].RemainingBytes = remaining
				ts.queue[i].EncroacherTenantID = decision.EncroacherTenantID
				ts.queue[i].Constraint = decision.Constraint
				ts.queuedBytes += remaining - old
				return
			}
		}
	}
}

func (m *machine) removeQueueIfComplete(requestID string, complete bool) {
	if !complete {
		return
	}
	for _, ts := range m.tenants {
		for i, q := range ts.queue {
			if q.RequestID != requestID {
				continue
			}
			ts.queuedBytes -= q.RemainingBytes
			ts.queue = append(ts.queue[:i], ts.queue[i+1:]...)
			if ts.queuedBytes < 0 {
				ts.queuedBytes = 0
			}
			return
		}
	}
}

func (m *machine) checkGuaranteeInvariant(ns *nodeState) error {
	reserve := m.guaranteeReserveForOthers(ns, "")
	free := ns.node.CapacityBytes - ns.used - ns.held
	if free < reserve {
		return bizerror.New(bizerror.CodeValueOutOfRange, "/guarantee/reserve", "node %s free %d below guarantee reserve %d", ns.node.ID, free, reserve)
	}
	return nil
}
