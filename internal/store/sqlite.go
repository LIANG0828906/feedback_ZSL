package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"resource-workbench/internal/workbench"
)

type SQLiteStore struct {
	db *sql.DB
}

func Open(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, fmt.Errorf("sqlite path is empty")
	}
	if path != ":memory:" && path != "file::memory:?cache=shared" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA busy_timeout=5000`,
		`PRAGMA foreign_keys=ON`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("configure sqlite: %w", err)
		}
	}
	store := &SQLiteStore{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteStore) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS snapshot_meta (
			snapshot_id TEXT PRIMARY KEY,
			region_id TEXT NOT NULL,
			base_hash TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			state_json TEXT NOT NULL,
			state_hash TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS zone_checkpoints (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_id TEXT NOT NULL,
			zone_id TEXT NOT NULL,
			topology_order INTEGER NOT NULL,
			generation INTEGER NOT NULL,
			commit_seq INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			sample_indices TEXT NOT NULL,
			estimate_json TEXT NOT NULL,
			point_summary TEXT NOT NULL,
			heat_summary TEXT NOT NULL,
			result_json TEXT NOT NULL,
			is_current INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			UNIQUE(snapshot_id, zone_id, generation)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS zone_checkpoints_current
			ON zone_checkpoints(snapshot_id, zone_id) WHERE is_current = 1`,
		`CREATE INDEX IF NOT EXISTS zone_checkpoints_order
			ON zone_checkpoints(snapshot_id, topology_order, zone_id)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate sqlite: %w", err)
		}
	}
	return nil
}

func (s *SQLiteStore) LoadSnapshot(_ string) (*workbench.SnapshotMeta, []workbench.ZoneCheckpoint, error) {
	row := s.db.QueryRow(`SELECT snapshot_id, region_id, base_hash, algorithm_version, state_json, state_hash, updated_at
		FROM snapshot_meta LIMIT 1`)
	var meta workbench.SnapshotMeta
	if err := row.Scan(&meta.SnapshotID, &meta.RegionID, &meta.BaseHash, &meta.AlgorithmVersion,
		&meta.StateJSON, &meta.StateHash, &meta.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("load snapshot meta: %w", err)
	}
	rows, err := s.db.Query(`SELECT zone_id, topology_order, generation, commit_seq, content_hash,
		param_version, algorithm_version, sample_indices, estimate_json, point_summary,
		heat_summary, result_json, is_current
		FROM zone_checkpoints WHERE snapshot_id = ? AND is_current = 1
		ORDER BY topology_order, zone_id`, meta.SnapshotID)
	if err != nil {
		return nil, nil, fmt.Errorf("load checkpoints: %w", err)
	}
	defer rows.Close()
	var checkpoints []workbench.ZoneCheckpoint
	for rows.Next() {
		var cp workbench.ZoneCheckpoint
		var current int
		if err := rows.Scan(&cp.ZoneID, &cp.TopologyOrder, &cp.Generation, &cp.CommitSeq,
			&cp.ContentHash, &cp.ParamVersion, &cp.AlgorithmVersion, &cp.SampleIndices,
			&cp.EstimateJSON, &cp.PointSummary, &cp.HeatSummary, &cp.ResultJSON, &current); err != nil {
			return nil, nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		cp.IsCurrent = current == 1
		checkpoints = append(checkpoints, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate checkpoints: %w", err)
	}
	return &meta, checkpoints, nil
}

func (s *SQLiteStore) SaveSnapshot(meta workbench.SnapshotMeta) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin snapshot save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	err = tx.QueryRow(`SELECT snapshot_id FROM snapshot_meta LIMIT 1`).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("read existing snapshot: %w", err)
	}
	if existing != "" && existing != meta.SnapshotID {
		if _, err := tx.Exec(`DELETE FROM zone_checkpoints WHERE snapshot_id = ?`, existing); err != nil {
			return fmt.Errorf("clear previous checkpoints: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM snapshot_meta WHERE snapshot_id = ?`, existing); err != nil {
			return fmt.Errorf("clear previous snapshot: %w", err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if meta.UpdatedAt == "" {
		meta.UpdatedAt = now
	}
	_, err = tx.Exec(`INSERT INTO snapshot_meta
		(snapshot_id, region_id, base_hash, algorithm_version, state_json, state_hash, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(snapshot_id) DO UPDATE SET
			region_id=excluded.region_id,
			base_hash=excluded.base_hash,
			algorithm_version=excluded.algorithm_version,
			state_json=excluded.state_json,
			state_hash=excluded.state_hash,
			updated_at=excluded.updated_at`,
		meta.SnapshotID, meta.RegionID, meta.BaseHash, meta.AlgorithmVersion,
		meta.StateJSON, meta.StateHash, meta.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save snapshot meta: %w", err)
	}
	if meta.SnapshotID != existing && existing != "" {
		// Rows belonging to a different base content hash are never reused.
		if _, err := tx.Exec(`DELETE FROM zone_checkpoints WHERE snapshot_id = ?`, existing); err != nil {
			return fmt.Errorf("clear stale checkpoints: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot meta: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteSnapshot(snapshotID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin snapshot delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM zone_checkpoints WHERE snapshot_id = ?`, snapshotID); err != nil {
		return fmt.Errorf("delete checkpoints: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM snapshot_meta WHERE snapshot_id = ?`, snapshotID); err != nil {
		return fmt.Errorf("delete snapshot meta: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit snapshot delete: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SaveState(snapshotID, stateJSON, stateHash string) error {
	result, err := s.db.Exec(`UPDATE snapshot_meta SET state_json = ?, state_hash = ?, updated_at = ?
		WHERE snapshot_id = ?`, stateJSON, stateHash, time.Now().UTC().Format(time.RFC3339Nano), snapshotID)
	if err != nil {
		return fmt.Errorf("save workspace state: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("verify workspace state save: %w", err)
	}
	if affected == 0 {
		return fmt.Errorf("snapshot %s does not exist", snapshotID)
	}
	return nil
}

func (s *SQLiteStore) CommitZone(
	snapshotID, stateJSON, stateHash string,
	checkpoint workbench.ZoneCheckpoint,
) (workbench.ZoneCheckpoint, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return checkpoint, fmt.Errorf("begin zone commit: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var nextSeq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(commit_seq), 0) + 1 FROM zone_checkpoints
		WHERE snapshot_id = ?`, snapshotID).Scan(&nextSeq); err != nil {
		return checkpoint, fmt.Errorf("allocate commit sequence: %w", err)
	}
	var generation int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(generation), 0) + 1 FROM zone_checkpoints
		WHERE snapshot_id = ? AND zone_id = ?`, snapshotID, checkpoint.ZoneID).Scan(&generation); err != nil {
		return checkpoint, fmt.Errorf("allocate generation: %w", err)
	}
	if _, err := tx.Exec(`UPDATE zone_checkpoints SET is_current = 0
		WHERE snapshot_id = ? AND zone_id = ? AND is_current = 1`, snapshotID, checkpoint.ZoneID); err != nil {
		return checkpoint, fmt.Errorf("retire previous checkpoint: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`INSERT INTO zone_checkpoints
		(snapshot_id, zone_id, topology_order, generation, commit_seq, content_hash,
		 param_version, algorithm_version, sample_indices, estimate_json, point_summary,
		 heat_summary, result_json, is_current, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		snapshotID, checkpoint.ZoneID, checkpoint.TopologyOrder, generation, nextSeq,
		checkpoint.ContentHash, checkpoint.ParamVersion, checkpoint.AlgorithmVersion,
		checkpoint.SampleIndices, checkpoint.EstimateJSON, checkpoint.PointSummary,
		checkpoint.HeatSummary, checkpoint.ResultJSON, now)
	if err != nil {
		return checkpoint, fmt.Errorf("insert checkpoint: %w", err)
	}
	if _, err := tx.Exec(`UPDATE snapshot_meta SET state_json = ?, state_hash = ?, updated_at = ?
		WHERE snapshot_id = ?`, stateJSON, stateHash, now, snapshotID); err != nil {
		return checkpoint, fmt.Errorf("update snapshot state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return checkpoint, fmt.Errorf("commit zone checkpoint: %w", err)
	}
	checkpoint.Generation = generation
	checkpoint.CommitSeq = nextSeq
	checkpoint.IsCurrent = true
	return checkpoint, nil
}

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

var _ workbench.PersistentStore = (*SQLiteStore)(nil)
