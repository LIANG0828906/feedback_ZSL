package gen

import (
	"fmt"
	"sort"

	"quotaengine/quota"
)

type referenceCandidate struct {
	entry       quota.QueueEntry
	isNew       bool
	guaranteed  bool
	priority    int
	guaranteeBP int64
}

type referenceState struct {
	weights   map[string]int64
	nodeUsage map[string]map[string]int64
	queue     []quota.QueueEntry
}

func buildTruth(p *quota.Project) (Truth, error) {
	state := referenceState{weights: map[string]int64{}, nodeUsage: map[string]map[string]int64{}}
	for _, t := range p.Tenants {
		state.weights[t.ID] = quota.RatioScalePermille
	}
	for _, n := range p.Nodes {
		state.nodeUsage[n.ID] = map[string]int64{}
	}
	var steps []quota.TimeStep
	for _, at := range referenceTimes(p.Events) {
		events := referenceEventsAt(p.Events, at)
		decisions := []quota.Decision{}

		for _, ev := range events {
			if ev.Type != quota.EventWeight {
				continue
			}
			t, _ := p.Tenant(ev.TenantID)
			used := referenceTenantUsed(state, t.ID)
			oldWeight := state.weights[t.ID]
			state.weights[t.ID] = ev.WeightPermille
			q, b, err := referenceHardCap(p, state, t.ID)
			if err != nil {
				return Truth{}, err
			}
			if used > q+b {
				return Truth{}, fmt.Errorf("reference weight event would exceed cap")
			}
			_ = oldWeight
			decisions = append(decisions, quota.Decision{
				Seq: ev.Seq, TimeMilli: at, Kind: string(ev.Type), Action: "applied",
				Trigger: "event", TenantID: t.ID, OrderBasis: "weighted-cap-recomputed",
				Reason: fmt.Sprintf("weight=%d", ev.WeightPermille),
			})
		}
		for _, ev := range events {
			if ev.Type != quota.EventRelease {
				continue
			}
			if ev.AmountMilli > state.nodeUsage[ev.NodeID][ev.TenantID] {
				return Truth{}, fmt.Errorf("reference release exceeds occupancy at %dms tenant=%s node=%s amount=%d used=%d",
					at, ev.TenantID, ev.NodeID, ev.AmountMilli, state.nodeUsage[ev.NodeID][ev.TenantID])
			}
			state.nodeUsage[ev.NodeID][ev.TenantID] -= ev.AmountMilli
			decisions = append(decisions, quota.Decision{
				Seq: ev.Seq, TimeMilli: at, Kind: string(ev.Type), Action: "applied",
				Trigger: "event", TenantID: ev.TenantID, NodeID: ev.NodeID,
				AmountMilli: ev.AmountMilli, OrderBasis: "release-before-admission",
				Reason: "capacity returned",
			})
		}

		cands := make([]referenceCandidate, 0, len(state.queue)+len(events))
		for _, q := range state.queue {
			cands = append(cands, referenceCandidate{entry: q})
		}
		for _, ev := range events {
			if ev.Type != quota.EventRequest {
				continue
			}
			cands = append(cands, referenceCandidate{
				entry: quota.QueueEntry{
					TenantID: ev.TenantID, NodeID: ev.NodeID, AmountMilli: ev.AmountMilli,
					Seq: ev.Seq, EnqueuedAt: at,
				},
				isNew: true,
			})
		}
		for i := range cands {
			c := &cands[i]
			t, _ := p.Tenant(c.entry.TenantID)
			g, err := referenceGuarantee(p, state, c.entry.TenantID, c.entry.NodeID)
			if err != nil {
				return Truth{}, err
			}
			own := state.nodeUsage[c.entry.NodeID][c.entry.TenantID]
			unmet := maxRef(0, g-own)
			c.guaranteed = c.entry.AmountMilli <= unmet
			c.priority = t.Priority
			c.guaranteeBP = t.GuaranteeBP
		}
		sort.SliceStable(cands, func(i, j int) bool { return referenceLess(cands[i], cands[j]) })

		for i := range cands {
			c := &cands[i]
			t, _ := p.Tenant(c.entry.TenantID)
			used := referenceTenantUsed(state, t.ID)
			q, b, err := referenceHardCap(p, state, t.ID)
			if err != nil {
				return Truth{}, err
			}
			basis := referenceBasis(c, t)
			if used+c.entry.AmountMilli > q+b {
				dec := quota.Decision{
					Seq: c.entry.Seq, TimeMilli: at, Kind: "request",
					Action: referenceAction(c.isNew, false), Trigger: referenceTrigger(c.isNew),
					TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
					AmountMilli: c.entry.AmountMilli, Constraint: quota.ConstraintTenantHardLimit,
					OrderBasis: basis,
					Reason:     fmt.Sprintf("used=%d + request=%d > hard_cap=%d", used, c.entry.AmountMilli, q+b),
				}
				if c.isNew {
					c.entry.Constraint = quota.ConstraintTenantHardLimit
					c.entry.OrderBasis = basis
					state.queue = append(state.queue, c.entry)
				}
				decisions = append(decisions, dec)
				continue
			}
			allowed, constraint, reason, err := referenceNodeAdmission(p, state, c.entry, basis)
			if err != nil {
				return Truth{}, err
			}
			if !allowed {
				dec := quota.Decision{
					Seq: c.entry.Seq, TimeMilli: at, Kind: "request",
					Action: referenceAction(c.isNew, false), Trigger: referenceTrigger(c.isNew),
					TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
					AmountMilli: c.entry.AmountMilli, Constraint: constraint,
					OrderBasis: basis, Reason: reason,
				}
				if c.isNew {
					c.entry.Constraint = constraint
					c.entry.OrderBasis = basis
					state.queue = append(state.queue, c.entry)
				} else {
					referenceUpdateQueue(&state.queue, c.entry, constraint, basis)
				}
				decisions = append(decisions, dec)
				continue
			}
			state.nodeUsage[c.entry.NodeID][c.entry.TenantID] += c.entry.AmountMilli
			if !c.isNew {
				referenceRemoveQueue(&state.queue, c.entry)
			}
			decisions = append(decisions, quota.Decision{
				Seq: c.entry.Seq, TimeMilli: at, Kind: "request",
				Action: referenceAction(c.isNew, true), Trigger: referenceTrigger(c.isNew),
				TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
				AmountMilli: c.entry.AmountMilli, OrderBasis: basis,
				Reason: "admitted within guarantee/priority order and node reservation",
			})
		}

		tenants, err := referenceLedgers(p, state)
		if err != nil {
			return Truth{}, err
		}
		nodes, err := referenceNodes(p, state)
		if err != nil {
			return Truth{}, err
		}
		committed := referenceCommitState(state, at)
		steps = append(steps, quota.TimeStep{
			TimeMilli: at, Tenants: tenants, Nodes: nodes,
			Queue:     append([]quota.QueueEntry(nil), state.queue...),
			Decisions: decisions, State: committed,
		})
	}
	return Truth{Seed: p.Seed, ToleranceMilli: quota.DefaultToleranceMilli, Steps: steps}, nil
}

func referenceTimes(events []quota.Event) []int64 {
	var out []int64
	for _, e := range events {
		if len(out) == 0 || out[len(out)-1] != e.TimeMilli {
			out = append(out, e.TimeMilli)
		}
	}
	return out
}

func referenceEventsAt(events []quota.Event, at int64) []quota.Event {
	var out []quota.Event
	for _, e := range events {
		if e.TimeMilli == at {
			out = append(out, e)
		}
	}
	return out
}

func referenceTenantUsed(state referenceState, tenantID string) int64 {
	var total int64
	for _, usage := range state.nodeUsage {
		total += usage[tenantID]
	}
	return total
}

func referenceNodeUsed(state referenceState, nodeID string) int64 {
	var total int64
	for _, amount := range state.nodeUsage[nodeID] {
		total += amount
	}
	return total
}

func referenceQuota(p *quota.Project, state referenceState, tenantID, nodeID string) (int64, error) {
	t, _ := p.Tenant(tenantID)
	n, _ := p.Node(nodeID)
	c, err := quota.QuotaForNode(n.CapacityMilli, t.QuotaBP, state.weights[tenantID], "reference.quota")
	return c.Result, err
}

func referenceBurst(p *quota.Project, state referenceState, tenantID, nodeID string) (int64, error) {
	t, _ := p.Tenant(tenantID)
	n, _ := p.Node(nodeID)
	c, err := quota.QuotaForNode(n.CapacityMilli, t.BurstBP, state.weights[tenantID], "reference.burst")
	return c.Result, err
}

func referenceGuarantee(p *quota.Project, state referenceState, tenantID, nodeID string) (int64, error) {
	t, _ := p.Tenant(tenantID)
	n, _ := p.Node(nodeID)
	c, err := quota.QuotaForNode(n.CapacityMilli, t.GuaranteeBP, state.weights[tenantID], "reference.guarantee")
	return c.Result, err
}

func referenceHardCap(p *quota.Project, state referenceState, tenantID string) (int64, int64, error) {
	var q, b int64
	for _, n := range p.Nodes {
		if !referenceContains(n.AllowedTenants, tenantID) {
			continue
		}
		qv, err := referenceQuota(p, state, tenantID, n.ID)
		if err != nil {
			return 0, 0, err
		}
		bv, err := referenceBurst(p, state, tenantID, n.ID)
		if err != nil {
			return 0, 0, err
		}
		q += qv
		b += bv
	}
	return q, b, nil
}

func referenceNodeAdmission(p *quota.Project, state referenceState, entry quota.QueueEntry, basis string) (bool, string, string, error) {
	n, _ := p.Node(entry.NodeID)
	nodeUsed := referenceNodeUsed(state, entry.NodeID)
	if nodeUsed+entry.AmountMilli > n.CapacityMilli {
		return false, quota.ConstraintNodeCapacity,
			fmt.Sprintf("node_used=%d + request=%d > capacity=%d", nodeUsed, entry.AmountMilli, n.CapacityMilli), nil
	}
	ownG, err := referenceGuarantee(p, state, entry.TenantID, entry.NodeID)
	if err != nil {
		return false, "", "", err
	}
	ownUsed := state.nodeUsage[entry.NodeID][entry.TenantID]
	ownUnmet := maxRef(0, ownG-ownUsed)
	if entry.AmountMilli <= ownUnmet {
		return true, "", "request fits inside the tenant's own unused guarantee", nil
	}
	var reserved int64
	for _, tenantID := range n.AllowedTenants {
		if tenantID == entry.TenantID {
			continue
		}
		g, err := referenceGuarantee(p, state, tenantID, entry.NodeID)
		if err != nil {
			return false, "", "", err
		}
		used := state.nodeUsage[entry.NodeID][tenantID]
		reserved += maxRef(0, g-used)
	}
	if nodeUsed+entry.AmountMilli+reserved > n.CapacityMilli {
		return false, quota.ConstraintGuaranteeReserve,
			fmt.Sprintf("node_used=%d + request=%d + other_unused_guarantees=%d > capacity=%d",
				nodeUsed, entry.AmountMilli, reserved, n.CapacityMilli), nil
	}
	return true, "", "request uses burst capacity after preserving all other guarantees; " + basis, nil
}

func referenceLess(a, b referenceCandidate) bool {
	if a.guaranteed != b.guaranteed {
		return a.guaranteed
	}
	if a.priority != b.priority {
		return a.priority > b.priority
	}
	if a.guaranteeBP != b.guaranteeBP {
		return a.guaranteeBP > b.guaranteeBP
	}
	if a.entry.TenantID != b.entry.TenantID {
		return a.entry.TenantID < b.entry.TenantID
	}
	if a.entry.EnqueuedAt != b.entry.EnqueuedAt {
		return a.entry.EnqueuedAt < b.entry.EnqueuedAt
	}
	return a.entry.Seq < b.entry.Seq
}

func referenceBasis(c *referenceCandidate, t quota.Tenant) string {
	class := "burst"
	if c.guaranteed {
		class = "guarantee"
	}
	return fmt.Sprintf("class=%s;priority=%d;guarantee_bp=%d;tenant=%s;seq=%d",
		class, t.Priority, t.GuaranteeBP, t.ID, c.entry.Seq)
}

func referenceTrigger(isNew bool) string {
	if isNew {
		return "new_request"
	}
	return "queue_retry"
}

func referenceAction(isNew, admitted bool) string {
	switch {
	case isNew && admitted:
		return "admitted"
	case isNew && !admitted:
		return "waiting"
	case !isNew && admitted:
		return "retried_admitted"
	default:
		return "retried_waiting"
	}
}

func referenceUpdateQueue(queue *[]quota.QueueEntry, target quota.QueueEntry, constraint, basis string) {
	for i, q := range *queue {
		if referenceQueueEqual(q, target) {
			(*queue)[i].Constraint = constraint
			(*queue)[i].OrderBasis = basis
			return
		}
	}
}

func referenceRemoveQueue(queue *[]quota.QueueEntry, target quota.QueueEntry) {
	for i, q := range *queue {
		if referenceQueueEqual(q, target) {
			*queue = append((*queue)[:i], (*queue)[i+1:]...)
			return
		}
	}
}

func referenceQueueEqual(a, b quota.QueueEntry) bool {
	return a.TenantID == b.TenantID && a.NodeID == b.NodeID &&
		a.AmountMilli == b.AmountMilli && a.Seq == b.Seq && a.EnqueuedAt == b.EnqueuedAt
}

func referenceLedgers(p *quota.Project, state referenceState) ([]quota.TenantLedger, error) {
	tenants := append([]quota.Tenant(nil), p.Tenants...)
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
	out := make([]quota.TenantLedger, 0, len(tenants))
	for _, t := range tenants {
		q, b, err := referenceHardCap(p, state, t.ID)
		if err != nil {
			return nil, err
		}
		used := referenceTenantUsed(state, t.ID)
		var guaranteeRemaining int64
		for _, n := range p.Nodes {
			if !referenceContains(n.AllowedTenants, t.ID) {
				continue
			}
			g, err := referenceGuarantee(p, state, t.ID, n.ID)
			if err != nil {
				return nil, err
			}
			guaranteeRemaining += maxRef(0, g-state.nodeUsage[n.ID][t.ID])
		}
		out = append(out, quota.TenantLedger{
			TenantID: t.ID, AvailableMilli: q + b - used, UsedMilli: used,
			GuaranteeRemainingMilli: guaranteeRemaining,
			QuotaMilli:              q, BurstMilli: b, WeightPermille: state.weights[t.ID],
		})
	}
	return out, nil
}

func referenceNodes(p *quota.Project, state referenceState) ([]quota.NodeSnapshot, error) {
	nodes := append([]quota.Node(nil), p.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	out := make([]quota.NodeSnapshot, 0, len(nodes))
	for _, n := range nodes {
		usage := make(map[string]int64, len(state.nodeUsage[n.ID]))
		for tenant, amount := range state.nodeUsage[n.ID] {
			if amount != 0 {
				usage[tenant] = amount
			}
		}
		out = append(out, quota.NodeSnapshot{
			NodeID: n.ID, RackID: n.RackID, CapacityMilli: n.CapacityMilli,
			UsedMilli: referenceNodeUsed(state, n.ID), UsedByTenant: usage,
		})
	}
	return out, nil
}

func referenceCommitState(state referenceState, at int64) quota.State {
	out := quota.State{
		Committed: true, CommittedTimeMilli: at,
		Weights: make(map[string]int64, len(state.weights)),
		Nodes:   make(map[string]map[string]int64, len(state.nodeUsage)),
		Queue:   append([]quota.QueueEntry(nil), state.queue...),
	}
	for tenant, weight := range state.weights {
		out.Weights[tenant] = weight
	}
	for node, usage := range state.nodeUsage {
		out.Nodes[node] = make(map[string]int64, len(usage))
		for tenant, amount := range usage {
			out.Nodes[node][tenant] = amount
		}
	}
	return out
}

func referenceContains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func maxRef(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
