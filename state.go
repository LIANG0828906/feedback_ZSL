package dialoguecal

import (
	"fmt"
	"sort"
	"sync"
)

type SegmentState string

const (
	SegmentPendingLoad SegmentState = "pending_load"
	SegmentExtracting  SegmentState = "extracting"
	SegmentCalibrating SegmentState = "calibrating"
	SegmentCommitted   SegmentState = "committed"
	SegmentFailed      SegmentState = "failed"
)

type IntervalState string

const (
	IntervalPending    IntervalState = "pending"
	IntervalProcessing IntervalState = "processing"
	IntervalCommitted  IntervalState = "committed"
	IntervalFailed     IntervalState = "failed"
)

type StateRecord struct {
	Key       string
	Segment   SegmentState
	Interval  IntervalState
	Progress  int64
	Sequence  int64
	LastError string
}

type StateSnapshot struct {
	Segments  []StateRecord
	Intervals []StateRecord
}

type StateMachine struct {
	mu        sync.Mutex
	segments  map[string]*StateRecord
	intervals map[string]*StateRecord
	seq       int64
}

func NewStateMachine(p *Project) (*StateMachine, error) {
	if p == nil {
		return nil, &Error{Code: CodeSchema, Path: "$", Message: "nil project"}
	}
	sm := &StateMachine{
		segments: make(map[string]*StateRecord), intervals: make(map[string]*StateRecord),
	}
	for _, d := range p.Dialogues {
		for _, s := range d.Segments {
			key := SegmentKey(d.ID, s.ID)
			sm.segments[key] = &StateRecord{Key: key, Segment: SegmentPendingLoad}
		}
	}
	intervals, err := BuildIntervals(p)
	if err != nil {
		return nil, err
	}
	for _, iv := range intervals {
		sm.intervals[iv.ID] = &StateRecord{Key: iv.ID, Interval: IntervalPending}
	}
	return sm, nil
}

func SegmentKey(dialogueID, segmentID string) string {
	return dialogueID + "/" + segmentID
}

func legalSegment(from, to SegmentState) bool {
	switch from {
	case SegmentPendingLoad:
		return to == SegmentExtracting || to == SegmentFailed
	case SegmentExtracting:
		return to == SegmentCalibrating || to == SegmentFailed
	case SegmentCalibrating:
		return to == SegmentCommitted || to == SegmentFailed
	case SegmentFailed:
		return to == SegmentPendingLoad
	default:
		return false
	}
}

func legalInterval(from, to IntervalState) bool {
	switch from {
	case IntervalPending:
		return to == IntervalProcessing || to == IntervalFailed
	case IntervalProcessing:
		return to == IntervalCommitted || to == IntervalFailed
	case IntervalFailed:
		return to == IntervalPending
	default:
		return false
	}
}

func (sm *StateMachine) TransitionSegment(key string, to SegmentState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rec := sm.segments[key]
	if rec == nil {
		return &Error{Code: CodeState, Entity: key, To: string(to), Message: "unknown segment state"}
	}
	from := rec.Segment
	if !legalSegment(from, to) {
		return &Error{
			Code: CodeState, Entity: key, From: string(from), To: string(to),
			Message: "illegal segment state transition; state and progress preserved",
		}
	}
	sm.seq++
	rec.Segment = to
	rec.Progress++
	rec.Sequence = sm.seq
	return nil
}

func (sm *StateMachine) TransitionInterval(key string, to IntervalState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rec := sm.intervals[key]
	if rec == nil {
		return &Error{Code: CodeState, Entity: key, To: string(to), Message: "unknown interval state"}
	}
	from := rec.Interval
	if !legalInterval(from, to) {
		return &Error{
			Code: CodeState, Entity: key, From: string(from), To: string(to),
			Message: "illegal interval state transition; state and progress preserved",
		}
	}
	sm.seq++
	rec.Interval = to
	rec.Progress++
	rec.Sequence = sm.seq
	return nil
}

func (sm *StateMachine) FailSegment(key string, err error) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rec := sm.segments[key]
	if rec == nil {
		return &Error{Code: CodeState, Entity: key, To: string(SegmentFailed), Message: "unknown segment state"}
	}
	if rec.Segment == SegmentFailed || rec.Segment == SegmentCommitted {
		return nil
	}
	if !legalSegment(rec.Segment, SegmentFailed) {
		return &Error{
			Code: CodeState, Entity: key, From: string(rec.Segment), To: string(SegmentFailed),
			Message: "illegal segment failure transition; state and progress preserved",
		}
	}
	rec.LastError = fmt.Sprint(err)
	sm.seq++
	rec.Segment = SegmentFailed
	rec.Progress++
	rec.Sequence = sm.seq
	return nil
}

func (sm *StateMachine) FailInterval(key string, err error) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	rec := sm.intervals[key]
	if rec == nil {
		return &Error{Code: CodeState, Entity: key, To: string(IntervalFailed), Message: "unknown interval state"}
	}
	if rec.Interval == IntervalFailed || rec.Interval == IntervalCommitted {
		return nil
	}
	if !legalInterval(rec.Interval, IntervalFailed) {
		return &Error{
			Code: CodeState, Entity: key, From: string(rec.Interval), To: string(IntervalFailed),
			Message: "illegal interval failure transition; state and progress preserved",
		}
	}
	rec.LastError = fmt.Sprint(err)
	sm.seq++
	rec.Interval = IntervalFailed
	rec.Progress++
	rec.Sequence = sm.seq
	return nil
}

func (sm *StateMachine) Record(key string, segment bool) (StateRecord, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if segment {
		rec, ok := sm.segments[key]
		if !ok {
			return StateRecord{}, false
		}
		return *rec, true
	}
	rec, ok := sm.intervals[key]
	if !ok {
		return StateRecord{}, false
	}
	return *rec, true
}

func (sm *StateMachine) Snapshot() StateSnapshot {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	out := StateSnapshot{
		Segments:  make([]StateRecord, 0, len(sm.segments)),
		Intervals: make([]StateRecord, 0, len(sm.intervals)),
	}
	for _, r := range sm.segments {
		out.Segments = append(out.Segments, *r)
	}
	for _, r := range sm.intervals {
		out.Intervals = append(out.Intervals, *r)
	}
	sort.Slice(out.Segments, func(i, j int) bool { return out.Segments[i].Key < out.Segments[j].Key })
	sort.Slice(out.Intervals, func(i, j int) bool { return out.Intervals[i].Key < out.Intervals[j].Key })
	return out
}
