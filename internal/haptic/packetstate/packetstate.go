package packetstate

import (
	"sync"

	"github.com/liang/hapticfeedback/internal/haptic/errs"
)

type State string

const (
	Pending    State = "pending"
	Quantizing State = "quantizing"
	Split      State = "split"
	Committed  State = "committed"
	Failed     State = "failed"
)

var allowed = map[State]map[State]bool{
	Pending:    {Quantizing: true},
	Quantizing: {Split: true, Failed: true},
	Split:      {Committed: true, Quantizing: true},
	Failed:     {Quantizing: true},
	Committed:  {},
}

type Machine struct {
	Seq       int         `json:"seq"`
	Channel   string      `json:"channel"`
	State     State       `json:"state"`
	Attempts  int         `json:"attempts"`
	LastError *errs.Error `json:"last_error,omitempty"`
}

func (m *Machine) Transition(to State) error {
	if m.State == to {
		return nil
	}
	if !allowed[m.State][to] {
		return (&errs.Error{
			Code: errs.EState, Message: "illegal packet state transition",
			Seq: m.Seq, Channel: m.Channel,
			Details: map[string]any{"from": string(m.State), "to": string(to)},
		})
	}
	if to == Quantizing {
		m.Attempts++
	}
	m.State = to
	if to != Failed {
		m.LastError = nil
	}
	return nil
}

type Tracker struct {
	mu       sync.Mutex
	machines map[int]*Machine
	order    []int
}

func NewTracker() *Tracker {
	return &Tracker{machines: map[int]*Machine{}}
}

func (t *Tracker) Add(seq int, channel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.machines[seq]; !exists {
		t.machines[seq] = &Machine{Seq: seq, Channel: channel, State: Pending}
		t.order = append(t.order, seq)
	}
}

func (t *Tracker) RestoreCommitted(seq int, channel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.machines[seq]; !exists {
		t.order = append(t.order, seq)
	}
	t.machines[seq] = &Machine{Seq: seq, Channel: channel, State: Committed, Attempts: 1}
}

func (t *Tracker) Transition(seq int, to State, cause error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	m, ok := t.machines[seq]
	if !ok {
		return errs.New(errs.EState, "unknown packet sequence")
	}
	if err := m.Transition(to); err != nil {
		if he, ok := errs.As(err); ok {
			he.Seq = seq
			if m.Channel != "" {
				he.Channel = m.Channel
			}
		}
		return err
	}
	if to == Failed && cause != nil {
		if he, ok := errs.As(cause); ok {
			m.LastError = he
		} else {
			m.LastError = errs.Wrap(errs.EStage, "packet stage failed", cause)
			m.LastError.Seq = seq
			m.LastError.Channel = m.Channel
		}
	}
	return nil
}

func (t *Tracker) Get(seq int) Machine {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *t.machines[seq]
}

func (t *Tracker) Matrix() []Machine {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Machine, 0, len(t.order))
	for _, seq := range t.order {
		out = append(out, *t.machines[seq])
	}
	return out
}

func (t *Tracker) Progress() (committed, total int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, seq := range t.order {
		total++
		if t.machines[seq].State == Committed {
			committed++
		}
	}
	return committed, total
}

func (t *Tracker) Retry(seq int) error {
	return t.Transition(seq, Quantizing, nil)
}
