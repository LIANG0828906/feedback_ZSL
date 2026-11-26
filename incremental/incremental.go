// Package incremental implements dependency-aware cache invalidation and
// propagation closure reporting.
package incremental

import (
	"encoding/json"
	"fmt"
	"reflect"

	"quotakernel/engine"
	"quotakernel/fixed"
	"quotakernel/model"
)

type EditKind string

const (
	EditNodeCapacity    EditKind = "node_capacity"
	EditTenantQuota     EditKind = "tenant_quota"
	EditTenantGuarantee EditKind = "tenant_guarantee"
	EditWeightEvent     EditKind = "weight_event"
)

type Edit struct {
	Kind      EditKind   `json:"kind"`
	Target    string     `json:"target"`
	AtTick    model.Tick `json:"atTick,omitempty"`
	NewBytes  int64      `json:"newBytes,omitempty"`
	NewPPM    int64      `json:"newPpm,omitempty"`
	NewWeight int64      `json:"newWeight,omitempty"`
}

type ClosureBoundary struct {
	FromTick        model.Tick `json:"fromTick"`
	ToTick          model.Tick `json:"toTick"`
	FirstReusedTick model.Tick `json:"firstReusedTick,omitempty"`
	Converged       bool       `json:"converged"`
	Reason          string     `json:"reason"`
}

type Proof struct {
	Edit             Edit                `json:"edit"`
	DirectTicks      []model.Tick        `json:"directTicks"`
	PropagatedTicks  []model.Tick        `json:"propagatedTicks"`
	RecomputedTicks  []model.Tick        `json:"recomputedTicks"`
	ReusedTicks      []model.Tick        `json:"reusedTicks"`
	Closure          ClosureBoundary     `json:"closure"`
	SourcesByTick    map[string][]string `json:"sourcesByTick"`
	ProvenanceByTick map[string]string   `json:"provenanceByTick"`
	CacheHits        int                 `json:"cacheHits"`
}

type Result struct {
	Proof   Proof
	Reports []model.TickReport
}

type Manager struct {
	Project *model.Project
	Engine  *engine.Engine
	Horizon model.Tick
	reports map[model.Tick]model.TickReport
	states  map[model.Tick]*engine.State
}

func NewManager(project *model.Project) (*Manager, error) {
	eng := engine.New(project)
	state := engine.NewState(project)
	manager := &Manager{
		Project: project, Engine: eng, Horizon: eng.Horizon(),
		reports: map[model.Tick]model.TickReport{},
		states:  map[model.Tick]*engine.State{0: state.Clone()},
	}
	for tick := model.Tick(1); tick <= manager.Horizon; tick++ {
		result, err := eng.RunFrom(state, tick)
		if err != nil {
			return nil, err
		}
		if len(result.Reports) != 1 {
			return nil, fmt.Errorf("expected one report for tick %d", tick)
		}
		manager.reports[tick] = result.Reports[0]
		manager.states[tick] = state.Clone()
	}
	return manager, nil
}

func (m *Manager) CachedReport(tick model.Tick) (model.TickReport, bool) {
	r, ok := m.reports[tick]
	return r, ok
}

// Apply recomputes only the invalidated closure and reuses cached reports
// before the edit and after carried state has converged.
func (m *Manager) Apply(edit Edit) (*Result, error) {
	editedProject, start, hook, err := m.prepare(edit)
	if err != nil {
		return nil, err
	}
	edit.AtTick = start
	eng2 := engine.New(editedProject)
	eng2.BeforeTick = hook

	directTicks := m.directDependencyTicks(edit, start)
	proof := Proof{
		Edit: edit, DirectTicks: directTicks,
		SourcesByTick: map[string][]string{}, ProvenanceByTick: map[string]string{},
	}
	for tick := model.Tick(1); tick < start; tick++ {
		proof.ReusedTicks = append(proof.ReusedTicks, tick)
		proof.ProvenanceByTick[key(tick)] = "cache:before-effective-edit"
		proof.CacheHits++
	}

	work := m.states[start-1].Clone()
	reports := make([]model.TickReport, 0, m.Horizon)
	for tick := model.Tick(1); tick < start; tick++ {
		reports = append(reports, m.reports[tick])
	}

	convergedTo := start - 1
	converged := false
	for tick := start; tick <= m.Horizon; tick++ {
		result, err := eng2.RunFrom(work, tick)
		if err != nil {
			return nil, err
		}
		newReport := result.Reports[0]
		reports = append(reports, newReport)
		proof.RecomputedTicks = append(proof.RecomputedTicks, tick)
		proof.PropagatedTicks = append(proof.PropagatedTicks, tick)
		proof.ProvenanceByTick[key(tick)] = "recomputed:dependency-closure"
		sources := []string{fmt.Sprintf("edit:%s:%s@%d", edit.Kind, edit.Target, start)}
		if tick > start {
			sources = append(sources, fmt.Sprintf("propagation:carried-state:tick-%d", tick-1))
		}
		if containsString(newReport.Dependencies.TenantIDs, edit.Target) || containsString(newReport.Dependencies.NodeIDs, edit.Target) {
			sources = append(sources, "direct-dependency:"+edit.Target)
		}
		proof.SourcesByTick[key(tick)] = sources

		if work.Hash() == m.states[tick].Hash() {
			converged = true
			convergedTo = tick
			break
		}
		convergedTo = tick
	}

	if converged {
		for tick := convergedTo + 1; tick <= m.Horizon; tick++ {
			reports = append(reports, m.reports[tick])
			proof.ReusedTicks = append(proof.ReusedTicks, tick)
			proof.ProvenanceByTick[key(tick)] = "cache:state-hash-converged"
			proof.CacheHits++
		}
		proof.Closure = ClosureBoundary{
			FromTick: start, ToTick: convergedTo, FirstReusedTick: convergedTo + 1,
			Converged: true, Reason: "carried-state-hash-equal-at-boundary",
		}
	} else {
		proof.Closure = ClosureBoundary{
			FromTick: start, ToTick: m.Horizon, Converged: false,
			Reason: "horizon-exhausted-before-state-convergence",
		}
	}
	return &Result{Proof: proof, Reports: reports}, nil
}

// FullRecompute applies the same edit to a fresh full run. Tests compare this
// against Apply's mixed recomputed/cached reports.
func (m *Manager) FullRecompute(edit Edit) ([]model.TickReport, error) {
	project, _, hook, err := m.prepare(edit)
	if err != nil {
		return nil, err
	}
	eng2 := engine.New(project)
	eng2.BeforeTick = hook
	result, err := eng2.Run()
	if err != nil {
		return nil, err
	}
	return result.Reports, nil
}

func (m *Manager) prepare(edit Edit) (*model.Project, model.Tick, engine.BeforeTickHook, error) {
	project, err := CloneProject(m.Project)
	if err != nil {
		return nil, 0, nil, err
	}
	start := edit.AtTick
	if edit.Kind == EditWeightEvent {
		found := false
		for i := range project.Events {
			if project.Events[i].ID != edit.Target {
				continue
			}
			project.Events[i].NewWeight = edit.NewWeight
			if start == 0 {
				start = project.Events[i].Tick
			}
			found = true
			break
		}
		if !found {
			return nil, 0, nil, fmt.Errorf("weight event %q not found", edit.Target)
		}
	}
	if start <= 0 {
		start = 1
	}

	var hook engine.BeforeTickHook
	if edit.Kind != EditWeightEvent {
		applied := false
		hook = func(state *engine.State, tick model.Tick) error {
			if applied || tick != start {
				return nil
			}
			applied = true
			return applySpecEdit(state, edit)
		}
	}
	return project, start, hook, nil
}

func applySpecEdit(state *engine.State, edit Edit) error {
	switch edit.Kind {
	case EditNodeCapacity:
		node, ok := state.Nodes[edit.Target]
		if !ok {
			return fmt.Errorf("node %q not found", edit.Target)
		}
		if edit.NewBytes <= 0 {
			return fmt.Errorf("node capacity edit requires positive newBytes")
		}
		node.CapacityBytes = edit.NewBytes
	case EditTenantQuota, EditTenantGuarantee:
		tenant, ok := state.Tenants[edit.Target]
		if !ok {
			return fmt.Errorf("tenant %q not found", edit.Target)
		}
		totalCapacity := int64(0)
		for _, node := range state.Nodes {
			totalCapacity += node.CapacityBytes
		}
		value := edit.NewBytes
		if edit.NewPPM > 0 {
			rate := fixed.Result{Value: edit.NewPPM}
			if edit.Kind == EditTenantQuota {
				tenant.QuotaPPM = edit.NewPPM
			} else {
				tenant.GuaranteePPM = edit.NewPPM
			}
			converted := fixed.RateBytes(totalCapacity, edit.NewPPM)
			if converted.Overflow {
				return fmt.Errorf("%w: %s ppm %d", fixed.ErrOverflow, edit.Target, edit.NewPPM)
			}
			value = converted.Value
			_ = rate
		}
		if value <= 0 {
			return fmt.Errorf("%s edit requires positive value", edit.Kind)
		}
		if edit.Kind == EditTenantQuota {
			tenant.QuotaBytes = value
		} else {
			tenant.GuaranteeBytes = value
		}
	default:
		return fmt.Errorf("unsupported incremental edit kind %q", edit.Kind)
	}
	return nil
}

func (m *Manager) directDependencyTicks(edit Edit, start model.Tick) []model.Tick {
	out := []model.Tick{}
	for tick := model.Tick(1); tick <= m.Horizon; tick++ {
		report := m.reports[tick]
		matched := false
		switch edit.Kind {
		case EditWeightEvent:
			matched = containsString(report.Dependencies.EventIDs, edit.Target)
		case EditNodeCapacity:
			matched = containsString(report.Dependencies.NodeIDs, edit.Target)
		default:
			matched = containsString(report.Dependencies.TenantIDs, edit.Target)
		}
		if matched && tick >= start {
			out = append(out, tick)
		}
	}
	return out
}

func EqualReports(a, b []model.TickReport) error {
	if len(a) != len(b) {
		return fmt.Errorf("report length mismatch: incremental %d full %d", len(a), len(b))
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			aj, _ := json.MarshalIndent(a[i], "", "  ")
			bj, _ := json.MarshalIndent(b[i], "", "  ")
			return fmt.Errorf("tick %d report mismatch\nincremental=%s\nfull=%s", a[i].Tick, aj, bj)
		}
	}
	return nil
}

func CloneProject(in *model.Project) (*model.Project, error) {
	data, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func key(tick model.Tick) string { return fmt.Sprintf("tick-%06d", tick) }

func containsString(list []string, target string) bool {
	for _, v := range list {
		if v == target {
			return true
		}
	}
	return false
}
