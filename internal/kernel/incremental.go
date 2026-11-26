package kernel

import (
	"fmt"
	"sort"
)

type EditKind string

const (
	EditNodeCapacity    EditKind = "node_capacity"
	EditTenantQuota     EditKind = "tenant_quota"
	EditTenantGuarantee EditKind = "tenant_guarantee"
	EditWeightEvent     EditKind = "weight_event"
)

type Edit struct {
	Kind     EditKind `json:"kind"`
	AtTick   int64    `json:"at_tick,omitempty"`
	NodeID   ID       `json:"node_id,omitempty"`
	TenantID ID       `json:"tenant_id,omitempty"`
	EventID  string   `json:"event_id,omitempty"`
	NewValue int64    `json:"new_value"`
}

type InvalidationReport struct {
	Source          Edit                `json:"source"`
	SourceKey       string              `json:"source_key"`
	FromTick        int64               `json:"from_tick"`
	ToTick          int64               `json:"to_tick"`
	Closure         []string            `json:"closure"`
	RecomputedTicks []int64             `json:"recomputed_ticks"`
	ReusedTicks     []int64             `json:"reused_ticks"`
	RecomputedKeys  []string            `json:"recomputed_keys"`
	ReusedKeys      []string            `json:"reused_keys"`
	Provenance      map[string][]string `json:"provenance"`
	CacheHits       int                 `json:"cache_hits"`
	CacheMisses     int                 `json:"cache_misses"`
}

type ResultCache struct {
	Fingerprint string               `json:"fingerprint"`
	Entries     map[int64]TickReport `json:"entries"`
}

type IncrementalEngine struct {
	Project *Project
	Budgets Budgets
	Mode    string
	Cache   *ResultCache
	last    *RunReport
}

func NewIncrementalEngine(p *Project, budgets Budgets) (*IncrementalEngine, error) {
	if err := budgets.Validate(); err != nil {
		return nil, err
	}
	proj := p.Clone()
	report, err := RunEngine(proj, budgets, ModeAuto)
	if err != nil {
		return nil, err
	}
	cache := &ResultCache{Fingerprint: proj.Fingerprint(), Entries: map[int64]TickReport{}}
	for _, tick := range report.Ticks {
		cache.Entries[tick.Tick] = tick
	}
	return &IncrementalEngine{Project: proj, Budgets: budgets, Mode: ModeAuto, Cache: cache, last: report}, nil
}

func (e *IncrementalEngine) LastReport() *RunReport { return e.last }

// ApplyEdit validates the edit on a clone before touching committed cache
// state. Failed injections therefore leave the previous report usable.
func (e *IncrementalEngine) ApplyEdit(edit Edit) (*InvalidationReport, *RunReport, error) {
	candidate := e.Project.Clone()
	sourceKey, fromTick, affectedNode, affectedTenant, err := mutateForEdit(candidate, edit)
	if err != nil {
		if be, ok := AsBizError(err); ok {
			return nil, nil, be.WithDetail("edit", edit)
		}
		return nil, nil, err
	}
	if err := ValidateProject(candidate); err != nil {
		return nil, nil, err
	}

	maxTick := int64(0)
	for _, ev := range candidate.Events {
		if ev.Tick > maxTick {
			maxTick = ev.Tick
		}
	}
	if fromTick < 0 {
		fromTick = 0
	}
	if fromTick > maxTick {
		fromTick = maxTick
	}

	inv := &InvalidationReport{
		Source: edit, SourceKey: sourceKey, FromTick: fromTick, ToTick: maxTick,
		Provenance: map[string][]string{},
	}
	inv.Closure = buildClosure(sourceKey, fromTick, maxTick, affectedNode, affectedTenant)

	for tick := int64(0); tick <= maxTick; tick++ {
		key := fmt.Sprintf("tick:%d", tick)
		if tick < fromTick {
			inv.ReusedTicks = append(inv.ReusedTicks, tick)
			inv.ReusedKeys = append(inv.ReusedKeys, key)
			inv.CacheHits++
			inv.Provenance[key] = []string{"cache:prefix-unchanged", sourceKey}
			continue
		}
		inv.RecomputedTicks = append(inv.RecomputedTicks, tick)
		inv.RecomputedKeys = append(inv.RecomputedKeys, key)
		inv.CacheMisses++
		chain := []string{sourceKey}
		if affectedTenant != "" {
			chain = append(chain, fmt.Sprintf("tenant:%s@%d", affectedTenant, tick))
		}
		if affectedNode != "" {
			chain = append(chain, fmt.Sprintf("node:%s@%d", affectedNode, tick))
		}
		chain = append(chain, key)
		if tick > fromTick {
			chain = append(chain, fmt.Sprintf("state-carry-forward:tick:%d", tick-1))
		}
		inv.Provenance[key] = chain
	}

	suffix, err := runSeeded(candidate, e.Budgets, e.Mode, fromTick, e.Cache)
	if err != nil {
		return nil, nil, err
	}

	// Commit point: only after successful recomputation do we adopt the
	// candidate and replace cache entries.
	e.Project = candidate
	for _, tick := range suffix.Ticks {
		tick := tick
		e.Cache.Entries[tick.Tick] = tick
	}
	e.Cache.Fingerprint = candidate.Fingerprint()

	full := &RunReport{ProjectID: candidate.ProjectID, Seed: candidate.Seed, Path: "incremental", Budgets: e.Budgets}
	for tick := int64(0); tick <= maxTick; tick++ {
		if tr, ok := e.Cache.Entries[tick]; ok {
			full.Ticks = append(full.Ticks, tr)
		}
	}
	e.last = full
	return inv, full, nil
}

func mutateForEdit(p *Project, edit Edit) (sourceKey string, fromTick int64, node ID, tenant ID, err error) {
	fromTick = edit.AtTick
	node = edit.NodeID
	tenant = edit.TenantID
	switch edit.Kind {
	case EditNodeCapacity:
		n, ok := p.Node(edit.NodeID)
		if !ok {
			return "", 0, node, tenant, NewBizError(CodeReferenceNotFound, "edit node %q not found", edit.NodeID).WithNode(edit.NodeID)
		}
		if edit.NewValue < 0 {
			return "", 0, node, tenant, NewBizError(CodeNegativeCapacity, "new capacity %d is negative", edit.NewValue).WithNode(edit.NodeID)
		}
		n.CapacityBytes = edit.NewValue
		for i := range p.Nodes {
			if p.Nodes[i].ID == n.ID {
				p.Nodes[i].CapacityBytes = edit.NewValue
			}
		}
		return fmt.Sprintf("node:%s.capacity", edit.NodeID), fromTick, edit.NodeID, "", nil

	case EditTenantQuota, EditTenantGuarantee:
		t, ok := p.Tenant(edit.TenantID)
		if !ok {
			return "", 0, node, tenant, NewBizError(CodeReferenceNotFound, "edit tenant %q not found", edit.TenantID).WithTenant(edit.TenantID)
		}
		if edit.NewValue < 0 {
			code := CodeNegativeQuota
			return "", 0, node, tenant, NewBizError(code, "new value %d is negative", edit.NewValue).WithTenant(edit.TenantID)
		}
		target := "quota"
		if edit.Kind == EditTenantGuarantee {
			target = "guarantee"
			if edit.NewValue > t.QuotaBytes {
				return "", 0, node, tenant, NewBizError(CodeGuaranteeOverQuota, "guarantee %d exceeds quota %d", edit.NewValue, t.QuotaBytes).
					WithTenant(edit.TenantID).WithDetail("new_value", edit.NewValue)
			}
		}
		for i := range p.Tenants {
			if p.Tenants[i].ID != edit.TenantID {
				continue
			}
			if edit.Kind == EditTenantQuota {
				if edit.NewValue < p.Tenants[i].GuaranteeBytes {
					return "", 0, node, tenant, NewBizError(CodeGuaranteeOverQuota, "quota %d is below guarantee %d", edit.NewValue, p.Tenants[i].GuaranteeBytes).
						WithTenant(edit.TenantID)
				}
				p.Tenants[i].QuotaBytes = edit.NewValue
			} else {
				p.Tenants[i].GuaranteeBytes = edit.NewValue
			}
		}
		return fmt.Sprintf("tenant:%s.%s", edit.TenantID, target), fromTick, "", edit.TenantID, nil

	case EditWeightEvent:
		var eventID string
		found := false
		for i := range p.Events {
			ev := &p.Events[i]
			if edit.EventID != "" && ev.ID == edit.EventID {
				ev.WeightPermyriad = edit.NewValue
				fromTick = ev.Tick
				tenant = ev.TenantID
				eventID = ev.ID
				found = true
				break
			}
			if edit.EventID == "" && ev.Type == EventTypeWeight && ev.TenantID == edit.TenantID && ev.Tick == edit.AtTick {
				ev.WeightPermyriad = edit.NewValue
				edit.EventID = ev.ID
				eventID = ev.ID
				fromTick = ev.Tick
				tenant = ev.TenantID
				found = true
				break
			}
		}
		if !found {
			return "", 0, node, tenant, NewBizError(CodeReferenceNotFound, "weight event not found").
				WithEvent(edit.EventID).WithTenant(edit.TenantID).WithDetail("at_tick", edit.AtTick)
		}
		if edit.NewValue < 0 || edit.NewValue > 10000 {
			return "", 0, node, tenant, NewBizError(CodeBurstPercentRange, "weight BP must be [0,10000], got %d", edit.NewValue).
				WithEvent(eventID).WithTenant(tenant)
		}
		return fmt.Sprintf("event:%s.weight", eventID), fromTick, "", tenant, nil

	default:
		return "", 0, "", "", NewBizError(CodeFieldConflict, "unknown edit kind %q", edit.Kind)
	}
}

func buildClosure(sourceKey string, fromTick, maxTick int64, node ID, tenant ID) []string {
	closure := []string{sourceKey}
	if tenant != "" {
		closure = append(closure, fmt.Sprintf("dependency:tenant:%s", tenant))
	}
	if node != "" {
		closure = append(closure, fmt.Sprintf("dependency:node:%s", node))
	}
	for tick := fromTick; tick <= maxTick; tick++ {
		if tenant != "" {
			closure = append(closure, fmt.Sprintf("tenant:%s@%d", tenant, tick))
		}
		if node != "" {
			closure = append(closure, fmt.Sprintf("node:%s@%d", node, tick))
		}
		closure = append(closure, fmt.Sprintf("tick:%d", tick))
	}
	return closure
}

func runSeeded(p *Project, budgets Budgets, mode string, fromTick int64, cache *ResultCache) (*RunReport, error) {
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

	if fromTick > 0 {
		prev, ok := cache.Entries[fromTick-1]
		if !ok {
			return nil, NewBizError(CodeReferenceNotFound, "incremental seed tick %d is not cached", fromTick-1)
		}
		for _, l := range prev.Ledgers {
			s.used[l.TenantID] = l.UsedBytes
			s.weights[l.TenantID] = l.EffectiveWeightBP
		}
		for _, n := range prev.Nodes {
			s.nodeOcc[n.NodeID] = n.OccupiedBytes
			s.nodeHeld[n.NodeID] = n.HeldBytes
		}
		for _, q := range prev.Queues {
			if s.queues[q.TenantID] == nil {
				s.queues[q.TenantID] = map[string]QueueItem{}
			}
			s.queues[q.TenantID][q.RequestID] = q
		}
	}
	if mode == PathParallel {
		s.pool = NewWorkerPool(budgets.Workers)
		defer s.pool.Wait()
	}

	maxTick := int64(0)
	eventsByTick := map[int64][]Event{}
	for _, ev := range proj.Events {
		eventsByTick[ev.Tick] = append(eventsByTick[ev.Tick], ev)
		if ev.Tick > maxTick {
			maxTick = ev.Tick
		}
	}
	report := &RunReport{ProjectID: proj.ProjectID, Seed: proj.Seed, Path: "incremental-suffix", Budgets: budgets}
	for tick := fromTick; tick <= maxTick; tick++ {
		s.order = nil
		s.allocs = nil
		if err := s.processTick(tick, eventsByTick[tick]); err != nil {
			return nil, err
		}
		report.Ticks = append(report.Ticks, s.snapshot(tick))
	}
	return report, nil
}

func FullRecompute(p *Project, budgets Budgets) (*RunReport, error) {
	return RunReference(p, budgets)
}

func SortedInvalidationKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
