package statemachine

import (
	"fmt"

	"screenrelay/internal/model"
)

type SessionState string

const (
	SessionEstablishing SessionState = "establishing"
	SessionNegotiating  SessionState = "negotiating"
	SessionCasting      SessionState = "casting"
	SessionPaused       SessionState = "paused"
	SessionEnded        SessionState = "ended"
)

type SessionMachine struct {
	Current SessionState `json:"current"`
}

func NewSession() *SessionMachine { return &SessionMachine{Current: SessionEstablishing} }

func (m *SessionMachine) Transition(to SessionState) error {
	allowed := map[SessionState][]SessionState{
		SessionEstablishing: {SessionNegotiating, SessionEnded},
		SessionNegotiating:  {SessionCasting, SessionEnded},
		SessionCasting:      {SessionPaused, SessionEnded},
		SessionPaused:       {SessionCasting, SessionEnded},
		SessionEnded:        {},
	}
	for _, candidate := range allowed[m.Current] {
		if candidate == to {
			m.Current = to
			return nil
		}
	}
	return model.NewError(model.EState, "illegal session transition %s -> %s", m.Current, to)
}

type ScreenState string

const (
	ScreenPending     ScreenState = "pending_negotiation"
	ScreenNegotiating ScreenState = "negotiating"
	ScreenCommitted   ScreenState = "committed"
	ScreenFailed      ScreenState = "failed"
)

type ScreenMachine struct {
	ScreenID string      `json:"screen_id"`
	Current  ScreenState `json:"current"`
}

func NewScreen(id string) *ScreenMachine {
	return &ScreenMachine{ScreenID: id, Current: ScreenPending}
}

func (m *ScreenMachine) Transition(to ScreenState) error {
	allowed := map[ScreenState][]ScreenState{
		ScreenPending:     {ScreenNegotiating, ScreenFailed},
		ScreenNegotiating: {ScreenCommitted, ScreenFailed},
		ScreenCommitted:   {ScreenNegotiating, ScreenFailed},
		ScreenFailed:      {ScreenNegotiating},
	}
	for _, candidate := range allowed[m.Current] {
		if candidate == to {
			m.Current = to
			return nil
		}
	}
	return &model.Error{Code: model.EState, ScreenID: m.ScreenID, Message: fmt.Sprintf("illegal screen transition %s -> %s", m.Current, to)}
}

type WindowState string

const (
	WindowPending      WindowState = "pending_dispatch"
	WindowDistributing WindowState = "distributing"
	WindowCommitted    WindowState = "committed"
	WindowFailed       WindowState = "failed"
)

type WindowMachine struct {
	Seq     int         `json:"seq"`
	Current WindowState `json:"current"`
}

func NewWindow(seq int) *WindowMachine {
	return &WindowMachine{Seq: seq, Current: WindowPending}
}

func (m *WindowMachine) Transition(to WindowState) error {
	allowed := map[WindowState][]WindowState{
		WindowPending:      {WindowDistributing, WindowFailed},
		WindowDistributing: {WindowCommitted, WindowFailed},
		WindowFailed:       {WindowDistributing},
		WindowCommitted:    {},
	}
	for _, candidate := range allowed[m.Current] {
		if candidate == to {
			m.Current = to
			return nil
		}
	}
	return model.NewError(model.EState, "window %d illegal transition %s -> %s", m.Seq, m.Current, to)
}
