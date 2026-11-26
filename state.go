package mirror

import (
	"fmt"
	"sort"
	"sync"
)

type SessionState string

const (
	SessionEstablished SessionState = "established"
	SessionNegotiating SessionState = "negotiating"
	SessionMirroring   SessionState = "mirroring"
	SessionPaused      SessionState = "paused"
	SessionEnded       SessionState = "ended"
)

type ScreenState string

const (
	ScreenPending     ScreenState = "pending_negotiation"
	ScreenNegotiating ScreenState = "negotiating"
	ScreenCommitted   ScreenState = "committed"
	ScreenFailed      ScreenState = "failed"
)

type WindowState string

const (
	WindowPending      WindowState = "pending"
	WindowDistributing WindowState = "distributing"
	WindowCommitted    WindowState = "committed"
	WindowFailed       WindowState = "failed"
)

// StateMachine keeps the session, screen, and window layers isolated while
// enforcing the same transition table used by live and replay runs.
type StateMachine struct {
	mu       sync.Mutex
	session  SessionState
	screens  map[string]ScreenState
	windows  map[string]WindowState
	progress int
}

func NewStateMachine(session *Session) *StateMachine {
	sm := &StateMachine{
		session: SessionEstablished,
		screens: map[string]ScreenState{},
		windows: map[string]WindowState{},
	}
	if session != nil {
		for _, sc := range session.Screens {
			sm.screens[sc.ID] = ScreenPending
		}
		for _, w := range session.Windows {
			sm.windows[w.ID] = WindowPending
		}
	}
	return sm
}

func (sm *StateMachine) SessionState() SessionState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.session
}

func (sm *StateMachine) ScreenState(id string) (ScreenState, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	v, ok := sm.screens[id]
	return v, ok
}

func (sm *StateMachine) WindowState(id string) (WindowState, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	v, ok := sm.windows[id]
	return v, ok
}

func (sm *StateMachine) Progress() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.progress
}

func (sm *StateMachine) TransitionSession(to SessionState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if !legalSessionTransition(sm.session, to) {
		return stateError("session", "session", string(sm.session), string(to))
	}
	sm.session = to
	return nil
}

func (sm *StateMachine) TransitionScreen(id string, to ScreenState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	from, ok := sm.screens[id]
	if !ok {
		return stateError("screen", id, "absent", string(to))
	}
	if !legalScreenTransition(from, to) {
		return stateError("screen", id, string(from), string(to))
	}
	sm.screens[id] = to
	return nil
}

func (sm *StateMachine) TransitionWindow(id string, to WindowState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	from, ok := sm.windows[id]
	if !ok {
		return stateError("window", id, "absent", string(to))
	}
	if !legalWindowTransition(from, to) {
		return stateError("window", id, string(from), string(to))
	}
	sm.windows[id] = to
	if to == WindowCommitted {
		sm.progress++
	}
	return nil
}

func (sm *StateMachine) Snapshot() StateSnapshot {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	screens := make([]EntityState, 0, len(sm.screens))
	for id, st := range sm.screens {
		screens = append(screens, EntityState{ID: id, State: string(st)})
	}
	sort.Slice(screens, func(i, j int) bool { return screens[i].ID < screens[j].ID })
	windows := make([]EntityState, 0, len(sm.windows))
	for id, st := range sm.windows {
		windows = append(windows, EntityState{ID: id, State: string(st)})
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].ID < windows[j].ID })
	return StateSnapshot{Session: string(sm.session), Screens: screens, Windows: windows, Progress: sm.progress}
}

type EntityState struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type StateSnapshot struct {
	Session  string        `json:"session"`
	Screens  []EntityState `json:"screens"`
	Windows  []EntityState `json:"windows"`
	Progress int           `json:"progress"`
}

func stateError(layer, id, from, to string) *Error {
	return &Error{Code: CodeState, Path: layer + "." + id,
		Message: fmt.Sprintf("illegal transition %s -> %s; state and progress unchanged", from, to)}
}

func legalSessionTransition(from, to SessionState) bool {
	switch from {
	case SessionEstablished:
		return to == SessionNegotiating || to == SessionEnded
	case SessionNegotiating:
		return to == SessionMirroring || to == SessionEnded
	case SessionMirroring:
		return to == SessionPaused || to == SessionEnded
	case SessionPaused:
		return to == SessionMirroring || to == SessionEnded
	default:
		return false
	}
}

func legalScreenTransition(from, to ScreenState) bool {
	switch from {
	case ScreenPending:
		return to == ScreenNegotiating || to == ScreenFailed
	case ScreenNegotiating:
		return to == ScreenCommitted || to == ScreenFailed || to == ScreenPending
	case ScreenCommitted:
		return to == ScreenNegotiating || to == ScreenFailed || to == ScreenPending
	case ScreenFailed:
		return to == ScreenNegotiating || to == ScreenPending
	default:
		return false
	}
}

func legalWindowTransition(from, to WindowState) bool {
	switch from {
	case WindowPending:
		return to == WindowDistributing || to == WindowFailed
	case WindowDistributing:
		return to == WindowCommitted || to == WindowFailed
	case WindowFailed:
		return to == WindowPending
	default:
		return false
	}
}
