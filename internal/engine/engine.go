package engine

import (
	"fmt"
	"sort"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/session"
)

type State struct {
	CommittedTick   int64                `json:"committed_tick"`
	TopologyVersion int64                `json:"topology_version"`
	TotalBudget     int64                `json:"total_budget_kbps"`
	UsedBudget      int64                `json:"used_budget_kbps"`
	Topology        session.Topology     `json:"topology"`
	Allocations     []session.Allocation `json:"allocations"`
}

type AdmissionReport struct {
	Decision         session.Decision
	ErrorCode        string
	Reason           string
	Constraint       string
	RequestedScale   int64
	SettledScale     int64
	RequestedMapping *session.Mapping
	Mapping          *session.Mapping
	MinimumMapping   *session.Mapping
	Cost             int64
	MinimumCost      int64
	Margin           int64
	Shortfall        int64
}

type Engine struct {
	project   session.Project
	screens   map[string]session.Screen
	screenIDs []string
	events    []session.Event
	nextEvent int

	cache  *MappingCache
	state  State
	active map[string]session.Allocation
}

func New(project session.Project) (*Engine, error) {
	return NewWithCache(project, NewMappingCache())
}

func NewWithCache(project session.Project, cache *MappingCache) (*Engine, error) {
	screens := append([]session.Screen(nil), project.Screens...)
	sort.SliceStable(screens, func(i, j int) bool { return screens[i].ID < screens[j].ID })
	project.Screens = screens

	events := append([]session.Event(nil), project.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Tick < events[j].Tick })
	project.Events = events

	if err := session.ValidateProject(&project); err != nil {
		return nil, err
	}

	e := &Engine{
		project:   project,
		screens:   map[string]session.Screen{},
		screenIDs: make([]string, 0, len(screens)),
		events:    events,
		cache:     cache,
		active:    map[string]session.Allocation{},
	}
	for _, screen := range screens {
		e.screens[screen.ID] = screen
		e.screenIDs = append(e.screenIDs, screen.ID)
	}
	topology := session.InitialTopology(screens)
	e.state = State{
		CommittedTick:   0,
		TopologyVersion: topology.Version,
		TotalBudget:     project.TotalBudget,
		UsedBudget:      0,
		Topology:        topology,
		Allocations:     nil,
	}
	return e, nil
}

func (e *Engine) Project() session.Project { return e.project }

func (e *Engine) State() State {
	state := e.state
	state.Allocations = make([]session.Allocation, 0, len(e.active))
	for _, id := range e.screenIDs {
		if alloc, ok := e.active[id]; ok {
			state.Allocations = append(state.Allocations, alloc)
		}
	}
	state.Topology = cloneTopology(e.state.Topology)
	return state
}

func (e *Engine) RestoreState(state State) error {
	if state.TopologyVersion < 1 {
		return bizerror.New(bizerror.ECheckpointVersion, "restored state has invalid topology version")
	}
	active := map[string]session.Allocation{}
	var used int64
	for _, alloc := range state.Allocations {
		screen, ok := e.screens[alloc.ScreenID]
		if !ok {
			return bizerror.New(bizerror.EScreenNotFound, "checkpoint allocation references unknown screen").WithScreen(alloc.ScreenID)
		}
		if _, dup := active[alloc.ScreenID]; dup {
			return bizerror.New(bizerror.EInternal, "checkpoint contains duplicate budget allocation").WithScreen(alloc.ScreenID)
		}
		if alloc.BudgetCostKbps != alloc.Mapping.BudgetCostKbps {
			return bizerror.New(bizerror.ECheckpointChecksum, "checkpoint allocation cost does not match mapping").WithScreen(alloc.ScreenID)
		}
		if alloc.TopologyVersion != state.TopologyVersion {
			return bizerror.New(bizerror.ECheckpointVersion, "allocation topology version does not match global checkpoint").WithScreen(alloc.ScreenID)
		}
		active[alloc.ScreenID] = alloc
		used += alloc.BudgetCostKbps
		_ = screen
	}
	if used != state.UsedBudget {
		return bizerror.New(bizerror.ECheckpointChecksum, "checkpoint used budget does not match allocations").
			WithDetail("recorded", state.UsedBudget).WithDetail("recomputed", used)
	}
	e.active = active
	e.state = state
	e.state.Topology = cloneTopology(state.Topology)

	e.nextEvent = 0
	for e.nextEvent < len(e.events) && e.events[e.nextEvent].Tick <= state.CommittedTick {
		e.nextEvent++
	}
	return nil
}

func (e *Engine) MaxTick() int64 {
	var max int64
	for _, event := range e.events {
		if event.Tick > max {
			max = event.Tick
		}
	}
	return max
}

func (e *Engine) RunAll() ([]session.TickResult, error) {
	maxTick := e.MaxTick()
	results := make([]session.TickResult, 0, maxTick)
	for tick := e.state.CommittedTick + 1; tick <= maxTick; tick++ {
		result, err := e.AdvanceOne()
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (e *Engine) AdvanceOne() (session.TickResult, error) {
	tick := e.state.CommittedTick + 1
	results := map[string]*session.ScreenResult{}
	order := make([]string, 0, len(e.screenIDs))
	for _, id := range e.screenIDs {
		order = append(order, id)
		screen := e.screens[id]
		result := &session.ScreenResult{
			Tick:           tick,
			ScreenID:       id,
			Host:           screen.Host,
			RequestedScale: screen.ScalePermyriad,
		}
		if alloc, ok := e.active[id]; ok {
			result.Active = true
			result.Decision = session.DecisionUnchanged
			result.SettledScale = alloc.SettledScale
			mapping := alloc.Mapping
			result.Mapping = &mapping
			result.BudgetCostKbps = alloc.BudgetCostKbps
		} else {
			result.Decision = session.DecisionPending
		}
		results[id] = result
	}

	for e.nextEvent < len(e.events) && e.events[e.nextEvent].Tick == tick {
		event := e.events[e.nextEvent]
		e.nextEvent++
		if err := e.applyEvent(event, results, tick); err != nil {
			return session.TickResult{}, err
		}
	}

	finalResults := make([]session.ScreenResult, 0, len(order))
	remaining := e.state.TotalBudget - e.state.UsedBudget
	for _, id := range order {
		result := results[id]
		result.Host = e.screens[id].Host
		result.RemainingBudget = remaining
		if alloc, ok := e.active[id]; ok {
			result.Active = true
			if result.Mapping == nil {
				mapping := alloc.Mapping
				result.Mapping = &mapping
			}
			result.SettledScale = alloc.SettledScale
			result.BudgetCostKbps = alloc.BudgetCostKbps
			result.JoinVerdict = session.DecisionActive
		} else {
			result.Active = false
			result.Mapping = nil
			if result.Decision == session.DecisionUnchanged {
				result.Decision = session.DecisionPending
			}
			verdict, err := e.EvaluateJoin(id, "tick_end_verdict")
			if err != nil {
				return session.TickResult{}, err
			}
			result.JoinVerdict = verdict.Decision
			result.JoinScale = verdict.SettledScale
			result.JoinCost = verdict.Cost
			if result.RequestedMapping == nil && verdict.RequestedMapping != nil {
				result.RequestedMapping = verdict.RequestedMapping
			}
		}
		finalResults = append(finalResults, *result)
	}

	e.state.CommittedTick = tick
	return session.TickResult{
		Tick:            tick,
		TopologyVersion: e.state.TopologyVersion,
		TotalBudget:     e.state.TotalBudget,
		UsedBudget:      e.state.UsedBudget,
		RemainingBudget: remaining,
		Results:         finalResults,
	}, nil
}

func (e *Engine) applyEvent(event session.Event, results map[string]*session.ScreenResult, tick int64) error {
	trigger := eventTrigger(event, tick)
	switch event.Type {
	case session.EventJoin:
		report, err := e.EvaluateJoin(event.ScreenID, trigger)
		if err != nil {
			if be, ok := bizerror.As(err); ok {
				return be.WithTick(tick)
			}
			return err
		}
		result := results[event.ScreenID]
		result.TriggerSource = trigger
		result.Reason = report.Reason
		result.Constraint = report.Constraint
		result.ErrorCode = report.ErrorCode
		result.RequestedScale = report.RequestedScale
		result.BudgetShortfallKbps = report.Shortfall
		result.BudgetMarginKbps = report.Margin
		result.RequestedMapping = report.RequestedMapping

		if report.Decision == session.DecisionRejected {
			result.Active = false
			result.Decision = session.DecisionRejected
			result.SettledScale = report.SettledScale
			result.JoinScale = report.SettledScale
			result.JoinCost = report.MinimumCost
			return nil
		}

		mapping := *report.Mapping
		scale := report.SettledScale
		e.active[event.ScreenID] = session.Allocation{
			ScreenID:        event.ScreenID,
			TopologyVersion: e.state.TopologyVersion,
			RequestedScale:  report.RequestedScale,
			SettledScale:    scale,
			Mapping:         mapping,
			BudgetCostKbps:  mapping.BudgetCostKbps,
			CommittedTick:   tick,
		}
		e.state.UsedBudget += mapping.BudgetCostKbps
		result.Active = true
		result.Decision = report.Decision
		result.SettledScale = scale
		result.Mapping = &mapping
		result.BudgetCostKbps = mapping.BudgetCostKbps
		result.BudgetMarginKbps = e.state.TotalBudget - e.state.UsedBudget
		return nil

	case session.EventLeave:
		alloc, ok := e.active[event.ScreenID]
		if !ok {
			return bizerror.New(bizerror.EScreenNotActive, "cannot leave inactive screen").
				WithScreen(event.ScreenID).WithTick(tick).WithDetail("trigger", trigger)
		}
		e.state.UsedBudget -= alloc.BudgetCostKbps
		delete(e.active, event.ScreenID)
		result := results[event.ScreenID]
		result.Active = false
		result.Decision = session.DecisionLeft
		result.TriggerSource = trigger
		result.Reason = "SCREEN_LEAVE_RELEASES_ALLOCATION"
		result.Constraint = "C-BUDGET-TOTAL"
		result.BudgetCostKbps = 0
		result.Mapping = nil
		result.SettledScale = 0
		return nil

	case session.EventBudget:
		e.state.TotalBudget = event.Budget
		return e.rebalance(trigger, tick, results)

	case session.EventUpdate:
		return e.applyUpdate(event, trigger, tick, results)

	case session.EventTopology:
		if event.Topology == nil {
			return bizerror.New(bizerror.ETopologyEventInvalid, "missing topology").WithTick(tick)
		}
		if event.Topology.Version <= e.state.TopologyVersion {
			return bizerror.New(bizerror.ETopologyEventInvalid, "topology version must be greater than the committed version").
				WithTick(tick).
				WithConstraint("C-TOPOLOGY-VERSION").
				WithDetail("committed", e.state.TopologyVersion).
				WithDetail("actual", event.Topology.Version)
		}
		if err := session.ValidateTopology(*event.Topology, e.project.Screens); err != nil {
			return err
		}
		e.state.Topology = cloneTopology(*event.Topology)
		e.state.TopologyVersion = event.Topology.Version
		for id, alloc := range e.active {
			alloc.TopologyVersion = event.Topology.Version
			e.active[id] = alloc
		}
		for _, host := range event.Topology.Hosts {
			for _, id := range host.ScreenIDs {
				screen := e.screens[id]
				screen.Host = host.ID
				e.screens[id] = screen
			}
		}
		return nil

	default:
		return bizerror.New(bizerror.ETopologyEventInvalid, "unknown event type").WithTick(tick).WithDetail("type", event.Type)
	}
}

func (e *Engine) EvaluateJoin(screenID, trigger string) (AdmissionReport, error) {
	screen, ok := e.screens[screenID]
	if !ok {
		return AdmissionReport{}, bizerror.New(bizerror.EScreenNotFound, "unknown controlled screen").WithScreen(screenID)
	}
	requestedMapping, err := e.mapRegion(e.project.Master, screen.Width, screen.Height, screen.ScalePermyriad, string(screen.Chroma))
	if err != nil {
		return AdmissionReport{}, err.(*bizerror.Error).WithScreen(screenID)
	}
	minimumMapping, err := e.mapRegion(e.project.Master, screen.Width, screen.Height, screen.MinScalePermyriad, string(screen.Chroma))
	if err != nil {
		return AdmissionReport{}, err.(*bizerror.Error).WithScreen(screenID)
	}
	remaining := e.state.TotalBudget - e.state.UsedBudget
	report := AdmissionReport{
		RequestedScale:   screen.ScalePermyriad,
		RequestedMapping: &requestedMapping,
		MinimumMapping:   &minimumMapping,
		MinimumCost:      minimumMapping.BudgetCostKbps,
	}
	if requestedMapping.BudgetCostKbps <= remaining {
		report.Decision = session.DecisionAdmitted
		report.Constraint = "C-BUDGET-TOTAL"
		report.Reason = "REQUESTED_QUALITY_FITS"
		report.SettledScale = screen.ScalePermyriad
		report.Mapping = &requestedMapping
		report.Cost = requestedMapping.BudgetCostKbps
		report.Margin = remaining - requestedMapping.BudgetCostKbps
		return report, nil
	}

	report.Shortfall = requestedMapping.BudgetCostKbps - remaining
	for _, rung := range session.DeclaredScaleLadderPermyriad {
		if rung > screen.ScalePermyriad || rung < screen.MinScalePermyriad {
			continue
		}
		mapping, err := e.mapRegion(e.project.Master, screen.Width, screen.Height, rung, string(screen.Chroma))
		if err != nil {
			return AdmissionReport{}, err.(*bizerror.Error).WithScreen(screenID)
		}
		if mapping.BudgetCostKbps <= remaining {
			report.Decision = session.DecisionDowngraded
			report.Constraint = "C-BUDGET-TOTAL"
			report.Reason = "INSUFFICIENT_BUDGET_HEADROOM_FOR_REQUESTED_SCALE"
			report.SettledScale = rung
			report.Mapping = &mapping
			report.Cost = mapping.BudgetCostKbps
			report.Margin = remaining - mapping.BudgetCostKbps
			return report, nil
		}
	}

	report.Decision = session.DecisionRejected
	report.ErrorCode = bizerror.EAdmissionRejected
	report.Constraint = "C-MIN-QUALITY"
	report.Reason = "MINIMUM_ACCEPTABLE_QUALITY_EXCEEDS_REMAINING_BUDGET"
	report.SettledScale = screen.MinScalePermyriad
	report.Cost = minimumMapping.BudgetCostKbps
	report.Shortfall = minimumMapping.BudgetCostKbps - remaining
	if report.Shortfall < 0 {
		report.Shortfall = 0
	}
	_ = trigger
	return report, nil
}

func (e *Engine) rebalance(trigger string, tick int64, results map[string]*session.ScreenResult) error {
	for {
		if e.state.UsedBudget <= e.state.TotalBudget {
			return nil
		}
		overage := e.state.UsedBudget - e.state.TotalBudget
		candidate := e.selectDowngradeCandidate()
		if candidate != "" {
			alloc := e.active[candidate]
			screen := e.screens[candidate]
			nextScale, ok := session.NextLowerRung(alloc.SettledScale)
			if !ok || nextScale < screen.MinScalePermyriad {
				// selectDowngradeCandidate should have excluded this, but keep
				// the invariant explicit.
				evictID := e.selectEvictionCandidate()
				if evictID == "" {
					return bizerror.New(bizerror.EAdmissionRejected, "budget below minimum quality and no eviction candidate").WithTick(tick)
				}
				candidate = evictID
			} else {
				mapping, err := e.mapRegion(e.project.Master, screen.Width, screen.Height, nextScale, string(screen.Chroma))
				if err != nil {
					return err.(*bizerror.Error).WithScreen(candidate).WithTick(tick)
				}
				oldCost := alloc.BudgetCostKbps
				alloc.SettledScale = nextScale
				alloc.Mapping = mapping
				alloc.BudgetCostKbps = mapping.BudgetCostKbps
				alloc.CommittedTick = tick
				e.active[candidate] = alloc
				e.state.UsedBudget += mapping.BudgetCostKbps - oldCost

				result := results[candidate]
				result.Active = true
				result.Decision = session.DecisionDowngraded
				result.ErrorCode = ""
				result.TriggerSource = trigger
				result.Reason = "TOTAL_BUDGET_REDUCED_BELOW_REQUESTED_ALLOCATION"
				result.Constraint = "C-BUDGET-TOTAL"
				result.SettledScale = nextScale
				mappingCopy := mapping
				result.Mapping = &mappingCopy
				result.BudgetCostKbps = mapping.BudgetCostKbps
				result.BudgetShortfallKbps = overage
				result.BudgetMarginKbps = e.state.TotalBudget - e.state.UsedBudget
				continue
			}
		}

		evictID := e.selectEvictionCandidate()
		if evictID == "" {
			return bizerror.New(bizerror.EAdmissionRejected, "budget cannot be satisfied").WithTick(tick).WithConstraint("C-MIN-QUALITY")
		}
		alloc := e.active[evictID]
		e.state.UsedBudget -= alloc.BudgetCostKbps
		delete(e.active, evictID)
		result := results[evictID]
		result.Active = false
		result.Decision = session.DecisionEvicted
		result.ErrorCode = bizerror.EAdmissionRejected
		result.TriggerSource = trigger
		result.Reason = "BUDGET_BELOW_DECLARED_MINIMUM_QUALITY"
		result.Constraint = "C-MIN-QUALITY"
		result.BudgetShortfallKbps = overage
		result.BudgetMarginKbps = e.state.TotalBudget - e.state.UsedBudget
		result.BudgetCostKbps = 0
		result.Mapping = nil
	}
}

func (e *Engine) applyUpdate(event session.Event, trigger string, tick int64, results map[string]*session.ScreenResult) error {
	if event.Update == nil {
		return bizerror.New(bizerror.EInvalidEdit, "update event has no payload").WithScreen(event.ScreenID).WithTick(tick)
	}
	screen, ok := e.screens[event.ScreenID]
	if !ok {
		return bizerror.New(bizerror.EScreenNotFound, "unknown controlled screen").WithScreen(event.ScreenID).WithTick(tick)
	}
	if event.Update.ScalePermyriad != nil {
		screen.ScalePermyriad = *event.Update.ScalePermyriad
	}
	if event.Update.MinScalePermyriad != nil {
		screen.MinScalePermyriad = *event.Update.MinScalePermyriad
	}
	if event.Update.Chroma != nil {
		screen.Chroma = *event.Update.Chroma
	}
	if err := session.ValidateScale(screen.ScalePermyriad); err != nil {
		return err.(*bizerror.Error).WithScreen(screen.ID).WithTick(tick)
	}
	if err := session.ValidateScale(screen.MinScalePermyriad); err != nil {
		return err.(*bizerror.Error).WithScreen(screen.ID).WithTick(tick)
	}
	if screen.MinScalePermyriad > screen.ScalePermyriad {
		return bizerror.New(bizerror.ESchemaConflict, "minimum scale exceeds requested scale after edit").WithScreen(screen.ID).WithTick(tick)
	}
	if _, _, err := session.ParseChroma(string(screen.Chroma)); err != nil {
		return err.(*bizerror.Error).WithScreen(screen.ID).WithTick(tick)
	}
	e.screens[event.ScreenID] = screen

	result := results[event.ScreenID]
	result.TriggerSource = trigger
	result.RequestedScale = screen.ScalePermyriad

	alloc, active := e.active[event.ScreenID]
	if !active {
		result.Decision = session.DecisionPending
		result.Reason = "CAPABILITY_EDIT_STORED_FOR_NEXT_JOIN"
		return nil
	}

	targetScale := alloc.SettledScale
	if event.Update.ScalePermyriad != nil {
		targetScale = screen.ScalePermyriad
	}
	if targetScale < screen.MinScalePermyriad {
		targetScale = screen.MinScalePermyriad
	}
	mapping, err := e.mapRegion(e.project.Master, screen.Width, screen.Height, targetScale, string(screen.Chroma))
	if err != nil {
		return err.(*bizerror.Error).WithScreen(screen.ID).WithTick(tick)
	}
	oldCost := alloc.BudgetCostKbps
	alloc.RequestedScale = screen.ScalePermyriad
	alloc.SettledScale = targetScale
	alloc.Mapping = mapping
	alloc.BudgetCostKbps = mapping.BudgetCostKbps
	alloc.CommittedTick = tick
	e.active[event.ScreenID] = alloc
	e.state.UsedBudget += mapping.BudgetCostKbps - oldCost

	result.Active = true
	result.Decision = session.DecisionUpdated
	result.Reason = "CONTROLLED_SCREEN_CAPABILITY_EDIT"
	result.Constraint = "C-DEPENDENCY-GRAPH"
	result.SettledScale = targetScale
	result.Mapping = &mapping
	result.BudgetCostKbps = mapping.BudgetCostKbps
	result.BudgetMarginKbps = e.state.TotalBudget - e.state.UsedBudget

	if e.state.UsedBudget > e.state.TotalBudget {
		return e.rebalance(trigger+":budget", tick, results)
	}
	return nil
}

func (e *Engine) selectDowngradeCandidate() string {
	ids := make([]string, 0, len(e.active))
	for id, alloc := range e.active {
		screen := e.screens[id]
		next, ok := session.NextLowerRung(alloc.SettledScale)
		if ok && next >= screen.MinScalePermyriad {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		si, sj := e.screens[ids[i]], e.screens[ids[j]]
		if si.Priority != sj.Priority {
			return si.Priority < sj.Priority
		}
		return ids[i] < ids[j]
	})
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (e *Engine) selectEvictionCandidate() string {
	ids := make([]string, 0, len(e.active))
	for id := range e.active {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := e.screens[ids[i]].Priority, e.screens[ids[j]].Priority
		if pi != pj {
			return pi < pj
		}
		return ids[i] < ids[j]
	})
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func eventTrigger(event session.Event, tick int64) string {
	if event.ID != "" {
		return event.ID
	}
	if event.ScreenID != "" {
		return fmt.Sprintf("tick:%d:%s:%s", tick, event.Type, event.ScreenID)
	}
	return fmt.Sprintf("tick:%d:%s", tick, event.Type)
}

func cloneTopology(topology session.Topology) session.Topology {
	out := topology
	out.Hosts = make([]session.TopologyHost, len(topology.Hosts))
	for i, host := range topology.Hosts {
		out.Hosts[i] = session.TopologyHost{ID: host.ID, ScreenIDs: append([]string(nil), host.ScreenIDs...)}
	}
	return out
}
