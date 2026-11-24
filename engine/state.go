package engine

import (
	"fmt"
	"sync"

	"virtualstudio/model"
)

type WindowState string

const (
	StatePending     WindowState = "pending"
	StateCompositing WindowState = "compositing"
	StateCommitted   WindowState = "committed"
	StateFailed      WindowState = "failed"
)

type StateChange struct {
	From   WindowState
	To     WindowState
	Reason string
}

type Window struct {
	mu         sync.RWMutex
	index      int
	startFrame int
	endFrame   int
	state      WindowState
	snapshot   *model.WindowSnapshot
	archive    []*model.WindowSnapshot
	lastError  string
	history    []StateChange
}

func NewWindow(index, startFrame, endFrame int) *Window {
	return &Window{
		index:      index,
		startFrame: startFrame,
		endFrame:   endFrame,
		state:      StatePending,
		history:    []StateChange{{From: "", To: StatePending, Reason: "created"}},
	}
}

func (w *Window) Index() int      { return w.index }
func (w *Window) StartFrame() int { return w.startFrame }
func (w *Window) EndFrame() int   { return w.endFrame }

func (w *Window) State() WindowState {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.state
}

func (w *Window) LastError() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastError
}

func (w *Window) Snapshot() *model.WindowSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snapshot.Clone()
}

func (w *Window) History() []StateChange {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return append([]StateChange(nil), w.history...)
}

func (w *Window) Begin() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StatePending {
		return w.stateError(StateCompositing)
	}
	w.transition(StateCompositing, "begin composition")
	return nil
}

func (w *Window) Commit(snapshot *model.WindowSnapshot) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StateCompositing {
		return w.stateError(StateCommitted)
	}
	if snapshot == nil {
		return model.NewError(model.CodeState, fmt.Sprintf("windows[%d].snapshot", w.index), "cannot commit without a frozen snapshot")
	}
	cp := snapshot.Clone()
	w.snapshot = cp
	w.lastError = ""
	w.transition(StateCommitted, "commit")
	return nil
}

func (w *Window) Fail(reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StateCompositing {
		return w.stateError(StateFailed)
	}
	w.lastError = reason
	w.transition(StateFailed, reason)
	return nil
}

func (w *Window) Retry() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StateFailed {
		return w.stateError(StateCompositing)
	}
	w.transition(StatePending, "retry requested")
	return nil
}

// Abort returns a scheduled-but-uncommitted window to pending after a peer
// window failed or the scheduler stopped before its commit boundary.
func (w *Window) Abort(reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StateCompositing {
		return w.stateError(StatePending)
	}
	w.transition(StatePending, reason)
	return nil
}

func (w *Window) Invalidate(layers []string, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == StatePending {
		return nil
	}
	if w.state != StateCommitted && w.state != StateFailed {
		return w.stateError(StatePending)
	}
	if w.snapshot != nil {
		w.archive = append(w.archive, w.snapshot.Clone())
	}
	w.snapshot = nil
	w.lastError = ""
	w.transition(StatePending, reason)
	return nil
}

func (w *Window) RestoreCommitted(snapshot *model.WindowSnapshot) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state != StatePending {
		return w.stateError(StateCommitted)
	}
	if snapshot == nil {
		return model.NewError(model.CodeState, fmt.Sprintf("windows[%d].snapshot", w.index), "cannot restore without snapshot")
	}
	w.snapshot = snapshot.Clone()
	w.transition(StateCompositing, "restore cache")
	w.transition(StateCommitted, "restore committed cache")
	return nil
}

func (w *Window) stateError(target WindowState) error {
	return model.NewError(model.CodeState, fmt.Sprintf("windows[%d].state", w.index), fmt.Sprintf("illegal transition %s -> %s", w.state, target))
}

func (w *Window) transition(next WindowState, reason string) {
	w.history = append(w.history, StateChange{From: w.state, To: next, Reason: reason})
	w.state = next
}

type WindowSet struct {
	windows []*Window
}

func NewWindowSet(frameCount, windowFrames int) (*WindowSet, error) {
	if frameCount <= 0 || windowFrames <= 0 {
		return nil, model.NewError(model.CodeParam, "window_frames", "frame count and window size must be > 0")
	}
	count := (frameCount + windowFrames - 1) / windowFrames
	set := &WindowSet{windows: make([]*Window, 0, count)}
	for i := 0; i < count; i++ {
		start := i * windowFrames
		end := start + windowFrames - 1
		if end >= frameCount {
			end = frameCount - 1
		}
		set.windows = append(set.windows, NewWindow(i, start, end))
	}
	return set, nil
}

func (s *WindowSet) Windows() []*Window {
	return append([]*Window(nil), s.windows...)
}

func (s *WindowSet) Window(index int) (*Window, bool) {
	if index < 0 || index >= len(s.windows) {
		return nil, false
	}
	return s.windows[index], true
}

func (s *WindowSet) Total() int { return len(s.windows) }

func (s *WindowSet) Committed() int {
	count := 0
	for _, w := range s.windows {
		if w.State() == StateCommitted {
			count++
		}
	}
	return count
}

func (s *WindowSet) States() []WindowState {
	out := make([]WindowState, len(s.windows))
	for i, w := range s.windows {
		out[i] = w.State()
	}
	return out
}
