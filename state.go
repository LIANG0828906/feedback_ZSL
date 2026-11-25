package lipsync

import (
	"fmt"
	"sync"
)

type WindowState string

const (
	StatePending    WindowState = "pending"
	StateScheduling WindowState = "scheduling"
	StateLipMapping WindowState = "lip_mapping"
	StateCommitted  WindowState = "committed"
	StateFailed     WindowState = "failed"
)

type StateTransition struct {
	From WindowState `json:"from"`
	To   WindowState `json:"to"`
}

// Window is the fixed-frame state machine for one dialogue window.
type Window struct {
	mu         sync.Mutex
	DialogueID string
	Index      int
	FrameStart int
	FrameEnd   int
	state      WindowState
	progress   int
	history    []StateTransition
}

func NewWindow(dialogueID string, index, frameStart, frameEnd int) *Window {
	return &Window{
		DialogueID: dialogueID,
		Index:      index,
		FrameStart: frameStart,
		FrameEnd:   frameEnd,
		state:      StatePending,
		history:    []StateTransition{},
	}
}

func (w *Window) State() WindowState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *Window) Progress() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.progress
}

func (w *Window) History() []StateTransition {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]StateTransition(nil), w.history...)
}

func legalTransition(from, to WindowState) bool {
	switch from {
	case StatePending:
		return to == StateScheduling
	case StateScheduling:
		return to == StateLipMapping || to == StateFailed
	case StateLipMapping:
		return to == StateCommitted || to == StateFailed
	case StateFailed:
		return to == StatePending
	default:
		return false
	}
}

// Transition moves the window only along the explicit state graph. On error it
// leaves both state and progress untouched.
func (w *Window) Transition(to WindowState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.transitionLocked(to)
}

func (w *Window) transitionLocked(to WindowState) error {
	if !legalTransition(w.state, to) {
		return &DomainError{
			Code:       CodeState,
			DialogueID: w.DialogueID,
			Window:     intPtr(w.Index),
			Frame:      intPtr(w.FrameStart),
			Field:      "window.state",
			Current:    string(w.state),
			Expected:   string(to),
			Reason:     "illegal window state transition",
			Message:    fmt.Sprintf("illegal window transition %s -> %s", w.state, to),
		}
	}
	w.history = append(w.history, StateTransition{From: w.state, To: to})
	w.state = to
	return nil
}

// SetProgress records consumed frames. It does not move state.
func (w *Window) SetProgress(progress int) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if progress < 0 || progress > w.FrameEnd-w.FrameStart {
		return &DomainError{
			Code:       CodeState,
			DialogueID: w.DialogueID,
			Window:     intPtr(w.Index),
			Frame:      intPtr(w.FrameStart + w.progress),
			Field:      "window.progress",
			Current:    fmt.Sprintf("%d", w.progress),
			Expected:   fmt.Sprintf("%d", progress),
			Reason:     "progress outside window",
			Message:    "window progress is outside its frame range",
		}
	}
	w.progress = progress
	return nil
}

func (w *Window) Fail(stage string, frame int, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.transitionLocked(StateFailed); err != nil {
		return err
	}
	return &DomainError{
		Code:       CodeStage,
		DialogueID: w.DialogueID,
		Window:     intPtr(w.Index),
		Frame:      intPtr(frame),
		Field:      "window",
		Stage:      stage,
		Reason:     reason,
		Message:    fmt.Sprintf("window %d failed in stage %s", w.Index, stage),
	}
}
