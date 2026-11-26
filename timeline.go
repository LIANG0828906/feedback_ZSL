package mirror

import (
	"fmt"
	"sort"
)

type ArbitrationRecord struct {
	Tick          int64     `json:"tick"`
	EventID       string    `json:"eventId"`
	EventKind     EventKind `json:"eventKind"`
	ScreenID      string    `json:"screenId,omitempty"`
	WinnerID      string    `json:"winnerId"`
	SuppressedIDs []string  `json:"suppressedIds,omitempty"`
	Outcome       string    `json:"outcome"`
	Basis         string    `json:"basis"`
}

type Endpoint struct {
	ScreenID       string      `json:"screenId"`
	Domain         string      `json:"domain"`
	Status         ScreenState `json:"status"`
	JoinedTick     int64       `json:"joinedTick"`
	NegotiatedTick int64       `json:"negotiatedTick,omitempty"`
	Generation     int         `json:"generation"`
}

type EndpointSnapshot struct {
	WindowID  string     `json:"windowId"`
	Tick      int64      `json:"tick"`
	Endpoints []Endpoint `json:"endpoints"`
}

type WindowInvalidation struct {
	ScreenID string `json:"screenId"`
	WindowID string `json:"windowId"`
	Tick     int64  `json:"tick"`
	Reason   string `json:"reason"`
}

type ScreenGeneration struct {
	ScreenID   string `json:"screenId"`
	Generation int    `json:"generation"`
}

// ArbitrateEvents applies a deterministic total order to same-tick events.
// The priority is deliberately data-driven and independent of map iteration.
func ArbitrateEvents(events []Event) ([]Event, []ArbitrationRecord, error) {
	ordered := append([]Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].AtTick != ordered[j].AtTick {
			return ordered[i].AtTick < ordered[j].AtTick
		}
		if eventPriority(ordered[i].Kind) != eventPriority(ordered[j].Kind) {
			return eventPriority(ordered[i].Kind) > eventPriority(ordered[j].Kind)
		}
		return ordered[i].ID < ordered[j].ID
	})
	applied := []Event{}
	records := []ArbitrationRecord{}
	for i := 0; i < len(ordered); {
		j := i + 1
		for j < len(ordered) && ordered[j].AtTick == ordered[i].AtTick {
			j++
		}
		group := ordered[i:j]
		groupStart := len(records)
		records = append(records, make([]ArbitrationRecord, len(group))...)
		winnerByScreen := map[string]int{}
		seenIDs := map[string]bool{}
		for k := range group {
			ev := group[k]
			recordIdx := groupStart + k
			records[recordIdx] = ArbitrationRecord{
				Tick: ev.AtTick, EventID: ev.ID, EventKind: ev.Kind, ScreenID: ev.ScreenID,
				WinnerID: ev.ID, Outcome: "applied",
				Basis: fmt.Sprintf("priority %s(%d); tie-break event-id", ev.Kind, eventPriority(ev.Kind)),
			}
			if seenIDs[ev.ID] {
				ids := eventIDs(group)
				return nil, records, &Error{Code: CodeArbitration, Path: "events",
					Tick: ev.AtTick, Events: ids,
					Message: "duplicate event id makes same-tick arbitration ambiguous"}
			}
			seenIDs[ev.ID] = true
			if eventPriority(ev.Kind) == 0 {
				return nil, records, &Error{Code: CodeArbitration, Path: "events." + ev.ID,
					Tick: ev.AtTick, Events: eventIDs(group), Message: "unknown event kind"}
			}
			if ev.ScreenID == "" {
				applied = append(applied, ev)
				continue
			}
			if winnerIdx, ok := winnerByScreen[ev.ScreenID]; ok {
				winner := group[winnerIdx]
				records[recordIdx].Outcome = "suppressed"
				records[recordIdx].WinnerID = winner.ID
				records[recordIdx].Basis = fmt.Sprintf("screen %s already won by %s with priority %d > %d; tie-break event-id",
					ev.ScreenID, winner.ID, eventPriority(winner.Kind), eventPriority(ev.Kind))
				records[groupStart+winnerIdx].SuppressedIDs = append(records[groupStart+winnerIdx].SuppressedIDs, ev.ID)
				continue
			}
			winnerByScreen[ev.ScreenID] = k
			applied = append(applied, ev)
		}
		i = j
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Tick != records[j].Tick {
			return records[i].Tick < records[j].Tick
		}
		return records[i].EventID < records[j].EventID
	})
	return applied, records, nil
}

func eventIDs(events []Event) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.ID)
	}
	sort.Strings(out)
	return out
}

// Timeline advances endpoint and topology evolution using only declared
// logical ticks and the deterministic arbitration result.
type Timeline struct {
	Session      *Session
	active       map[string]Endpoint
	generations  map[string]int
	cursor       int
	arbitrations []ArbitrationRecord
	invalid      []WindowInvalidation
	lastTick     int64
}

func NewTimeline(session *Session) *Timeline {
	return &Timeline{Session: session, active: map[string]Endpoint{}, generations: map[string]int{}}
}

// Restore seeds the timeline from a committed checkpoint. Only the durable
// endpoint set and event cursor are used; no uncommitted event is replayed.
func (t *Timeline) Restore(endpoints []Endpoint, cursor int) error {
	return t.RestoreWithGenerations(endpoints, cursor, nil)
}

func (t *Timeline) RestoreWithGenerations(endpoints []Endpoint, cursor int, generations []ScreenGeneration) error {
	if cursor < 0 || cursor > len(t.Session.Events) {
		return &Error{Code: CodeState, Path: "checkpoint.events_consumed",
			Message: fmt.Sprintf("event cursor %d is out of range", cursor)}
	}
	t.active = map[string]Endpoint{}
	t.generations = map[string]int{}
	for _, g := range generations {
		if g.Generation > t.generations[g.ScreenID] {
			t.generations[g.ScreenID] = g.Generation
		}
	}
	for _, e := range endpoints {
		if _, ok := t.Session.ScreenByID(e.ScreenID); !ok {
			return &Error{Code: CodeState, Path: "checkpoint.endpoints",
				Message: "unknown screen " + e.ScreenID}
		}
		t.active[e.ScreenID] = e
		if e.Generation > t.generations[e.ScreenID] {
			t.generations[e.ScreenID] = e.Generation
		}
	}
	t.cursor = cursor
	if cursor > 0 {
		t.lastTick = t.Session.Events[cursor-1].AtTick
	}
	return nil
}

func (t *Timeline) EventsConsumed() int { return t.cursor }

func (t *Timeline) Generations() []ScreenGeneration {
	ids := make([]string, 0, len(t.generations))
	for id := range t.generations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ScreenGeneration, 0, len(ids))
	for _, id := range ids {
		out = append(out, ScreenGeneration{ScreenID: id, Generation: t.generations[id]})
	}
	return out
}

func (t *Timeline) AdvanceTo(tick int64) error {
	for t.cursor < len(t.Session.Events) && t.Session.Events[t.cursor].AtTick <= tick {
		start := t.cursor
		end := start + 1
		for end < len(t.Session.Events) && t.Session.Events[end].AtTick == t.Session.Events[start].AtTick {
			end++
		}
		group := t.Session.Events[start:end]
		applied, records, err := ArbitrateEvents(group)
		if err != nil {
			return err
		}
		t.arbitrations = append(t.arbitrations, records...)
		for _, ev := range applied {
			if err := t.applyEvent(ev); err != nil {
				return err
			}
		}
		t.cursor = end
		t.lastTick = tick
	}
	return nil
}

func (t *Timeline) applyEvent(ev Event) error {
	switch ev.Kind {
	case EventJoin:
		if _, ok := t.Session.ScreenByID(ev.ScreenID); !ok {
			return errf(CodeSchema, "events."+ev.ID+".screen_id", "unknown screen %q", ev.ScreenID)
		}
		domain := ev.Domain
		if domain == "" {
			if sc, ok := t.Session.ScreenByID(ev.ScreenID); ok {
				domain = sc.Domain
			}
		}
		if _, ok := t.Session.DomainByID(domain); !ok {
			return errf(CodeSchema, "events."+ev.ID+".domain", "unknown domain %q", domain)
		}
		t.active[ev.ScreenID] = Endpoint{
			ScreenID: ev.ScreenID, Domain: domain, Status: ScreenPending, JoinedTick: ev.AtTick,
			Generation: t.nextGeneration(ev.ScreenID),
		}
	case EventNegotiate:
		e, ok := t.active[ev.ScreenID]
		if !ok {
			return nil
		}
		e.Status = ScreenCommitted
		e.NegotiatedTick = ev.AtTick
		e.Generation = t.nextGeneration(ev.ScreenID)
		t.active[ev.ScreenID] = e
	case EventLeave:
		if _, ok := t.active[ev.ScreenID]; !ok {
			return nil
		}
		delete(t.active, ev.ScreenID)
		for _, w := range t.Session.Windows {
			if w.StartTick <= ev.AtTick && ev.AtTick <= w.EndTick {
				t.invalid = append(t.invalid, WindowInvalidation{
					ScreenID: ev.ScreenID, WindowID: w.ID, Tick: ev.AtTick,
					Reason: "screen left during an uncommitted window",
				})
			}
		}
	case EventTopology:
		e, ok := t.active[ev.ScreenID]
		if !ok {
			return nil
		}
		target := ev.ToDomain
		if target == "" {
			target = ev.Domain
		}
		if target == "" {
			return errf(CodeSchema, "events."+ev.ID+".to_domain", "topology event has no target domain")
		}
		if _, ok := t.Session.DomainByID(target); !ok {
			return errf(CodeSchema, "events."+ev.ID+".to_domain", "unknown domain %q", target)
		}
		e.Domain = target
		t.active[ev.ScreenID] = e
	default:
		return errf(CodeSchema, "events."+ev.ID+".kind", "unknown event kind")
	}
	return nil
}

func (t *Timeline) nextGeneration(screenID string) int {
	current := t.generations[screenID]
	if e, ok := t.active[screenID]; ok && e.Generation > current {
		current = e.Generation
	}
	current++
	t.generations[screenID] = current
	return current
}

func (t *Timeline) Snapshot(windowID string, tick int64) EndpointSnapshot {
	ids := make([]string, 0, len(t.active))
	for id := range t.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := EndpointSnapshot{WindowID: windowID, Tick: tick, Endpoints: make([]Endpoint, 0, len(ids))}
	for _, id := range ids {
		out.Endpoints = append(out.Endpoints, t.active[id])
	}
	return out
}

func (t *Timeline) EligibleScreens() []Screen {
	ids := make([]string, 0, len(t.active))
	for id, e := range t.active {
		if e.Status == ScreenCommitted {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]Screen, 0, len(ids))
	for _, id := range ids {
		sc, ok := t.Session.ScreenByID(id)
		if ok {
			out = append(out, sc)
		}
	}
	return out
}

func (t *Timeline) InvalidationFor(screenID, windowID string) (WindowInvalidation, bool) {
	for _, inv := range t.invalid {
		if inv.ScreenID == screenID && inv.WindowID == windowID {
			return inv, true
		}
	}
	return WindowInvalidation{}, false
}

func (t *Timeline) Arbitrations() []ArbitrationRecord {
	return append([]ArbitrationRecord(nil), t.arbitrations...)
}

func (t *Timeline) Invalidations() []WindowInvalidation {
	return append([]WindowInvalidation(nil), t.invalid...)
}

func (t *Timeline) LastTick() int64 { return t.lastTick }
