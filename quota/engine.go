package quota

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Options struct {
	Workers        int
	QueuePerTenant int
}

func DefaultOptions() Options {
	return Options{Workers: 1, QueuePerTenant: 64}
}

func normalizeOptions(opts Options) (Options, error) {
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if opts.QueuePerTenant == 0 {
		opts.QueuePerTenant = 64
	}
	if opts.Workers < 1 || opts.Workers > MaxWorkers {
		e := NewError(CodeInvalidBudget, "options", "workers", fmt.Sprintf("workers must be in [1,%d], got %d", MaxWorkers, opts.Workers))
		return opts, e
	}
	if opts.QueuePerTenant < 1 || opts.QueuePerTenant > MaxQueuePerTenant {
		e := NewError(CodeInvalidBudget, "options", "queue_per_tenant", fmt.Sprintf("queue limit must be in [1,%d], got %d", MaxQueuePerTenant, opts.QueuePerTenant))
		return opts, e
	}
	return opts, nil
}

type QueueEntry struct {
	TenantID    string `json:"tenant_id"`
	NodeID      string `json:"node_id"`
	AmountMilli int64  `json:"amount_millicores"`
	Constraint  string `json:"constraint"`
	OrderBasis  string `json:"order_basis"`
	Seq         int    `json:"seq"`
	EnqueuedAt  int64  `json:"enqueued_at_millis"`
}

type Decision struct {
	Seq         int    `json:"seq"`
	TimeMilli   int64  `json:"time_millis"`
	Kind        string `json:"kind"`
	Action      string `json:"action"`
	Trigger     string `json:"trigger"`
	TenantID    string `json:"tenant_id"`
	NodeID      string `json:"node_id"`
	AmountMilli int64  `json:"amount_millicores"`
	Constraint  string `json:"constraint,omitempty"`
	OrderBasis  string `json:"order_basis"`
	Reason      string `json:"reason"`
}

type Backpressure struct {
	Code      string `json:"code"`
	TimeMilli int64  `json:"time_millis"`
	TenantID  string `json:"tenant_id,omitempty"`
	Detail    string `json:"detail"`
}

type TenantLedger struct {
	TenantID                string `json:"tenant_id"`
	AvailableMilli          int64  `json:"available_millicores"`
	UsedMilli               int64  `json:"used_millicores"`
	GuaranteeRemainingMilli int64  `json:"guarantee_remaining_millicores"`
	QuotaMilli              int64  `json:"quota_millicores"`
	BurstMilli              int64  `json:"burst_millicores"`
	WeightPermille          int64  `json:"weight_permille"`
}

type NodeSnapshot struct {
	NodeID        string           `json:"node_id"`
	RackID        string           `json:"rack_id"`
	CapacityMilli int64            `json:"capacity_millicores"`
	UsedMilli     int64            `json:"used_millicores"`
	UsedByTenant  map[string]int64 `json:"used_by_tenant"`
}

type TimeStep struct {
	TimeMilli             int64            `json:"time_millis"`
	Tenants               []TenantLedger   `json:"tenants"`
	Nodes                 []NodeSnapshot   `json:"nodes"`
	Queue                 []QueueEntry     `json:"queue"`
	Decisions             []Decision       `json:"decisions"`
	Backpressure          []Backpressure   `json:"backpressure,omitempty"`
	ObservedLatencyMicros map[string]int64 `json:"observed_latency_micros,omitempty"`
	State                 State            `json:"state"`
}

type Timeline struct {
	ProjectFingerprint string     `json:"project_fingerprint"`
	Steps              []TimeStep `json:"steps"`
	FinalState         State      `json:"final_state"`
	CacheReused        bool       `json:"cache_reused,omitempty"`
}

type State struct {
	Committed          bool                        `json:"committed"`
	CommittedTimeMilli int64                       `json:"committed_time_millis"`
	Weights            map[string]int64            `json:"weights"`
	Nodes              map[string]map[string]int64 `json:"node_usage"`
	Queue              []QueueEntry                `json:"queue"`
}

func NewState(p *Project) State {
	s := State{
		Weights: make(map[string]int64, len(p.Tenants)),
		Nodes:   make(map[string]map[string]int64, len(p.Nodes)),
	}
	for _, t := range p.Tenants {
		s.Weights[t.ID] = RatioScalePermille
	}
	for _, n := range p.Nodes {
		s.Nodes[n.ID] = make(map[string]int64)
	}
	return s
}

func (s State) Clone() State {
	out := State{
		Committed:          s.Committed,
		CommittedTimeMilli: s.CommittedTimeMilli,
		Weights:            make(map[string]int64, len(s.Weights)),
		Nodes:              make(map[string]map[string]int64, len(s.Nodes)),
		Queue:              append([]QueueEntry(nil), s.Queue...),
	}
	for k, v := range s.Weights {
		out.Weights[k] = v
	}
	for node, usage := range s.Nodes {
		out.Nodes[node] = make(map[string]int64, len(usage))
		for tenant, amount := range usage {
			out.Nodes[node][tenant] = amount
		}
	}
	return out
}

func (s State) NodeUsed(node string) int64 {
	var total int64
	for _, amount := range s.Nodes[node] {
		total += amount
	}
	return total
}

func (s State) TenantUsed(tenant string) int64 {
	var total int64
	for _, usage := range s.Nodes {
		total += usage[tenant]
	}
	return total
}

// Run starts from an empty state. Resume starts after the committed logical
// time held in State and is the only entry point used by checkpoint recovery.
func Run(ctx context.Context, p *Project, opts Options) (*Timeline, error) {
	return Resume(ctx, p, opts, NewState(p))
}

func Resume(ctx context.Context, p *Project, opts Options, state State) (*Timeline, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	working := *p
	working.Events = append([]Event(nil), p.Events...)
	sortEvents(working.Events)
	p = &working
	opts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	if err := validateState(p, state); err != nil {
		return nil, err
	}
	for tenantID, count := range queueCounts(state.Queue) {
		if count > opts.QueuePerTenant {
			return nil, &BusinessError{
				Code: CodeBackpressureQueue, Op: "resume", TenantID: tenantID, TimeMilli: state.CommittedTimeMilli,
				Detail: fmt.Sprintf("restored queue length %d exceeds configured limit %d", count, opts.QueuePerTenant),
			}
		}
	}
	fp, err := p.Fingerprint()
	if err != nil {
		return nil, err
	}
	initial := state.Clone()
	var steps []TimeStep
	after := int64(-1)
	if state.Committed {
		after = state.CommittedTimeMilli
	}
	times := distinctTimes(p.Events, after)
	for _, at := range times {
		if err := ctx.Err(); err != nil {
			return &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}, err
		}
		step, next, err := advanceTime(ctx, p, opts, state, at)
		if err != nil {
			tl := &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone()}
			return tl, err
		}
		state = next
		steps = append(steps, step)
	}
	return &Timeline{ProjectFingerprint: fp, Steps: steps, FinalState: state.Clone(), CacheReused: !initial.Committed}, nil
}

func validateState(p *Project, state State) error {
	if state.Weights == nil {
		state.Weights = make(map[string]int64)
	}
	for _, t := range p.Tenants {
		w, ok := state.Weights[t.ID]
		if !ok || w <= 0 || w > MaxWeightPermille {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: t.ID,
				Detail: "checkpoint weight is missing or out of range"}
		}
	}
	for node, usage := range state.Nodes {
		n, ok := p.Node(node)
		if !ok {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", NodeID: node, Detail: "unknown node in checkpoint"}
		}
		var total int64
		for tenant, amount := range usage {
			if !contains(n.AllowedTenants, tenant) {
				return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", NodeID: node, TenantID: tenant,
					Detail: "tenant not allowed on checkpoint node"}
			}
			if amount < 0 {
				return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", NodeID: node, TenantID: tenant,
					Detail: "negative checkpoint occupancy"}
			}
			var overflow bool
			total, overflow = checkedAdd(total, amount)
			if overflow {
				return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", NodeID: node,
					Detail: "checkpoint node occupancy sum overflow"}
			}
		}
		if total > n.CapacityMilli {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", NodeID: node,
				Detail: "checkpoint occupancy exceeds node capacity"}
		}
	}
	for _, n := range p.Nodes {
		if _, ok := state.Nodes[n.ID]; !ok {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", NodeID: n.ID,
				Detail: "checkpoint is missing a project node"}
		}
	}
	for _, t := range p.Tenants {
		used, err := tenantUsedSafe(state, t.ID)
		if err != nil {
			return err
		}
		quota, burst, err := tenantHardCap(p, state, t.ID)
		if err != nil {
			return err
		}
		total, overflow := checkedAdd(quota, burst)
		if overflow || used > total {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: t.ID,
				Detail: fmt.Sprintf("tenant used=%d exceeds hard cap=%d", used, total)}
		}
	}
	if err := validateWeightedGuarantees(p, state); err != nil {
		return err
	}
	for _, q := range state.Queue {
		if q.AmountMilli <= 0 {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: q.TenantID, NodeID: q.NodeID,
				Detail: "negative or zero queued amount"}
		}
		if _, ok := p.Tenant(q.TenantID); !ok {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: q.TenantID, NodeID: q.NodeID,
				Detail: "queue references unknown tenant"}
		}
		n, ok := p.Node(q.NodeID)
		if !ok || !contains(n.AllowedTenants, q.TenantID) {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: q.TenantID, NodeID: q.NodeID,
				Detail: "queue references unknown node or tenant is not allowed"}
		}
		switch q.Constraint {
		case ConstraintTenantHardLimit, ConstraintNodeCapacity, ConstraintGuaranteeReserve:
		default:
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: q.TenantID, NodeID: q.NodeID,
				Detail: "queue has unknown constraint code"}
		}
		if q.EnqueuedAt < 0 {
			return &BusinessError{Code: CodeCheckpointInvariant, Op: "resume", TenantID: q.TenantID, NodeID: q.NodeID,
				Detail: "queue has negative enqueue time"}
		}
	}
	return nil
}

func queueCounts(queue []QueueEntry) map[string]int {
	out := make(map[string]int)
	for _, q := range queue {
		out[q.TenantID]++
	}
	return out
}

func distinctTimes(events []Event, after int64) []int64 {
	var times []int64
	for _, e := range events {
		if e.TimeMilli <= after {
			continue
		}
		if len(times) == 0 || times[len(times)-1] != e.TimeMilli {
			times = append(times, e.TimeMilli)
		}
	}
	return times
}

type candidate struct {
	entry       QueueEntry
	isNew       bool
	guaranteed  bool
	priority    int
	guaranteeBP int64
}

func advanceTime(ctx context.Context, p *Project, opts Options, state State, at int64) (TimeStep, State, error) {
	next := state.Clone()
	events := eventsAt(p.Events, at)
	decisions := []Decision{}

	for _, ev := range events {
		if ev.Type != EventWeight {
			continue
		}
		t, _ := p.Tenant(ev.TenantID)
		used, err := tenantUsedSafe(next, t.ID)
		if err != nil {
			return TimeStep{}, state, err
		}
		trial := next.Clone()
		trial.Weights[t.ID] = ev.WeightPermille
		tq, tb, err := tenantHardCap(p, trial, t.ID)
		if err != nil {
			return TimeStep{}, state, err
		}
		total, overflow := checkedAdd(tq, tb)
		if overflow || used > total {
			return TimeStep{}, state, &BusinessError{
				Code: CodeInvalidEvent, Op: "apply_weight", Path: fmt.Sprintf("event[%d]", ev.Seq),
				TenantID: t.ID, TimeMilli: at,
				Detail: fmt.Sprintf("weight would make used=%d exceed hard cap=%d", used, total),
			}
		}
		if err := validateWeightedGuarantees(p, trial); err != nil {
			return TimeStep{}, state, &BusinessError{
				Code: CodeGuaranteeOverCapacity, Op: "apply_weight", Path: fmt.Sprintf("event[%d]", ev.Seq),
				TenantID: t.ID, TimeMilli: at,
				Detail: "weighted guaranteed shares exceed node capacity: " + err.Error(),
			}
		}
		next = trial
		decisions = append(decisions, Decision{
			Seq: ev.Seq, TimeMilli: at, Kind: string(ev.Type), Action: "applied",
			Trigger: "event", TenantID: t.ID, OrderBasis: "weighted-cap-recomputed",
			Reason: fmt.Sprintf("weight=%d", ev.WeightPermille),
		})
	}

	for _, ev := range events {
		if ev.Type != EventRelease {
			continue
		}
		usage := next.Nodes[ev.NodeID]
		current := usage[ev.TenantID]
		if ev.AmountMilli > current {
			return TimeStep{}, state, &BusinessError{
				Code: CodeInvalidEvent, Op: "apply_release", Path: fmt.Sprintf("event[%d]", ev.Seq),
				TenantID: ev.TenantID, NodeID: ev.NodeID, TimeMilli: at,
				Detail: fmt.Sprintf("release %d exceeds occupancy %d", ev.AmountMilli, current),
			}
		}
		usage[ev.TenantID] = current - ev.AmountMilli
		decisions = append(decisions, Decision{
			Seq: ev.Seq, TimeMilli: at, Kind: string(ev.Type), Action: "applied",
			Trigger: "event", TenantID: ev.TenantID, NodeID: ev.NodeID,
			AmountMilli: ev.AmountMilli, OrderBasis: "release-before-admission",
			Reason: "capacity returned",
		})
	}

	cands := make([]candidate, 0, len(next.Queue)+len(events))
	for _, q := range next.Queue {
		cands = append(cands, candidate{entry: q, isNew: false})
	}
	for _, ev := range events {
		if ev.Type != EventRequest {
			continue
		}
		cands = append(cands, candidate{entry: QueueEntry{
			TenantID: ev.TenantID, NodeID: ev.NodeID, AmountMilli: ev.AmountMilli,
			Seq: ev.Seq, EnqueuedAt: at,
		}, isNew: true})
	}

	if err := rankCandidates(ctx, p, next, opts.Workers, cands); err != nil {
		return TimeStep{}, state, err
	}
	bp := []Backpressure{}
	if len(cands) > opts.Workers || len(p.Tenants) > opts.Workers {
		bp = append(bp, Backpressure{
			Code: CodeBackpressureWorker, TimeMilli: at,
			Detail: fmt.Sprintf("%d admission candidates and %d ledger tasks scheduled behind worker budget %d; deterministic order retained",
				len(cands), len(p.Tenants), opts.Workers),
		})
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return lessCandidate(cands[i], cands[j])
	})

	for i := range cands {
		if err := ctx.Err(); err != nil {
			return TimeStep{}, state, err
		}
		c := &cands[i]
		t, _ := p.Tenant(c.entry.TenantID)
		used, err := tenantUsedSafe(next, t.ID)
		if err != nil {
			return TimeStep{}, state, err
		}
		quota, burst, err := tenantHardCap(p, next, t.ID)
		if err != nil {
			return TimeStep{}, state, err
		}
		totalCap, overflow := checkedAdd(quota, burst)
		if overflow {
			return TimeStep{}, state, &BusinessError{Code: CodeOverflow, Op: "admission", TenantID: t.ID,
				Detail: "tenant hard cap overflow"}
		}
		orderBasis := candidateBasis(c, t)
		if exceedsAdd(used, c.entry.AmountMilli, totalCap) {
			decision := Decision{
				Seq: c.entry.Seq, TimeMilli: at, Kind: "request",
				Action: actionFor(c.isNew, false), Trigger: triggerFor(c.isNew),
				TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
				AmountMilli: c.entry.AmountMilli, Constraint: ConstraintTenantHardLimit,
				OrderBasis: orderBasis,
				Reason:     fmt.Sprintf("used=%d + request=%d > hard_cap=%d", used, c.entry.AmountMilli, totalCap),
			}
			if c.isNew {
				if lenEntriesFor(next.Queue, c.entry.TenantID) >= opts.QueuePerTenant {
					bp = append(bp, Backpressure{Code: CodeBackpressureQueue, TimeMilli: at, TenantID: c.entry.TenantID,
						Detail: fmt.Sprintf("queue limit %d exhausted", opts.QueuePerTenant)})
					return TimeStep{}, state, &BusinessError{
						Code: CodeBackpressureQueue, Op: "admission", TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
						TimeMilli: at, Detail: fmt.Sprintf("queue limit %d exhausted; request not admitted and this time is not committed", opts.QueuePerTenant),
					}
				}
				c.entry.Constraint = ConstraintTenantHardLimit
				c.entry.OrderBasis = orderBasis
				next.Queue = append(next.Queue, c.entry)
			}
			decisions = append(decisions, decision)
			continue
		}

		allowed, constraint, reason := evaluateNodeAdmission(p, next, c.entry, orderBasis)
		if !allowed {
			decision := Decision{
				Seq: c.entry.Seq, TimeMilli: at, Kind: "request",
				Action: actionFor(c.isNew, false), Trigger: triggerFor(c.isNew),
				TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
				AmountMilli: c.entry.AmountMilli, Constraint: constraint,
				OrderBasis: orderBasis, Reason: reason,
			}
			if c.isNew {
				if lenEntriesFor(next.Queue, c.entry.TenantID) >= opts.QueuePerTenant {
					bp = append(bp, Backpressure{Code: CodeBackpressureQueue, TimeMilli: at, TenantID: c.entry.TenantID,
						Detail: fmt.Sprintf("queue limit %d exhausted", opts.QueuePerTenant)})
					return TimeStep{}, state, &BusinessError{
						Code: CodeBackpressureQueue, Op: "admission", TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
						TimeMilli: at, Detail: fmt.Sprintf("queue limit %d exhausted; request not admitted and this time is not committed", opts.QueuePerTenant),
					}
				}
				c.entry.Constraint = constraint
				c.entry.OrderBasis = orderBasis
				next.Queue = append(next.Queue, c.entry)
			} else {
				updateQueueConstraint(&next.Queue, c.entry, constraint, orderBasis)
			}
			decisions = append(decisions, decision)
			continue
		}

		usage := next.Nodes[c.entry.NodeID]
		usage[c.entry.TenantID] += c.entry.AmountMilli
		if !c.isNew {
			removeQueueEntry(&next.Queue, c.entry)
		}
		decisions = append(decisions, Decision{
			Seq: c.entry.Seq, TimeMilli: at, Kind: "request",
			Action: actionFor(c.isNew, true), Trigger: triggerFor(c.isNew),
			TenantID: c.entry.TenantID, NodeID: c.entry.NodeID,
			AmountMilli: c.entry.AmountMilli, OrderBasis: orderBasis,
			Reason: "admitted within guarantee/priority order and node reservation",
		})
	}

	tenants, latencies, err := ledgerSnapshot(ctx, p, next, opts.Workers)
	if err != nil {
		return TimeStep{}, state, err
	}
	nodes := nodeSnapshot(p, next)
	next.Committed = true
	next.CommittedTimeMilli = at
	step := TimeStep{
		TimeMilli: at, Tenants: tenants, Nodes: nodes,
		Queue: append([]QueueEntry(nil), next.Queue...), Decisions: decisions,
		Backpressure: bp, ObservedLatencyMicros: latencies, State: next.Clone(),
	}
	return step, next, nil
}

func eventsAt(events []Event, at int64) []Event {
	var out []Event
	for _, e := range events {
		if e.TimeMilli == at {
			out = append(out, e)
		}
	}
	return out
}

func rankCandidates(ctx context.Context, p *Project, state State, workers int, cands []candidate) error {
	if len(cands) == 0 {
		return nil
	}
	if workers > len(cands) {
		workers = len(cands)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				c := &cands[idx]
				t, ok := p.Tenant(c.entry.TenantID)
				if !ok {
					errCh <- NewError(CodeInvalidEvent, "rank_candidates", "tenant", "unknown tenant "+c.entry.TenantID)
					return
				}
				g, err := effectiveGuarantee(p, state, c.entry.TenantID, c.entry.NodeID)
				if err != nil {
					errCh <- err
					return
				}
				own := state.Nodes[c.entry.NodeID][c.entry.TenantID]
				unmet := g - min64(own, g)
				c.guaranteed = c.entry.AmountMilli <= max64(0, unmet)
				c.priority = t.Priority
				c.guaranteeBP = t.GuaranteeBP
			}
		}()
	}
sendLoop:
	for i := range cands {
		select {
		case <-ctx.Done():
			break sendLoop
		case jobs <- i:
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return err
	default:
	}
	return ctx.Err()
}

func lessCandidate(a, b candidate) bool {
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

func candidateBasis(c *candidate, t Tenant) string {
	class := "burst"
	if c.guaranteed {
		class = "guarantee"
	}
	return fmt.Sprintf("class=%s;priority=%d;guarantee_bp=%d;tenant=%s;seq=%d",
		class, t.Priority, t.GuaranteeBP, t.ID, c.entry.Seq)
}

func triggerFor(isNew bool) string {
	if isNew {
		return "new_request"
	}
	return "queue_retry"
}

func actionFor(isNew, admitted bool) string {
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

func evaluateNodeAdmission(p *Project, state State, entry QueueEntry, basis string) (bool, string, string) {
	n, _ := p.Node(entry.NodeID)
	nodeUsed := state.NodeUsed(entry.NodeID)
	if exceedsAdd(nodeUsed, entry.AmountMilli, n.CapacityMilli) {
		return false, ConstraintNodeCapacity,
			fmt.Sprintf("node_used=%d + request=%d > capacity=%d", nodeUsed, entry.AmountMilli, n.CapacityMilli)
	}
	g, err := effectiveGuarantee(p, state, entry.TenantID, entry.NodeID)
	if err != nil {
		return false, ConstraintNodeCapacity, err.Error()
	}
	ownUsed := state.Nodes[entry.NodeID][entry.TenantID]
	ownUnmet := max64(0, g-ownUsed)
	if entry.AmountMilli <= ownUnmet {
		return true, "", "request fits inside the tenant's own unused guarantee"
	}
	reservedOthers, err := reservedGuarantees(p, state, entry.NodeID, entry.TenantID)
	if err != nil {
		return false, ConstraintGuaranteeReserve, err.Error()
	}
	afterRequest, overflow := checkedAdd(nodeUsed, entry.AmountMilli)
	if overflow || exceedsAdd(afterRequest, reservedOthers, n.CapacityMilli) {
		return false, ConstraintGuaranteeReserve,
			fmt.Sprintf("node_used=%d + request=%d + other_unused_guarantees=%d > capacity=%d",
				nodeUsed, entry.AmountMilli, reservedOthers, n.CapacityMilli)
	}
	return true, "", "request uses burst capacity after preserving all other guarantees; " + basis
}

func reservedGuarantees(p *Project, state State, nodeID, exceptTenant string) (int64, error) {
	n, _ := p.Node(nodeID)
	var total int64
	for _, tenantID := range n.AllowedTenants {
		if tenantID == exceptTenant {
			continue
		}
		g, err := effectiveGuarantee(p, state, tenantID, nodeID)
		if err != nil {
			return 0, err
		}
		used := state.Nodes[nodeID][tenantID]
		unmet := max64(0, g-used)
		var overflow bool
		total, overflow = checkedAdd(total, unmet)
		if overflow {
			return 0, NewError(CodeOverflow, "reserved_guarantees", "guarantee", "aggregate reservation overflow")
		}
	}
	return total, nil
}

func effectiveQuota(p *Project, state State, tenantID, nodeID string) (int64, error) {
	t, ok := p.Tenant(tenantID)
	if !ok {
		return 0, NewError(CodeInvalidEvent, "effective_quota", "tenant", "unknown tenant "+tenantID)
	}
	n, ok := p.Node(nodeID)
	if !ok {
		return 0, NewError(CodeInvalidEvent, "effective_quota", "node", "unknown node "+nodeID)
	}
	w := state.Weights[tenantID]
	c, err := QuotaForNode(n.CapacityMilli, t.QuotaBP, w, "quota.tenant="+tenantID+".node="+nodeID)
	return c.Result, err
}

func effectiveBurst(p *Project, state State, tenantID, nodeID string) (int64, error) {
	t, ok := p.Tenant(tenantID)
	if !ok {
		return 0, NewError(CodeInvalidEvent, "effective_burst", "tenant", "unknown tenant "+tenantID)
	}
	n, ok := p.Node(nodeID)
	if !ok {
		return 0, NewError(CodeInvalidEvent, "effective_burst", "node", "unknown node "+nodeID)
	}
	w := state.Weights[tenantID]
	c, err := QuotaForNode(n.CapacityMilli, t.BurstBP, w, "burst.tenant="+tenantID+".node="+nodeID)
	return c.Result, err
}

func effectiveGuarantee(p *Project, state State, tenantID, nodeID string) (int64, error) {
	t, ok := p.Tenant(tenantID)
	if !ok {
		return 0, NewError(CodeInvalidEvent, "effective_guarantee", "tenant", "unknown tenant "+tenantID)
	}
	n, ok := p.Node(nodeID)
	if !ok {
		return 0, NewError(CodeInvalidEvent, "effective_guarantee", "node", "unknown node "+nodeID)
	}
	w := state.Weights[tenantID]
	c, err := QuotaForNode(n.CapacityMilli, t.GuaranteeBP, w, "guarantee.tenant="+tenantID+".node="+nodeID)
	return c.Result, err
}

func tenantHardCap(p *Project, state State, tenantID string) (int64, int64, error) {
	var quotaTotal, burstTotal int64
	for _, n := range p.Nodes {
		if !contains(n.AllowedTenants, tenantID) {
			continue
		}
		q, err := effectiveQuota(p, state, tenantID, n.ID)
		if err != nil {
			return 0, 0, err
		}
		b, err := effectiveBurst(p, state, tenantID, n.ID)
		if err != nil {
			return 0, 0, err
		}
		if quotaTotal > maxInt64-q {
			return 0, 0, NewError(CodeOverflow, "tenant_hard_cap", "quota", "aggregate quota overflow")
		}
		if burstTotal > maxInt64-b {
			return 0, 0, NewError(CodeOverflow, "tenant_hard_cap", "burst", "aggregate burst overflow")
		}
		quotaTotal += q
		burstTotal += b
	}
	return quotaTotal, burstTotal, nil
}

func tenantUsedSafe(state State, tenantID string) (int64, error) {
	var total int64
	for _, usage := range state.Nodes {
		amount := usage[tenantID]
		var overflow bool
		total, overflow = checkedAdd(total, amount)
		if overflow {
			return 0, NewError(CodeOverflow, "tenant_used", "tenant", "aggregate tenant usage overflow")
		}
	}
	return total, nil
}

func validateWeightedGuarantees(p *Project, state State) error {
	for _, n := range p.Nodes {
		var total int64
		for _, tenantID := range n.AllowedTenants {
			g, err := effectiveGuarantee(p, state, tenantID, n.ID)
			if err != nil {
				return err
			}
			var overflow bool
			total, overflow = checkedAdd(total, g)
			if overflow {
				return NewError(CodeOverflow, "validate_guarantees", "guarantee", "aggregate guarantee overflow")
			}
		}
		if total > n.CapacityMilli {
			return &BusinessError{Code: CodeGuaranteeOverCapacity, Op: "validate_guarantees", NodeID: n.ID,
				Detail: fmt.Sprintf("weighted guarantees=%d exceed capacity=%d", total, n.CapacityMilli)}
		}
	}
	return nil
}

func lenEntriesFor(queue []QueueEntry, tenantID string) int {
	count := 0
	for _, q := range queue {
		if q.TenantID == tenantID {
			count++
		}
	}
	return count
}

func removeQueueEntry(queue *[]QueueEntry, target QueueEntry) {
	for i, q := range *queue {
		if queueIdentityEqual(q, target) {
			*queue = append((*queue)[:i], (*queue)[i+1:]...)
			return
		}
	}
}

func updateQueueConstraint(queue *[]QueueEntry, target QueueEntry, constraint, basis string) {
	for i, q := range *queue {
		if queueIdentityEqual(q, target) {
			(*queue)[i].Constraint = constraint
			(*queue)[i].OrderBasis = basis
			return
		}
	}
}

func queueIdentityEqual(a, b QueueEntry) bool {
	return a.TenantID == b.TenantID && a.NodeID == b.NodeID &&
		a.AmountMilli == b.AmountMilli && a.Seq == b.Seq && a.EnqueuedAt == b.EnqueuedAt
}

func ledgerSnapshot(ctx context.Context, p *Project, state State, workers int) ([]TenantLedger, map[string]int64, error) {
	tenants := append([]Tenant(nil), p.Tenants...)
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
	if workers > len(tenants) {
		workers = len(tenants)
	}
	if workers < 1 {
		workers = 1
	}
	type job struct {
		index int
		start time.Time
	}
	jobs := make(chan job)
	results := make([]TenantLedger, len(tenants))
	latencies := make(map[string]int64, len(tenants))
	var mu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, len(tenants))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				t := tenants[j.index]
				quota, burst, err := tenantHardCap(p, state, t.ID)
				if err != nil {
					errCh <- err
					return
				}
				used, err := tenantUsedSafe(state, t.ID)
				if err != nil {
					errCh <- err
					return
				}
				var guaranteeRemaining int64
				for _, n := range p.Nodes {
					if !contains(n.AllowedTenants, t.ID) {
						continue
					}
					g, err := effectiveGuarantee(p, state, t.ID, n.ID)
					if err != nil {
						errCh <- err
						return
					}
					nodeUsed := state.Nodes[n.ID][t.ID]
					unmet := max64(0, g-nodeUsed)
					var overflow bool
					guaranteeRemaining, overflow = checkedAdd(guaranteeRemaining, unmet)
					if overflow {
						errCh <- NewError(CodeOverflow, "ledger", "guarantee", "guarantee remaining overflow")
						return
					}
				}
				hardCap, overflow := checkedAdd(quota, burst)
				if overflow {
					errCh <- NewError(CodeOverflow, "ledger", "hard_cap", "quota plus burst overflow")
					return
				}
				available := hardCap - used
				if available < 0 {
					errCh <- &BusinessError{Code: CodeOverflow, Op: "ledger", TenantID: t.ID,
						Detail: fmt.Sprintf("used %d exceeds hard cap %d", used, hardCap)}
					return
				}
				results[j.index] = TenantLedger{
					TenantID: t.ID, AvailableMilli: available, UsedMilli: used,
					GuaranteeRemainingMilli: guaranteeRemaining,
					QuotaMilli:              quota, BurstMilli: burst, WeightPermille: state.Weights[t.ID],
				}
				elapsed := time.Since(j.start).Microseconds()
				mu.Lock()
				latencies[t.ID] = elapsed
				mu.Unlock()
			}
		}()
	}
send:
	for i := range tenants {
		select {
		case <-ctx.Done():
			break send
		case jobs <- job{index: i, start: time.Now()}:
		}
	}
	close(jobs)
	wg.Wait()
	select {
	case err := <-errCh:
		return nil, nil, err
	default:
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return results, latencies, nil
}

func nodeSnapshot(p *Project, state State) []NodeSnapshot {
	nodes := append([]Node(nil), p.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	out := make([]NodeSnapshot, 0, len(nodes))
	for _, n := range nodes {
		cp := make(map[string]int64, len(state.Nodes[n.ID]))
		for tenant, amount := range state.Nodes[n.ID] {
			if amount != 0 {
				cp[tenant] = amount
			}
		}
		out = append(out, NodeSnapshot{
			NodeID: n.ID, RackID: n.RackID, CapacityMilli: n.CapacityMilli,
			UsedMilli: state.NodeUsed(n.ID), UsedByTenant: cp,
		})
	}
	return out
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

const maxInt64 = int64(^uint64(0) >> 1)

func checkedAdd(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, true
	}
	if a > maxInt64-b {
		return maxInt64, true
	}
	return a + b, false
}

func exceedsAdd(a, b, limit int64) bool {
	if a < 0 || b < 0 || limit < 0 || a > limit {
		return true
	}
	return b > limit-a
}

// CompareTimelines ignores time observations and compares every correctness
// field: ledger quantities, queue, decisions and node occupancy.
func CompareTimelines(got, want *Timeline) error {
	if got == nil || want == nil {
		return NewError(CodeInvalidEvent, "compare", "", "nil timeline")
	}
	if len(got.Steps) != len(want.Steps) {
		return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", Detail: fmt.Sprintf("step count got=%d want=%d", len(got.Steps), len(want.Steps))}
	}
	for i := range want.Steps {
		g, w := got.Steps[i], want.Steps[i]
		if g.TimeMilli != w.TimeMilli {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli,
				Detail: fmt.Sprintf("time mismatch got=%d", g.TimeMilli)}
		}
		if len(g.Tenants) != len(w.Tenants) {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli, Detail: "tenant ledger count mismatch"}
		}
		for j := range w.Tenants {
			if !tenantLedgerEqual(g.Tenants[j], w.Tenants[j]) {
				return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli, TenantID: w.Tenants[j].TenantID,
					Detail: fmt.Sprintf("ledger got=%+v want=%+v", g.Tenants[j], w.Tenants[j])}
			}
		}
		if err := compareNodes(g.Nodes, w.Nodes, w.TimeMilli); err != nil {
			return err
		}
		if len(g.Queue) != len(w.Queue) {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli, Detail: "queue length mismatch"}
		}
		for j := range w.Queue {
			a, b := g.Queue[j], w.Queue[j]
			if !queueIdentityEqual(a, b) || a.Constraint != b.Constraint || a.OrderBasis != b.OrderBasis {
				return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli, TenantID: b.TenantID,
					Detail: fmt.Sprintf("queue got=%+v want=%+v", a, b)}
			}
		}
		if len(g.Decisions) != len(w.Decisions) {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli, Detail: "decision count mismatch"}
		}
		for j := range w.Decisions {
			if !decisionEqual(g.Decisions[j], w.Decisions[j]) {
				return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: w.TimeMilli, TenantID: w.Decisions[j].TenantID,
					Detail: fmt.Sprintf("decision got=%+v want=%+v", g.Decisions[j], w.Decisions[j])}
			}
		}
	}
	if err := compareStates(got.FinalState, want.FinalState); err != nil {
		return err
	}
	return nil
}

func tenantLedgerEqual(a, b TenantLedger) bool {
	tolerance := DefaultToleranceMilli
	return a.TenantID == b.TenantID &&
		abs64(a.AvailableMilli-b.AvailableMilli) <= tolerance &&
		abs64(a.UsedMilli-b.UsedMilli) <= tolerance &&
		abs64(a.GuaranteeRemainingMilli-b.GuaranteeRemainingMilli) <= tolerance &&
		a.QuotaMilli == b.QuotaMilli && a.BurstMilli == b.BurstMilli && a.WeightPermille == b.WeightPermille
}

func decisionEqual(a, b Decision) bool {
	return a.Seq == b.Seq && a.Kind == b.Kind && a.Action == b.Action && a.Trigger == b.Trigger &&
		a.TenantID == b.TenantID && a.NodeID == b.NodeID && a.AmountMilli == b.AmountMilli &&
		a.Constraint == b.Constraint && a.OrderBasis == b.OrderBasis && a.Reason == b.Reason
}

func compareStates(g, w State) error {
	if g.Committed != w.Committed || g.CommittedTimeMilli != w.CommittedTimeMilli {
		return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare_state",
			Detail: fmt.Sprintf("commit got=%t/%d want=%t/%d", g.Committed, g.CommittedTimeMilli, w.Committed, w.CommittedTimeMilli)}
	}
	if len(g.Weights) != len(w.Weights) || len(g.Nodes) != len(w.Nodes) || len(g.Queue) != len(w.Queue) {
		return NewError(CodePrecisionToleranceMiss, "compare_state", "state", "state collection size mismatch")
	}
	for tenant, amount := range w.Weights {
		if g.Weights[tenant] != amount {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare_state", TenantID: tenant,
				Detail: fmt.Sprintf("weight got=%d want=%d", g.Weights[tenant], amount)}
		}
	}
	for node, wantUsage := range w.Nodes {
		gotUsage := g.Nodes[node]
		if nonzeroUsageCount(gotUsage) != nonzeroUsageCount(wantUsage) {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare_state", NodeID: node,
				Detail: "node usage entry count mismatch"}
		}
		for tenant, amount := range wantUsage {
			if amount != 0 && gotUsage[tenant] != amount {
				return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare_state", NodeID: node, TenantID: tenant,
					Detail: fmt.Sprintf("node usage got=%d want=%d", gotUsage[tenant], amount)}
			}
		}
	}
	for i := range w.Queue {
		a, b := g.Queue[i], w.Queue[i]
		if !queueIdentityEqual(a, b) || a.Constraint != b.Constraint || a.OrderBasis != b.OrderBasis {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare_state", TenantID: b.TenantID,
				Detail: fmt.Sprintf("queue got=%+v want=%+v", a, b)}
		}
	}
	return nil
}

func nonzeroUsageCount(usage map[string]int64) int {
	count := 0
	for _, amount := range usage {
		if amount != 0 {
			count++
		}
	}
	return count
}

func compareNodes(g, w []NodeSnapshot, at int64) error {
	if len(g) != len(w) {
		return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: at, Detail: "node count mismatch"}
	}
	for i := range w {
		if g[i].NodeID != w[i].NodeID || g[i].UsedMilli != w[i].UsedMilli || g[i].CapacityMilli != w[i].CapacityMilli {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: at, NodeID: w[i].NodeID,
				Detail: fmt.Sprintf("node got=%+v want=%+v", g[i], w[i])}
		}
		if len(g[i].UsedByTenant) != len(w[i].UsedByTenant) {
			return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: at, NodeID: w[i].NodeID, Detail: "tenant occupancy entry mismatch"}
		}
		for tenant, amount := range w[i].UsedByTenant {
			if g[i].UsedByTenant[tenant] != amount {
				return &BusinessError{Code: CodePrecisionToleranceMiss, Op: "compare", TimeMilli: at, NodeID: w[i].NodeID, TenantID: tenant,
					Detail: fmt.Sprintf("occupancy got=%d want=%d", g[i].UsedByTenant[tenant], amount)}
			}
		}
	}
	return nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (s State) String() string {
	var b strings.Builder
	for node, usage := range s.Nodes {
		fmt.Fprintf(&b, "%s:%d;", node, sumMap(usage))
	}
	return b.String()
}

func sumMap(m map[string]int64) int64 {
	var total int64
	for _, v := range m {
		total += v
	}
	return total
}
