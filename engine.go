package hckernel

import (
	"fmt"
	"sort"
)

type activeProbe struct {
	Probe
	DeadlineMs int64 `json:"deadline_ms"`
}

type InstanceState struct {
	Instance      Instance     `json:"instance"`
	Health        HealthState  `json:"health"`
	SuccessStreak int          `json:"success_streak"`
	FailureStreak int          `json:"failure_streak"`
	NextPeriodAt  int64        `json:"next_period_at"`
	BackoffUntil  int64        `json:"backoff_until"`
	HalfOpenAt    int64        `json:"half_open_at"`
	RetryAttempts int          `json:"retry_attempts"`
	Outstanding   *activeProbe `json:"outstanding,omitempty"`
	LastEventID   string       `json:"last_event_id,omitempty"`
}

type candidate struct {
	instanceID string
	rackID     string
	weightPPM  int64
	halfOpen   bool
	deferred   *DeferredEntry
}

// Engine is the deterministic logical-time scheduler and health evaluator.
type Engine struct {
	project Project
	events  []Event

	states       map[string]*InstanceState
	eventOrder   []Event
	eventCursor  int
	globalActive int
	rackActive   map[string]int
	queue        []DeferredEntry
	queueSeq     int64

	reports []RoundReport
	trace   []HealthObservation
	obsSeq  int
	nowMs   int64

	fingerprint string
}

func CloneProject(in Project) Project {
	out := in
	if in.Racks != nil {
		out.Racks = append([]Rack(nil), in.Racks...)
	}
	if in.Instances != nil {
		out.Instances = append([]Instance(nil), in.Instances...)
	}
	return out
}

// NewEngine creates an isolated engine. The project and event slices are copied
// so event/edit processing never modifies caller-owned source data.
func NewEngine(project *Project, events []Event) (*Engine, error) {
	if project == nil {
		return nil, newError(ErrSchemaPayloadInvalid, "nil project")
	}
	p := CloneProject(*project)
	if err := ValidateProject(&p); err != nil {
		return nil, err
	}
	fp, err := p.Fingerprint()
	if err != nil {
		return nil, newError(ErrSchemaPayloadInvalid, "fingerprint failed", withCause(err))
	}
	e := &Engine{
		project:     p,
		events:      append([]Event(nil), events...),
		states:      map[string]*InstanceState{},
		rackActive:  map[string]int{},
		fingerprint: fp,
	}
	if err := e.resetStates(); err != nil {
		return nil, err
	}
	e.sortEvents()
	return e, nil
}

func (e *Engine) resetStates() error {
	e.states = make(map[string]*InstanceState, len(e.project.Instances))
	for _, inst := range e.project.Instances {
		rt := &InstanceState{
			Instance: inst,
			Health:   inst.InitialState,
		}
		if rt.Health == StateUnhealthy {
			rt.HalfOpenAt = e.project.CountPolicy.HalfOpenDelayMs
		}
		if _, exists := e.states[inst.ID]; exists {
			return newError(ErrInstanceDuplicate, "duplicate runtime instance", withInstance(inst.ID))
		}
		e.states[inst.ID] = rt
	}
	for _, rack := range e.project.Racks {
		e.rackActive[rack.ID] = 0
	}
	return nil
}

func (e *Engine) sortEvents() {
	e.eventOrder = append([]Event(nil), e.events...)
	sort.SliceStable(e.eventOrder, func(i, j int) bool {
		if e.eventOrder[i].AtMs != e.eventOrder[j].AtMs {
			return e.eventOrder[i].AtMs < e.eventOrder[j].AtMs
		}
		return e.eventOrder[i].ID < e.eventOrder[j].ID
	})
}

func (e *Engine) sortedInstanceIDs() []string {
	ids := make([]string, 0, len(e.states))
	for id := range e.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Run executes all declared logical rounds.
func (e *Engine) Run() (*Result, error) {
	for round := 1; round <= e.project.Rounds; round++ {
		now := int64(round-1) * e.project.StepMs
		if err := e.Advance(now, round); err != nil {
			return nil, err
		}
	}
	return e.Result(), nil
}

// Advance processes due injections/mutations and then one scheduling round.
// Explicit now/round make resume and tests deterministic.
func (e *Engine) Advance(nowMs int64, round int) error {
	e.nowMs = nowMs
	report := RoundReport{
		Round:  round,
		NowMs:  nowMs,
		Budget: e.budgetSnapshot(),
	}

	if err := e.processDueEvents(nowMs, &report); err != nil {
		return err
	}
	e.processTimeouts(nowMs)
	e.expireDeferred(nowMs, round, &report)

	candidates, suppressed := e.collectCandidates(nowMs)
	report.Suppressed = append(report.Suppressed, suppressed...)
	report.Eligible = candidateIDs(candidates)
	sort.Strings(report.Eligible)

	e.allocate(candidates, nowMs, round, &report)
	e.finalizeReport(&report)
	e.reports = append(e.reports, report)
	return nil
}

func (e *Engine) budgetSnapshot() BudgetSnapshot {
	snap := BudgetSnapshot{
		GlobalCapacity:  e.project.GlobalConcurrency,
		GlobalActive:    e.globalActive,
		GlobalAvailable: e.project.GlobalConcurrency - e.globalActive,
		RackCapacity:    map[string]int{},
		RackActive:      map[string]int{},
		RackAvailable:   map[string]int{},
	}
	for _, rack := range e.project.Racks {
		active := e.rackActive[rack.ID]
		snap.RackCapacity[rack.ID] = rack.Concurrency
		snap.RackActive[rack.ID] = active
		snap.RackAvailable[rack.ID] = rack.Concurrency - active
	}
	return snap
}

func candidateIDs(cs []candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.instanceID)
	}
	return out
}

func (e *Engine) finalizeReport(report *RoundReport) {
	sort.Slice(report.Suppressed, func(i, j int) bool {
		return report.Suppressed[i].InstanceID < report.Suppressed[j].InstanceID
	})
	if report.Rejected == nil {
		report.Rejected = []Suppression{}
	}
	if report.DeferredNow == nil {
		report.DeferredNow = []DeferredEntry{}
	}
	if report.Rescheduled == nil {
		report.Rescheduled = []RescheduledProbe{}
	}
	if report.Dropped == nil {
		report.Dropped = []DroppedProbe{}
	}
	report.Queue = append([]DeferredEntry(nil), e.queue...)
	report.Budget = e.budgetSnapshot()
}

// Result returns a detached result snapshot.
func (e *Engine) Result() *Result {
	out := &Result{
		ProjectID:   e.project.ProjectID,
		Version:     e.project.ProjectVersion,
		Fingerprint: e.fingerprint,
		Rounds:      append([]RoundReport(nil), e.reports...),
		HealthTrace: append([]HealthObservation(nil), e.trace...),
		FinalHealth: map[string]HealthState{},
		FinalCounts: map[string][2]int{},
	}
	for id, rt := range e.states {
		out.FinalHealth[id] = rt.Health
		out.FinalCounts[id] = [2]int{rt.SuccessStreak, rt.FailureStreak}
	}
	return out
}

func (e *Engine) recordObservation(obs HealthObservation) HealthObservation {
	e.obsSeq++
	if obs.ObservationID == "" {
		obs.ObservationID = fmt.Sprintf("OBS-%05d", e.obsSeq)
	}
	e.trace = append(e.trace, obs)
	return obs
}
