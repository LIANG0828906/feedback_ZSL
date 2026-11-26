package registry

import (
	"fmt"
	"sort"
)

type EditKind string

const (
	EditProbePeriod     EditKind = "probe_period"
	EditProbeTimeout    EditKind = "probe_timeout"
	EditRenewalPeriod   EditKind = "renewal_period"
	EditLeaseTTL        EditKind = "lease_ttl"
	EditHealthThreshold EditKind = "health_threshold"
	EditWeightScale     EditKind = "weight_scale"
	EditAppendEvent     EditKind = "append_event"
	EditDeleteEvent     EditKind = "delete_event"
)

type Edit struct {
	Kind          EditKind
	InstanceID    string
	ServiceID     string
	EffectiveTick int64
	Value         int64
	Value2        int64
	Scale         WeightScale
	Event         *Event
	EventIndex    int
}

type IncrementalResult struct {
	Run          *RunResult
	Project      *Project
	FromTick     int64
	Invalidation Invalidation
}

func ApplyEdit(source *Project, edit Edit) (*Project, []string, error) {
	if source == nil {
		return nil, nil, NewError(CodeEditInvalid, "source project is nil", ErrorContext{})
	}
	project := source.Clone()
	if project == nil {
		return nil, nil, NewError(CodeInternal, "cannot clone project", ErrorContext{})
	}
	if edit.EffectiveTick < 0 {
		return nil, nil, NewError(CodeEditInvalid, "effective tick must not be negative", ErrorContext{InstanceID: edit.InstanceID, ServiceID: edit.ServiceID})
	}
	var closure []string
	switch edit.Kind {
	case EditProbePeriod, EditProbeTimeout, EditRenewalPeriod, EditLeaseTTL:
		instance, err := findEditableInstance(project, edit.InstanceID)
		if err != nil {
			return nil, nil, err
		}
		if edit.Value <= 0 || edit.Value > MaxDurationMS {
			return nil, nil, NewError(CodeInvalidDuration, "edited duration is outside the supported range", ErrorContext{InstanceID: instance.ID, ServiceID: instance.ServiceID})
		}
		if edit.EffectiveTick == 0 {
			switch edit.Kind {
			case EditProbePeriod:
				instance.ProbePeriodMS = edit.Value
			case EditProbeTimeout:
				instance.ProbeTimeoutMS = edit.Value
			case EditRenewalPeriod:
				instance.HeartbeatMS = edit.Value
			case EditLeaseTTL:
				instance.LeaseTTLMS = edit.Value
			}
		} else {
			override := ConfigOverride{InstanceID: instance.ID, FromTick: edit.EffectiveTick}
			switch edit.Kind {
			case EditProbePeriod:
				override.ProbePeriodMS = int64PtrCopy(edit.Value)
			case EditProbeTimeout:
				override.ProbeTimeoutMS = int64PtrCopy(edit.Value)
			case EditRenewalPeriod:
				override.HeartbeatMS = int64PtrCopy(edit.Value)
			case EditLeaseTTL:
				override.LeaseTTLMS = int64PtrCopy(edit.Value)
			}
			project.Overrides = append(project.Overrides, override)
		}
		closure = instanceClosure(project, *instance, edit.Kind)
	case EditHealthThreshold:
		instance, err := findEditableInstance(project, edit.InstanceID)
		if err != nil {
			return nil, nil, err
		}
		if edit.Value <= 0 || edit.Value > 1_000_001 || edit.Value2 <= 0 || edit.Value2 > 1_000_001 {
			return nil, nil, NewError(CodeInvalidThreshold, "edited thresholds must be positive", ErrorContext{InstanceID: instance.ID, ServiceID: instance.ServiceID})
		}
		if edit.EffectiveTick == 0 {
			instance.SuccessThreshold = int(edit.Value)
			instance.FailureThreshold = int(edit.Value2)
		} else {
			success := int(edit.Value)
			failure := int(edit.Value2)
			project.Overrides = append(project.Overrides, ConfigOverride{
				InstanceID:       instance.ID,
				FromTick:         edit.EffectiveTick,
				SuccessThreshold: &success,
				FailureThreshold: &failure,
			})
		}
		closure = instanceClosure(project, *instance, edit.Kind)
	case EditWeightScale:
		instance, err := findEditableInstance(project, edit.InstanceID)
		if err != nil {
			return nil, nil, err
		}
		denominator := edit.Scale.Denominator()
		if denominator == 0 {
			return nil, nil, NewError(CodeInvalidWeightScale, "unknown edit weight scale", ErrorContext{InstanceID: instance.ID, ServiceID: instance.ServiceID})
		}
		effective := EffectiveInstance(project, *instance, edit.EffectiveTick)
		weightPPM, err := effective.NormalizedWeightPPM()
		if err != nil {
			return nil, nil, err
		}
		newWeight := weightPPM * denominator / 1_000_000
		if edit.EffectiveTick == 0 {
			instance.WeightScale = edit.Scale
			instance.Weight = newWeight
		} else {
			scale := edit.Scale
			project.Overrides = append(project.Overrides, ConfigOverride{
				InstanceID:  instance.ID,
				FromTick:    edit.EffectiveTick,
				Weight:      &newWeight,
				WeightScale: &scale,
			})
		}
		closure = instanceClosure(project, *instance, edit.Kind)
	case EditAppendEvent:
		if edit.Event == nil {
			return nil, nil, NewError(CodeEditInvalid, "append event requires an event", ErrorContext{})
		}
		event := *edit.Event
		if event.Success != nil {
			value := *event.Success
			event.Success = &value
		}
		if event.AtMS == 0 && edit.EffectiveTick > 0 {
			event.AtMS = edit.EffectiveTick * project.TickMS
		}
		project.Events = append(project.Events, event)
		sort.SliceStable(project.Events, func(i, j int) bool {
			return project.Events[i].AtMS < project.Events[j].AtMS
		})
		closure = eventClosure(project, event)
	case EditDeleteEvent:
		if edit.EventIndex < 0 || edit.EventIndex >= len(project.Events) {
			return nil, nil, NewError(CodeEditInvalid, "event index is outside the project", ErrorContext{})
		}
		deleted := project.Events[edit.EventIndex]
		project.Events = append(project.Events[:edit.EventIndex], project.Events[edit.EventIndex+1:]...)
		closure = eventClosure(project, deleted)
	default:
		return nil, nil, NewError(CodeEditInvalid, "unknown edit kind", ErrorContext{InstanceID: edit.InstanceID, ServiceID: edit.ServiceID})
	}
	if err := validateProject(project); err != nil {
		return nil, nil, err
	}
	sort.Strings(closure)
	return project, closure, nil
}

func int64PtrCopy(value int64) *int64 {
	return &value
}

func findEditableInstance(project *Project, instanceID string) (*Instance, error) {
	for index := range project.Instances {
		if project.Instances[index].ID == instanceID {
			return &project.Instances[index], nil
		}
	}
	return nil, NewError(CodeUnknownInstance, "edit references an unknown instance", ErrorContext{InstanceID: instanceID})
}

func instanceClosure(project *Project, instance Instance, kind EditKind) []string {
	values := []string{
		"instance:" + instance.ID,
		"service:" + instance.ServiceID,
		"catalog:" + instance.ServiceID,
	}
	switch kind {
	case EditProbePeriod, EditProbeTimeout:
		values = append(values, "scheduler:global")
		for _, candidate := range project.Instances {
			values = append(values, "scheduler:instance:"+candidate.ID)
		}
		for _, service := range project.Services {
			values = append(values, "catalog:"+service.ID)
		}
	case EditLeaseTTL, EditRenewalPeriod, EditHealthThreshold, EditWeightScale:
		for _, candidate := range project.Instances {
			if candidate.ServiceID == instance.ServiceID {
				values = append(values, "instance:"+candidate.ID)
			}
		}
	}
	return uniqueSorted(values)
}

func eventClosure(project *Project, event Event) []string {
	values := []string{"event:" + event.Kind.String()}
	if event.InstanceID != "" {
		if instance, ok := project.InstanceByID(event.InstanceID); ok {
			values = append(values, "instance:"+instance.ID, "service:"+instance.ServiceID, "catalog:"+instance.ServiceID)
			values = append(values, "scheduler:instance:"+instance.ID)
		}
	}
	if event.ServiceID != "" {
		values = append(values, "service:"+event.ServiceID, "catalog:"+event.ServiceID)
	}
	return uniqueSorted(values)
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func (k EventKind) String() string {
	return string(k)
}

// IncrementalRecompute reuses every step through CommittedThroughTick, resumes
// from the cached snapshot at FromTick-1, and recomputes only the suffix.
func IncrementalRecompute(
	previous *RunResult,
	previousSnapshots []Snapshot,
	source *Project,
	edit Edit,
	committedThroughTick int64,
) (*IncrementalResult, error) {
	if previous == nil || source == nil {
		return nil, NewError(CodeEditInvalid, "previous run and edited project are required", ErrorContext{})
	}
	if len(previousSnapshots) == 0 && len(previous.Steps) > 0 {
		return nil, NewError(CodeEditInvalid, "cached steps require cached snapshots", ErrorContext{})
	}
	if committedThroughTick < -1 {
		return nil, NewError(CodeEditInvalid, "committed boundary must be at least -1", ErrorContext{})
	}
	fromTick, err := editStartTick(source, edit)
	if err != nil {
		return nil, err
	}
	if fromTick <= committedThroughTick {
		tick := fromTick
		return nil, NewError(CodeEditCommittedHistory, "edit would change a committed logical moment", ErrorContext{InstanceID: edit.InstanceID, ServiceID: edit.ServiceID, Tick: &tick})
	}
	project, closure, err := ApplyEdit(source, edit)
	if err != nil {
		return nil, err
	}
	editedMaxTick := project.MaxEventTick()
	prefixNeeded := fromTick
	if prefixNeeded > editedMaxTick+1 {
		prefixNeeded = editedMaxTick + 1
	}
	prefixSteps := append([]StepResult(nil), previous.Steps...)
	prefixSnapshots := append([]Snapshot(nil), previousSnapshots...)
	if prefixNeeded > int64(len(prefixSnapshots)) {
		var extensionEngine *Engine
		if len(prefixSnapshots) == 0 {
			extensionEngine, err = NewEngine(source)
		} else {
			boundary := prefixSnapshots[len(prefixSnapshots)-1]
			extensionEngine, err = RestoreEngine(source, &boundary)
		}
		if err != nil {
			return nil, err
		}
		extended, err := extensionEngine.run(prefixNeeded-1, true)
		if err != nil {
			return nil, err
		}
		prefixSteps = append(prefixSteps, extended.result.Steps...)
		prefixSnapshots = append(prefixSnapshots, extended.snapshots...)
	}
	if prefixNeeded > int64(len(prefixSnapshots)) {
		return nil, NewError(CodeEditInvalid, "missing cached snapshot for incremental boundary", ErrorContext{InstanceID: edit.InstanceID, ServiceID: edit.ServiceID})
	}

	computedSteps := []StepResult{}
	if fromTick <= editedMaxTick {
		var engine *Engine
		if fromTick == 0 {
			engine, err = NewEngine(project)
		} else {
			boundary := prefixSnapshots[fromTick-1]
			hash, hashErr := project.ModelHash()
			if hashErr != nil {
				return nil, hashErr
			}
			// The state at the boundary is compatible; the model hash intentionally
			// changes because this is an edited project.
			boundary.ModelHash = hash
			boundary.ProjectVersion = project.ProjectVersion
			boundary.NextTick = fromTick
			engine, err = RestoreEngine(project, &boundary)
		}
		if err != nil {
			return nil, err
		}
		computed, err := engine.run(editedMaxTick, false)
		if err != nil {
			return nil, err
		}
		computedSteps = computed.result.Steps
	}
	if prefixNeeded > int64(len(prefixSteps)) {
		return nil, NewError(CodeEditInvalid, "step prefix is shorter than the edit boundary", ErrorContext{})
	}
	reused := int(prefixNeeded)
	if reused > len(previous.Steps) {
		reused = len(previous.Steps)
	}
	modelHash, err := project.ModelHash()
	if err != nil {
		return nil, err
	}
	invalidation := Invalidation{
		Source:     editSource(edit),
		Reason:     string(edit.Kind),
		Closure:    closure,
		FromSeq:    int(fromTick),
		FromAtMS:   fromTick * project.TickMS,
		Reused:     reused,
		Extended:   int(prefixNeeded) - reused,
		Recomputed: len(computedSteps),
	}
	result := &RunResult{
		ProjectVersion: project.ProjectVersion,
		ModelHash:      modelHash,
		SchemaVersion:  project.SchemaVersion,
		Steps:          append([]StepResult(nil), prefixSteps[:prefixNeeded]...),
	}
	for index := range computedSteps {
		step := computedSteps[index]
		metadata := invalidation
		step.Invalidation = &metadata
		result.Steps = append(result.Steps, step)
	}
	return &IncrementalResult{
		Run:          result,
		Project:      project,
		FromTick:     fromTick,
		Invalidation: invalidation,
	}, nil
}

func editStartTick(project *Project, edit Edit) (int64, error) {
	switch edit.Kind {
	case EditAppendEvent:
		if edit.Event == nil {
			return 0, NewError(CodeEditInvalid, "append event requires an event", ErrorContext{})
		}
		if edit.Event.AtMS > 0 {
			return edit.Event.AtMS / project.TickMS, nil
		}
		return edit.EffectiveTick, nil
	case EditDeleteEvent:
		if edit.EventIndex < 0 || edit.EventIndex >= len(project.Events) {
			return 0, NewError(CodeEditInvalid, "event index is outside the project", ErrorContext{})
		}
		return project.Events[edit.EventIndex].AtMS / project.TickMS, nil
	default:
		return edit.EffectiveTick, nil
	}
}

func editSource(edit Edit) string {
	switch edit.Kind {
	case EditAppendEvent, EditDeleteEvent:
		return fmt.Sprintf("event:%s", edit.Kind)
	default:
		return fmt.Sprintf("instance:%s:%s", edit.InstanceID, edit.Kind)
	}
}
