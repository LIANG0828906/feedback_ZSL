package stage

import (
	"fmt"
	"sync"
)

type WindowStatus struct {
	Index        int         `json:"index"`
	StartUS      int64       `json:"start_us"`
	EndUS        int64       `json:"end_us"`
	State        WindowState `json:"state"`
	Attempts     int         `json:"attempts"`
	CueStart     int         `json:"cue_start"`
	CueOffset    int         `json:"cue_offset"`
	ConsumedCues int         `json:"consumed_cues"`
	Error        string      `json:"error,omitempty"`
}

type WindowMachine struct {
	mu         sync.RWMutex
	specs      []WindowSpec
	states     []WindowState
	results    []*WindowResult
	attempts   []int
	errors     []string
	nextCommit int
}

func NewWindowMachine(specs []WindowSpec) *WindowMachine {
	m := &WindowMachine{
		specs:    append([]WindowSpec(nil), specs...),
		states:   make([]WindowState, len(specs)),
		results:  make([]*WindowResult, len(specs)),
		attempts: make([]int, len(specs)),
		errors:   make([]string, len(specs)),
	}
	for i := range m.states {
		m.states[i] = WindowPending
	}
	return m
}

func (m *WindowMachine) Total() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.specs)
}

func (m *WindowMachine) State(index int) (WindowState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if index < 0 || index >= len(m.states) {
		return "", newError(CodeState, "window index out of range", fmt.Sprintf("window:%d", index), "index")
	}
	return m.states[index], nil
}

func (m *WindowMachine) Begin(index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.states) {
		return newError(CodeState, "window index out of range", fmt.Sprintf("window:%d", index), "index")
	}
	if m.states[index] != WindowPending && m.states[index] != WindowFailed {
		return illegalTransition(index, m.states[index], WindowScheduling)
	}
	m.states[index] = WindowScheduling
	m.attempts[index]++
	m.errors[index] = ""
	m.results[index] = nil
	return nil
}

func (m *WindowMachine) Fail(index int, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.states) {
		return newError(CodeState, "window index out of range", fmt.Sprintf("window:%d", index), "index")
	}
	if m.states[index] != WindowScheduling {
		return illegalTransition(index, m.states[index], WindowFailed)
	}
	m.states[index] = WindowFailed
	if cause != nil {
		m.errors[index] = cause.Error()
	} else {
		m.errors[index] = "scheduling failed"
	}
	return nil
}

func (m *WindowMachine) Commit(result WindowResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if result.Index < 0 || result.Index >= len(m.states) {
		return newError(CodeState, "window index out of range", fmt.Sprintf("window:%d", result.Index), "index")
	}
	if m.states[result.Index] != WindowScheduling {
		return illegalTransition(result.Index, m.states[result.Index], WindowCommitted)
	}
	if result.Index != m.nextCommit {
		err := newError(CodeState, "window commit order is not contiguous", fmt.Sprintf("window:%d", result.Index), "commit_order")
		return withDetail(err, ErrorDetail{Expected: fmt.Sprint(m.nextCommit), Actual: fmt.Sprint(result.Index), Window: result.Index})
	}
	spec := m.specs[result.Index]
	if result.CueStart != spec.CueStart || result.CueOffset != spec.CueOffset {
		err := newError(CodeState, "window contains unconsumed cues", fmt.Sprintf("window:%d", result.Index), "cue_offset")
		return withDetail(err, ErrorDetail{
			Expected: fmt.Sprintf("start=%d offset=%d", spec.CueStart, spec.CueOffset),
			Actual:   fmt.Sprintf("start=%d offset=%d", result.CueStart, result.CueOffset),
			Window:   result.Index,
		})
	}
	expectedFrames := spec.FrameEnd - spec.FrameStart
	if len(result.Frames) != expectedFrames {
		err := newError(CodeState, "window frame output is incomplete", fmt.Sprintf("window:%d", result.Index), "frames")
		return withDetail(err, ErrorDetail{Expected: fmt.Sprint(expectedFrames), Actual: fmt.Sprint(len(result.Frames)), Window: result.Index})
	}
	for i, frame := range result.Frames {
		if frame.Frame != spec.FrameStart+i {
			err := newError(CodeState, "window frames are not contiguous", fmt.Sprintf("window:%d", result.Index), "frames")
			return withDetail(err, ErrorDetail{Expected: fmt.Sprint(spec.FrameStart + i), Actual: fmt.Sprint(frame.Frame), Window: result.Index})
		}
	}
	frozen := cloneJSON(result)
	frozen.State = WindowCommitted
	m.results[result.Index] = &frozen
	m.states[result.Index] = WindowCommitted
	m.errors[result.Index] = ""
	m.nextCommit++
	return nil
}

func (m *WindowMachine) Result(index int) (*WindowResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if index < 0 || index >= len(m.results) {
		return nil, newError(CodeState, "window index out of range", fmt.Sprintf("window:%d", index), "index")
	}
	if m.results[index] == nil {
		return nil, newError(CodeState, "window has no committed result", fmt.Sprintf("window:%d", index), "state")
	}
	result := cloneJSON(*m.results[index])
	return &result, nil
}

func (m *WindowMachine) Statuses() []WindowStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]WindowStatus, len(m.specs))
	for i, spec := range m.specs {
		out[i] = WindowStatus{
			Index:        i,
			StartUS:      spec.StartUS,
			EndUS:        spec.EndUS,
			State:        m.states[i],
			Attempts:     m.attempts[i],
			CueStart:     spec.CueStart,
			CueOffset:    spec.CueOffset,
			ConsumedCues: spec.CueOffset - spec.CueStart,
			Error:        m.errors[i],
		}
	}
	return out
}

func (m *WindowMachine) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	committed := 0
	for _, state := range m.states {
		if state == WindowCommitted {
			committed++
		}
	}
	return Progress{Committed: committed, Total: len(m.states)}
}

func (m *WindowMachine) FirstIncomplete() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for i, state := range m.states {
		if state != WindowCommitted {
			return i
		}
	}
	return len(m.states)
}

func (m *WindowMachine) InstallCommitted(result WindowResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if result.Index < 0 || result.Index >= len(m.states) {
		return newError(CodeState, "window index out of range", fmt.Sprintf("window:%d", result.Index), "index")
	}
	if m.states[result.Index] != WindowPending {
		return illegalTransition(result.Index, m.states[result.Index], WindowCommitted)
	}
	if result.Index != m.nextCommit {
		err := newError(CodeState, "checkpoint windows are not contiguous", fmt.Sprintf("window:%d", result.Index), "commit_order")
		return withDetail(err, ErrorDetail{Expected: fmt.Sprint(m.nextCommit), Actual: fmt.Sprint(result.Index), Window: result.Index})
	}
	spec := m.specs[result.Index]
	if result.CueStart != spec.CueStart || result.CueOffset != spec.CueOffset || len(result.Frames) != spec.FrameEnd-spec.FrameStart {
		return newError(CodeState, "checkpoint window does not match current project shape", fmt.Sprintf("window:%d", result.Index), "checkpoint")
	}
	frozen := cloneJSON(result)
	frozen.State = WindowCommitted
	m.results[result.Index] = &frozen
	m.states[result.Index] = WindowCommitted
	m.attempts[result.Index]++
	m.nextCommit++
	return nil
}

func illegalTransition(index int, from, to WindowState) *DomainError {
	err := newError(CodeState, "illegal window state transition", fmt.Sprintf("window:%d", index), "state")
	return withDetail(err, ErrorDetail{Expected: string(from) + "->" + string(to), Actual: string(from), Window: index})
}

func (m *WindowMachine) FailByIndex(index int, cause error) error {
	return m.Fail(index, cause)
}
