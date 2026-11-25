package engine

import (
	"fmt"
	"sort"
	"sync"

	"quotaflow/internal/domain"
)

type TickDeps struct {
	Tenants    map[string]bool `json:"tenants"`
	Nodes      map[string]bool `json:"nodes"`
	HasFailure bool            `json:"has_failure"`
}

type Engine struct {
	project *domain.Project
	cfg     Config
	events  map[int64][]domain.Event
	edits   []Edit

	initial *State
	states  []*State
	ticks   []*domain.TickObservation
	deps    []TickDeps
	next    int64
	stats   RunStats
	mu      sync.Mutex
}

func New(project *domain.Project, cfg Config) (*Engine, error) {
	if project == nil {
		return nil, domain.NewBizError(domain.EInternalInvariant, "$.project", "nil project")
	}
	if err := domain.ValidateProject(project); err != nil {
		return nil, err
	}
	validated, err := cfg.Validate()
	if err != nil {
		return nil, err
	}
	maxTick := project.MaxTick
	if maxTick < 1 {
		maxTick = 1
	}
	initial := InitialState(project)
	engine := &Engine{
		project: project,
		cfg:     validated,
		events:  project.EventsByTick(),
		initial: initial,
		states:  make([]*State, maxTick+1),
		ticks:   make([]*domain.TickObservation, maxTick+1),
		deps:    make([]TickDeps, maxTick+1),
		next:    1,
		stats: RunStats{
			ThreadBudget: validated.ThreadBudget,
			QueueLimit:   validated.QueueLimit,
			WorkerCount:  validated.ThreadBudget,
		},
	}
	for _, tenant := range project.Tenants {
		engine.stats.PerTenantTiming = append(engine.stats.PerTenantTiming, TenantTiming{TenantID: tenant.ID})
	}
	engine.states[0] = initial
	if err := engine.validateState(initial); err != nil {
		return nil, err
	}
	return engine, nil
}

func (e *Engine) Config() Config {
	return e.cfg
}

func (e *Engine) Project() *domain.Project {
	return e.project
}

func (e *Engine) MaxTick() int64 {
	return e.project.MaxTick
}

func (e *Engine) NextTick() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.next
}

func (e *Engine) RunAll() error {
	for {
		e.mu.Lock()
		done := e.next > e.project.MaxTick
		e.mu.Unlock()
		if done {
			return nil
		}
		if err := e.Step(); err != nil {
			return err
		}
	}
}

func (e *Engine) Step() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stepLocked()
}

func (e *Engine) stepLocked() error {
	if e.next > e.project.MaxTick {
		return nil
	}
	tick := e.next
	previous := e.states[tick-1]
	if previous == nil {
		return domain.NewBizError(domain.EInternalInvariant, "$.state",
			"missing state at tick %d before tick %d", tick-1, tick)
	}
	state := previous.Clone()
	state.Tick = tick
	if err := e.applyDueEdits(state, tick); err != nil {
		return err
	}
	if err := e.commitMigrations(state, tick); err != nil {
		return err
	}
	for _, event := range e.events[tick] {
		if err := e.applyEvent(state, event); err != nil {
			return err
		}
	}
	if err := e.retryPending(state); err != nil {
		return err
	}
	if err := e.validateState(state); err != nil {
		return err
	}
	obs, err := e.observe(state)
	if err != nil {
		return err
	}
	e.states[tick] = state
	e.ticks[tick] = obs
	e.deps[tick] = e.collectDeps(state)
	e.next = tick + 1
	return nil
}

func (e *Engine) Tick(tick int64) (*domain.TickObservation, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if tick < 0 || tick >= int64(len(e.ticks)) || e.ticks[tick] == nil {
		return nil, false
	}
	cp := *e.ticks[tick]
	return &cp, true
}

func (e *Engine) State(tick int64) (*State, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if tick < 0 || tick >= int64(len(e.states)) || e.states[tick] == nil {
		return nil, false
	}
	return e.states[tick].Clone(), true
}

func (e *Engine) LastState() *State {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.next-1 < 0 || int(e.next-1) >= len(e.states) {
		return e.initial.Clone()
	}
	return e.states[e.next-1].Clone()
}

func (e *Engine) InitialState() *State {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.initial.Clone()
}

func (e *Engine) Observations() []domain.TickObservation {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]domain.TickObservation, 0, e.project.MaxTick)
	for tick := int64(1); tick <= e.project.MaxTick; tick++ {
		if e.ticks[tick] != nil {
			out = append(out, *e.ticks[tick])
		}
	}
	return out
}

func (e *Engine) Stats() RunStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := e.stats
	cp.PerTenantTiming = append([]TenantTiming(nil), e.stats.PerTenantTiming...)
	return cp
}

func (e *Engine) applyEvent(state *State, event domain.Event) error {
	switch event.Kind {
	case domain.EventRequest:
		return e.request(state, event)
	case domain.EventRelease:
		if err := e.removePlacement(state, event.Tenant, event.Node, event.AmountMilli, true); err != nil {
			if be, ok := err.(*domain.BizError); ok {
				return domain.WithDetail(be, "event_id", event.ID)
			}
			return err
		}
		return nil
	case domain.EventWeight:
		spec, ok := e.project.TenantByID(event.Tenant)
		if !ok {
			return domain.NewBizError(domain.ETenantUnknown, "$.events."+event.ID+".tenant", "unknown tenant")
		}
		quota, err := domain.TenantQuota(e.project.TotalCapacityMilli, spec, event.Weight, "$.events."+event.ID)
		if err != nil {
			e.stats.FixedStats.Overflows++
			return err
		}
		tenant := state.Tenants[event.Tenant]
		if tenant.Used > quota {
			return domain.NewBizError(domain.EWeightBelowUsed, "$.events."+event.ID+".weight",
				"new quota %d would be below used %d for tenant %q", quota, tenant.Used, event.Tenant)
		}
		tenant.Weight = event.Weight
		tenant.Quota = quota
		state.Tenants[event.Tenant] = tenant
		return nil
	case domain.EventNodeFailure:
		return e.failNode(state, event.Tick, event.Node)
	case domain.EventNodeRecovery:
		node := state.Nodes[event.Node]
		node.Failed = false
		state.Nodes[event.Node] = node
		return nil
	default:
		return domain.NewBizError(domain.EEventInvalid, "$.events."+event.ID, "unknown event kind %q", event.Kind)
	}
}

func (e *Engine) collectDeps(state *State) TickDeps {
	deps := TickDeps{Tenants: map[string]bool{}, Nodes: map[string]bool{}}
	for _, tenant := range e.project.Tenants {
		deps.Tenants[tenant.ID] = true
	}
	for _, node := range state.Nodes {
		if node.Used > 0 || node.Failed {
			deps.Nodes[node.NodeID] = true
		}
	}
	for tick, events := range e.events {
		if tick != state.Tick {
			continue
		}
		for _, event := range events {
			if event.Node != "" {
				deps.Nodes[event.Node] = true
			}
			if event.Kind == domain.EventNodeFailure || event.Kind == domain.EventNodeRecovery {
				deps.HasFailure = true
				for _, node := range e.project.Nodes {
					deps.Nodes[node.ID] = true
				}
			}
		}
	}
	for _, migration := range state.Migrations {
		if migration.Status == "paused" {
			deps.Nodes[migration.FromNode] = true
			deps.Nodes[migration.ToNode] = true
			deps.HasFailure = true
		}
	}
	return deps
}

func (e *Engine) applyDueEdits(state *State, tick int64) error {
	edits := append([]Edit(nil), e.edits...)
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].FromTick != edits[j].FromTick {
			return edits[i].FromTick < edits[j].FromTick
		}
		return edits[i].ID < edits[j].ID
	})
	for _, edit := range edits {
		if tick < edit.FromTick || state.AppliedEdits[edit.ID] {
			continue
		}
		if err := e.applyEdit(state, edit); err != nil {
			return err
		}
		state.AppliedEdits[edit.ID] = true
	}
	return nil
}

func (e *Engine) applyEdit(state *State, edit Edit) error {
	switch edit.Kind {
	case EditTenantWeight:
		spec, ok := e.project.TenantByID(edit.Target)
		if !ok {
			return domain.NewBizError(domain.ETenantUnknown, "$.edit.target", "unknown tenant %q", edit.Target)
		}
		quota, err := domain.TenantQuota(e.project.TotalCapacityMilli, spec, edit.Weight, "$.edit."+edit.ID)
		if err != nil {
			e.stats.FixedStats.Overflows++
			return err
		}
		tenant := state.Tenants[edit.Target]
		if tenant.Used > quota {
			return domain.NewBizError(domain.EEditWouldBreakState, "$.edit."+edit.ID,
				"weight %d makes quota %d below used %d", edit.Weight, quota, tenant.Used)
		}
		tenant.Weight = edit.Weight
		tenant.Quota = quota
		state.Tenants[edit.Target] = tenant
	case EditNodeCapacity:
		node := state.Nodes[edit.Target]
		if node.Used > edit.CapacityMilli {
			return domain.NewBizError(domain.EEditWouldBreakState, "$.edit."+edit.ID,
				"capacity %d is below current node usage %d", edit.CapacityMilli, node.Used)
		}
		reserveSum, err := stateNodeReserveSum(e.project, state, edit.Target)
		if err != nil {
			return err
		}
		if reserveSum > edit.CapacityMilli {
			return domain.NewBizError(domain.EReserveExceedsNode, "$.edit."+edit.ID,
				"capacity %d is below protected reserves %d", edit.CapacityMilli, reserveSum)
		}
		node.Capacity = edit.CapacityMilli
		state.Nodes[edit.Target] = node
	case EditNodeFaultDomain:
		fd, ok := state.Domains[edit.FaultDomain]
		if !ok {
			return domain.NewBizError(domain.EFaultDomainUnknown, "$.edit.fault_domain",
				"unknown domain %q", edit.FaultDomain)
		}
		_ = fd
		node := state.Nodes[edit.Target]
		node.FaultDomain = edit.FaultDomain
		state.Nodes[edit.Target] = node
	case EditDomainParent:
		fd, ok := state.Domains[edit.Target]
		if !ok {
			return domain.NewBizError(domain.EFaultDomainUnknown, "$.edit.target", "unknown domain %q", edit.Target)
		}
		if edit.Parent != "" {
			if _, ok := state.Domains[edit.Parent]; !ok {
				return domain.NewBizError(domain.EFaultDomainUnknown, "$.edit.parent", "unknown domain %q", edit.Parent)
			}
		}
		old := fd.Parent
		fd.Parent = edit.Parent
		state.Domains[fd.ID] = fd
		if chainHasCycle(state.Domains, fd.ID) {
			fd.Parent = old
			state.Domains[fd.ID] = fd
			return domain.NewBizError(domain.EFaultDomainCycle, "$.edit."+edit.ID,
				"setting parent %q creates a fault-domain cycle", edit.Parent)
		}
	default:
		return domain.NewBizError(domain.EEditInvalid, "$.edit."+edit.ID, "unknown edit kind %q", edit.Kind)
	}
	return nil
}

func chainHasCycle(domains map[string]domain.FaultDomain, start string) bool {
	seen := map[string]bool{}
	current := start
	for current != "" {
		if seen[current] {
			return true
		}
		seen[current] = true
		fd, ok := domains[current]
		if !ok {
			return false
		}
		current = fd.Parent
	}
	return false
}

func (e *Engine) validateState(state *State) error {
	for _, tenantSpec := range e.project.Tenants {
		tenant := state.Tenants[tenantSpec.ID]
		if tenant.Used < 0 {
			return domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+tenantSpec.ID, "negative used amount")
		}
		if tenant.Used > tenant.Quota {
			return domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+tenantSpec.ID,
				"used %d exceeds quota %d", tenant.Used, tenant.Quota)
		}
		placementSum := int64(0)
		for _, amount := range state.Placements[tenantSpec.ID] {
			var err error
			placementSum, err = domain.CheckedAdd(placementSum, amount, "$.tenants."+tenantSpec.ID+".placements")
			if err != nil {
				return err
			}
		}
		if placementSum != tenant.Used {
			return domain.NewBizError(domain.ELedgerInvariant, "$.tenants."+tenantSpec.ID,
				"placement sum %d differs from used %d", placementSum, tenant.Used)
		}
	}
	for _, nodeSpec := range e.project.Nodes {
		node := state.Nodes[nodeSpec.ID]
		if node.Used < 0 || node.Used > node.Capacity {
			return domain.NewBizError(domain.ENodeCapacityExceeded, "$.nodes."+nodeSpec.ID,
				"node used %d outside 0..%d", node.Used, node.Capacity)
		}
		placementSum := int64(0)
		for _, tenant := range e.project.Tenants {
			var err error
			placementSum, err = domain.CheckedAdd(placementSum, state.actual(tenant.ID, nodeSpec.ID),
				"$.nodes."+nodeSpec.ID+".placements")
			if err != nil {
				return err
			}
		}
		if placementSum != node.Used {
			return domain.NewBizError(domain.ELedgerInvariant, "$.nodes."+nodeSpec.ID,
				"placement sum %d differs from node used %d", placementSum, node.Used)
		}
		reserveSum, err := stateNodeReserveSum(e.project, state, nodeSpec.ID)
		if err != nil {
			return err
		}
		if reserveSum > node.Capacity {
			return domain.NewBizError(domain.EReserveExceedsNode, "$.nodes."+nodeSpec.ID,
				"reserves %d exceed capacity %d", reserveSum, node.Capacity)
		}
		nresUsed, err := stateNodeNonReservedUsed(e.project, state, nodeSpec.ID)
		if err != nil {
			return err
		}
		if nresUsed > node.Capacity-reserveSum {
			return domain.NewBizError(domain.ELedgerInvariant, "$.nodes."+nodeSpec.ID,
				"non-reserved usage %d exceeds protected capacity %d", nresUsed, node.Capacity-reserveSum)
		}
	}
	for tenantID, pause := range state.Pauses {
		if _, exists := state.Tenants[tenantID]; !exists {
			return domain.NewBizError(domain.ELedgerInvariant, "$.pauses."+tenantID, "pause for unknown tenant")
		}
		if pause.FromNode == pause.ToNode {
			return domain.NewBizError(domain.ELedgerInvariant, "$.pauses."+tenantID, "migration source equals target")
		}
		found := false
		for _, migration := range state.Migrations {
			if migration.TenantID == tenantID && migration.Status == "paused" {
				found = true
			}
		}
		if !found {
			return domain.NewBizError(domain.ELedgerInvariant, "$.pauses."+tenantID, "pause has no paused migration")
		}
	}
	return nil
}

func (e *Engine) DebugString() string {
	return fmt.Sprintf("engine{project=%s ticks=%d next=%d}", e.project.ID, e.project.MaxTick, e.next)
}
