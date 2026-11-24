package state

import (
	"fmt"

	"mediaforge/internal/errs"
)

type State string

const (
	Pending   State = "pending"
	Decoding  State = "decoding"
	Encoding  State = "encoding"
	Committed State = "committed"
	Failed    State = "failed"
)

func All() []State {
	return []State{Pending, Decoding, Encoding, Committed, Failed}
}

func CanTransition(from, to State) bool {
	switch from {
	case Pending:
		return to == Decoding
	case Decoding:
		return to == Encoding || to == Failed || to == Pending
	case Encoding:
		return to == Committed || to == Failed || to == Pending
	case Failed:
		return to == Pending || to == Decoding
	case Committed:
		return false
	default:
		return false
	}
}

func TransitionError(variant string, from, to State) error {
	return &errs.Error{
		Code: errs.EState, Message: "illegal variant state transition", Variant: variant,
		Detail: fmt.Sprintf("%s -> %s", from, to),
	}
}

func Validate(variant string, from, to State) error {
	if !CanTransition(from, to) {
		return TransitionError(variant, from, to)
	}
	return nil
}

func IsFinal(s State) bool {
	return s == Committed || s == Failed
}
