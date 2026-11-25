package thermalcache

import (
	"fmt"
	"sort"
	"time"
)

const (
	ConstraintBatchObjects    = "C_BATCH_OBJECTS"
	ConstraintBatchBytes      = "C_BATCH_BYTES"
	ConstraintCapacityHot     = "C_CAPACITY_HOT"
	ConstraintCapacityCold    = "C_CAPACITY_COLD"
	ConstraintBuffer          = "C_BUFFER_BACKPRESSURE"
	ConstraintActiveBatch     = "C_ACTIVE_MIGRATION"
	ConstraintForcedCapacity  = "C_FORCED_CAPACITY"
	ConstraintNodeEligibility = "C_NODE_ELIGIBILITY"
)

type Deferred struct {
	ObjectID  string            `json:"object_id"`
	Direction DecisionDirection `json:"direction"`
	Code      string            `json:"code"`
	Detail    string            `json:"detail,omitempty"`
}

type Batch struct {
	ID               string     `json:"id"`
	Source           Tier       `json:"source"`
	Target           Tier       `json:"target"`
	StartedAtMs      int64      `json:"started_at_ms"`
	CommitAtMs       int64      `json:"commit_at_ms"`
	ObjectIDs        []string   `json:"object_ids"`
	CommittedIDs     []string   `json:"committed_ids,omitempty"`
	Status           string     `json:"status"`
	Forced           bool       `json:"forced"`
	Deferred         []Deferred `json:"deferred,omitempty"`
	RemainingObjects int        `json:"remaining_objects"`
	RemainingBytes   int64      `json:"remaining_bytes"`
	OccupancyBefore  Occupancy  `json:"occupancy_before"`
	OccupancyAfter   Occupancy  `json:"occupancy_after"`
	reservedBytes    int64
}

type BatchResult struct {
	ID               string     `json:"id"`
	Source           Tier       `json:"source"`
	Target           Tier       `json:"target"`
	StartedAtMs      int64      `json:"started_at_ms"`
	CommitAtMs       int64      `json:"commit_at_ms"`
	ObjectIDs        []string   `json:"object_ids"`
	CommittedIDs     []string   `json:"committed_ids,omitempty"`
	Status           string     `json:"status"`
	Forced           bool       `json:"forced"`
	Deferred         []Deferred `json:"deferred,omitempty"`
	RemainingObjects int        `json:"remaining_objects"`
	RemainingBytes   int64      `json:"remaining_bytes"`
	OccupancyBefore  Occupancy  `json:"occupancy_before"`
	OccupancyAfter   Occupancy  `json:"occupancy_after"`
}

func (b *Batch) snapshot(current Occupancy) BatchResult {
	after := b.OccupancyAfter
	if b.Status == "staged" || b.Status == "partial" {
		after = current
	}
	return BatchResult{
		ID: b.ID, Source: b.Source, Target: b.Target,
		StartedAtMs: b.StartedAtMs, CommitAtMs: b.CommitAtMs,
		ObjectIDs:    append([]string(nil), b.ObjectIDs...),
		CommittedIDs: append([]string(nil), b.CommittedIDs...),
		Status:       b.Status, Forced: b.Forced,
		Deferred:         append([]Deferred(nil), b.Deferred...),
		RemainingObjects: b.RemainingObjects, RemainingBytes: b.RemainingBytes,
		OccupancyBefore: b.OccupancyBefore, OccupancyAfter: after,
	}
}

func (e *Engine) nextBatchIDLocked() string {
	e.batchSeq++
	return fmt.Sprintf("batch-%04d", e.batchSeq)
}

func (e *Engine) activeBatchIDsLocked() []string {
	var out []string
	for _, id := range e.batchOrder {
		b := e.batches[id]
		if b.Status == "staged" || b.Status == "partial" {
			out = append(out, id)
		}
	}
	return out
}

func (e *Engine) startBatchLocked(source, target Tier, selected []string, deferred []Deferred, forced bool, now int64) (*Batch, error) {
	if len(selected) == 0 {
		return nil, nil
	}
	occ := e.occupancyLocked()
	capacity := occ.Capacity(target)
	reservedForTarget := int64(0)
	if target == TierHot {
		reservedForTarget = occ.HotReserved
	} else {
		reservedForTarget = occ.ColdReserved
	}
	var bytes int64
	for _, id := range selected {
		obj := e.objectByID(id)
		if obj == nil {
			return nil, biz(CodeObjectNotFound, "migration object does not exist", map[string]any{"object_id": id})
		}
		var overflow bool
		bytes, overflow = saturatingAddUnsigned(bytes, obj.SizeBytes, int64(^uint64(0)>>1))
		if overflow {
			return nil, biz(CodeCapacityOverflow, "migration batch bytes overflow", map[string]any{"batch_target": target})
		}
	}
	if bytes > e.project.Limits.MaxBatchBytes {
		return nil, biz(CodeMigrationBackpressure, "migration batch exceeds byte budget", map[string]any{
			"batch_bytes": bytes, "max_batch_bytes": e.project.Limits.MaxBatchBytes,
		})
	}
	if len(selected) > e.project.Limits.MaxBatchObjects {
		return nil, biz(CodeMigrationBackpressure, "migration batch exceeds object budget", map[string]any{
			"batch_objects": len(selected), "max_batch_objects": e.project.Limits.MaxBatchObjects,
		})
	}
	used := occ.Used(target)
	if capacity < used+reservedForTarget || bytes > capacity-used-reservedForTarget {
		return nil, biz(CodeMigrationTarget, "target layer has insufficient capacity", map[string]any{
			"target": target, "capacity": capacity, "used": used,
			"reserved": reservedForTarget, "requested_bytes": bytes,
		})
	}
	for _, id := range selected {
		st := e.objects[id]
		if st == nil {
			return nil, biz(CodeObjectNotFound, "migration object does not exist", map[string]any{"object_id": id})
		}
		if st.Migrating {
			return nil, biz(CodeMigrationBackpressure, "object already has an active migration", map[string]any{
				"object_id": id, "batch_id": st.BatchID,
			})
		}
		if st.Tier != source {
			return nil, biz(CodeMigrationSource, "object is not in the declared source layer", map[string]any{
				"object_id": id, "source": source, "actual": st.Tier,
			})
		}
	}
	id := e.nextBatchIDLocked()
	b := &Batch{
		ID: id, Source: source, Target: target, StartedAtMs: now,
		CommitAtMs: now + e.project.Limits.CommitDelayMs,
		ObjectIDs:  append([]string(nil), selected...), Status: "staged",
		Forced: forced, Deferred: append([]Deferred(nil), deferred...),
		RemainingObjects: e.project.Limits.MaxBatchObjects - len(selected),
		RemainingBytes:   e.project.Limits.MaxBatchBytes - bytes,
		OccupancyBefore:  occ, reservedBytes: bytes,
	}
	e.batches[id] = b
	e.batchOrder = append(e.batchOrder, id)
	e.reserved[target] += bytes
	for _, oid := range selected {
		st := e.objects[oid]
		st.Migrating = true
		st.TargetTier = target
		st.BatchID = id
		st.TargetVersion = st.SourceVersion
	}
	b.OccupancyAfter = e.occupancyLocked()
	return b, nil
}

func (e *Engine) commitDueLocked(now int64) ([]string, []string, error) {
	var started []string
	var committed []string
	for _, id := range e.batchOrder {
		b := e.batches[id]
		if b.Status == "staged" || b.Status == "partial" {
			if b.CommitAtMs <= now {
				committed = append(committed, id)
			}
		}
	}
	for _, id := range committed {
		if err := e.commitBatchAllLocked(id); err != nil {
			return started, committed, err
		}
	}
	return started, committed, nil
}

func (e *Engine) commitBatchAllLocked(id string) error {
	b := e.batches[id]
	if b == nil {
		return biz(CodeSchemaInvalid, "unknown batch", map[string]any{"batch_id": id})
	}
	if b.Status == "committed" || b.Status == "interrupted" {
		return nil
	}
	for _, oid := range b.ObjectIDs {
		if containsString(b.CommittedIDs, oid) {
			continue
		}
		if err := e.commitOneLocked(b, oid); err != nil {
			return err
		}
	}
	if len(b.CommittedIDs) == len(b.ObjectIDs) {
		b.Status = "committed"
	} else {
		b.Status = "partial"
	}
	b.OccupancyAfter = e.occupancyLocked()
	return nil
}

func (e *Engine) commitOneLocked(b *Batch, objectID string) error {
	st := e.objects[objectID]
	if st == nil {
		return biz(CodeObjectNotFound, "batch references unknown object", map[string]any{"batch_id": b.ID, "object_id": objectID})
	}
	if !st.Migrating || st.BatchID != b.ID {
		return biz(CodeMigrationSource, "object migration state does not match batch", map[string]any{
			"batch_id": b.ID, "object_id": objectID, "object_batch": st.BatchID,
		})
	}
	obj := e.objectByID(objectID)
	if obj == nil {
		return biz(CodeObjectNotFound, "batch object does not exist", map[string]any{"object_id": objectID})
	}
	st.Tier = b.Target
	st.Migrating = false
	st.TargetTier = ""
	st.BatchID = ""
	st.TargetVersion = st.SourceVersion
	st.PendingWrites = 0
	b.CommittedIDs = append(b.CommittedIDs, objectID)
	e.reserved[b.Target] -= obj.SizeBytes
	b.reservedBytes -= obj.SizeBytes
	if b.reservedBytes < 0 {
		b.reservedBytes = 0
	}
	if e.reserved[b.Target] < 0 {
		e.reserved[b.Target] = 0
	}
	return nil
}

// CommitBatch commits all remaining objects in a staged or partially committed
// batch. A checkpoint writes this state atomically with the object rows first.
func (e *Engine) CommitBatch(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitBatchAllLocked(id)
}

// CommitBatchStep creates a durable partial commit point. Already committed
// objects remain in the target layer; the rest continue to answer from source.
func (e *Engine) CommitBatchStep(id string, objects int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if objects < 0 {
		return biz(CodeSchemaInvalid, "partial commit count is negative", map[string]any{"batch_id": id, "objects": objects})
	}
	b := e.batches[id]
	if b == nil {
		return biz(CodeSchemaInvalid, "unknown batch", map[string]any{"batch_id": id})
	}
	if b.Status == "committed" || b.Status == "interrupted" {
		return nil
	}
	remaining := len(b.ObjectIDs) - len(b.CommittedIDs)
	if objects > remaining {
		objects = remaining
	}
	committed := 0
	for _, oid := range b.ObjectIDs {
		if committed == objects {
			break
		}
		if containsString(b.CommittedIDs, oid) {
			continue
		}
		if err := e.commitOneLocked(b, oid); err != nil {
			return err
		}
		committed++
	}
	if len(b.CommittedIDs) == len(b.ObjectIDs) {
		b.Status = "committed"
	} else if len(b.CommittedIDs) > 0 {
		b.Status = "partial"
	}
	b.OccupancyAfter = e.occupancyLocked()
	return nil
}

// InterruptBatch rolls back every uncommitted staged object. Committed objects
// remain at their commit point; the source layer of uncommitted objects is
// untouched. No target capacity is charged for rolled-back objects.
func (e *Engine) InterruptBatch(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.batches[id]
	if b == nil {
		return biz(CodeSchemaInvalid, "unknown batch", map[string]any{"batch_id": id})
	}
	if b.Status == "committed" || b.Status == "interrupted" {
		return nil
	}
	for _, oid := range b.ObjectIDs {
		if containsString(b.CommittedIDs, oid) {
			continue
		}
		st := e.objects[oid]
		if st == nil || !st.Migrating || st.BatchID != b.ID {
			continue
		}
		st.Migrating = false
		st.TargetTier = ""
		st.BatchID = ""
		st.PendingWrites = 0
	}
	e.reserved[b.Target] -= b.reservedBytes
	if e.reserved[b.Target] < 0 {
		e.reserved[b.Target] = 0
	}
	b.reservedBytes = 0
	b.Status = "interrupted"
	b.OccupancyAfter = e.occupancyLocked()
	return nil
}

func containsString(in []string, value string) bool {
	for _, v := range in {
		if v == value {
			return true
		}
	}
	return false
}

func (e *Engine) solveAutoLocked(now int64, decisions map[string]Decision) ([]Backpressure, []string, map[string]string, error) {
	e.lastSolveMetrics = nil
	deferred := make(map[string]string)
	var backpressure []Backpressure
	var started []string
	if len(e.activeBatchIDsLocked()) > 0 {
		for _, id := range e.objectOrder {
			d := decisions[id]
			if d.Direction == DirectionPromote || d.Direction == DirectionDemote {
				deferred[id] = ConstraintActiveBatch
			}
		}
		e.lastSolveMetrics = append(e.lastSolveMetrics,
			LayerMetric{Layer: TierHot, Phase: "solve", Objects: 0},
			LayerMetric{Layer: TierCold, Phase: "solve", Objects: 0})
		return backpressure, started, deferred, nil
	}

	var promotes, demotes []string
	for _, id := range e.objectOrder {
		d := decisions[id]
		st := e.objects[id]
		if d.Direction == DirectionPromote && st.Tier == TierCold {
			promotes = append(promotes, id)
		}
		if d.Direction == DirectionDemote && st.Tier == TierHot {
			demotes = append(demotes, id)
		}
	}
	if len(promotes) > e.project.Limits.PerLayerBuffer {
		backpressure = append(backpressure, Backpressure{
			Layer: TierCold, Code: CodeMigrationBackpressure,
			Candidates: len(promotes), Limit: e.project.Limits.PerLayerBuffer,
		})
		for _, id := range promotes {
			deferred[id] = ConstraintBuffer
		}
		promotes = nil
	}
	if len(demotes) > e.project.Limits.PerLayerBuffer {
		backpressure = append(backpressure, Backpressure{
			Layer: TierHot, Code: CodeMigrationBackpressure,
			Candidates: len(demotes), Limit: e.project.Limits.PerLayerBuffer,
		})
		for _, id := range demotes {
			deferred[id] = ConstraintBuffer
		}
		demotes = nil
	}

	sortCandidates(e, demotes, false)
	sortCandidates(e, promotes, true)
	if len(demotes) > 0 {
		start := time.Now()
		selected, defs := e.selectCandidatesLocked(demotes, DirectionDemote)
		metric := e.layerSolveMetric(TierHot, len(demotes), start)
		e.lastSolveMetrics = append(e.lastSolveMetrics, metric)
		for _, d := range defs {
			deferred[d.ObjectID] = d.Code
		}
		if len(selected) > 0 {
			b, err := e.startBatchLocked(TierHot, TierCold, selected, defs, false, now)
			if err != nil {
				return nil, nil, nil, err
			}
			if b != nil {
				started = append(started, b.ID)
			}
		}
	} else {
		e.lastSolveMetrics = append(e.lastSolveMetrics, LayerMetric{Layer: TierHot, Phase: "solve", Objects: 0})
	}
	if len(promotes) > 0 {
		start := time.Now()
		selected, defs := e.selectCandidatesLocked(promotes, DirectionPromote)
		metric := e.layerSolveMetric(TierCold, len(promotes), start)
		e.lastSolveMetrics = append(e.lastSolveMetrics, metric)
		for _, d := range defs {
			deferred[d.ObjectID] = d.Code
		}
		if len(selected) > 0 {
			b, err := e.startBatchLocked(TierCold, TierHot, selected, defs, false, now)
			if err != nil {
				return nil, nil, nil, err
			}
			if b != nil {
				started = append(started, b.ID)
			}
		}
	} else {
		e.lastSolveMetrics = append(e.lastSolveMetrics, LayerMetric{Layer: TierCold, Phase: "solve", Objects: 0})
	}
	return backpressure, started, deferred, nil
}

func sortCandidates(e *Engine, ids []string, promote bool) {
	sort.SliceStable(ids, func(i, j int) bool {
		a := e.objectByID(ids[i])
		b := e.objectByID(ids[j])
		if a == nil || b == nil {
			return ids[i] < ids[j]
		}
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		sa := e.objects[ids[i]].Score
		sb := e.objects[ids[j]].Score
		if sa != sb {
			if promote {
				return sa > sb
			}
			return sa < sb
		}
		return ids[i] < ids[j]
	})
}

func (e *Engine) selectCandidatesLocked(ids []string, direction DecisionDirection) ([]string, []Deferred) {
	target := TierHot
	capacityCode := ConstraintCapacityHot
	if direction == DirectionDemote {
		target = TierCold
		capacityCode = ConstraintCapacityCold
	}
	occ := e.occupancyLocked()
	reserved := occ.HotReserved
	if target == TierCold {
		reserved = occ.ColdReserved
	}
	available := occ.Capacity(target) - occ.Used(target) - reserved
	if available < 0 {
		available = 0
	}
	var selected []string
	var defs []Deferred
	var selectedBytes int64
	for _, id := range ids {
		obj := e.objectByID(id)
		if obj == nil {
			defs = append(defs, Deferred{ObjectID: id, Direction: direction, Code: ConstraintForcedCapacity, Detail: "object missing"})
			continue
		}
		if len(selected) >= e.project.Limits.MaxBatchObjects {
			defs = append(defs, Deferred{
				ObjectID: id, Direction: direction, Code: ConstraintBatchObjects,
				Detail: fmt.Sprintf("max_batch_objects=%d", e.project.Limits.MaxBatchObjects),
			})
			continue
		}
		if obj.SizeBytes > e.project.Limits.MaxBatchBytes-selectedBytes {
			defs = append(defs, Deferred{
				ObjectID: id, Direction: direction, Code: ConstraintBatchBytes,
				Detail: fmt.Sprintf("max_batch_bytes=%d selected_bytes=%d object_bytes=%d",
					e.project.Limits.MaxBatchBytes, selectedBytes, obj.SizeBytes),
			})
			continue
		}
		if obj.SizeBytes > available {
			defs = append(defs, Deferred{
				ObjectID: id, Direction: direction, Code: capacityCode,
				Detail: fmt.Sprintf("available_bytes=%d object_bytes=%d", available, obj.SizeBytes),
			})
			continue
		}
		if !e.eligibleNodeForObjectLocked(target, id) {
			defs = append(defs, Deferred{
				ObjectID: id, Direction: direction, Code: ConstraintNodeEligibility,
				Detail: fmt.Sprintf("no active %s node can carry object", target),
			})
			continue
		}
		selected = append(selected, id)
		selectedBytes += obj.SizeBytes
		available -= obj.SizeBytes
	}
	return selected, defs
}

func (e *Engine) applyForcedMigrationsLocked(now int64) ([]string, error) {
	var started []string
	for e.migrationCursor < len(e.project.Migrations) {
		cmd := e.project.Migrations[e.migrationCursor]
		if cmd.AtMs > now {
			break
		}
		e.migrationCursor++
		st := e.objects[cmd.ObjectID]
		if st == nil {
			return nil, biz(CodeObjectNotFound, "migration command object is missing", map[string]any{
				"object_id": cmd.ObjectID, "at_ms": cmd.AtMs,
			})
		}
		if st.Tier != cmd.FromTier {
			return nil, biz(CodeMigrationSource, "migration command source mismatch", map[string]any{
				"object_id": cmd.ObjectID, "declared": cmd.FromTier, "actual": st.Tier,
			})
		}
		if st.Migrating {
			return nil, biz(CodeMigrationBackpressure, "migration command targets an already migrating object", map[string]any{
				"object_id": cmd.ObjectID, "batch_id": st.BatchID,
			})
		}
		if err := e.validateTargetNodeLocked(cmd); err != nil {
			return nil, err
		}
		b, err := e.startBatchLocked(cmd.FromTier, cmd.ToTier, []string{cmd.ObjectID}, nil, true, now)
		if err != nil {
			return nil, err
		}
		if b != nil {
			started = append(started, b.ID)
		}
	}
	return started, nil
}

func (e *Engine) validateTargetNodeLocked(cmd MigrationCommand) error {
	if cmd.TargetNodeID == "" {
		if e.layerCapacityLocked(cmd.ToTier) <= 0 {
			return biz(CodeMigrationTarget, "target layer has no active capacity", map[string]any{
				"object_id": cmd.ObjectID, "target": cmd.ToTier,
			})
		}
		return nil
	}
	node, ok := e.project.nodeByID(cmd.TargetNodeID)
	if !ok {
		return biz(CodeMigrationTarget, "migration target node does not exist", map[string]any{
			"object_id": cmd.ObjectID, "target_node_id": cmd.TargetNodeID,
		})
	}
	if node.Tier != cmd.ToTier {
		return biz(CodeMigrationTarget, "migration target node belongs to another tier", map[string]any{
			"object_id": cmd.ObjectID, "target_node_id": cmd.TargetNodeID,
			"node_tier": node.Tier, "target": cmd.ToTier,
		})
	}
	rc := e.capacities[cmd.TargetNodeID]
	if rc == nil || rc.Deleted || rc.Bytes <= 0 {
		return biz(CodeMigrationTarget, "migration target node capacity was deleted", map[string]any{
			"object_id": cmd.ObjectID, "target_node_id": cmd.TargetNodeID, "at_ms": cmd.AtMs,
		})
	}
	if len(node.AllowedObjects) > 0 && !containsString(node.AllowedObjects, cmd.ObjectID) {
		return biz(CodeMigrationTarget, "migration target node cannot carry object", map[string]any{
			"object_id": cmd.ObjectID, "target_node_id": cmd.TargetNodeID,
		})
	}
	return nil
}

func (e *Engine) eligibleNodeForObjectLocked(tier Tier, objectID string) bool {
	for _, node := range e.project.Nodes {
		if node.Tier != tier {
			continue
		}
		rc := e.capacities[node.ID]
		if rc == nil || rc.Deleted || rc.Bytes <= 0 {
			continue
		}
		if len(node.AllowedObjects) == 0 || containsString(node.AllowedObjects, objectID) {
			return true
		}
	}
	return false
}

// StartMigration is useful for integrators that already have a live engine.
// It uses the engine's current logical tick and the same deterministic batch
// rules used by the automatic solver.
func (e *Engine) StartMigration(objectID string, target Tier) (*BatchResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.objects[objectID]
	if st == nil {
		return nil, biz(CodeObjectNotFound, "object does not exist", map[string]any{"object_id": objectID})
	}
	if !validateTier(target) || target == st.Tier {
		return nil, biz(CodeSchemaInvalid, "invalid migration target", map[string]any{"object_id": objectID, "target": target})
	}
	now := e.currentAtMs
	b, err := e.startBatchLocked(st.Tier, target, []string{objectID}, nil, true, now)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, biz(CodeInternal, "batch unexpectedly empty", map[string]any{"object_id": objectID})
	}
	snap := b.snapshot(e.occupancyLocked())
	return &snap, nil
}

func (e *Engine) layerSolveMetric(layer Tier, objects int, start time.Time) LayerMetric {
	return LayerMetric{Layer: layer, Phase: "solve", Objects: objects, DurationNs: time.Since(start).Nanoseconds()}
}
