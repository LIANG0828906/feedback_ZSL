package engine

import (
	"fmt"
	"sync"

	"maskpreview/internal/core"
)

type Stage string

const (
	StageGrow  Stage = "grow"
	StageClean Stage = "clean"
	StageStats Stage = "stats"
)

var StageOrder = []Stage{StageGrow, StageClean, StageStats}

type StageState string

const (
	StatePending    StageState = "pending"
	StateProcessing StageState = "processing"
	StateCommitted  StageState = "committed"
	StateFailed     StageState = "failed"
)

type StageSnapshot struct {
	State           StageState `json:"state"`
	CommittedDigest string     `json:"committed_digest,omitempty"`
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

// BlockFSM enforces per-block, per-stage state transitions. Failed transitions
// are rejected before any mutation, so callers can inspect the original state.
type BlockFSM struct {
	mu        sync.Mutex
	blockID   int
	states    map[Stage]StageState
	committed map[Stage]string
	active    map[Stage]string
	frozen    string
}

func NewBlockFSM(blockID int) *BlockFSM {
	states := make(map[Stage]StageState, len(StageOrder))
	for _, stage := range StageOrder {
		states[stage] = StatePending
	}
	return &BlockFSM{
		blockID:   blockID,
		states:    states,
		committed: map[Stage]string{},
		active:    map[Stage]string{},
	}
}

func (b *BlockFSM) Start(stage Stage, snapshotVersion string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !validStage(stage) {
		return &core.Error{Code: core.CodeState, Op: "fsm.start", Field: "stage", Got: stage, Want: "grow|clean|stats"}
	}
	if b.frozen != "" && snapshotVersion != b.frozen {
		return core.VersionError("fsm.start", b.frozen, snapshotVersion)
	}
	current := b.states[stage]
	if current != StatePending && current != StateFailed {
		return stateError(b.blockID, stage, current, StateProcessing)
	}
	b.states[stage] = StateProcessing
	b.active[stage] = snapshotVersion
	return nil
}

func (b *BlockFSM) Commit(stage Stage, snapshotVersion, digest string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !validStage(stage) {
		return &core.Error{Code: core.CodeState, Op: "fsm.commit", Field: "stage", Got: stage, Want: "grow|clean|stats"}
	}
	if b.frozen != "" && snapshotVersion != b.frozen {
		return core.VersionError("fsm.commit", b.frozen, snapshotVersion)
	}
	current := b.states[stage]
	if current != StateProcessing {
		return stateError(b.blockID, stage, current, StateCommitted)
	}
	if b.active[stage] != snapshotVersion {
		return core.VersionError("fsm.commit", b.active[stage], snapshotVersion)
	}
	b.states[stage] = StateCommitted
	b.committed[stage] = digest
	delete(b.active, stage)
	if b.frozen == "" {
		b.frozen = snapshotVersion
	}
	return nil
}

func (b *BlockFSM) Fail(stage Stage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !validStage(stage) {
		return &core.Error{Code: core.CodeState, Op: "fsm.fail", Field: "stage", Got: stage, Want: "grow|clean|stats"}
	}
	current := b.states[stage]
	if current != StateProcessing {
		return stateError(b.blockID, stage, current, StateFailed)
	}
	b.states[stage] = StateFailed
	return nil
}

func (b *BlockFSM) Snapshot(stage Stage) StageSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return StageSnapshot{State: b.states[stage], CommittedDigest: b.committed[stage]}
}

func (b *BlockFSM) AllStates() map[Stage]StageState {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[Stage]StageState, len(b.states))
	for stage, state := range b.states {
		out[stage] = state
	}
	return out
}

func (b *BlockFSM) FrozenVersion() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.frozen
}

func (b *BlockFSM) Progress() Progress {
	b.mu.Lock()
	defer b.mu.Unlock()
	committed := 0
	for _, stage := range StageOrder {
		if b.states[stage] == StateCommitted {
			committed++
		}
	}
	return Progress{Committed: committed, Total: len(StageOrder)}
}

func stateError(blockID int, stage Stage, current, attempted StageState) *core.Error {
	return &core.Error{
		Code:    core.CodeState,
		Op:      "fsm.transition",
		Field:   "state",
		Got:     current,
		Want:    attempted,
		Message: fmt.Sprintf("illegal transition for block %d stage %s", blockID, stage),
		Data: map[string]any{
			"block_id": blockID,
			"stage":    stage,
			"current":  current,
			"attempt":  attempted,
		},
	}
}

func validStage(stage Stage) bool {
	return stage == StageGrow || stage == StageClean || stage == StageStats
}
