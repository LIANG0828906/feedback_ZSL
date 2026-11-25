package engine

import (
	"sort"

	"quotaflow/internal/domain"
)

type InvalidationSource struct {
	Kind     string `json:"kind"`
	Target   string `json:"target"`
	FromTick int64  `json:"from_tick"`
	Reason   string `json:"reason"`
}

type TickInvalidation struct {
	Tick    int64    `json:"tick"`
	Sources []string `json:"sources"`
}

type InvalidationReport struct {
	EditID             string               `json:"edit_id"`
	FirstAffectedTick  int64                `json:"first_affected_tick"`
	ReusedTicks        []int64              `json:"reused_ticks"`
	RecomputedTicks    []int64              `json:"recomputed_ticks"`
	RecomputeTenants   []string             `json:"recompute_tenants"`
	PropagationClosure []string             `json:"propagation_closure"`
	Sources            []InvalidationSource `json:"invalidation_sources"`
	TickSources        []TickInvalidation   `json:"tick_sources"`
}

func (e *Engine) Edits() []Edit {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Edit(nil), e.edits...)
}

func (e *Engine) ApplyEdit(edit Edit) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := edit.Validate(e.project); err != nil {
		return nil, err
	}
	for _, existing := range e.edits {
		if existing.ID == edit.ID {
			return nil, domain.NewBizError(domain.EEditInvalid, "$.edit.id", "duplicate edit id %q", edit.ID)
		}
	}
	if e.project.MaxTick < 1 {
		return nil, domain.NewBizError(domain.EEditInvalid, "$.project", "project has no timeline")
	}
	if e.next <= e.project.MaxTick {
		return nil, domain.NewBizError(domain.EEditWouldBreakState, "$.edit",
			"incremental edits require a complete cached run; next tick is %d", e.next)
	}
	first := e.firstAffectedTick(edit)
	report := &InvalidationReport{
		EditID:            edit.ID,
		FirstAffectedTick: first,
		Sources: []InvalidationSource{{
			Kind: string(edit.Kind), Target: edit.Target, FromTick: edit.FromTick,
			Reason: "edit dependency seeded at or after from_tick",
		}},
	}
	report.RecomputeTenants, report.PropagationClosure = e.closureForEdit(edit, first)
	if first == 0 || first > e.project.MaxTick {
		for tick := int64(1); tick <= e.project.MaxTick; tick++ {
			report.ReusedTicks = append(report.ReusedTicks, tick)
		}
		e.edits = append(e.edits, edit)
		return report, nil
	}
	for tick := int64(1); tick < first; tick++ {
		report.ReusedTicks = append(report.ReusedTicks, tick)
	}
	for tick := first; tick <= e.project.MaxTick; tick++ {
		report.RecomputedTicks = append(report.RecomputedTicks, tick)
		report.TickSources = append(report.TickSources, TickInvalidation{
			Tick: tick,
			Sources: []string{
				"edit:" + edit.ID + "@" + formatTick(edit.FromTick),
				"propagation:" + tickRange(first, e.project.MaxTick),
			},
		})
	}
	oldStates := append([]*State(nil), e.states...)
	oldTicks := append([]*domain.TickObservation(nil), e.ticks...)
	oldDeps := append([]TickDeps(nil), e.deps...)
	oldNext := e.next
	oldStats := e.stats
	e.edits = append(e.edits, edit)
	for tick := first; tick <= e.project.MaxTick; tick++ {
		e.states[tick] = nil
		e.ticks[tick] = nil
		e.deps[tick] = TickDeps{}
	}
	e.next = first
	for e.next <= e.project.MaxTick {
		if err := e.stepLocked(); err != nil {
			e.states = oldStates
			e.ticks = oldTicks
			e.deps = oldDeps
			e.next = oldNext
			e.stats = oldStats
			e.edits = e.edits[:len(e.edits)-1]
			return nil, err
		}
	}
	return report, nil
}

func (e *Engine) firstAffectedTick(edit Edit) int64 {
	if edit.FromTick > e.project.MaxTick {
		return 0
	}
	switch edit.Kind {
	case EditTenantWeight:
		return edit.FromTick
	case EditNodeCapacity:
		// Node capacity is part of every tick's node ledger observation.
		return edit.FromTick
	case EditNodeFaultDomain, EditDomainParent:
		for tick := edit.FromTick; tick <= e.project.MaxTick; tick++ {
			if e.deps[tick].HasFailure {
				return tick
			}
		}
	}
	return 0
}

func (e *Engine) closureForEdit(edit Edit, first int64) ([]string, []string) {
	tenants := map[string]bool{}
	closure := []string{"edit:" + edit.ID}
	switch edit.Kind {
	case EditTenantWeight:
		tenants[edit.Target] = true
	case EditNodeCapacity:
		for _, tenantID := range e.project.Tenants {
			if allowedOn(e.project, tenantID.ID, edit.Target) {
				tenants[tenantID.ID] = true
			}
		}
		closure = append(closure, "node:"+edit.Target)
	case EditNodeFaultDomain, EditDomainParent:
		if first > 0 {
			for tick := first; tick <= e.project.MaxTick; tick++ {
				state := e.states[tick]
				if state == nil {
					continue
				}
				for _, migration := range state.Migrations {
					if migration.StartTick >= first || migration.Status == "paused" {
						tenants[migration.TenantID] = true
					}
				}
			}
		}
		closure = append(closure, "fault-domain:"+edit.Target)
	}
	if first > 0 && first <= e.project.MaxTick {
		closure = append(closure, "ticks:"+tickRange(first, e.project.MaxTick))
	}
	tenantIDs := make([]string, 0, len(tenants))
	for tenantID := range tenants {
		tenantIDs = append(tenantIDs, tenantID)
	}
	sort.Strings(tenantIDs)
	for _, tenantID := range tenantIDs {
		closure = append(closure, "tenant:"+tenantID)
	}
	sort.Strings(closure)
	return tenantIDs, closure
}

func tickRange(first, last int64) string {
	if first == last {
		return formatTick(first)
	}
	return formatTick(first) + "-" + formatTick(last)
}

func formatTick(tick int64) string {
	if tick == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for tick > 0 {
		index--
		digits[index] = byte('0' + tick%10)
		tick /= 10
	}
	return string(digits[index:])
}

func (e *Engine) FullRecompute() (*Engine, error) {
	e.mu.Lock()
	edits := append([]Edit(nil), e.edits...)
	project := e.project
	cfg := e.cfg
	e.mu.Unlock()
	full, err := New(project, cfg)
	if err != nil {
		return nil, err
	}
	full.edits = edits
	if err := full.RunAll(); err != nil {
		return nil, err
	}
	return full, nil
}

func (e *Engine) ResumeFrom(state *State) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state == nil {
		return domain.NewBizError(domain.EInternalInvariant, "$.state", "nil resume state")
	}
	cp := state.Clone()
	if cp.Tick < 0 || cp.Tick > e.project.MaxTick {
		return domain.NewBizError(domain.ECheckpointVersion, "$.state.tick",
			"checkpoint tick %d outside project timeline 0..%d", cp.Tick, e.project.MaxTick)
	}
	if cp.Tenants == nil || cp.Nodes == nil || cp.Placements == nil || cp.Domains == nil ||
		cp.Migrations == nil || cp.Pauses == nil {
		return domain.NewBizError(domain.ECheckpointCorrupt, "$.state", "checkpoint state maps are incomplete")
	}
	if cp.AppliedEdits == nil {
		cp.AppliedEdits = map[string]bool{}
	}
	for _, tenant := range e.project.Tenants {
		if _, ok := cp.Tenants[tenant.ID]; !ok {
			return domain.NewBizError(domain.ECheckpointCorrupt, "$.tenants."+tenant.ID, "tenant missing from checkpoint")
		}
		if _, ok := cp.Placements[tenant.ID]; !ok {
			cp.Placements[tenant.ID] = map[string]int64{}
		}
	}
	for _, node := range e.project.Nodes {
		if _, ok := cp.Nodes[node.ID]; !ok {
			return domain.NewBizError(domain.ECheckpointCorrupt, "$.nodes."+node.ID, "node missing from checkpoint")
		}
	}
	if err := e.validateState(cp); err != nil {
		return err
	}
	e.states[cp.Tick] = cp
	e.next = cp.Tick + 1
	return nil
}
