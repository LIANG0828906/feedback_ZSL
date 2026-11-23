package fsm

import (
	"fmt"
	"sort"
	"sync"

	"roomreverb/internal/errs"
	"roomreverb/internal/model"
	"roomreverb/internal/solver"
)

type State string

const (
	Pending      State = "pending"
	Connectivity State = "connectivity"
	Parameters   State = "parameters"
	Committed    State = "committed"
	Failed       State = "failed"
)

func (s State) Label() string {
	switch s {
	case Pending:
		return "待求解"
	case Connectivity:
		return "连通分析中"
	case Parameters:
		return "参数求解中"
	case Committed:
		return "已提交"
	case Failed:
		return "失败"
	default:
		return string(s)
	}
}

var allowed = map[State]map[State]bool{
	Pending:      {Connectivity: true},
	Connectivity: {Parameters: true, Failed: true},
	Parameters:   {Committed: true, Failed: true},
	Failed:       {Pending: true},
	Committed:    {},
}

type Frozen struct {
	Params           model.RoomParams    `json:"params"`
	Portals          []solver.PortalRef  `json:"portals"`
	Neighbors        []string            `json:"neighbors"`
	Pairs            []solver.PairResult `json:"pairs"`
	Result           solver.RoomResult   `json:"result"`
	ContentHash      string              `json:"content_hash"`
	ParamsVersion    int                 `json:"params_version"`
	AlgorithmVersion string              `json:"algorithm_version"`
}

type RoomState struct {
	ID            string  `json:"room_id"`
	State         State   `json:"state"`
	StateLabel    string  `json:"state_label"`
	LastError     string  `json:"last_error,omitempty"`
	InvalidReason string  `json:"invalid_reason,omitempty"`
	Frozen        *Frozen `json:"frozen,omitempty"`
}

type Tracker struct {
	mu     sync.RWMutex
	states map[string]*RoomState
	order  []string
}

func New(roomIDs []string) *Tracker {
	t := &Tracker{states: make(map[string]*RoomState, len(roomIDs)), order: append([]string(nil), roomIDs...)}
	sort.Strings(t.order)
	for _, id := range t.order {
		t.states[id] = &RoomState{ID: id, State: Pending, StateLabel: Pending.Label()}
	}
	return t
}

func (t *Tracker) Transition(roomID string, to State, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs, ok := t.states[roomID]
	if !ok {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: "unknown room"}
	}
	if !allowed[rs.State][to] {
		return &errs.Error{
			Code: errs.EState, Room: roomID, Field: "state",
			Message: fmt.Sprintf("illegal transition %s -> %s", rs.State, to),
		}
	}
	rs.State = to
	rs.StateLabel = to.Label()
	rs.LastError = ""
	if to == Pending {
		rs.Frozen = nil
		rs.InvalidReason = reason
	} else {
		rs.InvalidReason = ""
	}
	return nil
}

// Invalidate is the explicit editor transition that unfreezes a committed
// room. A frozen snapshot is never edited in place.
func (t *Tracker) Invalidate(roomID, reason string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs, ok := t.states[roomID]
	if !ok {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: "unknown room"}
	}
	if reason == "" {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "reason", Message: "invalidation reason is required"}
	}
	if rs.State != Committed {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: fmt.Sprintf("cannot invalidate state %s", rs.State)}
	}
	rs.State = Pending
	rs.StateLabel = Pending.Label()
	rs.Frozen = nil
	rs.InvalidReason = reason
	rs.LastError = ""
	return nil
}

func (t *Tracker) Fail(roomID string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs, ok := t.states[roomID]
	if !ok {
		return
	}
	if rs.State != Connectivity && rs.State != Parameters {
		return
	}
	rs.State = Failed
	rs.StateLabel = Failed.Label()
	rs.LastError = err.Error()
}

func (t *Tracker) Commit(roomID string, frozen Frozen) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs, ok := t.states[roomID]
	if !ok {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: "unknown room"}
	}
	if rs.State != Parameters {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: fmt.Sprintf("commit requires parameters state, got %s", rs.State)}
	}
	f := cloneFrozen(frozen)
	rs.Frozen = &f
	rs.State = Committed
	rs.StateLabel = Committed.Label()
	rs.InvalidReason = ""
	rs.LastError = ""
	return nil
}

func (t *Tracker) Retry(roomID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs, ok := t.states[roomID]
	if !ok {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: "unknown room"}
	}
	if rs.State != Failed {
		return &errs.Error{Code: errs.EState, Room: roomID, Field: "state", Message: fmt.Sprintf("retry requires failed state, got %s", rs.State)}
	}
	rs.State = Pending
	rs.StateLabel = Pending.Label()
	rs.LastError = ""
	return nil
}

func (t *Tracker) Get(roomID string) (RoomState, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	rs, ok := t.states[roomID]
	if !ok {
		return RoomState{}, false
	}
	c := *rs
	if rs.Frozen != nil {
		f := cloneFrozen(*rs.Frozen)
		c.Frozen = &f
	}
	return c, true
}

type Progress struct {
	Committed int `json:"committed"`
	Total     int `json:"total"`
}

func (t *Tracker) Progress() Progress {
	t.mu.RLock()
	defer t.mu.RUnlock()
	p := Progress{Total: len(t.order)}
	for _, rs := range t.states {
		if rs.State == Committed {
			p.Committed++
		}
	}
	return p
}

func (t *Tracker) Matrix() []RoomState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]RoomState, 0, len(t.order))
	for _, id := range t.order {
		rs := t.states[id]
		c := *rs
		if rs.Frozen != nil {
			f := cloneFrozen(*rs.Frozen)
			c.Frozen = &f
		}
		out = append(out, c)
	}
	return out
}

func cloneFrozen(f Frozen) Frozen {
	out := f
	out.Portals = make([]solver.PortalRef, len(f.Portals))
	for i, p := range f.Portals {
		out.Portals[i] = p.Clone()
	}
	out.Neighbors = append([]string(nil), f.Neighbors...)
	out.Pairs = make([]solver.PairResult, len(f.Pairs))
	for i, p := range f.Pairs {
		out.Pairs[i] = p.Clone()
	}
	out.Result = f.Result.Clone()
	return out
}
