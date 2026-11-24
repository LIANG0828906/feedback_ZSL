package session

import "spatialworkbench/internal/werr"

type State string

const (
	StatePending   State = "pending"
	StateEstimated State = "estimated"
	StateCommitted State = "committed"
	StateFailed    State = "failed"
)

func legalTransition(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateEstimated || to == StateFailed
	case StateEstimated:
		return to == StateCommitted || to == StatePending || to == StateFailed
	case StateFailed:
		return to == StatePending
	default:
		return false
	}
}

func transitionError(frame int, from, to State) *werr.Error {
	e := werr.New(werr.CodeState, "illegal frame transition %s -> %s", from, to)
	e.Field = "state"
	e.Frame = frame
	e.Actual = from
	e.Expected = to
	return e
}
