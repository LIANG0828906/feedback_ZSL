// Package statefx defines the legal chunk and rebuild-round state machines.
package statefx

import (
	"sync"

	"acousticfield/internal/acoustic"
)

type ChunkState string

const (
	ChunkPending   ChunkState = "pending"
	ChunkScreening ChunkState = "path_screening"
	ChunkTransfer  ChunkState = "energy_transfer"
	ChunkCommitted ChunkState = "committed"
	ChunkFailed    ChunkState = "failed"
)

type RoundState string

const (
	RoundPending   RoundState = "pending"
	RoundRunning   RoundState = "rebuilding"
	RoundCommitted RoundState = "committed"
	RoundFailed    RoundState = "failed"
)

var chunkAllowed = map[ChunkState]map[ChunkState]bool{
	ChunkPending:   {ChunkScreening: true},
	ChunkScreening: {ChunkTransfer: true, ChunkFailed: true},
	ChunkTransfer:  {ChunkCommitted: true, ChunkFailed: true},
	ChunkFailed:    {ChunkScreening: true},
	ChunkCommitted: {},
}

var roundAllowed = map[RoundState]map[RoundState]bool{
	RoundPending:   {RoundRunning: true},
	RoundRunning:   {RoundCommitted: true, RoundFailed: true},
	RoundFailed:    {RoundRunning: true},
	RoundCommitted: {},
}

type ChunkMachine struct {
	mu    sync.Mutex
	id    string
	state ChunkState
}

func NewChunk(id string) *ChunkMachine {
	return &ChunkMachine{id: id, state: ChunkPending}
}

func (m *ChunkMachine) ID() string { return m.id }

func (m *ChunkMachine) State() ChunkState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *ChunkMachine) Transition(next ChunkState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !chunkAllowed[m.state][next] {
		return acoustic.StateError(m.id, string(m.state), "transition:"+string(next))
	}
	m.state = next
	return nil
}

type RoundMachine struct {
	mu    sync.Mutex
	id    string
	state RoundState
}

func NewRound(id string) *RoundMachine {
	return &RoundMachine{id: id, state: RoundPending}
}

func (m *RoundMachine) ID() string { return m.id }

func (m *RoundMachine) State() RoundState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *RoundMachine) Transition(next RoundState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !roundAllowed[m.state][next] {
		return acoustic.StateError(m.id, string(m.state), "transition:"+string(next))
	}
	m.state = next
	return nil
}

// Progress is the explicit two-level progress representation.
type Progress struct {
	mu              sync.Mutex
	TotalChunks     int
	CommittedChunks int
	CommittedRounds int64
}

func NewProgress(totalChunks int) *Progress {
	return &Progress{TotalChunks: totalChunks}
}

func (p *Progress) ChunkCommitted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CommittedChunks++
}

func (p *Progress) ResetChunks() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CommittedChunks = 0
}

func (p *Progress) RoundCommitted() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.CommittedRounds++
}

func (p *Progress) Snapshot() acoustic.Progress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return acoustic.Progress{
		CommittedChunks: p.CommittedChunks,
		TotalChunks:     p.TotalChunks,
		CommittedRounds: p.CommittedRounds,
	}
}
