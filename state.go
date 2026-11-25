package textureconv

import (
	"sort"
	"sync"
)

type TileState string

const (
	StatePending    TileState = "pending"
	StateProcessing TileState = "processing"
	StateCommitted  TileState = "committed"
	StateFailed     TileState = "failed"
)

var legalTransitions = map[TileState]map[TileState]bool{
	StatePending:    {StateProcessing: true},
	StateProcessing: {StateCommitted: true, StateFailed: true},
	StateFailed:     {StateProcessing: true},
	StateCommitted:  {},
}

type stageTileID struct {
	Entry int
	Tile  int
}

// StateMachine owns the two-level (entry x tile) state matrix.
type StateMachine struct {
	mu         sync.Mutex
	states     map[stageTileID]TileState
	entryOrder []int
	tileCount  int
}

func NewStateMachine(entries []int, tileCount int) *StateMachine {
	sm := &StateMachine{
		states:     make(map[stageTileID]TileState, len(entries)*tileCount),
		entryOrder: append([]int(nil), entries...),
		tileCount:  tileCount,
	}
	for _, entry := range entries {
		for tile := 0; tile < tileCount; tile++ {
			sm.states[stageTileID{Entry: entry, Tile: tile}] = StatePending
		}
	}
	return sm
}

func (s *StateMachine) State(entry, tile int) TileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[stageTileID{Entry: entry, Tile: tile}]; ok {
		return st
	}
	return ""
}

// Transition applies one legal edge. Illegal edges return E_STATE and leave
// the original state untouched.
func (s *StateMachine) Transition(entry, tile int, to TileState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := stageTileID{Entry: entry, Tile: tile}
	from, ok := s.states[id]
	if !ok {
		return stateErr(entry, tile, "unknown state cell")
	}
	if !legalTransitions[from][to] {
		return stateErr(entry, tile, "illegal transition %s -> %s", from, to)
	}
	s.states[id] = to
	return nil
}

func (s *StateMachine) ResetCell(entry, tile int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[stageTileID{Entry: entry, Tile: tile}] = StatePending
}

func (s *StateMachine) MarkCommitted(entry, tile int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.states[stageTileID{Entry: entry, Tile: tile}] = StateCommitted
}

type StageProgress struct {
	Entry     int `json:"entry"`
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

type Progress struct {
	CommittedTiles int             `json:"committedTiles"`
	TotalTiles     int             `json:"totalTiles"`
	Stages         []StageProgress `json:"stages"`
}

func (s *StateMachine) Progress() Progress {
	s.mu.Lock()
	defer s.mu.Unlock()
	byEntry := map[int]int{}
	completeTiles := make(map[int]int)
	for id, st := range s.states {
		if st == StateCommitted {
			byEntry[id.Entry]++
			completeTiles[id.Tile]++
		}
	}
	p := Progress{TotalTiles: s.tileCount}
	for tile := 0; tile < s.tileCount; tile++ {
		if completeTiles[tile] == len(s.entryOrder) && len(s.entryOrder) > 0 {
			p.CommittedTiles++
		}
	}
	for _, e := range s.entryOrder {
		p.Stages = append(p.Stages, StageProgress{Entry: e, Committed: byEntry[e], Total: s.tileCount})
	}
	sort.Slice(p.Stages, func(i, j int) bool { return p.Stages[i].Entry < p.Stages[j].Entry })
	return p
}

func (s *StateMachine) EntryStates(entry int) []TileState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TileState, s.tileCount)
	for i := range out {
		out[i] = s.states[stageTileID{Entry: entry, Tile: i}]
	}
	return out
}
