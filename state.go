package bonerig

import "sync"

type IntervalState string

const (
	StatePendingLoad     IntervalState = "pending_load"
	StatePendingRetarget IntervalState = "pending_retarget"
	StateRetargeting     IntervalState = "retargeting"
	StateCommitted       IntervalState = "committed"
	StateFailed          IntervalState = "failed"
)

var legalTransitions = map[IntervalState]map[IntervalState]bool{
	StatePendingLoad:     {StatePendingRetarget: true, StateFailed: true},
	StatePendingRetarget: {StateRetargeting: true, StateFailed: true},
	StateRetargeting:     {StateCommitted: true, StateFailed: true},
	StateFailed:          {StatePendingRetarget: true, StatePendingLoad: true},
	StateCommitted:       {},
}

// IntervalMachine guards legal frame-interval state transitions.
type IntervalMachine struct {
	mu             sync.Mutex
	IntervalID     string        `json:"interval_id"`
	State          IntervalState `json:"state"`
	ProgressFrames int           `json:"progress_frames"`
	Attempts       int           `json:"attempts"`
	LastError      *Error        `json:"last_error,omitempty"`
	FailedFrames   []int         `json:"failed_frames,omitempty"`
}

func NewIntervalMachine(id string) *IntervalMachine {
	return &IntervalMachine{IntervalID: id, State: StatePendingLoad}
}

func (m *IntervalMachine) GetState() IntervalState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.State
}

func (m *IntervalMachine) Transition(to IntervalState) *Error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !legalTransitions[m.State][to] {
		e := errorf(CodeState, "illegal interval state transition")
		e.IntervalID = m.IntervalID
		e.State = string(m.State)
		e.OpA = string(m.State)
		e.OpB = string(to)
		return e
	}
	m.State = to
	switch to {
	case StateRetargeting:
		m.Attempts++
	case StateCommitted:
		m.LastError = nil
	}
	return nil
}

// Fail moves an active interval to failed while preserving progress.
func (m *IntervalMachine) Fail(frame int, cause *Error) *Error {
	m.mu.Lock()
	if m.State != StateRetargeting && m.State != StatePendingRetarget && m.State != StatePendingLoad {
		e := errorf(CodeState, "failure cannot be recorded from current state")
		e.IntervalID = m.IntervalID
		e.State = string(m.State)
		return e
	}
	if frame >= 0 {
		exists := false
		for _, f := range m.FailedFrames {
			if f == frame {
				exists = true
			}
		}
		if !exists {
			m.FailedFrames = append(m.FailedFrames, frame)
		}
	}
	m.State = StateFailed
	m.LastError = cause
	m.mu.Unlock()
	return nil
}

func (m *IntervalMachine) SetProgress(frames int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ProgressFrames = frames
}

// IntervalSnapshot is a lock-free value copy of machine progress.
type IntervalSnapshot struct {
	IntervalID     string        `json:"interval_id"`
	State          IntervalState `json:"state"`
	ProgressFrames int           `json:"progress_frames"`
	Attempts       int           `json:"attempts"`
	LastError      *Error        `json:"last_error,omitempty"`
	FailedFrames   []int         `json:"failed_frames,omitempty"`
}

func (m *IntervalMachine) Snapshot() IntervalSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := IntervalSnapshot{
		IntervalID: m.IntervalID, State: m.State, ProgressFrames: m.ProgressFrames,
		Attempts: m.Attempts, LastError: m.LastError,
	}
	if m.FailedFrames != nil {
		cp.FailedFrames = append([]int(nil), m.FailedFrames...)
	}
	return cp
}
