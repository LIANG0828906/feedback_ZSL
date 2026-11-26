package locklab

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

const (
	OutcomeExecuted         = "executed"
	OutcomeBlocked          = "blocked"
	OutcomeConflictRejected = "conflict_rejected"
	OutcomeDeferred         = "deferred"
	OutcomeSkipped          = "skipped"
	OutcomeCommitted        = "committed"
	OutcomeRolledBack       = "rolled_back"

	LockGranted  = "granted"
	LockBlocked  = "blocked"
	LockRejected = "rejected"
	LockNone     = "none"

	WaitGranted    = "granted"
	WaitTimedOut   = "timed_out"
	WaitDeadlocked = "deadlocked"
	WaitCancelled  = "cancelled"
)

type StepObservation struct {
	GlobalStep    int    `json:"global_step"`
	ExecutedStep  int    `json:"executed_step"`
	TxID          string `json:"tx_id"`
	LocalSeq      int    `json:"local_seq"`
	Kind          OpKind `json:"kind"`
	ClockMs       int64  `json:"clock_ms"`
	ElapsedMicros int64  `json:"elapsed_micros"`
	Outcome       string `json:"outcome"`
}

type CommitObserver func(state *RuntimeState, commits []CommitEvent) error

type RunOptions struct {
	Context          context.Context
	ObserveStep      func(StepObservation)
	OnCommit         CommitObserver
	StopAfterCommits int
	AfterStep        func(step int, state *RuntimeState) (stop bool, err error)
}

type WaitMember struct {
	Seat       int      `json:"seat"`
	WaitID     string   `json:"wait_id"`
	TxID       string   `json:"tx_id"`
	Step       int      `json:"step"`
	Mode       LockMode `json:"mode"`
	DeadlineMs int64    `json:"deadline_ms"`
}

type WaitQueueSnapshot struct {
	Resource string       `json:"resource"`
	Members  []WaitMember `json:"members"`
}

type QueueEvent struct {
	AfterStep int                 `json:"after_step"`
	AtMs      int64               `json:"at_ms"`
	Event     string              `json:"event"`
	TxID      string              `json:"tx_id,omitempty"`
	Resource  string              `json:"resource,omitempty"`
	Seat      int                 `json:"seat,omitempty"`
	Queues    []WaitQueueSnapshot `json:"queues"`
}

type WaitRequest struct {
	ID           string      `json:"id"`
	TxID         string      `json:"tx_id"`
	Step         int         `json:"step"`
	Resource     string      `json:"resource"`
	Granularity  Granularity `json:"granularity"`
	Table        string      `json:"table"`
	ItemID       string      `json:"item_id,omitempty"`
	Mode         LockMode    `json:"mode"`
	Kind         OpKind      `json:"kind"`
	DeadlineMs   int64       `json:"deadline_ms"`
	EnqueuedAtMs int64       `json:"enqueued_at_ms"`
}

type ReadObservation struct {
	TxID          string         `json:"tx_id"`
	ItemID        string         `json:"item_id"`
	GlobalStep    int            `json:"global_step"`
	ExecutedStep  int            `json:"executed_step"`
	AtMs          int64          `json:"at_ms"`
	Isolation     IsolationLevel `json:"isolation"`
	SnapshotSeq   uint64         `json:"snapshot_seq"`
	VersionNumber int            `json:"version_number"`
	Value         string         `json:"value"`
	WriterTx      string         `json:"writer_tx"`
	CommitSeq     uint64         `json:"commit_seq"`
	Dirty         bool           `json:"dirty"`
}

type WriteObservation struct {
	TxID         string `json:"tx_id"`
	ItemID       string `json:"item_id"`
	GlobalStep   int    `json:"global_step"`
	ExecutedStep int    `json:"executed_step"`
	AtMs         int64  `json:"at_ms"`
	Value        string `json:"value"`
	WriteSeq     uint64 `json:"write_seq"`
}

type WriteRecord struct {
	ItemID       string `json:"item_id"`
	Value        string `json:"value"`
	WriterTx     string `json:"writer_tx"`
	GlobalStep   int    `json:"global_step"`
	ExecutedStep int    `json:"executed_step"`
	AtMs         int64  `json:"at_ms"`
	WriteSeq     uint64 `json:"write_seq"`
}

type VersionRecord struct {
	ItemID      string `json:"item_id"`
	Number      int    `json:"number"`
	Value       string `json:"value"`
	WriterTx    string `json:"writer_tx"`
	CommitSeq   uint64 `json:"commit_seq"`
	WriteStep   int    `json:"write_step"`
	CreatedAtMs int64  `json:"created_at_ms"`
	Initial     bool   `json:"initial,omitempty"`
}

type ReadFact struct {
	TxID          string         `json:"tx_id"`
	ItemID        string         `json:"item_id"`
	Value         string         `json:"value"`
	ReaderStep    int            `json:"reader_step"`
	ReaderAtMs    int64          `json:"reader_at_ms"`
	VersionNumber int            `json:"version_number"`
	WriterTx      string         `json:"writer_tx"`
	CommitSeq     uint64         `json:"commit_seq"`
	SnapshotSeq   uint64         `json:"snapshot_seq"`
	Isolation     IsolationLevel `json:"isolation"`
	Dirty         bool           `json:"dirty"`
}

type CommitEvent struct {
	TxID          string        `json:"tx_id"`
	GlobalStep    int           `json:"global_step"`
	ExecutedStep  int           `json:"executed_step"`
	AtMs          int64         `json:"at_ms"`
	CommitSeq     uint64        `json:"commit_seq"`
	Writes        []WriteRecord `json:"writes"`
	ReleasedLocks []string      `json:"released_locks"`
}

type RollbackEvent struct {
	TxID          string   `json:"tx_id"`
	GlobalStep    int      `json:"global_step"`
	ExecutedStep  int      `json:"executed_step"`
	AtMs          int64    `json:"at_ms"`
	Reason        string   `json:"reason"`
	ReleasedLocks []string `json:"released_locks"`
	Cycle         []string `json:"cycle,omitempty"`
}

type StepResult struct {
	GlobalStep        int                 `json:"global_step"`
	ExecutedStep      int                 `json:"executed_step"`
	TxID              string              `json:"tx_id"`
	LocalSeq          int                 `json:"local_seq"`
	Kind              OpKind              `json:"kind"`
	ScheduledAtMs     int64               `json:"scheduled_at_ms"`
	ExecutedAtMs      int64               `json:"executed_at_ms"`
	Outcome           string              `json:"outcome"`
	LockDecision      string              `json:"lock_decision,omitempty"`
	Resource          string              `json:"resource,omitempty"`
	Granularity       Granularity         `json:"granularity,omitempty"`
	Mode              LockMode            `json:"mode,omitempty"`
	ConflictWith      []string            `json:"conflict_with,omitempty"`
	WaitID            string              `json:"wait_id,omitempty"`
	WaitSeat          int                 `json:"wait_seat,omitempty"`
	WaitDeadlineMs    int64               `json:"wait_deadline_ms,omitempty"`
	EventuallyGranted bool                `json:"eventually_granted,omitempty"`
	WaitOutcome       string              `json:"wait_outcome,omitempty"`
	RollbackReason    string              `json:"rollback_reason,omitempty"`
	CommitSeq         uint64              `json:"commit_seq,omitempty"`
	ReleasedLocks     []string            `json:"released_locks,omitempty"`
	Read              *ReadObservation    `json:"read,omitempty"`
	Write             *WriteObservation   `json:"write,omitempty"`
	WaitQueues        []WaitQueueSnapshot `json:"wait_queues,omitempty"`
}

type TxState struct {
	ID             string         `json:"id"`
	Priority       int            `json:"priority"`
	Isolation      IsolationLevel `json:"isolation"`
	Started        bool           `json:"started"`
	SnapshotSeq    uint64         `json:"snapshot_seq"`
	Done           bool           `json:"done"`
	RolledBack     bool           `json:"rolled_back"`
	RollbackReason string         `json:"rollback_reason,omitempty"`
	BlockedWaitID  string         `json:"blocked_wait_id,omitempty"`
	PendingWrites  []WriteRecord  `json:"pending_writes,omitempty"`
	DeferredSteps  []int          `json:"deferred_steps,omitempty"`
}

type WaitGraphEdge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Resource string `json:"resource"`
	Step     int    `json:"step"`
}

type Anomaly struct {
	Kind   string   `json:"kind"`
	TxIDs  []string `json:"tx_ids"`
	ItemID string   `json:"item_id,omitempty"`
	Steps  []int    `json:"steps,omitempty"`
	Detail string   `json:"detail"`
}

type RuntimeState struct {
	ProjectID       string                     `json:"project_id"`
	ProjectDigest   string                     `json:"project_digest"`
	ClockMs         int64                      `json:"clock_ms"`
	NextStep        int                        `json:"next_step"`
	CommitSeq       uint64                     `json:"commit_seq"`
	NextWaitSeq     int                        `json:"next_wait_seq"`
	NextWriteSeq    uint64                     `json:"next_write_seq"`
	Transactions    map[string]*TxState        `json:"transactions"`
	Locks           map[string]HeldLock        `json:"locks"`
	WaitQueues      map[string][]WaitRequest   `json:"wait_queues"`
	Versions        map[string][]VersionRecord `json:"versions"`
	ReadFacts       []ReadFact                 `json:"read_facts"`
	WriteFacts      []WriteRecord              `json:"write_facts"`
	Steps           []StepResult               `json:"steps"`
	QueueEvents     []QueueEvent               `json:"queue_events"`
	Commits         []CommitEvent              `json:"commits"`
	Rollbacks       []RollbackEvent            `json:"rollbacks"`
	Anomalies       []Anomaly                  `json:"anomalies"`
	DeadlockVictims []string                   `json:"deadlock_victims"`
}

type RunResult struct {
	ProjectID       string                   `json:"project_id"`
	ProjectDigest   string                   `json:"project_digest"`
	Seed            int64                    `json:"seed"`
	Steps           []StepResult             `json:"steps"`
	QueueEvents     []QueueEvent             `json:"queue_events"`
	Reads           []ReadObservation        `json:"reads"`
	Writes          []WriteObservation       `json:"writes"`
	Commits         []CommitEvent            `json:"commits"`
	Rollbacks       []RollbackEvent          `json:"rollbacks"`
	FinalWrites     map[string]VersionRecord `json:"final_writes"`
	Anomalies       []Anomaly                `json:"anomalies"`
	DeadlockVictims []string                 `json:"deadlock_victims"`
	FinalWaitGraph  []WaitGraphEdge          `json:"final_wait_graph"`
	FinalWaitQueues []WaitQueueSnapshot      `json:"final_wait_queues"`
	FinalState      *RuntimeState            `json:"-"`
	Partial         bool                     `json:"-"`
}

type engine struct {
	project        *Project
	schedule       []ScheduledOp
	state          RuntimeState
	options        RunOptions
	pendingCommits []CommitEvent
	waking         bool
	wakeRequested  bool
	stoppedEarly   bool
}

func Run(project *Project, options RunOptions) (*RunResult, error) {
	p := project.Clone()
	if p == nil {
		return nil, NewError(EFieldRequired, "$", "project is nil")
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	schedule := BuildSchedule(p)
	state := newRuntimeState(p, schedule)
	eng := &engine{project: p, schedule: schedule, state: state, options: options}
	return eng.run()
}

// ResumeRun continues a previously checkpointed deterministic execution.
func ResumeRun(project *Project, state *RuntimeState, options RunOptions) (*RunResult, error) {
	p := project.Clone()
	if p == nil {
		return nil, NewError(EFieldRequired, "$", "project is nil")
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	if state == nil {
		return nil, NewError(EFieldRequired, "$.state", "runtime state is nil")
	}
	if state.ProjectID != p.ID {
		return nil, NewError(EProjectVersion, "$.state.project_id",
			"checkpoint project id %q does not match project %q", state.ProjectID, p.ID)
	}
	if state.ProjectDigest != p.CanonicalDigest() {
		return nil, NewError(EProjectVersion, "$.state.project_digest",
			"checkpoint project digest does not match project snapshot")
	}
	schedule := BuildSchedule(p)
	if len(state.Steps) != len(schedule) {
		return nil, NewError(ECheckpointVersion, "$.state.steps",
			"checkpoint step count %d does not match schedule %d", len(state.Steps), len(schedule))
	}
	cloned, err := cloneRuntimeState(state)
	if err != nil {
		return nil, err
	}
	for i := cloned.NextStep; i < len(schedule); i++ {
		cloned.Steps[i] = StepResult{
			GlobalStep: schedule[i].GlobalStep,
			TxID:       schedule[i].TxID,
			LocalSeq:   schedule[i].LocalSeq,
		}
	}
	eng := &engine{project: p, schedule: schedule, state: *cloned, options: options}
	return eng.run()
}

func newRuntimeState(project *Project, schedule []ScheduledOp) RuntimeState {
	state := RuntimeState{
		ProjectID: project.ID, ProjectDigest: project.CanonicalDigest(),
		NextStep: 0, NextWaitSeq: 1, NextWriteSeq: 1,
		Transactions: make(map[string]*TxState),
		Locks:        make(map[string]HeldLock),
		WaitQueues:   make(map[string][]WaitRequest),
		Versions:     make(map[string][]VersionRecord),
		Steps:        make([]StepResult, len(schedule)),
	}
	for i, scheduled := range schedule {
		state.Steps[i].GlobalStep = scheduled.GlobalStep
		state.Steps[i].TxID = scheduled.TxID
		state.Steps[i].LocalSeq = scheduled.LocalSeq
	}
	for _, tx := range project.Transactions {
		state.Transactions[tx.ID] = &TxState{
			ID: tx.ID, Priority: tx.Priority, Isolation: tx.Isolation,
		}
	}
	for _, item := range project.Items {
		state.Versions[item.ID] = []VersionRecord{{
			ItemID: item.ID, Number: 0, Value: item.InitialValue,
			WriterTx: "__initial__", Initial: true,
		}}
	}
	return state
}

func cloneRuntimeState(state *RuntimeState) (*RuntimeState, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return nil, WrapError(EInternal, "$.state", err, "cannot clone runtime state")
	}
	var out RuntimeState
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, WrapError(EInternal, "$.state", err, "cannot clone runtime state")
	}
	return &out, nil
}

func (e *engine) run() (*RunResult, error) {
	total := len(e.schedule)
	for e.state.NextStep < total {
		if e.options.Context != nil {
			select {
			case <-e.options.Context.Done():
				e.stoppedEarly = true
				result := e.buildResult()
				return result, WrapError(EInterrupted, "$", e.options.Context.Err(),
					"execution interrupted at logical step %d", e.state.NextStep)
			default:
			}
		}
		started := time.Now()
		executed := e.processNextStep()
		elapsed := time.Since(started).Microseconds()
		if e.options.ObserveStep != nil {
			result := &e.state.Steps[executed-1]
			e.options.ObserveStep(StepObservation{
				GlobalStep: result.GlobalStep, ExecutedStep: result.ExecutedStep,
				TxID: result.TxID, LocalSeq: result.LocalSeq, Kind: result.Kind,
				ClockMs: e.state.ClockMs, ElapsedMicros: elapsed, Outcome: result.Outcome,
			})
		}
		if err := e.flushCommitObserver(); err != nil {
			return nil, err
		}
		if e.options.AfterStep != nil {
			clone, err := cloneRuntimeState(&e.state)
			if err != nil {
				return nil, err
			}
			stop, err := e.options.AfterStep(executed, clone)
			if err != nil {
				return nil, err
			}
			if stop {
				e.stoppedEarly = true
				return e.buildResult(), nil
			}
		}
		if e.options.StopAfterCommits > 0 && len(e.state.Commits) >= e.options.StopAfterCommits {
			e.stoppedEarly = true
			return e.buildResult(), nil
		}
	}

	// Resolve waits whose deadlines lie after the final declared operation.
	for e.hasActiveWaits() {
		deadline := e.earliestWaitDeadline()
		if deadline > e.state.ClockMs {
			e.state.ClockMs = deadline
		}
		before := e.waitCount()
		e.processDueTimers(total + 1)
		if e.waitCount() == before {
			// No timer was due and no progress can be made. Force deterministic
			// cancellation rather than silently dropping the wait.
			e.cancelAllWaits(total+1, "unresolvable_wait")
		}
	}
	if err := e.flushCommitObserver(); err != nil {
		return nil, err
	}
	result := e.buildResult()
	return result, nil
}

func (e *engine) processNextStep() int {
	index := e.state.NextStep
	e.state.NextStep++
	scheduled := e.schedule[index]
	tx := e.state.Transactions[scheduled.TxID]
	op := e.project.Transactions[scheduled.TxIndex].Ops[scheduled.OpIndex]
	result := &e.state.Steps[index]
	result.Kind = op.Kind
	result.ScheduledAtMs = op.AtMs
	result.ExecutedStep = scheduled.GlobalStep
	result.ExecutedAtMs = op.AtMs

	if op.AtMs > e.state.ClockMs {
		e.state.ClockMs = op.AtMs
	}
	e.processDueTimers(scheduled.GlobalStep)

	switch {
	case tx.Done:
		result.Outcome = OutcomeSkipped
	case tx.BlockedWaitID != "":
		tx.DeferredSteps = append(tx.DeferredSteps, scheduled.GlobalStep)
		result.Outcome = OutcomeDeferred
	default:
		e.ensureStarted(tx)
		e.executeOperation(tx, op, scheduled.GlobalStep, result)
	}
	e.processDueTimers(scheduled.GlobalStep)
	result.WaitQueues = e.snapshotWaitQueues()
	return scheduled.GlobalStep
}

func (e *engine) ensureStarted(tx *TxState) {
	if tx.Started {
		return
	}
	tx.Started = true
	tx.SnapshotSeq = e.state.CommitSeq
}

func (e *engine) executeOperation(tx *TxState, op Operation, step int, result *StepResult) {
	result.Kind = op.Kind
	result.ScheduledAtMs = op.AtMs
	result.ExecutedStep = step
	result.ExecutedAtMs = e.state.ClockMs
	switch op.Kind {
	case OpLock:
		e.attemptLock(tx, op, step, result, false)
	case OpRead:
		if op.Mode != "" {
			e.attemptLock(tx, op, step, result, true)
		} else {
			e.performRead(tx, op, step, result)
		}
	case OpWrite:
		if op.Mode != "" {
			e.attemptLock(tx, op, step, result, true)
		} else {
			e.performWrite(tx, op, step, result)
		}
	case OpCommit:
		e.commitTransaction(tx, step, result)
	case OpRollback:
		result.Outcome = OutcomeRolledBack
		result.RollbackReason = "explicit_rollback"
		e.rollbackTransaction(tx, step, "explicit_rollback", nil, false)
	}
}

func (e *engine) lockRequest(tx *TxState, op Operation, step int) HeldLock {
	table := op.Table
	if op.Granularity == RowGranularity {
		if item, ok := e.project.ItemByID(op.ItemID); ok {
			table = item.Table
		}
	}
	resource := ResourceKey(op.Granularity, table, op.ItemID)
	return HeldLock{
		TxID: tx.ID, Resource: resource, Granularity: op.Granularity,
		Table: table, ItemID: op.ItemID, Mode: op.Mode, AcquiredStep: step,
	}
}

func (e *engine) heldLock(txID, resource string) (string, HeldLock, bool) {
	key := txID + "|" + resource
	lock, ok := e.state.Locks[key]
	return key, lock, ok
}

func (e *engine) attemptLock(tx *TxState, op Operation, step int, result *StepResult, completeOp bool) {
	requested := e.lockRequest(tx, op, step)
	result.Resource = requested.Resource
	result.Granularity = requested.Granularity
	result.Mode = op.Mode
	key, held, heldOK := e.heldLock(tx.ID, requested.Resource)
	desired := requested
	if heldOK {
		if ModeSubsumes(held.Mode, requested.Mode) {
			result.LockDecision = LockGranted
			result.Outcome = OutcomeExecuted
			if completeOp {
				e.completeLockedOperation(tx, op, step, result, false)
			}
			return
		}
		desired.Mode = MergeModes(held.Mode, requested.Mode)
		desired.AcquiredStep = held.AcquiredStep
	}
	opponents := conflictingLocks(tx.ID, desired, e.state.Locks)
	if len(opponents) == 0 {
		transient := !heldOK && op.Kind == OpRead && tx.Isolation == ReadCommitted
		desired.Transient = transient
		e.state.Locks[key] = desired
		result.LockDecision = LockGranted
		result.Outcome = OutcomeExecuted
		if completeOp {
			e.completeLockedOperation(tx, op, step, result, transient)
		}
		return
	}
	result.ConflictWith = opponents
	if op.NoWait {
		result.LockDecision = LockRejected
		result.Outcome = OutcomeConflictRejected
		result.WaitOutcome = "conflict_rejected"
		return
	}

	wait := WaitRequest{
		ID: fmt.Sprintf("W%06d", e.state.NextWaitSeq), TxID: tx.ID, Step: step,
		Resource: requested.Resource, Granularity: requested.Granularity,
		Table: requested.Table, ItemID: requested.ItemID, Mode: desired.Mode,
		Kind: op.Kind, DeadlineMs: op.AtMs + op.WaitTimeoutMs, EnqueuedAtMs: e.state.ClockMs,
	}
	e.state.NextWaitSeq++
	queue := append(e.state.WaitQueues[requested.Resource], wait)
	e.state.WaitQueues[requested.Resource] = queue
	tx.BlockedWaitID = wait.ID
	result.LockDecision = LockBlocked
	result.Outcome = OutcomeBlocked
	result.WaitID = wait.ID
	result.WaitSeat = len(queue)
	result.WaitDeadlineMs = wait.DeadlineMs
	e.recordQueueEvent(step, "enqueue", tx.ID, requested.Resource, len(queue))
	if e.project.DeadlockDetection {
		e.resolveDeadlocks(step)
	}
	e.processDueTimers(step)
}

func (e *engine) completeLockedOperation(tx *TxState, op Operation, step int, result *StepResult, transient bool) {
	switch op.Kind {
	case OpRead:
		e.performRead(tx, op, step, result)
		if transient {
			e.releaseLock(tx.ID, e.lockRequest(tx, op, step).Resource)
			e.wakeAll(step)
		}
	case OpWrite:
		e.performWrite(tx, op, step, result)
	}
}

func (e *engine) grantWait(wait WaitRequest, step int) {
	tx := e.state.Transactions[wait.TxID]
	if tx == nil || tx.Done || tx.BlockedWaitID != wait.ID {
		return
	}
	resource := wait.Resource
	desired := HeldLock{
		TxID: tx.ID, Resource: resource, Granularity: wait.Granularity,
		Table: wait.Table, ItemID: wait.ItemID, Mode: wait.Mode, AcquiredStep: step,
	}
	key, held, heldOK := e.heldLock(tx.ID, resource)
	if heldOK {
		desired.Mode = MergeModes(held.Mode, wait.Mode)
		desired.AcquiredStep = held.AcquiredStep
	}
	op := e.operationForStep(wait.Step)
	transient := !heldOK && op.Kind == OpRead && tx.Isolation == ReadCommitted
	desired.Transient = transient
	e.state.Locks[key] = desired
	tx.BlockedWaitID = ""
	result := &e.state.Steps[wait.Step-1]
	result.LockDecision = LockGranted
	result.EventuallyGranted = true
	result.WaitOutcome = WaitGranted
	result.ExecutedStep = step
	result.ExecutedAtMs = e.state.ClockMs
	result.Outcome = OutcomeExecuted
	switch op.Kind {
	case OpRead:
		e.performRead(tx, op, step, result)
		if transient {
			e.releaseLock(tx.ID, resource)
		}
	case OpWrite:
		e.performWrite(tx, op, step, result)
	}
	result.WaitQueues = e.snapshotWaitQueues()
	e.recordQueueEvent(step, "grant", tx.ID, resource, 0)
	e.executeDeferred(tx, step)
}

func (e *engine) executeDeferred(tx *TxState, step int) {
	for len(tx.DeferredSteps) > 0 && !tx.Done && tx.BlockedWaitID == "" {
		globalStep := tx.DeferredSteps[0]
		tx.DeferredSteps = tx.DeferredSteps[1:]
		op := e.operationForStep(globalStep)
		result := &e.state.Steps[globalStep-1]
		result.Outcome = OutcomeExecuted
		result.ExecutedStep = step
		result.ExecutedAtMs = e.state.ClockMs
		e.executeOperation(tx, op, step, result)
		result.WaitQueues = e.snapshotWaitQueues()
	}
	if tx.Done {
		e.cancelDeferred(tx, step)
	}
}

func (e *engine) cancelDeferred(tx *TxState, step int) {
	for _, globalStep := range tx.DeferredSteps {
		result := &e.state.Steps[globalStep-1]
		result.Outcome = OutcomeSkipped
		result.ExecutedStep = step
		result.ExecutedAtMs = e.state.ClockMs
	}
	tx.DeferredSteps = nil
}

func (e *engine) operationForStep(globalStep int) Operation {
	scheduled := e.schedule[globalStep-1]
	return e.project.Transactions[scheduled.TxIndex].Ops[scheduled.OpIndex]
}

func (e *engine) releaseLock(txID, resource string) bool {
	key := txID + "|" + resource
	if _, ok := e.state.Locks[key]; !ok {
		return false
	}
	delete(e.state.Locks, key)
	return true
}

func (e *engine) releaseAllLocks(txID string) []string {
	keys := sortedLockKeys(e.state.Locks, txID)
	resources := make([]string, 0, len(keys))
	for _, key := range keys {
		lock := e.state.Locks[key]
		resources = append(resources, lock.Resource)
		delete(e.state.Locks, key)
	}
	sort.Strings(resources)
	return resources
}

func (e *engine) wakeAll(step int) {
	if e.waking {
		e.wakeRequested = true
		return
	}
	e.waking = true
	defer func() { e.waking = false }()
	for {
		e.wakeRequested = false
		if !e.wakePass(step) && !e.wakeRequested {
			return
		}
	}
}

func (e *engine) wakePass(step int) bool {
	resources := make([]string, 0, len(e.state.WaitQueues))
	for resource, queue := range e.state.WaitQueues {
		if len(queue) > 0 {
			resources = append(resources, resource)
		}
	}
	sort.Strings(resources)
	for _, resource := range resources {
		queue := e.state.WaitQueues[resource]
		if len(queue) == 0 {
			delete(e.state.WaitQueues, resource)
			continue
		}
		head := queue[0]
		tx := e.state.Transactions[head.TxID]
		if tx == nil || tx.Done {
			e.removeWaiter(resource, head.ID)
			return true
		}
		requested := HeldLock{
			TxID: head.TxID, Resource: resource, Granularity: head.Granularity,
			Table: head.Table, ItemID: head.ItemID, Mode: head.Mode, AcquiredStep: head.Step,
		}
		if key, held, ok := e.heldLock(tx.ID, resource); ok {
			requested.Mode = MergeModes(held.Mode, head.Mode)
			requested.AcquiredStep = held.AcquiredStep
			_ = key
		}
		if len(conflictingLocks(tx.ID, requested, e.state.Locks)) != 0 {
			continue
		}
		e.removeWaiter(resource, head.ID)
		e.grantWait(head, step)
		return true
	}
	return false
}

func (e *engine) removeWaiter(resource, waitID string) {
	queue := e.state.WaitQueues[resource]
	for i, wait := range queue {
		if wait.ID == waitID {
			queue = append(queue[:i], queue[i+1:]...)
			break
		}
	}
	if len(queue) == 0 {
		delete(e.state.WaitQueues, resource)
	} else {
		e.state.WaitQueues[resource] = queue
	}
}

func (e *engine) commitTransaction(tx *TxState, step int, result *StepResult) {
	e.state.CommitSeq++
	commitSeq := e.state.CommitSeq
	writes := append([]WriteRecord(nil), tx.PendingWrites...)
	for _, write := range writes {
		versions := e.state.Versions[write.ItemID]
		number := 0
		if len(versions) > 0 {
			number = versions[len(versions)-1].Number + 1
		}
		e.state.Versions[write.ItemID] = append(versions, VersionRecord{
			ItemID: write.ItemID, Number: number, Value: write.Value,
			WriterTx: tx.ID, CommitSeq: commitSeq,
			WriteStep: write.ExecutedStep, CreatedAtMs: e.state.ClockMs,
		})
		e.state.WriteFacts = append(e.state.WriteFacts, write)
	}
	tx.PendingWrites = nil
	tx.Done = true
	tx.RolledBack = false
	released := e.releaseAllLocks(tx.ID)
	result.Outcome = OutcomeCommitted
	result.CommitSeq = commitSeq
	result.ReleasedLocks = released
	commit := CommitEvent{
		TxID: tx.ID, GlobalStep: result.GlobalStep, ExecutedStep: step, AtMs: e.state.ClockMs,
		CommitSeq: commitSeq, Writes: writes, ReleasedLocks: released,
	}
	e.state.Commits = append(e.state.Commits, commit)
	e.pendingCommits = append(e.pendingCommits, commit)
	e.cancelDeferred(tx, step)
	e.wakeAll(step)
}

func (e *engine) rollbackTransaction(tx *TxState, step int, reason string, cycle []string, alreadyUnblocked bool) {
	if tx.Done {
		return
	}
	if !alreadyUnblocked && tx.BlockedWaitID != "" {
		e.cancelWait(tx.BlockedWaitID, WaitCancelled)
	}
	e.finishRollback(tx, step, reason, cycle)
	e.wakeAll(step)
}

func (e *engine) finishRollback(tx *TxState, step int, reason string, cycle []string) []string {
	tx.PendingWrites = nil
	tx.Done = true
	tx.RolledBack = true
	tx.RollbackReason = reason
	released := e.releaseAllLocks(tx.ID)
	event := RollbackEvent{
		TxID: tx.ID, GlobalStep: step, ExecutedStep: step, AtMs: e.state.ClockMs,
		Reason: reason, ReleasedLocks: released, Cycle: append([]string(nil), cycle...),
	}
	e.state.Rollbacks = append(e.state.Rollbacks, event)
	if reason == "deadlock" {
		e.state.DeadlockVictims = append(e.state.DeadlockVictims, tx.ID)
	}
	e.cancelDeferred(tx, step)
	return released
}

func (e *engine) cancelWait(waitID, outcome string) {
	for resource, queue := range e.state.WaitQueues {
		for i, wait := range queue {
			if wait.ID != waitID {
				continue
			}
			queue = append(queue[:i], queue[i+1:]...)
			if len(queue) == 0 {
				delete(e.state.WaitQueues, resource)
			} else {
				e.state.WaitQueues[resource] = queue
			}
			result := &e.state.Steps[wait.Step-1]
			result.WaitOutcome = outcome
			result.WaitQueues = e.snapshotWaitQueues()
			tx := e.state.Transactions[wait.TxID]
			if tx != nil && tx.BlockedWaitID == waitID {
				tx.BlockedWaitID = ""
			}
			return
		}
	}
}

func (e *engine) processDueTimers(step int) {
	type dueWait struct {
		resource string
		wait     WaitRequest
	}
	due := make([]dueWait, 0)
	for resource, queue := range e.state.WaitQueues {
		for _, wait := range queue {
			if wait.DeadlineMs <= e.state.ClockMs {
				due = append(due, dueWait{resource: resource, wait: wait})
			}
		}
	}
	if len(due) == 0 {
		return
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].wait.DeadlineMs != due[j].wait.DeadlineMs {
			return due[i].wait.DeadlineMs < due[j].wait.DeadlineMs
		}
		return due[i].wait.TxID < due[j].wait.TxID
	})
	for _, item := range due {
		tx := e.state.Transactions[item.wait.TxID]
		if tx == nil || tx.Done || tx.BlockedWaitID != item.wait.ID {
			continue
		}
		e.removeWaiter(item.resource, item.wait.ID)
		tx.BlockedWaitID = ""
		result := &e.state.Steps[item.wait.Step-1]
		result.WaitOutcome = WaitTimedOut
		result.RollbackReason = "wait_timeout"
		result.WaitQueues = e.snapshotWaitQueues()
		e.recordQueueEvent(step, "timeout", tx.ID, item.resource, 0)
		e.finishRollback(tx, step, "wait_timeout", nil)
	}
	e.wakeAll(step)
}

func (e *engine) resolveDeadlocks(step int) {
	for {
		cycle := e.findWaitCycle()
		if len(cycle) == 0 {
			return
		}
		victim := e.lowestPriority(cycle)
		tx := e.state.Transactions[victim]
		if tx == nil || tx.Done {
			return
		}
		if tx.BlockedWaitID != "" {
			e.cancelWait(tx.BlockedWaitID, WaitDeadlocked)
		}
		e.rollbackTransaction(tx, step, "deadlock", cycle, true)
	}
}

func (e *engine) lowestPriority(cycle []string) string {
	ordered := append([]string(nil), cycle...)
	sort.Strings(ordered)
	victim := ordered[0]
	for _, id := range ordered[1:] {
		tx, other := e.state.Transactions[victim], e.state.Transactions[id]
		if tx == nil {
			victim = id
			continue
		}
		if other == nil {
			continue
		}
		if other.Priority < tx.Priority ||
			(other.Priority == tx.Priority && other.ID > tx.ID) {
			victim = id
		}
	}
	return victim
}

func (e *engine) buildWaitGraph() []WaitGraphEdge {
	edges := make([]WaitGraphEdge, 0)
	resources := make([]string, 0, len(e.state.WaitQueues))
	for resource := range e.state.WaitQueues {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	for _, resource := range resources {
		queue := e.state.WaitQueues[resource]
		for index, wait := range queue {
			requested := HeldLock{
				TxID: wait.TxID, Resource: resource, Granularity: wait.Granularity,
				Table: wait.Table, ItemID: wait.ItemID, Mode: wait.Mode, AcquiredStep: wait.Step,
			}
			if _, held, ok := e.heldLock(wait.TxID, resource); ok {
				requested.Mode = MergeModes(held.Mode, wait.Mode)
			}
			for _, holder := range conflictingLocks(wait.TxID, requested, e.state.Locks) {
				edges = append(edges, WaitGraphEdge{
					From: wait.TxID, To: holder, Resource: resource, Step: wait.Step,
				})
			}
			for earlier := 0; earlier < index; earlier++ {
				if queue[earlier].TxID != wait.TxID {
					edges = append(edges, WaitGraphEdge{
						From: wait.TxID, To: queue[earlier].TxID, Resource: resource, Step: wait.Step,
					})
				}
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		if edges[i].To != edges[j].To {
			return edges[i].To < edges[j].To
		}
		if edges[i].Resource != edges[j].Resource {
			return edges[i].Resource < edges[j].Resource
		}
		return edges[i].Step < edges[j].Step
	})
	dedup := edges[:0]
	for _, edge := range edges {
		if len(dedup) > 0 {
			last := dedup[len(dedup)-1]
			if last.From == edge.From && last.To == edge.To && last.Resource == edge.Resource {
				continue
			}
		}
		dedup = append(dedup, edge)
	}
	return dedup
}

func (e *engine) findWaitCycle() []string {
	graph := e.buildWaitGraph()
	adj := make(map[string][]string)
	nodes := make(map[string]struct{})
	for _, edge := range graph {
		adj[edge.From] = append(adj[edge.From], edge.To)
		nodes[edge.From] = struct{}{}
		nodes[edge.To] = struct{}{}
	}
	for node := range adj {
		sort.Strings(adj[node])
	}
	ordered := make([]string, 0, len(nodes))
	for node := range nodes {
		ordered = append(ordered, node)
	}
	sort.Strings(ordered)
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int)
	stack := make([]string, 0)
	var found []string
	var visit func(string) bool
	visit = func(node string) bool {
		color[node] = gray
		stack = append(stack, node)
		for _, next := range adj[node] {
			switch color[next] {
			case white:
				if visit(next) {
					return true
				}
			case gray:
				start := 0
				for i, id := range stack {
					if id == next {
						start = i
						break
					}
				}
				found = append([]string(nil), stack[start:]...)
				sort.Strings(found)
				return true
			}
		}
		stack = stack[:len(stack)-1]
		color[node] = black
		return false
	}
	for _, node := range ordered {
		if color[node] == white && visit(node) {
			return found
		}
	}
	return nil
}

func (e *engine) snapshotWaitQueues() []WaitQueueSnapshot {
	resources := make([]string, 0, len(e.state.WaitQueues))
	for resource, queue := range e.state.WaitQueues {
		if len(queue) > 0 {
			resources = append(resources, resource)
		}
	}
	sort.Strings(resources)
	out := make([]WaitQueueSnapshot, 0, len(resources))
	for _, resource := range resources {
		snapshot := WaitQueueSnapshot{Resource: resource}
		for i, wait := range e.state.WaitQueues[resource] {
			snapshot.Members = append(snapshot.Members, WaitMember{
				Seat: i + 1, WaitID: wait.ID, TxID: wait.TxID, Step: wait.Step,
				Mode: wait.Mode, DeadlineMs: wait.DeadlineMs,
			})
		}
		out = append(out, snapshot)
	}
	return out
}

func (e *engine) recordQueueEvent(step int, event, txID, resource string, seat int) {
	e.state.QueueEvents = append(e.state.QueueEvents, QueueEvent{
		AfterStep: step, AtMs: e.state.ClockMs, Event: event, TxID: txID,
		Resource: resource, Seat: seat, Queues: e.snapshotWaitQueues(),
	})
}

func (e *engine) hasActiveWaits() bool {
	for _, queue := range e.state.WaitQueues {
		if len(queue) > 0 {
			return true
		}
	}
	return false
}

func (e *engine) waitCount() int {
	count := 0
	for _, queue := range e.state.WaitQueues {
		count += len(queue)
	}
	return count
}

func (e *engine) earliestWaitDeadline() int64 {
	first := true
	var deadline int64
	for _, queue := range e.state.WaitQueues {
		for _, wait := range queue {
			if first || wait.DeadlineMs < deadline {
				first = false
				deadline = wait.DeadlineMs
			}
		}
	}
	return deadline
}

func (e *engine) cancelAllWaits(step int, reason string) {
	for {
		var victim *WaitRequest
		for _, queue := range e.state.WaitQueues {
			if len(queue) > 0 {
				wait := queue[0]
				if victim == nil || wait.TxID < victim.TxID {
					victim = &wait
				}
			}
		}
		if victim == nil {
			return
		}
		tx := e.state.Transactions[victim.TxID]
		e.cancelWait(victim.ID, WaitCancelled)
		if tx != nil {
			e.rollbackTransaction(tx, step, reason, nil, true)
		}
	}
}

func (e *engine) flushCommitObserver() error {
	if len(e.pendingCommits) == 0 {
		return nil
	}
	commits := append([]CommitEvent(nil), e.pendingCommits...)
	e.pendingCommits = nil
	if e.options.OnCommit == nil {
		return nil
	}
	clone, err := cloneRuntimeState(&e.state)
	if err != nil {
		return err
	}
	if err := e.options.OnCommit(clone, commits); err != nil {
		if ErrorCodeOf(err) != "" {
			return err
		}
		return WrapError(EInternal, "$.checkpoint", err, "checkpoint observer failed")
	}
	return nil
}

func (e *engine) buildResult() *RunResult {
	steps := make([]StepResult, len(e.state.Steps))
	copy(steps, e.state.Steps)
	reads := make([]ReadObservation, 0)
	writes := make([]WriteObservation, 0)
	for _, step := range steps {
		if step.Read != nil {
			reads = append(reads, *step.Read)
		}
		if step.Write != nil {
			writes = append(writes, *step.Write)
		}
	}
	sort.Slice(reads, func(i, j int) bool {
		if reads[i].GlobalStep != reads[j].GlobalStep {
			return reads[i].GlobalStep < reads[j].GlobalStep
		}
		return reads[i].TxID < reads[j].TxID
	})
	sort.Slice(writes, func(i, j int) bool {
		if writes[i].GlobalStep != writes[j].GlobalStep {
			return writes[i].GlobalStep < writes[j].GlobalStep
		}
		return writes[i].TxID < writes[j].TxID
	})
	finalWrites := make(map[string]VersionRecord)
	for itemID, versions := range e.state.Versions {
		if len(versions) > 0 && versions[len(versions)-1].CommitSeq > 0 {
			finalWrites[itemID] = versions[len(versions)-1]
		}
	}
	anomalies := e.computeAnomalies()
	stateCopy, _ := cloneRuntimeState(&e.state)
	return &RunResult{
		ProjectID: e.project.ID, ProjectDigest: e.project.CanonicalDigest(), Seed: e.project.Seed,
		Steps: steps, QueueEvents: append([]QueueEvent(nil), e.state.QueueEvents...),
		Reads: reads, Writes: writes,
		Commits:     append([]CommitEvent(nil), e.state.Commits...),
		Rollbacks:   append([]RollbackEvent(nil), e.state.Rollbacks...),
		FinalWrites: finalWrites, Anomalies: anomalies,
		DeadlockVictims: append([]string(nil), e.state.DeadlockVictims...),
		FinalWaitGraph:  e.buildWaitGraph(), FinalWaitQueues: e.snapshotWaitQueues(),
		FinalState: stateCopy, Partial: e.stoppedEarly,
	}
}

func (r *RunResult) SemanticDigest() string {
	if r == nil {
		return ""
	}
	payload, _ := json.Marshal(struct {
		ProjectID       string                   `json:"project_id"`
		ProjectDigest   string                   `json:"project_digest"`
		Steps           []StepResult             `json:"steps"`
		QueueEvents     []QueueEvent             `json:"queue_events"`
		Reads           []ReadObservation        `json:"reads"`
		Writes          []WriteObservation       `json:"writes"`
		Commits         []CommitEvent            `json:"commits"`
		Rollbacks       []RollbackEvent          `json:"rollbacks"`
		FinalWrites     map[string]VersionRecord `json:"final_writes"`
		Anomalies       []Anomaly                `json:"anomalies"`
		DeadlockVictims []string                 `json:"deadlock_victims"`
	}{
		ProjectID: r.ProjectID, ProjectDigest: r.ProjectDigest, Steps: r.Steps,
		QueueEvents: r.QueueEvents, Reads: r.Reads, Writes: r.Writes,
		Commits: r.Commits, Rollbacks: r.Rollbacks, FinalWrites: r.FinalWrites,
		Anomalies: r.Anomalies, DeadlockVictims: r.DeadlockVictims,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func (r *RunResult) StateFingerprint() string {
	if r == nil || r.FinalState == nil {
		return ""
	}
	return RuntimeStateFingerprint(r.FinalState)
}
