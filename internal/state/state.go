package state

import (
	"fmt"

	"audiobus/errs"
)

type State string

const (
	Pending    State = "pending_quantization"
	Quantizing State = "quantizing"
	Sliced     State = "sliced"
	Committed  State = "committed"
	Failed     State = "failed"
)

var allowed = map[State]map[State]bool{
	Pending:    {Quantizing: true, Failed: true},
	Quantizing: {Sliced: true, Failed: true, Pending: true},
	Sliced:     {Committed: true, Failed: true, Quantizing: true},
	Failed:     {Pending: true, Quantizing: true},
	Committed:  {},
}

type Machine struct {
	Kind    string
	Channel int
	Window  int
	Packet  int
	Current State
}

func New(kind string, channel, window, packet int) *Machine {
	return &Machine{Kind: kind, Channel: channel, Window: window, Packet: packet, Current: Pending}
}

func (m *Machine) Transition(next State) error {
	if m.Current == next {
		return nil
	}
	if !allowed[m.Current][next] {
		e := errs.New(errs.EState, fmt.Sprintf("illegal %s transition %s -> %s", m.Kind, m.Current, next))
		e.Channel = m.Channel
		e.Window = m.Window
		e.Packet = m.Packet
		return e
	}
	m.Current = next
	return nil
}

func Can(from, to State) bool { return allowed[from][to] }
