package incremental

import (
	"encoding/json"
	"fmt"
	"sort"

	"grayrelease/internal/engine"
	"grayrelease/internal/model"
)

type Edit struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	Service         string  `json:"service"`
	RuleID          string  `json:"rule_id,omitempty"`
	AtTick          int     `json:"at_tick"`
	WeightBPS       *int    `json:"weight_bps,omitempty"`
	MatchLabelKey   string  `json:"match_label_key,omitempty"`
	MatchLabelValue string  `json:"match_label_value,omitempty"`
	MirrorRatioBPS  *int    `json:"mirror_ratio_bps,omitempty"`
	SampleModulus   *uint64 `json:"sample_modulus,omitempty"`
	RemoveSample    bool    `json:"remove_sample,omitempty"`
	AddDependency   string  `json:"add_dependency,omitempty"`
}

type Closure struct {
	EditID      string   `json:"edit_id"`
	Kind        string   `json:"kind"`
	Services    []string `json:"services"`
	StartTick   int      `json:"start_tick"`
	EndTick     int      `json:"end_tick"`
	Ticks       []int    `json:"ticks"`
	StableKeys  []string `json:"stable_keys,omitempty"`
	Components  []string `json:"components"`
	Explanation string   `json:"explanation"`
}

type Entry struct {
	Service             string                   `json:"service"`
	Tick                int                      `json:"tick"`
	State               engine.BrokerState       `json:"state"`
	Decisions           []engine.Decision        `json:"decisions"`
	Report              engine.ServiceTickReport `json:"report"`
	CacheState          string                   `json:"cache_state"`
	InvalidationSources []string                 `json:"invalidation_sources"`
	ptrs                []*engine.Decision
}

type Report struct {
	Edit               Edit                     `json:"edit"`
	Closure            Closure                  `json:"closure"`
	Result             *engine.SimulationResult `json:"result"`
	RecomputedTicks    int                      `json:"recomputed_ticks"`
	ReusedTicks        int                      `json:"reused_ticks"`
	RecomputedRequests int                      `json:"recomputed_requests"`
	ReusedRequests     int                      `json:"reused_requests"`
	EquivalentToFull   bool                     `json:"equivalent_to_full"`
	FullResult         *engine.SimulationResult `json:"-"`
}

type Manager struct {
	project  *model.Project
	requests []model.Request
	config   engine.Config
	endTick  int
	entries  map[string]*Entry
}

func CloneProject(project *model.Project) (*model.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func NewManager(project *model.Project, requests []model.Request, config engine.Config) (*Manager, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	endTick := computeEndTick(requests, config)
	manager := &Manager{
		project: project, requests: requests, config: config, endTick: endTick,
		entries: map[string]*Entry{},
	}
	if err := manager.buildBaseline(); err != nil {
		return nil, err
	}
	return manager, nil
}

func computeEndTick(requests []model.Request, config engine.Config) int {
	maxTick := 0
	for _, req := range requests {
		if req.Tick > maxTick {
			maxTick = req.Tick
		}
	}
	drain := config.ShadowTimeoutTicks
	if config.ShadowLatencyTicks > drain {
		drain = config.ShadowLatencyTicks
	}
	if drain <= 0 {
		drain = 2
	}
	return maxTick + drain
}

func entryKey(service string, tick int) string {
	return fmt.Sprintf("%s#%06d", service, tick)
}

func (m *Manager) buildBaseline() error {
	services := make([]string, 0, len(m.project.Services))
	for _, service := range m.project.Services {
		services = append(services, service.Name)
	}
	sort.Strings(services)
	for _, service := range services {
		runtime, err := engine.NewRuntime(m.project, m.config, nil)
		if err != nil {
			return err
		}
		for tick := 0; tick <= m.endTick; tick++ {
			reqs := m.requestsFor(service, tick)
			report, ptrs, err := runtime.ProcessServiceTick(service, tick, reqs)
			if err != nil {
				return err
			}
			state := runtime.StateSnapshots()[service]
			m.entries[entryKey(service, tick)] = &Entry{
				Service: service, Tick: tick, State: state, Report: report,
				CacheState: "baseline", ptrs: ptrs,
			}
		}
	}
	for _, entry := range m.entries {
		entry.Decisions = make([]engine.Decision, 0, len(entry.ptrs))
		for _, ptr := range entry.ptrs {
			entry.Decisions = append(entry.Decisions, *ptr)
		}
	}
	return nil
}

func (m *Manager) requestsFor(service string, tick int) []model.Request {
	out := make([]model.Request, 0)
	for _, req := range m.requests {
		if req.Service == service && req.Tick == tick {
			out = append(out, req)
		}
	}
	return out
}

func (m *Manager) Apply(edit Edit) (*Report, error) {
	if edit.AtTick < 0 || edit.AtTick > m.endTick {
		return nil, model.NewError("E_TICK_RANGE", "edit at_tick is outside simulated interval").
			With("at_tick", edit.AtTick).With("end_tick", m.endTick)
	}
	edited, oldRuleForMigration, err := ApplyEdit(m.project, edit)
	if err != nil {
		return nil, err
	}
	closure := m.buildClosure(edit, oldRuleForMigration)

	nextEntries := map[string]*Entry{}
	for key, entry := range m.entries {
		nextEntries[key] = entry
	}

	recomputedTicks := 0
	recomputedRequests := 0
	for _, service := range closure.Services {
		var initial map[string]engine.BrokerState
		if edit.AtTick > 0 {
			if baseEntry, ok := m.entries[entryKey(service, edit.AtTick-1)]; ok {
				initial = map[string]engine.BrokerState{service: baseEntry.State}
			}
		}
		runtime, err := engine.NewRuntime(edited, m.config, initial)
		if err != nil {
			return nil, err
		}
		for tick := edit.AtTick; tick <= m.endTick; tick++ {
			reqs := m.requestsFor(service, tick)
			serviceReport, ptrs, err := runtime.ProcessServiceTick(service, tick, reqs)
			if err != nil {
				return nil, err
			}
			state := runtime.StateSnapshots()[service]
			sources := append([]string{}, closure.Components...)
			sources = append(sources, "edit:"+edit.ID)
			entry := &Entry{
				Service: service, Tick: tick, State: state, Report: serviceReport,
				CacheState: "recomputed", InvalidationSources: sources, ptrs: ptrs,
			}
			nextEntries[entryKey(service, tick)] = entry
			recomputedTicks++
			recomputedRequests += len(reqs)
		}
	}

	for _, entry := range nextEntries {
		if entry.CacheState == "recomputed" {
			entry.Decisions = nil
			for _, ptr := range entry.ptrs {
				entry.Decisions = append(entry.Decisions, *ptr)
			}
		}
	}

	result, reusedTicks, reusedRequests := m.assemble(nextEntries)
	fullResult, err := m.fullTimelineRecompute(edited, edit.AtTick)
	if err != nil {
		return nil, err
	}
	equivalent := ResultsEquivalent(result, fullResult)

	return &Report{
		Edit: edit, Closure: closure, Result: result,
		RecomputedTicks: recomputedTicks, ReusedTicks: reusedTicks,
		RecomputedRequests: recomputedRequests, ReusedRequests: reusedRequests,
		EquivalentToFull: equivalent, FullResult: fullResult,
	}, nil
}

func (m *Manager) assemble(entries map[string]*Entry) (*engine.SimulationResult, int, int) {
	result := &engine.SimulationResult{Config: m.config}
	reusedTicks := 0
	reusedRequests := 0
	for tick := 0; tick <= m.endTick; tick++ {
		tickReport := engine.TickReport{
			Tick: tick, QueueOccupancy: map[string]int{}, ActiveOccupancy: map[string]int{},
			Services: map[string]engine.ServiceTickReport{},
		}
		services := make([]string, 0, len(m.project.Services))
		for _, svc := range m.project.Services {
			services = append(services, svc.Name)
		}
		for _, service := range services {
			entry := entries[entryKey(service, tick)]
			if entry == nil {
				continue
			}
			serviceReport := entry.Report
			decisionCopies := append([]engine.Decision{}, entry.Decisions...)
			sources := entry.InvalidationSources
			if entry.CacheState != "recomputed" {
				sources = []string{"reuse:unchanged-dependency-closure", "cache_key:" + entryKey(service, tick)}
				serviceReport.CacheState = "reused"
				reusedTicks++
				reusedRequests += len(entry.Decisions)
			} else {
				serviceReport.CacheState = "recomputed"
			}
			serviceReport.InvalidationSources = sources
			tickReport.Services[service] = serviceReport
			tickReport.MainAllowed += serviceReport.MainAllowed
			tickReport.BaselineCount += serviceReport.BaselineCount
			tickReport.CanaryCount += serviceReport.CanaryCount
			tickReport.MirrorSelected += serviceReport.MirrorSelected
			tickReport.QueueOccupancy[service] = serviceReport.QueueOccupancy
			tickReport.ActiveOccupancy[service] = serviceReport.ActiveOccupancy
			tickReport.DroppedSet = append(tickReport.DroppedSet, serviceReport.Dropped...)
			tickReport.DeferredSet = append(tickReport.DeferredSet, serviceReport.Deferred...)
			tickReport.Violations = append(tickReport.Violations, serviceReport.Violations...)

			for _, decision := range decisionCopies {
				decision.CacheState = serviceReport.CacheState
				decision.InvalidationSources = append([]string{}, sources...)
				result.Decisions = append(result.Decisions, decision)
			}
		}
		sort.Strings(tickReport.DroppedSet)
		sort.Strings(tickReport.DeferredSet)
		recomputedServices := 0
		sourceSet := map[string]bool{}
		for _, serviceReport := range tickReport.Services {
			if serviceReport.CacheState == "recomputed" {
				recomputedServices++
			}
			for _, source := range serviceReport.InvalidationSources {
				sourceSet[source] = true
			}
		}
		sources := make([]string, 0, len(sourceSet))
		for source := range sourceSet {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		tickReport.InvalidationSources = sources
		switch {
		case recomputedServices == 0:
			tickReport.CacheState = "reused"
		case recomputedServices == len(tickReport.Services):
			tickReport.CacheState = "recomputed"
		default:
			tickReport.CacheState = "partial-recomputed"
		}
		result.Reports = append(result.Reports, tickReport)
	}
	sort.SliceStable(result.Decisions, func(i, j int) bool {
		if result.Decisions[i].Tick != result.Decisions[j].Tick {
			return result.Decisions[i].Tick < result.Decisions[j].Tick
		}
		return result.Decisions[i].RequestID < result.Decisions[j].RequestID
	})
	return result, reusedTicks, reusedRequests
}

func (m *Manager) fullTimelineRecompute(edited *model.Project, atTick int) (*engine.SimulationResult, error) {
	oldRuntime, err := engine.NewRuntime(m.project, m.config, nil)
	if err != nil {
		return nil, err
	}
	result := &engine.SimulationResult{Config: m.config}
	allPointers := make([]*engine.Decision, 0, len(m.requests))
	var states map[string]engine.BrokerState
	for tick := 0; tick < atTick; tick++ {
		report, decisions, err := oldRuntime.ProcessTick(tick, m.requestsAtTick(tick))
		if err != nil {
			return nil, err
		}
		result.Reports = append(result.Reports, report)
		allPointers = append(allPointers, decisions...)
		states = oldRuntime.StateSnapshots()
	}
	newRuntime, err := engine.NewRuntime(edited, m.config, states)
	if err != nil {
		return nil, err
	}
	for tick := atTick; tick <= m.endTick; tick++ {
		report, decisions, err := newRuntime.ProcessTick(tick, m.requestsAtTick(tick))
		if err != nil {
			return nil, err
		}
		result.Reports = append(result.Reports, report)
		allPointers = append(allPointers, decisions...)
	}
	sort.SliceStable(allPointers, func(i, j int) bool {
		if allPointers[i].Tick != allPointers[j].Tick {
			return allPointers[i].Tick < allPointers[j].Tick
		}
		return allPointers[i].RequestID < allPointers[j].RequestID
	})
	result.Decisions = make([]engine.Decision, 0, len(allPointers))
	for _, decision := range allPointers {
		result.Decisions = append(result.Decisions, *decision)
	}
	return result, nil
}

func (m *Manager) requestsAtTick(tick int) []model.Request {
	out := make([]model.Request, 0)
	for _, req := range m.requests {
		if req.Tick == tick {
			out = append(out, req)
		}
	}
	return out
}

func ResultsEquivalent(a, b *engine.SimulationResult) bool {
	if a == nil || b == nil || len(a.Decisions) != len(b.Decisions) || len(a.Reports) != len(b.Reports) {
		return false
	}
	am := map[string]engine.Decision{}
	for _, d := range a.Decisions {
		am[d.RequestID] = d
	}
	for _, bd := range b.Decisions {
		ad, ok := am[bd.RequestID]
		if !ok || !engine.EqualDecisions(ad, bd) {
			return false
		}
	}
	for i := range a.Reports {
		ar, br := a.Reports[i], b.Reports[i]
		if ar.Tick != br.Tick || ar.MainAllowed != br.MainAllowed ||
			ar.MirrorSelected != br.MirrorSelected ||
			fmt.Sprint(ar.QueueOccupancy) != fmt.Sprint(br.QueueOccupancy) ||
			fmt.Sprint(ar.ActiveOccupancy) != fmt.Sprint(br.ActiveOccupancy) ||
			fmt.Sprint(ar.DroppedSet) != fmt.Sprint(br.DroppedSet) ||
			fmt.Sprint(ar.DeferredSet) != fmt.Sprint(br.DeferredSet) {
			return false
		}
	}
	return true
}
