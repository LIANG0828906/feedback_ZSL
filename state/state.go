package state

import (
	"sort"

	"iconflow/errs"
)

type Stage string

const (
	StageLoaded    Stage = "loaded"
	StageGradient  Stage = "gradient"
	StageThreshold Stage = "threshold"
	StageMarked    Stage = "marked"
	StageCleaned   Stage = "cleaned_stats"
)

func Stages() []Stage {
	return []Stage{StageLoaded, StageGradient, StageThreshold, StageMarked, StageCleaned}
}

type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCommitted Status = "committed"
	StatusFailed    Status = "failed"
)

type WindowStatus string

const (
	WindowPending    WindowStatus = "pending"
	WindowSegmenting WindowStatus = "segmenting"
	WindowCommitted  WindowStatus = "committed"
	WindowFailed     WindowStatus = "failed"
)

type BlockMachine struct {
	Coord  string           `json:"coord"`
	Stages map[Stage]Status `json:"stages"`
}

func NewBlockMachine(coord string) *BlockMachine {
	m := &BlockMachine{Coord: coord, Stages: map[Stage]Status{}}
	for _, stage := range Stages() {
		m.Stages[stage] = StatusPending
	}
	return m
}

func (m *BlockMachine) Transition(stage Stage, next Status) *errs.Error {
	current, ok := m.Stages[stage]
	if !ok {
		return errs.State("$.block.stage", "unknown stage %q", stage)
	}
	allowed := false
	switch current {
	case StatusPending:
		allowed = next == StatusRunning
	case StatusRunning:
		allowed = next == StatusCommitted || next == StatusFailed
	case StatusFailed:
		allowed = next == StatusRunning
	case StatusCommitted:
		allowed = false
	}
	if !allowed {
		return errs.State("$.block."+string(stage), "illegal transition %s -> %s for block %s", current, next, m.Coord)
	}
	m.Stages[stage] = next
	return nil
}

func (m *BlockMachine) Commit(stage Stage) *errs.Error {
	if m.Stages[stage] == StatusCommitted {
		return nil
	}
	if m.Stages[stage] == StatusPending {
		if err := m.Transition(stage, StatusRunning); err != nil {
			return err
		}
	}
	return m.Transition(stage, StatusCommitted)
}

func (m *BlockMachine) Fail(stage Stage) *errs.Error {
	if m.Stages[stage] == StatusPending {
		if err := m.Transition(stage, StatusRunning); err != nil {
			return err
		}
	}
	return m.Transition(stage, StatusFailed)
}

func (m *BlockMachine) Retry(stage Stage) *errs.Error {
	if m.Stages[stage] != StatusFailed {
		return errs.State("$.block."+string(stage), "retry requires failed state, got %s", m.Stages[stage])
	}
	return m.Transition(stage, StatusRunning)
}

func (m *BlockMachine) CommittedStages() int {
	count := 0
	for _, stage := range Stages() {
		if m.Stages[stage] == StatusCommitted {
			count++
		}
	}
	return count
}

func (m *BlockMachine) Frozen() bool {
	return m.CommittedStages() == len(Stages())
}

type WindowMachine struct {
	ID     string       `json:"id"`
	Status WindowStatus `json:"status"`
}

func NewWindowMachine(id string) *WindowMachine {
	return &WindowMachine{ID: id, Status: WindowPending}
}

func (m *WindowMachine) Transition(next WindowStatus) *errs.Error {
	current := m.Status
	allowed := false
	switch current {
	case WindowPending:
		allowed = next == WindowSegmenting
	case WindowSegmenting:
		allowed = next == WindowCommitted || next == WindowFailed
	case WindowFailed:
		allowed = next == WindowSegmenting
	case WindowCommitted:
		allowed = false
	}
	if !allowed {
		return errs.State("$.window.status", "illegal window transition %s -> %s for %s", current, next, m.ID)
	}
	m.Status = next
	return nil
}

type Progress struct {
	CommittedBlocks  int `json:"committed_blocks"`
	TotalBlocks      int `json:"total_blocks"`
	CommittedWindows int `json:"committed_windows"`
	TotalWindows     int `json:"total_windows"`
}

func CountCommittedBlocks(blocks map[string]*BlockMachine) int {
	count := 0
	for _, key := range SortedBlockMachines(blocks) {
		if blocks[key].Frozen() {
			count++
		}
	}
	return count
}

func CountCommittedWindows(windows map[string]*WindowMachine) int {
	count := 0
	for _, key := range SortedWindows(windows) {
		if windows[key].Status == WindowCommitted {
			count++
		}
	}
	return count
}

func SortedBlockMachines(m map[string]*BlockMachine) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func SortedWindows(m map[string]*WindowMachine) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
