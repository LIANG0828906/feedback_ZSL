// Package engine advances quota ledgers and deterministic admission gates.
package engine

import (
	"sort"
	"sync"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/model"
)

const (
	ConstraintTenantCeil       = "C-TENANT-CEILING"
	ConstraintNodeCapacity     = "C-NODE-CAPACITY"
	ConstraintGuaranteeReserve = "C-GUARANTEE-RESERVE"
	ConstraintWaitQueueLimit   = "C-WAIT-QUEUE-LIMIT"
	ConstraintGateLatency      = "C-GATE-LATENCY-BUDGET"

	ReasonQuotaExceeded      = "E_QUOTA_EXCEEDED"
	ReasonNodeCapacity       = "E_NODE_CAPACITY_EXCEEDED"
	ReasonGuaranteeProtected = "E_GUARANTEE_RESERVE_PROTECTED"
)

// Options controls deterministic execution. Workers and batch size may change
// scheduling but never admission results.
type Options struct {
	Workers   int
	BatchSize int
	Edits     []model.Edit
}

type tenantState struct {
	tenant      model.Tenant
	weight      int64
	used        int64
	held        int64
	queuedBytes int64
	queue       []model.QueueMember
}

type nodeState struct {
	node       model.Node
	used       int64
	held       int64
	tenantUsed map[string]int64
}

type lease struct {
	RequestID  string
	TenantID   string
	NodeID     string
	AskedBytes int64
	Admitted   int64
	Released   int64
}

type candidate struct {
	requestID    string
	tenantID     string
	nodeID       string
	ask          int64
	sequence     int
	enqueuedTick int64
	budgetMS     int64
	fromQueue    bool
}

type tenantEvaluation struct {
	ceilingRemaining   int64
	guaranteeRemaining int64
	latencyMS          int64
	budgetOK           bool
}

type machine struct {
	project     *model.Project
	tenants     map[string]*tenantState
	nodes       map[string]*nodeState
	leases      map[string]*lease
	applyEvents map[string]model.Event
	reports     []model.TickReport
	decisions   []model.Decision
}

// Run executes all logical ticks through Project.LogicalTicks inclusively.
func Run(project *model.Project, opts Options) (*model.Report, error) {
	result, err := RunWithStates(project, opts)
	if err != nil {
		return nil, err
	}
	return result.Report, nil
}

// RunResult contains the report and restorable per-tick states.
type RunResult struct {
	Report *model.Report
	States map[int64]*State
}

// RunWithStates executes all ticks and exports a deep-copy state after each.
func RunWithStates(project *model.Project, opts Options) (*RunResult, error) {
	if project == nil {
		return nil, bizerror.New(bizerror.CodeFieldRequired, "/project", "project is nil")
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = project.Policy.WorkerBudget
	}
	if workers <= 0 {
		workers = 2
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 1
	}

	m := newMachine(project)
	states := map[int64]*State{}
	eventsByTick := map[int64][]model.Event{}
	for _, ev := range project.Events {
		eventsByTick[ev.Tick] = append(eventsByTick[ev.Tick], ev)
	}
	for tick := int64(0); tick <= project.LogicalTicks; tick++ {
		m.applyEdits(tick, opts.Edits)
		events := eventsByTick[tick]
		sort.SliceStable(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
		applied := make([]int, 0, len(events))
		var newApplies []model.Event
		for _, ev := range events {
			applied = append(applied, ev.Sequence)
			switch ev.Kind {
			case model.EventWeight:
				m.tenants[ev.TenantID].weight = ev.Weight
			case model.EventRelease:
				if err := m.release(ev); err != nil {
					return nil, err
				}
			case model.EventApply:
				newApplies = append(newApplies, ev)
			}
		}

		candidates := m.buildCandidates(tick, newApplies)
		sort.SliceStable(candidates, func(i, j int) bool {
			return m.compareCandidates(candidates[i], candidates[j]) < 0
		})
		evaluations := parallelEvaluate(m, candidates, workers, batchSize)
		tickDecisions := make([]model.Decision, 0, len(candidates))
		for i, cand := range candidates {
			decision, err := m.admit(tick, cand, evaluations[i])
			if err != nil {
				return nil, err
			}
			tickDecisions = append(tickDecisions, decision)
			m.decisions = append(m.decisions, decision)
		}
		m.reports = append(m.reports, m.snapshot(tick, applied, tickDecisions))
		states[tick] = m.exportState(tick)
	}

	report := &model.Report{
		ProjectID: project.ProjectID, Seed: project.Seed, FinalTick: project.LogicalTicks,
		Ticks: m.reports, Decisions: m.decisions, WorkerBudget: workers, BatchSize: batchSize,
	}
	return &RunResult{Report: report, States: states}, nil
}

func (m *machine) applyEdits(tick int64, edits []model.Edit) {
	for _, edit := range edits {
		if edit.AtTick != tick {
			continue
		}
		switch edit.Kind {
		case model.EditNodeCapacity:
			if ns := m.nodes[edit.TargetID]; ns != nil {
				ns.node.CapacityBytes = edit.Value
			}
		case model.EditTenantQuota:
			if ts := m.tenants[edit.TargetID]; ts != nil {
				ts.tenant.QuotaBytes = edit.Value
			}
		case model.EditTenantGuarantee:
			if ts := m.tenants[edit.TargetID]; ts != nil {
				ts.tenant.GuaranteeBytes = edit.Value
			}
		}
	}
}

func newMachine(p *model.Project) *machine {
	m := &machine{project: p, tenants: map[string]*tenantState{}, nodes: map[string]*nodeState{}, leases: map[string]*lease{}, applyEvents: map[string]model.Event{}}
	for _, t := range p.Tenants {
		m.tenants[t.ID] = &tenantState{tenant: t, weight: t.InitialWeight}
	}
	for _, n := range p.Nodes {
		m.nodes[n.ID] = &nodeState{node: n, tenantUsed: map[string]int64{}}
	}
	for _, ev := range p.Events {
		if ev.Kind == model.EventApply {
			m.applyEvents[ev.RequestID] = ev
		}
	}
	return m
}

func (m *machine) release(ev model.Event) error {
	lease := m.leases[ev.ReleaseRequestID]
	if lease == nil {
		return bizerror.New(bizerror.CodeReferenceMissing, "/events/"+ev.ReleaseRequestID, "unknown lease")
	}
	amount := ev.AmountBytes
	releasable := lease.Admitted - lease.Released
	if amount > releasable {
		return bizerror.New(bizerror.CodeValueOutOfRange, "/events/release", "release %d exceeds releasable %d for %s", amount, releasable, lease.RequestID)
	}
	ts := m.tenants[lease.TenantID]
	ns := m.nodes[lease.NodeID]
	ts.used -= amount
	ns.used -= amount
	ns.tenantUsed[lease.TenantID] -= amount
	lease.Released += amount
	if ts.used < 0 || ns.used < 0 || ns.tenantUsed[lease.TenantID] < 0 {
		return bizerror.New(bizerror.CodeValueOutOfRange, "/events/release", "release produced negative ledger state")
	}
	return nil
}

func (m *machine) buildCandidates(tick int64, applies []model.Event) []candidate {
	var out []candidate
	for _, ts := range m.tenants {
		for i := range ts.queue {
			q := ts.queue[i]
			out = append(out, candidate{requestID: q.RequestID, tenantID: q.TenantID, nodeID: q.NodeID, ask: q.RemainingBytes, sequence: queueSequence(q.RequestID), enqueuedTick: q.EnqueuedTick, budgetMS: m.project.Policy.DefaultBudgetMS, fromQueue: true})
		}
	}
	for _, ev := range applies {
		budget := ev.LatencyBudgetMS
		if budget <= 0 {
			budget = m.project.Policy.DefaultBudgetMS
		}
		out = append(out, candidate{requestID: ev.RequestID, tenantID: ev.TenantID, nodeID: ev.NodeID, ask: ev.AmountBytes, sequence: ev.Sequence, enqueuedTick: tick, budgetMS: budget})
		m.leases[ev.RequestID] = &lease{RequestID: ev.RequestID, TenantID: ev.TenantID, NodeID: ev.NodeID, AskedBytes: ev.AmountBytes}
	}
	return out
}

func queueSequence(requestID string) int {
	return 0
}

func (m *machine) compareCandidates(a, b candidate) int {
	ta, tb := m.tenants[a.tenantID], m.tenants[b.tenantID]
	ga := m.guaranteeTier(a, ta)
	gb := m.guaranteeTier(b, tb)
	if ga != gb {
		return ga - gb
	}
	if ta.tenant.Priority != tb.tenant.Priority {
		if ta.tenant.Priority > tb.tenant.Priority {
			return -1
		}
		return 1
	}
	if ta.weight != tb.weight {
		if ta.weight > tb.weight {
			return -1
		}
		return 1
	}
	if a.tenantID != b.tenantID {
		if a.tenantID < b.tenantID {
			return -1
		}
		return 1
	}
	if a.sequence != b.sequence {
		return a.sequence - b.sequence
	}
	if a.requestID < b.requestID {
		return -1
	}
	if a.requestID > b.requestID {
		return 1
	}
	return 0
}

// guaranteeTier returns 0 for fully guarantee-covered, 1 for partially covered,
// and 2 for pure burst requests.
func (m *machine) guaranteeTier(c candidate, ts *tenantState) int {
	remaining := ts.tenant.GuaranteeBytes - ts.used
	if remaining <= 0 {
		return 2
	}
	if remaining >= c.ask {
		return 0
	}
	return 1
}

func parallelEvaluate(m *machine, candidates []candidate, workers, batchSize int) []tenantEvaluation {
	results := make([]tenantEvaluation, len(candidates))
	if len(candidates) == 0 {
		return results
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for start := 0; start < len(candidates); start += batchSize {
		end := start + batchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		for i := start; i < end; i++ {
			i := i
			cand := candidates[i]
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				ts := m.tenants[cand.tenantID]
				ceiling := ts.tenant.QuotaBytes + ts.tenant.BurstLimitBytes
				results[i] = tenantEvaluation{
					ceilingRemaining:   max64(0, ceiling-ts.used-ts.held),
					guaranteeRemaining: max64(0, ts.tenant.GuaranteeBytes-ts.used),
					latencyMS:          m.project.Policy.FastCostMS + m.project.Policy.PreciseCostMS,
					budgetOK:           cand.budgetMS >= m.project.Policy.FastCostMS+m.project.Policy.PreciseCostMS,
				}
			}()
		}
		wg.Wait()
	}
	return results
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
