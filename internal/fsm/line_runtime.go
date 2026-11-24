package fsm

import (
	"fmt"
	"sync"

	"ocrworkbench/internal/core"
)

// LineRuntime is the mutable per-line unit. Once committed, its parameters,
// box and recognition result are detached from later parameter changes.
type LineRuntime struct {
	mu     sync.RWMutex
	state  *LineState
	params core.SegmentParams
	box    core.Box
	result *core.LineResult
}

type LineView struct {
	State  LinePhase
	Params core.SegmentParams
	Box    core.Box
	Result *core.LineResult
}

func NewLineRuntime(index int, params core.SegmentParams, box core.Box) *LineRuntime {
	return &LineRuntime{
		state:  NewLine(index),
		params: params,
		box:    box,
	}
}

func (l *LineRuntime) Transition(next LinePhase) error {
	return l.state.Transition(next)
}

func (l *LineRuntime) Phase() LinePhase {
	return l.state.PhaseValue()
}

// ApplyParams changes segmentation only while a line is not committed.
func (l *LineRuntime) ApplyParams(next core.SegmentParams) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.PhaseValue() == LineCommitted {
		return false, nil
	}
	if err := core.ValidateSegmentParams(next); err != nil {
		return false, err
	}
	l.params = next
	return true, nil
}

func (l *LineRuntime) Commit(result core.LineResult) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state.PhaseValue() != LineRecognizing {
		return core.NewError(core.CodeState, fmt.Sprintf("line cannot commit from %s", l.state.PhaseValue()))
	}
	result.State = string(LineCommitted)
	result.Box = l.box
	result.SegmentParams = l.params
	if len(result.Characters) > 0 {
		result.Characters = append([]core.CharResult(nil), result.Characters...)
	}
	l.result = &result
	return l.state.Transition(LineCommitted)
}

func (l *LineRuntime) View() LineView {
	l.mu.RLock()
	defer l.mu.RUnlock()
	view := LineView{
		State:  l.state.PhaseValue(),
		Params: l.params,
		Box:    l.box,
	}
	if l.result != nil {
		copyResult := *l.result
		copyResult.Characters = append([]core.CharResult(nil), l.result.Characters...)
		view.Result = &copyResult
	}
	return view
}
