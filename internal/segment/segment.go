// Package segment implements the fixed-duration processing-chunk state
// machine. Each chunk moves through pending -> processing -> committed
// (or failed); illegal transitions return E_STATE and leave the state
// untouched. Committed chunks are frozen.
package segment

import (
	"fmt"

	"captionx/internal/errs"
)

// State is the lifecycle state of one processing chunk.
type State string

const (
	Pending    State = "pending"
	Processing State = "processing"
	Committed  State = "committed"
	Failed     State = "failed"
)

// legal transitions: from -> set of allowed targets.
var legal = map[State]map[State]bool{
	Pending:    {Processing: true},
	Processing: {Committed: true, Failed: true},
	Failed:     {Processing: true}, // retry
	Committed:  {},                 // frozen
}

// Machine tracks the states of all chunks of one project.
type Machine struct {
	states []State
}

// NewMachine creates a machine with total chunks, all pending.
func NewMachine(total int) *Machine {
	st := make([]State, total)
	for i := range st {
		st[i] = Pending
	}
	return &Machine{states: st}
}

// Total returns the chunk count.
func (m *Machine) Total() int { return len(m.states) }

// State returns the state of chunk i.
func (m *Machine) State(i int) State { return m.states[i] }

// States returns a copy of all states.
func (m *Machine) States() []State {
	out := make([]State, len(m.states))
	copy(out, m.states)
	return out
}

// Transition moves chunk i to next, enforcing the legal-transition
// table. Illegal moves return E_STATE and keep the current state.
func (m *Machine) Transition(i int, next State) error {
	if i < 0 || i >= len(m.states) {
		return errs.State(fmt.Sprintf("chunk %d out of range", i))
	}
	cur := m.states[i]
	if !legal[cur][next] {
		return errs.State(fmt.Sprintf("chunk %d: illegal transition %s -> %s", i, cur, next))
	}
	m.states[i] = next
	return nil
}

// Progress reports committed chunks vs total chunks.
func (m *Machine) Progress() (committed, total int) {
	total = len(m.states)
	for _, s := range m.states {
		if s == Committed {
			committed++
		}
	}
	return committed, total
}

// ChunkCount computes how many fixed-duration chunks cover totalSamples.
func ChunkCount(totalSamples, chunkSamples int64) int {
	if totalSamples <= 0 {
		return 0
	}
	return int((totalSamples + chunkSamples - 1) / chunkSamples)
}

// ChunkOf returns the index of the chunk owning sample position pos.
func ChunkOf(pos, chunkSamples int64) int {
	return int(pos / chunkSamples)
}
