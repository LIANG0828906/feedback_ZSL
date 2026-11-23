package orchestrator

import "fmt"

// State is the per-dialogue orchestration state.
type State string

const (
	StatePending       State = "pending"        // 待编排
	StateOrchestrating State = "orchestrating"  // 编排中
	StateSubmitted     State = "submitted"      // 已提交
	StateInterrupted   State = "interrupted"    // 被打断
	StateResumePending State = "resume_pending" // 续讲待办
	StateFailed        State = "failed"         // 失败
)

// legalTransitions defines the state machine edges.
var legalTransitions = map[State][]State{
	StatePending:       {StateOrchestrating},
	StateOrchestrating: {StateSubmitted, StateInterrupted, StateFailed},
	StateInterrupted:   {StateResumePending},
	StateResumePending: {StateOrchestrating},
	StateFailed:        {StateOrchestrating}, // retry
	StateSubmitted:     {},                   // locked
}

// DialogueState tracks the mutable state of one dialogue.
type DialogueState struct {
	DialogueID string `json:"dialogue_id"`
	State      State  `json:"state"`
}

// Transition moves the state machine along a legal edge. Illegal
// transitions return E_STATE and leave the state untouched.
func (s *DialogueState) Transition(to State) *Error {
	for _, legal := range legalTransitions[s.State] {
		if legal == to {
			s.State = to
			return nil
		}
	}
	return newError(EState, s.DialogueID, "state",
		fmt.Sprintf("illegal transition %s -> %s", s.State, to))
}

// CanTransition reports whether the edge is legal without mutating.
func (s *DialogueState) CanTransition(to State) bool {
	for _, legal := range legalTransitions[s.State] {
		if legal == to {
			return true
		}
	}
	return false
}
