package session

import (
	"fmt"

	"mirrorcast/internal/bizerror"
)

const MaxTotalBudgetKbps int64 = 1_000_000_000_000_000

func ValidateProject(project *Project) error {
	if project.ProjectID == "" {
		return bizerror.New(bizerror.EFieldRequired, "project_id is required").WithPath("project_id")
	}
	if err := ValidateMaster(project.Master); err != nil {
		return err
	}
	if project.TotalBudget < 0 || project.TotalBudget > MaxTotalBudgetKbps {
		return bizerror.New(bizerror.EBudgetOutOfRange, "total budget is outside declared range").
			WithPath("total_budget_kbps").
			WithDetail("value", project.TotalBudget).
			WithDetail("min", 0).
			WithDetail("max", MaxTotalBudgetKbps)
	}
	if project.Threads < MinThreads || project.Threads > MaxThreads {
		return bizerror.New(bizerror.EThreadOutOfRange, "dispatch threads is outside declared range").
			WithPath("dispatch_threads").
			WithDetail("value", project.Threads).
			WithDetail("min", MinThreads).
			WithDetail("max", MaxThreads)
	}
	if project.BufferBudget < 1 || project.BufferBudget > MaxBufferBudget {
		return bizerror.New(bizerror.EBufferBudgetOutOfRange, "buffer budget is outside declared range").
			WithPath("buffer_budget_units").
			WithDetail("value", project.BufferBudget).
			WithDetail("min", 1).
			WithDetail("max", MaxBufferBudget)
	}

	seen := map[string]bool{}
	for i := range project.Screens {
		screen := &project.Screens[i]
		path := fmt.Sprintf("screens[%d]", i)
		if screen.ID == "" {
			return bizerror.New(bizerror.EFieldRequired, "screen id is required").WithPath(path + ".id")
		}
		if seen[screen.ID] {
			return bizerror.New(bizerror.EScreenDuplicate, "duplicate controlled screen id").WithPath(path + ".id").WithScreen(screen.ID)
		}
		seen[screen.ID] = true
		if screen.Host == "" {
			return bizerror.New(bizerror.EFieldRequired, "host id is required").WithPath(path + ".host").WithScreen(screen.ID)
		}
		if err := ValidateResolution(screen.Width, screen.Height); err != nil {
			return wrapContext(err, path+".resolution", screen.ID)
		}
		if err := ValidateScale(screen.ScalePermyriad); err != nil {
			return wrapContext(err, path+".scale", screen.ID)
		}
		if err := ValidateScale(screen.MinScalePermyriad); err != nil {
			return wrapContext(err, path+".min_scale", screen.ID)
		}
		if screen.MinScalePermyriad > screen.ScalePermyriad {
			return bizerror.New(bizerror.ESchemaConflict, "minimum scale cannot exceed requested scale").
				WithPath(path+".min_scale").
				WithScreen(screen.ID).
				WithDetail("requested_permyriad", screen.ScalePermyriad).
				WithDetail("minimum_permyriad", screen.MinScalePermyriad)
		}
		if _, _, err := ParseChroma(string(screen.Chroma)); err != nil {
			return err.(*bizerror.Error).WithPath(path + ".chroma").WithScreen(screen.ID)
		}
		if _, err := MapRegion(project.Master, screen.Width, screen.Height, screen.ScalePermyriad, string(screen.Chroma)); err != nil {
			return err.(*bizerror.Error).WithScreen(screen.ID).WithPath(path)
		}
		if _, err := MapRegion(project.Master, screen.Width, screen.Height, screen.MinScalePermyriad, string(screen.Chroma)); err != nil {
			return err.(*bizerror.Error).WithScreen(screen.ID).WithPath(path + ".min_scale")
		}
	}

	screenIDs := map[string]bool{}
	for _, screen := range project.Screens {
		screenIDs[screen.ID] = true
	}
	for i, event := range project.Events {
		path := fmt.Sprintf("events[%d]", i)
		if event.Tick < 0 {
			return bizerror.New(bizerror.ETopologyEventInvalid, "logical tick cannot be negative").WithPath(path + ".tick").WithTick(event.Tick)
		}
		switch event.Type {
		case EventJoin, EventLeave, EventUpdate:
			if event.ScreenID == "" {
				return bizerror.New(bizerror.EFieldRequired, "screen event requires screen_id").WithPath(path + ".screen_id")
			}
			if !screenIDs[event.ScreenID] {
				return bizerror.New(bizerror.EScreenNotFound, "event references unknown screen").WithPath(path + ".screen_id").WithScreen(event.ScreenID).WithTick(event.Tick)
			}
			if event.Type == EventUpdate {
				if event.Update == nil {
					return bizerror.New(bizerror.EInvalidEdit, "update event requires update payload").WithPath(path + ".update").WithTick(event.Tick)
				}
				if event.Update.ScalePermyriad != nil {
					if err := ValidateScale(*event.Update.ScalePermyriad); err != nil {
						be, _ := bizerror.As(err)
						return be.WithPath(path + ".update.scale_permyriad").WithScreen(event.ScreenID).WithTick(event.Tick)
					}
				}
				if event.Update.MinScalePermyriad != nil {
					if err := ValidateScale(*event.Update.MinScalePermyriad); err != nil {
						be, _ := bizerror.As(err)
						return be.WithPath(path + ".update.min_scale_permyriad").WithScreen(event.ScreenID).WithTick(event.Tick)
					}
				}
				if event.Update.Chroma != nil {
					if _, _, err := ParseChroma(string(*event.Update.Chroma)); err != nil {
						return err.(*bizerror.Error).WithPath(path + ".update.chroma").WithScreen(event.ScreenID)
					}
				}
			}
		case EventBudget:
			if event.Budget < 0 || event.Budget > MaxTotalBudgetKbps {
				return bizerror.New(bizerror.EBudgetOutOfRange, "budget event is outside declared range").WithPath(path + ".budget_kbps").WithTick(event.Tick)
			}
		case EventTopology:
			if event.Topology == nil {
				return bizerror.New(bizerror.ETopologyEventInvalid, "topology event requires a topology snapshot").WithPath(path + ".topology").WithTick(event.Tick)
			}
			if err := ValidateTopology(*event.Topology, project.Screens); err != nil {
				return err.(*bizerror.Error).WithPath(path + ".topology").WithTick(event.Tick)
			}
		default:
			return bizerror.New(bizerror.ETopologyEventInvalid, "unknown event type").WithPath(path+".type").WithDetail("type", event.Type)
		}
	}
	return nil
}

func ValidateTopology(topology Topology, screens []Screen) error {
	known := map[string]bool{}
	for _, screen := range screens {
		known[screen.ID] = true
	}
	seen := map[string]bool{}
	hostSeen := map[string]bool{}
	count := 0
	for hi, host := range topology.Hosts {
		if host.ID == "" {
			return bizerror.New(bizerror.ETopologyEventInvalid, "topology host id is required").
				WithPath(fmt.Sprintf("hosts[%d].id", hi))
		}
		if hostSeen[host.ID] {
			return bizerror.New(bizerror.ETopologyEventInvalid, "duplicate topology host").
				WithPath(fmt.Sprintf("hosts[%d].id", hi)).WithDetail("host", host.ID)
		}
		hostSeen[host.ID] = true
		for _, id := range host.ScreenIDs {
			if !known[id] {
				return bizerror.New(bizerror.EScreenNotFound, "topology references unknown screen").WithScreen(id)
			}
			if seen[id] {
				return bizerror.New(bizerror.ETopologyEventInvalid, "screen appears more than once in topology").WithScreen(id)
			}
			seen[id] = true
			count++
		}
	}
	if count != len(screens) {
		return bizerror.New(bizerror.ETopologyEventInvalid, "topology must contain every known screen exactly once").
			WithDetail("topology_count", count).WithDetail("screen_count", len(screens))
	}
	return nil
}

func InitialTopology(screens []Screen) Topology {
	hostOrder := []string{}
	hostScreens := map[string][]string{}
	for _, screen := range screens {
		if _, ok := hostScreens[screen.Host]; !ok {
			hostOrder = append(hostOrder, screen.Host)
		}
		hostScreens[screen.Host] = append(hostScreens[screen.Host], screen.ID)
	}
	hosts := make([]TopologyHost, 0, len(hostOrder))
	for _, host := range hostOrder {
		ids := append([]string(nil), hostScreens[host]...)
		ids = sortedStrings(ids)
		hosts = append(hosts, TopologyHost{ID: host, ScreenIDs: ids})
	}
	return Topology{Version: 1, Hosts: hosts}
}

func sortedStrings(values []string) []string {
	for i := 0; i < len(values); i++ {
		for j := i + 1; j < len(values); j++ {
			if values[j] < values[i] {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
	return values
}

func wrapContext(err error, path, screenID string) error {
	if be, ok := bizerror.As(err); ok {
		return be.WithPath(path).WithScreen(screenID)
	}
	return bizerror.New(bizerror.EInternal, err.Error()).WithPath(path).WithScreen(screenID)
}
