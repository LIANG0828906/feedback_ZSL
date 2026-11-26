package engine

import (
	"sort"
	"time"

	"quotakernel/gate"
	"quotakernel/model"
)

type Result struct {
	Reports    []model.TickReport `json:"reports"`
	FinalState *State             `json:"-"`
}

type BeforeTickHook func(state *State, tick model.Tick) error

type Engine struct {
	Project    *model.Project
	Rules      model.Rules
	Events     map[model.Tick][]model.Event
	horizon    model.Tick
	gate       *gate.FastGate
	now        func() time.Time
	BeforeTick BeforeTickHook
}

func New(project *model.Project) *Engine {
	rules := project.Rules
	if rules.FixedToleranceBytes == 0 {
		rules = model.DefaultRules()
	}
	events := make(map[model.Tick][]model.Event)
	var last model.Tick
	maxWait := rules.MaxWaitTicks
	for _, ev := range project.Events {
		events[ev.Tick] = append(events[ev.Tick], ev)
		if ev.Tick > last {
			last = ev.Tick
		}
	}
	for _, tenant := range project.Tenants {
		if tenant.MaxWaitTicks > maxWait {
			maxWait = tenant.MaxWaitTicks
		}
	}
	if maxWait < 1 {
		maxWait = model.DefaultRules().MaxWaitTicks
	}
	return &Engine{
		Project: project,
		Rules:   rules,
		Events:  events,
		horizon: last + maxWait + 1,
		gate:    gate.New(time.Duration(rules.GateBudgetNs)),
		now:     time.Now,
	}
}

func (e *Engine) WithClock(now func() time.Time) *Engine {
	e.now = now
	e.gate.Now = now
	return e
}

func (e *Engine) Horizon() model.Tick { return e.horizon }

// Run performs a deterministic full calculation from tick zero.
func (e *Engine) Run() (*Result, error) {
	state := NewState(e.Project)
	return e.RunFrom(state, e.horizon)
}

// RunFrom continues from a committed snapshot. The next settled tick is
// state.Tick+1, which prevents an incomplete tick from being replayed as
// committed twice.
func (e *Engine) RunFrom(state *State, through model.Tick) (*Result, error) {
	if through == 0 {
		through = e.horizon
	}
	reports := []model.TickReport{}
	for tick := state.Tick + 1; tick <= through; tick++ {
		if e.BeforeTick != nil {
			if err := e.BeforeTick(state, tick); err != nil {
				return nil, err
			}
		}
		events := append([]model.Event(nil), e.Events[tick]...)
		report, err := settleTick(e, state, tick, events)
		if err != nil {
			return nil, err
		}
		state.Tick = tick
		reports = append(reports, *report)
	}
	return &Result{Reports: reports, FinalState: state}, nil
}

func sortedEventCopy(events []model.Event) []model.Event {
	out := append([]model.Event(nil), events...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tick != out[j].Tick {
			return out[i].Tick < out[j].Tick
		}
		if out[i].SubmittedOrder != out[j].SubmittedOrder {
			return out[i].SubmittedOrder < out[j].SubmittedOrder
		}
		return out[i].ID < out[j].ID
	})
	return out
}
