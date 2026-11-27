package stage

import (
	"fmt"
	"sync"
)

type WindowState string

const (
	WindowPending        WindowState = "pending"
	WindowPoseEvaluating WindowState = "pose_evaluating"
	WindowConeSampling   WindowState = "cone_sampling"
	WindowCommitted      WindowState = "committed"
	WindowFailed         WindowState = "failed"
)

type FixtureState string

const (
	FixturePending   FixtureState = "pending"
	FixtureEvaluated FixtureState = "evaluated"
	FixtureSampled   FixtureState = "sampled"
	FixtureCommitted FixtureState = "committed"
	FixtureFailed    FixtureState = "failed"
)

type WindowStateMachine struct {
	mu       sync.Mutex
	state    WindowState
	progress int
}

func NewWindowStateMachine() *WindowStateMachine {
	return &WindowStateMachine{state: WindowPending}
}

func (m *WindowStateMachine) State() WindowState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *WindowStateMachine) Progress() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.progress
}

func (m *WindowStateMachine) Transition(to WindowState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	legal := map[WindowState][]WindowState{
		WindowPending:        {WindowPoseEvaluating, WindowFailed},
		WindowPoseEvaluating: {WindowConeSampling, WindowFailed},
		WindowConeSampling:   {WindowCommitted, WindowFailed},
		WindowCommitted:      {},
		WindowFailed:         {},
	}
	for _, candidate := range legal[m.state] {
		if candidate == to {
			m.state = to
			if to != WindowFailed {
				m.progress++
			}
			return nil
		}
	}
	return &Error{
		Code:    CodeState,
		Message: fmt.Sprintf("illegal window transition %s -> %s", m.state, to),
		Field:   "window.state", Actual: string(m.state), Expected: string(to),
	}
}

type FixtureStateMachine struct {
	mu       sync.Mutex
	fixture  string
	state    FixtureState
	progress int
}

func NewFixtureStateMachine(fixtureID string) *FixtureStateMachine {
	return &FixtureStateMachine{fixture: fixtureID, state: FixturePending}
}

func (m *FixtureStateMachine) State() FixtureState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *FixtureStateMachine) Progress() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.progress
}

func (m *FixtureStateMachine) Transition(to FixtureState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	legal := map[FixtureState][]FixtureState{
		FixturePending:   {FixtureEvaluated, FixtureFailed},
		FixtureEvaluated: {FixtureSampled, FixtureFailed},
		FixtureSampled:   {FixtureCommitted, FixtureFailed},
		FixtureCommitted: {},
		FixtureFailed:    {},
	}
	for _, candidate := range legal[m.state] {
		if candidate == to {
			m.state = to
			if to != FixtureFailed {
				m.progress++
			}
			return nil
		}
	}
	return &Error{
		Code:    CodeState,
		Message: fmt.Sprintf("illegal fixture transition %s -> %s", m.state, to),
		Field:   "fixture.state", FixtureID: m.fixture,
		Actual: string(m.state), Expected: string(to),
	}
}
