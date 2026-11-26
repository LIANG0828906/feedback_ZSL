package engine

import (
	"fmt"
	"sort"

	"quotakernel/fixed"
	"quotakernel/gate"
	"quotakernel/model"
)

type requestState struct {
	eventID      string
	kind         model.EventKind
	tick         model.Tick
	tenantID     string
	nodeID       string
	requested    int64
	admitted     int64
	fromQueue    bool
	enqueuedTick model.Tick
	expiresTick  model.Tick
	queueID      string
	tranches     []model.Allocation
	gateRecord   *model.GateRecord
	reserve      *model.ReserveRecord
	constraint   string
	reasonCode   string
	encroacher   string
	victim       string
}

type demand struct {
	req   *requestState
	class string
	rank  int
}

func settleTick(eng *Engine, state *State, tick model.Tick, events []model.Event) (*model.TickReport, error) {
	events = sortedEventCopy(events)
	report := &model.TickReport{
		ProjectID: eng.Project.ProjectID,
		Seed:      eng.Project.Seed,
		Tick:      tick,
	}
	depTenants := map[string]struct{}{}
	depNodes := map[string]struct{}{}
	depEvents := map[string]struct{}{}
	for _, ev := range events {
		report.CanonicalOrder = append(report.CanonicalOrder, ev.ID)
		depEvents[ev.ID] = struct{}{}
		if ev.TenantID != "" {
			depTenants[ev.TenantID] = struct{}{}
		}
		if ev.NodeID != "" {
			depNodes[ev.NodeID] = struct{}{}
		}
	}

	if err := applyWeights(state, events); err != nil {
		return nil, err
	}
	if err := applyReleases(state, events, report); err != nil {
		return nil, err
	}

	requests := map[string]*requestState{}
	order := []string{}

	// Existing waits are retried before new events are gated, but final
	// allocation order is always recomputed by priority/class below.
	for i := range state.Queue {
		item := state.Queue[i]
		depTenants[item.TenantID] = struct{}{}
		depNodes[item.NodeID] = struct{}{}
		depEvents[item.EventID] = struct{}{}
		if tick > item.ExpiresTick {
			report.Rejections = append(report.Rejections, model.Rejection{
				EventID: item.EventID, TenantID: item.TenantID, NodeID: item.NodeID,
				RequestedBytes: item.RemainingAmountBytes, RejectedBytes: item.RemainingAmountBytes,
				Code: model.CodeWaitTTLExceeded, Reason: "queued request exceeded declared wait TTL",
				ConstraintID: model.ConstraintWaitTTL, SuggestedRetryTick: suggestRetry(eng.Project, tick, item.TenantID, item.NodeID),
			})
			continue
		}
		req := &requestState{
			eventID: item.EventID, kind: item.Kind, tick: tick,
			tenantID: item.TenantID, nodeID: item.NodeID,
			requested: item.RemainingAmountBytes, fromQueue: true,
			enqueuedTick: item.EnqueuedTick, expiresTick: item.ExpiresTick,
			queueID: item.ID, constraint: item.ConstraintID, reasonCode: item.ReasonCode,
			encroacher: item.EncroachingTenantID, victim: item.VictimTenantID,
		}
		requests[req.eventID] = req
		order = append(order, req.eventID)
	}

	for _, ev := range events {
		if ev.Kind != model.EventApply && ev.Kind != model.EventBurst {
			continue
		}
		if _, exists := requests[ev.ID]; exists {
			continue
		}
		req, rejection, gateRecord, reserve, err := prepareNewRequest(eng, state, ev, tick)
		if err != nil {
			return nil, err
		}
		if gateRecord != nil {
			report.Gate = append(report.Gate, *gateRecord)
		}
		if rejection != nil {
			report.Rejections = append(report.Rejections, *rejection)
			if gateRecord != nil {
				updateGateFinal(report.Gate, gateRecord.DecisionID, gate.ConclusionReject, true)
			}
			continue
		}
		if req == nil {
			continue
		}
		req.gateRecord = gateRecord
		req.reserve = reserve
		if reserve != nil {
			report.Reconciliation.Records = append(report.Reconciliation.Records, *reserve)
		}
		requests[ev.ID] = req
		order = append(order, ev.ID)
		depTenants[ev.TenantID] = struct{}{}
		depNodes[req.nodeID] = struct{}{}
	}

	// Fast reserves were only O(1) provisional holds. Release them before the
	// exact canonical settlement so same-tick submission order cannot bias it.
	for _, id := range order {
		req := requests[id]
		if req.reserve != nil && req.reserve.ReservedBytes > 0 {
			node := state.Nodes[req.nodeID]
			node.PreDeductedBytes -= req.reserve.ReservedBytes
			if node.PreDeductedBytes < 0 {
				return nil, fmt.Errorf("node %s pre-deduct underflow", node.ID)
			}
		}
	}

	demands := buildDemands(state, requests, order)
	for _, d := range demands {
		allocateDemand(state, d)
	}

	nextQueue := []model.QueueItem{}
	newQueueCounts := map[string]int{}
	for _, id := range order {
		req := requests[id]
		if req == nil {
			continue
		}
		remaining := req.requested - req.admitted
		queuedBytes := int64(0)
		rejectedBytes := int64(0)
		var rejection *model.Rejection
		var queueItem *model.QueueItem

		switch {
		case remaining <= 0:
			// fully settled
		case tick >= req.expiresTick:
			rejectedBytes = remaining
			rejection = &model.Rejection{
				EventID: req.eventID, TenantID: req.tenantID, NodeID: req.nodeID,
				RequestedBytes: req.requested, RejectedBytes: remaining,
				Code: model.CodeWaitTTLExceeded, Reason: "request remained unserved through declared queue window",
				ConstraintID:        model.ConstraintWaitTTL,
				EncroachingTenantID: req.encroacher, VictimTenantID: req.victim,
				SuggestedRetryTick: suggestRetry(eng.Project, tick, req.tenantID, req.nodeID),
			}
		case !req.fromQueue && activeTenantQueueCount(state.Queue, tick, req.tenantID)+newQueueCounts[req.tenantID] >= eng.Rules.MaxQueuePerTenant:
			rejectedBytes = remaining
			rejection = &model.Rejection{
				EventID: req.eventID, TenantID: req.tenantID, NodeID: req.nodeID,
				RequestedBytes: req.requested, RejectedBytes: remaining,
				Code: model.CodeBackpressureQueueFull, Reason: "per-tenant wait queue budget exhausted",
				ConstraintID:        model.ConstraintQueueBound,
				EncroachingTenantID: req.encroacher, VictimTenantID: req.victim,
				SuggestedRetryTick: suggestRetry(eng.Project, tick, req.tenantID, req.nodeID),
			}
		default:
			queuedBytes = remaining
			if req.fromQueue {
				for i := range state.Queue {
					if state.Queue[i].EventID == req.eventID {
						item := state.Queue[i]
						item.RemainingAmountBytes = remaining
						item.LastAttemptTick = tick
						item.Priority = state.Tenants[req.tenantID].Priority
						item.Weight = state.Tenants[req.tenantID].Weight
						item.ReasonCode = req.reasonCode
						item.ConstraintID = req.constraint
						item.EncroachingTenantID = req.encroacher
						item.VictimTenantID = req.victim
						queueItem = &item
						break
					}
				}
			} else {
				t := state.Tenants[req.tenantID]
				item := model.QueueItem{
					ID: fmt.Sprintf("queue-%s", req.eventID), EventID: req.eventID, Kind: req.kind,
					TenantID: req.tenantID, NodeID: req.nodeID,
					OriginalAmountBytes: req.requested, RemainingAmountBytes: remaining,
					EnqueuedTick: tick, LastAttemptTick: tick, ExpiresTick: req.expiresTick,
					Priority: t.Priority, Weight: t.Weight, GuaranteeBytes: t.GuaranteeBytes,
					ReasonCode: req.reasonCode, ConstraintID: req.constraint,
					EncroachingTenantID: req.encroacher, VictimTenantID: req.victim,
				}
				queueItem = &item
			}
			if queueItem != nil {
				nextQueue = append(nextQueue, *queueItem)
				if !req.fromQueue {
					newQueueCounts[req.tenantID]++
				}
			}
		}

		if req.admitted > 0 {
			report.Admissions = append(report.Admissions, model.Admission{
				EventID: req.eventID, TenantID: req.tenantID, NodeID: req.nodeID, Kind: req.kind,
				RequestedBytes: req.requested, AdmittedBytes: req.admitted,
				QueuedBytes: queuedBytes, RejectedBytes: rejectedBytes,
				FullyAdmitted: remaining <= 0, Tranches: append([]model.Allocation(nil), req.tranches...),
				GateConclusion: finalGateConclusion(req, remaining),
			})
		}
		if rejection != nil {
			report.Rejections = append(report.Rejections, *rejection)
		}
		if req.gateRecord != nil {
			conclusion := gate.ConclusionAllow
			if rejection != nil {
				conclusion = gate.ConclusionReject
			} else if remaining > 0 {
				conclusion = gate.ConclusionQueue
			}
			updateGateFinal(report.Gate, req.gateRecord.DecisionID, conclusion, true)
		}
		if req.reserve != nil {
			req.reserve.SettledBytes = req.admitted
			req.reserve.DeltaBytes = req.admitted - req.reserve.ReservedBytes
			switch {
			case req.reserve.DeltaBytes == 0:
				req.reserve.Status = "matched"
			case req.reserve.DeltaBytes < 0:
				req.reserve.Status = "over_reserved_released"
			default:
				req.reserve.Status = "fast_queue_caught_up"
			}
			for i := range report.Reconciliation.Records {
				if report.Reconciliation.Records[i].DecisionID == req.reserve.DecisionID {
					report.Reconciliation.Records[i] = *req.reserve
				}
			}
		}
	}

	// Preserve TTL rejections that had no request and do not carry their queue
	// item into the next state.
	state.Queue = nextQueue
	sort.Slice(state.Queue, func(i, j int) bool {
		if state.Queue[i].EnqueuedTick != state.Queue[j].EnqueuedTick {
			return state.Queue[i].EnqueuedTick < state.Queue[j].EnqueuedTick
		}
		if state.Queue[i].Priority != state.Queue[j].Priority {
			return state.Queue[i].Priority > state.Queue[j].Priority
		}
		return state.Queue[i].ID < state.Queue[j].ID
	})

	if err := assertInvariants(state); err != nil {
		return nil, err
	}

	report.Queue = append([]model.QueueItem(nil), state.Queue...)
	report.Ledgers = ledgerSnapshots(state)
	report.Nodes = nodeSnapshots(state)
	report.Dependencies = model.Dependencies{
		TenantIDs: setKeys(depTenants), NodeIDs: setKeys(depNodes), EventIDs: setKeys(depEvents),
	}
	report.Reconciliation = finishReconciliation(report.Reconciliation, state)
	report.StateHash = state.Hash()
	sortReport(report)
	return report, nil
}

func prepareNewRequest(eng *Engine, state *State, ev model.Event, tick model.Tick) (*requestState, *model.Rejection, *model.GateRecord, *model.ReserveRecord, error) {
	tenant, ok := state.Tenants[ev.TenantID]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("event %s references unknown tenant %s", ev.ID, ev.TenantID)
	}
	nodeID := ev.NodeID
	if nodeID == "" {
		nodeID = tenant.HomeNodeID
	}
	node, ok := state.Nodes[nodeID]
	if !ok {
		return nil, nil, nil, nil, fmt.Errorf("event %s references unknown node %q", ev.ID, nodeID)
	}
	if len(node.AllowedTenants) > 0 && !contains(node.AllowedTenants, tenant.ID) {
		rejection := &model.Rejection{
			EventID: ev.ID, TenantID: tenant.ID, NodeID: nodeID, RequestedBytes: ev.AmountBytes,
			RejectedBytes: ev.AmountBytes, Code: model.CodeRejectedNode,
			Reason: "tenant is not allowed on node", ConstraintID: model.ConstraintNodeACL,
			SuggestedRetryTick: tick + 1,
		}
		decision := &model.GateRecord{DecisionID: "gate-" + ev.ID, EventID: ev.ID, Tick: tick,
			TenantID: tenant.ID, NodeID: nodeID, Conclusion: gate.ConclusionReject,
			FastConclusion: gate.ConclusionReject, ReasonCode: model.CodeRejectedNode,
			ConstraintID: model.ConstraintNodeACL, BudgetNs: eng.Rules.GateBudgetNs}
		return nil, rejection, decision, nil, nil
	}

	absoluteMax := tenant.QuotaBytes
	allowedRemaining := max64(0, tenant.QuotaBytes-tenant.QuotaUsedBytes)
	if ev.Kind == model.EventBurst {
		absoluteMax = tenant.QuotaBytes
		if tenant.BurstEnabled {
			absoluteMax += tenant.BurstCapBytes
			allowedRemaining += max64(0, tenant.BurstCapBytes-tenant.BurstUsedBytes)
		}
	}

	if ev.AmountBytes <= 0 || ev.AmountBytes > node.CapacityBytes || ev.AmountBytes > absoluteMax {
		constraint := model.ConstraintQuotaHard
		code := model.CodeRejectedQuota
		reason := "request exceeds tenant absolute quota/burst ceiling"
		if ev.AmountBytes > node.CapacityBytes {
			constraint = model.ConstraintNodeCapacity
			code = model.CodeRejectedNode
			reason = "request exceeds total node capacity"
		}
		if ev.AmountBytes <= 0 {
			reason = "request must have positive bytes"
		}
		rejection := &model.Rejection{
			EventID: ev.ID, TenantID: tenant.ID, NodeID: nodeID, RequestedBytes: ev.AmountBytes,
			RejectedBytes: ev.AmountBytes, Code: code, Reason: reason, ConstraintID: constraint,
			SuggestedRetryTick: suggestRetry(eng.Project, tick, tenant.ID, nodeID),
		}
		decision := &model.GateRecord{DecisionID: "gate-" + ev.ID, EventID: ev.ID, Tick: tick,
			TenantID: tenant.ID, NodeID: nodeID, Conclusion: gate.ConclusionReject,
			FastConclusion: gate.ConclusionReject, ReasonCode: code, ConstraintID: constraint,
			BudgetNs: eng.Rules.GateBudgetNs}
		return nil, rejection, decision, nil, nil
	}

	pinConflict := tenant.PinnedNodeID != "" && tenant.PinnedNodeID != nodeID && tenant.UsedBytes > 0
	decision := eng.gate.Decide(gate.Input{
		Tick: tick, EventID: ev.ID, TenantID: tenant.ID, NodeID: nodeID,
		RequestedBytes: ev.AmountBytes, BudgetNs: eng.Rules.GateBudgetNs,
		NodeCapacityBytes: node.CapacityBytes, NodeUsedBytes: node.UsedBytes,
		NodePreDeductedBytes:   node.PreDeductedBytes,
		TenantAllowedRemaining: allowedRemaining, TenantAbsoluteMaxBytes: absoluteMax,
	})
	record := decision.Record
	if pinConflict && record.Conclusion == gate.ConclusionAllow {
		record.Conclusion = gate.ConclusionQueue
		record.FastConclusion = gate.ConclusionQueue
		record.FastPath = false
		record.ReasonCode = "QUEUE_SINGLE_NODE_WAIT"
		record.ConstraintID = model.ConstraintSingleNode
		record.NodeReserveBytes = 0
	}

	req := &requestState{
		eventID: ev.ID, kind: ev.Kind, tick: tick, tenantID: tenant.ID, nodeID: nodeID,
		requested: ev.AmountBytes, enqueuedTick: tick,
		expiresTick: tick + tenant.MaxWaitTicks,
	}
	reserve := &model.ReserveRecord{
		DecisionID: record.DecisionID, EventID: ev.ID, Tick: tick,
		TenantID: tenant.ID, NodeID: nodeID,
	}
	if record.Conclusion == gate.ConclusionAllow {
		node.PreDeductedBytes += ev.AmountBytes
		reserve.ReservedBytes = ev.AmountBytes
		reserve.Status = "fast_held"
	} else if record.Conclusion == gate.ConclusionQueue {
		req.constraint = record.ConstraintID
		req.reasonCode = record.ReasonCode
		if pinConflict {
			req.constraint = model.ConstraintSingleNode
			req.reasonCode = "QUEUE_SINGLE_NODE_WAIT"
		}
	}
	return req, nil, &record, reserve, nil
}

func applyWeights(state *State, events []model.Event) error {
	for _, ev := range events {
		if ev.Kind != model.EventWeight {
			continue
		}
		t, ok := state.Tenants[ev.TenantID]
		if !ok {
			return fmt.Errorf("weight event %s references unknown tenant %s", ev.ID, ev.TenantID)
		}
		t.Weight = ev.NewWeight
	}
	return nil
}

func applyReleases(state *State, events []model.Event, report *model.TickReport) error {
	for _, ev := range events {
		if ev.Kind != model.EventRelease {
			continue
		}
		t, ok := state.Tenants[ev.TenantID]
		if !ok {
			return fmt.Errorf("release %s references unknown tenant %s", ev.ID, ev.TenantID)
		}
		nodeID := ev.NodeID
		if nodeID == "" {
			nodeID = t.PinnedNodeID
		}
		if nodeID == "" {
			nodeID = t.HomeNodeID
		}
		n, ok := state.Nodes[nodeID]
		if !ok {
			return fmt.Errorf("release %s references unknown node %q", ev.ID, nodeID)
		}
		if ev.AmountBytes > t.UsedBytes || ev.AmountBytes > n.UsedBytes {
			return fmt.Errorf("release %s amount %d exceeds tenant used %d/node used %d", ev.ID, ev.AmountBytes, t.UsedBytes, n.UsedBytes)
		}
		remaining := ev.AmountBytes
		burstReleased := min64(remaining, t.BurstUsedBytes)
		t.BurstUsedBytes -= burstReleased
		remaining -= burstReleased
		quotaReleased := min64(remaining, t.QuotaUsedBytes)
		t.QuotaUsedBytes -= quotaReleased
		remaining -= quotaReleased
		if remaining != 0 {
			return fmt.Errorf("release %s could not settle %d bytes", ev.ID, remaining)
		}
		t.UsedBytes -= ev.AmountBytes
		n.UsedBytes -= ev.AmountBytes
		if t.UsedBytes == 0 {
			t.PinnedNodeID = ""
		}
		report.Releases = append(report.Releases, model.ReleaseRecord{
			EventID: ev.ID, TenantID: t.ID, NodeID: n.ID, ReleasedBytes: ev.AmountBytes,
			QuotaReleasedBytes: quotaReleased, BurstReleasedBytes: burstReleased,
		})
	}
	return nil
}

func buildDemands(state *State, requests map[string]*requestState, order []string) []demand {
	demands := []demand{}
	for idx, id := range order {
		req := requests[id]
		if req == nil {
			continue
		}
		classes := []string{model.ClassGuarantee, model.ClassQuota}
		if req.kind == model.EventBurst {
			classes = append(classes, model.ClassBurst)
		}
		for _, class := range classes {
			demands = append(demands, demand{req: req, class: class, rank: idx})
		}
	}
	sort.SliceStable(demands, func(i, j int) bool {
		a, b := demands[i], demands[j]
		if classRank(a.class) != classRank(b.class) {
			return classRank(a.class) < classRank(b.class)
		}
		ta := state.Tenants[a.req.tenantID]
		tb := state.Tenants[b.req.tenantID]
		if ta.Priority != tb.Priority {
			return ta.Priority > tb.Priority
		}
		if ta.Weight != tb.Weight {
			return ta.Weight > tb.Weight
		}
		if ta.GuaranteeBytes != tb.GuaranteeBytes {
			return ta.GuaranteeBytes > tb.GuaranteeBytes
		}
		if a.req.enqueuedTick != b.req.enqueuedTick {
			return a.req.enqueuedTick < b.req.enqueuedTick
		}
		if a.req.eventID != b.req.eventID {
			return a.req.eventID < b.req.eventID
		}
		return a.rank < b.rank
	})
	return demands
}

func classRank(class string) int {
	switch class {
	case model.ClassGuarantee:
		return 0
	case model.ClassQuota:
		return 1
	default:
		return 2
	}
}

func allocateDemand(state *State, d demand) {
	req := d.req
	if req.requested-req.admitted <= 0 {
		return
	}
	tenant := state.Tenants[req.tenantID]
	node := state.Nodes[req.nodeID]
	want := req.requested - req.admitted

	if tenant.PinnedNodeID != "" && tenant.PinnedNodeID != req.nodeID {
		req.constraint = model.ConstraintSingleNode
		req.reasonCode = "QUEUE_SINGLE_NODE_WAIT"
		return
	}

	physicalAvailable := max64(0, node.CapacityBytes-node.UsedBytes-node.PreDeductedBytes)
	protected, victim := protectedGuarantee(state, req.nodeID)
	available := physicalAvailable
	if d.class != model.ClassGuarantee {
		available = min64(available, max64(0, physicalAvailable-protected))
	}

	classCapacity := int64(0)
	switch d.class {
	case model.ClassGuarantee:
		classCapacity = max64(0, tenant.GuaranteeBytes-tenant.QuotaUsedBytes)
	case model.ClassQuota:
		classCapacity = max64(0, tenant.QuotaBytes-tenant.QuotaUsedBytes)
	case model.ClassBurst:
		if tenant.BurstEnabled {
			classCapacity = max64(0, tenant.BurstCapBytes-tenant.BurstUsedBytes)
		}
	}
	amount := min64(want, min64(classCapacity, available))
	if amount <= 0 {
		classifyBlock(state, d, physicalAvailable, protected, victim, classCapacity)
		return
	}

	tenant.UsedBytes += amount
	node.UsedBytes += amount
	if d.class == model.ClassBurst {
		tenant.BurstUsedBytes += amount
	} else {
		tenant.QuotaUsedBytes += amount
	}
	if tenant.PinnedNodeID == "" {
		tenant.PinnedNodeID = req.nodeID
	}
	req.admitted += amount
	req.tranches = append(req.tranches, model.Allocation{
		Class: d.class, Bytes: amount,
		NodeUsedAfter: node.UsedBytes, TenantUsedAfter: tenant.UsedBytes,
	})

	if amount < want {
		// There may be a later class for the same request; record the reason
		// now and let a later zero/partial demand refine it. Use post-allocation
		// headroom so a protected guarantee floor is attributed correctly.
		classifyBlock(state, d, max64(0, physicalAvailable-amount), protected, victim, max64(0, classCapacity-amount))
	}
}

func classifyBlock(state *State, d demand, physicalAvailable, protected int64, victim string, classCapacity int64) {
	req := d.req
	if req.reasonCode == "GUARANTEE_BLOCKED_BY_BURST" {
		return
	}
	switch {
	case d.class == model.ClassGuarantee && classCapacity > 0 && physicalAvailable <= 0:
		req.constraint = model.ConstraintGuaranteeReserve
		req.reasonCode = "GUARANTEE_BLOCKED_BY_BURST"
		req.victim = req.tenantID
		req.encroacher = topBurstUser(state, req.nodeID, req.tenantID)
	case d.class != model.ClassGuarantee && protected > 0 && physicalAvailable-protected <= 0:
		req.constraint = model.ConstraintGuaranteeReserve
		req.reasonCode = "BURST_WOULD_ENCROACH_GUARANTEE"
		req.encroacher = req.tenantID
		req.victim = victim
	case classCapacity <= 0:
		req.constraint = model.ConstraintQuotaHard
		req.reasonCode = "TENANT_CLASS_QUOTA_EXHAUSTED"
		req.encroacher = req.tenantID
		req.victim = req.tenantID
	case physicalAvailable <= 0:
		req.constraint = model.ConstraintNodeCapacity
		req.reasonCode = "NODE_CAPACITY_WAIT"
		req.encroacher = topNodeUser(state, req.nodeID, req.tenantID)
		req.victim = req.tenantID
	default:
		req.constraint = model.ConstraintNodeCapacity
		req.reasonCode = "NODE_CAPACITY_WAIT"
		req.encroacher = ""
		req.victim = req.tenantID
	}
}

func protectedGuarantee(state *State, nodeID string) (int64, string) {
	node := state.Nodes[nodeID]
	protected := int64(0)
	victim := ""
	victimAmount := int64(-1)
	for _, t := range state.Tenants {
		eligible := false
		if len(node.AllowedTenants) > 0 {
			eligible = contains(node.AllowedTenants, t.ID)
		} else {
			eligible = t.HomeNodeID == nodeID || t.PinnedNodeID == nodeID
		}
		if !eligible {
			continue
		}
		remaining := max64(0, t.GuaranteeBytes-t.QuotaUsedBytes)
		protected += remaining
		if remaining > victimAmount || (remaining == victimAmount && t.ID < victim) {
			victimAmount = remaining
			victim = t.ID
		}
	}
	return protected, victim
}

func topBurstUser(state *State, nodeID, excludeTenant string) string {
	best := ""
	bestAmount := int64(-1)
	for _, t := range state.Tenants {
		if t.ID == excludeTenant || t.PinnedNodeID != nodeID || t.BurstUsedBytes <= 0 {
			continue
		}
		if t.BurstUsedBytes > bestAmount || (t.BurstUsedBytes == bestAmount && t.ID < best) {
			best, bestAmount = t.ID, t.BurstUsedBytes
		}
	}
	return best
}

func topNodeUser(state *State, nodeID, excludeTenant string) string {
	best := ""
	bestAmount := int64(-1)
	for _, t := range state.Tenants {
		if t.ID == excludeTenant || t.PinnedNodeID != nodeID || t.UsedBytes <= 0 {
			continue
		}
		if t.UsedBytes > bestAmount || (t.UsedBytes == bestAmount && t.ID < best) {
			best, bestAmount = t.ID, t.UsedBytes
		}
	}
	return best
}

func assertInvariants(state *State) error {
	for _, t := range state.Tenants {
		if t.QuotaUsedBytes < 0 || t.QuotaUsedBytes > t.QuotaBytes {
			return fmt.Errorf("tenant %s quota used %d outside [0,%d]", t.ID, t.QuotaUsedBytes, t.QuotaBytes)
		}
		if t.BurstUsedBytes < 0 || (t.BurstEnabled && t.BurstUsedBytes > t.BurstCapBytes) || (!t.BurstEnabled && t.BurstUsedBytes != 0) {
			return fmt.Errorf("tenant %s burst used %d invalid (enabled=%v cap=%d)", t.ID, t.BurstUsedBytes, t.BurstEnabled, t.BurstCapBytes)
		}
		if t.UsedBytes != t.QuotaUsedBytes+t.BurstUsedBytes {
			return fmt.Errorf("tenant %s ledger not balanced: used=%d quotaUsed=%d burstUsed=%d", t.ID, t.UsedBytes, t.QuotaUsedBytes, t.BurstUsedBytes)
		}
		maxUsed := t.QuotaBytes
		if t.BurstEnabled {
			maxUsed += t.BurstCapBytes
		}
		if t.UsedBytes > maxUsed {
			return fmt.Errorf("tenant %s oversold: used %d > quota+burst %d", t.ID, t.UsedBytes, maxUsed)
		}
	}
	for _, n := range state.Nodes {
		if n.UsedBytes < 0 || n.UsedBytes > n.CapacityBytes {
			return fmt.Errorf("node %s used %d outside capacity %d", n.ID, n.UsedBytes, n.CapacityBytes)
		}
		if n.PreDeductedBytes < 0 || n.UsedBytes+n.PreDeductedBytes > n.CapacityBytes {
			return fmt.Errorf("node %s pre-deduct %d plus used %d exceeds capacity", n.ID, n.PreDeductedBytes, n.UsedBytes)
		}
	}
	for _, n := range state.Nodes {
		var occupied int64
		for _, t := range state.Tenants {
			if t.PinnedNodeID == n.ID {
				occupied += t.UsedBytes
			}
		}
		if occupied != n.UsedBytes {
			return fmt.Errorf("node %s occupancy mismatch: node=%d tenants=%d", n.ID, n.UsedBytes, occupied)
		}
	}
	return nil
}

func ledgerSnapshots(state *State) []model.TenantSnapshot {
	out := make([]model.TenantSnapshot, 0, len(state.Tenants))
	for _, t := range state.Tenants {
		queueLength := 0
		for _, q := range state.Queue {
			if q.TenantID == t.ID {
				queueLength++
			}
		}
		out = append(out, model.TenantSnapshot{
			TenantID: t.ID, Priority: t.Priority, Weight: t.Weight,
			QuotaBytes: t.QuotaBytes, GuaranteeBytes: t.GuaranteeBytes,
			BurstCapBytes: t.BurstCapBytes, BurstEnabled: t.BurstEnabled,
			UsedBytes: t.UsedBytes, QuotaUsedBytes: t.QuotaUsedBytes, BurstUsedBytes: t.BurstUsedBytes,
			AvailableQuotaBytes:     max64(0, t.QuotaBytes-t.QuotaUsedBytes),
			GuaranteeRemainingBytes: max64(0, t.GuaranteeBytes-t.QuotaUsedBytes),
			BurstRemainingBytes:     max64(0, t.BurstCapBytes-t.BurstUsedBytes),
			PinnedNodeID:            t.PinnedNodeID, QueueLength: queueLength,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TenantID < out[j].TenantID })
	return out
}

func nodeSnapshots(state *State) []model.NodeSnapshot {
	out := make([]model.NodeSnapshot, 0, len(state.Nodes))
	for _, n := range state.Nodes {
		protected, _ := protectedGuarantee(state, n.ID)
		out = append(out, model.NodeSnapshot{
			NodeID: n.ID, CapacityBytes: n.CapacityBytes, UsedBytes: n.UsedBytes,
			AvailableBytes:   max64(0, n.CapacityBytes-n.UsedBytes-n.PreDeductedBytes),
			PreDeductedBytes: n.PreDeductedBytes, ProtectedGuaranteeBytes: protected,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

func finishReconciliation(in model.Reconciliation, state *State) model.Reconciliation {
	for _, r := range in.Records {
		in.TotalReserved += r.ReservedBytes
		in.TotalSettled += r.SettledBytes
		in.TotalDelta += r.DeltaBytes
	}
	in.Closed = len(state.OpenReserves) == 0
	for _, n := range state.Nodes {
		if n.PreDeductedBytes != 0 {
			in.Closed = false
		}
	}
	return in
}

func suggestRetry(project *model.Project, tick model.Tick, tenantID, nodeID string) model.Tick {
	best := tick + 1
	for _, ev := range project.Events {
		if ev.Tick <= tick {
			continue
		}
		if ev.Kind == model.EventRelease && ev.TenantID == tenantID && (nodeID == "" || ev.NodeID == nodeID) {
			return ev.Tick
		}
		if ev.Kind == model.EventWeight && ev.TenantID == tenantID && ev.Tick < best {
			best = ev.Tick
		}
	}
	return best
}

func activeTenantQueueCount(queue []model.QueueItem, tick model.Tick, tenantID string) int {
	count := 0
	for _, item := range queue {
		if item.TenantID == tenantID && tick <= item.ExpiresTick {
			count++
		}
	}
	return count
}

func finalGateConclusion(req *requestState, remaining int64) string {
	if remaining <= 0 {
		return gate.ConclusionAllow
	}
	return gate.ConclusionQueue
}

func updateGateFinal(records []model.GateRecord, decisionID, conclusion string, reconciled bool) {
	for i := range records {
		if records[i].DecisionID == decisionID {
			records[i].Conclusion = conclusion
			records[i].Reconciled = reconciled
			return
		}
	}
}

func sortReport(report *model.TickReport) {
	sort.Slice(report.Admissions, func(i, j int) bool { return report.Admissions[i].EventID < report.Admissions[j].EventID })
	sort.Slice(report.Rejections, func(i, j int) bool { return report.Rejections[i].EventID < report.Rejections[j].EventID })
	sort.Slice(report.Gate, func(i, j int) bool { return report.Gate[i].EventID < report.Gate[j].EventID })
	sort.Slice(report.Releases, func(i, j int) bool { return report.Releases[i].EventID < report.Releases[j].EventID })
}

func setKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var _ = fixed.CheckedAdd
