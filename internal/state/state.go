// Package state implements the route and segment transition tables.
package state

import (
	"fmt"
	"sync"

	"hdrproof/internal/apperr"
)

type RoutePhase string
type SegmentPhase string

const (
	RoutePending   RoutePhase = "pending"
	RouteRunning   RoutePhase = "running"
	RouteCompleted RoutePhase = "completed"
	RouteFailed    RoutePhase = "failed"
	RouteCancelled RoutePhase = "cancelled"

	SegmentPending   SegmentPhase = "pending"
	SegmentRunning   SegmentPhase = "running"
	SegmentCommitted SegmentPhase = "committed"
	SegmentFailed    SegmentPhase = "failed"
)

type Segment struct {
	Index int
	Phase SegmentPhase
	Error string
}

type Route struct {
	ID       string
	Phase    RoutePhase
	Segments []Segment
}

type Machine struct {
	mu     sync.RWMutex
	order  []string
	routes map[string]*Route
}

func New(routeIDs []string, segmentsByRoute map[string][]int) *Machine {
	m := &Machine{routes: make(map[string]*Route, len(routeIDs))}
	for _, id := range routeIDs {
		r := &Route{ID: id, Phase: RoutePending}
		for _, seg := range segmentsByRoute[id] {
			r.Segments = append(r.Segments, Segment{Index: seg, Phase: SegmentPending})
		}
		m.order = append(m.order, id)
		m.routes[id] = r
	}
	return m
}

func legalRoute(from, to RoutePhase) bool {
	switch from {
	case RoutePending:
		return to == RouteRunning || to == RouteCancelled
	case RouteRunning:
		return to == RouteCompleted || to == RouteFailed || to == RouteCancelled
	case RouteFailed:
		return to == RouteRunning || to == RouteCancelled
	case RouteCancelled:
		return to == RouteRunning || to == RouteFailed
	case RouteCompleted:
		return false
	default:
		return false
	}
}

func legalSegment(from, to SegmentPhase) bool {
	switch from {
	case SegmentPending:
		return to == SegmentRunning || to == SegmentFailed
	case SegmentRunning:
		return to == SegmentCommitted || to == SegmentFailed
	case SegmentFailed:
		return to == SegmentRunning
	case SegmentCommitted:
		return false
	default:
		return false
	}
}

func (m *Machine) TransitionRoute(route string, to RoutePhase) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.routes[route]
	if !ok {
		return apperr.New(apperr.CodeState, route, -1, -1, "route_state", "known route",
			"route does not exist", nil)
	}
	if !legalRoute(r.Phase, to) {
		return apperr.New(apperr.CodeState, route, -1, -1, "route_state",
			string(to), fmt.Sprintf("illegal transition %s -> %s; state unchanged", r.Phase, to), nil)
	}
	r.Phase = to
	return nil
}

func (m *Machine) TransitionSegment(route string, segment int, to SegmentPhase) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.routes[route]
	if !ok {
		return apperr.New(apperr.CodeState, route, segment, -1, "segment_state", "known route",
			"route does not exist", nil)
	}
	for i := range r.Segments {
		if r.Segments[i].Index != segment {
			continue
		}
		if !legalSegment(r.Segments[i].Phase, to) {
			return apperr.New(apperr.CodeState, route, segment, -1, "segment_state",
				string(to), fmt.Sprintf("illegal transition %s -> %s; state unchanged", r.Segments[i].Phase, to), nil)
		}
		r.Segments[i].Phase = to
		if to != SegmentFailed {
			r.Segments[i].Error = ""
		}
		return nil
	}
	return apperr.New(apperr.CodeState, route, segment, -1, "segment_state", "existing segment",
		"segment does not exist", nil)
}

func (m *Machine) SetSegmentError(route string, segment int, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.routes[route]; r != nil {
		for i := range r.Segments {
			if r.Segments[i].Index == segment {
				r.Segments[i].Error = message
			}
		}
	}
}

func (m *Machine) RoutePhase(route string) RoutePhase {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r := m.routes[route]; r != nil {
		return r.Phase
	}
	return ""
}

func (m *Machine) SegmentPhase(route string, segment int) SegmentPhase {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if r := m.routes[route]; r != nil {
		for _, s := range r.Segments {
			if s.Index == segment {
				return s.Phase
			}
		}
	}
	return ""
}

type RouteView struct {
	ID       string        `json:"id"`
	Phase    RoutePhase    `json:"phase"`
	Segments []SegmentView `json:"segments"`
}

type SegmentView struct {
	Index int          `json:"index"`
	Phase SegmentPhase `json:"phase"`
	Error string       `json:"error,omitempty"`
}

func (m *Machine) View() []RouteView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]RouteView, 0, len(m.order))
	for _, id := range m.order {
		r := m.routes[id]
		rv := RouteView{ID: r.ID, Phase: r.Phase}
		for _, s := range r.Segments {
			rv.Segments = append(rv.Segments, SegmentView{Index: s.Index, Phase: s.Phase, Error: s.Error})
		}
		out = append(out, rv)
	}
	return out
}

type Progress struct {
	RoutesCommitted   int `json:"routes_committed"`
	RoutesTotal       int `json:"routes_total"`
	SegmentsCommitted int `json:"segments_committed"`
	SegmentsTotal     int `json:"segments_total"`
}

func (m *Machine) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var p Progress
	p.RoutesTotal = len(m.order)
	for _, id := range m.order {
		r := m.routes[id]
		all := true
		for _, s := range r.Segments {
			p.SegmentsTotal++
			if s.Phase == SegmentCommitted {
				p.SegmentsCommitted++
			} else {
				all = false
			}
		}
		if all && len(r.Segments) > 0 {
			p.RoutesCommitted++
		}
	}
	return p
}

func (m *Machine) MarkFailed(route string, segment int, err error) {
	_ = m.TransitionSegment(route, segment, SegmentFailed)
	m.SetSegmentError(route, segment, err.Error())
}
