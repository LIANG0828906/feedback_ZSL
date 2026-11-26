package registry

import (
	"fmt"
	"sort"
)

type runtimeInstance struct {
	config Instance
	state  InstanceState
}

type probeTask struct {
	ProbeRef
	NodeID string
	RackID string
	State  string
}

type Engine struct {
	project      *Project
	states       map[string]*runtimeInstance
	queue        []*probeTask
	inFlight     map[string]*probeTask
	nextQueueSeq int64
	nextTick     int64
	lastSeq      int
	lastAtMS     int64
}

type RunOptions struct {
	UntilTick *int64
}

type computation struct {
	result    *RunResult
	snapshots []Snapshot
}

type StepObserver func(step StepResult, snapshot Snapshot) error

func NewEngine(project *Project) (*Engine, error) {
	if project == nil {
		return nil, NewError(CodeInternal, "project is nil", ErrorContext{})
	}
	engine := &Engine{
		project:      project,
		states:       make(map[string]*runtimeInstance, len(project.Instances)),
		inFlight:     make(map[string]*probeTask),
		nextQueueSeq: 1,
		lastSeq:      -1,
		lastAtMS:     -1,
	}
	for _, instance := range project.Instances {
		engine.states[instance.ID] = &runtimeInstance{
			config: instance,
			state: InstanceState{
				InstanceID:       instance.ID,
				ServiceID:        instance.ServiceID,
				Phase:            PhaseUnregistered,
				Health:           instance.InitialHealth,
				LeaseExpiresAtMS: 0,
				LeaseRemainingMS: 0,
			},
		}
	}
	return engine, nil
}

func RestoreEngine(project *Project, snapshot *Snapshot) (*Engine, error) {
	if snapshot == nil {
		return NewEngine(project)
	}
	if snapshot.EngineVersion != EngineVersion {
		return nil, NewError(CodeCheckpointVersion, "checkpoint engine version does not match", ErrorContext{Checkpoint: snapshot.EngineVersion})
	}
	if snapshot.ProjectVersion != project.ProjectVersion {
		return nil, NewError(CodeCheckpointVersion, "checkpoint project version does not match", ErrorContext{Checkpoint: snapshot.ProjectVersion})
	}
	hash, err := project.ModelHash()
	if err != nil {
		return nil, err
	}
	if snapshot.ModelHash != hash {
		return nil, NewError(CodeCheckpointVersion, "checkpoint project model hash does not match", ErrorContext{Checkpoint: snapshot.ModelHash})
	}
	engine, err := NewEngine(project)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(snapshot.Instances))
	for _, runtimeSnapshot := range snapshot.Instances {
		runtime, ok := engine.states[runtimeSnapshot.InstanceID]
		if !ok {
			return nil, NewError(CodeCheckpointCorrupt, "checkpoint references an unknown instance", ErrorContext{InstanceID: runtimeSnapshot.InstanceID})
		}
		runtime.state = runtimeSnapshot.InstanceState
		seen[runtimeSnapshot.InstanceID] = struct{}{}
	}
	if len(seen) != len(engine.states) {
		return nil, NewError(CodeCheckpointCorrupt, "checkpoint does not contain every instance", ErrorContext{})
	}
	for _, taskSnapshot := range snapshot.InFlight {
		task, err := taskFromSnapshot(taskSnapshot)
		if err != nil {
			return nil, err
		}
		if _, exists := engine.states[task.InstanceID]; !exists {
			return nil, NewError(CodeCheckpointCorrupt, "in-flight probe references an unknown instance", ErrorContext{InstanceID: task.InstanceID})
		}
		if _, exists := engine.inFlight[task.InstanceID]; exists {
			return nil, NewError(CodeCheckpointCorrupt, "multiple in-flight probes for one instance", ErrorContext{InstanceID: task.InstanceID})
		}
		task.State = "in_flight"
		engine.inFlight[task.InstanceID] = task
	}
	for _, taskSnapshot := range snapshot.Queue {
		task, err := taskFromSnapshot(taskSnapshot)
		if err != nil {
			return nil, err
		}
		if _, exists := engine.states[task.InstanceID]; !exists {
			return nil, NewError(CodeCheckpointCorrupt, "queued probe references an unknown instance", ErrorContext{InstanceID: task.InstanceID})
		}
		task.State = "queued"
		engine.queue = append(engine.queue, task)
	}
	sort.SliceStable(engine.queue, func(i, j int) bool {
		return engine.queue[i].QueueSeq < engine.queue[j].QueueSeq
	})
	engine.nextQueueSeq = snapshot.NextQueueSeq
	if engine.nextQueueSeq <= 0 {
		engine.nextQueueSeq = 1
	}
	engine.nextTick = snapshot.NextTick
	engine.lastSeq = snapshot.CommitSeq
	engine.lastAtMS = snapshot.LastAtMS
	return engine, nil
}

func taskFromSnapshot(snapshot ProbeTaskSnapshot) (*probeTask, error) {
	return &probeTask{
		ProbeRef: ProbeRef{
			ProbeID:      snapshot.ProbeID,
			InstanceID:   snapshot.InstanceID,
			ServiceID:    snapshot.ServiceID,
			DueAtMS:      snapshot.DueAtMS,
			DispatchedMS: snapshot.DispatchedMS,
			DeadlineMS:   snapshot.DeadlineMS,
			QueueSeq:     snapshot.QueueSeq,
			Violations:   append([]string(nil), snapshot.Violations...),
		},
		NodeID: snapshot.NodeID,
		RackID: snapshot.RackID,
		State:  snapshot.State,
	}, nil
}

func (e *Engine) Snapshot() Snapshot {
	snapshot := Snapshot{
		EngineVersion:  EngineVersion,
		ProjectVersion: e.project.ProjectVersion,
		ModelHash:      "",
		SchemaVersion:  e.project.SchemaVersion,
		NextTick:       e.nextTick,
		CommitSeq:      e.lastSeq,
		LastAtMS:       e.lastAtMS,
		NextQueueSeq:   e.nextQueueSeq,
		Queue:          []ProbeTaskSnapshot{},
		InFlight:       []ProbeTaskSnapshot{},
	}
	snapshot.ModelHash, _ = e.project.ModelHash()
	for _, instance := range e.project.Instances {
		runtime := e.states[instance.ID]
		if runtime == nil {
			continue
		}
		snapshot.Instances = append(snapshot.Instances, RuntimeSnapshot{
			InstanceState: runtime.state,
			InitialHealth: instance.InitialHealth,
		})
	}
	for _, task := range e.queue {
		snapshot.Queue = append(snapshot.Queue, probeSnapshot(task, "queued"))
	}
	for _, instance := range e.project.Instances {
		if task, ok := e.inFlight[instance.ID]; ok {
			snapshot.InFlight = append(snapshot.InFlight, probeSnapshot(task, "in_flight"))
		}
	}
	return snapshot
}

func probeSnapshot(task *probeTask, state string) ProbeTaskSnapshot {
	return ProbeTaskSnapshot{
		ProbeID:      task.ProbeID,
		InstanceID:   task.InstanceID,
		ServiceID:    task.ServiceID,
		NodeID:       task.NodeID,
		RackID:       task.RackID,
		DueAtMS:      task.DueAtMS,
		DispatchedMS: task.DispatchedMS,
		DeadlineMS:   task.DeadlineMS,
		QueueSeq:     task.QueueSeq,
		Violations:   append([]string(nil), task.Violations...),
		State:        state,
	}
}

func (e *Engine) run(untilTick int64, keepSnapshots bool) (*computation, error) {
	hash, err := e.project.ModelHash()
	if err != nil {
		return nil, err
	}
	if untilTick < e.nextTick {
		return &computation{result: &RunResult{
			ProjectVersion: e.project.ProjectVersion,
			ModelHash:      hash,
			SchemaVersion:  e.project.SchemaVersion,
			Steps:          []StepResult{},
		}}, nil
	}
	eventsByTick := make(map[int64][]int)
	for index, event := range e.project.Events {
		tick := event.AtMS / e.project.TickMS
		eventsByTick[tick] = append(eventsByTick[tick], index)
	}
	result := &RunResult{
		ProjectVersion: e.project.ProjectVersion,
		ModelHash:      hash,
		SchemaVersion:  e.project.SchemaVersion,
		Steps:          []StepResult{},
	}
	computation := &computation{result: result}
	if keepSnapshots {
		computation.snapshots = []Snapshot{}
	}
	for tick := e.nextTick; tick <= untilTick; tick++ {
		step, err := e.step(tick, eventsByTick[tick])
		if err != nil {
			return nil, err
		}
		result.Steps = append(result.Steps, step)
		if keepSnapshots {
			computation.snapshots = append(computation.snapshots, e.Snapshot())
		}
	}
	return computation, nil
}

func (e *Engine) step(tick int64, eventIndexes []int) (StepResult, error) {
	now := tick * e.project.TickMS
	step := StepResult{
		Seq:              int(tick),
		Tick:             tick,
		AtMS:             now,
		Events:           []EventOutcome{},
		Instances:        []InstanceState{},
		DueProbes:        []ProbeRef{},
		SuppressedProbes: []SuppressedProbe{},
		DeferredProbes:   []DeferredProbe{},
		DroppedProbes:    []DroppedProbe{},
		CompletedProbes:  []ProbeCompletion{},
		TimedOutProbes:   []ProbeCompletion{},
		Transitions:      []HealthTransition{},
		Catalogs:         []Catalog{},
		Queries:          []QueryResult{},
	}

	e.expireLeases(now, &step)
	for _, index := range eventIndexes {
		outcome := e.applyEvent(e.project.Events[index], index, now, &step)
		step.Events = append(step.Events, outcome)
	}
	e.expireProbeTimeouts(now, &step)
	e.dispatch(now, &step)

	for _, instance := range e.project.Instances {
		runtime := e.states[instance.ID]
		if runtime == nil {
			continue
		}
		if runtime.state.Phase == PhaseRegistered {
			remaining := runtime.state.LeaseExpiresAtMS - now
			if remaining < 0 {
				remaining = 0
			}
			runtime.state.LeaseRemainingMS = remaining
		} else {
			runtime.state.LeaseRemainingMS = 0
		}
		if task, ok := e.inFlight[instance.ID]; ok {
			runtime.state.InFlightProbeID = task.ProbeID
		} else {
			runtime.state.InFlightProbeID = ""
		}
		step.Instances = append(step.Instances, runtime.state)
	}
	for _, service := range e.project.Services {
		step.Catalogs = append(step.Catalogs, buildCatalog(e.project, e.states, service, now, tick, service.DefaultWeightScale, 0))
	}
	e.lastSeq = int(tick)
	e.lastAtMS = now
	e.nextTick = tick + 1
	return step, nil
}

func (e *Engine) expireLeases(now int64, step *StepResult) {
	for _, instance := range e.project.Instances {
		runtime := e.states[instance.ID]
		if runtime == nil || runtime.state.Phase != PhaseRegistered {
			continue
		}
		if now < runtime.state.LeaseExpiresAtMS {
			continue
		}
		runtime.state.Phase = PhaseRemoval
		runtime.state.RemovalReason = CodeExcludeLeaseExpired
		runtime.state.LeaseRemainingMS = 0
		runtime.state.NextProbeDueMS = 0
		e.cancelProbes(instance.ID, now, CodeExcludeLeaseExpired, step)
	}
}

func (e *Engine) cancelProbes(instanceID string, now int64, reason string, step *StepResult) {
	if task, ok := e.inFlight[instanceID]; ok {
		delete(e.inFlight, instanceID)
		step.CompletedProbes = append(step.CompletedProbes, ProbeCompletion{
			ProbeID:    task.ProbeID,
			InstanceID: task.InstanceID,
			AtMS:       now,
			Success:    false,
			Reason:     reason,
		})
	}
	kept := e.queue[:0]
	for _, task := range e.queue {
		if task.InstanceID != instanceID {
			kept = append(kept, task)
			continue
		}
		step.DroppedProbes = append(step.DroppedProbes, DroppedProbe{
			ProbeID:    task.ProbeID,
			InstanceID: task.InstanceID,
			DroppedMS:  now,
			QueuedAtMS: task.DueAtMS,
			QueueSeq:   task.QueueSeq,
			Reason:     reason,
			Violations: append([]string(nil), task.Violations...),
		})
	}
	e.queue = kept
}

func (e *Engine) applyEvent(event Event, index int, now int64, step *StepResult) EventOutcome {
	outcome := EventOutcome{
		Index:      index,
		Kind:       event.Kind,
		AtMS:       now,
		ServiceID:  event.ServiceID,
		InstanceID: event.InstanceID,
	}
	runtime := e.states[event.InstanceID]
	tick := now / e.project.TickMS
	switch event.Kind {
	case EventRegister:
		if runtime == nil {
			outcome.Code = CodeUnknownInstance
			return outcome
		}
		if runtime.state.Phase == PhaseRegistered {
			outcome.Code = CodeInvalidEvent
			return outcome
		}
		runtime.state.Phase = PhaseRegistered
		runtime.state.Health = runtime.config.InitialHealth
		config := EffectiveInstance(e.project, runtime.config, tick)
		runtime.state.LeaseExpiresAtMS = now + config.LeaseTTLMS
		runtime.state.LeaseRemainingMS = config.LeaseTTLMS
		runtime.state.RenewalCount = 0
		runtime.state.ConsecutiveOK = 0
		runtime.state.ConsecutiveFail = 0
		runtime.state.NextProbeDueMS = now
		runtime.state.RemovalReason = ""
		runtime.state.LastTransitionAt = now
		runtime.state.LastTransitionWhy = "register"
		outcome.Accepted = true
	case EventDeregister:
		if runtime == nil {
			outcome.Code = CodeUnknownInstance
			return outcome
		}
		if runtime.state.Phase != PhaseRegistered {
			outcome.Code = CodeExcludeNotRegistered
			return outcome
		}
		runtime.state.Phase = PhaseRemoval
		runtime.state.RemovalReason = CodeExcludeDeregistered
		runtime.state.LeaseRemainingMS = 0
		runtime.state.NextProbeDueMS = 0
		e.cancelProbes(runtime.config.ID, now, CodeExcludeDeregistered, step)
		outcome.Accepted = true
	case EventHeartbeat:
		if runtime == nil {
			outcome.Code = CodeUnknownInstance
			return outcome
		}
		if runtime.state.Phase != PhaseRegistered {
			outcome.Code = CodeExcludeNotRegistered
			return outcome
		}
		config := EffectiveInstance(e.project, runtime.config, tick)
		runtime.state.LeaseExpiresAtMS = now + config.LeaseTTLMS
		runtime.state.LeaseRemainingMS = config.LeaseTTLMS
		runtime.state.RenewalCount++
		outcome.Accepted = true
	case EventProbeResult:
		if runtime == nil {
			outcome.Code = CodeUnknownInstance
			return outcome
		}
		if runtime.state.Phase != PhaseRegistered {
			outcome.Code = CodeExcludeNotRegistered
			return outcome
		}
		task := e.inFlight[runtime.config.ID]
		if event.ProbeID != "" {
			task = nil
			for _, candidate := range e.inFlight {
				if candidate.ProbeID == event.ProbeID {
					task = candidate
					break
				}
			}
			if task != nil && task.InstanceID != runtime.config.ID {
				task = nil
			}
			if task == nil {
				if e.queuedProbeByID(event.ProbeID) != nil {
					outcome.Code = CodeProbeQueued
				} else {
					outcome.Code = CodeProbeUnknownTask
				}
				return outcome
			}
		}
		if task == nil {
			outcome.Code = CodeProbeNoInflight
			return outcome
		}
		delete(e.inFlight, task.InstanceID)
		success := event.Success != nil && *event.Success
		step.CompletedProbes = append(step.CompletedProbes, ProbeCompletion{
			ProbeID:    task.ProbeID,
			InstanceID: task.InstanceID,
			AtMS:       now,
			Success:    success,
			Reason:     "probe_result",
		})
		e.applyProbeOutcome(runtime, success, now, "probe_result", step)
		outcome.Accepted = true
	case EventQuery:
		service, ok := e.project.ServiceByID(event.ServiceID)
		if !ok {
			outcome.Code = CodeUnknownService
			return outcome
		}
		queryID := event.QueryID
		if queryID == "" {
			queryID = fmt.Sprintf("query-%d", index)
		}
		catalog := buildCatalog(e.project, e.states, service, now, tick, event.WeightScale, event.Limit)
		step.Queries = append(step.Queries, QueryResult{
			QueryID:   queryID,
			ServiceID: service.ID,
			AtMS:      now,
			Catalog:   catalog,
		})
		outcome.Accepted = true
	default:
		outcome.Code = CodeInvalidEvent
	}
	return outcome
}

func (e *Engine) queuedProbeByID(probeID string) *probeTask {
	for _, task := range e.queue {
		if task.ProbeID == probeID {
			return task
		}
	}
	return nil
}

func (e *Engine) expireProbeTimeouts(now int64, step *StepResult) {
	for _, instance := range e.project.Instances {
		task := e.inFlight[instance.ID]
		if task == nil || task.DeadlineMS > now {
			continue
		}
		delete(e.inFlight, instance.ID)
		completion := ProbeCompletion{
			ProbeID:    task.ProbeID,
			InstanceID: task.InstanceID,
			AtMS:       now,
			Success:    false,
			Reason:     "probe_timeout",
			TimedOut:   true,
		}
		step.CompletedProbes = append(step.CompletedProbes, completion)
		step.TimedOutProbes = append(step.TimedOutProbes, completion)
		e.applyProbeOutcome(e.states[instance.ID], false, now, "probe_timeout", step)
	}
}

func (e *Engine) applyProbeOutcome(runtime *runtimeInstance, success bool, atMS int64, cause string, step *StepResult) {
	if runtime == nil || runtime.state.Phase != PhaseRegistered {
		return
	}
	from := runtime.state.Health
	tick := atMS / e.project.TickMS
	config := EffectiveInstance(e.project, runtime.config, tick)
	if success {
		runtime.state.ConsecutiveOK++
		runtime.state.ConsecutiveFail = 0
	} else {
		runtime.state.ConsecutiveFail++
		runtime.state.ConsecutiveOK = 0
	}
	to := from
	switch from {
	case HealthInitial:
		switch {
		case runtime.state.ConsecutiveFail >= config.FailureThreshold:
			to = HealthUnhealthy
		case runtime.state.ConsecutiveOK >= config.SuccessThreshold:
			to = HealthHealthy
		}
	case HealthHealthy:
		if runtime.state.ConsecutiveFail >= config.FailureThreshold {
			to = HealthUnhealthy
		}
	case HealthUnhealthy:
		if runtime.state.ConsecutiveOK >= config.SuccessThreshold {
			to = HealthHalfOpen
		}
	case HealthHalfOpen:
		switch {
		case runtime.state.ConsecutiveFail >= config.FailureThreshold:
			to = HealthUnhealthy
		case runtime.state.ConsecutiveOK >= config.SuccessThreshold:
			to = HealthHealthy
		}
	}
	okCount := runtime.state.ConsecutiveOK
	failCount := runtime.state.ConsecutiveFail
	if to != from {
		runtime.state.Health = to
		runtime.state.ConsecutiveOK = 0
		runtime.state.ConsecutiveFail = 0
		runtime.state.LastTransitionAt = atMS
		runtime.state.LastTransitionWhy = cause
		step.Transitions = append(step.Transitions, HealthTransition{
			InstanceID:        runtime.config.ID,
			From:              from,
			To:                to,
			AtMS:              atMS,
			Cause:             cause,
			ConsecutiveOK:     okCount,
			ConsecutiveFailed: failCount,
		})
	}
}

func (e *Engine) dispatch(now int64, step *StepResult) {
	e.expireDeferred(now, step)
	e.dispatchQueue(now, step)

	var newTasks []*probeTask
	tick := now / e.project.TickMS
	for _, baseInstance := range e.project.Instances {
		instance := EffectiveInstance(e.project, baseInstance, tick)
		runtime := e.states[instance.ID]
		if runtime == nil || runtime.state.Phase != PhaseRegistered {
			continue
		}
		if runtime.state.NextProbeDueMS > now {
			continue
		}
		probeID := fmt.Sprintf("%s@%d", instance.ID, now)
		ref := ProbeRef{
			ProbeID:    probeID,
			InstanceID: instance.ID,
			ServiceID:  instance.ServiceID,
			DueAtMS:    now,
		}
		if _, exists := e.inFlight[instance.ID]; exists {
			step.SuppressedProbes = append(step.SuppressedProbes, SuppressedProbe{
				ProbeID:    probeID,
				InstanceID: instance.ID,
				AtMS:       now,
				Reason:     CodeProbeInFlight,
			})
			runtime.state.NextProbeDueMS = now + instance.ProbePeriodMS
			continue
		}
		if e.queuedCount(instance.ID) >= e.project.Budgets.InstanceBuffer {
			step.DroppedProbes = append(step.DroppedProbes, DroppedProbe{
				ProbeID:    probeID,
				InstanceID: instance.ID,
				DroppedMS:  now,
				QueuedAtMS: now,
				Reason:     CodeProbeBufferFull,
				Violations: []string{ConstraintInstance},
			})
			ref.Violations = []string{ConstraintInstance}
			step.DueProbes = append(step.DueProbes, ref)
			runtime.state.NextProbeDueMS = now + instance.ProbePeriodMS
			continue
		}
		task := &probeTask{
			ProbeRef: ref,
			NodeID:   instance.NodeID,
			RackID:   instance.RackID,
			State:    "queued",
		}
		task.QueueSeq = e.nextQueueSeq
		e.nextQueueSeq++
		e.queue = append(e.queue, task)
		newTasks = append(newTasks, task)
		runtime.state.NextProbeDueMS = now + instance.ProbePeriodMS
	}
	e.dispatchQueue(now, step)

	for _, task := range newTasks {
		step.DueProbes = append(step.DueProbes, task.ProbeRef)
	}
	for _, task := range e.queue {
		step.DeferredProbes = append(step.DeferredProbes, DeferredProbe{
			ProbeID:    task.ProbeID,
			InstanceID: task.InstanceID,
			QueuedAtMS: task.DueAtMS,
			QueueSeq:   task.QueueSeq,
			Violations: append([]string(nil), task.Violations...),
		})
	}
}

func (e *Engine) expireDeferred(now int64, step *StepResult) {
	kept := e.queue[:0]
	for _, task := range e.queue {
		if now-task.DueAtMS > e.project.Budgets.MaxDelayMS {
			step.DroppedProbes = append(step.DroppedProbes, DroppedProbe{
				ProbeID:    task.ProbeID,
				InstanceID: task.InstanceID,
				DroppedMS:  now,
				QueuedAtMS: task.DueAtMS,
				QueueSeq:   task.QueueSeq,
				Reason:     CodeProbeMaxDelay,
				Violations: append([]string(nil), task.Violations...),
			})
			continue
		}
		kept = append(kept, task)
	}
	e.queue = kept
}

func (e *Engine) dispatchQueue(now int64, step *StepResult) {
	if len(e.queue) == 0 {
		return
	}
	nodeCounts := make(map[string]int)
	rackCounts := make(map[string]int)
	for _, task := range e.inFlight {
		nodeCounts[task.NodeID]++
		rackCounts[task.RackID]++
	}
	kept := make([]*probeTask, 0, len(e.queue))
	for _, task := range e.queue {
		runtime := e.states[task.InstanceID]
		if runtime == nil || runtime.state.Phase != PhaseRegistered {
			step.DroppedProbes = append(step.DroppedProbes, DroppedProbe{
				ProbeID:    task.ProbeID,
				InstanceID: task.InstanceID,
				DroppedMS:  now,
				QueuedAtMS: task.DueAtMS,
				QueueSeq:   task.QueueSeq,
				Reason:     CodeExcludeNotRegistered,
				Violations: append([]string(nil), task.Violations...),
			})
			continue
		}
		if _, exists := e.inFlight[task.InstanceID]; exists {
			task.Violations = appendUnique(task.Violations, CodeProbeInFlight)
			kept = append(kept, task)
			continue
		}
		violations := make([]string, 0, 3)
		if len(e.inFlight) >= e.project.Budgets.GlobalConcurrency {
			violations = append(violations, ConstraintGlobal)
		}
		if nodeCounts[task.NodeID] >= e.project.Budgets.NodeConcurrency {
			violations = append(violations, ConstraintNode)
		}
		if rackCounts[task.RackID] >= e.project.Budgets.RackConcurrency {
			violations = append(violations, ConstraintRack)
		}
		for _, violation := range violations {
			task.Violations = appendUnique(task.Violations, violation)
		}
		if len(violations) > 0 {
			kept = append(kept, task)
			continue
		}
		task.State = "in_flight"
		task.DispatchedMS = now
		task.DeadlineMS = now + EffectiveInstance(e.project, runtime.config, now/e.project.TickMS).ProbeTimeoutMS
		e.inFlight[task.InstanceID] = task
		nodeCounts[task.NodeID]++
		rackCounts[task.RackID]++
	}
	e.queue = kept
}

func (e *Engine) queuedCount(instanceID string) int {
	count := 0
	for _, task := range e.queue {
		if task.InstanceID == instanceID {
			count++
		}
	}
	return count
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	values = append(values, value)
	sort.Strings(values)
	return values
}

func (e *Engine) RunAll() (*RunResult, error) {
	until := e.project.MaxEventTick()
	computed, err := e.run(until, false)
	if err != nil {
		return nil, err
	}
	return computed.result, nil
}

func (e *Engine) RunWithObserver(untilTick int64, observer StepObserver) (*RunResult, error) {
	hash, err := e.project.ModelHash()
	if err != nil {
		return nil, err
	}
	result := &RunResult{
		ProjectVersion: e.project.ProjectVersion,
		ModelHash:      hash,
		SchemaVersion:  e.project.SchemaVersion,
		Steps:          []StepResult{},
	}
	if untilTick < e.nextTick {
		return result, nil
	}
	eventsByTick := make(map[int64][]int)
	for index, event := range e.project.Events {
		tick := event.AtMS / e.project.TickMS
		eventsByTick[tick] = append(eventsByTick[tick], index)
	}
	for tick := e.nextTick; tick <= untilTick; tick++ {
		step, err := e.step(tick, eventsByTick[tick])
		if err != nil {
			return nil, err
		}
		result.Steps = append(result.Steps, step)
		if observer != nil {
			if err := observer(step, e.Snapshot()); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func (e *Engine) RunUntil(tick int64) (*RunResult, error) {
	computed, err := e.run(tick, false)
	if err != nil {
		return nil, err
	}
	return computed.result, nil
}

func RunProject(project *Project, options RunOptions) (*RunResult, error) {
	engine, err := NewEngine(project)
	if err != nil {
		return nil, err
	}
	until := project.MaxEventTick()
	if options.UntilTick != nil {
		until = *options.UntilTick
	}
	computed, err := engine.run(until, false)
	if err != nil {
		return nil, err
	}
	return computed.result, nil
}

func ComputeProject(project *Project, untilTick *int64, keepSnapshots bool) (*RunResult, []Snapshot, error) {
	engine, err := NewEngine(project)
	if err != nil {
		return nil, nil, err
	}
	until := project.MaxEventTick()
	if untilTick != nil {
		until = *untilTick
	}
	computed, err := engine.run(until, keepSnapshots)
	if err != nil {
		return nil, nil, err
	}
	return computed.result, computed.snapshots, nil
}
