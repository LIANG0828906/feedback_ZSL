package runner

import (
	"semanticshading/internal/apierr"
	"semanticshading/internal/model"
)

type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

type BlockState struct {
	BlockIndex int              `json:"block_index"`
	Block      model.BlockCoord `json:"block"`
	State      State            `json:"state"`
	Attempts   int              `json:"attempts"`
	LastError  *apierr.Error    `json:"last_error,omitempty"`
}

type Machine struct {
	info BlockState
}

func NewMachine(block model.BlockCoord) *Machine {
	return &Machine{info: BlockState{BlockIndex: block.Index, Block: block, State: StatePending}}
}

func (m *Machine) State() BlockState { return m.info }

func (m *Machine) Transition(to State) *apierr.Error {
	from := m.info.State
	allowed := map[State]map[State]bool{
		StatePending:    {StateProcessing: true},
		StateProcessing: {StateCommitted: true, StateFailed: true, StatePending: true},
		StateFailed:     {StatePending: true, StateProcessing: true},
		StateCommitted:  {},
	}
	if !allowed[from][to] {
		err := apierr.New(apierr.EState, "state", "illegal block transition %s -> %s", from, to).
			WithBlock(m.info.Block.BX, m.info.Block.BY).
			WithDetail("block_index", m.info.BlockIndex).
			WithDetail("from_state", string(from)).
			WithDetail("to_state", string(to))
		m.info.LastError = err
		return err
	}
	m.info.State = to
	if to == StateProcessing {
		m.info.Attempts++
	}
	if to == StatePending {
		m.info.LastError = nil
	}
	return nil
}

func IllegalTransitionError(block model.BlockCoord, from, to State) *apierr.Error {
	return apierr.New(apierr.EState, "state", "illegal block transition %s -> %s", from, to).
		WithBlock(block.BX, block.BY).
		WithDetail("from_state", string(from)).
		WithDetail("to_state", string(to))
}
