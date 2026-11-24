package pipeline

import (
	"time"

	"audiocheck/internal/model"
)

const (
	StatePending    = "pending"
	StateProcessing = "processing"
	StateCommitted  = "committed"
	StateFailed     = "failed"
)

const (
	StageDecode     = "decode"
	StageEvaluate   = "evaluate"
	StageBound      = "bound"
	StageCheckpoint = "checkpoint"
)

var Stages = []string{StageDecode, StageEvaluate, StageBound, StageCheckpoint}

var legalTransitions = map[string]map[string]bool{
	StatePending: {
		StateProcessing: true,
	},
	StateProcessing: {
		StateCommitted: true,
		StateFailed:    true,
	},
	StateFailed: {
		StateProcessing: true,
		StatePending:    true,
	},
	StateCommitted: {
		StateProcessing: true,
	},
}

type Machine struct {
	BlockIndex int
	States     map[string]model.StageState
}

func NewMachine(blockIndex int) *Machine {
	m := &Machine{BlockIndex: blockIndex, States: make(map[string]model.StageState, len(Stages))}
	now := time.Now().UnixNano()
	for _, stage := range Stages {
		m.States[stage] = model.StageState{Stage: stage, State: StatePending, UpdatedNS: now}
	}
	return m
}

func (m *Machine) Transition(stage, target, detail string) error {
	current, ok := m.States[stage]
	if !ok {
		return model.NewError(model.CodeState, "stage", "unknown stage %q", stage)
	}
	if current.State == target {
		return nil
	}
	if !legalTransitions[current.State][target] {
		err := model.NewError(model.CodeState, "state", "illegal block %d stage %s transition %s -> %s", m.BlockIndex, stage, current.State, target)
		err.Actual = current.State
		err.Expected = legalTargets(current.State)
		return err
	}
	current.State = target
	current.Detail = detail
	current.UpdatedNS = time.Now().UnixNano()
	m.States[stage] = current
	return nil
}

func legalTargets(from string) []string {
	out := make([]string, 0, len(legalTransitions[from]))
	for target := range legalTransitions[from] {
		out = append(out, target)
	}
	return out
}

func (m *Machine) StageStates() []model.StageState {
	out := make([]model.StageState, 0, len(Stages))
	for _, stage := range Stages {
		out = append(out, m.States[stage])
	}
	return out
}

func (m *Machine) Overall() string {
	committed := 0
	failed := false
	processing := false
	for _, stage := range Stages {
		switch m.States[stage].State {
		case StateFailed:
			failed = true
		case StateProcessing:
			processing = true
		case StateCommitted:
			committed++
		}
	}
	if failed {
		return StateFailed
	}
	if processing {
		return StateProcessing
	}
	if committed == len(Stages) {
		return StateCommitted
	}
	return StatePending
}
