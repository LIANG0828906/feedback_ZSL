package acousticfield

import (
	"fmt"
	"sync"
)

type ChunkState string

const (
	ChunkPending      ChunkState = "pending_filtering"
	ChunkFiltering    ChunkState = "path_filtering"
	ChunkTransferring ChunkState = "energy_transfer"
	ChunkCommitted    ChunkState = "committed"
	ChunkFailed       ChunkState = "failed"
)

type RoundState string

const (
	RoundPending    RoundState = "pending_rebuild"
	RoundRebuilding RoundState = "rebuilding"
	RoundCommitted  RoundState = "committed"
	RoundFailed     RoundState = "failed"
)

type StateSnapshot struct {
	CurrentRound    int64                `json:"current_round"`
	Chunks          map[int]ChunkState   `json:"chunks"`
	Rounds          map[int64]RoundState `json:"rounds"`
	CommittedChunks int                  `json:"committed_chunks"`
	CommittedRounds int                  `json:"committed_rounds"`
}

// StateMachine enforces the two-level state graph. Every mutation is under one
// mutex and illegal transitions leave both state and progress untouched.
type StateMachine struct {
	mu              sync.Mutex
	currentRound    int64
	chunks          map[int]ChunkState
	rounds          map[int64]RoundState
	committedChunks int
	committedRounds int
}

func NewStateMachine(chunkIDs []int) *StateMachine {
	m := &StateMachine{
		chunks: make(map[int]ChunkState, len(chunkIDs)),
		rounds: make(map[int64]RoundState),
	}
	for _, id := range chunkIDs {
		m.chunks[id] = ChunkPending
	}
	return m
}

func legalChunkTransition(from, to ChunkState) bool {
	switch from {
	case ChunkPending:
		return to == ChunkFiltering
	case ChunkFiltering:
		return to == ChunkTransferring || to == ChunkFailed
	case ChunkTransferring:
		return to == ChunkCommitted || to == ChunkFailed
	case ChunkFailed:
		return to == ChunkFiltering
	case ChunkCommitted:
		return to == ChunkPending // explicit invalidation only
	default:
		return false
	}
}

func legalRoundTransition(from, to RoundState) bool {
	switch from {
	case RoundPending:
		return to == RoundRebuilding
	case RoundRebuilding:
		return to == RoundCommitted || to == RoundFailed
	case RoundFailed:
		return to == RoundRebuilding
	case RoundCommitted:
		return false
	default:
		return false
	}
}

func (m *StateMachine) stateError(object, from, to string, progress int) error {
	return &FieldError{
		Code:      CodeState,
		ObjectID:  object,
		FieldPath: "state",
		Measured:  from,
		Message:   fmt.Sprintf("illegal transition %s -> %s; progress preserved", from, to),
		Values:    map[string]int64{"progress": int64(progress)},
	}
}

func (m *StateMachine) RegisterRound(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id <= 0 {
		return valueError(fmt.Sprintf("round-%d", id), "id", id, 1, int64(^uint64(0)>>1))
	}
	if _, exists := m.rounds[id]; exists {
		return &FieldError{Code: CodeState, ObjectID: fmt.Sprintf("round-%d", id), FieldPath: "id", Message: "round already registered", Values: map[string]int64{"progress": int64(m.committedRounds)}}
	}
	m.currentRound = id
	m.rounds[id] = RoundPending
	return nil
}

func (m *StateMachine) TransitionRound(id int64, to RoundState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.rounds[id]
	if !ok {
		return &FieldError{Code: CodeState, ObjectID: fmt.Sprintf("round-%d", id), FieldPath: "id", Message: "unknown round", Values: map[string]int64{"progress": int64(m.committedRounds)}}
	}
	if !legalRoundTransition(from, to) {
		return m.stateError(fmt.Sprintf("round-%d", id), string(from), string(to), m.committedRounds)
	}
	m.rounds[id] = to
	if to == RoundCommitted {
		m.committedRounds++
	}
	return nil
}

func (m *StateMachine) TransitionChunk(id int, to ChunkState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.chunks[id]
	if !ok {
		return &FieldError{Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", id), FieldPath: "state", Message: "unknown chunk", Values: map[string]int64{"progress": int64(m.committedChunks)}}
	}
	if !legalChunkTransition(from, to) {
		return m.stateError(fmt.Sprintf("chunk-%d", id), string(from), string(to), m.committedChunks)
	}
	if from == ChunkCommitted && to != ChunkCommitted {
		m.committedChunks--
	}
	if to == ChunkCommitted && from != ChunkCommitted {
		m.committedChunks++
	}
	m.chunks[id] = to
	return nil
}

// InvalidateChunk is the only sanctioned Committed -> Pending transition. The
// engine calls it after recording an edit reason.
func (m *StateMachine) InvalidateChunk(id int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.chunks[id]
	if !ok {
		return &FieldError{Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", id), FieldPath: "state", Message: "unknown chunk", Values: map[string]int64{"progress": int64(m.committedChunks)}}
	}
	if from != ChunkCommitted && from != ChunkFailed {
		return m.stateError(fmt.Sprintf("chunk-%d", id), string(from), string(ChunkPending), m.committedChunks)
	}
	if from == ChunkCommitted {
		m.committedChunks--
	}
	m.chunks[id] = ChunkPending
	return nil
}

func (m *StateMachine) ChunkState(id int) ChunkState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chunks[id]
}

func (m *StateMachine) RoundState(id int64) RoundState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rounds[id]
}

func (m *StateMachine) Progress() (committedChunks, committedRounds int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.committedChunks, m.committedRounds
}

func (m *StateMachine) CurrentRound() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentRound
}

func (m *StateMachine) Snapshot() StateSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := StateSnapshot{
		CurrentRound:    m.currentRound,
		Chunks:          make(map[int]ChunkState, len(m.chunks)),
		Rounds:          make(map[int64]RoundState, len(m.rounds)),
		CommittedChunks: m.committedChunks,
		CommittedRounds: m.committedRounds,
	}
	for k, v := range m.chunks {
		s.Chunks[k] = v
	}
	for k, v := range m.rounds {
		s.Rounds[k] = v
	}
	return s
}

// RetryChunk performs the only legal recovery transition for a failed unit.
func (m *StateMachine) RetryChunk(id int) error {
	return m.TransitionChunk(id, ChunkFiltering)
}

// Restore applies a durable checkpoint. This is intentionally not a normal
// transition: recovery reconstructs already-committed states from storage.
func (m *StateMachine) Restore(committedIDs []int, lastRound int64, completed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.chunks {
		m.chunks[id] = ChunkPending
	}
	m.committedChunks = 0
	for _, id := range committedIDs {
		if _, ok := m.chunks[id]; !ok {
			continue
		}
		if m.chunks[id] != ChunkCommitted {
			m.chunks[id] = ChunkCommitted
			m.committedChunks++
		}
	}
	m.currentRound = lastRound
	m.committedRounds = 0
	if lastRound > 0 {
		if completed {
			m.rounds[lastRound] = RoundCommitted
			m.committedRounds = 1
		} else {
			m.rounds[lastRound] = RoundFailed
		}
	}
}
