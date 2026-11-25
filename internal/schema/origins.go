package schema

import (
	"sort"

	"obsproj/internal/model"
)

type originChange struct {
	At     int64
	Origin model.Point
}

// buildOriginTimeline mirrors the event arbitration used by the kernel for the
// subset of events that can change a screen origin. It is used only by the v2
// offset codec, whose frame coordinates are relative to the effective origin at
// that frame's logical time.
func buildOriginTimeline(session *model.Session) map[string][]originChange {
	byScreenTime := make(map[string]map[int64][]model.Event)
	for _, event := range session.Events {
		if byScreenTime[event.ScreenID] == nil {
			byScreenTime[event.ScreenID] = make(map[int64][]model.Event)
		}
		byScreenTime[event.ScreenID][event.At] = append(byScreenTime[event.ScreenID][event.At], event)
	}
	out := make(map[string][]originChange)
	for screenID, byTime := range byScreenTime {
		times := make([]int64, 0, len(byTime))
		for at := range byTime {
			times = append(times, at)
		}
		sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
		for _, at := range times {
			events := byTime[at]
			sort.Slice(events, func(i, j int) bool {
				if events[i].Kind.Priority() != events[j].Kind.Priority() {
					return events[i].Kind.Priority() > events[j].Kind.Priority()
				}
				return events[i].ID < events[j].ID
			})
			winner := events[0]
			if winner.Topology == nil || winner.Topology.Origin == nil {
				continue
			}
			if winner.Kind != model.EventJoin && winner.Kind != model.EventTopology {
				continue
			}
			out[screenID] = append(out[screenID], originChange{At: at, Origin: *winner.Topology.Origin})
		}
	}
	return out
}

func originAt(initial model.Point, timeline []originChange, at int64) model.Point {
	out := initial
	for _, change := range timeline {
		if change.At > at {
			break
		}
		out = change.Origin
	}
	return out
}
