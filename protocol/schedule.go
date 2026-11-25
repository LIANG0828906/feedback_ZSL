package protocol

import "sort"

const (
	phaseNegotiate = 10
	phaseTopology  = 20
	phaseLeave     = 30
	phaseJoin      = 40
	phaseControl   = 50
	phaseFrame     = 60
)

func eventPhase(kind EventKind) int {
	switch kind {
	case EventNegotiate:
		return phaseNegotiate
	case EventTopology:
		return phaseTopology
	case EventLeave:
		return phaseLeave
	case EventJoin:
		return phaseJoin
	case EventStart, EventPause, EventResume, EventEnd:
		return phaseControl
	default:
		return phaseControl
	}
}

func BuildSchedule(session *Session) []Action {
	actions := make([]Action, 0, len(session.Events)+len(session.Windows))
	for i, event := range session.Events {
		actions = append(actions, Action{
			Time:        event.Time,
			Phase:       eventPhase(event.Kind),
			Seq:         event.Seq,
			Kind:        event.Kind,
			EventIndex:  i,
			WindowIndex: -1,
		})
	}
	for i, window := range session.Windows {
		actions = append(actions, Action{
			Time:        window.Time,
			Phase:       phaseFrame,
			Kind:        EventKind(ActionFrame),
			Frame:       true,
			EventIndex:  -1,
			WindowIndex: i,
		})
	}
	sort.SliceStable(actions, func(i, j int) bool {
		if actions[i].Time != actions[j].Time {
			return actions[i].Time < actions[j].Time
		}
		if actions[i].Phase != actions[j].Phase {
			return actions[i].Phase < actions[j].Phase
		}
		leftDisplay := ""
		rightDisplay := ""
		if !actions[i].Frame && actions[i].EventIndex >= 0 && actions[i].EventIndex < len(session.Events) {
			leftDisplay = session.Events[actions[i].EventIndex].DisplayID
		}
		if !actions[j].Frame && actions[j].EventIndex >= 0 && actions[j].EventIndex < len(session.Events) {
			rightDisplay = session.Events[actions[j].EventIndex].DisplayID
		}
		if leftDisplay != rightDisplay {
			return leftDisplay < rightDisplay
		}
		if actions[i].Seq != actions[j].Seq {
			return actions[i].Seq < actions[j].Seq
		}
		if actions[i].Frame != actions[j].Frame {
			return !actions[i].Frame
		}
		return actions[i].WindowIndex < actions[j].WindowIndex
	})
	return actions
}

func ExpectedTopologyVersionAt(schedule []Action, next int) int {
	version := 1
	if next > len(schedule) {
		next = len(schedule)
	}
	for i := 0; i < next; i++ {
		if !schedule[i].Frame && schedule[i].Kind.IsTopologyAffecting() {
			version++
		}
	}
	return version
}
