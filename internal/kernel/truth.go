package kernel

import (
	"sort"
)

type workRequest struct {
	id           string
	t            Tenant
	n            Node
	requested    int64
	remaining    int64
	enqueuedTick int64
	budgetMS     int64
	isNew        bool
	hardRejected int64
	granted      int64
	preHold      int64
	reasonCode   string
	constraintID string
	invader      ID
	protectedIDs []ID
	retryTick    int64
	decisionPath string
	delayMS      int64
	withinBudget bool
}

type referenceSim struct {
	p          *Project
	budgets    Budgets
	mode       string
	pool       *WorkerPool
	used       map[ID]int64
	nodeOcc    map[ID]int64
	nodeHeld   map[ID]int64
	tenantHeld map[ID]int64
	weights    map[ID]int64
	queues     map[ID]map[string]QueueItem
	order      []AdmissionOrderEntry
	allocs     []Allocation
}

func RunReference(p *Project, budgets Budgets) (*RunReport, error) {
	r, err := runSimulation(p, budgets, "reference")
	if err != nil {
		return nil, err
	}
	r.Path = "reference"
	return r, nil
}

func runSimulation(p *Project, budgets Budgets, mode string) (*RunReport, error) {
	if err := budgets.Validate(); err != nil {
		return nil, err
	}
	proj := p.Clone()
	if err := ValidateProject(proj); err != nil {
		return nil, err
	}
	s := &referenceSim{
		p:          proj,
		budgets:    budgets,
		mode:       mode,
		used:       map[ID]int64{},
		nodeOcc:    map[ID]int64{},
		nodeHeld:   map[ID]int64{},
		tenantHeld: map[ID]int64{},
		weights:    map[ID]int64{},
		queues:     map[ID]map[string]QueueItem{},
	}
	for _, t := range proj.Tenants {
		s.used[t.ID] = 0
		s.tenantHeld[t.ID] = 0
		s.weights[t.ID] = t.WeightPermyriad
		s.queues[t.ID] = map[string]QueueItem{}
	}
	for _, n := range proj.Nodes {
		s.nodeOcc[n.ID] = 0
		s.nodeHeld[n.ID] = 0
	}
	if mode == PathParallel {
		s.pool = NewWorkerPool(budgets.Workers)
		defer s.pool.Wait()
	}

	maxTick := int64(0)
	eventsByTick := map[int64][]Event{}
	for _, e := range proj.Events {
		eventsByTick[e.Tick] = append(eventsByTick[e.Tick], e)
		if e.Tick > maxTick {
			maxTick = e.Tick
		}
	}

	report := &RunReport{ProjectID: proj.ProjectID, Seed: proj.Seed, Path: mode, Budgets: budgets}
	for tick := int64(0); tick <= maxTick; tick++ {
		s.order = nil
		s.allocs = nil
		if err := s.processTick(tick, eventsByTick[tick]); err != nil {
			return nil, err
		}
		report.Ticks = append(report.Ticks, s.snapshot(tick))
	}
	return report, nil
}

func (s *referenceSim) processTick(tick int64, events []Event) error {
	weights := make([]Event, 0)
	releases := make([]Event, 0)
	applies := make([]Event, 0)
	for _, e := range events {
		switch e.Type {
		case EventTypeWeight:
			weights = append(weights, e)
		case EventTypeRelease:
			releases = append(releases, e)
		case EventTypeApply:
			applies = append(applies, e)
		}
	}
	sortEvents(weights)
	sortEvents(releases)
	sortEvents(applies)

	for _, e := range weights {
		s.weights[e.TenantID] = e.WeightPermyriad
	}
	for _, e := range releases {
		if err := s.release(e); err != nil {
			return err
		}
	}

	work := map[string]*workRequest{}
	for tid, tenantQueue := range s.queues {
		for id, item := range tenantQueue {
			t, _ := s.p.Tenant(tid)
			n, _ := s.p.Node(item.NodeID)
			latencyPath, delay, within := s.latency(item.BudgetMS)
			w := &workRequest{
				id: id, t: t, n: n, requested: item.AmountRemaining, remaining: item.AmountRemaining,
				enqueuedTick: item.EnqueuedTick, budgetMS: item.BudgetMS, isNew: false,
				reasonCode: item.ReasonCode, constraintID: item.ConstraintID,
				invader: item.InvaderTenantID, protectedIDs: item.ProtectedTenantIDs,
				retryTick: item.RetryAfterTick, decisionPath: latencyPath, delayMS: delay, withinBudget: within,
			}
			work[id] = w
			// The item is removed conceptually while it is reconsidered. It is
			// written back if some remainder stays blocked.
			delete(s.queues[tid], id)
		}
	}

	for _, e := range applies {
		t, _ := s.p.Tenant(e.TenantID)
		n, _ := s.p.Node(e.NodeID)
		used := s.used[t.ID]
		hardCap := t.QuotaBytes + t.BurstLimitBytes
		hardHeadroom := max64(0, hardCap-used)
		hardRejected := max64(0, e.AmountBytes-hardHeadroom)
		schedulable := min64(e.AmountBytes, hardHeadroom)
		budgetMS := e.BudgetMS
		if budgetMS <= 0 {
			budgetMS = DefaultDecisionBudgetMS
		}
		latencyPath, delay, within := s.latency(budgetMS)
		w := &workRequest{
			id: e.ID, t: t, n: n, requested: e.AmountBytes, remaining: schedulable,
			enqueuedTick: tick, budgetMS: budgetMS, isNew: true, hardRejected: hardRejected,
			decisionPath: latencyPath, delayMS: delay, withinBudget: within,
		}
		if hardRejected > 0 {
			w.reasonCode = CodeHardCap
			w.constraintID = ConstraintHardCap
		}
		work[e.ID] = w
	}

	startUsed := make(map[ID]int64, len(s.used))
	for id, used := range s.used {
		startUsed[id] = used
	}
	grants, err := s.scheduleWork(tick, work)
	if err != nil {
		return err
	}
	if s.mode == PathParallel {
		if err := s.parallelSettle(startUsed, grants); err != nil {
			return err
		}
	}

	// Persist queues in a stable fairness order. Existing entries are restored
	// first; new requests then consume the deterministic per-tenant capacity.
	workList := make([]*workRequest, 0, len(work))
	for _, w := range work {
		workList = append(workList, w)
	}
	sortCandidates(workList, s)
	for _, w := range workList {
		if !w.isNew && w.remaining > 0 {
			s.queues[w.t.ID][w.id] = QueueItem{
				RequestID: w.id, TenantID: w.t.ID, NodeID: w.n.ID,
				AmountRemaining: w.remaining, EnqueuedTick: w.enqueuedTick,
				BudgetMS: w.budgetMS, RetryAfterTick: s.nextReleaseTick(tick, w),
				ReasonCode: w.reasonCode, ConstraintID: w.constraintID,
				InvaderTenantID: w.invader, ProtectedTenantIDs: append([]ID(nil), w.protectedIDs...),
			}
		}
	}

	for _, w := range workList {
		a := Allocation{
			RequestID: w.id, Tick: tick, TenantID: w.t.ID, NodeID: w.n.ID,
			RequestedBytes: w.requested, GrantedBytes: w.granted,
			RejectedBytes: w.hardRejected, Decision: DecisionAdmit, PreHoldBytes: w.preHold,
			Path: w.decisionPath, DelayMS: w.delayMS, BudgetMS: w.budgetMS,
			WithinBudget: w.withinBudget, FinalSettled: w.remaining == 0,
		}
		if w.remaining > 0 {
			if w.isNew && len(s.queues[w.t.ID]) >= s.budgets.PerTenantQueue {
				rejected := w.remaining
				w.reasonCode = CodeQueueFull
				w.constraintID = ConstraintQueueLimit
				w.retryTick = s.nextReleaseTick(tick, w)
				a.RejectedBytes += rejected
				w.remaining = 0
				a.Decision = DecisionReject
			} else {
				retryTick := s.nextReleaseTick(tick, w)
				if w.isNew {
					s.queues[w.t.ID][w.id] = QueueItem{
						RequestID: w.id, TenantID: w.t.ID, NodeID: w.n.ID,
						AmountRemaining: w.remaining, EnqueuedTick: w.enqueuedTick,
						BudgetMS: w.budgetMS, RetryAfterTick: retryTick, ReasonCode: w.reasonCode,
						ConstraintID: w.constraintID, InvaderTenantID: w.invader,
						ProtectedTenantIDs: append([]ID(nil), w.protectedIDs...),
					}
				}
				a.QueuedBytes = w.remaining
				a.RetryAfterTick = retryTick
				a.Decision = DecisionQueue
			}
		} else if w.hardRejected > 0 {
			a.Decision = DecisionReject
		}
		if w.reasonCode != "" {
			a.ReasonCode = w.reasonCode
			a.ConstraintID = w.constraintID
			a.InvaderTenantID = w.invader
			a.ProtectedTenantIDs = append([]ID(nil), w.protectedIDs...)
			if a.RetryAfterTick == 0 {
				a.RetryAfterTick = w.retryTick
			}
		}
		s.allocs = append(s.allocs, a)
	}

	sort.Slice(s.order, func(i, j int) bool { return s.order[i].Order < s.order[j].Order })
	sortAllocations(s.allocs)
	return nil
}

func (s *referenceSim) scheduleWork(tick int64, work map[string]*workRequest) (map[ID]int64, error) {
	orderIndex := 0
	grants := map[ID]int64{}
	for layer := 0; layer <= 2; layer++ {
		candidates := make([]*workRequest, 0)
		for _, w := range work {
			if w.remaining <= 0 {
				continue
			}
			if currentLayer(s.used[w.t.ID], w.t) == layer {
				candidates = append(candidates, w)
			}
		}
		sortCandidates(candidates, s)
		for _, w := range candidates {
			if w.remaining <= 0 || currentLayer(s.used[w.t.ID], w.t) != layer {
				continue
			}
			layerEnd := layerBoundary(w.t, layer)
			used := s.used[w.t.ID]
			layerHeadroom := max64(0, layerEnd-used)
			avail, _, protected, protectedIDs := s.nodeAvailability(w.t.ID, w.n.ID)
			sought := min64(w.remaining, layerHeadroom)
			grant := min64(sought, avail)
			if grant > 0 {
				preHold := grant
				hardCap := w.t.QuotaBytes + w.t.BurstLimitBytes
				if s.used[w.t.ID]+s.tenantHeld[w.t.ID]+preHold > hardCap {
					return nil, NewBizError(CodeHardCap, "pre-deduction would cross hard cap").
						WithTenant(w.t.ID).WithNode(w.n.ID).
						WithConstraint(ConstraintHardCap)
				}
				if s.mode == PathPreDeduct || s.mode == PathParallel || s.mode == "reference" {
					s.tenantHeld[w.t.ID] += preHold
					s.nodeHeld[w.n.ID] += preHold
					w.preHold += preHold
				}
				// Exact settlement converts the hold. In exact mode there was
				// no explicit hold, but the same integer transition applies.
				if s.mode == PathPreDeduct || s.mode == PathParallel || s.mode == "reference" {
					s.tenantHeld[w.t.ID] -= preHold
					s.nodeHeld[w.n.ID] -= preHold
				}
				s.used[w.t.ID] += grant
				s.nodeOcc[w.n.ID] += grant
				grants[w.t.ID] += grant
				w.remaining -= grant
				w.granted += grant
				orderIndex++
				s.order = append(s.order, AdmissionOrderEntry{
					Order: orderIndex, RequestID: w.id, Tranche: layerName(layer),
					TenantID: w.t.ID, NodeID: w.n.ID, Layer: layer,
					Requested: sought, Granted: grant,
				})
			}
			if grant < sought {
				w.invader = w.t.ID
				w.protectedIDs = protectedIDs
				if protected > 0 && avail == 0 {
					w.reasonCode = CodeGuaranteeInvasion
					w.constraintID = ConstraintGuaranteeFloor
				} else if protected > 0 && avail < sought {
					w.reasonCode = CodeGuaranteeInvasion
					w.constraintID = ConstraintGuaranteeFloor
				} else {
					w.reasonCode = CodeNodeCapacity
					w.constraintID = ConstraintNodeCapacity
				}
				w.retryTick = s.nextReleaseTick(tick, w)
			}
		}
	}

	// If a numeric edge leaves a request unscheduled without a more specific
	// reason, classify it as node capacity rather than silently dropping it.
	for _, w := range work {
		if w.remaining > 0 && w.reasonCode == "" {
			w.reasonCode = CodeNodeCapacity
			w.constraintID = ConstraintNodeCapacity
			w.retryTick = tick + 1
		}
	}
	return grants, nil
}

func (s *referenceSim) parallelSettle(startUsed map[ID]int64, grants map[ID]int64) error {
	type settleResult struct {
		tenant ID
		used   int64
	}
	tenants := make([]ID, 0, len(grants))
	for tid := range grants {
		tenants = append(tenants, tid)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i] < tenants[j] })
	results := make(chan settleResult, len(tenants))
	errs := make(chan error, len(tenants))
	for _, tid := range tenants {
		tid := tid
		grant := grants[tid]
		task := func() {
			t, ok := s.p.Tenant(tid)
			if !ok {
				errs <- NewBizError(CodeReferenceNotFound, "settlement tenant %q not found", tid).WithTenant(tid)
				return
			}
			finalUsed := startUsed[tid] + grant
			if finalUsed > t.QuotaBytes+t.BurstLimitBytes {
				errs <- NewBizError(CodeHardCap, "parallel settlement crosses hard cap").
					WithTenant(tid).WithConstraint(ConstraintHardCap)
				return
			}
			if finalUsed != s.used[tid] {
				errs <- NewBizError(CodeFieldConflict, "parallel settlement mismatch: computed %d planned %d", finalUsed, s.used[tid]).
					WithTenant(tid)
				return
			}
			results <- settleResult{tenant: tid, used: finalUsed}
		}
		if s.budgets.FailFastWorkers {
			if err := s.pool.TrySubmit(task); err != nil {
				return err
			}
		} else {
			s.pool.Submit(task)
		}
	}
	s.pool.Wait()
	close(results)
	close(errs)
	for err := range errs {
		return err
	}
	for r := range results {
		s.used[r.tenant] = r.used
	}
	return nil
}

func (s *referenceSim) release(e Event) error {
	if e.AmountBytes < 0 {
		return NewBizError(CodeNegativeAmount, "negative release").WithEvent(e.ID)
	}
	if e.AmountBytes > s.used[e.TenantID] {
		return NewBizError(CodeNegativeAmount, "release %d exceeds tenant used %d", e.AmountBytes, s.used[e.TenantID]).
			WithEvent(e.ID).WithTenant(e.TenantID).WithNode(e.NodeID)
	}
	if e.AmountBytes > s.nodeOcc[e.NodeID] {
		return NewBizError(CodeNegativeAmount, "release %d exceeds node occupied %d", e.AmountBytes, s.nodeOcc[e.NodeID]).
			WithEvent(e.ID).WithTenant(e.TenantID).WithNode(e.NodeID)
	}
	s.used[e.TenantID] -= e.AmountBytes
	s.nodeOcc[e.NodeID] -= e.AmountBytes
	return nil
}

func (s *referenceSim) nodeAvailability(tid ID, nid ID) (avail, free, protected int64, protectedIDs []ID) {
	n, _ := s.p.Node(nid)
	free = n.CapacityBytes - s.nodeOcc[nid] - s.nodeHeld[nid]
	for _, t := range s.p.Tenants {
		if t.ID == tid || t.GuaranteeNodeID != nid {
			continue
		}
		remaining := max64(0, t.GuaranteeBytes-s.used[t.ID])
		if remaining > 0 {
			protected += remaining
			protectedIDs = append(protectedIDs, t.ID)
		}
	}
	sort.Slice(protectedIDs, func(i, j int) bool { return protectedIDs[i] < protectedIDs[j] })
	avail = max64(0, free-protected)
	return avail, free, protected, protectedIDs
}

func (s *referenceSim) nextReleaseTick(tick int64, w *workRequest) int64 {
	best := int64(0)
	for _, e := range s.p.Events {
		if e.Type != EventTypeRelease || e.Tick <= tick {
			continue
		}
		match := false
		if w.reasonCode == CodeHardCap || w.constraintID == ConstraintHardCap {
			match = e.TenantID == w.t.ID
		} else {
			match = e.NodeID == w.n.ID
		}
		if match && (best == 0 || e.Tick < best) {
			best = e.Tick
		}
	}
	if best == 0 {
		return tick + 1
	}
	return best
}

func (s *referenceSim) snapshot(tick int64) TickReport {
	tr := TickReport{Tick: tick, AdmissionOrder: append([]AdmissionOrderEntry(nil), s.order...)}
	tr.Allocations = append(tr.Allocations, s.allocs...)
	for _, t := range s.p.Tenants {
		used := s.used[t.ID]
		tr.Ledgers = append(tr.Ledgers, TenantState{
			TenantID: t.ID, Tick: tick, QuotaBytes: t.QuotaBytes,
			GuaranteeBytes: t.GuaranteeBytes, BurstLimitBytes: t.BurstLimitBytes,
			UsedBytes: used, HeldBytes: s.tenantHeld[t.ID],
			AvailableQuotaBytes:     max64(0, t.QuotaBytes-used-s.tenantHeld[t.ID]),
			GuaranteeRemainingBytes: max64(0, t.GuaranteeBytes-used),
			BurstRemainingBytes:     burstRemaining(t, used),
			HardHeadroomBytes:       max64(0, t.QuotaBytes+t.BurstLimitBytes-used-s.tenantHeld[t.ID]),
			EffectivePriority:       t.Priority, EffectiveWeightBP: s.weights[t.ID],
		})
	}
	sort.Slice(tr.Ledgers, func(i, j int) bool { return tr.Ledgers[i].TenantID < tr.Ledgers[j].TenantID })
	for _, n := range s.p.Nodes {
		_, free, protectedAll, _ := s.nodeAvailability("", n.ID)
		// Empty tenant ID excludes nobody in nodeAvailability? It skips only
		// t.ID == "", so all real tenants are included. Correct for report.
		tr.Nodes = append(tr.Nodes, NodeState{
			NodeID: n.ID, Tick: tick, CapacityBytes: n.CapacityBytes,
			OccupiedBytes:           s.nodeOcc[n.ID],
			HeldBytes:               s.nodeHeld[n.ID],
			AvailableBytes:          free,
			ProtectedGuaranteeBytes: protectedAll,
		})
	}
	sort.Slice(tr.Nodes, func(i, j int) bool { return tr.Nodes[i].NodeID < tr.Nodes[j].NodeID })
	for tid, qm := range s.queues {
		for _, item := range qm {
			tr.Queues = append(tr.Queues, item)
		}
		_ = tid
	}
	sort.Slice(tr.Queues, func(i, j int) bool {
		if tr.Queues[i].EnqueuedTick != tr.Queues[j].EnqueuedTick {
			return tr.Queues[i].EnqueuedTick < tr.Queues[j].EnqueuedTick
		}
		if tr.Queues[i].TenantID != tr.Queues[j].TenantID {
			return tr.Queues[i].TenantID < tr.Queues[j].TenantID
		}
		return tr.Queues[i].RequestID < tr.Queues[j].RequestID
	})
	return tr
}

func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool { return events[i].ID < events[j].ID })
}

func sortAllocations(allocs []Allocation) {
	sort.SliceStable(allocs, func(i, j int) bool {
		if allocs[i].TenantID != allocs[j].TenantID {
			return allocs[i].TenantID < allocs[j].TenantID
		}
		return allocs[i].RequestID < allocs[j].RequestID
	})
}

func sortCandidates(candidates []*workRequest, s *referenceSim) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.t.Priority != b.t.Priority {
			return a.t.Priority > b.t.Priority
		}
		wa, wb := s.weights[a.t.ID], s.weights[b.t.ID]
		if wa != wb {
			return wa > wb
		}
		if a.t.GuaranteeBytes != b.t.GuaranteeBytes {
			return a.t.GuaranteeBytes > b.t.GuaranteeBytes
		}
		if a.t.ID != b.t.ID {
			return a.t.ID < b.t.ID
		}
		if a.enqueuedTick != b.enqueuedTick {
			return a.enqueuedTick < b.enqueuedTick
		}
		return a.id < b.id
	})
}

func currentLayer(used int64, t Tenant) int {
	switch {
	case used < t.GuaranteeBytes:
		return 0
	case used < t.QuotaBytes:
		return 1
	default:
		return 2
	}
}

func layerBoundary(t Tenant, layer int) int64 {
	switch layer {
	case 0:
		return t.GuaranteeBytes
	case 1:
		return t.QuotaBytes
	default:
		return t.QuotaBytes + t.BurstLimitBytes
	}
}

func layerName(layer int) string {
	switch layer {
	case 0:
		return "guarantee"
	case 1:
		return "quota"
	default:
		return "burst"
	}
}

func burstRemaining(t Tenant, used int64) int64 {
	burstUsed := max64(0, used-t.QuotaBytes)
	return max64(0, t.BurstLimitBytes-burstUsed)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func latencyFor(requestID string, amount int64, budgetMS int64) (path string, delayMS int64, within bool) {
	const exactCost = int64(3)
	const fastCost = int64(1)
	if budgetMS >= exactCost {
		return PathExact, exactCost, exactCost <= budgetMS
	}
	return PathPreDeduct, fastCost, fastCost <= budgetMS
}

func (s *referenceSim) latency(budgetMS int64) (path string, delayMS int64, within bool) {
	switch s.mode {
	case PathExact:
		const exactCost = int64(3)
		return PathExact, exactCost, exactCost <= budgetMS
	case PathPreDeduct:
		const fastCost = int64(1)
		return PathPreDeduct, fastCost, fastCost <= budgetMS
	case PathParallel:
		const fastCost = int64(1)
		return PathParallel, fastCost, fastCost <= budgetMS
	default:
		return latencyFor("", 0, budgetMS)
	}
}
