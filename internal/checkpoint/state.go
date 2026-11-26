package checkpoint

import (
	"time"

	"lip-pipeline-prototype/internal/domain"
)

type State string

const (
	StatePending    State = "pending"
	StateLoading    State = "pending"
	StateQuantizing State = "quantizing"
	StateMerging    State = "merging"
	StateDelivering State = "delivering"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

type Machine struct {
	ShardID    string    `json:"shard_id"`
	ShardIndex int       `json:"shard_index"`
	State      State     `json:"state"`
	Progress   int       `json:"progress"`
	Reason     string    `json:"reason,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func NewMachine(shardID string, shardIndex int) *Machine {
	return &Machine{ShardID: shardID, ShardIndex: shardIndex, State: StatePending, Progress: 0, UpdatedAt: time.Now()}
}

var allowedTransitions = map[State]map[State]bool{
	StatePending:    {StateQuantizing: true, StateFailed: true},
	StateQuantizing: {StateMerging: true, StateFailed: true},
	StateMerging:    {StateDelivering: true, StateFailed: true},
	StateDelivering: {StateCommitted: true, StateFailed: true},
	StateFailed:     {StatePending: true, StateQuantizing: true},
	StateCommitted:  {StatePending: true},
}

var defaultProgress = map[State]int{
	StatePending: 0, StateQuantizing: 25, StateMerging: 50,
	StateDelivering: 75, StateCommitted: 100, StateFailed: -1,
}

func (m *Machine) Transition(next State, reason string) error {
	if m.State == next {
		return nil
	}
	if !allowedTransitions[m.State][next] {
		err := &domain.Error{
			Code:    domain.CodeState,
			Message: "illegal shard state transition " + string(m.State) + " -> " + string(next),
			ShardID: m.ShardID, ShardIndex: &m.ShardIndex,
			Expected: allowedNames(m.State), Actual: string(next),
		}
		return err
	}
	oldProgress := m.Progress
	m.State = next
	m.Reason = reason
	m.UpdatedAt = time.Now()
	if p := defaultProgress[next]; p >= 0 {
		m.Progress = p
	} else {
		m.Progress = oldProgress
	}
	return nil
}

func allowedNames(current State) []string {
	out := []string{}
	for next := range allowedTransitions[current] {
		out = append(out, string(next))
	}
	return out
}
