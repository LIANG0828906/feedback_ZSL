package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"performanceexpressions/workspace/domain"

	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

type Row struct {
	Seq              int64
	BatchID          string
	SegmentID        string
	Snapshot         domain.SegmentSnapshot
	Summary          domain.ResultSummary
	PriorityVersion  string
	ContentHash      string
	ParamVersion     string
	AlgorithmVersion string
	Operations       []domain.EditOp
	OperationsHash   string
}

type ReplayBatch struct {
	BatchID    string          `json:"batch_id"`
	Operations []domain.EditOp `json:"operations"`
}

type Recovery struct {
	Project       domain.Project `json:"project"`
	BatchIDs      []string       `json:"batch_ids"`
	Rows          []Row          `json:"-"`
	Invalidated   bool           `json:"invalidated"`
	Reasons       []string       `json:"invalid_reasons,omitempty"`
	ReplayBatches []ReplayBatch  `json:"replay_batches,omitempty"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS project_meta (
	project_id TEXT PRIMARY KEY,
	base_json BLOB NOT NULL,
	base_hash TEXT NOT NULL,
	schema_version TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoints (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	batch_id TEXT NOT NULL,
	segment_id TEXT NOT NULL,
	snapshot_json BLOB NOT NULL,
	summary_json BLOB NOT NULL,
	priority_version TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	ops_json BLOB NOT NULL,
	ops_hash TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	UNIQUE(project_id, seq),
	UNIQUE(project_id, batch_id, segment_id)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_project_seq ON checkpoints(project_id, seq);
`)
	return err
}

func (s *Store) EnsureProject(ctx context.Context, project domain.Project) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	baseJSON, err := json.Marshal(project)
	if err != nil {
		return err
	}
	baseHash := domain.HashBytes(baseJSON)
	var existingHash, existingJSON string
	err = s.db.QueryRowContext(ctx, `SELECT base_hash, base_json FROM project_meta WHERE project_id = ?`, project.ID).Scan(&existingHash, &existingJSON)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO project_meta(project_id, base_json, base_hash, schema_version, created_at) VALUES(?,?,?,?,?)`,
			project.ID, baseJSON, baseHash, project.SchemaVersion, time.Now().UTC().UnixNano())
		return err
	}
	if err != nil {
		return err
	}
	if existingHash != baseHash {
		return domain.NewError(domain.CodeSchema, "base project hash changed for project %q", project.ID)
	}
	return nil
}

func (s *Store) AppendBatch(ctx context.Context, checkpoints []domain.Checkpoint) (int64, error) {
	if len(checkpoints) == 0 {
		return 0, domain.NewError(domain.CodeSchema, "empty checkpoint batch")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	projectID := checkpoints[0].ProjectID
	batchID := checkpoints[0].BatchID
	if projectID == "" || batchID == "" {
		return 0, domain.NewError(domain.CodeSchema, "checkpoint project_id and batch_id are required")
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints WHERE project_id = ? AND batch_id = ?`, projectID, batchID).Scan(&existing); err != nil {
		return 0, err
	}
	if existing > 0 {
		return 0, domain.NewError(domain.CodeDuplicate, "batch %q already exists in checkpoint store", batchID)
	}
	var maxSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM checkpoints WHERE project_id = ?`, projectID).Scan(&maxSeq); err != nil {
		return 0, err
	}
	firstSeq := maxSeq + 1
	seenSegments := make(map[string]bool, len(checkpoints))
	for i, cp := range checkpoints {
		if cp.ProjectID != projectID || cp.BatchID != batchID {
			return 0, domain.NewError(domain.CodeSchema, "checkpoint batch contains mixed project or batch ids")
		}
		if cp.SegmentID == "" || seenSegments[cp.SegmentID] {
			return 0, domain.NewError(domain.CodeSchema, "checkpoint segment id is empty or duplicated").WithSegment(cp.SegmentID)
		}
		seenSegments[cp.SegmentID] = true
		snapshotJSON, err := json.Marshal(cp.Snapshot)
		if err != nil {
			return 0, err
		}
		summaryJSON, err := json.Marshal(cp.Summary)
		if err != nil {
			return 0, err
		}
		opsJSON, err := json.Marshal(cp.Operations)
		if err != nil {
			return 0, err
		}
		expectedContentHash, err := domain.CheckpointContentHash(cp.Snapshot, cp.Summary)
		if err != nil {
			return 0, err
		}
		if cp.ContentHash == "" {
			cp.ContentHash = expectedContentHash
		}
		if cp.ContentHash != expectedContentHash {
			return 0, domain.NewError(domain.CodeSchema, "checkpoint content hash does not match snapshot").WithSegment(cp.SegmentID)
		}
		if cp.OperationsHash == "" {
			cp.OperationsHash = domain.HashBytes(opsJSON)
		}
		if cp.OperationsHash != domain.HashBytes(opsJSON) {
			return 0, domain.NewError(domain.CodeSchema, "checkpoint operations hash does not match operations").WithSegment(cp.SegmentID)
		}
		if cp.PriorityVersion != domain.PriorityVersion(domain.SegmentFromSnapshot(cp.Snapshot)) {
			return 0, domain.NewError(domain.CodeSchema, "checkpoint priority version does not match snapshot").WithSegment(cp.SegmentID)
		}
		seq := firstSeq + int64(i)
		if _, err := tx.ExecContext(ctx, `
INSERT INTO checkpoints(project_id, seq, batch_id, segment_id, snapshot_json, summary_json,
	priority_version, content_hash, param_version, algorithm_version, ops_json, ops_hash, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			projectID, seq, batchID, cp.SegmentID, snapshotJSON, summaryJSON,
			cp.PriorityVersion, cp.ContentHash, cp.ParamVersion, cp.AlgorithmVersion,
			opsJSON, cp.OperationsHash, time.Now().UTC().UnixNano()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return firstSeq, nil
}

// Load validates the checkpoint set as a whole. A single bad hash or version
// invalidates every snapshot row, which is then deleted. Valid operation logs
// are returned so the caller can replay committed batches from an empty state.
func (s *Store) Load(ctx context.Context, base domain.Project, params domain.Parameters) (Recovery, error) {
	baseJSON, err := json.Marshal(base)
	if err != nil {
		return Recovery{}, err
	}
	expectedBaseHash := domain.HashBytes(baseJSON)
	var storedBaseHash string
	err = s.db.QueryRowContext(ctx, `SELECT base_hash FROM project_meta WHERE project_id = ?`, base.ID).Scan(&storedBaseHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Recovery{Project: domain.CloneProject(base)}, nil
	}
	if err != nil {
		return Recovery{}, err
	}
	rows, err := s.loadRows(ctx, base.ID)
	if err != nil {
		return Recovery{}, err
	}
	if len(rows) == 0 {
		return Recovery{Project: domain.CloneProject(base)}, nil
	}
	recovery := Recovery{Rows: rows}
	if storedBaseHash != expectedBaseHash {
		recovery.Reasons = append(recovery.Reasons, "base project content hash mismatch")
	}
	seenSeq := make(map[int64]bool, len(rows))
	batchOrder := make([]string, 0)
	seenBatch := make(map[string]bool)
	for _, row := range rows {
		if seenSeq[row.Seq] {
			recovery.Reasons = append(recovery.Reasons, fmt.Sprintf("duplicate checkpoint sequence %d", row.Seq))
		}
		seenSeq[row.Seq] = true
		if row.PriorityVersion != domain.PriorityVersion(domain.SegmentFromSnapshot(row.Snapshot)) {
			recovery.Reasons = append(recovery.Reasons, fmt.Sprintf("priority version mismatch at seq %d segment %s", row.Seq, row.SegmentID))
		}
		if row.ParamVersion != params.ParamVersion {
			recovery.Reasons = append(recovery.Reasons, fmt.Sprintf("parameter version mismatch at seq %d: %s != %s", row.Seq, row.ParamVersion, params.ParamVersion))
		}
		if row.AlgorithmVersion != params.AlgorithmVersion {
			recovery.Reasons = append(recovery.Reasons, fmt.Sprintf("algorithm version mismatch at seq %d: %s != %s", row.Seq, row.AlgorithmVersion, params.AlgorithmVersion))
		}
		expectedContentHash, hashErr := domain.CheckpointContentHash(row.Snapshot, row.Summary)
		if hashErr != nil || row.ContentHash != expectedContentHash {
			recovery.Reasons = append(recovery.Reasons, fmt.Sprintf("content hash mismatch at seq %d segment %s", row.Seq, row.SegmentID))
		}
		rowOpsBytes, _ := json.Marshal(row.Operations)
		if row.OperationsHash != domain.HashBytes(rowOpsBytes) {
			recovery.Reasons = append(recovery.Reasons, fmt.Sprintf("operations hash mismatch at seq %d segment %s", row.Seq, row.SegmentID))
		}
		if !seenBatch[row.BatchID] {
			seenBatch[row.BatchID] = true
			batchOrder = append(batchOrder, row.BatchID)
		}
	}
	recovery.BatchIDs = batchOrder
	if len(recovery.Reasons) == 0 {
		project, err := assembleProject(base, rows)
		if err != nil {
			recovery.Reasons = append(recovery.Reasons, err.Error())
		} else {
			recovery.Project = project
			return recovery, nil
		}
	}
	recovery.Invalidated = true
	replay, replayErr := buildReplay(rows)
	if replayErr != nil {
		recovery.Reasons = append(recovery.Reasons, replayErr.Error())
	}
	recovery.ReplayBatches = replay
	s.writeMu.Lock()
	err = s.invalidateLocked(ctx, base.ID)
	s.writeMu.Unlock()
	if err != nil {
		return Recovery{}, err
	}
	recovery.Project = domain.CloneProject(base)
	return recovery, nil
}

func (s *Store) loadRows(ctx context.Context, projectID string) ([]Row, error) {
	result, err := s.db.QueryContext(ctx, `
SELECT seq, batch_id, segment_id, snapshot_json, summary_json, priority_version,
	content_hash, param_version, algorithm_version, ops_json, ops_hash
FROM checkpoints WHERE project_id = ? ORDER BY seq ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	var rows []Row
	for result.Next() {
		var row Row
		var snapshotJSON, summaryJSON, opsJSON []byte
		if err := result.Scan(&row.Seq, &row.BatchID, &row.SegmentID, &snapshotJSON, &summaryJSON,
			&row.PriorityVersion, &row.ContentHash, &row.ParamVersion, &row.AlgorithmVersion,
			&opsJSON, &row.OperationsHash); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(snapshotJSON, &row.Snapshot); err != nil {
			return nil, err
		}
		if row.Snapshot.SegmentID != row.SegmentID {
			return nil, domain.NewError(domain.CodeSchema, "snapshot segment id mismatch at seq %d", row.Seq).WithSegment(row.SegmentID)
		}
		if err := json.Unmarshal(summaryJSON, &row.Summary); err != nil {
			return nil, err
		}
		if len(opsJSON) > 0 && string(opsJSON) != "null" {
			if err := json.Unmarshal(opsJSON, &row.Operations); err != nil {
				return nil, err
			}
		}
		rows = append(rows, row)
	}
	if err := result.Err(); err != nil {
		return nil, err
	}
	return rows, nil
}

func assembleProject(base domain.Project, rows []Row) (domain.Project, error) {
	project := domain.CloneProject(base)
	truthBySegment := make(map[string]domain.Truth, len(base.Segments))
	for _, seg := range base.Segments {
		truthBySegment[seg.ID] = seg.Truth
	}
	for _, row := range rows {
		seg := domain.SegmentFromSnapshot(row.Snapshot)
		index := segmentIndex(project.Segments, seg.ID)
		if index < 0 {
			return domain.Project{}, domain.NewError(domain.CodeSchema, "checkpoint references unknown segment %q", seg.ID).WithSegment(seg.ID)
		}
		seg.Truth = truthBySegment[seg.ID]
		project.Segments[index] = seg
	}
	return project, nil
}

func buildReplay(rows []Row) ([]ReplayBatch, error) {
	var batches []ReplayBatch
	byID := make(map[string]int)
	for _, row := range rows {
		opsBytes, _ := json.Marshal(row.Operations)
		if row.OperationsHash != domain.HashBytes(opsBytes) {
			return nil, domain.NewError(domain.CodeSchema, "cannot replay tampered operations at seq %d", row.Seq).WithSegment(row.SegmentID)
		}
		index, ok := byID[row.BatchID]
		if !ok {
			index = len(batches)
			byID[row.BatchID] = index
			batches = append(batches, ReplayBatch{BatchID: row.BatchID})
		}
		batches[index].Operations = append(batches[index].Operations, row.Operations...)
	}
	return batches, nil
}

func (s *Store) Invalidate(ctx context.Context, projectID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.invalidateLocked(ctx, projectID)
}

func (s *Store) invalidateLocked(ctx context.Context, projectID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM checkpoints WHERE project_id = ?`, projectID)
	return err
}

func (s *Store) Count(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints WHERE project_id = ?`, projectID).Scan(&n)
	return n, err
}

func (s *Store) BatchIDs(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT batch_id FROM checkpoints WHERE project_id = ? GROUP BY batch_id ORDER BY MIN(seq) ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func segmentIndex(segments []domain.Segment, id string) int {
	for i := range segments {
		if segments[i].ID == id {
			return i
		}
	}
	return -1
}

func SortReplayBatches(batches []ReplayBatch) {
	sort.SliceStable(batches, func(i, j int) bool { return batches[i].BatchID < batches[j].BatchID })
}
