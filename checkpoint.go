package iconmask

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	Seq              int64  `json:"seq"`
	RunID            string `json:"runId"`
	ImageID          string `json:"imageId"`
	Block            Coord  `json:"block"`
	Round            int    `json:"round"`
	Stage            Stage  `json:"stage"`
	OutputDigest     string `json:"outputDigest"`
	RadiusTableJSON  string `json:"radiusTableJson"`
	CacheKey         string `json:"cacheKey"`
	ContentHash      string `json:"contentHash"`
	ParameterVersion string `json:"parameterVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	OutputMask       []byte `json:"outputMask,omitempty"`
}

type StageCommit struct {
	Seq              int64  `json:"seq"`
	RunID            string `json:"runId"`
	ImageID          string `json:"imageId"`
	Block            Coord  `json:"block"`
	Round            int    `json:"round"`
	Stage            Stage  `json:"stage"`
	OutputDigest     string `json:"outputDigest"`
	CacheKey         string `json:"cacheKey"`
	ContentHash      string `json:"contentHash"`
	ParameterVersion string `json:"parameterVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
}

type CheckpointStore struct {
	db     *sql.DB
	mu     sync.Mutex
	closed bool
}

func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS stage_checkpoints (
    seq INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL,
    image_id TEXT NOT NULL,
    block_x INTEGER NOT NULL,
    block_y INTEGER NOT NULL,
    round INTEGER NOT NULL,
    stage TEXT NOT NULL,
    output_digest TEXT NOT NULL,
    radius_table_json TEXT NOT NULL,
    cache_key TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    parameter_version TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    output_mask BLOB,
    UNIQUE(run_id, image_id, block_x, block_y, round, stage)
);
CREATE INDEX IF NOT EXISTS idx_checkpoints_run_seq ON stage_checkpoints(run_id, seq);
`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &CheckpointStore{db: db}, nil
}

func (store *CheckpointStore) SaveStageCheckpoint(ctx context.Context, checkpoint Checkpoint) (Checkpoint, error) {
	if checkpoint.RunID == "" {
		return Checkpoint{}, checkpointError("<none>", "runId", "run id is required")
	}
	if checkpoint.ImageID == "" {
		return Checkpoint{}, checkpointError(checkpoint.RunID, "imageId", "image id is required")
	}
	if !legalStage(checkpoint.Stage) {
		return Checkpoint{}, checkpointError(checkpoint.RunID, "stage", "unknown stage")
	}
	required := []struct {
		field string
		value string
	}{
		{"outputDigest", checkpoint.OutputDigest},
		{"radiusTableJson", checkpoint.RadiusTableJSON},
		{"cacheKey", checkpoint.CacheKey},
		{"contentHash", checkpoint.ContentHash},
		{"parameterVersion", checkpoint.ParameterVersion},
		{"algorithmVersion", checkpoint.AlgorithmVersion},
	}
	for _, item := range required {
		if item.value == "" {
			return Checkpoint{}, checkpointError(checkpoint.RunID, item.field, "checkpoint field is required")
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return Checkpoint{}, checkpointError(checkpoint.RunID, "store", "checkpoint store is closed")
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return Checkpoint{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.ExecContext(ctx, `
INSERT INTO stage_checkpoints
    (run_id, image_id, block_x, block_y, round, stage, output_digest,
     radius_table_json, cache_key, content_hash, parameter_version,
     algorithm_version, output_mask)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		checkpoint.RunID, checkpoint.ImageID, checkpoint.Block.X, checkpoint.Block.Y,
		checkpoint.Round, string(checkpoint.Stage), checkpoint.OutputDigest,
		checkpoint.RadiusTableJSON, checkpoint.CacheKey, checkpoint.ContentHash,
		checkpoint.ParameterVersion, checkpoint.AlgorithmVersion, checkpoint.OutputMask,
	)
	if err != nil {
		return Checkpoint{}, checkpointError(checkpoint.RunID, "checkpoint",
			"cannot atomically insert checkpoint: "+err.Error())
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return Checkpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return Checkpoint{}, err
	}
	committed = true
	checkpoint.Seq = seq
	return checkpoint, nil
}

func checkpointError(imageID, field, message string) *Error {
	return domainError(CodeCheckpoint, imageID, field, message)
}

func (store *CheckpointStore) LoadCheckpoints(ctx context.Context, runID string) ([]Checkpoint, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil, checkpointError(runID, "store", "checkpoint store is closed")
	}
	rows, err := store.db.QueryContext(ctx, `
SELECT seq, run_id, image_id, block_x, block_y, round, stage, output_digest,
       radius_table_json, cache_key, content_hash, parameter_version,
       algorithm_version, output_mask
FROM stage_checkpoints
WHERE run_id = ?
ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	checkpoints := make([]Checkpoint, 0)
	for rows.Next() {
		var checkpoint Checkpoint
		var stage string
		if err := rows.Scan(
			&checkpoint.Seq, &checkpoint.RunID, &checkpoint.ImageID,
			&checkpoint.Block.X, &checkpoint.Block.Y, &checkpoint.Round, &stage,
			&checkpoint.OutputDigest, &checkpoint.RadiusTableJSON, &checkpoint.CacheKey,
			&checkpoint.ContentHash, &checkpoint.ParameterVersion, &checkpoint.AlgorithmVersion,
			&checkpoint.OutputMask,
		); err != nil {
			return nil, err
		}
		checkpoint.Stage = Stage(stage)
		checkpoints = append(checkpoints, checkpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return checkpoints, nil
}

func (store *CheckpointStore) CommitSequence(ctx context.Context, runID string) ([]StageCommit, error) {
	checkpoints, err := store.LoadCheckpoints(ctx, runID)
	if err != nil {
		return nil, err
	}
	commits := make([]StageCommit, len(checkpoints))
	for i, checkpoint := range checkpoints {
		commits[i] = StageCommit{
			Seq:              int64(i + 1),
			RunID:            checkpoint.RunID,
			ImageID:          checkpoint.ImageID,
			Block:            checkpoint.Block,
			Round:            checkpoint.Round,
			Stage:            checkpoint.Stage,
			OutputDigest:     checkpoint.OutputDigest,
			CacheKey:         checkpoint.CacheKey,
			ContentHash:      checkpoint.ContentHash,
			ParameterVersion: checkpoint.ParameterVersion,
			AlgorithmVersion: checkpoint.AlgorithmVersion,
		}
	}
	return commits, nil
}

func (store *CheckpointStore) Count(ctx context.Context, runID string) (int, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return 0, checkpointError(runID, "store", "checkpoint store is closed")
	}
	var count int
	err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM stage_checkpoints WHERE run_id = ?`, runID).Scan(&count)
	return count, err
}

func (store *CheckpointStore) DeleteRun(ctx context.Context, runID string) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return checkpointError(runID, "store", "checkpoint store is closed")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM stage_checkpoints WHERE run_id = ?`, runID); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (store *CheckpointStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return store.db.Close()
}

func (store *CheckpointStore) DB() *sql.DB {
	return store.db
}

func EncodeRadiusTable(table *RadiusTable) string {
	if table == nil {
		return ""
	}
	data, err := json.Marshal(table)
	if err != nil {
		return ""
	}
	return string(data)
}

func digestPayload(mask *Mask, fallback string) string {
	if mask != nil {
		return mask.SHA256()
	}
	return fmt.Sprintf("%x", sha256String(fallback))
}
