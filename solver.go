package layercache

import (
	"fmt"
	"sort"
)

type solverObject struct {
	id       string
	size     int64
	priority int
	order    int
	layer    string
	heat     int64
	level    HeatLevel
}

type candidate struct {
	direction string
	object    solverObject
	position  int
	source    string
	target    string
}

func solveMigrationBatch(p *Project, current map[string]string, heats map[string]heatPoint, event Event) (BatchResult, error) {
	layers := p.SortedLayers()
	layerByID := make(map[string]Layer, len(layers))
	for _, layer := range layers {
		layerByID[layer.ID] = layer
	}
	occupancy := map[string]int64{}
	for _, object := range p.Objects {
		occupancy[current[object.ID]] += object.SizeBytes
	}
	batchID := event.BatchID
	if batchID == "" {
		batchID = fmt.Sprintf("auto-%d-%d", event.Order, event.AtMS)
	}
	var demoteQueue, promoteQueue []solverObject
	for _, object := range p.SortedObjects() {
		layer := layerByID[current[object.ID]]
		if event.SourceLayer != "" && current[object.ID] != event.SourceLayer {
			continue
		}
		heat := heats[object.ID]
		so := solverObject{
			id: object.ID, size: object.SizeBytes, priority: object.Priority, order: object.DeclOrder,
			layer: current[object.ID], heat: heat.score, level: heat.level,
		}
		switch layer.Kind {
		case LayerHot:
			if event.TargetLayer != "" && layerByID[event.TargetLayer].Kind != LayerCold {
				continue
			}
			if heat.score <= p.Config.MigrateColdLimit && heat.level == HeatCold {
				demoteQueue = append(demoteQueue, so)
			}
		case LayerCold:
			if event.TargetLayer != "" && layerByID[event.TargetLayer].Kind != LayerHot {
				continue
			}
			if heat.score >= p.Config.MigrateHotLimit && heat.level == HeatHot {
				promoteQueue = append(promoteQueue, so)
			}
		}
	}
	sortCandidates(demoteQueue)
	sortCandidates(promoteQueue)
	varied := make([]candidate, 0, len(demoteQueue)+len(promoteQueue))
	pos := 0
	for _, object := range demoteQueue {
		pos++
		varied = append(varied, candidate{direction: "demote", object: object, position: pos, source: object.layer, target: event.TargetLayer})
	}
	for _, object := range promoteQueue {
		pos++
		varied = append(varied, candidate{direction: "promote", object: object, position: pos, source: object.layer, target: event.TargetLayer})
	}
	projected := cloneInt64Map(occupancy)
	before := occupancySnapshot(layers, occupancy)
	var promote []Migration
	var demote []Migration
	var deferred []DeferredDecision
	var promotedBytes, demotedBytes int64
	selectedCount := 0
	selectedBytes := int64(0)
	for _, cand := range varied {
		// These checks are expressed as stable constraint identifiers so a
		// caller can explain every omitted candidate without guessing.
		if selectedCount+1 > p.Config.BatchMaxObjects {
			deferred = append(deferred, deferredFor(cand, "C_BATCH_COUNT", "batch object limit reached"))
			continue
		}
		if selectedBytes+cand.object.size > p.Config.BatchMaxBytes {
			deferred = append(deferred, deferredFor(cand, "C_BATCH_BYTES", "batch byte limit reached"))
			continue
		}
		target, constraint, detail := chooseTarget(p, layers, layerByID, projected, cand)
		if target == "" {
			deferred = append(deferred, deferredFor(cand, constraint, detail))
			continue
		}
		cand.target = target
		projected[cand.source] -= cand.object.size
		projected[target] += cand.object.size
		selectedCount++
		selectedBytes += cand.object.size
		move := Migration{ObjectID: cand.object.id, SourceLayer: cand.source, TargetLayer: target, SizeBytes: cand.object.size}
		if cand.direction == "promote" {
			promote = append(promote, move)
			promotedBytes += cand.object.size
		} else {
			demote = append(demote, move)
			demotedBytes += cand.object.size
		}
	}
	if len(promote)+len(demote) > p.Config.PerLayerBuffer {
		return BatchResult{}, bizError(CodeBackpressureBuffer, p.SchemaVersion, "migration", "pending objects exceed per-layer buffer")
	}
	after := occupancySnapshot(layers, projected)
	return BatchResult{
		BatchID: batchID, EventOrder: event.Order, TimeMS: event.AtMS,
		Promote: promote, Demote: demote, Deferred: deferred,
		PlannedBefore: before, PlannedAfter: after,
		PromotedBytes: promotedBytes, DemotedBytes: demotedBytes,
	}, nil
}

func sortCandidates(values []solverObject) {
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].priority != values[j].priority {
			return values[i].priority > values[j].priority
		}
		return values[i].order < values[j].order
	})
}

func chooseTarget(p *Project, layers []Layer, layerByID map[string]Layer, projected map[string]int64, cand candidate) (string, string, string) {
	kind := LayerHot
	if cand.direction == "demote" {
		kind = LayerCold
	}
	if cand.target != "" {
		target := layerByID[cand.target]
		if !layerAllows(&target, cand.object.id) {
			return "", "C_LAYER_ALLOWED", "target layer does not allow object"
		}
		if projected[target.ID]+cand.object.size > target.CapacityBytes {
			return "", "C_CAPACITY", "target layer capacity would be exceeded"
		}
		return target.ID, "", ""
	}
	allowedSeen := false
	for _, layer := range layers {
		if layer.Kind != kind || layer.ID == cand.source {
			continue
		}
		if !layerAllows(&layer, cand.object.id) {
			continue
		}
		allowedSeen = true
		if projected[layer.ID]+cand.object.size <= layer.CapacityBytes {
			return layer.ID, "", ""
		}
	}
	if allowedSeen {
		return "", "C_CAPACITY", "all eligible target layers lack capacity"
	}
	return "", "C_LAYER_ALLOWED", "no target layer allows object"
}

func deferredFor(cand candidate, constraint, detail string) DeferredDecision {
	return DeferredDecision{
		ObjectID: cand.object.id, Direction: cand.direction, Position: cand.position,
		Constraint: constraint, Detail: detail,
	}
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func occupancySnapshot(layers []Layer, occupancy map[string]int64) []LayerOccupancy {
	out := make([]LayerOccupancy, 0, len(layers))
	for _, layer := range layers {
		out = append(out, LayerOccupancy{LayerID: layer.ID, Occupancy: occupancy[layer.ID]})
	}
	return out
}

func directMigration(p *Project, current map[string]string, occupancy map[string]int64, event Event) (BatchResult, error) {
	layerByID := map[string]Layer{}
	for _, layer := range p.SortedLayers() {
		layerByID[layer.ID] = layer
	}
	if current[event.ObjectID] != event.SourceLayer {
		return BatchResult{}, bizError(CodeInvalidEvent, p.SchemaVersion, "events", "migrate source does not match committed object layer")
	}
	target := layerByID[event.TargetLayer]
	if !layerAllows(&target, event.ObjectID) {
		return BatchResult{}, bizError(CodeObjectNotAllowed, p.SchemaVersion, "events", "target layer does not allow object")
	}
	var object Object
	for _, candidateObject := range p.Objects {
		if candidateObject.ID == event.ObjectID {
			object = candidateObject
			break
		}
	}
	if occupancy[target.ID]+object.SizeBytes > target.CapacityBytes {
		return BatchResult{}, bizError(CodeCapacityExceeded, p.SchemaVersion, "events", "direct migration exceeds target capacity")
	}
	batchID := event.BatchID
	if batchID == "" {
		batchID = fmt.Sprintf("direct-%d-%d", event.Order, event.AtMS)
	}
	before := occupancySnapshot(p.SortedLayers(), occupancy)
	planned := cloneInt64Map(occupancy)
	planned[event.SourceLayer] -= object.SizeBytes
	planned[event.TargetLayer] += object.SizeBytes
	after := occupancySnapshot(p.SortedLayers(), planned)
	kind := "promote"
	if layerByID[event.SourceLayer].Kind == LayerHot {
		kind = "demote"
	}
	move := Migration{ObjectID: event.ObjectID, SourceLayer: event.SourceLayer, TargetLayer: event.TargetLayer, SizeBytes: object.SizeBytes}
	result := BatchResult{BatchID: batchID, EventOrder: event.Order, TimeMS: event.AtMS, PlannedBefore: before, PlannedAfter: after}
	if kind == "promote" {
		result.Promote = []Migration{move}
		result.PromotedBytes = object.SizeBytes
	} else {
		result.Demote = []Migration{move}
		result.DemotedBytes = object.SizeBytes
	}
	return result, nil
}
