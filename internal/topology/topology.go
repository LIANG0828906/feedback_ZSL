package topology

import (
	"encoding/json"
	"fmt"
	"sort"

	"screenrelay/internal/model"
)

type Suppressed struct {
	EventID  string          `json:"event_id"`
	Kind     model.EventKind `json:"kind"`
	Revision int64           `json:"revision"`
	Reason   string          `json:"reason"`
}

type Decision struct {
	Tick           int64           `json:"tick"`
	ScreenID       string          `json:"screen_id"`
	WinnerID       string          `json:"winner_id"`
	WinnerKind     model.EventKind `json:"winner_kind"`
	WinnerRevision int64           `json:"winner_revision"`
	Suppressed     []Suppressed    `json:"suppressed"`
	Basis          string          `json:"basis"`
}

type State struct {
	Screens map[string]model.Screen `json:"screens"`
}

func NewState(initial []model.Screen) *State {
	s := &State{Screens: map[string]model.Screen{}}
	for _, screen := range initial {
		s.Screens[screen.ID] = screen
	}
	return s
}

func (s *State) Clone() *State {
	clone := &State{Screens: make(map[string]model.Screen, len(s.Screens))}
	for id, screen := range s.Screens {
		scales := append([]model.ScaleTier(nil), screen.SupportedScales...)
		chromas := append([]model.Chroma(nil), screen.SupportedChromas...)
		screen.SupportedScales = scales
		screen.SupportedChromas = chromas
		clone.Screens[id] = screen
	}
	return clone
}

func (s *State) IDs() []string {
	ids := make([]string, 0, len(s.Screens))
	for id := range s.Screens {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *State) SortedScreens() []model.Screen {
	ids := s.IDs()
	out := make([]model.Screen, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.Screens[id])
	}
	return out
}

func (s *State) Apply(event model.TopologyEvent) error {
	switch event.Kind {
	case model.EventLeave:
		delete(s.Screens, event.ScreenID)
	case model.EventJoin, model.EventTopology:
		if event.Screen == nil {
			return model.NewError(model.EInternal, "event %s missing screen payload", event.ID)
		}
		s.Screens[event.ScreenID] = *event.Screen
	default:
		return model.SchemaError("$.events", "unknown event kind %q", event.Kind)
	}
	return nil
}

type groupKey struct {
	tick     int64
	screenID string
}

func Adjudicate(events []model.TopologyEvent) ([]Decision, []model.TopologyEvent, error) {
	groups := map[groupKey][]model.TopologyEvent{}
	keys := []groupKey{}
	for _, event := range events {
		key := groupKey{tick: event.Tick, screenID: event.ScreenID}
		if _, ok := groups[key]; !ok {
			keys = append(keys, key)
		}
		groups[key] = append(groups[key], event)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tick != keys[j].tick {
			return keys[i].tick < keys[j].tick
		}
		return keys[i].screenID < keys[j].screenID
	})

	decisions := make([]Decision, 0, len(keys))
	winners := make([]model.TopologyEvent, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		winner, suppressed, basis, err := selectWinner(group)
		if err != nil {
			return nil, nil, err
		}
		decisions = append(decisions, Decision{
			Tick: key.tick, ScreenID: key.screenID,
			WinnerID: winner.ID, WinnerKind: winner.Kind, WinnerRevision: winner.Revision,
			Suppressed: suppressed, Basis: basis,
		})
		winners = append(winners, winner)
	}
	sort.Slice(winners, func(i, j int) bool {
		if winners[i].Tick != winners[j].Tick {
			return winners[i].Tick < winners[j].Tick
		}
		if rank(winners[i].Kind) != rank(winners[j].Kind) {
			return rank(winners[i].Kind) < rank(winners[j].Kind)
		}
		return winners[i].ID < winners[j].ID
	})
	return decisions, winners, nil
}

func rank(kind model.EventKind) int {
	switch kind {
	case model.EventLeave:
		return 0
	case model.EventJoin:
		return 1
	case model.EventTopology:
		return 2
	default:
		return 99
	}
}

func selectWinner(group []model.TopologyEvent) (model.TopologyEvent, []Suppressed, string, error) {
	if len(group) == 1 {
		return group[0], nil, "single event at this logical tick and screen", nil
	}

	sorted := append([]model.TopologyEvent(nil), group...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ri, rj := rank(sorted[i].Kind), rank(sorted[j].Kind)
		if ri != rj {
			return ri < rj
		}
		if sorted[i].Revision != sorted[j].Revision {
			return sorted[i].Revision > sorted[j].Revision
		}
		return sorted[i].ID < sorted[j].ID
	})

	winner := sorted[0]
	suppressed := []Suppressed{}
	basis := "priority leave > join > topology; within category higher revision wins; identical revision and payload is idempotent"
	for _, candidate := range sorted[1:] {
		reason := ""
		if candidate.Kind != winner.Kind {
			reason = fmt.Sprintf("lower category priority than %s", winner.Kind)
		} else if candidate.Revision < winner.Revision {
			reason = fmt.Sprintf("revision %d is older than winning revision %d", candidate.Revision, winner.Revision)
		} else if candidate.Revision == winner.Revision {
			same, err := samePayload(winner, candidate)
			if err != nil {
				return winner, nil, "", err
			}
			if !same {
				return winner, nil, "", &model.Error{
					Code: model.EConflict, ScreenID: winner.ScreenID,
					Field: "events", Path: fmt.Sprintf("$.events[tick=%d,screen=%s]", winner.Tick, winner.ScreenID),
					Message: "same category, logical tick, revision and screen carry different payloads",
					Actual:  fmt.Sprintf("events=%s,%s", winner.ID, candidate.ID),
					Details: map[string]any{"winner": winner.ID, "candidate": candidate.ID, "tick": winner.Tick, "revision": winner.Revision},
				}
			}
			reason = "identical revision and canonical payload; idempotent duplicate suppressed by event id tie-break"
		} else {
			reason = "deterministic event id tie-break"
		}
		suppressed = append(suppressed, Suppressed{
			EventID: candidate.ID, Kind: candidate.Kind, Revision: candidate.Revision, Reason: reason,
		})
	}

	if winner.Kind == model.EventTopology {
		for _, candidate := range group {
			if candidate.Kind == model.EventJoin {
				basis = "join establishes membership before same-tick topology; join wins and topology is deferred/suppressed"
				break
			}
		}
	}
	if winner.Kind == model.EventLeave && len(suppressed) > 0 {
		basis = "safety priority: leave wins at the same logical tick, so no packet is sent to a potentially exited screen"
	}
	return winner, suppressed, basis, nil
}

func samePayload(a, b model.TopologyEvent) (bool, error) {
	ca, err := canonicalScreen(a.Screen)
	if err != nil {
		return false, err
	}
	cb, err := canonicalScreen(b.Screen)
	if err != nil {
		return false, err
	}
	return ca == cb, nil
}

func canonicalScreen(screen *model.Screen) (string, error) {
	if screen == nil {
		return "", nil
	}
	data, err := json.Marshal(screen)
	if err != nil {
		return "", model.NewError(model.EInternal, "canonical screen fingerprint: %v", err)
	}
	return string(data), nil
}

func PendingThrough(events []model.TopologyEvent, throughTick int64, applied map[string]bool) []model.TopologyEvent {
	out := []model.TopologyEvent{}
	for _, event := range events {
		if event.Tick <= throughTick && !applied[event.ID] {
			out = append(out, event)
		}
	}
	return out
}

func MarkApplied(applied map[string]bool, events ...model.TopologyEvent) {
	for _, event := range events {
		applied[event.ID] = true
	}
}

var _ = sort.Strings
