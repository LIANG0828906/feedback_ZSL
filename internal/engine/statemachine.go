package engine

import "strokeforge/internal/model"

// Segment state values.
const (
	StatePending    = "pending"
	StateProcessing = "processing"
	StateCommitted  = "committed"
	StateFailed     = "failed"
)

// legalTransitions encodes the only allowed state-machine edges.
var legalTransitions = map[string]map[string]bool{
	StatePending:    {StateProcessing: true},
	StateProcessing: {StateCommitted: true, StateFailed: true},
	StateFailed:     {StatePending: true},
	StateCommitted:  {},
}

// Transition validates and applies a state change. Illegal moves return
// E_STATE and leave the current state untouched.
func Transition(current, target string) error {
	if !legalTransitions[current][target] {
		return model.NewError(model.EState,
			"illegal transition "+current+" -> "+target).WithField("state")
	}
	return nil
}

// IsTerminal reports whether a segment is frozen.
func IsTerminal(state string) bool { return state == StateCommitted }
