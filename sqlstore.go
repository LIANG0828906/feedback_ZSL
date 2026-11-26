package vpf

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
)

// SQLiteSchema is the checkpoint table contract. The Go wrapper itself does
// not import a SQLite driver: register any database/sql driver named by the
// deploying binary and pass its *sql.DB to NewSQLStore.
const SQLiteSchema = `
CREATE TABLE IF NOT EXISTS chunk_checkpoints (
    frame_index            INTEGER NOT NULL,
    chunk_index            INTEGER NOT NULL,
    chunk_id               TEXT NOT NULL,
    x                      INTEGER NOT NULL,
    y                      INTEGER NOT NULL,
    width                  INTEGER NOT NULL,
    height                 INTEGER NOT NULL,
    pixel_blob             BLOB NOT NULL,
    pixel_digest           TEXT NOT NULL,
    stack_snapshot_version INTEGER NOT NULL,
    ripple_source_version  INTEGER NOT NULL,
    cache_key              TEXT NOT NULL,
    content_hash           TEXT NOT NULL,
    param_version          INTEGER NOT NULL,
    algorithm_version      TEXT NOT NULL,
    pixel_count            INTEGER NOT NULL,
    commit_seq             INTEGER NOT NULL,
    PRIMARY KEY (frame_index, chunk_index)
);
CREATE INDEX IF NOT EXISTS idx_chunk_checkpoints_cache_key
    ON chunk_checkpoints(frame_index, cache_key);
`

type SQLStore struct {
	db *sql.DB
	mu sync.Mutex
}

// NewSQLStore wraps a driver-agnostic *sql.DB. For file SQLite, a deployment
// may use github.com/mattn/go-sqlite3, modernc.org/sqlite, or the optional
// stdlib/cgo bridge in internal/sqlite; this module does not require them.
func NewSQLStore(db *sql.DB) *SQLStore {
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return &SQLStore{db: db}
}

func (s *SQLStore) DB() *sql.DB { return s.db }

func (s *SQLStore) Init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, SQLiteSchema); err != nil {
		return fmt.Errorf("vpf: initialize sqlite checkpoint schema: %w", err)
	}
	return nil
}

// PutCheckpoint executes sequence allocation, replacement and insert in one
// transaction. The mutex makes concurrent caller transactions serial too.
func (s *SQLStore) PutCheckpoint(ctx context.Context, record CheckpointRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("vpf: begin checkpoint tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq), 0) + 1 FROM chunk_checkpoints`).Scan(&nextSeq); err != nil {
		return 0, fmt.Errorf("vpf: allocate commit sequence: %w", err)
	}
	record.CommitSeq = nextSeq
	if record.ChunkID == "" {
		record.ChunkID = ChunkID(record.FrameIndex, record.ChunkIndex)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chunk_checkpoints WHERE frame_index = ? AND chunk_index = ?`,
		record.FrameIndex, record.ChunkIndex,
	); err != nil {
		return 0, fmt.Errorf("vpf: replace old checkpoint: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO chunk_checkpoints (
    frame_index, chunk_index, chunk_id, x, y, width, height, pixel_blob,
    pixel_digest, stack_snapshot_version, ripple_source_version, cache_key,
    content_hash, param_version, algorithm_version, pixel_count, commit_seq
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.FrameIndex, record.ChunkIndex, record.ChunkID,
		record.Bounds.X, record.Bounds.Y, record.Bounds.W, record.Bounds.H,
		record.Pix, record.PixelDigest, record.StackSnapshotVersion,
		record.RippleSourceVersion, record.CacheKey, record.ContentHash,
		record.ParamVersion, record.AlgorithmVersion, record.PixelCount, record.CommitSeq,
	)
	if err != nil {
		return 0, fmt.Errorf("vpf: insert checkpoint: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("vpf: commit checkpoint tx: %w", err)
	}
	return record.CommitSeq, nil
}

const selectCheckpointColumns = `
SELECT frame_index, chunk_index, chunk_id, x, y, width, height, pixel_blob,
       pixel_digest, stack_snapshot_version, ripple_source_version, cache_key,
       content_hash, param_version, algorithm_version, pixel_count, commit_seq
FROM chunk_checkpoints
`

func scanCheckpoint(scanner interface {
	Scan(dest ...any) error
}) (CheckpointRecord, error) {
	var record CheckpointRecord
	err := scanner.Scan(
		&record.FrameIndex, &record.ChunkIndex, &record.ChunkID,
		&record.Bounds.X, &record.Bounds.Y, &record.Bounds.W, &record.Bounds.H,
		&record.Pix, &record.PixelDigest, &record.StackSnapshotVersion,
		&record.RippleSourceVersion, &record.CacheKey, &record.ContentHash,
		&record.ParamVersion, &record.AlgorithmVersion, &record.PixelCount, &record.CommitSeq,
	)
	return record, err
}

func (s *SQLStore) GetCheckpoint(ctx context.Context, frameIndex, chunkIndex int) (CheckpointRecord, bool, error) {
	row := s.db.QueryRowContext(ctx, selectCheckpointColumns+` WHERE frame_index = ? AND chunk_index = ?`, frameIndex, chunkIndex)
	record, err := scanCheckpoint(row)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckpointRecord{}, false, nil
	}
	if err != nil {
		return CheckpointRecord{}, false, fmt.Errorf("vpf: get checkpoint: %w", err)
	}
	return record, true, nil
}

func (s *SQLStore) ListCheckpoints(ctx context.Context, frameIndex int) ([]CheckpointRecord, error) {
	rows, err := s.db.QueryContext(ctx, selectCheckpointColumns+` WHERE frame_index = ? ORDER BY chunk_index ASC`, frameIndex)
	if err != nil {
		return nil, fmt.Errorf("vpf: list checkpoints: %w", err)
	}
	defer rows.Close()
	var records []CheckpointRecord
	for rows.Next() {
		record, err := scanCheckpoint(rows)
		if err != nil {
			return nil, fmt.Errorf("vpf: scan checkpoint: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *SQLStore) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}
