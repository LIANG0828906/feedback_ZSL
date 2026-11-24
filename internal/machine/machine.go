// Package machine implements the per-packet lifecycle state machine.
package machine

import (
	"fmt"
	"sync"

	"audioforge/internal/errs"
)

type State string

const (
	PendingQuantization State = "pending_quantization"
	Quantizing          State = "quantizing"
	Sliced              State = "sliced"
	Committed           State = "committed"
	Failed              State = "failed"
)

var legal = map[State]map[State]bool{
	PendingQuantization: {Quantizing: true, Failed: true},
	Quantizing:          {Sliced: true, Failed: true},
	Sliced:              {Committed: true, Failed: true},
	Committed:           {},
	Failed:              {},
}

type Transition struct {
	From State  `json:"from"`
	To   State  `json:"to"`
	At   string `json:"at"`
}

type PacketMachine struct {
	mu      sync.Mutex
	channel int
	seq     int
	state   State
	history []Transition
}

func New(channel, seq int) *PacketMachine {
	return &PacketMachine{
		channel: channel,
		seq:     seq,
		state:   PendingQuantization,
		history: []Transition{{From: "", To: PendingQuantization}},
	}
}

func (m *PacketMachine) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *PacketMachine) History() []Transition {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Transition(nil), m.history...)
}

func (m *PacketMachine) Transition(to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !legal[m.state][to] {
		return &errs.Error{
			Code:      errs.EState,
			Message:   fmt.Sprintf("illegal transition %s -> %s", m.state, to),
			PacketSeq: m.seq,
			Channel:   m.channel,
			Field:     "state",
		}
	}
	from := m.state
	m.state = to
	m.history = append(m.history, Transition{From: from, To: to})
	return nil
}

type Manager struct {
	mu       sync.Mutex
	machines map[string]*PacketMachine
}

func NewManager() *Manager {
	return &Manager{machines: make(map[string]*PacketMachine)}
}

func (m *Manager) Ensure(channel, seq int) *PacketMachine {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%d/%d", channel, seq)
	if existing, ok := m.machines[key]; ok {
		return existing
	}
	machine := New(channel, seq)
	m.machines[key] = machine
	return machine
}

func (m *Manager) Get(channel, seq int) (*PacketMachine, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := fmt.Sprintf("%d/%d", channel, seq)
	machine, ok := m.machines[key]
	return machine, ok
}

func (m *Manager) States() map[string]State {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]State, len(m.machines))
	for key, machine := range m.machines {
		out[key] = machine.State()
	}
	return out
}
