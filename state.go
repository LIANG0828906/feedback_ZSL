package audiofp

import (
	"fmt"
	"sync"
)

type WindowState string

const (
	StatePending    WindowState = "pending"
	StateExtracting WindowState = "extracting"
	StateCommitted  WindowState = "committed"
	StateFailed     WindowState = "failed"
)

var allowedTransitions = map[WindowState][]WindowState{
	StatePending:    {StateExtracting},
	StateExtracting: {StateCommitted, StateFailed},
	StateFailed:     {StateExtracting},
	StateCommitted:  {},
}

type Window struct {
	Index         int          `json:"index"`
	StartSample   int          `json:"start_sample"`
	State         WindowState  `json:"state"`
	Attempts      int          `json:"attempts"`
	Digest        string       `json:"digest,omitempty"`
	Fingerprint   *Fingerprint `json:"fingerprint,omitempty"`
	ParamVersion  string       `json:"param_version,omitempty"`
	ParamSnapshot *Params      `json:"param_snapshot,omitempty"`
	LastError     string       `json:"last_error,omitempty"`
}

type WindowMachine struct {
	mu      sync.RWMutex
	windows []*Window
}

func NewWindowMachine(starts []int, params Params) *WindowMachine {
	windows := make([]*Window, len(starts))
	for i, start := range starts {
		p := params
		windows[i] = &Window{
			Index:         i,
			StartSample:   start,
			State:         StatePending,
			ParamVersion:  params.Version(),
			ParamSnapshot: &p,
		}
	}
	return &WindowMachine{windows: windows}
}

func NewFrameWindowMachine(sampleCount int, params Params) *WindowMachine {
	count := FrameCount(sampleCount, params)
	starts := make([]int, count)
	for i := range starts {
		starts[i] = FrameStart(i, params)
	}
	return NewWindowMachine(starts, params)
}

func (m *WindowMachine) Total() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.windows)
}

// Progress returns committed window count and total window count.
func (m *WindowMachine) Progress() (committed, total int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, w := range m.windows {
		if w.State == StateCommitted {
			committed++
		}
	}
	return committed, len(m.windows)
}

func (m *WindowMachine) States() []WindowState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]WindowState, len(m.windows))
	for i, w := range m.windows {
		out[i] = w.State
	}
	return out
}

func (m *WindowMachine) Window(index int) (Window, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if index < 0 || index >= len(m.windows) {
		return Window{}, false
	}
	return *m.windows[index], true
}

func (m *WindowMachine) Snapshot() []Window {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Window, len(m.windows))
	for i, w := range m.windows {
		out[i] = *w
		if w.Fingerprint != nil {
			fp := *w.Fingerprint
			fp.Bands = append([]int(nil), w.Fingerprint.Bands...)
			out[i].Fingerprint = &fp
		}
		if w.ParamSnapshot != nil {
			p := *w.ParamSnapshot
			out[i].ParamSnapshot = &p
		}
	}
	return out
}

func canTransition(from, to WindowState) bool {
	for _, candidate := range allowedTransitions[from] {
		if candidate == to {
			return true
		}
	}
	return false
}

// Transition applies a legal state transition or returns E_STATE without
// mutating the window state.
func (m *WindowMachine) Transition(index int, to WindowState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.windows) {
		return stateError(index, "", to)
	}
	w := m.windows[index]
	from := w.State
	if !canTransition(from, to) {
		return stateError(index, from, to)
	}
	w.State = to
	return nil
}

func (m *WindowMachine) Start(index int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.windows) {
		return stateError(index, "", StateExtracting)
	}
	w := m.windows[index]
	if !canTransition(w.State, StateExtracting) {
		return stateError(index, w.State, StateExtracting)
	}
	w.State = StateExtracting
	w.Attempts++
	w.LastError = ""
	return nil
}

func (m *WindowMachine) Fail(index int, cause error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.windows) {
		return stateError(index, "", StateFailed)
	}
	w := m.windows[index]
	if !canTransition(w.State, StateFailed) {
		return stateError(index, w.State, StateFailed)
	}
	w.State = StateFailed
	if cause != nil {
		w.LastError = cause.Error()
	} else {
		w.LastError = "extraction failed"
	}
	return nil
}

// Commit freezes the fingerprint and parameter snapshot in a committed window.
func (m *WindowMachine) Commit(index int, fp Fingerprint, params Params) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if index < 0 || index >= len(m.windows) {
		return stateError(index, "", StateCommitted)
	}
	w := m.windows[index]
	if !canTransition(w.State, StateCommitted) {
		return stateError(index, w.State, StateCommitted)
	}
	fpCopy := fp
	fpCopy.Bands = append([]int(nil), fp.Bands...)
	pCopy := params
	w.Fingerprint = &fpCopy
	w.Digest = fpCopy.Digest()
	w.ParamSnapshot = &pCopy
	w.ParamVersion = params.Version()
	w.State = StateCommitted
	w.LastError = ""
	return nil
}

// UpdateUncommittedParameters changes parameter snapshots only for pending and
// failed windows. Committed windows remain frozen and the returned count is
// the number of changed uncommitted windows.
func (m *WindowMachine) UpdateUncommittedParameters(params Params) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := 0
	for _, w := range m.windows {
		if w.State == StateCommitted {
			continue
		}
		p := params
		w.ParamSnapshot = &p
		w.ParamVersion = params.Version()
		changed++
	}
	return changed
}

func (m *WindowMachine) CommittedFingerprints() []Fingerprint {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Fingerprint, 0, len(m.windows))
	for _, w := range m.windows {
		if w.State == StateCommitted && w.Fingerprint != nil {
			fp := *w.Fingerprint
			fp.Bands = append([]int(nil), w.Fingerprint.Bands...)
			out = append(out, fp)
		}
	}
	return out
}

// OrderedCommittedFingerprints returns a dense frame slice when committed
// windows form the stable prefix 0..n-1. Resume and ordered commit use this
// invariant.
func (m *WindowMachine) OrderedCommittedFingerprints() ([]Fingerprint, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	count := 0
	for count < len(m.windows) && m.windows[count].State == StateCommitted {
		count++
	}
	for i := count; i < len(m.windows); i++ {
		if m.windows[i].State == StateCommitted {
			return nil, newError(CodeState, fmt.Sprintf("windows[%d]", i),
				"committed windows are not a stable prefix; first uncommitted index=%d", count)
		}
	}
	out := make([]Fingerprint, count)
	for i := 0; i < count; i++ {
		fp := *m.windows[i].Fingerprint
		fp.Bands = append([]int(nil), m.windows[i].Fingerprint.Bands...)
		out[i] = fp
	}
	return out, nil
}
