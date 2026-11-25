// Package model defines the version-independent internal representation that
// every schema version is mapped onto. All durations are integer logical
// ticks; all concurrency figures are absolute integer slots.
package model

// Priority distinguishes requests that must land inside reserved concurrency
// from requests that may consume shared concurrency.
type Priority string

const (
	PriorityReserved Priority = "RESERVED"
	PriorityShared   Priority = "SHARED"
)

// Function is the unified function declaration.
type Function struct {
	ID                 string
	ConcurrencyLimit   int64 // absolute total slot count
	Reserved           int64 // slots reserved for RESERVED requests
	WarmPoolSize       int64 // pre-warmed idle instances at tick 0
	ColdMinTicks       int64 // cold-start duration distribution bounds (ticks)
	ColdMaxTicks       int64
	ColdTimeoutTicks   int64 // creation past this limit fails
	ColdBudgetRatio    int64 // canonical ‱ fraction of horizon requests
	ColdBudgetAbsolute int64 // resolved maximum number of cold starts
	QueueLimit         int64 // max queued requests per function
	WaitLimitTicks     int64 // max time a request may sit in the queue
}

// SharedLimit is the slot count usable by SHARED requests.
func (f *Function) SharedLimit() int64 { return f.ConcurrencyLimit - f.Reserved }

// Request is the unified arrival record.
type Request struct {
	ID         string
	FunctionID string
	ArriveTick int64
	Slots      int64    // expected concurrency slots (>=1)
	Priority   Priority
	Tenant     string
	HoldTicks  int64 // slots held after admission before release
	Seq        int // stable ordering index within a tick
}

// Project is a fully loaded function engineering project.
type Project struct {
	SchemaVersion string
	SourceVersion string // original version tag for checkpoint matching
	TickMs        int64  // duration of one logical tick in milliseconds
	HorizonTicks  int64  // exclusive upper bound of simulated ticks
	Functions     []*Function
	Requests      []*Request
	Seed          uint64

	// ConversionWidth is the declared integer width every conversion uses.
	ConversionWidth int
	// FixedTolerance is the declared per-case fixed-point tolerance.
	FixedTolerance int64

	byFunc map[string]*Function
}

// FunctionByID looks up a function declaration.
func (p *Project) FunctionByID(id string) (*Function, bool) {
	f, ok := p.byFunc[id]
	return f, ok
}

// Index builds lookup tables. Loaders call it after mapping.
func (p *Project) Index() {
	p.byFunc = make(map[string]*Function, len(p.Functions))
	for _, f := range p.Functions {
		p.byFunc[f.ID] = f
	}
}

// Decision is the per-request gate verdict.
type Decision string

const (
	DecisionAdmit    Decision = "ADMIT"
	DecisionQueue    Decision = "QUEUE"
	DecisionReject   Decision = "REJECT"
)

// RejectReason is the stable code explaining a rejection.
type RejectReason string

const (
	ReasonNone            RejectReason = ""
	ReasonReservedFull    RejectReason = "C1_RESERVED_EXHAUSTED"
	ReasonSharedFull      RejectReason = "C2_SHARED_EXHAUSTED"
	ReasonTotalFull       RejectReason = "C3_TOTAL_EXHAUSTED"
	ReasonQueueFull       RejectReason = "E_QUEUE_FULL"
	ReasonWaitTimeout     RejectReason = "E_WAIT_TIMEOUT"
	ReasonBudgetExhausted RejectReason = "E_BUDGET_EXHAUSTED"
	ReasonColdTimeout     RejectReason = "E_COLD_TIMEOUT"
)

// RequestOutcome records the verdict reached for one request at one tick.
type RequestOutcome struct {
	RequestID    string
	FunctionID   string
	Tick         int64
	Decision     Decision
	ViolatedRule RejectReason
	QueueRank    int64 // 1-based position when QUEUE, else 0
	// Instance handling at admission.
	ReusedWarm  bool
	ColdStarted bool
	ColdDoneAt  int64 // tick at which the cold creation becomes available
	CompleteTick int64 // tick at which the request releases its slots
}

// FunctionTickState is the observable per-function state at a tick.
type FunctionTickState struct {
	FunctionID      string
	Tick            int64
	Occupied        int64
	ReservedUsed    int64
	SharedUsed      int64
	WarmIdle        int64 // idle warm instances available for immediate reuse
	InFlightCold    int64 // creations not yet complete
	Available       int64 // warm idle + in-flight-independent ready instances
	QueueLen        int64
	ColdCount       int64 // cumulative cold starts
	ReuseCount      int64 // cumulative warm reuses
	BudgetConsumed  int64 // cumulative cold starts charged to budget
	BudgetRemaining int64
}

// TickResult is the complete truth for one logical tick.
type TickResult struct {
	Tick      int64
	Admitted  []*RequestOutcome
	Queued    []*RequestOutcome
	Rejected  []*RequestOutcome
	Functions map[string]*FunctionTickState
}

// RunResult is the ordered per-tick output of a full simulation.
type RunResult struct {
	Ticks       []*TickResult
	ByRequest   map[string]*RequestOutcome
	ConvError   int64 // accumulated fixed-point conversion error
	ToleranceOK bool
}
