package dialogue

import (
	"fmt"
	"sync"
)

type SegmentStatus string

const (
	StatusPending     SegmentStatus = "pending"
	StatusMeasuring   SegmentStatus = "measuring"
	StatusCalibrating SegmentStatus = "calibrating"
	StatusCommitted   SegmentStatus = "committed"
	StatusFailed      SegmentStatus = "failed"
)

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type SegmentState struct {
	SegmentID   string              `json:"segment_id"`
	Status      SegmentStatus       `json:"status"`
	Envelope    *SegmentEnvelope    `json:"envelope,omitempty"`
	Calibration *SegmentCalibration `json:"calibration,omitempty"`
	Error       string              `json:"error,omitempty"`
}

type StateSnapshot struct {
	Progress Progress       `json:"progress"`
	Segments []SegmentState `json:"segments"`
}

// StateMachine enforces the five-state segment lifecycle. Every mutating method
// validates before changing state, so a rejected transition preserves both state
// and committed progress.
type StateMachine struct {
	mu     sync.RWMutex
	order  []string
	states map[string]*SegmentState
}

func NewStateMachine(project *Project) *StateMachine {
	machine := &StateMachine{states: map[string]*SegmentState{}}
	if project == nil {
		return machine
	}
	for _, segment := range project.Segments() {
		machine.order = append(machine.order, segment.ID)
		machine.states[segment.ID] = &SegmentState{SegmentID: segment.ID, Status: StatusPending}
	}
	return machine
}

func (m *StateMachine) Transition(segmentID string, to SegmentStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[segmentID]
	if !ok {
		return newError(CodeState, "", "segments."+segmentID, "unknown segment")
	}
	if !validTransition(state.Status, to) {
		return newError(CodeState, "", "segments."+segmentID+".status", fmt.Sprintf("illegal transition %s -> %s", state.Status, to))
	}
	state.Status = to
	state.Error = ""
	return nil
}

func validTransition(from, to SegmentStatus) bool {
	switch from {
	case StatusPending:
		return to == StatusMeasuring || to == StatusFailed
	case StatusMeasuring:
		return to == StatusCalibrating || to == StatusFailed
	case StatusCalibrating:
		return to == StatusCommitted || to == StatusFailed
	case StatusFailed:
		return to == StatusPending
	case StatusCommitted:
		return false
	default:
		return false
	}
}

func (m *StateMachine) SetEnvelope(segmentID string, envelope *SegmentEnvelope) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[segmentID]
	if !ok {
		return newError(CodeState, "", "segments."+segmentID, "unknown segment")
	}
	if state.Status != StatusMeasuring {
		return newError(CodeState, "", "segments."+segmentID+".envelope", "envelope can only be stored while measuring")
	}
	state.Envelope = cloneEnvelope(envelope)
	return nil
}

func (m *StateMachine) SetCalibration(segmentID string, calibration *SegmentCalibration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[segmentID]
	if !ok {
		return newError(CodeState, "", "segments."+segmentID, "unknown segment")
	}
	if state.Status != StatusCalibrating {
		return newError(CodeState, "", "segments."+segmentID+".calibration", "calibration can only be stored while calibrating")
	}
	state.Calibration = cloneCalibration(calibration)
	return nil
}

func (m *StateMachine) Commit(segmentID string) error {
	return m.Transition(segmentID, StatusCommitted)
}

func (m *StateMachine) Fail(segmentID string, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[segmentID]
	if !ok {
		return newError(CodeState, "", "segments."+segmentID, "unknown segment")
	}
	if state.Status == StatusCommitted {
		return newError(CodeState, "", "segments."+segmentID+".status", "committed segment cannot transition to failed")
	}
	state.Status = StatusFailed
	if cause != nil {
		state.Error = cause.Error()
	}
	return nil
}

func (m *StateMachine) Reset(segmentID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[segmentID]
	if !ok {
		return newError(CodeState, "", "segments."+segmentID, "unknown segment")
	}
	state.Status = StatusPending
	state.Envelope = nil
	state.Calibration = nil
	state.Error = ""
	return nil
}

func (m *StateMachine) RestoreCommitted(segmentID string, envelope *SegmentEnvelope, calibration *SegmentCalibration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[segmentID]
	if !ok {
		return newError(CodeState, "", "segments."+segmentID, "unknown segment")
	}
	state.Status = StatusCommitted
	state.Envelope = cloneEnvelope(envelope)
	state.Calibration = cloneCalibration(calibration)
	state.Error = ""
	return nil
}

func (m *StateMachine) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	progress := Progress{Total: len(m.order)}
	for _, id := range m.order {
		if m.states[id].Status == StatusCommitted {
			progress.Committed++
		}
	}
	return progress
}

func (m *StateMachine) Snapshot() StateSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot := StateSnapshot{Progress: Progress{Total: len(m.order)}}
	for _, id := range m.order {
		state := m.states[id]
		cloned := *state
		cloned.Envelope = cloneEnvelope(state.Envelope)
		cloned.Calibration = cloneCalibration(state.Calibration)
		if state.Status == StatusCommitted {
			snapshot.Progress.Committed++
		}
		snapshot.Segments = append(snapshot.Segments, cloned)
	}
	return snapshot
}

func (m *StateMachine) Status(segmentID string) (SegmentStatus, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.states[segmentID]
	if !ok {
		return "", false
	}
	return state.Status, true
}

func cloneEnvelope(envelope *SegmentEnvelope) *SegmentEnvelope {
	if envelope == nil {
		return nil
	}
	out := *envelope
	out.Frames = append([]FrameEnvelope(nil), envelope.Frames...)
	out.Truncations = append([]Truncation(nil), envelope.Truncations...)
	for i := range out.Frames {
		out.Frames[i].Truncations = append([]Truncation(nil), envelope.Frames[i].Truncations...)
	}
	return &out
}

func cloneCalibration(calibration *SegmentCalibration) *SegmentCalibration {
	if calibration == nil {
		return nil
	}
	out := *calibration
	out.Frames = append([]CalibratedFrame(nil), calibration.Frames...)
	out.Truncations = append([]Truncation(nil), calibration.Truncations...)
	return &out
}
