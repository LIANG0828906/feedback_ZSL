package engine

import (
	"fmt"
	"sync"

	"audioeval/internal/model"
)

type stateKey struct {
	Frame int
	Block int
}

type blockState struct {
	Statuses     map[model.BlockStage]model.BlockStatus
	ParamVersion string
}

type StateMachine struct {
	mu       sync.Mutex
	frames   int
	blocks   int
	states   map[stateKey]*blockState
	frozen   map[stateKey]model.BlockResult
	progress map[stateKey]bool
}

func NewStateMachine(frameCount, blocksPerFrame int) (*StateMachine, error) {
	if frameCount <= 0 {
		return nil, model.NewError(model.CodePara, "frame_count", -1, "frame count must be positive")
	}
	if blocksPerFrame <= 0 {
		return nil, model.NewError(model.CodePara, "blocks_per_frame", -1, "blocks per frame must be positive")
	}
	m := &StateMachine{
		frames:   frameCount,
		blocks:   blocksPerFrame,
		states:   make(map[stateKey]*blockState, frameCount*blocksPerFrame),
		frozen:   make(map[stateKey]model.BlockResult),
		progress: make(map[stateKey]bool),
	}
	for frame := 0; frame < frameCount; frame++ {
		for block := 0; block < blocksPerFrame; block++ {
			statuses := make(map[model.BlockStage]model.BlockStatus, len(model.AllStages()))
			for _, stage := range model.AllStages() {
				statuses[stage] = model.StatusPending
			}
			m.states[stateKey{Frame: frame, Block: block}] = &blockState{Statuses: statuses}
		}
	}
	return m, nil
}

func (m *StateMachine) Status(coord model.BlockCoord, stage model.BlockStage) model.BlockStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[stateKey{Frame: coord.Frame, Block: coord.Block}]
	if !ok {
		return ""
	}
	return state.Statuses[stage]
}

func (m *StateMachine) Transition(coord model.BlockCoord, stage model.BlockStage, to model.BlockStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[stateKey{Frame: coord.Frame, Block: coord.Block}]
	if !ok {
		return model.NewError(model.CodePara, "block", coord.Frame, "block coordinate is outside the state machine").WithBlock(coord)
	}
	if !knownStage(stage) {
		return model.NewError(model.CodePara, "stage", coord.Frame, "unknown evaluation stage").WithBlock(coord)
	}
	from := state.Statuses[stage]
	if !legalTransition(from, to) {
		return model.NewError(model.CodeState, "state", coord.Frame,
			fmt.Sprintf("illegal transition %s -> %s for stage %s", from, to, stage)).WithBlock(coord)
	}
	state.Statuses[stage] = to
	m.refreshProgressLocked(stateKey{Frame: coord.Frame, Block: coord.Block})
	return nil
}

func (m *StateMachine) MarkProcessing(coord model.BlockCoord) error {
	for _, stage := range model.AllStages() {
		if m.Status(coord, stage) == model.StatusPending {
			if err := m.Transition(coord, stage, model.StatusProcessing); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *StateMachine) MarkFailed(coord model.BlockCoord) error {
	for _, stage := range model.AllStages() {
		if m.Status(coord, stage) == model.StatusProcessing {
			if err := m.Transition(coord, stage, model.StatusFailed); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *StateMachine) CommitResult(coord model.BlockCoord, result model.BlockResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := stateKey{Frame: coord.Frame, Block: coord.Block}
	state, ok := m.states[key]
	if !ok {
		return model.NewError(model.CodePara, "block", coord.Frame, "block coordinate is outside the state machine").WithBlock(coord)
	}
	for _, stage := range model.AllStages() {
		if state.Statuses[stage] != model.StatusProcessing {
			return model.NewError(model.CodeState, "state", coord.Frame,
				fmt.Sprintf("cannot commit block while stage %s is %s", stage, state.Statuses[stage])).WithBlock(coord)
		}
	}
	for _, stage := range model.AllStages() {
		state.Statuses[stage] = model.StatusCommitted
	}
	m.frozen[key] = result
	m.progress[key] = true
	return nil
}

func (m *StateMachine) FrozenResult(coord model.BlockCoord) (model.BlockResult, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result, ok := m.frozen[stateKey{Frame: coord.Frame, Block: coord.Block}]
	return result, ok
}

func (m *StateMachine) Progress() (committed, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.progress), m.frames * m.blocks
}

func (m *StateMachine) UpdatePendingParamVersion(version string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	updated := 0
	for key, state := range m.states {
		if m.progress[key] {
			continue
		}
		state.ParamVersion = version
		updated++
	}
	return updated
}

func (m *StateMachine) ParamVersion(coord model.BlockCoord) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[stateKey{Frame: coord.Frame, Block: coord.Block}]
	if !ok {
		return ""
	}
	return state.ParamVersion
}

func (m *StateMachine) refreshProgressLocked(key stateKey) {
	state, ok := m.states[key]
	if !ok {
		return
	}
	for _, stage := range model.AllStages() {
		if state.Statuses[stage] != model.StatusCommitted {
			delete(m.progress, key)
			return
		}
	}
	m.progress[key] = true
}

func knownStage(stage model.BlockStage) bool {
	for _, candidate := range model.AllStages() {
		if stage == candidate {
			return true
		}
	}
	return false
}

func legalTransition(from, to model.BlockStatus) bool {
	switch from {
	case model.StatusPending:
		return to == model.StatusProcessing
	case model.StatusProcessing:
		return to == model.StatusCommitted || to == model.StatusFailed
	case model.StatusFailed:
		return to == model.StatusProcessing
	default:
		return false
	}
}
