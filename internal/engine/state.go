package engine

import (
	"sort"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/model"
)

// TenantState is a deep-copyable tenant ledger state.
type TenantState struct {
	Tenant      model.Tenant        `json:"tenant"`
	Weight      int64               `json:"weight"`
	Used        int64               `json:"used"`
	Held        int64               `json:"held"`
	QueuedBytes int64               `json:"queued_bytes"`
	Queue       []model.QueueMember `json:"queue"`
}

// NodeState is a deep-copyable node occupancy state.
type NodeState struct {
	Node       model.Node       `json:"node"`
	Used       int64            `json:"used"`
	Held       int64            `json:"held"`
	TenantUsed map[string]int64 `json:"tenant_used"`
}

// Lease is the admitted/released accounting for one request id.
type Lease struct {
	RequestID  string `json:"request_id"`
	TenantID   string `json:"tenant_id"`
	NodeID     string `json:"node_id"`
	AskedBytes int64  `json:"asked_bytes"`
	Admitted   int64  `json:"admitted"`
	Released   int64  `json:"released"`
}

// State is the restorable engine state after one committed tick.
type State struct {
	Tick    int64                  `json:"tick"`
	Tenants map[string]TenantState `json:"tenants"`
	Nodes   map[string]NodeState   `json:"nodes"`
	Leases  map[string]Lease       `json:"leases"`
}

func (m *machine) exportState(tick int64) *State {
	st := &State{Tick: tick, Tenants: map[string]TenantState{}, Nodes: map[string]NodeState{}, Leases: map[string]Lease{}}
	for id, ts := range m.tenants {
		st.Tenants[id] = TenantState{
			Tenant: ts.tenant, Weight: ts.weight, Used: ts.used, Held: ts.held,
			QueuedBytes: ts.queuedBytes, Queue: append([]model.QueueMember(nil), ts.queue...),
		}
	}
	for id, ns := range m.nodes {
		used := map[string]int64{}
		for k, v := range ns.tenantUsed {
			used[k] = v
		}
		st.Nodes[id] = NodeState{Node: ns.node, Used: ns.used, Held: ns.held, TenantUsed: used}
	}
	for id, l := range m.leases {
		st.Leases[id] = Lease{
			RequestID: l.RequestID, TenantID: l.TenantID, NodeID: l.NodeID,
			AskedBytes: l.AskedBytes, Admitted: l.Admitted, Released: l.Released,
		}
	}
	return st
}

func (m *machine) restoreState(st *State) error {
	if st == nil {
		return nil
	}
	for id, saved := range st.Tenants {
		ts, ok := m.tenants[id]
		if !ok {
			return bizerror.New(bizerror.CodeReferenceMissing, "/checkpoint/tenants/"+id, "tenant not present in target project")
		}
		ts.tenant = saved.Tenant
		ts.weight = saved.Weight
		ts.used = saved.Used
		ts.held = saved.Held
		ts.queuedBytes = saved.QueuedBytes
		ts.queue = append([]model.QueueMember(nil), saved.Queue...)
	}
	for id, saved := range st.Nodes {
		ns, ok := m.nodes[id]
		if !ok {
			return bizerror.New(bizerror.CodeReferenceMissing, "/checkpoint/nodes/"+id, "node not present in target project")
		}
		ns.node = saved.Node
		ns.used = saved.Used
		ns.held = saved.Held
		ns.tenantUsed = map[string]int64{}
		for k, v := range saved.TenantUsed {
			ns.tenantUsed[k] = v
		}
	}
	for id, saved := range st.Leases {
		m.leases[id] = &lease{
			RequestID: saved.RequestID, TenantID: saved.TenantID, NodeID: saved.NodeID,
			AskedBytes: saved.AskedBytes, Admitted: saved.Admitted, Released: saved.Released,
		}
	}
	return nil
}

// RecomputeSuffix restores the state at fromTick-1 and recomputes only
// [fromTick, Project.LogicalTicks].
func RecomputeSuffix(project *model.Project, opts Options, fromTick int64, base *State) (*RunResult, error) {
	if project == nil {
		return nil, bizerror.New(bizerror.CodeFieldRequired, "/project", "project is nil")
	}
	if fromTick < 0 || fromTick > project.LogicalTicks {
		return nil, bizerror.New(bizerror.CodeValueOutOfRange, "/incremental/from_tick", "from_tick %d outside 0..%d", fromTick, project.LogicalTicks)
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
	if fromTick > 0 {
		if base == nil || base.Tick != fromTick-1 {
			return nil, bizerror.New(bizerror.CodeCheckpointCorrupt, "/incremental/base_state", "suffix from %d requires state at tick %d", fromTick, fromTick-1)
		}
		if err := m.restoreState(base); err != nil {
			return nil, err
		}
	}

	eventsByTick := map[int64][]model.Event{}
	for _, ev := range project.Events {
		eventsByTick[ev.Tick] = append(eventsByTick[ev.Tick], ev)
	}
	states := map[int64]*State{}
	for tick := fromTick; tick <= project.LogicalTicks; tick++ {
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
		tickReport := m.snapshot(tick, applied, tickDecisions)
		m.reports = append(m.reports, tickReport)
		states[tick] = m.exportState(tick)
	}
	report := &model.Report{
		ProjectID: project.ProjectID, Seed: project.Seed, FinalTick: project.LogicalTicks,
		Ticks: m.reports, Decisions: m.decisions, WorkerBudget: workers, BatchSize: batchSize,
	}
	return &RunResult{Report: report, States: states}, nil
}
