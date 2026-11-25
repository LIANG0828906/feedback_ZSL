package domain

import "fmt"

type WindowState string

const (
	WindowPending     WindowState = "pending"
	WindowArbitrating WindowState = "arbitrating"
	WindowPreempting  WindowState = "preempting"
	WindowCommitted   WindowState = "committed"
	WindowFailed      WindowState = "failed"
)

var windowTransitions = map[WindowState]map[WindowState]bool{
	WindowPending: {
		WindowArbitrating: true,
		WindowFailed:      true,
	},
	WindowArbitrating: {
		WindowPreempting: true,
		WindowCommitted:  true,
		WindowFailed:     true,
	},
	WindowPreempting: {
		WindowCommitted: true,
		WindowFailed:    true,
	},
	WindowCommitted: {},
	WindowFailed: {
		WindowPending: true,
	},
}

// WindowProgress is transactional in memory: it is only replaced after a
// transition has been validated.
type WindowProgress struct {
	State       WindowState `json:"state"`
	WindowIndex int         `json:"window_index"`
	StartFrame  int64       `json:"start_frame"`
	EndFrame    int64       `json:"end_frame"`
	Committed   int         `json:"committed_frames"`
}

func NewWindowProgress(index int, start, end int64) WindowProgress {
	return WindowProgress{
		State:       WindowPending,
		WindowIndex: index,
		StartFrame:  start,
		EndFrame:    end,
	}
}

func (w *WindowProgress) CanTransition(to WindowState) bool {
	return windowTransitions[w.State][to]
}

// Transition preserves both old state and progress on an illegal move.
func (w *WindowProgress) Transition(to WindowState) error {
	if !w.CanTransition(to) {
		return &Error{
			Code:    CodeState,
			Path:    fmt.Sprintf("window[%d].state", w.WindowIndex),
			State:   string(w.State),
			Message: fmt.Sprintf("illegal transition %s -> %s", w.State, to),
		}
	}
	w.State = to
	if to == WindowCommitted {
		w.Committed += 1
	}
	return nil
}
