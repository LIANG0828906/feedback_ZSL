package dialogue

import "sync"

// State is the clip lifecycle state.
type State string

const (
	StatePending     State = "pending"
	StateExtracting  State = "extracting"
	StateCalibrating State = "calibrating"
	StateCommitted   State = "committed"
	StateFailed      State = "failed"
)

var stateLabels = map[State]string{
	StatePending:     "待装载",
	StateExtracting:  "特征提取中",
	StateCalibrating: "响度校准中",
	StateCommitted:   "已提交",
	StateFailed:      "失败",
}

func (s State) Label() string {
	if label, ok := stateLabels[s]; ok {
		return label
	}
	return string(s)
}

var legalTransitions = map[State]map[State]bool{
	StatePending: {
		StateExtracting: true,
		StateFailed:     true,
	},
	StateExtracting: {
		StateCalibrating: true,
		StateFailed:      true,
	},
	StateCalibrating: {
		StateCommitted: true,
		StateFailed:    true,
	},
	StateFailed: {
		StateExtracting: true,
		StatePending:    true,
	},
	StateCommitted: {},
}

// CanTransition reports whether a state transition is legal.
func CanTransition(from, to State) bool {
	return legalTransitions[from][to]
}

// StateMatrix is a snapshot of clip states and commit progress.
type StateMatrix struct {
	TotalClips       int              `json:"total_clips"`
	CommittedClips   int              `json:"committed_clips"`
	FailedClips      int              `json:"failed_clips"`
	ProgressPermille int64            `json:"progress_permille"`
	States           map[string]State `json:"states"`
	Order            []string         `json:"order"`
}

// Machine owns the legal clip state transitions and stable progress accounting.
type Machine struct {
	mu     sync.Mutex
	states map[string]State
	order  []string
}

func NewMachine(clipIDs []string) *Machine {
	m := &Machine{states: make(map[string]State, len(clipIDs)), order: append([]string(nil), clipIDs...)}
	for _, id := range clipIDs {
		m.states[id] = StatePending
	}
	return m
}

func (m *Machine) State(clipID string) (State, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.states[clipID]
	return s, ok
}

// Transition changes state only for a legal edge. Illegal transitions return
// E_STATE and preserve both state and progress.
func (m *Machine) Transition(clipID string, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, ok := m.states[clipID]
	if !ok {
		return &Error{Code: CodeState, Message: "unknown clip", ClipID: clipID, ToState: to}
	}
	if !CanTransition(from, to) {
		return &Error{
			Code:      CodeState,
			Message:   "illegal clip state transition",
			ClipID:    clipID,
			FromState: from,
			ToState:   to,
			Details: map[string][]State{
				"legal_targets": legalTargetList(from),
			},
		}
	}
	m.states[clipID] = to
	return nil
}

func legalTargetList(from State) []State {
	out := make([]State, 0, len(legalTransitions[from]))
	for to := range legalTransitions[from] {
		out = append(out, to)
	}
	return out
}

func (m *Machine) Snapshot() StateMatrix {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap := StateMatrix{
		TotalClips: len(m.order),
		States:     make(map[string]State, len(m.order)),
		Order:      append([]string(nil), m.order...),
	}
	for _, id := range m.order {
		st := m.states[id]
		snap.States[id] = st
		switch st {
		case StateCommitted:
			snap.CommittedClips++
		case StateFailed:
			snap.FailedClips++
		}
	}
	if snap.TotalClips > 0 {
		snap.ProgressPermille = int64(snap.CommittedClips) * PermilleScale / int64(snap.TotalClips)
	}
	return snap
}
