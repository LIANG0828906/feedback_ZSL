package engine

import (
	"sort"

	"quota-cluster/internal/model"
)

func (m *machine) snapshot(tick int64, applied []int, decisions []model.Decision) model.TickReport {
	tenantIDs := make([]string, 0, len(m.tenants))
	for id := range m.tenants {
		tenantIDs = append(tenantIDs, id)
	}
	sort.Strings(tenantIDs)

	tenantStates := map[string]model.TenantSnapshot{}
	var globalQueue []model.QueueMember
	for _, id := range tenantIDs {
		ts := m.tenants[id]
		for i := range ts.queue {
			ts.queue[i].Position = i + 1
		}
		queue := append([]model.QueueMember(nil), ts.queue...)
		globalQueue = append(globalQueue, queue...)
		quotaRemaining := ts.tenant.QuotaBytes - ts.used
		usableRemaining := max64(0, ts.tenant.QuotaBytes+ts.tenant.BurstLimitBytes-ts.used-ts.held)
		tenantStates[id] = model.TenantSnapshot{
			TenantID: id, Weight: ts.weight,
			AvailableQuotaBytes: ts.tenant.QuotaBytes,
			UsedBytes:           ts.used, QuotaRemainingBytes: quotaRemaining,
			UsableRemainingBytes:    usableRemaining,
			GuaranteeBytes:          ts.tenant.GuaranteeBytes,
			GuaranteeRemainingBytes: max64(0, ts.tenant.GuaranteeBytes-ts.used),
			BurstLimitBytes:         ts.tenant.BurstLimitBytes,
			BurstUsedBytes:          max64(0, ts.used-ts.tenant.QuotaBytes),
			HeldBytes:               ts.held, QueuedBytes: ts.queuedBytes, Queue: queue,
		}
	}
	sort.SliceStable(globalQueue, func(i, j int) bool {
		if globalQueue[i].TenantID != globalQueue[j].TenantID {
			return globalQueue[i].TenantID < globalQueue[j].TenantID
		}
		return globalQueue[i].Position < globalQueue[j].Position
	})

	nodeIDs := make([]string, 0, len(m.nodes))
	for id := range m.nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	nodeStates := map[string]model.NodeSnapshot{}
	for _, id := range nodeIDs {
		ns := m.nodes[id]
		nodeStates[id] = model.NodeSnapshot{
			NodeID: id, CapacityBytes: ns.node.CapacityBytes, UsedBytes: ns.used,
			HeldBytes: ns.held, FreeBytes: ns.node.CapacityBytes - ns.used - ns.held,
		}
	}

	return model.TickReport{
		Tick: tick, TenantStates: tenantStates, NodeStates: nodeStates,
		Decisions: append([]model.Decision(nil), decisions...), Queue: globalQueue,
		AppliedEvents: append([]int(nil), applied...),
	}
}
