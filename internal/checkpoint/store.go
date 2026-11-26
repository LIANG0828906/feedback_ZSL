package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"enginelab.local/internal/model"
)

var ErrNoCommittedCheckpoint = errors.New("no globally committed checkpoint")

type Store struct {
	Path string
	db   *sql.DB
	Hook func(stage string)
}

type SaveRequest struct {
	CheckpointID       string
	ProjectID          string
	SchemaVersion      string
	GlobalCommittedLSN uint64
	Result             *model.RecoveryResult
}

type RecordProgress struct {
	SegmentID    string `json:"segment_id"`
	AppliedCount int    `json:"applied_count"`
	LastLSN      uint64 `json:"last_lsn"`
	LastOffset   uint64 `json:"last_offset"`
	LastIndex    int    `json:"last_index"`
	LastRecordID string `json:"last_record_id"`
	UpdatedAtNS  int64  `json:"updated_at_ns"`
}

type PersistedSegment struct {
	SegmentID     string               `json:"segment_id"`
	State         string               `json:"state"`
	Integrity     string               `json:"integrity"`
	StartLSN      uint64               `json:"start_lsn"`
	EndLSN        uint64               `json:"end_lsn"`
	StartIndex    int                  `json:"start_index"`
	EndIndex      int                  `json:"end_index"`
	RecordCount   int                  `json:"record_count"`
	LastRecovered model.Point          `json:"last_recovered"`
	ElapsedNS     int64                `json:"elapsed_ns"`
	CacheKey      string               `json:"cache_key"`
	Progress      RecordProgress       `json:"progress"`
	Report        *model.SegmentReport `json:"report,omitempty"`
}

type PersistedCheckpoint struct {
	CheckpointID       string             `json:"checkpoint_id"`
	SchemaVersion      string             `json:"schema_version"`
	ProjectID          string             `json:"project_id"`
	GlobalCommittedLSN uint64             `json:"global_committed_lsn"`
	CommittedAtNS      int64              `json:"committed_at_ns"`
	Segments           []PersistedSegment `json:"segments"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return nil, model.WrapError(model.ECheckpointWrite, path, err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{Path: path, db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_global (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			committed_checkpoint_id TEXT NOT NULL,
			checkpoint_schema_version TEXT NOT NULL,
			project_id TEXT NOT NULL,
			global_committed_lsn INTEGER NOT NULL,
			committed_at_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_batch (
			checkpoint_id TEXT PRIMARY KEY,
			schema_version TEXT NOT NULL,
			project_id TEXT NOT NULL,
			global_committed_lsn INTEGER NOT NULL,
			status TEXT NOT NULL,
			created_at_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS segment_state (
			checkpoint_id TEXT NOT NULL,
			segment_id TEXT NOT NULL,
			state TEXT NOT NULL,
			integrity TEXT NOT NULL,
			start_lsn INTEGER NOT NULL,
			end_lsn INTEGER NOT NULL,
			start_index INTEGER NOT NULL,
			end_index INTEGER NOT NULL,
			record_count INTEGER NOT NULL,
			last_lsn INTEGER NOT NULL,
			last_record_id TEXT NOT NULL,
			elapsed_ns INTEGER NOT NULL,
			cache_key TEXT NOT NULL,
			report_json TEXT NOT NULL,
			updated_at_ns INTEGER NOT NULL,
			PRIMARY KEY(checkpoint_id, segment_id)
		)`,
		`CREATE TABLE IF NOT EXISTS record_progress (
			checkpoint_id TEXT NOT NULL,
			segment_id TEXT NOT NULL,
			applied_count INTEGER NOT NULL,
			last_lsn INTEGER NOT NULL,
			last_offset INTEGER NOT NULL,
			last_index INTEGER NOT NULL,
			last_record_id TEXT NOT NULL,
			updated_at_ns INTEGER NOT NULL,
			PRIMARY KEY(checkpoint_id, segment_id)
		)`,
		`CREATE TABLE IF NOT EXISTS store_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at_ns INTEGER NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return model.WrapError(model.ECheckpointWrite, "sqlite.migrate", err)
		}
	}
	return nil
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO store_meta(key,value,updated_at_ns) VALUES(?,?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at_ns=excluded.updated_at_ns`,
		key, value, time.Now().UnixNano())
	return err
}

func (s *Store) SaveRecovery(ctx context.Context, req SaveRequest) (string, error) {
	if req.SchemaVersion == "" {
		req.SchemaVersion = model.CheckpointSchemaVersion
	}
	if req.CheckpointID == "" && req.Result != nil {
		req.CheckpointID = req.Result.CheckpointID
	}
	if req.CheckpointID == "" {
		req.CheckpointID = "cp-default"
	}
	if req.GlobalCommittedLSN == 0 && req.Result != nil {
		for _, seg := range req.Result.Segments {
			if seg != nil && seg.LastRecovered.LSN > req.GlobalCommittedLSN {
				req.GlobalCommittedLSN = seg.LastRecovered.LSN
			}
		}
	}
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx, `INSERT INTO checkpoint_batch(checkpoint_id,schema_version,project_id,global_committed_lsn,status,created_at_ns)
		VALUES(?,?,?,?,?,?) ON CONFLICT(checkpoint_id) DO UPDATE SET schema_version=excluded.schema_version, project_id=excluded.project_id, global_committed_lsn=excluded.global_committed_lsn, status='writing'`,
		req.CheckpointID, req.SchemaVersion, req.ProjectID, req.GlobalCommittedLSN, "writing", now)
	if err != nil {
		return req.CheckpointID, model.WrapError(model.ECheckpointWrite, "checkpoint_batch.insert", err)
	}

	if req.Result != nil {
		for _, rep := range req.Result.Segments {
			if rep == nil {
				continue
			}
			if err := s.writeSegment(ctx, req.CheckpointID, rep); err != nil {
				return req.CheckpointID, err
			}
			if s.Hook != nil {
				s.Hook("segment:" + rep.SegmentID)
			}
		}
	}
	if s.Hook != nil {
		s.Hook("before_global_commit")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return req.CheckpointID, model.WrapError(model.ECheckpointWrite, "global.begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO checkpoint_global(id,committed_checkpoint_id,checkpoint_schema_version,project_id,global_committed_lsn,committed_at_ns)
		VALUES(1,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET committed_checkpoint_id=excluded.committed_checkpoint_id, checkpoint_schema_version=excluded.checkpoint_schema_version, project_id=excluded.project_id, global_committed_lsn=excluded.global_committed_lsn, committed_at_ns=excluded.committed_at_ns`,
		req.CheckpointID, req.SchemaVersion, req.ProjectID, req.GlobalCommittedLSN, time.Now().UnixNano())
	if err != nil {
		return req.CheckpointID, model.WrapError(model.ECheckpointWrite, "checkpoint_global.upsert", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE checkpoint_batch SET status='committed' WHERE checkpoint_id=?`, req.CheckpointID)
	if err != nil {
		return req.CheckpointID, model.WrapError(model.ECheckpointWrite, "checkpoint_batch.commit", err)
	}
	if err := tx.Commit(); err != nil {
		return req.CheckpointID, model.WrapError(model.ECheckpointWrite, "global.commit", err)
	}
	return req.CheckpointID, nil
}

func (s *Store) writeSegment(ctx context.Context, cpID string, rep *model.SegmentReport) error {
	reportJSON, err := json.Marshal(rep)
	if err != nil {
		return model.WrapError(model.ECheckpointWrite, rep.SegmentID+".json", err)
	}
	now := time.Now().UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.WrapError(model.ECheckpointWrite, rep.SegmentID+".begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO segment_state(checkpoint_id,segment_id,state,integrity,start_lsn,end_lsn,start_index,end_index,record_count,last_lsn,last_record_id,elapsed_ns,cache_key,report_json,updated_at_ns)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(checkpoint_id,segment_id) DO UPDATE SET state=excluded.state,integrity=excluded.integrity,start_lsn=excluded.start_lsn,end_lsn=excluded.end_lsn,start_index=excluded.start_index,end_index=excluded.end_index,record_count=excluded.record_count,last_lsn=excluded.last_lsn,last_record_id=excluded.last_record_id,elapsed_ns=excluded.elapsed_ns,cache_key=excluded.cache_key,report_json=excluded.report_json,updated_at_ns=excluded.updated_at_ns`,
		cpID, rep.SegmentID, rep.State, rep.Integrity, rep.Range.StartLSN, rep.Range.EndLSN, rep.Range.StartIndex, rep.Range.EndIndex, rep.Range.RecordCount, rep.LastRecovered.LSN, rep.LastRecovered.RecordID, rep.ElapsedNanoseconds, rep.CacheKey, string(reportJSON), now)
	if err != nil {
		return model.WrapError(model.ECheckpointWrite, rep.SegmentID+".segment_state", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO record_progress(checkpoint_id,segment_id,applied_count,last_lsn,last_offset,last_index,last_record_id,updated_at_ns)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(checkpoint_id,segment_id) DO UPDATE SET applied_count=excluded.applied_count,last_lsn=excluded.last_lsn,last_offset=excluded.last_offset,last_index=excluded.last_index,last_record_id=excluded.last_record_id,updated_at_ns=excluded.updated_at_ns`,
		cpID, rep.SegmentID, rep.AppliedRecords, rep.LastRecovered.LSN, rep.LastRecovered.Offset, rep.LastRecovered.Index, rep.LastRecovered.RecordID, now)
	if err != nil {
		return model.WrapError(model.ECheckpointWrite, rep.SegmentID+".record_progress", err)
	}
	return tx.Commit()
}

func (s *Store) LoadCommitted(ctx context.Context, expectedVersion string) (*PersistedCheckpoint, error) {
	if expectedVersion == "" {
		expectedVersion = model.CheckpointSchemaVersion
	}
	var cpID, version, projectID string
	var globalLSN, committedAt int64
	err := s.db.QueryRowContext(ctx, `SELECT committed_checkpoint_id,checkpoint_schema_version,project_id,global_committed_lsn,committed_at_ns FROM checkpoint_global WHERE id=1`).
		Scan(&cpID, &version, &projectID, &globalLSN, &committedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoCommittedCheckpoint
	}
	if err != nil {
		return nil, model.WrapError(model.ECheckpointWrite, "checkpoint_global.load", err)
	}
	if version != expectedVersion {
		return nil, model.NewError(model.EVersionMismatch, "$.checkpoint_global.checkpoint_schema_version",
			"checkpoint version %q does not match runtime %q; full replay required", version, expectedVersion).
			WithDetails(map[string]any{"stored_version": version, "expected_version": expectedVersion, "checkpoint_id": cpID})
	}

	pc := &PersistedCheckpoint{CheckpointID: cpID, SchemaVersion: version, ProjectID: projectID, GlobalCommittedLSN: uint64(globalLSN), CommittedAtNS: committedAt}
	rows, err := s.db.QueryContext(ctx, `SELECT segment_id,state,integrity,start_lsn,end_lsn,start_index,end_index,record_count,last_lsn,last_record_id,elapsed_ns,cache_key,report_json FROM segment_state WHERE checkpoint_id=? ORDER BY segment_id`, cpID)
	if err != nil {
		return nil, model.WrapError(model.ECheckpointWrite, "segment_state.load", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ps PersistedSegment
		var startLSN, endLSN, lastLSN int64
		var reportJSON string
		if err := rows.Scan(&ps.SegmentID, &ps.State, &ps.Integrity, &startLSN, &endLSN, &ps.StartIndex, &ps.EndIndex, &ps.RecordCount, &lastLSN, &ps.LastRecovered.RecordID, &ps.ElapsedNS, &ps.CacheKey, &reportJSON); err != nil {
			return nil, model.WrapError(model.ECheckpointWrite, "segment_state.scan", err)
		}
		ps.StartLSN = uint64(startLSN)
		ps.EndLSN = uint64(endLSN)
		ps.LastRecovered.LSN = uint64(lastLSN)
		if err := json.Unmarshal([]byte(reportJSON), &ps.Report); err != nil {
			return nil, model.WrapError(model.ECheckpointWrite, ps.SegmentID+".report_json", err)
		}
		pc.Segments = append(pc.Segments, ps)
	}
	if err := rows.Err(); err != nil {
		return nil, model.WrapError(model.ECheckpointWrite, "segment_state.rows", err)
	}
	if err := rows.Close(); err != nil {
		return nil, model.WrapError(model.ECheckpointWrite, "segment_state.close", err)
	}
	for i := range pc.Segments {
		ps := &pc.Segments[i]
		var lastLSN, lastOffset int64
		err := s.db.QueryRowContext(ctx, `SELECT applied_count,last_lsn,last_offset,last_index,last_record_id,updated_at_ns FROM record_progress WHERE checkpoint_id=? AND segment_id=?`, cpID, ps.SegmentID).
			Scan(&ps.Progress.AppliedCount, &lastLSN, &lastOffset, &ps.Progress.LastIndex, &ps.Progress.LastRecordID, &ps.Progress.UpdatedAtNS)
		if err != nil {
			return nil, model.WrapError(model.ECheckpointWrite, ps.SegmentID+".record_progress", err)
		}
		ps.Progress.LastLSN = uint64(lastLSN)
		ps.Progress.LastOffset = uint64(lastOffset)
		ps.Progress.SegmentID = ps.SegmentID
	}
	return pc, nil
}
