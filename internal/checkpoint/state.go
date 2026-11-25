package checkpoint

import (
	"iconseg/internal/core"
)

type State string

const (
	Pending    State = "pending"
	Processing State = "processing"
	Committed  State = "committed"
	Failed     State = "failed"
)

var AllStates = []State{Pending, Processing, Committed, Failed}

func CanTransition(from, to State) bool {
	switch from {
	case Pending:
		return to == Processing || to == Failed
	case Processing:
		return to == Committed || to == Failed
	case Failed:
		return to == Processing || to == Pending
	case Committed:
		return false
	default:
		return false
	}
}

func ValidateTransition(block string, layer string, from, to State) error {
	if !CanTransition(from, to) {
		return core.StateError(block, layer, string(from), string(to))
	}
	return nil
}
