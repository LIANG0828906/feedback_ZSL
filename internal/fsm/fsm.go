package fsm

import (
	"fmt"
	"sync"

	"ocrworkbench/internal/core"
)

type TilePhase string
type LinePhase string

const (
	TilePending   TilePhase = "pending"
	TileDetecting TilePhase = "detecting"
	TileCommitted TilePhase = "committed"
	TileFailed    TilePhase = "failed"

	LinePending     LinePhase = "pending_recognition"
	LineRecognizing LinePhase = "recognizing"
	LineCommitted   LinePhase = "committed"
	LineFailed      LinePhase = "failed"
)

var tileTransitions = map[TilePhase]map[TilePhase]bool{
	TilePending:   {TileDetecting: true},
	TileDetecting: {TileCommitted: true, TileFailed: true},
	TileFailed:    {TilePending: true},
	TileCommitted: {},
}

var lineTransitions = map[LinePhase]map[LinePhase]bool{
	LinePending:     {LineRecognizing: true},
	LineRecognizing: {LineCommitted: true, LineFailed: true},
	LineFailed:      {LinePending: true},
	LineCommitted:   {},
}

type TileState struct {
	mu    sync.RWMutex
	Index int
	Phase TilePhase
}

func NewTile(index int) *TileState {
	return &TileState{Index: index, Phase: TilePending}
}

func (s *TileState) PhaseValue() TilePhase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Phase
}

func (s *TileState) Transition(next TilePhase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !tileTransitions[s.Phase][next] {
		return core.NewError(core.CodeState, fmt.Sprintf("tile %d cannot transition %s -> %s", s.Index, s.Phase, next))
	}
	s.Phase = next
	return nil
}

type LineState struct {
	mu    sync.RWMutex
	Index int
	Phase LinePhase
}

func NewLine(index int) *LineState {
	return &LineState{Index: index, Phase: LinePending}
}

func (s *LineState) PhaseValue() LinePhase {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Phase
}

func (s *LineState) Transition(next LinePhase) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !lineTransitions[s.Phase][next] {
		return core.NewError(core.CodeState, fmt.Sprintf("line %d cannot transition %s -> %s", s.Index, s.Phase, next))
	}
	s.Phase = next
	return nil
}

type Progress struct {
	mu    sync.RWMutex
	Tiles map[int]*TileState
	Lines map[int]*LineState
}

func NewProgress() *Progress {
	return &Progress{Tiles: map[int]*TileState{}, Lines: map[int]*LineState{}}
}

func (p *Progress) AddTile(index int) *TileState {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := NewTile(index)
	p.Tiles[index] = state
	return state
}

func (p *Progress) AddLine(index int) *LineState {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := NewLine(index)
	p.Lines[index] = state
	return state
}

func (p *Progress) Counts() (tilesCommitted, tilesTotal, linesCommitted, linesTotal int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, tile := range p.Tiles {
		tilesTotal++
		if tile.PhaseValue() == TileCommitted {
			tilesCommitted++
		}
	}
	for _, line := range p.Lines {
		linesTotal++
		if line.PhaseValue() == LineCommitted {
			linesCommitted++
		}
	}
	return
}
