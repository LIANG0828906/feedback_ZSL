package state

import (
	"fmt"
	"sync"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

type CellState string

const (
	Pending    CellState = "pending"
	Processing CellState = "processing"
	Committed  CellState = "committed"
	Failed     CellState = "failed"
)

type Cell struct {
	EntryIndex   int          `json:"entry_index"`
	EntryID      string       `json:"entry_id"`
	Segment      int          `json:"segment"`
	State        CellState    `json:"state"`
	Version      int          `json:"version"`
	Revision     int          `json:"revision"`
	CacheKey     string       `json:"cache_key,omitempty"`
	Reason       string       `json:"reason,omitempty"`
	Reused       bool         `json:"reused"`
	Invalidated  bool         `json:"invalidated"`
	Snapshot     *model.Entry `json:"snapshot,omitempty"`
	SnapshotHash string       `json:"snapshot_hash,omitempty"`
}

type Matrix struct {
	mu       sync.Mutex
	Cells    map[string]*Cell
	Segments int
	Entries  int
	Version  int
	Revision int
}

func NewMatrix(entries, segments, version, revision int) *Matrix {
	m := &Matrix{
		Cells: make(map[string]*Cell), Segments: segments, Entries: entries,
		Version: version, Revision: revision,
	}
	for e := 1; e <= entries; e++ {
		for s := 0; s < segments; s++ {
			m.Cells[key(e, s)] = &Cell{EntryIndex: e, Segment: s, State: Pending, Version: version, Revision: revision}
		}
	}
	return m
}

func key(entry, segment int) string {
	return fmt.Sprintf("%d/%d", entry, segment)
}

func (m *Matrix) Get(entry, segment int) Cell {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.Cells[key(entry, segment)]
	if !ok {
		return Cell{EntryIndex: entry, Segment: segment, State: Failed, Reason: "missing cell"}
	}
	cp := *c
	if c.Snapshot != nil {
		snap := c.Snapshot.ParamSnapshot()
		cp.Snapshot = &snap
	}
	return cp
}

func (m *Matrix) Transition(entry, segment int, to CellState, version int, reason string, snapshot *model.Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.Cells[key(entry, segment)]
	if !ok {
		return apperr.At(apperr.EState, entry, "state", "cell %d/%d does not exist", entry, segment)
	}
	if c.State == Committed && version != c.Version {
		return &apperr.Error{
			Code: apperr.EVersion, EntryIndex: entry, Segment: segment,
			Field: "version", Version: version, Other: c.Version,
			Message: fmt.Sprintf("committed cell %d/%d is frozen at version %d, got %d", entry, segment, c.Version, version),
		}
	}
	if c.State == Committed && c.Revision != m.Revision && version == c.Version {
		return &apperr.Error{
			Code: apperr.EVersion, EntryIndex: entry, Segment: segment,
			Field: "revision", Version: m.Revision, Other: c.Revision,
			Message: fmt.Sprintf("committed cell %d/%d belongs to revision %d, matrix is revision %d", entry, segment, c.Revision, m.Revision),
		}
	}
	legal := false
	switch c.State {
	case Pending:
		legal = to == Processing
	case Failed:
		legal = to == Processing
	case Processing:
		legal = to == Committed || to == Failed
	}
	if !legal {
		return &apperr.Error{
			Code: apperr.EState, EntryIndex: entry, Segment: segment, Field: "state",
			Version: version, Other: c.Version,
			Message: fmt.Sprintf("illegal state transition %s -> %s for cell %d/%d", c.State, to, entry, segment),
		}
	}
	if to == Committed && snapshot == nil {
		return &apperr.Error{Code: apperr.EState, EntryIndex: entry, Segment: segment, Field: "snapshot", Message: "commit requires a frozen parameter snapshot"}
	}
	c.State = to
	c.Version = version
	c.Revision = m.Revision
	c.Reason = reason
	if to == Committed {
		snap := snapshot.ParamSnapshot()
		c.Snapshot = &snap
		c.SnapshotHash = snap.ParamSignature()
	}
	return nil
}

func (m *Matrix) MarkReuse(entry, segment int, cacheKey, reason string, reused, invalidated bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.Cells[key(entry, segment)]; ok {
		c.CacheKey = cacheKey
		c.Reason = reason
		c.Reused = reused
		c.Invalidated = invalidated
	}
}

func (m *Matrix) Progress() (committed, total, failed int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total = len(m.Cells)
	for _, c := range m.Cells {
		switch c.State {
		case Committed:
			committed++
		case Failed:
			failed++
		}
	}
	return
}

func (m *Matrix) SegmentCommitted(segment int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for e := 1; e <= m.Entries; e++ {
		c := m.Cells[key(e, segment)]
		if c == nil || c.State != Committed {
			return false
		}
	}
	return true
}

func (m *Matrix) Snapshot() []Cell {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Cell, 0, len(m.Cells))
	for e := 1; e <= m.Entries; e++ {
		for s := 0; s < m.Segments; s++ {
			c := *m.Cells[key(e, s)]
			if c.Snapshot != nil {
				snap := c.Snapshot.ParamSnapshot()
				c.Snapshot = &snap
			}
			out = append(out, c)
		}
	}
	return out
}
