package core

import (
	"fmt"
	"sync"
)

type BlockRuntime struct {
	Stage     string     `json:"stage"`
	Block     int        `json:"block"`
	State     BlockState `json:"state"`
	Attempts  int        `json:"attempts"`
	LastError string     `json:"lastError,omitempty"`
}

type StageBlockMatrix struct {
	Stage  string       `json:"stage"`
	States []BlockState `json:"states"`
}

type StateMachine struct {
	mu      sync.RWMutex
	grid    Grid
	records map[CommitKey]*BlockRuntime
}

func NewStateMachine(grid Grid) *StateMachine {
	m := &StateMachine{grid: grid, records: make(map[CommitKey]*BlockRuntime, len(StageOrder)*len(grid.Blocks))}
	for _, stage := range StageOrder {
		for _, block := range grid.Blocks {
			key := CommitKey{Stage: stage, Block: block.Index}
			m.records[key] = &BlockRuntime{Stage: stage, Block: block.Index, State: BlockPending}
		}
	}
	return m
}

func LegalTransition(from, to BlockState) bool {
	switch from {
	case BlockPending:
		return to == BlockProcessing
	case BlockProcessing:
		return to == BlockCommitted || to == BlockFailed
	case BlockFailed:
		return to == BlockPending
	default:
		return false
	}
}

func (m *StateMachine) Transition(stage string, block int, to BlockState) error {
	key := CommitKey{Stage: stage, Block: block}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok {
		return &AppError{Code: CodeState, Field: "block", Message: "unknown stage/block", Details: map[string]any{"stage": stage, "block": block}}
	}
	from := rec.State
	if !LegalTransition(from, to) {
		return &AppError{
			Code:    CodeState,
			Field:   "state",
			Message: fmt.Sprintf("illegal block transition %s -> %s", from, to),
			Details: map[string]any{"stage": stage, "block": block, "from": from, "to": to},
		}
	}
	if from == BlockFailed && to == BlockPending {
		rec.LastError = ""
	}
	if to == BlockProcessing {
		rec.Attempts++
	}
	rec.State = to
	return nil
}

func (m *StateMachine) Fail(stage string, block int, reason string) error {
	key := CommitKey{Stage: stage, Block: block}
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok {
		return NewError(CodeState, "block", "unknown stage/block")
	}
	if !LegalTransition(rec.State, BlockFailed) {
		return &AppError{
			Code:    CodeState,
			Field:   "state",
			Message: fmt.Sprintf("illegal block transition %s -> %s", rec.State, BlockFailed),
			Details: map[string]any{"stage": stage, "block": block, "from": rec.State, "to": BlockFailed},
		}
	}
	rec.LastError = reason
	rec.State = BlockFailed
	return nil
}

func (m *StateMachine) RestoreCommitted(stage string, block int) {
	key := CommitKey{Stage: stage, Block: block}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.records[key]; ok {
		rec.State = BlockCommitted
	}
}

func (m *StateMachine) State(stage string, block int) (BlockState, bool) {
	key := CommitKey{Stage: stage, Block: block}
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.records[key]
	if !ok {
		return "", false
	}
	return rec.State, true
}

func (m *StateMachine) Runtime(stage string, block int) (BlockRuntime, bool) {
	key := CommitKey{Stage: stage, Block: block}
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, ok := m.records[key]
	if !ok {
		return BlockRuntime{}, false
	}
	return *rec, true
}

func (m *StateMachine) Matrix() []StageBlockMatrix {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]StageBlockMatrix, 0, len(StageOrder))
	for _, stage := range StageOrder {
		row := StageBlockMatrix{Stage: stage, States: make([]BlockState, len(m.grid.Blocks))}
		for _, block := range m.grid.Blocks {
			row.States[block.Index] = m.records[CommitKey{Stage: stage, Block: block.Index}].State
		}
		out = append(out, row)
	}
	return out
}
