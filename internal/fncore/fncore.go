// Package fncore is the deterministic per-function simulation state machine.
// It contains no goroutines: both the single-threaded oracle and the
// parallel engine drive these cores, so any scheduling/worker difference can
// only affect timing observations, never the admission/cold-start verdicts.
package fncore

import (
	"coldkernel/internal/gen"
	"coldkernel/internal/model"
)

type active struct {
	reqID      string
	slots      int64
	priority   model.Priority
	releaseAt  int64
	instanceID string
}

type inflight struct {
	reqID     string
	readyAt   int64
	slots     int64
	priority  model.Priority
	holdTicks int64
}

type queued struct {
	req      *model.Request
	enqueued int64
	rank     int64
}

// ActiveState is a serialisable running-request record.
type ActiveState struct {
	ReqID      string
	Slots      int64
	Priority   model.Priority
	ReleaseAt  int64
	InstanceID string
	Cold       bool
}

// InflightState is a serialisable in-flight cold creation.
type InflightState struct {
	ReqID     string
	ReadyAt   int64
	Slots     int64
	Priority  model.Priority
	HoldTicks int64
}

// QueuedState is a serialisable queue entry (request stored by id).
type QueuedState struct {
	RequestID string
	Enqueued  int64
	Rank      int64
}

// State is the full serialisable per-function core state.
type State struct {
	Occupied       int64
	ReservedUsed   int64
	SharedUsed     int64
	WarmIdle       []string
	Active         []ActiveState
	Inflight       []InflightState
	Queue          []QueuedState
	ColdCount      int64
	ReuseCount     int64
	BudgetConsumed int64
}

// Save returns a deep, serialisable copy of the core state.
func (c *Core) Save() *State {
	st := &State{
		Occupied:       c.occupied,
		ReservedUsed:   c.reservedUsed,
		SharedUsed:     c.sharedUsed,
		WarmIdle:       append([]string(nil), c.warmIdle...),
		ColdCount:      c.ColdCount,
		ReuseCount:     c.ReuseCount,
		BudgetConsumed: c.BudgetConsumed,
	}
	for _, a := range c.active {
		st.Active = append(st.Active, ActiveState{
			ReqID: a.reqID, Slots: a.slots, Priority: a.priority,
			ReleaseAt: a.releaseAt, InstanceID: a.instanceID,
		})
	}
	for _, in := range c.inflight {
		st.Inflight = append(st.Inflight, InflightState{
			ReqID: in.reqID, ReadyAt: in.readyAt, Slots: in.slots,
			Priority: in.priority, HoldTicks: in.holdTicks,
		})
	}
	for _, q := range c.queue {
		st.Queue = append(st.Queue, QueuedState{
			RequestID: q.req.ID, Enqueued: q.enqueued, Rank: q.rank,
		})
	}
	return st
}

// Load restores a previously saved state. Request references are resolved
// through reqByID so queue entries keep their full model.Request.
func (c *Core) Load(st *State) {
	c.occupied = st.Occupied
	c.reservedUsed = st.ReservedUsed
	c.sharedUsed = st.SharedUsed
	c.warmIdle = append([]string(nil), st.WarmIdle...)
	c.ColdCount = st.ColdCount
	c.ReuseCount = st.ReuseCount
	c.BudgetConsumed = st.BudgetConsumed
	c.active = c.active[:0]
	for _, a := range st.Active {
		c.active = append(c.active, &active{
			reqID: a.ReqID, slots: a.Slots, priority: a.Priority,
			releaseAt: a.ReleaseAt, instanceID: a.InstanceID,
		})
	}
	c.inflight = c.inflight[:0]
	for _, in := range st.Inflight {
		c.inflight = append(c.inflight, &inflight{
			reqID: in.ReqID, readyAt: in.ReadyAt, slots: in.Slots,
			priority: in.Priority, holdTicks: in.HoldTicks,
		})
	}
}

// LoadQueue restores queue entries using a request lookup.
func (c *Core) LoadQueue(st *State, reqByID map[string]*model.Request) {
	c.queue = c.queue[:0]
	for _, q := range st.Queue {
		c.queue = append(c.queue, &queued{
			req: reqByID[q.RequestID], enqueued: q.Enqueued, rank: q.Rank,
		})
	}
}

// Core holds one function's full mutable simulation state.
type Core struct {
	Fn *model.Function

	occupied     int64
	reservedUsed int64
	sharedUsed   int64
	warmIdle     []string
	inflight     []*inflight
	queue        []*queued
	active       []*active

	ColdCount      int64
	ReuseCount     int64
	BudgetConsumed int64

	rng *gen.RNG
}

// New creates a core seeded independently per function so parallel draws stay
// deterministic regardless of scheduling.
func New(fn *model.Function, seed uint64) *Core {
	c := &Core{Fn: fn, rng: gen.NewRNG(seed ^ gen.HashString(fn.ID))}
	for i := int64(0); i < fn.WarmPoolSize; i++ {
		c.warmIdle = append(c.warmIdle, gen.WarmInstanceID(fn.ID, int(i)))
	}
	return c
}

// CanAdmit reports whether the request fits total, class and reserved rules.
func (c *Core) CanAdmit(r *model.Request) bool {
	fn := c.Fn
	if c.occupied+r.Slots > fn.ConcurrencyLimit {
		return false
}
	if r.Priority == model.PriorityReserved {
		return c.reservedUsed+r.Slots <= fn.Reserved
	}
	return c.sharedUsed+r.Slots <= fn.SharedLimit()
}

func (c *Core) budgetAllows() bool {
	if len(c.warmIdle) > 0 {
		return true
	}
	return c.BudgetConsumed < c.Fn.ColdBudgetAbsolute
}

// ViolatedRule returns the constraint id that blocks admission.
func (c *Core) ViolatedRule(r *model.Request) model.RejectReason {
	fn := c.Fn
	if c.occupied+r.Slots > fn.ConcurrencyLimit {
		return model.ReasonTotalFull
	}
	if r.Priority == model.PriorityReserved {
		return model.ReasonReservedFull
	}
	return model.ReasonSharedFull
}

// BeginTick advances lifecycle events that resolve at the start of the tick:
// cold creations completing and held slots being released. The returned
// outcomes are completion bookkeeping (not new gate verdicts).
func (c *Core) BeginTick(tick int64) {
	kept := c.inflight[:0]
	for _, in := range c.inflight {
		if in.readyAt <= tick {
			c.active = append(c.active, &active{
				reqID:      in.reqID,
				slots:      in.slots,
				priority:   in.priority,
				releaseAt:  in.readyAt + in.holdTicks,
				instanceID: "cold-" + in.reqID,
			})
		} else {
			kept = append(kept, in)
		}
	}
	c.inflight = kept

	keptA := c.active[:0]
	for _, a := range c.active {
		if a.releaseAt <= tick {
			c.occupied -= a.slots
			if a.priority == model.PriorityReserved {
				c.reservedUsed -= a.slots
			} else {
				c.sharedUsed -= a.slots
			}
			c.warmIdle = append(c.warmIdle, a.instanceID)
		} else {
			keptA = append(keptA, a)
		}
}
	c.active = keptA
}

// ExpireQueue rejects queued requests past the wait limit.
func (c *Core) ExpireQueue(tick int64) []*model.RequestOutcome {
	var rejected []*model.RequestOutcome
	var kept []*queued
	for _, q := range c.queue {
		if tick-q.enqueued >= c.Fn.WaitLimitTicks {
			rejected = append(rejected, reject(q.req, tick, model.ReasonWaitTimeout))
			continue
		}
		kept = append(kept, q)
	}
	c.queue = kept
	return rejected
}

// DrainQueue promotes queued requests FIFO until the first non-fitting one.
func (c *Core) DrainQueue(tick int64) (admitted, rejected []*model.RequestOutcome) {
	var kept []*queued
	blocked := false
	for _, q := range c.queue {
		if !blocked && c.CanAdmit(q.req) && c.budgetAllows() {
			oc := c.admit(q.req, tick)
			if oc.Decision == model.DecisionAdmit {
				admitted = append(admitted, oc)
			} else {
				rejected = append(rejected, oc)
			}
		} else {
			blocked = true
			q.rank = int64(len(kept) + 1)
			kept = append(kept, q)
		}
	}
	c.queue = kept
	return admitted, rejected
}

// HandleArrival applies the gate to one new request: admit, queue or reject.
func (c *Core) HandleArrival(r *model.Request, tick int64) *model.RequestOutcome {
	if c.CanAdmit(r) {
		if !c.budgetAllows() {
			return reject(r, tick, model.ReasonBudgetExhausted)
		}
		return c.admit(r, tick)
	}
	rule := c.ViolatedRule(r)
	if int64(len(c.queue)) >= c.Fn.QueueLimit {
		return reject(r, tick, model.ReasonQueueFull)
	}
	rank := int64(len(c.queue) + 1)
	c.queue = append(c.queue, &queued{req: r, enqueued: tick, rank: rank})
	return &model.RequestOutcome{
		RequestID:    r.ID,
		FunctionID:   r.FunctionID,
		Tick:         tick,
		Decision:     model.DecisionQueue,
		ViolatedRule: rule,
		QueueRank:    rank,
	}
}

func (c *Core) admit(r *model.Request, tick int64) *model.RequestOutcome {
	oc := &model.RequestOutcome{
		RequestID:  r.ID,
		FunctionID: r.FunctionID,
		Tick:       tick,
		Decision:   model.DecisionAdmit,
	}
	c.occupied += r.Slots
	if r.Priority == model.PriorityReserved {
		c.reservedUsed += r.Slots
	} else {
		c.sharedUsed += r.Slots
	}

	if len(c.warmIdle) > 0 {
		id := c.warmIdle[len(c.warmIdle)-1]
		c.warmIdle = c.warmIdle[:len(c.warmIdle)-1]
		oc.ReusedWarm = true
		c.ReuseCount++
		c.active = append(c.active, &active{
			reqID:      r.ID,
			slots:      r.Slots,
			priority:   r.Priority,
			releaseAt:  tick + r.HoldTicks,
			instanceID: id,
		})
		oc.CompleteTick = tick + r.HoldTicks
		return oc
	}

	dur := c.rng.Range(c.Fn.ColdMinTicks, c.Fn.ColdMaxTicks)
	oc.ColdStarted = true
	oc.ColdDoneAt = tick + dur
	c.ColdCount++
	c.BudgetConsumed++
	if dur > c.Fn.ColdTimeoutTicks {
		oc.Decision = model.DecisionReject
		oc.ViolatedRule = model.ReasonColdTimeout
		c.occupied -= r.Slots
		if r.Priority == model.PriorityReserved {
			c.reservedUsed -= r.Slots
		} else {
			c.sharedUsed -= r.Slots
		}
		c.ColdCount--
		c.BudgetConsumed--
		return oc
	}
	c.inflight = append(c.inflight, &inflight{
		reqID:     r.ID,
		readyAt:   tick + dur,
		slots:     r.Slots,
		priority:  r.Priority,
		holdTicks: r.HoldTicks,
	})
	return oc
}

func reject(r *model.Request, tick int64, rule model.RejectReason) *model.RequestOutcome {
	return &model.RequestOutcome{
		RequestID:    r.ID,
		FunctionID:   r.FunctionID,
		Tick:         tick,
		Decision:     model.DecisionReject,
		ViolatedRule: rule,
	}
}

// QueueLen exposes the current queue depth for backpressure checks.
func (c *Core) QueueLen() int64 { return int64(len(c.queue)) }

// Snapshot returns the observable per-function state at a tick.
func (c *Core) Snapshot(tick int64) *model.FunctionTickState {
	return &model.FunctionTickState{
		FunctionID:      c.Fn.ID,
		Tick:            tick,
		Occupied:        c.occupied,
		ReservedUsed:    c.reservedUsed,
		SharedUsed:      c.sharedUsed,
		WarmIdle:        int64(len(c.warmIdle)),
		InFlightCold:    int64(len(c.inflight)),
		Available:       int64(len(c.warmIdle)),
		QueueLen:        int64(len(c.queue)),
		ColdCount:       c.ColdCount,
		ReuseCount:      c.ReuseCount,
		BudgetConsumed:  c.BudgetConsumed,
		BudgetRemaining: c.Fn.ColdBudgetAbsolute - c.BudgetConsumed,
	}
}
