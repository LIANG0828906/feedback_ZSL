// Package statemachine implements the per-block evaluation state lifecycle.
// Every block moves Pending -> Evaluating -> Committed, or Evaluating ->
// Failed; retries move Failed -> Evaluating. Illegal transitions return
// E_STATE and leave the current state untouched.
package statemachine

import (
	"fmt"

	"vqa/internal/errs"
)

// State is one block lifecycle state.
type State string

const (
	Pending    State = "pending"
	Evaluating State = "evaluating"
	Committed  State = "committed"
	Failed     State = "failed"
)

// allowed transitions define the only legal edges of the lifecycle.
var allowed = map[State][]State{
	Pending:    {Evaluating},
	Evaluating: {Committed, Failed},
	Failed:     {Evaluating},
	Committed:  {},
}

// Machine tracks one block state and its transition history.
type Machine struct {
	State   State   `json:"state"`
	BlockID int     `json:"block_id"`
	History []State `json:"history"`
}

// New starts a block in Pending.
func New(blockID int) *Machine {
	return &Machine{State: Pending, BlockID: blockID, History: []State{Pending}}
}

// Transition attempts a legal move. On an illegal move it returns E_STATE and
// keeps the original state, including its history.
func (m *Machine) Transition(next State) error {
	for _, ok := range allowed[m.State] {
		if ok == next {
			m.State = next
			m.History = append(m.History, next)
			return nil
		}
	}
	return errs.New(errs.EState, "illegal block state transition").
		WithInt("block_id", int64(m.BlockID)).
		With("from", string(m.State)).
		With("to", string(next))
}

// Progress is committed count over total blocks for one frame.
type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

func (p Progress) String() string {
	return fmt.Sprintf("%d/%d", p.Committed, p.Total)
}
