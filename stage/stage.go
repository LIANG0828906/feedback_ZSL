package stage

import (
	"fmt"
	"sort"
	"sync"

	"lipsync/domain"
)

type State string

const (
	PendingLoad State = "pending_load"
	Quantizing  State = "quantizing"
	Merging     State = "merging"
	Dispatching State = "dispatching"
	Committed   State = "committed"
	Failed      State = "failed"
)

var legal = map[State][]State{
	PendingLoad: {Quantizing, Failed},
	Quantizing:  {Merging, Failed},
	Merging:     {Dispatching, Failed},
	Dispatching: {Committed, Failed},
	Failed:      {PendingLoad},
	Committed:   {},
}

type Snapshot struct {
	State      State  `json:"state"`
	Progress   int    `json:"progress"`
	ShardIndex int    `json:"shard_index"`
	DialogueID string `json:"dialogue_id"`
}

type Machine struct {
	mu         sync.Mutex
	state      State
	progress   int
	shardIndex int
	dialogueID string
}

func New(shardIndex int, dialogueID string) *Machine {
	return &Machine{state: PendingLoad, shardIndex: shardIndex, dialogueID: dialogueID}
}

func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return Snapshot{State: m.state, Progress: m.progress, ShardIndex: m.shardIndex, DialogueID: m.dialogueID}
}

func (m *Machine) Transition(next State, progress int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if progress < 0 || progress > 100 {
		return m.stateError(next, progress, "progress must be in [0,100]")
	}
	if progress < m.progress {
		return m.stateError(next, progress, "progress cannot move backwards")
	}
	allowed := legal[m.state]
	ok := false
	for _, candidate := range allowed {
		if candidate == next {
			ok = true
			break
		}
	}
	if !ok {
		return m.stateError(next, progress, "illegal state transition")
	}
	m.state = next
	m.progress = progress
	return nil
}

func (m *Machine) Fail(progress int, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == Failed || m.state == Committed {
		return m.stateError(Failed, progress, "cannot fail from current state")
	}
	if progress < m.progress {
		progress = m.progress
	}
	m.state = Failed
	m.progress = progress
	return cause
}

func (m *Machine) stateError(next State, progress int, message string) error {
	allowed := legal[m.state]
	names := make([]string, len(allowed))
	for i, s := range allowed {
		names[i] = string(s)
	}
	sort.Strings(names)
	return &domain.Error{
		Code: domain.CodeState, Message: message,
		Field: "shard.state", Expected: fmt.Sprintf("%v", names),
		Actual: string(next), ShardIndex: m.shardIndex, DialogueID: m.dialogueID,
		Value: fmt.Sprintf("%s@%d", m.state, m.progress),
	}
}
