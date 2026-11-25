package checkpoint

import (
	"sync"

	"lipsync.local/prototype/pkg/domain"
)

type State string

const (
	StatePending     State = "pending"
	StateQuantizing  State = "quantizing"
	StateMerging     State = "merging"
	StateDispatching State = "dispatching"
	StateCommitted   State = "committed"
	StateFailed      State = "failed"
)

var legalTransitions = map[State]map[State]struct{}{
	StatePending: {
		StateQuantizing: {},
		StateFailed:     {},
	},
	StateQuantizing: {
		StateMerging: {},
		StateFailed:  {},
	},
	StateMerging: {
		StateDispatching: {},
		StateFailed:      {},
	},
	StateDispatching: {
		StateCommitted: {},
		StateFailed:    {},
	},
	StateCommitted: {},
	StateFailed:    {},
}

type ShardSnapshot struct {
	ShardID    string `json:"shard_id"`
	DialogueID string `json:"dialogue_id"`
	State      State  `json:"state"`
	Progress   int    `json:"progress"`
}

// ShardMachine is a small, race-safe state machine. Failed transitions never
// mutate either state or progress.
type ShardMachine struct {
	mu         sync.RWMutex
	shardID    string
	dialogueID string
	state      State
	progress   int
}

func NewShardMachine(shardID, dialogueID string) *ShardMachine {
	return &ShardMachine{shardID: shardID, dialogueID: dialogueID, state: StatePending}
}

func (m *ShardMachine) Transition(next State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := legalTransitions[m.state][next]; !ok {
		return &domain.Error{
			Code: domain.ErrState, ShardID: m.shardID, DialogueID: m.dialogueID,
			Field: "state", Reason: string(m.state) + "->" + string(next),
		}
	}
	m.state = next
	return nil
}

func (m *ShardMachine) SetProgress(progress int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if progress < 0 || progress > 100 {
		return &domain.Error{
			Code: domain.ErrState, ShardID: m.shardID, DialogueID: m.dialogueID,
			Field: "progress", Reason: "must be in [0,100]",
		}
	}
	if m.state == StateCommitted || m.state == StateFailed {
		return &domain.Error{
			Code: domain.ErrState, ShardID: m.shardID, DialogueID: m.dialogueID,
			Field: "progress", Reason: "terminal state cannot change progress",
		}
	}
	m.progress = progress
	return nil
}

func (m *ShardMachine) Snapshot() ShardSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return ShardSnapshot{
		ShardID: m.shardID, DialogueID: m.dialogueID,
		State: m.state, Progress: m.progress,
	}
}
