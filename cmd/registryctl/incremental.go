package main

import (
	"bytes"
	"encoding/json"
	"strconv"

	"serviceregistry/registry"
)

func commandIncremental(args []string) int {
	flags := newFlagSet("incremental")
	projectPath := flags.String("project", "", "project JSON file")
	instanceID := flags.String("instance", "", "instance id")
	field := flags.String("field", "", "probe_period|probe_timeout|renewal_period|lease_ttl|health_threshold|weight_scale|append_event|delete_event")
	value := flags.Int64("value", 0, "numeric value")
	value2 := flags.Int64("value2", 0, "second numeric value")
	scale := flags.String("scale", "", "weight scale")
	atTick := flags.Int64("at-tick", 0, "effective logical tick")
	committed := flags.Int64("committed-through", -2, "last committed tick (default: at-tick-1)")
	eventJSON := flags.String("event-json", "", "canonical event JSON for append_event")
	eventIndex := flags.Int("event-index", -1, "event index for delete_event")
	reportPath := flags.String("report", "", "write the incremental run result")
	if err := parseFlags(flags, args); err != nil {
		return fail(err, exitUsage)
	}
	if *projectPath == "" {
		return fail(registry.NewError(registry.CodeSchemaField, "--project is required", registry.ErrorContext{}), exitUsage)
	}
	project, err := registry.LoadProjectFile(*projectPath)
	if err != nil {
		return fail(err, exitBusiness)
	}
	beforeHash, err := project.ModelHash()
	if err != nil {
		return fail(err, exitBusiness)
	}
	previous, snapshots, err := registry.ComputeProject(project, nil, true)
	if err != nil {
		return fail(err, exitBusiness)
	}
	edit, err := buildEdit(*field, *instanceID, *atTick, *value, *value2, *scale, *eventJSON, *eventIndex)
	if err != nil {
		return fail(err, exitBusiness)
	}
	committedThrough := *committed
	if committedThrough == -2 {
		committedThrough = *atTick - 1
	}
	incremental, err := registry.IncrementalRecompute(previous, snapshots, project, edit, committedThrough)
	if err != nil {
		return fail(err, exitBusiness)
	}
	full, err := registry.RunProject(incremental.Project, registry.RunOptions{})
	if err != nil {
		return fail(err, exitBusiness)
	}
	stripped := stripInvalidation(incremental.Run.Steps)
	actualJSON, _ := json.Marshal(stripped)
	fullJSON, _ := json.Marshal(full.Steps)
	matches := bytes.Equal(actualJSON, fullJSON)
	if *reportPath != "" {
		if err := writeJSONFile(*reportPath, incremental.Run); err != nil {
			return fail(err, exitBusiness)
		}
	}
	afterHash, _ := project.ModelHash()
	report := map[string]any{
		"ok":                matches && beforeHash == afterHash,
		"command":           "incremental",
		"model_hash_before": beforeHash,
		"model_hash_after":  incremental.Run.ModelHash,
		"source_unchanged":  beforeHash == afterHash,
		"full_recompute":    matches,
		"from_tick":         incremental.FromTick,
		"invalidation":      incremental.Invalidation,
		"steps":             len(incremental.Run.Steps),
		"report":            *reportPath,
	}
	if !matches || beforeHash != afterHash {
		report["code"] = registry.CodeVerificationFailed
	}
	if err := jsonEncodeStdout(report); err != nil {
		return fail(err, exitInternal)
	}
	if !matches || beforeHash != afterHash {
		return exitBusiness
	}
	return exitOK
}

func buildEdit(field, instanceID string, atTick, value, value2 int64, scale, eventJSON string, eventIndex int) (registry.Edit, error) {
	edit := registry.Edit{
		InstanceID:    instanceID,
		EffectiveTick: atTick,
		Value:         value,
		Value2:        value2,
	}
	switch field {
	case "probe_period":
		edit.Kind = registry.EditProbePeriod
	case "probe_timeout":
		edit.Kind = registry.EditProbeTimeout
	case "renewal_period":
		edit.Kind = registry.EditRenewalPeriod
	case "lease_ttl":
		edit.Kind = registry.EditLeaseTTL
	case "health_threshold":
		edit.Kind = registry.EditHealthThreshold
	case "weight_scale":
		edit.Kind = registry.EditWeightScale
		edit.Scale = registry.WeightScale(scale)
	case "append_event":
		edit.Kind = registry.EditAppendEvent
		if eventJSON == "" {
			return edit, registry.NewError(registry.CodeEditInvalid, "--event-json is required", registry.ErrorContext{})
		}
		var event registry.Event
		if err := json.Unmarshal([]byte(eventJSON), &event); err != nil {
			return edit, registry.WrapError(registry.CodeEditInvalid, "cannot decode --event-json", registry.ErrorContext{}, err)
		}
		edit.Event = &event
	case "delete_event":
		edit.Kind = registry.EditDeleteEvent
		edit.EventIndex = eventIndex
	default:
		return edit, registry.NewError(registry.CodeEditInvalid, "unknown --field "+strconv.Quote(field), registry.ErrorContext{})
	}
	return edit, nil
}

func stripInvalidation(steps []registry.StepResult) []registry.StepResult {
	result := append([]registry.StepResult(nil), steps...)
	for index := range result {
		result[index].Invalidation = nil
	}
	return result
}
