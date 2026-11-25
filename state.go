package texture

import (
	"fmt"
	"sync"
)

type RunState string

const (
	StatePending    RunState = "pending"
	StateProcessing RunState = "processing"
	StateCommitted  RunState = "committed"
	StateFailed     RunState = "failed"
)

type pairKey struct {
	Entry int
	Chunk int
}

// FrozenOutput is the immutable snapshot attached to a committed pair.
type FrozenOutput struct {
	EntryIndex        int    `json:"entry_index"`
	ChunkIndex        int    `json:"chunk_index"`
	ChunkX            int    `json:"chunk_x"`
	ChunkY            int    `json:"chunk_y"`
	OutputHash        string `json:"output_hash"`
	CacheKey          string `json:"cache_key"`
	ParameterHash     string `json:"parameter_hash"`
	ParameterSnapshot string `json:"parameter_snapshot"`
	PlanFingerprint   string `json:"plan_fingerprint"`
	PlanVersion       string `json:"plan_version"`
}

type pairState struct {
	state  RunState
	frozen *FrozenOutput
}

// StateMachine implements the per-entry/per-block lifecycle. All methods are
// safe for concurrent workers.
type StateMachine struct {
	mu      sync.Mutex
	pairs   map[pairKey]*pairState
	entries int
	chunks  int
	frozen  map[pairKey]FrozenOutput
}

func NewStateMachine(entries, chunks int) (*StateMachine, error) {
	if entries <= 0 || chunks <= 0 {
		return nil, errf(CodePara, "$.state", "entries/chunks", "must be positive")
	}
	m := &StateMachine{
		pairs:   make(map[pairKey]*pairState),
		frozen:  make(map[pairKey]FrozenOutput),
		entries: entries, chunks: chunks,
	}
	for e := 0; e < entries; e++ {
		for c := 0; c < chunks; c++ {
			m.pairs[pairKey{Entry: e, Chunk: c}] = &pairState{state: StatePending}
		}
	}
	return m, nil
}

func (m *StateMachine) get(entry, chunk int) (*pairState, error) {
	p, ok := m.pairs[pairKey{Entry: entry, Chunk: chunk}]
	if !ok {
		return nil, errf(CodePara, "$.state", "entry/chunk", "unknown pair entry=%d chunk=%d", entry, chunk)
	}
	return p, nil
}

func (m *StateMachine) Begin(entry, chunk int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.get(entry, chunk)
	if err != nil {
		return err
	}
	return transition(p, StateProcessing)
}

func (m *StateMachine) Commit(entry, chunk int, snapshot FrozenOutput) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.get(entry, chunk)
	if err != nil {
		return err
	}
	if snapshot.EntryIndex != entry || snapshot.ChunkIndex != chunk {
		return errf(CodeState, "$.state", "snapshot", "snapshot identity does not match pair")
	}
	if p.state != StateProcessing {
		return errf(CodeState, "$.state", "transition", "illegal transition %s -> %s for entry %d chunk %d", p.state, StateCommitted, entry, chunk)
	}
	p.state = StateCommitted
	p.frozen = &snapshot
	m.frozen[pairKey{Entry: entry, Chunk: chunk}] = snapshot
	return nil
}

func (m *StateMachine) Fail(entry, chunk int, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.get(entry, chunk)
	if err != nil {
		return err
	}
	if p.state != StateProcessing {
		return errf(CodeState, "$.state", "transition", "illegal transition %s -> %s for entry %d chunk %d", p.state, StateFailed, entry, chunk)
	}
	p.state = StateFailed
	if cause == nil {
		cause = fmt.Errorf("unspecified failure")
	}
	return nil
}

func (m *StateMachine) Retry(entry, chunk int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.get(entry, chunk)
	if err != nil {
		return err
	}
	return transition(p, StatePending)
}

func (m *StateMachine) State(entry, chunk int) (RunState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, err := m.get(entry, chunk)
	if err != nil {
		return "", err
	}
	return p.state, nil
}

func (m *StateMachine) Frozen(entry, chunk int) (FrozenOutput, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, ok := m.frozen[pairKey{Entry: entry, Chunk: chunk}]
	return out, ok
}

type StateProgress struct {
	CommittedPairs  int `json:"committed_pairs"`
	TotalPairs      int `json:"total_pairs"`
	CommittedChunks int `json:"committed_chunks"`
	TotalChunks     int `json:"total_chunks"`
}

func (m *StateMachine) Progress() StateProgress {
	m.mu.Lock()
	defer m.mu.Unlock()
	progress := StateProgress{TotalPairs: m.entries * m.chunks, TotalChunks: m.chunks}
	chunkCommitted := make([]bool, m.chunks)
	for key, pair := range m.pairs {
		if pair.state == StateCommitted {
			progress.CommittedPairs++
			chunkCommitted[key.Chunk] = true
		}
	}
	for _, committed := range chunkCommitted {
		if committed {
			progress.CommittedChunks++
		}
	}
	return progress
}

func transition(p *pairState, to RunState) error {
	from := p.state
	legal := false
	switch from {
	case StatePending:
		legal = to == StateProcessing
	case StateProcessing:
		legal = to == StateCommitted || to == StateFailed
	case StateFailed:
		legal = to == StatePending
	case StateCommitted:
		legal = false
	}
	if !legal {
		return errf(CodeState, "$.state", "transition", "illegal transition %s -> %s; state unchanged", from, to)
	}
	p.state = to
	return nil
}
