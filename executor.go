package macrokit

import (
	"fmt"
	"sort"
)

type Executor struct {
	project *Project
	config  ExecutorConfig
	flags   map[string]bool

	frame          int64
	down           map[string]bool
	pressed        map[string]bool
	released       map[string]bool
	pressedAt      map[string]int64
	consumedOffset int

	results map[string]*BranchState
	active  map[string]*RuntimeState

	frames        []FrameTrace
	adjudications []Adjudication
	commits       []CommitRecord
}

func NewExecutor(project *Project, config ExecutorConfig) (*Executor, error) {
	if project == nil {
		return nil, newSchemaError("$", "project is required")
	}
	if project.BranchByID == nil {
		project.Index()
	}
	policy := config.Arbitration
	if policy == "" {
		policy = ArbitrationDeterministic
	}
	flags := cloneFlags(project.Flags)
	for k, v := range config.Flags {
		flags[k] = v
	}
	return &Executor{
		project:       project,
		config:        config,
		flags:         flags,
		down:          map[string]bool{},
		pressed:       map[string]bool{},
		released:      map[string]bool{},
		pressedAt:     map[string]int64{},
		results:       map[string]*BranchState{},
		active:        map[string]*RuntimeState{},
		adjudications: []Adjudication{},
		commits:       []CommitRecord{},
	}, nil
}

func RunProject(project *Project, stream EventStream, config ExecutorConfig) (*RunResult, error) {
	exec, err := NewExecutor(project, config)
	if err != nil {
		return nil, err
	}
	inputs, err := BuildFrameInputs(stream, config.FrameCount)
	if err != nil {
		return nil, err
	}
	for _, input := range inputs {
		if _, err := exec.Step(input); err != nil {
			return exec.Result(), err
		}
	}
	return exec.Result(), nil
}

func BuildFrameInputs(stream EventStream, frameCount int64) ([]FrameInput, error) {
	events := append([]KeyEvent(nil), stream.Events...)
	sort.SliceStable(events, func(i, j int) bool { return events[i].Frame < events[j].Frame })

	count := frameCount
	if stream.FrameCount > count {
		count = stream.FrameCount
	}
	for _, ev := range events {
		if ev.Frame < 0 {
			return nil, newSchemaError("/events/frame", "event frame must be non-negative")
		}
		if ev.Frame+1 > count {
			count = ev.Frame + 1
		}
	}
	if count <= 0 {
		count = 1
	}

	inputs := make([]FrameInput, count)
	for i := range inputs {
		inputs[i].Frame = int64(i)
	}
	for _, ev := range events {
		if ev.Frame < count {
			inputs[ev.Frame].Events = append(inputs[ev.Frame].Events, ev)
		}
	}
	return inputs, nil
}

func (x *Executor) Step(input FrameInput) (FrameTrace, error) {
	if input.Frame != x.frame {
		err := newSchemaError("/frame", "non-contiguous frame: expected %d got %d", x.frame, input.Frame)
		return FrameTrace{}, err
	}

	x.pressed = map[string]bool{}
	x.released = map[string]bool{}
	for _, ev := range input.Events {
		switch ev.Kind {
		case "down":
			x.down[ev.Key] = true
			x.pressed[ev.Key] = true
			x.pressedAt[ev.Key] = input.Frame
		case "up":
			delete(x.down, ev.Key)
			x.released[ev.Key] = true
		default:
			return FrameTrace{}, newSchemaError(fmt.Sprintf("/events/%s/kind", ev.Key), "unsupported event kind %q", ev.Kind)
		}
		x.consumedOffset++
	}

	snapshot := KeySnapshot{
		Frame:          input.Frame,
		Down:           cloneBoolMap(x.down),
		Pressed:        cloneBoolMap(x.pressed),
		Released:       cloneBoolMap(x.released),
		PressedAt:      cloneInt64Map(x.pressedAt),
		ConsumedOffset: x.consumedOffset,
	}

	trace := FrameTrace{
		Frame:             input.Frame,
		ConsumedOffset:    x.consumedOffset,
		ConditionSnapshot: map[string]bool{},
	}

	prior := make(map[string]*BranchState)
	for id, state := range x.results {
		if isTerminal(state.Status) && state.CommitFrame < input.Frame {
			prior[id] = state
		}
	}
	evalState := EvalState{Frame: input.Frame, Keys: snapshot, Flags: x.flags, Results: prior}

	candidates := make([]string, 0)
	for _, branch := range x.project.SortedBranches() {
		if _, seen := x.results[branch.ID]; seen {
			continue
		}
		matched, values := EvaluateBranchCondition(branch, evalState)
		for k, v := range values {
			trace.ConditionSnapshot[k] = v
		}
		if matched {
			candidates = append(candidates, branch.ID)
		}
	}

	accepted, suppressed, adjudications, conflictErr := arbitrate(x.project, candidates, input.Frame, x.config.Arbitration)
	trace.Adjudications = append(trace.Adjudications, adjudications...)
	x.adjudications = append(x.adjudications, adjudications...)
	if conflictErr != nil {
		trace.SnapshotHash = hashSnapshot(snapshot)
		x.frames = append(x.frames, trace)
		x.frame++
		return trace, conflictErr
	}

	for _, adj := range adjudications {
		state := &BranchState{
			BranchID:     adj.SuppressedID,
			MacroID:      adj.SuppressedMacroID,
			Status:       StatusSuppressed,
			Priority:     x.project.BranchByID[adj.SuppressedID].EffectivePriority(),
			TriggerFrame: input.Frame,
			CommitFrame:  input.Frame,
			CancelReason: "suppressed_by_priority_conflict",
		}
		x.results[adj.SuppressedID] = state
		x.commits = append(x.commits, CommitRecord{Frame: input.Frame, BranchID: adj.SuppressedID, Status: StatusSuppressed})
		x.appendCancellation(&trace, Cancellation{
			Frame: input.Frame, BranchID: adj.SuppressedID, MacroID: adj.SuppressedMacroID,
			Reason: state.CancelReason, SuppressedBy: adj.WinnerID,
		})
		_ = suppressed
	}

	for _, id := range accepted {
		branch := x.project.BranchByID[id]
		trace.Triggered = append(trace.Triggered, id)
		x.results[id] = &BranchState{
			BranchID: id, MacroID: branch.MacroID, Status: StatusTriggered,
			Priority: branch.EffectivePriority(), TriggerFrame: input.Frame,
		}
		x.active[id] = &RuntimeState{BranchID: id, NextIndex: 0, NextFrame: input.Frame + branch.DelayFrames}
	}
	sort.Strings(trace.Triggered)

	x.cancelActive(&trace, snapshot)
	x.runActiveSteps(&trace, input.Frame)
	x.fillRemainingDelays(&trace, input.Frame)

	trace.SnapshotHash = hashSnapshot(snapshot)
	x.frames = append(x.frames, trace)
	x.frame++
	return trace, nil
}

func (x *Executor) appendCancellation(trace *FrameTrace, c Cancellation) {
	trace.Cancellations = append(trace.Cancellations, c)
}

func (x *Executor) cancelActive(trace *FrameTrace, snapshot KeySnapshot) {
	ids := make([]string, 0, len(x.active))
	for id := range x.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		branch := x.project.BranchByID[id]
		cancelKey := ""
		for _, key := range branch.CancelKeys {
			if snapshot.Pressed[key] {
				cancelKey = key
				break
			}
		}
		if cancelKey == "" {
			continue
		}
		state := x.results[id]
		state.Status = StatusCanceled
		state.CommitFrame = snapshot.Frame
		state.CancelReason = "cancel_key:" + cancelKey
		cancellation := Cancellation{
			Frame: snapshot.Frame, BranchID: id, MacroID: branch.MacroID,
			Reason: state.CancelReason,
		}
		trace.Cancellations = append(trace.Cancellations, cancellation)
		x.commits = append(x.commits, CommitRecord{Frame: snapshot.Frame, BranchID: id, Status: StatusCanceled})
		delete(x.active, id)
	}
}

func (x *Executor) runActiveSteps(trace *FrameTrace, frame int64) {
	ids := make([]string, 0, len(x.active))
	for id := range x.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		rt := x.active[id]
		branch := x.project.BranchByID[id]
		state := x.results[id]

		for rt.NextIndex < len(branch.Steps) && rt.NextFrame <= frame {
			idx := rt.NextIndex
			step := branch.Steps[idx]
			exec := StepExecution{
				Frame: frame, BranchID: id, MacroID: branch.MacroID,
				StepIndex: idx, StepID: step.ID, Action: step.Action, Lane: step.Lane,
			}
			trace.ExecutedSteps = append(trace.ExecutedSteps, exec)
			state.ExecutedSteps = append(state.ExecutedSteps, exec)
			state.Status = StatusRunning

			rt.NextIndex++
			if rt.NextIndex < len(branch.Steps) {
				rt.NextFrame = frame + branch.Steps[rt.NextIndex].GapFrames
			}
		}

		if rt.NextIndex >= len(branch.Steps) {
			state.Status = StatusCompleted
			state.CommitFrame = frame
			x.commits = append(x.commits, CommitRecord{Frame: frame, BranchID: id, Status: StatusCompleted})
			delete(x.active, id)
		}
	}
}

func (x *Executor) fillRemainingDelays(trace *FrameTrace, frame int64) {
	ids := make([]string, 0, len(x.active))
	for id := range x.active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		rt := x.active[id]
		branch := x.project.BranchByID[id]
		if rt.NextIndex >= len(branch.Steps) {
			continue
		}
		step := branch.Steps[rt.NextIndex]
		kind := "step_gap"
		total := step.GapFrames
		if rt.NextIndex == 0 {
			kind = "branch_delay"
			total = branch.DelayFrames
		}
		remaining := rt.NextFrame - frame
		if remaining < 0 {
			remaining = 0
		}
		trace.RemainingDelays = append(trace.RemainingDelays, DelayState{
			BranchID: id, StepID: step.ID, Kind: kind, Total: total, Remaining: remaining,
		})
	}
}

func (x *Executor) Result() *RunResult {
	branchCopy := make(map[string]*BranchState, len(x.results))
	for k, v := range x.results {
		branchCopy[k] = cloneBranchState(v)
	}
	frames := make([]FrameTrace, 0, len(x.frames))
	for _, f := range x.frames {
		frames = append(frames, cloneTrace(f))
	}
	adjudications := append([]Adjudication(nil), x.adjudications...)
	commits := append([]CommitRecord(nil), x.commits...)
	return &RunResult{
		ProjectID: x.project.ID, FormatNo: x.project.FormatNo, FormatVersion: x.project.FormatVersion,
		FrameCount: int64(len(x.frames)), Frames: frames, Adjudications: adjudications,
		Commits: commits, Branches: branchCopy,
	}
}

func (x *Executor) ExportState() *ExecutionState {
	results := make(map[string]*BranchState, len(x.results))
	for k, v := range x.results {
		results[k] = cloneBranchState(v)
	}
	active := make(map[string]*RuntimeState, len(x.active))
	for k, v := range x.active {
		cp := *v
		active[k] = &cp
	}
	frames := make([]FrameTrace, 0, len(x.frames))
	for _, f := range x.frames {
		frames = append(frames, cloneTrace(f))
	}
	return &ExecutionState{
		NextFrame: x.frame, Down: cloneBoolMap(x.down), PressedAt: cloneInt64Map(x.pressedAt),
		Flags: cloneBoolMap(x.flags), ConsumedOffset: x.consumedOffset, Results: results,
		Active: active, Frames: frames,
		Adjudications: append([]Adjudication(nil), x.adjudications...),
		Commits:       append([]CommitRecord(nil), x.commits...),
	}
}

func (x *Executor) ImportState(state *ExecutionState) error {
	if state == nil {
		return newSchemaError("$", "execution state is required")
	}
	x.frame = state.NextFrame
	x.down = cloneBoolMap(state.Down)
	x.pressedAt = cloneInt64Map(state.PressedAt)
	x.flags = cloneBoolMap(x.project.Flags)
	for k, v := range state.Flags {
		x.flags[k] = v
	}
	x.consumedOffset = state.ConsumedOffset
	x.results = make(map[string]*BranchState, len(state.Results))
	for k, v := range state.Results {
		x.results[k] = cloneBranchState(v)
	}
	x.active = make(map[string]*RuntimeState, len(state.Active))
	for k, v := range state.Active {
		cp := *v
		x.active[k] = &cp
	}
	x.frames = make([]FrameTrace, 0, len(state.Frames))
	for _, f := range state.Frames {
		x.frames = append(x.frames, cloneTrace(f))
	}
	x.adjudications = append([]Adjudication(nil), state.Adjudications...)
	x.commits = append([]CommitRecord(nil), state.Commits...)
	x.pressed = map[string]bool{}
	x.released = map[string]bool{}
	return nil
}

func isTerminal(status BranchStatus) bool {
	return status == StatusCompleted || status == StatusCanceled || status == StatusSuppressed
}

func cloneBoolMap(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
