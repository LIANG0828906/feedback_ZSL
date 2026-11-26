package locklab

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

type StepInvalidation struct {
	GlobalStep int    `json:"global_step"`
	TxID       string `json:"tx_id"`
	Reused     bool   `json:"reused"`
	SourceTx   string `json:"source_tx,omitempty"`
	Reason     string `json:"reason"`
}

type IncrementalReport struct {
	SourceTx               string             `json:"source_tx"`
	EditedFields           []string           `json:"edited_fields"`
	PropagationClosure     []string           `json:"propagation_closure"`
	FirstInvalidatedStep   int                `json:"first_invalidated_step"`
	LastEditedStep         int                `json:"last_edited_step"`
	ResyncStep             int                `json:"resync_step,omitempty"`
	ReusedPrefixSteps      int                `json:"reused_prefix_steps"`
	RecomputedSteps        []int              `json:"recomputed_steps"`
	ReusedSuffixSteps      int                `json:"reused_suffix_steps"`
	AffectedQueueIntervals []QueueInterval    `json:"affected_queue_intervals,omitempty"`
	Steps                  []StepInvalidation `json:"steps"`
}

type QueueInterval struct {
	Resource string   `json:"resource"`
	FromStep int      `json:"from_step"`
	ToStep   int      `json:"to_step"`
	Waiters  []string `json:"waiters"`
}

type IncrementalResult struct {
	Result *RunResult
	Report IncrementalReport
}

type BaselineTrace struct {
	Project       *Project
	ProjectDigest string
	Schedule      []ScheduledOp
	Result        *RunResult
	Snapshots     []*RuntimeState
}

// BuildBaseline materializes the cacheable full-run trace used by incremental
// replay. Callers can keep this value across edits to the same base project.
func BuildBaseline(project *Project) (*BaselineTrace, error) {
	p := project.Clone()
	if p == nil {
		return nil, NewError(EFieldRequired, "$", "project is nil")
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	schedule := BuildSchedule(p)
	snapshots := make([]*RuntimeState, len(schedule))
	result, err := Run(p, RunOptions{AfterStep: func(step int, state *RuntimeState) (bool, error) {
		snapshots[step-1] = state
		return false, nil
	}})
	if err != nil {
		return nil, err
	}
	return &BaselineTrace{
		Project: p, ProjectDigest: p.CanonicalDigest(),
		Schedule: schedule, Result: result, Snapshots: snapshots,
	}, nil
}

// RunIncremental recomputes only the dependency chain affected by one edited
// transaction. If operation edits shift global scheduling slots, the cache is
// reused up to the first divergence. When the new execution state
// reconverges with the cached trace after all edited operations are consumed,
// the old suffix is spliced back without re-executing it.
func RunIncremental(oldProject, newProject *Project, editedTxID string) (*IncrementalResult, error) {
	baseline, err := BuildBaseline(oldProject)
	if err != nil {
		return nil, err
	}
	return RunIncrementalWithBaseline(baseline, newProject, editedTxID)
}

func RunIncrementalWithBaseline(baseline *BaselineTrace, newProject *Project, editedTxID string) (*IncrementalResult, error) {
	if baseline == nil || baseline.Project == nil || baseline.Result == nil {
		return nil, NewError(EFieldRequired, "$.baseline", "baseline trace is incomplete")
	}
	if len(baseline.Schedule) == 0 || len(baseline.Snapshots) != len(baseline.Schedule) ||
		len(baseline.Result.Steps) != len(baseline.Schedule) {
		return nil, NewError(EInvalidEdit, "$.baseline",
			"baseline schedule, snapshots and result steps are inconsistent")
	}
	oldP := baseline.Project.Clone()
	newP := newProject.Clone()
	if newP == nil {
		return nil, NewError(EFieldRequired, "$", "old and new projects are required")
	}
	if err := ValidateProject(newP); err != nil {
		return nil, err
	}
	if baseline.ProjectDigest != oldP.CanonicalDigest() {
		return nil, NewError(EInvalidEdit, "$.baseline.project_digest",
			"baseline trace does not match the old project")
	}
	if err := validateIncrementalEdit(oldP, newP, editedTxID); err != nil {
		return nil, err
	}
	return runIncrementalWithTrace(oldP, newP, baseline.Schedule, baseline.Snapshots,
		baseline.Result, editedTxID)
}

func runIncrementalWithTrace(oldP, newP *Project, oldSched []ScheduledOp,
	oldSnapshots []*RuntimeState, oldResult *RunResult, editedTxID string) (*IncrementalResult, error) {
	newSched := BuildSchedule(newP)
	if len(oldSched) != len(newSched) {
		return nil, NewError(EInvalidEdit, "$.schedule",
			"incremental edits must preserve logical step count")
	}

	editedFields := editedFieldNames(oldP, newP, editedTxID)
	first := firstInvalidatedStep(oldP, newP, oldSched, newSched, editedTxID)
	last := lastTxStep(newSched, editedTxID)
	if divergenceEnd := scheduleDivergenceEnd(oldP, newP, oldSched, newSched, editedTxID); divergenceEnd > last {
		last = divergenceEnd
	}
	report := IncrementalReport{
		SourceTx: editedTxID, EditedFields: editedFields,
		FirstInvalidatedStep: first, LastEditedStep: last,
	}
	if first == 0 {
		report.PropagationClosure = []string{editedTxID}
		report.ReusedPrefixSteps = len(oldSched)
		for i, scheduled := range oldSched {
			report.Steps = append(report.Steps, StepInvalidation{
				GlobalStep: i + 1, TxID: scheduled.TxID, Reused: true, Reason: "no_change",
			})
		}
		result := cloneRunResult(oldResult)
		result.ProjectDigest = newP.CanonicalDigest()
		return &IncrementalResult{Result: result, Report: report}, nil
	}

	var base *RuntimeState
	if first == 1 {
		state := newRuntimeState(newP, newSched)
		base = &state
	} else {
		cloned, err := cloneRuntimeState(oldSnapshots[first-2])
		if err != nil {
			return nil, err
		}
		base = cloned
		base.ProjectID = newP.ID
		base.ProjectDigest = newP.CanonicalDigest()
		reconcileTxConfiguration(base, newP)
	}

	resyncStep := 0
	partial, err := ResumeRun(newP, base, RunOptions{AfterStep: func(step int, state *RuntimeState) (bool, error) {
		if step < last {
			return false, nil
		}
		if RuntimeStateFingerprint(state) != RuntimeStateFingerprint(oldSnapshots[step-1]) {
			return false, nil
		}
		if !semanticStepEqual(state.Steps[step-1], oldResult.Steps[step-1]) {
			return false, nil
		}
		resyncStep = step
		return true, nil
	}})
	if err != nil {
		return nil, err
	}

	var result *RunResult
	if resyncStep > 0 {
		result = spliceIncrementalResult(partial, oldResult, resyncStep, newP.CanonicalDigest())
	} else {
		result = partial
		result.ProjectDigest = newP.CanonicalDigest()
	}
	report.ResyncStep = resyncStep
	report.ReusedPrefixSteps = first - 1
	report.PropagationClosure = propagationClosure(editedTxID, oldResult)
	for i := 0; i < len(result.Steps) && i < len(oldResult.Steps); i++ {
		if !semanticStepEqual(result.Steps[i], oldResult.Steps[i]) {
			report.PropagationClosure = appendIfMissing(report.PropagationClosure, result.Steps[i].TxID)
		}
	}
	report.PropagationClosure = append(report.PropagationClosure, editedTxID)
	report.PropagationClosure = uniqueSorted(report.PropagationClosure)
	stop := len(result.Steps)
	if resyncStep > 0 {
		stop = resyncStep
		report.ReusedSuffixSteps = len(result.Steps) - resyncStep
	}
	for i, scheduled := range oldSched {
		step := i + 1
		invalidation := StepInvalidation{GlobalStep: step, TxID: scheduled.TxID}
		switch {
		case step < first:
			invalidation.Reused = true
			invalidation.Reason = "prefix_cache"
		case step > stop:
			invalidation.Reused = true
			invalidation.Reason = "state_reconverged"
		default:
			invalidation.Reason = "state_chain"
			switch {
			case scheduled.TxID == editedTxID:
				invalidation.SourceTx = editedTxID
				invalidation.Reason = "direct_edit"
			case containsString(report.PropagationClosure, scheduled.TxID):
				invalidation.SourceTx = editedTxID
				invalidation.Reason = "propagation"
			default:
				invalidation.SourceTx = editedTxID
			}
			report.RecomputedSteps = append(report.RecomputedSteps, step)
		}
		report.Steps = append(report.Steps, invalidation)
	}
	report.AffectedQueueIntervals = affectedQueueIntervals(result, first, stop)
	return &IncrementalResult{Result: result, Report: report}, nil
}

func validateIncrementalEdit(oldP, newP *Project, editedTxID string) error {
	if oldP.ID != newP.ID || oldP.Seed != newP.Seed ||
		oldP.DeadlockDetection != newP.DeadlockDetection ||
		oldP.WorkerBudget != newP.WorkerBudget ||
		oldP.MemoryBudgetBytes != newP.MemoryBudgetBytes ||
		oldP.ClockOriginMs != newP.ClockOriginMs ||
		!reflect.DeepEqual(oldP.Items, newP.Items) {
		return NewError(EInvalidEdit, "$", "project items/configuration cannot change during incremental replay")
	}
	if len(oldP.Transactions) != len(newP.Transactions) {
		return NewError(EInvalidEdit, "$.transactions", "transaction count must remain stable")
	}
	found := false
	for i := range oldP.Transactions {
		oldTx, newTx := oldP.Transactions[i], newP.Transactions[i]
		if oldTx.ID != newTx.ID {
			return NewError(EInvalidEdit, fmt.Sprintf("$.transactions[%d].id", i),
				"transaction order/identity changed")
		}
		if oldTx.ID == editedTxID {
			found = true
			if oldTx.Priority != newTx.Priority {
				return NewError(EInvalidEdit, fmt.Sprintf("$.transactions[%d].priority", i),
					"priority edits are not part of the incremental contract")
			}
			if len(oldTx.Ops) != len(newTx.Ops) {
				return NewError(EInvalidEdit, fmt.Sprintf("$.transactions[%d].operations", i),
					"operation count must remain stable for logical-step caching")
			}
			continue
		}
		if !reflect.DeepEqual(oldTx, newTx) {
			return NewError(EInvalidEdit, fmt.Sprintf("$.transactions[%d]", i),
				"only transaction %q may change", editedTxID)
		}
	}
	if !found {
		return NewError(EInvalidEdit, "$.transactions", "edited transaction %q was not found", editedTxID)
	}
	return nil
}

func editedFieldNames(oldP, newP *Project, editedTxID string) []string {
	oldTx, newTx := findTx(oldP, editedTxID), findTx(newP, editedTxID)
	fields := make([]string, 0)
	if oldTx == nil || newTx == nil {
		return fields
	}
	if oldTx.Isolation != newTx.Isolation {
		fields = append(fields, "isolation")
	}
	for i := range oldTx.Ops {
		if normalizeOpSignature(oldTx.Ops[i]) != normalizeOpSignature(newTx.Ops[i]) {
			fields = append(fields, "operations")
			break
		}
	}
	if len(fields) == 0 {
		fields = append(fields, "none")
	}
	return fields
}

func firstInvalidatedStep(oldP, newP *Project, oldSchedule, newSchedule []ScheduledOp, txID string) int {
	oldTx, newTx := findTx(oldP, txID), findTx(newP, txID)
	if oldTx == nil || newTx == nil {
		return 0
	}
	isolationChanged := oldTx.Isolation != newTx.Isolation
	for i := range oldSchedule {
		oldStep, newStep := oldSchedule[i], newSchedule[i]
		if oldStep.TxID != newStep.TxID ||
			oldStep.LocalSeq != newStep.LocalSeq ||
			oldStep.AtMs != newStep.AtMs {
			return i + 1
		}
		if oldStep.TxID != txID && newStep.TxID != txID {
			continue
		}
		if isolationChanged && (oldStep.TxID == txID || newStep.TxID == txID) {
			return i + 1
		}
		if oldStep.TxID == txID && newStep.TxID == txID {
			oldOp := oldTx.Ops[oldStep.OpIndex]
			newOp := newTx.Ops[newStep.OpIndex]
			if normalizeOpSignature(oldOp) != normalizeOpSignature(newOp) {
				return i + 1
			}
		}
	}
	return 0
}

func scheduleDivergenceEnd(oldP, newP *Project, oldSchedule, newSchedule []ScheduledOp, txID string) int {
	last := 0
	oldTx, newTx := findTx(oldP, txID), findTx(newP, txID)
	for i := range oldSchedule {
		oldStep, newStep := oldSchedule[i], newSchedule[i]
		different := oldStep.TxID != newStep.TxID ||
			oldStep.LocalSeq != newStep.LocalSeq ||
			oldStep.OpIndex != newStep.OpIndex ||
			oldStep.AtMs != newStep.AtMs
		if !different && oldStep.TxID == txID && oldTx != nil && newTx != nil {
			different = normalizeOpSignature(oldTx.Ops[oldStep.OpIndex]) !=
				normalizeOpSignature(newTx.Ops[newStep.OpIndex])
		}
		if different {
			last = i + 1
		}
	}
	return last
}

func lastTxStep(schedule []ScheduledOp, txID string) int {
	last := 0
	for i, scheduled := range schedule {
		if scheduled.TxID == txID {
			last = i + 1
		}
	}
	return last
}

func findTx(project *Project, txID string) *Transaction {
	for i := range project.Transactions {
		if project.Transactions[i].ID == txID {
			return &project.Transactions[i]
		}
	}
	return nil
}

func reconcileTxConfiguration(state *RuntimeState, project *Project) {
	if state == nil || project == nil {
		return
	}
	for _, tx := range project.Transactions {
		runtimeTx := state.Transactions[tx.ID]
		if runtimeTx == nil {
			continue
		}
		runtimeTx.Priority = tx.Priority
		runtimeTx.Isolation = tx.Isolation
	}
}

func spliceIncrementalResult(prefix, suffix *RunResult, resyncStep int, digest string) *RunResult {
	result := &RunResult{
		ProjectID: prefix.ProjectID, ProjectDigest: digest,
		Steps: make([]StepResult, 0, len(suffix.Steps)),
	}
	result.Steps = append(result.Steps, prefix.Steps[:resyncStep]...)
	result.Steps = append(result.Steps, suffix.Steps[resyncStep:]...)
	for _, event := range prefix.QueueEvents {
		if event.AfterStep <= resyncStep {
			result.QueueEvents = append(result.QueueEvents, event)
		}
	}
	for _, event := range suffix.QueueEvents {
		if event.AfterStep > resyncStep {
			result.QueueEvents = append(result.QueueEvents, event)
		}
	}
	for _, read := range prefix.Reads {
		if read.ExecutedStep <= resyncStep {
			result.Reads = append(result.Reads, read)
		}
	}
	for _, read := range suffix.Reads {
		if read.ExecutedStep > resyncStep {
			result.Reads = append(result.Reads, read)
		}
	}
	for _, write := range prefix.Writes {
		if write.ExecutedStep <= resyncStep {
			result.Writes = append(result.Writes, write)
		}
	}
	for _, write := range suffix.Writes {
		if write.ExecutedStep > resyncStep {
			result.Writes = append(result.Writes, write)
		}
	}
	for _, commit := range prefix.Commits {
		if commit.ExecutedStep <= resyncStep {
			result.Commits = append(result.Commits, commit)
		}
	}
	for _, commit := range suffix.Commits {
		if commit.ExecutedStep > resyncStep {
			result.Commits = append(result.Commits, commit)
		}
	}
	for _, rollback := range prefix.Rollbacks {
		if rollback.ExecutedStep <= resyncStep {
			result.Rollbacks = append(result.Rollbacks, rollback)
		}
	}
	for _, rollback := range suffix.Rollbacks {
		if rollback.ExecutedStep > resyncStep {
			result.Rollbacks = append(result.Rollbacks, rollback)
		}
	}
	result.FinalWrites = suffix.FinalWrites
	result.Anomalies = suffix.Anomalies
	result.DeadlockVictims = suffix.DeadlockVictims
	result.FinalWaitGraph = suffix.FinalWaitGraph
	result.FinalWaitQueues = suffix.FinalWaitQueues
	result.FinalState = suffix.FinalState
	result.Partial = false
	return result
}

func propagationClosure(edited string, result *RunResult) []string {
	adj := make(map[string]map[string]struct{})
	add := func(a, b string) {
		if a == "" || b == "" || a == b {
			return
		}
		if adj[a] == nil {
			adj[a] = make(map[string]struct{})
		}
		if adj[b] == nil {
			adj[b] = make(map[string]struct{})
		}
		adj[a][b] = struct{}{}
		adj[b][a] = struct{}{}
	}
	for _, step := range result.Steps {
		for _, opponent := range step.ConflictWith {
			add(step.TxID, opponent)
		}
	}
	for _, read := range result.Reads {
		if read.WriterTx != "__initial__" {
			add(read.WriterTx, read.TxID)
		}
	}
	for _, rollback := range result.Rollbacks {
		for _, id := range rollback.Cycle {
			add(rollback.TxID, id)
		}
	}
	for _, commit := range result.Commits {
		for _, write := range commit.Writes {
			add(write.WriterTx, commit.TxID)
		}
	}
	closed := map[string]struct{}{edited: {}}
	queue := []string{edited}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		next := make([]string, 0, len(adj[node]))
		for id := range adj[node] {
			next = append(next, id)
		}
		sort.Strings(next)
		for _, id := range next {
			if _, seen := closed[id]; seen {
				continue
			}
			closed[id] = struct{}{}
			queue = append(queue, id)
		}
	}
	out := make([]string, 0, len(closed))
	for id := range closed {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func cloneRunResult(result *RunResult) *RunResult {
	if result == nil {
		return nil
	}
	out := *result
	out.Steps = append([]StepResult(nil), result.Steps...)
	out.QueueEvents = append([]QueueEvent(nil), result.QueueEvents...)
	out.Reads = append([]ReadObservation(nil), result.Reads...)
	out.Writes = append([]WriteObservation(nil), result.Writes...)
	out.Commits = append([]CommitEvent(nil), result.Commits...)
	out.Rollbacks = append([]RollbackEvent(nil), result.Rollbacks...)
	out.Anomalies = append([]Anomaly(nil), result.Anomalies...)
	out.DeadlockVictims = append([]string(nil), result.DeadlockVictims...)
	out.FinalWaitGraph = append([]WaitGraphEdge(nil), result.FinalWaitGraph...)
	out.FinalWaitQueues = append([]WaitQueueSnapshot(nil), result.FinalWaitQueues...)
	if result.FinalWrites != nil {
		out.FinalWrites = make(map[string]VersionRecord, len(result.FinalWrites))
		for key, value := range result.FinalWrites {
			out.FinalWrites[key] = value
		}
	}
	return &out
}

func appendIfMissing(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func semanticStepEqual(a, b StepResult) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}

func affectedQueueIntervals(result *RunResult, fromStep, toStep int) []QueueInterval {
	type interval struct {
		from    int
		to      int
		waiters map[string]struct{}
	}
	intervals := make(map[string]*interval)
	for _, step := range result.Steps {
		if step.GlobalStep < fromStep || step.GlobalStep > toStep {
			continue
		}
		for _, queue := range step.WaitQueues {
			if len(queue.Members) == 0 {
				continue
			}
			current := intervals[queue.Resource]
			if current == nil {
				current = &interval{from: step.GlobalStep, to: step.GlobalStep, waiters: make(map[string]struct{})}
				intervals[queue.Resource] = current
			}
			if step.GlobalStep < current.from {
				current.from = step.GlobalStep
			}
			if step.GlobalStep > current.to {
				current.to = step.GlobalStep
			}
			for _, member := range queue.Members {
				current.waiters[member.TxID] = struct{}{}
			}
		}
	}
	resources := make([]string, 0, len(intervals))
	for resource := range intervals {
		resources = append(resources, resource)
	}
	sort.Strings(resources)
	out := make([]QueueInterval, 0, len(resources))
	for _, resource := range resources {
		value := intervals[resource]
		waiters := make([]string, 0, len(value.waiters))
		for id := range value.waiters {
			waiters = append(waiters, id)
		}
		sort.Strings(waiters)
		out = append(out, QueueInterval{
			Resource: resource, FromStep: value.from, ToStep: value.to, Waiters: waiters,
		})
	}
	return out
}
