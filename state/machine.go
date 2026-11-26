// Package state implements the per-clip lifecycle and commit boundary.
package state

import (
	"fmt"
	"sync"

	"dialogueaudio/audioerr"
)

// Status is one of the five required clip states.
type Status string

const (
	Pending     Status = "pending"
	Measuring   Status = "measuring"
	Calibrating Status = "calibrating"
	Committed   Status = "committed"
	Failed      Status = "failed"
)

// Progress is committed clips over total clips.
type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

// Snapshot is safe to serialize for diagnostics.
type Snapshot struct {
	ClipID   string   `json:"clip_id"`
	Status   Status   `json:"status"`
	Frozen   bool     `json:"frozen"`
	Progress Progress `json:"progress"`
}

// Manager owns all clip statuses. It guards immutable committed boundaries.
type Manager struct {
	mu        sync.RWMutex
	total     int
	committed int
	statuses  map[string]Status
	frozen    map[string]bool
	order     []string
}

// New creates a manager in canonical clip order.
func New(clipIDs []string) (*Manager, error) {
	if len(clipIDs) == 0 {
		return nil, audioerr.Schema("clips", "at least one clip is required")
	}
	m := &Manager{
		total:    len(clipIDs),
		statuses: make(map[string]Status, len(clipIDs)),
		frozen:   make(map[string]bool, len(clipIDs)),
		order:    append([]string(nil), clipIDs...),
	}
	for _, id := range clipIDs {
		if _, exists := m.statuses[id]; exists {
			return nil, audioerr.Schema("clips", "duplicate clip id "+id)
		}
		m.statuses[id] = Pending
	}
	return m, nil
}

func legal(from, to Status) bool {
	switch from {
	case Pending:
		return to == Measuring || to == Failed
	case Measuring:
		return to == Calibrating || to == Failed
	case Calibrating:
		return to == Committed || to == Failed
	case Committed, Failed:
		return false
	default:
		return false
	}
}

// Transition validates and applies one state transition. On an illegal
// transition neither the clip status nor any progress counter changes.
func (m *Manager) Transition(clipID string, to Status) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.statuses[clipID]
	if !ok {
		return audioerr.State("state."+clipID, "unknown clip")
	}
	if !legal(current, to) {
		return audioerr.State("state."+clipID,
			fmt.Sprintf("illegal transition %s -> %s", current, to)).
			WithValues(map[string]any{
				"current":            current,
				"requested":          to,
				"progress_committed": m.committed,
				"progress_total":     m.total,
			})
	}
	if to == Committed {
		if m.frozen[clipID] {
			return audioerr.State("state."+clipID, "clip is already frozen")
		}
		m.frozen[clipID] = true
		m.committed++
	}
	m.statuses[clipID] = to
	return nil
}

// StatusOf returns the current status.
func (m *Manager) StatusOf(clipID string) (Status, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status, ok := m.statuses[clipID]
	return status, ok
}

// Progress returns committed/total.
func (m *Manager) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Progress{Committed: m.committed, Total: m.total}
}

// EditAllowed reports whether a designer edit may target this clip. Only
// uncommitted, non-failed clips are editable.
func (m *Manager) EditAllowed(clipID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status, ok := m.statuses[clipID]
	return ok && status != Committed && status != Failed
}

// Frozen reports whether the clip's envelope and calibration are immutable.
func (m *Manager) Frozen(clipID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.frozen[clipID]
}

// Snapshot returns all statuses in canonical order.
func (m *Manager) Snapshot() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	progress := Progress{Committed: m.committed, Total: m.total}
	out := make([]Snapshot, 0, len(m.order))
	for _, id := range m.order {
		out = append(out, Snapshot{
			ClipID: id, Status: m.statuses[id], Frozen: m.frozen[id], Progress: progress,
		})
	}
	return out
}

// ClipIDs returns canonical order.
func (m *Manager) ClipIDs() []string {
	return append([]string(nil), m.order...)
}
