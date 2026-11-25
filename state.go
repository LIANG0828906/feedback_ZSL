package iconmask

import (
	"fmt"
	"sync"
)

type Stage string

const (
	StageRadius Stage = "radius"
	StageClose  Stage = "close"
	StageOpen   Stage = "open"
	StageStats  Stage = "stats"
)

var stageOrder = []Stage{StageRadius, StageClose, StageOpen, StageStats}

type StageState string

const (
	StatePending    StageState = "pending"
	StateProcessing StageState = "processing"
	StateCommitted  StageState = "committed"
	StateFailed     StageState = "failed"
)

type StageOutput struct {
	Digest      string            `json:"digest"`
	Mask        *Mask             `json:"mask,omitempty"`
	RadiusTable *RadiusTable      `json:"radiusTable,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

func (output StageOutput) Clone() StageOutput {
	cloned := StageOutput{Digest: output.Digest, RadiusTable: output.RadiusTable, Metadata: nil}
	if output.Mask != nil {
		mask := output.Mask.Clone()
		cloned.Mask = &mask
	}
	if output.RadiusTable != nil {
		table := *output.RadiusTable
		table.Segments = append([]SegmentRadius(nil), output.RadiusTable.Segments...)
		cloned.RadiusTable = &table
	}
	if output.Metadata != nil {
		cloned.Metadata = make(map[string]string, len(output.Metadata))
		for key, value := range output.Metadata {
			cloned.Metadata[key] = value
		}
	}
	return cloned
}

type StageRecord struct {
	State  StageState   `json:"state"`
	Output *StageOutput `json:"output,omitempty"`
	Error  string       `json:"error,omitempty"`
}

type RoundRecord struct {
	Round  int                    `json:"round"`
	Stages map[Stage]*StageRecord `json:"stages"`
}

type StateSnapshot struct {
	Block           Coord         `json:"block"`
	CurrentRound    int           `json:"currentRound"`
	Rounds          []RoundRecord `json:"rounds"`
	CommittedStages int           `json:"committedStages"`
	CommittedRounds int           `json:"committedRounds"`
}

type BlockStateMachine struct {
	mu              sync.Mutex
	block           Coord
	currentRound    int
	rounds          map[int]*RoundRecord
	committedStages int
	committedRounds int
}

func NewBlockStateMachine(block Coord) *BlockStateMachine {
	machine := &BlockStateMachine{
		block:  block,
		rounds: make(map[int]*RoundRecord),
	}
	machine.ensureRound(0)
	return machine
}

func (machine *BlockStateMachine) ensureRound(round int) *RoundRecord {
	record, ok := machine.rounds[round]
	if ok {
		return record
	}
	record = &RoundRecord{Round: round, Stages: make(map[Stage]*StageRecord, len(stageOrder))}
	for _, stage := range stageOrder {
		record.Stages[stage] = &StageRecord{State: StatePending}
	}
	machine.rounds[round] = record
	return record
}

func (machine *BlockStateMachine) stage(round int, stage Stage) (*StageRecord, *Error) {
	if !legalStage(stage) {
		return nil, stateError(machine.block, string(stage), "unknown block stage")
	}
	record := machine.rounds[round]
	if record == nil {
		return nil, stateError(machine.block, string(stage), "round has not been initialized")
	}
	return record.Stages[stage], nil
}

func legalStage(stage Stage) bool {
	for _, candidate := range stageOrder {
		if candidate == stage {
			return true
		}
	}
	return false
}

func stageIndex(stage Stage) int {
	for i, candidate := range stageOrder {
		if candidate == stage {
			return i
		}
	}
	return -1
}

func (machine *BlockStateMachine) Start(stage Stage) *Error {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	if !legalStage(stage) {
		return stateError(machine.block, string(stage), "unknown block stage")
	}
	round := machine.currentRound
	if stage == StageRadius {
		if record := machine.rounds[round]; record != nil {
			stats := record.Stages[StageStats]
			if stats.State == StateCommitted {
				round++
				machine.currentRound = round
				machine.ensureRound(round)
			}
		}
	} else if record := machine.rounds[round]; record == nil || record.Stages[stageOrder[stageIndex(stage)-1]].State != StateCommitted {
		return stateError(machine.block, string(stage), "previous stage must be committed before starting this stage")
	}
	record, err := machine.stage(round, stage)
	if err != nil {
		return err
	}
	if record.State != StatePending {
		return stateError(machine.block, string(stage), "only a pending stage can start processing")
	}
	record.State = StateProcessing
	return nil
}

func (machine *BlockStateMachine) Commit(stage Stage, output StageOutput) *Error {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	round := machine.currentRound
	record, err := machine.stage(round, stage)
	if err != nil {
		return err
	}
	if record.State != StateProcessing {
		return stateError(machine.block, string(stage), "only a processing stage can be committed")
	}
	copied := output.Clone()
	record.Output = &copied
	record.State = StateCommitted
	record.Error = ""
	machine.committedStages++
	if stage == StageStats {
		machine.committedRounds++
		if machine.currentRound+1 > 0 {
			machine.ensureRound(machine.currentRound + 1)
		}
	}
	return nil
}

func (machine *BlockStateMachine) Fail(stage Stage, reason string) *Error {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	round := machine.currentRound
	record, err := machine.stage(round, stage)
	if err != nil {
		return err
	}
	if record.State != StateProcessing {
		return stateError(machine.block, string(stage), "only a processing stage can fail")
	}
	record.State = StateFailed
	record.Error = reason
	return nil
}

func (machine *BlockStateMachine) Retry(stage Stage) *Error {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	record, err := machine.stage(machine.currentRound, stage)
	if err != nil {
		return err
	}
	if record.State != StateFailed {
		return stateError(machine.block, string(stage), "only a failed stage can return to pending")
	}
	record.State = StatePending
	record.Error = ""
	return nil
}

// ApplyEdit leaves committed stages frozen and resets the selected uncommitted
// stage and all downstream stages to pending.
func (machine *BlockStateMachine) ApplyEdit(from Stage) *Error {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	index := stageIndex(from)
	if index < 0 {
		return stateError(machine.block, string(from), "unknown block stage")
	}
	record := machine.rounds[machine.currentRound]
	if record == nil {
		return stateError(machine.block, string(from), "round has not been initialized")
	}
	for _, stage := range stageOrder[index:] {
		if record.Stages[stage].State == StateCommitted {
			return stateError(machine.block, string(from), "committed stage output is frozen")
		}
	}
	for _, stage := range stageOrder[index:] {
		record.Stages[stage].State = StatePending
		record.Stages[stage].Output = nil
		record.Stages[stage].Error = ""
	}
	return nil
}

func (machine *BlockStateMachine) Snapshot() StateSnapshot {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	snapshot := StateSnapshot{
		Block:           machine.block,
		CurrentRound:    machine.currentRound,
		CommittedStages: machine.committedStages,
		CommittedRounds: machine.committedRounds,
		Rounds:          make([]RoundRecord, 0, len(machine.rounds)),
	}
	roundNumbers := make([]int, 0, len(machine.rounds))
	for round := range machine.rounds {
		roundNumbers = append(roundNumbers, round)
	}
	sortInts(roundNumbers)
	for _, round := range roundNumbers {
		record := machine.rounds[round]
		copied := RoundRecord{Round: round, Stages: make(map[Stage]*StageRecord, len(record.Stages))}
		for stage, stageRecord := range record.Stages {
			stageCopy := &StageRecord{State: stageRecord.State, Error: stageRecord.Error}
			if stageRecord.Output != nil {
				output := stageRecord.Output.Clone()
				stageCopy.Output = &output
			}
			copied.Stages[stage] = stageCopy
		}
		snapshot.Rounds = append(snapshot.Rounds, copied)
	}
	return snapshot
}

func (machine *BlockStateMachine) Progress() (committedStages, committedRounds int) {
	machine.mu.Lock()
	defer machine.mu.Unlock()
	return machine.committedStages, machine.committedRounds
}

func (machine *BlockStateMachine) String() string {
	stages, rounds := machine.Progress()
	return fmt.Sprintf("block(%d,%d) stages=%d rounds=%d", machine.block.X, machine.block.Y, stages, rounds)
}

// ReplayCheckpointPrefix reconstructs per-block state machines from persisted
// commits in sequence order. It is used by resume diagnostics to prove that the
// stored prefix is a legal state-machine trace.
func ReplayCheckpointPrefix(checkpoints []Checkpoint) (map[Coord]*BlockStateMachine, *Error) {
	machines := make(map[Coord]*BlockStateMachine)
	for _, checkpoint := range checkpoints {
		machine := machines[checkpoint.Block]
		if machine == nil {
			machine = NewBlockStateMachine(checkpoint.Block)
			machines[checkpoint.Block] = machine
		}
		if err := machine.Start(checkpoint.Stage); err != nil {
			return nil, err
		}
		if err := machine.Commit(checkpoint.Stage, StageOutput{Digest: checkpoint.OutputDigest}); err != nil {
			return nil, err
		}
	}
	return machines, nil
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
