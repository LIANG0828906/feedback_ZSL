package dialoguecal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"
)

type SQLiteCheckpointStore struct {
	db *sql.DB
}

func NewSQLiteCheckpointStore(path string) (*SQLiteCheckpointStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store := &SQLiteCheckpointStore{db: db}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *SQLiteCheckpointStore) init(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS frames (
			project_version TEXT NOT NULL,
			seq INTEGER NOT NULL,
			segment_id TEXT NOT NULL,
			local_index INTEGER NOT NULL,
			state TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			PRIMARY KEY(project_version, seq)
		)`,
		`CREATE TABLE IF NOT EXISTS segment_progress (
			project_version TEXT NOT NULL,
			segment_id TEXT NOT NULL,
			last_local_index INTEGER NOT NULL,
			last_global_seq INTEGER NOT NULL,
			PRIMARY KEY(project_version, segment_id)
		)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:schema", Detail: err.Error()}
		}
	}
	return nil
}

func (s *SQLiteCheckpointStore) Close() error { return s.db.Close() }

func (s *SQLiteCheckpointStore) Reset(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta`); err != nil {
		return &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:meta", Detail: err.Error()}
	}
	return nil
}

// CommitFrame writes frame state and segment progress, then advances the
// global cursor as the final statement in the transaction.
func (s *SQLiteCheckpointStore) CommitFrame(ctx context.Context, version string, result FrameResult, segmentLastLocal int64) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO frames(project_version,seq,segment_id,local_index,state,payload_json) VALUES(?,?,?,?,?,?)`,
		version, result.Frame.GlobalIndex, result.Frame.SegmentID, result.Frame.IndexInSegment, string(result.Calibration.State), string(payload)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO segment_progress(project_version,segment_id,last_local_index,last_global_seq) VALUES(?,?,?,?)`,
		version, result.Frame.SegmentID, segmentLastLocal, result.Frame.GlobalIndex); err != nil {
		return err
	}
	// The global sequence is deliberately last. A crash before commit leaves
	// the old cursor and the transaction's frame writes invisible.
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('project_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('last_frame_seq',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fmt.Sprintf("%d", result.Frame.GlobalIndex)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteCheckpointStore) LoadCheckpoint(ctx context.Context, expectedVersion string) (*Checkpoint, error) {
	var version string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='project_version'`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return &Checkpoint{ProjectVersion: expectedVersion, LastFrameSeq: -1, Frames: []FrameResult{}, SegmentProgress: map[string]int64{}}, nil
	}
	if err != nil {
		return nil, &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:meta", Detail: err.Error()}
	}
	if version != expectedVersion {
		var last int64
		_ = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='last_frame_seq'`).Scan(&last)
		return nil, &BusinessError{Code: CodeCheckpointVersion, Path: "sqlite:meta.project_version", Detail: "checkpoint project version does not match current snapshot", Context: map[string]any{"checkpoint_version": version, "expected_version": expectedVersion, "last_frame_seq": last}}
	}

	var lastText string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='last_frame_seq'`).Scan(&lastText); err != nil {
		return nil, &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:meta.last_frame_seq", Detail: err.Error()}
	}
	var last int64
	if _, err := fmt.Sscanf(lastText, "%d", &last); err != nil {
		return nil, &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:meta.last_frame_seq", Detail: err.Error()}
	}
	cp := &Checkpoint{ProjectVersion: version, LastFrameSeq: last, SegmentProgress: map[string]int64{}}
	rowSet, err := s.db.QueryContext(ctx, `SELECT payload_json FROM frames WHERE project_version=? AND seq<=? ORDER BY seq`, version, last)
	if err != nil {
		return nil, &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:frames", Detail: err.Error()}
	}
	defer rowSet.Close()
	for rowSet.Next() {
		var payload string
		if err := rowSet.Scan(&payload); err != nil {
			return nil, &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:frames.payload_json", Detail: err.Error()}
		}
		var result FrameResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, classifyCheckpointReadError(err, "sqlite:frames.payload_json")
		}
		cp.Frames = append(cp.Frames, result)
	}
	progRows, err := s.db.QueryContext(ctx, `SELECT segment_id,last_local_index FROM segment_progress WHERE project_version=?`, version)
	if err == nil {
		defer progRows.Close()
		for progRows.Next() {
			var id string
			var local int64
			if err := progRows.Scan(&id, &local); err != nil {
				return nil, &BusinessError{Code: CodeCheckpointCorrupt, Path: "sqlite:segment_progress", Detail: err.Error()}
			}
			cp.SegmentProgress[id] = local
		}
	}
	return cp, nil
}
