package thermalcache

import (
	"encoding/json"
	"sort"
	"strings"
)

const CheckpointFormatVersion = 1

type CheckpointCapacity struct {
	NodeID  string `json:"node_id"`
	Bytes   int64  `json:"bytes"`
	Deleted bool   `json:"deleted"`
}

type CheckpointBatch struct {
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
	ReservedBytes    int64      `json:"reserved_bytes"`
}

type Checkpoint struct {
	FormatVersion   int                  `json:"format_version"`
	ProjectID       string               `json:"project_id"`
	SchemaVersion   string               `json:"schema_version"`
	Fingerprint     string               `json:"fingerprint"`
	CommittedTick   int64                `json:"committed_tick"`
	Revision        int64                `json:"revision"`
	CurrentAtMs     int64                `json:"current_at_ms"`
	NextTimeline    int                  `json:"next_timeline"`
	BatchSeq        int                  `json:"batch_seq"`
	AccessCursor    int                  `json:"access_cursor"`
	OpCursor        int                  `json:"op_cursor"`
	CapacityCursor  int                  `json:"capacity_cursor"`
	MigrationCursor int                  `json:"migration_cursor"`
	Objects         []ObjectState        `json:"objects"`
	Batches         []CheckpointBatch    `json:"batches"`
	Capacities      []CheckpointCapacity `json:"capacities"`
}

func (e *Engine) Checkpoint(path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.saveCheckpointLocked(path)
}

// Snapshot returns a deep, in-memory checkpoint. It is used by the incremental
// simulator to resume from an already evaluated logical tick without touching
// the source project or a database file.
func (e *Engine) Snapshot() (*Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := &Checkpoint{
		FormatVersion: CheckpointFormatVersion,
		ProjectID:     e.project.ID, SchemaVersion: e.project.SchemaVersion,
		Fingerprint: e.project.Fingerprint(), CommittedTick: e.committedTick,
		Revision: e.revision, CurrentAtMs: e.currentAtMs, NextTimeline: e.nextTimeline,
		BatchSeq: e.batchSeq, AccessCursor: e.accessCursor, OpCursor: e.opCursor,
		CapacityCursor: e.capacityCursor, MigrationCursor: e.migrationCursor,
	}
	for _, id := range e.objectOrder {
		cp.Objects = append(cp.Objects, *e.objects[id])
	}
	for _, id := range e.batchOrder {
		b := e.batches[id]
		cp.Batches = append(cp.Batches, CheckpointBatch{
			ID: b.ID, Source: b.Source, Target: b.Target,
			StartedAtMs: b.StartedAtMs, CommitAtMs: b.CommitAtMs,
			ObjectIDs:    append([]string(nil), b.ObjectIDs...),
			CommittedIDs: append([]string(nil), b.CommittedIDs...),
			Status:       b.Status, Forced: b.Forced,
			Deferred:         append([]Deferred(nil), b.Deferred...),
			RemainingObjects: b.RemainingObjects, RemainingBytes: b.RemainingBytes,
			OccupancyBefore: b.OccupancyBefore, OccupancyAfter: b.OccupancyAfter,
			ReservedBytes: b.reservedBytes,
		})
	}
	nodeIDs := make([]string, 0, len(e.capacities))
	for id := range e.capacities {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		rc := e.capacities[id]
		cp.Capacities = append(cp.Capacities, CheckpointCapacity{NodeID: id, Bytes: rc.Bytes, Deleted: rc.Deleted})
	}
	return cp, nil
}

func (e *Engine) saveCheckpointLocked(path string) error {
	db, err := openSQLiteDB(path, false)
	if err != nil {
		return err
	}
	defer db.Close()
	ddl := []string{
		`PRAGMA journal_mode=DELETE`,
		`PRAGMA synchronous=FULL`,
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK(id=1),
			format_version INTEGER NOT NULL,
			project_id TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			committed_tick INTEGER NOT NULL,
			revision INTEGER NOT NULL,
			current_at_ms INTEGER NOT NULL,
			next_timeline INTEGER NOT NULL,
			batch_seq INTEGER NOT NULL,
			access_cursor INTEGER NOT NULL,
			op_cursor INTEGER NOT NULL,
			capacity_cursor INTEGER NOT NULL,
			migration_cursor INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_objects (
			object_id TEXT PRIMARY KEY,
			tier TEXT NOT NULL,
			access_total INTEGER NOT NULL,
			last_access_ms INTEGER NOT NULL,
			last_applied_step INTEGER NOT NULL,
			score INTEGER NOT NULL,
			expired INTEGER NOT NULL,
			saturated INTEGER NOT NULL,
			overflowed INTEGER NOT NULL,
			source_version INTEGER NOT NULL,
			target_version INTEGER NOT NULL,
			pending_writes INTEGER NOT NULL,
			migrating INTEGER NOT NULL,
			target_tier TEXT NOT NULL,
			batch_id TEXT NOT NULL,
			gate_denials INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_batches (
			batch_id TEXT PRIMARY KEY,
			source_tier TEXT NOT NULL,
			target_tier TEXT NOT NULL,
			started_at_ms INTEGER NOT NULL,
			commit_at_ms INTEGER NOT NULL,
			object_ids TEXT NOT NULL,
			committed_ids TEXT NOT NULL,
			status TEXT NOT NULL,
			forced INTEGER NOT NULL,
			deferred TEXT NOT NULL,
			remaining_objects INTEGER NOT NULL,
			remaining_bytes INTEGER NOT NULL,
			occupancy_before TEXT NOT NULL,
			occupancy_after TEXT NOT NULL,
			reserved_bytes INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_capacities (
			node_id TEXT PRIMARY KEY,
			capacity_bytes INTEGER NOT NULL,
			deleted INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_write_log (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			section TEXT NOT NULL
		)`,
	}
	for _, stmt := range ddl {
		if err := db.Exec(stmt); err != nil {
			return mapSQLiteCheckpointError(err)
		}
	}
	if err := db.Exec(`BEGIN IMMEDIATE`); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = db.Exec(`ROLLBACK`)
		}
	}()
	if err := db.Exec(`DELETE FROM checkpoint_objects`); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	for _, id := range e.objectOrder {
		st := e.objects[id]
		if err := db.Exec(`INSERT INTO checkpoint_objects(
			object_id,tier,access_total,last_access_ms,last_applied_step,score,expired,
			saturated,overflowed,source_version,target_version,pending_writes,migrating,
			target_tier,batch_id,gate_denials) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			st.ID, string(st.Tier), st.AccessTotal, st.LastAccessMs, st.LastAppliedStep,
			st.Score, st.Expired, st.Saturated, st.Overflowed, st.SourceVersion,
			st.TargetVersion, st.PendingWrites, st.Migrating, string(st.TargetTier),
			st.BatchID, st.GateDenials); err != nil {
			return mapSQLiteCheckpointError(err)
		}
	}
	if err := db.Exec(`INSERT INTO checkpoint_write_log(section) VALUES('objects')`); err != nil {
		return mapSQLiteCheckpointError(err)
	}

	if err := db.Exec(`DELETE FROM checkpoint_batches`); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	for _, id := range e.batchOrder {
		b := e.batches[id]
		objectIDs, _ := json.Marshal(b.ObjectIDs)
		committedIDs, _ := json.Marshal(b.CommittedIDs)
		deferredJSON, _ := json.Marshal(b.Deferred)
		before, _ := json.Marshal(b.OccupancyBefore)
		after, _ := json.Marshal(b.OccupancyAfter)
		if err := db.Exec(`INSERT INTO checkpoint_batches(
			batch_id,source_tier,target_tier,started_at_ms,commit_at_ms,object_ids,
			committed_ids,status,forced,deferred,remaining_objects,remaining_bytes,
			occupancy_before,occupancy_after,reserved_bytes)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			b.ID, string(b.Source), string(b.Target), b.StartedAtMs, b.CommitAtMs,
			string(objectIDs), string(committedIDs), b.Status, b.Forced, string(deferredJSON),
			b.RemainingObjects, b.RemainingBytes, string(before), string(after), b.reservedBytes); err != nil {
			return mapSQLiteCheckpointError(err)
		}
	}
	if err := db.Exec(`INSERT INTO checkpoint_write_log(section) VALUES('batches')`); err != nil {
		return mapSQLiteCheckpointError(err)
	}

	if err := db.Exec(`DELETE FROM checkpoint_capacities`); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	nodeIDs := make([]string, 0, len(e.capacities))
	for id := range e.capacities {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		rc := e.capacities[id]
		if err := db.Exec(`INSERT INTO checkpoint_capacities(node_id,capacity_bytes,deleted) VALUES(?,?,?)`,
			id, rc.Bytes, rc.Deleted); err != nil {
			return mapSQLiteCheckpointError(err)
		}
	}
	if err := db.Exec(`INSERT INTO checkpoint_write_log(section) VALUES('capacities')`); err != nil {
		return mapSQLiteCheckpointError(err)
	}

	// The global committed tick is deliberately the last semantic write in the
	// transaction. Readers never observe a new tick with old object rows.
	if err := db.Exec(`INSERT OR REPLACE INTO checkpoint_meta(
		id,format_version,project_id,schema_version,fingerprint,committed_tick,revision,
		current_at_ms,next_timeline,batch_seq,access_cursor,op_cursor,capacity_cursor,migration_cursor)
		VALUES(1,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		CheckpointFormatVersion, e.project.ID, e.project.SchemaVersion,
		e.project.Fingerprint(), e.committedTick, e.revision, e.currentAtMs,
		e.nextTimeline, e.batchSeq, e.accessCursor, e.opCursor, e.capacityCursor,
		e.migrationCursor); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	if err := db.Exec(`INSERT INTO checkpoint_write_log(section) VALUES('meta')`); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	if err := db.Exec(`COMMIT`); err != nil {
		return mapSQLiteCheckpointError(err)
	}
	rollback = false
	return nil
}

func mapSQLiteCheckpointError(err error) error {
	if err == nil {
		return nil
	}
	if ErrorCode(err) != "" {
		return err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") || strings.Contains(msg, "database disk image is malformed") {
		return biz(CodeCheckpointTruncated, "checkpoint database is truncated or malformed", map[string]any{"error": err.Error()})
	}
	return biz(CodeCheckpointInvalid, "SQLite checkpoint operation failed", map[string]any{"error": err.Error()})
}

func LoadCheckpoint(path string) (*Checkpoint, error) {
	db, err := openSQLiteDB(path, true)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	quick, err := db.Query(`PRAGMA quick_check`)
	if err != nil {
		return nil, biz(CodeCheckpointTruncated, "checkpoint integrity check failed", map[string]any{"path": path, "error": err.Error()})
	}
	if len(quick) == 0 || len(quick[0]) == 0 || rowString(quick[0], 0, "") != "ok" {
		return nil, biz(CodeCheckpointTruncated, "checkpoint integrity check is not ok", map[string]any{"path": path})
	}
	metaRows, err := db.Query(`SELECT format_version,project_id,schema_version,fingerprint,
		committed_tick,revision,current_at_ms,next_timeline,batch_seq,access_cursor,
		op_cursor,capacity_cursor,migration_cursor FROM checkpoint_meta WHERE id=1`)
	if err != nil {
		return nil, biz(CodeCheckpointTruncated, "checkpoint metadata is missing or unreadable", map[string]any{"path": path, "error": err.Error()})
	}
	if len(metaRows) != 1 {
		return nil, biz(CodeCheckpointTruncated, "checkpoint metadata row is missing", map[string]any{"path": path})
	}
	m := metaRows[0]
	if len(m) != 13 {
		return nil, biz(CodeCheckpointTruncated, "checkpoint metadata row is incomplete", map[string]any{"path": path})
	}
	cp := &Checkpoint{
		FormatVersion: int(rowInt(m, 0)), ProjectID: rowString(m, 1, ""),
		SchemaVersion: rowString(m, 2, ""), Fingerprint: rowString(m, 3, ""),
		CommittedTick: rowInt(m, 4), Revision: rowInt(m, 5), CurrentAtMs: rowInt(m, 6),
		NextTimeline: int(rowInt(m, 7)), BatchSeq: int(rowInt(m, 8)),
		AccessCursor: int(rowInt(m, 9)), OpCursor: int(rowInt(m, 10)),
		CapacityCursor: int(rowInt(m, 11)), MigrationCursor: int(rowInt(m, 12)),
	}
	if cp.FormatVersion != CheckpointFormatVersion {
		return nil, biz(CodeCheckpointVersion, "unsupported checkpoint format", map[string]any{
			"found": cp.FormatVersion, "expected": CheckpointFormatVersion,
		})
	}

	objRows, err := db.Query(`SELECT object_id,tier,access_total,last_access_ms,last_applied_step,
		score,expired,saturated,overflowed,source_version,target_version,pending_writes,
		migrating,target_tier,batch_id,gate_denials FROM checkpoint_objects ORDER BY object_id`)
	if err != nil {
		return nil, biz(CodeCheckpointTruncated, "checkpoint object table is unreadable", map[string]any{"error": err.Error()})
	}
	for _, row := range objRows {
		if len(row) != 16 {
			return nil, biz(CodeCheckpointTruncated, "checkpoint object row is incomplete", nil)
		}
		cp.Objects = append(cp.Objects, ObjectState{
			ID: rowString(row, 0, ""), Tier: Tier(rowString(row, 1, "")),
			AccessTotal: rowInt(row, 2), LastAccessMs: rowInt(row, 3),
			LastAppliedStep: int(rowInt(row, 4)), Score: rowInt(row, 5),
			Expired: rowBool(row, 6), Saturated: rowBool(row, 7), Overflowed: rowBool(row, 8),
			SourceVersion: rowInt(row, 9), TargetVersion: rowInt(row, 10),
			PendingWrites: rowInt(row, 11), Migrating: rowBool(row, 12),
			TargetTier: Tier(rowString(row, 13, "")), BatchID: rowString(row, 14, ""),
			GateDenials: rowInt(row, 15),
		})
	}
	batchRows, err := db.Query(`SELECT batch_id,source_tier,target_tier,started_at_ms,commit_at_ms,
		object_ids,committed_ids,status,forced,deferred,remaining_objects,remaining_bytes,
		occupancy_before,occupancy_after,reserved_bytes FROM checkpoint_batches ORDER BY batch_id`)
	if err != nil {
		return nil, biz(CodeCheckpointTruncated, "checkpoint batch table is unreadable", map[string]any{"error": err.Error()})
	}
	for _, row := range batchRows {
		if len(row) != 15 {
			return nil, biz(CodeCheckpointTruncated, "checkpoint batch row is incomplete", nil)
		}
		b := CheckpointBatch{
			ID: rowString(row, 0, ""), Source: Tier(rowString(row, 1, "")),
			Target: Tier(rowString(row, 2, "")), StartedAtMs: rowInt(row, 3),
			CommitAtMs: rowInt(row, 4), Status: rowString(row, 7, ""),
			Forced: rowBool(row, 8), RemainingObjects: int(rowInt(row, 10)),
			RemainingBytes: rowInt(row, 11), ReservedBytes: rowInt(row, 14),
		}
		if err := json.Unmarshal([]byte(rowString(row, 5, "[]")), &b.ObjectIDs); err != nil {
			return nil, biz(CodeCheckpointTruncated, "checkpoint batch object list is corrupt", map[string]any{"batch_id": b.ID})
		}
		if err := json.Unmarshal([]byte(rowString(row, 6, "[]")), &b.CommittedIDs); err != nil {
			return nil, biz(CodeCheckpointTruncated, "checkpoint batch committed list is corrupt", map[string]any{"batch_id": b.ID})
		}
		if err := json.Unmarshal([]byte(rowString(row, 9, "[]")), &b.Deferred); err != nil {
			return nil, biz(CodeCheckpointTruncated, "checkpoint batch deferred list is corrupt", map[string]any{"batch_id": b.ID})
		}
		if err := json.Unmarshal([]byte(rowString(row, 12, "{}")), &b.OccupancyBefore); err != nil {
			return nil, biz(CodeCheckpointTruncated, "checkpoint batch occupancy is corrupt", map[string]any{"batch_id": b.ID})
		}
		if err := json.Unmarshal([]byte(rowString(row, 13, "{}")), &b.OccupancyAfter); err != nil {
			return nil, biz(CodeCheckpointTruncated, "checkpoint batch occupancy is corrupt", map[string]any{"batch_id": b.ID})
		}
		cp.Batches = append(cp.Batches, b)
	}
	capRows, err := db.Query(`SELECT node_id,capacity_bytes,deleted FROM checkpoint_capacities ORDER BY node_id`)
	if err != nil {
		return nil, biz(CodeCheckpointTruncated, "checkpoint capacity table is unreadable", map[string]any{"error": err.Error()})
	}
	for _, row := range capRows {
		if len(row) != 3 {
			return nil, biz(CodeCheckpointTruncated, "checkpoint capacity row is incomplete", nil)
		}
		cp.Capacities = append(cp.Capacities, CheckpointCapacity{
			NodeID: rowString(row, 0, ""), Bytes: rowInt(row, 1), Deleted: rowBool(row, 2),
		})
	}
	if len(cp.Objects) == 0 {
		return nil, biz(CodeCheckpointTruncated, "checkpoint has no object rows", map[string]any{"path": path})
	}
	return cp, nil
}

func rowString(row sqliteRow, idx int, fallback string) string {
	if idx < 0 || idx >= len(row) {
		return fallback
	}
	v, ok := row[idx].(string)
	if !ok {
		return fallback
	}
	return v
}

func rowInt(row sqliteRow, idx int) int64 {
	if idx < 0 || idx >= len(row) {
		return 0
	}
	v, ok := row[idx].(int64)
	if !ok {
		return 0
	}
	return v
}

func rowBool(row sqliteRow, idx int) bool {
	return rowInt(row, idx) != 0
}

func (e *Engine) RestoreCheckpoint(path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp, err := LoadCheckpoint(path)
	if err != nil {
		return err
	}
	return e.restoreCheckpointLocked(cp, true, true)
}

func NewEngineFromCheckpoint(project *Project, path string) (*Engine, error) {
	e, err := NewEngine(project)
	if err != nil {
		return nil, err
	}
	if err := e.RestoreCheckpoint(path); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) restoreCheckpointLocked(cp *Checkpoint, recoverInterrupted, validateProject bool) error {
	if cp.SchemaVersion != e.project.SchemaVersion {
		return biz(CodeCheckpointVersion, "checkpoint schema version does not match project snapshot", map[string]any{
			"checkpoint_schema": cp.SchemaVersion, "project_schema": e.project.SchemaVersion,
		})
	}
	if validateProject && cp.ProjectID != e.project.ID {
		return biz(CodeCheckpointProject, "checkpoint belongs to another project", map[string]any{
			"checkpoint_project": cp.ProjectID, "project": e.project.ID,
		})
	}
	if validateProject && cp.Fingerprint != e.project.Fingerprint() {
		return biz(CodeCheckpointVersion, "checkpoint project fingerprint does not match", map[string]any{
			"checkpoint_fingerprint": cp.Fingerprint, "project_fingerprint": e.project.Fingerprint(),
		})
	}
	if cp.NextTimeline < 0 || cp.NextTimeline > len(e.timeline) {
		return biz(CodeCheckpointTruncated, "checkpoint timeline cursor is outside the project timeline", map[string]any{
			"next_timeline": cp.NextTimeline, "timeline_len": len(e.timeline),
		})
	}
	objects := make(map[string]*ObjectState, len(cp.Objects))
	order := make([]string, 0, len(cp.Objects))
	for i := range cp.Objects {
		st := cp.Objects[i]
		if e.objectByID(st.ID) == nil {
			return biz(CodeCheckpointProject, "checkpoint contains an object absent from project", map[string]any{"object_id": st.ID})
		}
		copyState := st
		objects[st.ID] = &copyState
		order = append(order, st.ID)
	}
	if len(objects) != len(e.project.Objects) {
		return biz(CodeCheckpointTruncated, "checkpoint does not contain every project object", map[string]any{
			"checkpoint_objects": len(objects), "project_objects": len(e.project.Objects),
		})
	}
	sort.Strings(order)
	batches := make(map[string]*Batch, len(cp.Batches))
	batchOrder := make([]string, 0, len(cp.Batches))
	for i := range cp.Batches {
		cb := cp.Batches[i]
		b := &Batch{
			ID: cb.ID, Source: cb.Source, Target: cb.Target,
			StartedAtMs: cb.StartedAtMs, CommitAtMs: cb.CommitAtMs,
			ObjectIDs:    append([]string(nil), cb.ObjectIDs...),
			CommittedIDs: append([]string(nil), cb.CommittedIDs...),
			Status:       cb.Status, Forced: cb.Forced,
			Deferred:         append([]Deferred(nil), cb.Deferred...),
			RemainingObjects: cb.RemainingObjects, RemainingBytes: cb.RemainingBytes,
			OccupancyBefore: cb.OccupancyBefore, OccupancyAfter: cb.OccupancyAfter,
		}
		batches[b.ID] = b
		batchOrder = append(batchOrder, b.ID)
	}
	sort.Strings(batchOrder)

	// A checkpoint with a staged batch but no committed object is a pre-commit
	// interruption. Roll it back. A partial batch resumes from its last commit.
	for _, id := range batchOrder {
		b := batches[id]
		if b.Status != "staged" && b.Status != "partial" {
			continue
		}
		if !recoverInterrupted {
			continue
		}
		if len(b.CommittedIDs) == 0 {
			for _, oid := range b.ObjectIDs {
				if st := objects[oid]; st != nil && st.BatchID == b.ID {
					st.Migrating = false
					st.TargetTier = ""
					st.BatchID = ""
					st.PendingWrites = 0
				}
			}
			b.Status = "interrupted"
			continue
		}
		for _, oid := range b.ObjectIDs {
			if containsString(b.CommittedIDs, oid) {
				continue
			}
			st := objects[oid]
			if st == nil || !st.Migrating || st.BatchID != b.ID {
				return biz(CodeCheckpointTruncated, "partial batch and object migration state disagree", map[string]any{
					"batch_id": b.ID, "object_id": oid,
				})
			}
		}
	}

	capacities := make(map[string]*runtimeCapacity, len(cp.Capacities))
	for _, c := range cp.Capacities {
		if _, ok := e.project.nodeByID(c.NodeID); !ok {
			return biz(CodeCheckpointProject, "checkpoint contains an unknown node", map[string]any{"node_id": c.NodeID})
		}
		capacities[c.NodeID] = &runtimeCapacity{Bytes: c.Bytes, Deleted: c.Deleted}
	}
	if len(capacities) != len(e.project.Nodes) {
		return biz(CodeCheckpointTruncated, "checkpoint does not contain every node capacity", nil)
	}

	e.objects = objects
	e.objectOrder = order
	e.batches = batches
	e.batchOrder = batchOrder
	e.capacities = capacities
	e.committedTick = cp.CommittedTick
	e.revision = cp.Revision
	e.currentAtMs = cp.CurrentAtMs
	e.nextTimeline = cp.NextTimeline
	e.batchSeq = cp.BatchSeq
	e.accessCursor = cp.AccessCursor
	e.opCursor = cp.OpCursor
	e.capacityCursor = cp.CapacityCursor
	e.migrationCursor = cp.MigrationCursor
	e.reserved = map[Tier]int64{TierHot: 0, TierCold: 0}
	for _, id := range batchOrder {
		b := batches[id]
		if b.Status != "staged" && b.Status != "partial" {
			continue
		}
		var reserved int64
		for _, oid := range b.ObjectIDs {
			if containsString(b.CommittedIDs, oid) {
				continue
			}
			if obj := e.objectByID(oid); obj != nil {
				reserved += obj.SizeBytes
			}
		}
		b.reservedBytes = reserved
		e.reserved[b.Target] += reserved
	}
	e.finished = e.nextTimeline >= len(e.timeline)
	e.lastLayerMetrics = nil
	e.lastSolveMetrics = nil
	e.ticks = nil
	return nil
}
