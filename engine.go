package orchestrator

import (
	"fmt"
	"math"
	"sort"
)

type NodeStatus string

const (
	NodeHealthy     NodeStatus = "healthy"
	NodeIsolated    NodeStatus = "isolated"
	NodeForced      NodeStatus = "forced_isolated"
	NodeUnrecovered NodeStatus = "unrecovered"
	NodeRecovered   NodeStatus = "recovered"
)

type DomainStatus string

const (
	DomainActive    DomainStatus = "active"
	DomainForced    DomainStatus = "forced"
	DomainCommitted DomainStatus = "committed"
)

type DomainView struct {
	ID              int64        `json:"id"`
	RootNodes       []string     `json:"root_nodes"`
	Nodes           []string     `json:"nodes"`
	AffectedNodes   []string     `json:"affected_nodes"`
	StartedTick     int64        `json:"started_tick"`
	TimeoutTicks    Ratio        `json:"timeout_ticks"`
	DeadlineTick    uint64       `json:"deadline_tick"`
	Status          DomainStatus `json:"status"`
	Forced          bool         `json:"forced"`
	UpgradedTick    *int64       `json:"upgraded_tick,omitempty"`
	UpgradeEvidence string       `json:"upgrade_evidence,omitempty"`
	CommittedTick   *int64       `json:"committed_tick,omitempty"`
}

type PropagationState struct {
	NodeID     string   `json:"node_id"`
	Generation int      `json:"generation"`
	Chain      []string `json:"chain"`
	Sources    []string `json:"sources"`
}

type WaitEntry struct {
	ID          string   `json:"id"`
	WorkloadID  string   `json:"workload_id"`
	NodeID      string   `json:"node_id"`
	AmountBytes int64    `json:"amount_bytes"`
	ArrivedTick int64    `json:"arrived_tick"`
	Seq         int      `json:"seq"`
	Sources     []string `json:"freeze_sources"`
}

type RequestOutcome struct {
	Tick        int64    `json:"tick"`
	Seq         int      `json:"seq"`
	WorkloadID  string   `json:"workload_id"`
	NodeID      string   `json:"node_id"`
	AmountBytes int64    `json:"amount_bytes"`
	Status      string   `json:"status"`
	Reason      string   `json:"reason"`
	Sources     []string `json:"freeze_sources,omitempty"`
}

type Unfreeze struct {
	Tick       int64    `json:"tick"`
	WorkloadID string   `json:"workload_id"`
	NodeID     string   `json:"node_id"`
	Reason     string   `json:"reason"`
	Sources    []string `json:"previous_freeze_sources"`
}

type RecoveryDecision struct {
	NodeID        string   `json:"node_id"`
	RequestedTick int64    `json:"requested_tick"`
	DecidedTick   int64    `json:"decided_tick"`
	Status        string   `json:"status"`
	Reason        string   `json:"reason"`
	Chain         []string `json:"dependency_chain,omitempty"`
	Generation    int      `json:"generation"`
}

type RecoveryGate struct {
	NodeID     string   `json:"node_id"`
	Reason     string   `json:"reason"`
	Chain      []string `json:"dependency_chain"`
	Generation int      `json:"generation"`
}

type NodeState struct {
	NodeID     string     `json:"node_id"`
	Rack       string     `json:"rack"`
	Zone       int64      `json:"zone"`
	Status     NodeStatus `json:"status"`
	Faulty     bool       `json:"faulty"`
	Isolated   bool       `json:"isolated"`
	DomainID   *int64     `json:"domain_id,omitempty"`
	Generation int        `json:"propagation_generation"`
}

type BackpressureSignal struct {
	Tick          int64   `json:"tick"`
	DomainIDs     []int64 `json:"domain_ids"`
	Layer         int     `json:"layer"`
	PendingNodes  int     `json:"pending_nodes"`
	WorkerBudget  int     `json:"worker_budget"`
	PerDomainCap  int     `json:"per_domain_pending_limit"`
	Batches       int     `json:"batches"`
	DeferredNodes int     `json:"deferred_nodes"`
}

type TickResult struct {
	Tick            int64                `json:"tick"`
	FaultyNodes     []string             `json:"faulty_nodes"`
	Domains         []DomainView         `json:"domains"`
	Propagation     []PropagationState   `json:"propagation"`
	FrozenWorkloads []string             `json:"frozen_workloads"`
	WaitingQueue    []WaitEntry          `json:"waiting_queue"`
	Unfrozen        []Unfreeze           `json:"unfrozen"`
	Requests        []RequestOutcome     `json:"requests"`
	Recoveries      []RecoveryDecision   `json:"recoveries"`
	Unrecovered     []RecoveryGate       `json:"unrecovered"`
	NodeStates      []NodeState          `json:"node_states"`
	Backpressure    []BackpressureSignal `json:"backpressure,omitempty"`
	InvalidatedBy   []string             `json:"invalidated_by,omitempty"`
}

type FinalState struct {
	LastTick      int64        `json:"last_tick"`
	Nodes         []NodeState  `json:"nodes"`
	ActiveDomains []DomainView `json:"active_domains"`
	WaitingQueue  []WaitEntry  `json:"waiting_queue"`
	Frozen        []string     `json:"frozen_workloads"`
}

type RunResult struct {
	Ticks []TickResult `json:"ticks"`
	Final FinalState   `json:"final"`
}

type SolveOptions struct {
	WorkerBudget          int
	PerDomainPendingLimit int
}

type Solver struct {
	Project               *Project
	WorkerBudget          int
	PerDomainPendingLimit int
}

func NewSolver(project *Project, options ...SolveOptions) (*Solver, error) {
	if project == nil {
		return nil, newError(CodeInvalidSchema, "$", "nil project")
	}
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	if cycles := FindDependencyCycles(clone); len(cycles) > 0 {
		return nil, cycleError(cycles)
	}
	solver := &Solver{
		Project:               clone,
		WorkerBudget:          clone.WorkerBudget,
		PerDomainPendingLimit: clone.PerDomainPendingLimit,
	}
	if len(options) > 0 {
		if options[0].WorkerBudget > 0 {
			solver.WorkerBudget = options[0].WorkerBudget
		}
		if options[0].PerDomainPendingLimit > 0 {
			solver.PerDomainPendingLimit = options[0].PerDomainPendingLimit
		}
	}
	if solver.WorkerBudget <= 0 {
		solver.WorkerBudget = 1
	}
	if solver.PerDomainPendingLimit <= 0 {
		solver.PerDomainPendingLimit = 1
	}
	return solver, nil
}

func (s *Solver) Solve() (*RunResult, error) {
	simulation, err := newSimulation(s)
	if err != nil {
		return nil, err
	}
	maxTick, err := simulation.maxEventTick()
	if err != nil {
		return nil, err
	}
	result := &RunResult{}
	for tick := int64(0); tick <= maxTick; tick++ {
		step, err := simulation.Step(tick)
		if err != nil {
			return nil, err
		}
		result.Ticks = append(result.Ticks, step)
	}
	result.Final = simulation.finalState()
	return result, nil
}

type domainState struct {
	ID              int64
	Roots           map[string]struct{}
	StartedTick     int64
	Timeout         Timeout
	DeadlineTick    uint64
	Forced          bool
	UpgradedTick    *int64
	UpgradeEvidence string
	CommittedTick   *int64
}

func (d *domainState) clone() *domainState {
	if d == nil {
		return nil
	}
	clone := *d
	clone.Roots = cloneStringSet(d.Roots)
	clone.UpgradedTick = copyInt64(d.UpgradedTick)
	clone.CommittedTick = copyInt64(d.CommittedTick)
	return &clone
}

type waitRequest struct {
	ID          string
	WorkloadID  string
	NodeID      string
	AmountBytes int64
	ArrivedTick int64
	Seq         int
	Sources     []string
}

type simState struct {
	Domains       map[int64]*domainState
	Faulty        map[string]struct{}
	Pending       map[string]int64
	Recovered     map[string]struct{}
	Used          map[string]int64
	Waiting       []waitRequest
	Frozen        map[string]struct{}
	FrozenSources map[string]map[string]struct{}
	UnfrozenAt    map[string]int64
	LastTick      int64
}

func newInitialState(project *Project) *simState {
	used := make(map[string]int64)
	for _, node := range project.Nodes {
		for _, workload := range node.Workloads {
			used[workload.ID] = workload.UsedBytes
		}
	}
	return &simState{
		Domains:       make(map[int64]*domainState),
		Faulty:        make(map[string]struct{}),
		Pending:       make(map[string]int64),
		Recovered:     make(map[string]struct{}),
		Used:          used,
		Frozen:        make(map[string]struct{}),
		FrozenSources: make(map[string]map[string]struct{}),
		UnfrozenAt:    make(map[string]int64),
		LastTick:      -1,
	}
}

func (s *simState) clone() *simState {
	if s == nil {
		return nil
	}
	clone := &simState{
		Domains:       make(map[int64]*domainState, len(s.Domains)),
		Faulty:        cloneStringSet(s.Faulty),
		Pending:       cloneInt64Map(s.Pending),
		Recovered:     cloneStringSet(s.Recovered),
		Used:          cloneInt64Map(s.Used),
		Waiting:       cloneWaiting(s.Waiting),
		Frozen:        cloneStringSet(s.Frozen),
		FrozenSources: make(map[string]map[string]struct{}, len(s.FrozenSources)),
		UnfrozenAt:    cloneInt64Map(s.UnfrozenAt),
		LastTick:      s.LastTick,
	}
	for id, domain := range s.Domains {
		clone.Domains[id] = domain.clone()
	}
	for workload, sources := range s.FrozenSources {
		clone.FrozenSources[workload] = cloneStringSet(sources)
	}
	return clone
}

type simulation struct {
	project      *Project
	state        *simState
	workers      int
	perDomainCap int
	nodeByID     map[string]Node
	workloadByID map[string]Workload
	reverse      map[string][]string
	events       map[int64][]Event
	maxTick      int64
	backpressure []BackpressureSignal
	bpSeen       map[string]struct{}
}

func newSimulation(solver *Solver) (*simulation, error) {
	return newSimulationFromState(solver, newInitialState(solver.Project))
}

func newSimulationFromState(solver *Solver, state *simState) (*simulation, error) {
	if solver == nil || solver.Project == nil {
		return nil, newError(CodeInvalidSchema, "$", "nil solver")
	}
	sim := &simulation{
		project:      solver.Project,
		state:        state,
		workers:      solver.WorkerBudget,
		perDomainCap: solver.PerDomainPendingLimit,
		nodeByID:     solver.Project.NodeIndex(),
		workloadByID: make(map[string]Workload),
		reverse:      make(map[string][]string),
		events:       make(map[int64][]Event),
		maxTick:      -1,
		bpSeen:       make(map[string]struct{}),
	}
	for _, node := range solver.Project.Nodes {
		for _, workload := range node.Workloads {
			sim.workloadByID[workload.ID] = workload
		}
	}
	for _, edge := range solver.Project.Edges {
		sim.reverse[edge.To] = append(sim.reverse[edge.To], edge.From)
	}
	for target := range sim.reverse {
		sort.Strings(sim.reverse[target])
	}
	for _, event := range solver.Project.Events {
		sim.events[event.Tick] = append(sim.events[event.Tick], event)
		if event.Tick > sim.maxTick {
			sim.maxTick = event.Tick
		}
	}
	return sim, nil
}

func (s *simulation) maxEventTick() (int64, error) {
	if s.maxTick < 0 {
		return 0, nil
	}
	return s.maxTick, nil
}

func (s *simulation) Step(tick int64) (TickResult, error) {
	if tick < 0 {
		return TickResult{}, newError(CodeInvalidEvent, "$.tick", "logical tick must be non-negative")
	}
	if s.state.LastTick >= 0 && tick != s.state.LastTick+1 {
		return TickResult{}, newError(CodeInvalidEvent, "$.tick", "ticks must be advanced exactly once: have %d, got %d", s.state.LastTick, tick)
	}
	result := TickResult{Tick: tick}
	s.backpressure = nil
	s.bpSeen = make(map[string]struct{})
	changedDomains := make(map[int64]DomainView)
	for _, event := range s.events[tick] {
		switch event.Kind {
		case EventProbe:
			if err := s.probe(event, &result, changedDomains); err != nil {
				return TickResult{}, err
			}
		case EventCommit:
			if err := s.commit(event, &result, changedDomains); err != nil {
				return TickResult{}, err
			}
		case EventRecover:
			s.state.Pending[event.NodeID] = event.Tick
		case EventRequest:
			outcome, err := s.handleRequest(event, &result)
			if err != nil {
				return TickResult{}, err
			}
			if outcome != nil {
				result.Requests = append(result.Requests, *outcome)
			}
		}
	}

	if _, err := s.refreshFreeze(tick, &result); err != nil {
		return TickResult{}, err
	}
	if err := s.applyTimeouts(tick, changedDomains); err != nil {
		return TickResult{}, err
	}
	if _, err := s.refreshFreeze(tick, &result); err != nil {
		return TickResult{}, err
	}
	s.attemptRecoveries(tick, &result)
	s.state.LastTick = tick

	propagation, err := s.propagation(tick)
	if err != nil {
		return TickResult{}, err
	}
	result.Propagation = propagation
	result.Backpressure = append([]BackpressureSignal(nil), s.backpressure...)
	result.Domains = s.domainViews(tick, changedDomains, propagation)
	result.FaultyNodes = sortedSet(s.state.Faulty)
	result.FrozenWorkloads = sortedSet(s.state.Frozen)
	result.WaitingQueue = s.waitingSnapshot()
	result.NodeStates = s.nodeStates(propagation)
	result.Unrecovered = s.recoveryGates(propagation)
	sort.Slice(result.Requests, func(i, j int) bool {
		if result.Requests[i].Tick != result.Requests[j].Tick {
			return result.Requests[i].Tick < result.Requests[j].Tick
		}
		if result.Requests[i].Seq != result.Requests[j].Seq {
			return result.Requests[i].Seq < result.Requests[j].Seq
		}
		return result.Requests[i].WorkloadID < result.Requests[j].WorkloadID
	})
	normalizeTickResult(&result)
	return result, nil
}

func (s *simulation) probe(event Event, result *TickResult, changed map[int64]DomainView) error {
	node, ok := s.nodeByID[event.NodeID]
	if !ok {
		return newError(CodeUnknownNode, "$.event.node_id", "unknown node %q", event.NodeID)
	}
	s.state.Faulty[node.ID] = struct{}{}
	delete(s.state.Recovered, node.ID)
	domain := s.state.Domains[node.Zone]
	if domain == nil {
		deadline, err := node.Timeout.Ticks.DeadlineTicks()
		if err != nil {
			return err
		}
		if event.Tick < 0 {
			return newError(CodeInvalidEvent, "$.event.tick", "negative start tick")
		}
		if uint64(event.Tick) > math.MaxUint64-deadline {
			return newError(CodeTimeoutOverflow, "$.event.tick", "absolute deadline overflows uint64")
		}
		domain = &domainState{
			ID:           node.Zone,
			Roots:        make(map[string]struct{}),
			StartedTick:  event.Tick,
			Timeout:      node.Timeout,
			DeadlineTick: uint64(event.Tick) + deadline,
		}
		s.state.Domains[node.Zone] = domain
	}
	domain.Roots[node.ID] = struct{}{}
	// A later probe in the same domain tightens the deadline to the earliest
	// root timeout. The choice is stable because deadlines are exact ticks.
	deadline, err := node.Timeout.Ticks.DeadlineTicks()
	if err != nil {
		return err
	}
	if deadline > math.MaxUint64-uint64(event.Tick) {
		return newError(CodeTimeoutOverflow, "$.event.tick", "absolute deadline overflows uint64")
	}
	candidate := uint64(event.Tick) + deadline
	if candidate < domain.DeadlineTick {
		domain.DeadlineTick = candidate
		domain.Timeout = node.Timeout
	}
	if _, err := s.refreshFreeze(event.Tick, result); err != nil {
		return err
	}
	changed[domain.ID] = s.domainView(domain, "active", nil)
	return nil
}

func (s *simulation) commit(event Event, result *TickResult, changed map[int64]DomainView) error {
	var domainID int64
	if event.DomainID != nil {
		domainID = *event.DomainID
	} else {
		node, ok := s.nodeByID[event.NodeID]
		if !ok {
			return newError(CodeUnknownNode, "$.event.node_id", "unknown node %q", event.NodeID)
		}
		domainID = node.Zone
	}
	domain := s.state.Domains[domainID]
	if domain == nil {
		return newError(CodeInvalidEvent, "$.event.domain_id", "no active isolation domain %d", domainID)
	}
	tick := event.Tick
	domain.CommittedTick = &tick
	delete(s.state.Domains, domainID)
	if _, err := s.refreshFreeze(event.Tick, result); err != nil {
		return err
	}
	changed[domainID] = s.domainView(domain, "committed", nil)
	return nil
}

func (s *simulation) applyTimeouts(tick int64, changed map[int64]DomainView) error {
	ids := make([]int64, 0, len(s.state.Domains))
	for id := range s.state.Domains {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		domain := s.state.Domains[id]
		if domain == nil || domain.Forced {
			continue
		}
		elapsed := tick - domain.StartedTick
		if elapsed < 0 {
			return newError(CodeInvalidEvent, "$.tick", "tick moved backwards")
		}
		if domain.Timeout.Ticks.TimedOutAt(uint64(elapsed)) {
			upgraded := tick
			domain.Forced = true
			domain.UpgradedTick = &upgraded
			domain.UpgradeEvidence = fmt.Sprintf("elapsed_ticks=%d timeout_ticks=%s deadline_tick=%d", elapsed, domain.Timeout.Ticks, domain.DeadlineTick)
			changed[id] = s.domainView(domain, "forced", nil)
		}
	}
	return nil
}

func (s *simulation) handleRequest(event Event, result *TickResult) (*RequestOutcome, error) {
	workload, ok := s.workloadByID[event.WorkloadID]
	if !ok {
		return nil, newError(CodeUnknownWorkload, "$.event.workload_id", "unknown workload %q", event.WorkloadID)
	}
	if _, frozen := s.state.Frozen[workload.ID]; frozen {
		sources := sortedSet(s.state.FrozenSources[workload.ID])
		s.state.Waiting = append(s.state.Waiting, waitRequest{
			ID:          fmt.Sprintf("%d/%d", event.Tick, event.Seq),
			WorkloadID:  workload.ID,
			NodeID:      workload.NodeID,
			AmountBytes: event.AmountBytes,
			ArrivedTick: event.Tick,
			Seq:         event.Seq,
			Sources:     sources,
		})
		return &RequestOutcome{
			Tick:        event.Tick,
			Seq:         event.Seq,
			WorkloadID:  workload.ID,
			NodeID:      workload.NodeID,
			AmountBytes: event.AmountBytes,
			Status:      "queued",
			Reason:      "workload_frozen",
			Sources:     sources,
		}, nil
	}
	status, reason := s.admit(workload, event.AmountBytes)
	return &RequestOutcome{
		Tick:        event.Tick,
		Seq:         event.Seq,
		WorkloadID:  workload.ID,
		NodeID:      workload.NodeID,
		AmountBytes: event.AmountBytes,
		Status:      status,
		Reason:      reason,
	}, nil
}

func (s *simulation) admit(workload Workload, amount int64) (string, string) {
	used := s.state.Used[workload.ID]
	if amount > 0 && used <= workload.BudgetBytes-amount {
		s.state.Used[workload.ID] = used + amount
		return "admitted", "budget_available"
	}
	return "denied", "budget_exceeded"
}

// refreshFreeze derives the freeze set solely from active domains and their
// reverse dependency propagation. It drains waits only after a frozen workload
// has become unblocked, which in turn only happens after domain commit.
func (s *simulation) refreshFreeze(tick int64, result *TickResult) (map[string]int, error) {
	propagation, err := s.propagation(tick)
	if err != nil {
		return nil, err
	}
	affected := make(map[string]int, len(propagation))
	nodeSources := make(map[string]map[string]struct{}, len(propagation))
	for _, item := range propagation {
		affected[item.NodeID] = item.Generation
		nodeSources[item.NodeID] = stringSet(item.Sources)
	}
	nextFrozen := make(map[string]struct{})
	nextSources := make(map[string]map[string]struct{})
	for nodeID := range affected {
		node, ok := s.nodeByID[nodeID]
		if !ok {
			continue
		}
		for _, workload := range node.Workloads {
			nextFrozen[workload.ID] = struct{}{}
			nextSources[workload.ID] = cloneStringSet(nodeSources[nodeID])
		}
	}

	workloads := make([]string, 0, len(s.state.Frozen))
	for workloadID := range s.state.Frozen {
		workloads = append(workloads, workloadID)
	}
	sort.Strings(workloads)
	for _, workloadID := range workloads {
		if _, stillFrozen := nextFrozen[workloadID]; stillFrozen {
			continue
		}
		workload := s.workloadByID[workloadID]
		previous := sortedSet(s.state.FrozenSources[workloadID])
		result.Unfrozen = append(result.Unfrozen, Unfreeze{
			Tick:       tick,
			WorkloadID: workloadID,
			NodeID:     workload.NodeID,
			Reason:     "isolation_committed",
			Sources:    previous,
		})
		s.state.UnfrozenAt[workloadID] = tick
	}
	s.state.Frozen = nextFrozen
	s.state.FrozenSources = nextSources
	s.drainWaiting(tick, result)
	return affected, nil
}

func (s *simulation) drainWaiting(tick int64, result *TickResult) {
	if len(s.state.Waiting) == 0 {
		return
	}
	remaining := make([]waitRequest, 0, len(s.state.Waiting))
	for _, wait := range s.state.Waiting {
		if _, frozen := s.state.Frozen[wait.WorkloadID]; frozen {
			remaining = append(remaining, wait)
			continue
		}
		workload := s.workloadByID[wait.WorkloadID]
		status, reason := s.admit(workload, wait.AmountBytes)
		releasedStatus := "released_" + status
		result.Requests = append(result.Requests, RequestOutcome{
			Tick:        tick,
			Seq:         wait.Seq,
			WorkloadID:  wait.WorkloadID,
			NodeID:      wait.NodeID,
			AmountBytes: wait.AmountBytes,
			Status:      releasedStatus,
			Reason:      reason,
			Sources:     append([]string(nil), wait.Sources...),
		})
	}
	s.state.Waiting = remaining
}

func (s *simulation) attemptRecoveries(tick int64, result *TickResult) {
	if len(s.state.Pending) == 0 {
		return
	}
	for {
		nodes := make([]string, 0, len(s.state.Pending))
		for nodeID := range s.state.Pending {
			nodes = append(nodes, nodeID)
		}
		sort.Strings(nodes)
		progress := false
		for _, nodeID := range nodes {
			requested := s.state.Pending[nodeID]
			ok, _ := s.canRecover(nodeID)
			decision := RecoveryDecision{
				NodeID:        nodeID,
				RequestedTick: requested,
				DecidedTick:   tick,
			}
			if ok {
				delete(s.state.Pending, nodeID)
				delete(s.state.Faulty, nodeID)
				s.state.Recovered[nodeID] = struct{}{}
				decision.Status = "recovered"
				decision.Reason = "isolation_committed_and_dependencies_healthy"
				progress = true
				result.Recoveries = append(result.Recoveries, decision)
				continue
			}
		}
		if !progress {
			break
		}
	}
	if len(s.state.Pending) == 0 {
		return
	}
	propagation, _ := s.propagation(tick)
	generations := make(map[string]int, len(propagation))
	for _, item := range propagation {
		generations[item.NodeID] = item.Generation
	}
	nodes := make([]string, 0, len(s.state.Pending))
	for nodeID := range s.state.Pending {
		nodes = append(nodes, nodeID)
	}
	sort.Strings(nodes)
	for _, nodeID := range nodes {
		requested := s.state.Pending[nodeID]
		_, chain := s.canRecover(nodeID)
		if len(chain) == 0 {
			chain = []string{nodeID}
		}
		generation := len(chain) - 1
		if value, ok := generations[nodeID]; ok && value > generation {
			generation = value
		}
		result.Recoveries = append(result.Recoveries, RecoveryDecision{
			NodeID:        nodeID,
			RequestedTick: requested,
			DecidedTick:   tick,
			Status:        "blocked",
			Reason:        "isolation_or_dependency_gate",
			Chain:         chain,
			Generation:    generation,
		})
	}
}

func (s *simulation) canRecover(nodeID string) (bool, []string) {
	if node, ok := s.nodeByID[nodeID]; ok {
		if _, isolated := s.state.Domains[node.Zone]; isolated {
			return false, []string{nodeID}
		}
	}
	queue := []string{nodeID}
	parent := map[string]string{nodeID: ""}
	blocked := ""
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, ok := s.nodeByID[current]; !ok {
			continue
		}
		for _, edge := range s.project.Edges {
			if edge.From != current {
				continue
			}
			dependency := edge.To
			if _, seen := parent[dependency]; seen {
				continue
			}
			parent[dependency] = current
			dependencyNode, exists := s.nodeByID[dependency]
			if !exists {
				continue
			}
			_, dependencyFaulty := s.state.Faulty[dependency]
			_, dependencyIsolated := s.state.Domains[dependencyNode.Zone]
			if dependencyFaulty || dependencyIsolated {
				blocked = dependency
				// Keep the last blocking node; the chain below is rebuilt from
				// the deterministic BFS parent relation.
				queue = nil
				break
			}
			queue = append(queue, dependency)
		}
		if blocked != "" {
			break
		}
	}
	if blocked == "" {
		return true, nil
	}
	chain := []string{blocked}
	for current := blocked; parent[current] != ""; {
		current = parent[current]
		chain = append(chain, current)
	}
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}
	if chain[0] != nodeID {
		chain = append([]string{nodeID}, chain...)
	}
	return false, chain
}

func (s *simulation) recoveryGates(propagation []PropagationState) []RecoveryGate {
	generations := make(map[string]int, len(propagation))
	for _, item := range propagation {
		generations[item.NodeID] = item.Generation
	}
	nodes := sortedSet(s.state.Faulty)
	gates := make([]RecoveryGate, 0, len(nodes))
	for _, nodeID := range nodes {
		ok, chain := s.canRecover(nodeID)
		reason := "awaiting_recovery_event"
		if !ok {
			reason = "isolation_or_dependency_gate"
		} else if _, pending := s.state.Pending[nodeID]; !pending {
			reason = "awaiting_recovery_event"
		}
		if len(chain) == 0 {
			chain = []string{nodeID}
		}
		generation := len(chain) - 1
		if value, ok := generations[nodeID]; ok && value > generation {
			generation = value
		}
		gates = append(gates, RecoveryGate{NodeID: nodeID, Reason: reason, Chain: chain, Generation: generation})
	}
	return gates
}

func (s *simulation) waitingSnapshot() []WaitEntry {
	queue := append([]waitRequest(nil), s.state.Waiting...)
	sort.SliceStable(queue, func(i, j int) bool {
		if queue[i].ArrivedTick != queue[j].ArrivedTick {
			return queue[i].ArrivedTick < queue[j].ArrivedTick
		}
		if queue[i].Seq != queue[j].Seq {
			return queue[i].Seq < queue[j].Seq
		}
		return queue[i].WorkloadID < queue[j].WorkloadID
	})
	out := make([]WaitEntry, 0, len(queue))
	for _, wait := range queue {
		out = append(out, WaitEntry{
			ID:          wait.ID,
			WorkloadID:  wait.WorkloadID,
			NodeID:      wait.NodeID,
			AmountBytes: wait.AmountBytes,
			ArrivedTick: wait.ArrivedTick,
			Seq:         wait.Seq,
			Sources:     append([]string(nil), wait.Sources...),
		})
	}
	return out
}

func (s *simulation) nodeStates(propagation []PropagationState) []NodeState {
	generations := make(map[string]int, len(propagation))
	for _, item := range propagation {
		generations[item.NodeID] = item.Generation
	}
	states := make([]NodeState, 0, len(s.project.Nodes))
	for _, node := range s.project.Nodes {
		state := NodeState{NodeID: node.ID, Rack: node.Rack, Zone: node.Zone, Generation: generations[node.ID]}
		_, state.Faulty = s.state.Faulty[node.ID]
		if domain, isolated := s.state.Domains[node.Zone]; isolated {
			state.Isolated = true
			domainID := domain.ID
			state.DomainID = &domainID
			if domain.Forced {
				state.Status = NodeForced
			} else {
				state.Status = NodeIsolated
			}
		} else if state.Faulty {
			state.Status = NodeUnrecovered
		} else if _, recovered := s.state.Recovered[node.ID]; recovered {
			state.Status = NodeRecovered
		} else {
			state.Status = NodeHealthy
		}
		states = append(states, state)
	}
	return states
}

func (s *simulation) domainViews(tick int64, changed map[int64]DomainView, propagation []PropagationState) []DomainView {
	byDomain := make(map[int64][]string)
	for _, item := range propagation {
		for _, source := range item.Sources {
			if len(source) > len("domain:") && source[:len("domain:")] == "domain:" {
				var id int64
				if _, err := fmt.Sscanf(source, "domain:%d", &id); err == nil {
					byDomain[id] = append(byDomain[id], item.NodeID)
				}
			}
		}
	}
	views := make(map[int64]DomainView, len(changed))
	for id, view := range changed {
		views[id] = view
	}
	for id, domain := range s.state.Domains {
		views[id] = s.domainView(domain, "active", byDomain[id])
	}
	result := make([]DomainView, 0, len(views))
	for _, view := range views {
		if view.Status == DomainActive && view.Forced {
			view.Status = DomainForced
		}
		view.AffectedNodes = uniqueSorted(view.AffectedNodes)
		result = append(result, view)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *simulation) domainView(domain *domainState, status string, affected []string) DomainView {
	view := DomainView{
		ID:              domain.ID,
		RootNodes:       sortedSet(domain.Roots),
		Nodes:           s.domainNodes(domain.ID),
		StartedTick:     domain.StartedTick,
		TimeoutTicks:    domain.Timeout.Ticks,
		DeadlineTick:    domain.DeadlineTick,
		Forced:          domain.Forced,
		UpgradedTick:    copyInt64(domain.UpgradedTick),
		UpgradeEvidence: domain.UpgradeEvidence,
		CommittedTick:   copyInt64(domain.CommittedTick),
		AffectedNodes:   append([]string(nil), affected...),
	}
	if status == "active" && domain.Forced {
		view.Status = DomainForced
	} else {
		view.Status = DomainStatus(status)
	}
	return view
}

func (s *simulation) domainNodes(domainID int64) []string {
	var nodes []string
	for _, node := range s.project.Nodes {
		if node.Zone == domainID {
			nodes = append(nodes, node.ID)
		}
	}
	sort.Strings(nodes)
	return nodes
}

func (s *simulation) finalState() FinalState {
	final := FinalState{
		LastTick:     s.state.LastTick,
		WaitingQueue: s.waitingSnapshot(),
		Frozen:       sortedSet(s.state.Frozen),
	}
	propagation, _ := s.propagation(maxInt64(s.state.LastTick, 0))
	final.Nodes = s.nodeStates(propagation)
	for _, domain := range s.state.Domains {
		final.ActiveDomains = append(final.ActiveDomains, s.domainView(domain, "active", nil))
	}
	sort.Slice(final.ActiveDomains, func(i, j int) bool { return final.ActiveDomains[i].ID < final.ActiveDomains[j].ID })
	if final.Nodes == nil {
		final.Nodes = []NodeState{}
	}
	if final.ActiveDomains == nil {
		final.ActiveDomains = []DomainView{}
	}
	if final.WaitingQueue == nil {
		final.WaitingQueue = []WaitEntry{}
	}
	if final.Frozen == nil {
		final.Frozen = []string{}
	}
	return final
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func cloneStringSet(values map[string]struct{}) map[string]struct{} {
	if values == nil {
		return nil
	}
	clone := make(map[string]struct{}, len(values))
	for value := range values {
		clone[value] = struct{}{}
	}
	return clone
}

func stringSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func cloneInt64Map(values map[string]int64) map[string]int64 {
	clone := make(map[string]int64, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func sortedSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func cloneWaiting(values []waitRequest) []waitRequest {
	clone := make([]waitRequest, len(values))
	for i, value := range values {
		clone[i] = value
		clone[i].Sources = append([]string(nil), value.Sources...)
	}
	return clone
}

func uniqueSorted(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func normalizeTickResult(result *TickResult) {
	result.FaultyNodes = nonNilStrings(result.FaultyNodes)
	if result.Domains == nil {
		result.Domains = []DomainView{}
	}
	for index := range result.Domains {
		result.Domains[index].RootNodes = nonNilStrings(result.Domains[index].RootNodes)
		result.Domains[index].Nodes = nonNilStrings(result.Domains[index].Nodes)
		result.Domains[index].AffectedNodes = nonNilStrings(result.Domains[index].AffectedNodes)
	}
	if result.Propagation == nil {
		result.Propagation = []PropagationState{}
	}
	for index := range result.Propagation {
		result.Propagation[index].Chain = nonNilStrings(result.Propagation[index].Chain)
		result.Propagation[index].Sources = nonNilStrings(result.Propagation[index].Sources)
	}
	result.FrozenWorkloads = nonNilStrings(result.FrozenWorkloads)
	if result.WaitingQueue == nil {
		result.WaitingQueue = []WaitEntry{}
	}
	for index := range result.WaitingQueue {
		result.WaitingQueue[index].Sources = nonNilStrings(result.WaitingQueue[index].Sources)
	}
	if result.Unfrozen == nil {
		result.Unfrozen = []Unfreeze{}
	}
	for index := range result.Unfrozen {
		result.Unfrozen[index].Sources = nonNilStrings(result.Unfrozen[index].Sources)
	}
	if result.Requests == nil {
		result.Requests = []RequestOutcome{}
	}
	if result.Recoveries == nil {
		result.Recoveries = []RecoveryDecision{}
	}
	if result.Unrecovered == nil {
		result.Unrecovered = []RecoveryGate{}
	}
	if result.NodeStates == nil {
		result.NodeStates = []NodeState{}
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
