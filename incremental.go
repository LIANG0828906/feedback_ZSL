package layercache

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type EditKind string

const (
	EditAccessCount   EditKind = "access_count"
	EditLayerCapacity EditKind = "layer_capacity"
	EditAppendEvent   EditKind = "append_event"
	EditDeleteEvent   EditKind = "delete_event"
)

type Edit struct {
	Kind       EditKind `json:"kind"`
	ObjectID   string   `json:"objectId,omitempty"`
	AtMS       int64    `json:"atMs,omitempty"`
	Count      *int64   `json:"count,omitempty"`
	LayerID    string   `json:"layerId,omitempty"`
	Capacity   *int64   `json:"capacity,omitempty"`
	Event      *Event   `json:"event,omitempty"`
	EventIndex int      `json:"eventIndex,omitempty"`
}

func (e *Engine) ApplyEdit(ctx context.Context, edit Edit, opts RunOptions) (*IncrementalResult, error) {
	if e.lastRun == nil {
		if _, err := e.Run(ctx, opts); err != nil {
			return nil, err
		}
	}
	oldRun := e.lastRun
	oldProject := e.project.DeepCopy()
	candidate := e.project.DeepCopy()
	startMS, affectedObjects, affectedLayers, source, err := applyEdit(candidate, edit)
	if err != nil {
		return nil, err
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	nextEngine, err := NewEngine(candidate)
	if err != nil {
		return nil, err
	}
	newRun, err := nextEngine.Run(ctx, opts)
	if err != nil {
		return nil, err
	}
	report, mergedRun := buildInvalidationReport(oldRun, newRun, startMS, affectedObjects, affectedLayers, source)
	e.project = candidate
	e.records = collectAccessRecords(candidate)
	e.lastRun = mergedRun
	_ = oldProject
	return &IncrementalResult{Run: *mergedRun, Report: report}, nil
}

func applyEdit(project *Project, edit Edit) (int64, []string, []string, string, error) {
	switch edit.Kind {
	case EditAccessCount:
		if edit.Count == nil || *edit.Count < 0 {
			return 0, nil, nil, "", bizError(CodeEditInvalid, project.SchemaVersion, "edit.count", "access count edit requires non-negative count")
		}
		objectIndex := -1
		for i := range project.Objects {
			if project.Objects[i].ID == edit.ObjectID {
				objectIndex = i
				break
			}
		}
		if objectIndex < 0 {
			return 0, nil, nil, "", bizError(CodeMissingObject, project.SchemaVersion, "edit.objectId", "unknown object")
		}
		sampleIndex := -1
		for j := range project.Objects[objectIndex].Samples {
			if project.Objects[objectIndex].Samples[j].AtMS == edit.AtMS {
				sampleIndex = j
				break
			}
		}
		if sampleIndex < 0 {
			return 0, nil, nil, "", bizError(CodeEditInvalid, project.SchemaVersion, "edit.atMs", "object has no access sample at this time")
		}
		project.Objects[objectIndex].Samples[sampleIndex].Count = *edit.Count
		// An explicit edit invalidates the old watermark snapshot. The engine
		// then follows the declared derived-watermark degradation rule.
		project.Objects[objectIndex].Samples[sampleIndex].Watermark = nil
		layers := layerIDsForObject(project, edit.ObjectID)
		return edit.AtMS, []string{edit.ObjectID}, layers, fmt.Sprintf("access-count:%s@%d", edit.ObjectID, edit.AtMS), nil
	case EditLayerCapacity:
		if edit.Capacity == nil || *edit.Capacity < 0 {
			return 0, nil, nil, "", bizError(CodeNegativeCapacity, project.SchemaVersion, "edit.capacity", "layer capacity edit requires non-negative value")
		}
		layerIndex := -1
		for i := range project.Layers {
			if project.Layers[i].ID == edit.LayerID {
				layerIndex = i
				break
			}
		}
		if layerIndex < 0 {
			return 0, nil, nil, "", bizError(CodeMissingLayer, project.SchemaVersion, "edit.layerId", "unknown layer")
		}
		occupancy := int64(0)
		for _, object := range project.Objects {
			if object.InitialLayer == edit.LayerID {
				occupancy += object.SizeBytes
			}
		}
		if *edit.Capacity < occupancy {
			return 0, nil, nil, "", bizError(CodeCapacityExceeded, project.SchemaVersion, "edit.capacity", "capacity is below initial occupancy")
		}
		project.Layers[layerIndex].CapacityBytes = *edit.Capacity
		return 0, nil, []string{edit.LayerID}, fmt.Sprintf("layer-capacity:%s", edit.LayerID), nil
	case EditAppendEvent:
		if edit.Event == nil {
			return 0, nil, nil, "", bizError(CodeEditInvalid, project.SchemaVersion, "edit.event", "append event requires event")
		}
		event := *edit.Event
		if event.Order == 0 {
			maxOrder := 0
			for _, existing := range project.Events {
				if existing.Order > maxOrder {
					maxOrder = existing.Order
				}
			}
			event.Order = maxOrder + 1
		}
		project.Events = append(project.Events, event)
		sort.SliceStable(project.Events, func(i, j int) bool {
			if project.Events[i].AtMS != project.Events[j].AtMS {
				return project.Events[i].AtMS < project.Events[j].AtMS
			}
			return project.Events[i].Order < project.Events[j].Order
		})
		objects, layers := eventDependency(project, event)
		if event.Kind == EventAccess {
			invalidateObjectWatermarks(project, event.ObjectID)
		}
		return event.AtMS, objects, layers, fmt.Sprintf("append-event:%d", event.Order), nil
	case EditDeleteEvent:
		if edit.EventIndex < 0 || edit.EventIndex >= len(project.Events) {
			return 0, nil, nil, "", bizError(CodeEditInvalid, project.SchemaVersion, "edit.eventIndex", "event index is out of range")
		}
		event := project.Events[edit.EventIndex]
		project.Events = append(project.Events[:edit.EventIndex], project.Events[edit.EventIndex+1:]...)
		objects, layers := eventDependency(project, event)
		if event.Kind == EventAccess {
			invalidateObjectWatermarks(project, event.ObjectID)
		}
		return event.AtMS, objects, layers, fmt.Sprintf("delete-event:%d", event.Order), nil
	default:
		return 0, nil, nil, "", bizError(CodeEditInvalid, project.SchemaVersion, "edit.kind", "unknown edit kind")
	}
}

func layerIDsForObject(project *Project, objectID string) []string {
	var out []string
	for _, layer := range project.Layers {
		if layerAllows(&layer, objectID) {
			// All layers allowing an object are plausible propagation targets.
			out = append(out, layer.ID)
		}
	}
	return out
}

func eventDependency(project *Project, event Event) ([]string, []string) {
	if event.ObjectID != "" {
		return []string{event.ObjectID}, layerIDsForObject(project, event.ObjectID)
	}
	var layers []string
	for _, layer := range project.Layers {
		layers = append(layers, layer.ID)
	}
	if event.SourceLayer != "" {
		layers = append(layers, event.SourceLayer)
	}
	if event.TargetLayer != "" {
		layers = append(layers, event.TargetLayer)
	}
	return nil, uniqueStrings(layers)
}

func invalidateObjectWatermarks(project *Project, objectID string) {
	for i := range project.Objects {
		if project.Objects[i].ID != objectID {
			continue
		}
		for j := range project.Objects[i].Samples {
			project.Objects[i].Samples[j].Watermark = nil
		}
	}
}

func buildInvalidationReport(oldRun, newRun *RunResult, startMS int64, affectedObjects, affectedLayers []string, source string) (InvalidationReport, *RunResult) {
	oldByTime := map[int64]TickResult{}
	for _, tick := range oldRun.Ticks {
		oldByTime[tick.TimeMS] = tick
	}
	report := InvalidationReport{
		Source: source, StartTimeMS: startMS,
		AffectedObjects: uniqueStrings(affectedObjects), AffectedLayers: uniqueStrings(affectedLayers),
	}
	merged := &RunResult{ProjectKey: newRun.ProjectKey, Metrics: newRun.Metrics}
	closureSeen := map[string]bool{}
	addEdge := func(from, to, kind string) {
		key := from + "\x00" + to + "\x00" + kind
		if closureSeen[key] {
			return
		}
		closureSeen[key] = true
		report.Closure = append(report.Closure, DependencyEdge{From: from, To: to, Kind: kind})
	}
	for _, object := range affectedObjects {
		addEdge(source, "object:"+object, "edited-object")
	}
	for _, layer := range affectedLayers {
		addEdge(source, "layer:"+layer, "edited-layer")
	}
	for _, tick := range newRun.Ticks {
		invalidation := TickInvalidation{TimeMS: tick.TimeMS}
		old, ok := oldByTime[tick.TimeMS]
		if ok && old.Revision == tick.Revision {
			// Content-addressed reuse: the cached tick is safe even if it
			// follows the edited instant because its dependency closure did
			// not change.
			merged.Ticks = append(merged.Ticks, old)
			invalidation.Reused = true
			report.Ticks = append(report.Ticks, invalidation)
			continue
		}
		merged.Ticks = append(merged.Ticks, tick)
		invalidation.Recomputed = true
		if tick.TimeMS >= startMS {
			invalidation.Reasons = []string{source}
		}
		if ok {
			changedObjects := changedObjectIDs(old, tick)
			changedLayers := changedLayerIDs(old, tick)
			invalidation.ChangedObjects = changedObjects
			invalidation.ChangedLayers = changedLayers
			for _, object := range changedObjects {
				addEdge("object:"+object, fmt.Sprintf("tick:%d", tick.TimeMS), "recomputed-heat-or-migration")
				report.AffectedObjects = append(report.AffectedObjects, object)
			}
			for _, layer := range changedLayers {
				addEdge("layer:"+layer, fmt.Sprintf("tick:%d", tick.TimeMS), "occupancy-or-batch")
				report.AffectedLayers = append(report.AffectedLayers, layer)
			}
		}
		report.Ticks = append(report.Ticks, invalidation)
	}
	report.AffectedObjects = uniqueStrings(report.AffectedObjects)
	report.AffectedLayers = uniqueStrings(report.AffectedLayers)
	merged.Interrupted = newRun.Interrupted
	merged.LastCommittedTick = newRun.LastCommittedTick
	merged.LastCommittedBatch = newRun.LastCommittedBatch
	return report, merged
}

func changedObjectIDs(a, b TickResult) []string {
	oldObjects := map[string]ObjectSnapshot{}
	for _, object := range a.Objects {
		oldObjects[object.ID] = object
	}
	var out []string
	for _, object := range b.Objects {
		old, ok := oldObjects[object.ID]
		if !ok || objectFingerprint(old) != objectFingerprint(object) {
			out = append(out, object.ID)
		}
	}
	return out
}

func objectFingerprint(object ObjectSnapshot) string {
	raw, _ := json.Marshal(object)
	return string(raw)
}

func changedLayerIDs(a, b TickResult) []string {
	oldLayers := map[string]string{}
	for _, layer := range a.Layers {
		raw, _ := json.Marshal(layer)
		oldLayers[layer.ID] = string(raw)
	}
	var out []string
	for _, layer := range b.Layers {
		raw, _ := json.Marshal(layer)
		if oldLayers[layer.ID] != string(raw) {
			out = append(out, layer.ID)
		}
	}
	return out
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
