package state

import (
	"fmt"
	"sync"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

type Stage string

const (
	StageForeground Stage = "foreground_separation"
	StageEdgeTrace  Stage = "edge_tracing"
	StageRefine     Stage = "refinement"
)

func Stages() []Stage {
	return []Stage{StageForeground, StageEdgeTrace, StageRefine}
}

func StageIndex(s Stage) int {
	for i, v := range Stages() {
		if v == s {
			return i
		}
	}
	return -1
}

type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusCommitted  Status = "committed"
	StatusFailed     Status = "failed"
)

type Machine struct {
	Tile   model.TileCoord `json:"tile"`
	States [3]Status       `json:"states"`
}

func NewMachine(tile model.TileCoord) *Machine {
	return &Machine{Tile: tile, States: [3]Status{StatusPending, StatusPending, StatusPending}}
}

var allowed = map[Status]map[Status]bool{
	StatusPending:    {StatusProcessing: true},
	StatusProcessing: {StatusCommitted: true, StatusFailed: true, StatusPending: true},
	StatusFailed:     {StatusProcessing: true},
	StatusCommitted:  {},
}

func (m *Machine) Transition(stage Stage, to Status) error {
	idx := StageIndex(stage)
	if idx < 0 {
		return fmt.Errorf("unknown stage %q", stage)
	}
	from := m.States[idx]
	if from == to {
		return nil
	}
	if !allowed[from][to] {
		return errs.State(m.Tile.Array(), string(stage), string(from), string(to))
	}
	m.States[idx] = to
	return nil
}

func (m *Machine) Clone() Machine {
	return Machine{Tile: m.Tile, States: m.States}
}

type Matrix struct {
	mu       sync.Mutex
	machines map[[2]int]*Machine
	order    []model.TileCoord
	sequence []string
}

func NewMatrix(tiles []model.TileCoord) *Matrix {
	m := &Matrix{machines: make(map[[2]int]*Machine, len(tiles)), order: append([]model.TileCoord(nil), tiles...)}
	for _, t := range tiles {
		m.machines[t.Array()] = NewMachine(t)
	}
	return m
}

func (m *Matrix) Transition(tile model.TileCoord, stage Stage, to Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mac := m.machines[tile.Array()]
	if mac == nil {
		return errs.State(tile.Array(), string(stage), "missing", string(to))
	}
	if err := mac.Transition(stage, to); err != nil {
		return err
	}
	if to == StatusCommitted {
		m.sequence = append(m.sequence, fmt.Sprintf("%d,%d:%s", tile.TX, tile.TY, stage))
	}
	return nil
}

func (m *Matrix) RetryFailed(tile model.TileCoord, stage Stage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	mac := m.machines[tile.Array()]
	if mac == nil {
		return errs.State(tile.Array(), string(stage), "missing", "processing")
	}
	idx := StageIndex(stage)
	if mac.States[idx] != StatusFailed {
		return errs.State(tile.Array(), string(stage), string(mac.States[idx]), "processing")
	}
	mac.States[idx] = StatusProcessing
	return nil
}

func (m *Matrix) Snapshot() []Machine {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Machine, 0, len(m.order))
	for _, t := range m.order {
		out = append(out, m.machines[t.Array()].Clone())
	}
	return out
}

func (m *Matrix) Sequence() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.sequence...)
}

func (m *Matrix) CommittedStages() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, mac := range m.machines {
		for _, s := range mac.States {
			if s == StatusCommitted {
				n++
			}
		}
	}
	return n
}

func (m *Matrix) TotalStages() int {
	return len(m.order) * len(Stages())
}
