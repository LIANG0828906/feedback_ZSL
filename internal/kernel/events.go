package kernel

import (
	"fmt"
	"sort"

	"obsproj/internal/model"
)

type eventOutcome struct {
	decisions       []model.Decision
	newJoins        []string
	exited          []string
	topologyVersion int64
	cursor          int
}

func (e *Engine) processEvents(window int, at int64) (eventOutcome, *model.KernelError) {
	out := eventOutcome{
		topologyVersion: e.topology,
		cursor:          e.eventCursor,
	}
	events := e.session.Events
	for out.cursor < len(events) && events[out.cursor].At <= at {
		groupAt := events[out.cursor].At
		var group []model.Event
		for out.cursor < len(events) && events[out.cursor].At == groupAt {
			group = append(group, events[out.cursor])
			out.cursor++
		}
		byScreen := make(map[string][]model.Event)
		for _, event := range group {
			byScreen[event.ScreenID] = append(byScreen[event.ScreenID], event)
		}
		screenIDs := make([]string, 0, len(byScreen))
		for screenID := range byScreen {
			screenIDs = append(screenIDs, screenID)
		}
		sort.Strings(screenIDs)
		for _, screenID := range screenIDs {
			eventsForScreen := byScreen[screenID]
			sort.Slice(eventsForScreen, func(i, j int) bool {
				a, b := eventsForScreen[i], eventsForScreen[j]
				if a.Kind.Priority() != b.Kind.Priority() {
					return a.Kind.Priority() > b.Kind.Priority()
				}
				return a.ID < b.ID
			})
			winner := eventsForScreen[0]
			for _, candidate := range eventsForScreen[1:] {
				if candidate.ID == winner.ID {
					return out, &model.KernelError{
						Code:     model.CodeConflict,
						Message:  "events at the same tick cannot be ordered because ids are identical",
						Window:   window,
						ScreenID: screenID,
						Field:    "events.event_id",
					}
				}
			}
			suppressed := make([]string, 0, len(eventsForScreen)-1)
			for _, candidate := range eventsForScreen[1:] {
				suppressed = append(suppressed, candidate.ID)
			}
			sort.Strings(suppressed)
			rationale := fmt.Sprintf("fixed priority exit(40) > join(30) > topology(20); ties use lexicographically smallest event id; category=%s priority=%d",
				winner.Kind, winner.Kind.Priority())
			if len(suppressed) == 0 {
				rationale = fmt.Sprintf("single event for screen at tick; category=%s priority=%d", winner.Kind, winner.Kind.Priority())
			}
			out.decisions = append(out.decisions, model.Decision{
				Window:     window,
				At:         groupAt,
				ScreenID:   screenID,
				Category:   winner.Kind,
				Winner:     winner.ID,
				Suppressed: suppressed,
				Rationale:  rationale,
			})
			screen, ok := e.screens[screenID]
			if !ok {
				return out, &model.KernelError{
					Code:     model.CodeCapacity,
					Message:  "event references an unknown screen",
					Window:   window,
					ScreenID: screenID,
					Field:    "events.screen_id",
				}
			}
			wasActive := e.active[screenID]
			switch winner.Kind {
			case model.EventJoin:
				e.active[screenID] = true
				if !wasActive {
					out.newJoins = append(out.newJoins, screenID)
				}
				if winner.Topology != nil {
					if patchErr := applyTopologyPatch(&screen, winner.Topology); patchErr != nil {
						patchErr.Window = window
						patchErr.ScreenID = screenID
						return out, patchErr
					}
					e.screens[screenID] = screen
					out.topologyVersion++
				}
				e.activateScreenFSM(screenID)
			case model.EventExit:
				e.active[screenID] = false
				if wasActive {
					out.exited = append(out.exited, screenID)
				}
			case model.EventTopology:
				if winner.Topology == nil {
					return out, &model.KernelError{
						Code:     model.CodeCapacity,
						Message:  "topology event has no patch",
						Window:   window,
						ScreenID: screenID,
						Field:    "events.topology",
					}
				}
				if patchErr := applyTopologyPatch(&screen, winner.Topology); patchErr != nil {
					patchErr.Window = window
					patchErr.ScreenID = screenID
					return out, patchErr
				}
				e.screens[screenID] = screen
				out.topologyVersion++
			}
		}
	}
	sort.Strings(out.newJoins)
	sort.Strings(out.exited)
	return out, nil
}

func (e *Engine) activateScreenFSM(screenID string) {
	fsm := e.screenFSM[screenID]
	if fsm == nil {
		fsm = model.NewScreenFSM()
		e.screenFSM[screenID] = fsm
	}
	if fsm.State == model.ScreenPending {
		if err := fsm.Transition(model.ScreenNegotiating); err == nil {
			_ = fsm.Transition(model.ScreenCommitted)
		}
	}
}

func applyTopologyPatch(screen *model.Screen, patch *model.TopologyPatch) *model.KernelError {
	if patch.CapabilityScales != nil {
		screen.Capability.Scales = append([]model.Scale(nil), (*patch.CapabilityScales)...)
	}
	if patch.CapabilityChromas != nil {
		screen.Capability.Chromas = append([]model.Chroma(nil), (*patch.CapabilityChromas)...)
	}
	if patch.MaxPacketSize != nil {
		if *patch.MaxPacketSize <= 0 {
			return &model.KernelError{
				Code:     model.CodeCapacity,
				Message:  "topology set an invalid max packet size",
				ScreenID: screen.ID,
				Field:    "topology.max_packet_size",
			}
		}
		screen.Capability.MaxPacketSize = *patch.MaxPacketSize
	}
	if patch.Origin != nil {
		screen.Origin = *patch.Origin
	}
	if patch.Resolution != nil {
		if patch.Resolution.W <= 0 || patch.Resolution.H <= 0 {
			return &model.KernelError{
				Code:     model.CodeCapacity,
				Message:  "topology set an invalid resolution",
				ScreenID: screen.ID,
				Field:    "topology.resolution",
			}
		}
		screen.Resolution = *patch.Resolution
	}
	if patch.Scale != nil {
		screen.Scale = *patch.Scale
	}
	if patch.Chroma != nil {
		screen.Chroma = *patch.Chroma
	}
	if !screen.AllowsScale(screen.Scale) {
		return &model.KernelError{
			Code:           model.CodeCapacity,
			Message:        "topology leaves selected scale outside capability declaration",
			ScreenID:       screen.ID,
			RequestedScale: &screen.Scale,
			AllowedScales:  append([]model.Scale(nil), screen.Capability.Scales...),
			Field:          "topology.scale",
		}
	}
	if !screen.AllowsChroma(screen.Chroma) {
		return &model.KernelError{
			Code:            model.CodeCapacity,
			Message:         "topology leaves selected chroma outside capability declaration",
			ScreenID:        screen.ID,
			RequestedChroma: &screen.Chroma,
			AllowedChromas:  append([]model.Chroma(nil), screen.Capability.Chromas...),
			Field:           "topology.chroma",
		}
	}
	return nil
}
