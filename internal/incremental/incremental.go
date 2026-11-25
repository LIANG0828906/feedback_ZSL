// Package incremental recomputes only the (function, tick) region whose
// dependency chain actually reaches an edit. Warm instances and in-flight
// cold starts carry state across ticks, so invalidation propagates forward
// through an explicit closure instead of naively restarting at tick zero.
package incremental

import (
	"sort"

	"coldkernel/internal/fixedpoint"
	"coldkernel/internal/fncore"
	"coldkernel/internal/model"
)

// Kind enumerates supported edits.
type Kind string

const (
	EditLimit        Kind = "limit"
	EditReserved     Kind = "reserved"
	EditWarmPool     Kind = "warm_pool"
	EditBudget       Kind = "budget"
	EditRequestAdd   Kind = "request_add"
	EditRequestDel   Kind = "request_del"
)

// Edit is one logical change applied at a tick. Function-level edits take
// effect at FromTick; request edits name the affected request.
type Edit struct {
	Kind       Kind
	FunctionID string
	FromTick   int64
	RequestID  string
	Request    *model.Request // for add
}

// Cell is the cached per-function/per-tick computation. CoreState captures
// the core at the END of the tick so an affected function can resume there.
type Cell struct {
	CoreState *fncore.State
	Snapshot *model.FunctionTickState
	// OutcomeIDs stores request verdicts produced in this cell.
	Outcomes []*model.RequestOutcome
}

// Cache maps "function/tick" to its computed cell.
type Cache map[string]*Cell

func key(fnID string, tick int64) string {
	return fnID + "/" + itoa(tick)
}

// Closure returns the explicit set of "function/tick" cells that must be
// recomputed because an edit (or the cross-tick state it changes) reaches
// them. Each affected function is invalidated from its earliest edit tick
// to the horizon, which captures warm-pool and in-flight propagation.
func Closure(p *model.Project, edits []Edit) map[string]bool {
	first := map[string]int64{}
	for _, e := range edits {
		from := e.FromTick
		if e.Kind == EditWarmPool || e.Kind == EditBudget {
			// Initial pool membership and the function-wide budget ratio are
			// established at tick 0, so their closure restarts there.
			from = 0
		}
		t, ok := first[e.FunctionID]
		if !ok || from < t {
			first[e.FunctionID] = from
		}
	}
	out := map[string]bool{}
	for fnID, from := range first {
		for tick := from; tick < p.HorizonTicks; tick++ {
			out[key(fnID, tick)] = true
		}
	}
	return out
}

// AffectedFunctions returns the sorted function ids touched by the closure.
func AffectedFunctions(closure map[string]bool) []string {
	seen := map[string]bool{}
	var ids []string
	for k := range closure {
		id := k[:idx(k)]
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
}
	sort.Strings(ids)
	return ids
}

func idx(k string) int {
	for i := 0; i < len(k); i++ {
		if k[i] == '/' {
			return i
		}
	}
	return len(k)
}

// Engine performs cached, edit-aware recomputation. It mirrors the oracle's
// per-tick ordering but preserves cores for unaffected functions/ticks.
type Engine struct {
	project *model.Project
	cores   map[string]*fncore.Core
	cache   Cache
	arrival map[int64]map[string][]*model.Request
	reqByID map[string]*model.Request
}

// NewEngine builds an incremental engine over the project.
func NewEngine(p *model.Project) *Engine {
	e := &Engine{
		project: p,
		cores:   map[string]*fncore.Core{},
		cache:   Cache{},
		arrival: map[int64]map[string][]*model.Request{},
	}
	for _, fn := range p.Functions {
		e.cores[fn.ID] = fncore.New(fn, p.Seed)
	}
	for _, r := range p.Requests {
		e.addArrival(r)
	}
	e.reqByID = map[string]*model.Request{}
	for _, r := range p.Requests {
		e.reqByID[r.ID] = r
	}
	return e
}

func (e *Engine) addArrival(r *model.Request) {
	if e.arrival[r.ArriveTick] == nil {
		e.arrival[r.ArriveTick] = map[string][]*model.Request{}
	}
	e.arrival[r.ArriveTick][r.FunctionID] =
		append(e.arrival[r.ArriveTick][r.FunctionID], r)
}

// ApplyLimit changes a function's absolute concurrency limit at fromTick.
// Only that function's closure from fromTick is invalidated.
func (e *Engine) ApplyLimit(fnID string, limit int64) {
	if fn, ok := e.project.FunctionByID(fnID); ok {
		fn.ConcurrencyLimit = limit
	}
}

// ApplyReserved changes a function's reserved concurrency.
func (e *Engine) ApplyReserved(fnID string, reserved int64) {
	if fn, ok := e.project.FunctionByID(fnID); ok {
		fn.Reserved = reserved
	}
}

// ApplyWarmPool changes the initial warm pool size. Because pool members
// propagate across ticks, the closure restarts at the edit boundary.
func (e *Engine) ApplyWarmPool(fnID string, size int64) {
	if fn, ok := e.project.FunctionByID(fnID); ok {
		fn.WarmPoolSize = size
	}
}

// ApplyBudget changes the canonical ‱ cold budget.
func (e *Engine) ApplyBudget(fnID string, ratio int64) {
	if fn, ok := e.project.FunctionByID(fnID); ok {
		fn.ColdBudgetRatio = ratio
		reqCount := int64(len(e.project.Requests))
		mul := fixedpoint.Mul(e.project.ConversionWidth, fn.ColdBudgetRatio, reqCount)
		div := fixedpoint.Div(e.project.ConversionWidth, mul.Value, fixedpoint.ScalePerTenThou)
		fn.ColdBudgetAbsolute = div.Value
	}
}

// AddRequest inserts a request and rebuilds the arrival index.
func (e *Engine) AddRequest(r *model.Request) {
	e.project.Requests = append(e.project.Requests, r)
	e.reqByID[r.ID] = r
	e.addArrival(r)
}

// DeleteRequest removes a request by id and rebuilds the arrival index.
func (e *Engine) DeleteRequest(id string) {
	for i, r := range e.project.Requests {
		if r.ID == id {
			e.project.Requests = append(e.project.Requests[:i], e.project.Requests[i+1:]...)
			break
		}
	}
	delete(e.reqByID, id)
	e.rebuildArrivals()
}

func (e *Engine) rebuildArrivals() {
	e.arrival = map[int64]map[string][]*model.Request{}
	for _, r := range e.project.Requests {
		e.addArrival(r)
	}
}

// ReusedCount reports how many cells were served from cache during the last
// Run, proving unaffected intervals were not recomputed.
type Stats struct {
	Recomputed int
	Reused     int
}

// Baseline computes and caches the unedited run. It must be called once
// before applying edits so unaffected cells can be reused.
func (e *Engine) Baseline() (*model.RunResult, error) {
	res, _, err := e.runWithClosure(nil, true)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Run recomputes the closure while reusing every other cached cell. The
// returned RunResult is built from the union of reused and fresh cells.
func (e *Engine) Run(edits []Edit) (*model.RunResult, Stats, error) {
	closure := Closure(e.project, edits)

	// At each function's earliest invalidated tick, restore the core state
	// saved at the end of the previous tick (its unaffected prefix). This is
	// what makes warm-pool and in-flight cross-tick state resume correctly
	// without replaying ticks that the edit cannot reach.
	first := map[string]int64{}
	for _, fnID := range AffectedFunctions(closure) {
		for tick := int64(0); tick < e.project.HorizonTicks; tick++ {
			if closure[key(fnID, tick)] {
				first[fnID] = tick
				break
			}
		}
	}
	for fnID, from := range first {
		fn, _ := e.project.FunctionByID(fnID)
		c := fncore.New(fn, e.project.Seed)
		if from > 0 {
			if prev, ok := e.cache[key(fnID, from-1)]; ok && prev.CoreState != nil {
				c.Load(prev.CoreState)
				c.LoadQueue(prev.CoreState, e.reqByID)
			}
		}
		e.cores[fnID] = c
		for tick := from; tick < e.project.HorizonTicks; tick++ {
			delete(e.cache, key(fnID, tick))
		}
	}

	res, stats, err := e.runWithClosure(closure, false)
	return res, stats, err
}

func (e *Engine) runWithClosure(closure map[string]bool, baseline bool) (*model.RunResult, Stats, error) {
	stats := Stats{}
	if baseline {
		for _, fn := range e.project.Functions {
			e.cores[fn.ID] = fncore.New(fn, e.project.Seed)
		}
		e.cache = Cache{}
	}
	out := &model.RunResult{ByRequest: map[string]*model.RequestOutcome{}, ToleranceOK: true}
	for tick := int64(0); tick < e.project.HorizonTicks; tick++ {
		tr := &model.TickResult{Tick: tick, Functions: map[string]*model.FunctionTickState{}}
		for _, fn := range e.project.Functions {
			cellKey := key(fn.ID, tick)
			if closure != nil && !closure[cellKey] {
				if cell, ok := e.cache[cellKey]; ok {
					stats.Reused++
					attach(tr, cell)
					continue
				}
			}
			stats.Recomputed++
			c := e.cores[fn.ID]
			c.BeginTick(tick)
			cell := &Cell{}
			cell.Outcomes = append(cell.Outcomes, c.ExpireQueue(tick)...)
			adm, rej := c.DrainQueue(tick)
			cell.Outcomes = append(cell.Outcomes, adm...)
			cell.Outcomes = append(cell.Outcomes, rej...)
			for _, r := range e.arrival[tick][fn.ID] {
				cell.Outcomes = append(cell.Outcomes, c.HandleArrival(r, tick))
			}
			cell.Snapshot = c.Snapshot(tick)
			cell.CoreState = c.Save()
			e.cache[cellKey] = cell
			attach(tr, cell)
		}
		sortTick(tr)
		for _, oc := range all(tr) {
			out.ByRequest[oc.RequestID] = oc
		}
		out.Ticks = append(out.Ticks, tr)
	}
	return out, stats, nil
}

func attach(tr *model.TickResult, cell *Cell) {
	for _, oc := range cell.Outcomes {
		switch oc.Decision {
		case model.DecisionAdmit:
			tr.Admitted = append(tr.Admitted, oc)
		case model.DecisionQueue:
			tr.Queued = append(tr.Queued, oc)
		default:
			tr.Rejected = append(tr.Rejected, oc)
		}
	}
	if cell.Snapshot != nil {
		tr.Functions[cell.Snapshot.FunctionID] = cell.Snapshot
	}
}

func all(tr *model.TickResult) []*model.RequestOutcome {
	var o []*model.RequestOutcome
	o = append(o, tr.Admitted...)
	o = append(o, tr.Queued...)
	o = append(o, tr.Rejected...)
	return o
}

func sortTick(tr *model.TickResult) {
	less := func(o []*model.RequestOutcome) {
		sort.SliceStable(o, func(i, j int) bool { return o[i].RequestID < o[j].RequestID })
	}
	less(tr.Admitted)
	less(tr.Queued)
	less(tr.Rejected)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
