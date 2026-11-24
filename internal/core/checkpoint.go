package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type CommitKey struct {
	Stage string `json:"stage"`
	Block int    `json:"block"`
}

func (k CommitKey) String() string { return fmt.Sprintf("%s:%d", k.Stage, k.Block) }

type CheckpointStore struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, WrapError(CodeCheckpoint, "path", "cannot create checkpoint directory", err)
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, WrapError(CodeCheckpoint, "open", "cannot open SQLite checkpoint database", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON;`); err != nil {
		_ = db.Close()
		return nil, WrapError(CodeCheckpoint, "pragma", "cannot configure SQLite", err)
	}
	store := &CheckpointStore{db: db, path: path}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *CheckpointStore) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS snapshots (
			image_fp TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			params_json TEXT NOT NULL,
			stage TEXT NOT NULL,
			block_index INTEGER NOT NULL,
			block_row INTEGER NOT NULL,
			block_col INTEGER NOT NULL,
			x INTEGER NOT NULL,
			y INTEGER NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			channels INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			coverage REAL NOT NULL,
			pixels BLOB NOT NULL,
			commit_seq INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (image_fp, param_version, algorithm_version, stage, block_index)
		);`,
		`CREATE TABLE IF NOT EXISTS commits (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			image_fp TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			stage TEXT NOT NULL,
			block_index INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			UNIQUE (image_fp, param_version, algorithm_version, stage, block_index)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_snapshots_commit ON snapshots(image_fp, param_version, algorithm_version, commit_seq);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return WrapError(CodeCheckpoint, "migrate", "cannot initialize checkpoint schema", err)
		}
	}
	return nil
}

func (s *CheckpointStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *CheckpointStore) Path() string { return s.path }

// SaveBlock commits one block-stage result atomically. The mutex and the
// single-connection pool guarantee that writes never interleave.
func (s *CheckpointStore) SaveBlock(ctx context.Context, imageFP string, params Params, algorithmVersion string, stage string, block Block, mask Image, contentHash string) error {
	if err := ctx.Err(); err != nil {
		return WrapError(CodeCanceled, "", "checkpoint write canceled", err)
	}
	if err := params.Validate(); err != nil {
		return err
	}
	if stageIndex(stage) < 0 {
		return NewError(CodeState, "stage", "unknown stage "+stage)
	}
	if err := mask.Validate(); err != nil {
		return err
	}
	if contentHash == "" {
		contentHash = FingerprintImage(mask)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WrapError(CodeCheckpoint, "begin", "cannot begin checkpoint transaction", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UnixNano()

	var existingHash string
	var existingSeq int64
	err = tx.QueryRowContext(ctx, `SELECT content_hash, commit_seq FROM snapshots
		WHERE image_fp=? AND param_version=? AND algorithm_version=? AND stage=? AND block_index=?`,
		imageFP, params.Version(), algorithmVersion, stage, block.Index).Scan(&existingHash, &existingSeq)
	if err != nil && err != sql.ErrNoRows {
		return WrapError(CodeCheckpoint, "read-existing", "cannot read existing checkpoint row", err)
	}
	if err == nil {
		if existingHash != contentHash {
			return &AppError{
				Code:    CodeCheckpoint,
				Field:   "contentHash",
				Message: "checkpoint content hash mismatch for an existing block",
				Details: map[string]any{"stage": stage, "block": block.Index, "expected": existingHash, "actual": contentHash},
			}
		}
		if err := tx.Commit(); err != nil {
			return WrapError(CodeCheckpoint, "commit", "cannot commit idempotent checkpoint transaction", err)
		}
		return nil
	}

	res, err := tx.ExecContext(ctx, `INSERT INTO commits
		(image_fp,param_version,algorithm_version,stage,block_index,content_hash,created_at)
		VALUES (?,?,?,?,?,?,?)`,
		imageFP, params.Version(), algorithmVersion, stage, block.Index, contentHash, now)
	if err != nil {
		return WrapError(CodeCheckpoint, "commit-log", "cannot append checkpoint commit", err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return WrapError(CodeCheckpoint, "commit-seq", "cannot read checkpoint commit sequence", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO snapshots
		(image_fp,param_version,algorithm_version,params_json,stage,block_index,block_row,block_col,x,y,width,height,channels,content_hash,coverage,pixels,commit_seq,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		imageFP, params.Version(), algorithmVersion, params.CanonicalJSON(),
		stage, block.Index, block.Row, block.Col, block.X, block.Y, block.Width, block.Height, mask.Channels,
		contentHash, coverage(mask.Pix, mask.PixelCount()), mask.Pix, seq, now)
	if err != nil {
		return WrapError(CodeCheckpoint, "snapshot", "cannot write checkpoint snapshot", err)
	}
	if err := tx.Commit(); err != nil {
		return WrapError(CodeCheckpoint, "commit", "cannot commit checkpoint transaction", err)
	}
	return nil
}

type Snapshot struct {
	Valid       bool             `json:"valid"`
	Complete    bool             `json:"complete"`
	Invalidated bool             `json:"invalidated"`
	Reason      string           `json:"reason,omitempty"`
	Stages      map[string]Image `json:"-"`
	CompleteSet map[string]bool  `json:"stageComplete"`
	CommitKeys  []CommitKey      `json:"commitKeys"`
	Rows        int              `json:"rows"`
}

type checkpointRow struct {
	ParamVersion     string
	AlgorithmVersion string
	ParamsJSON       string
	Stage            string
	BlockIndex       int
	BlockRow         int
	BlockCol         int
	X                int
	Y                int
	Width            int
	Height           int
	Channels         int
	ContentHash      string
	Coverage         float64
	Pixels           []byte
	CommitSeq        int64
}

// LoadSnapshot validates the whole snapshot prefix. A single version, hash, or
// ordering mismatch deletes every row for the image before returning an
// invalidated snapshot.
func (s *CheckpointStore) LoadSnapshot(ctx context.Context, imageFP string, params Params, algorithmVersion string, imageWidth, imageHeight, imageChannels int, grid Grid) (*Snapshot, error) {
	if err := params.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.loadRows(ctx, imageFP)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return &Snapshot{Valid: true, Complete: false, Stages: map[string]Image{}, CompleteSet: map[string]bool{}}, nil
	}
	expectedVersion := params.Version()
	canonicalParams := params.CanonicalJSON()
	invalid := func(reason string) (*Snapshot, error) {
		if err := s.WipeImage(context.Background(), imageFP); err != nil {
			return nil, err
		}
		return &Snapshot{
			Valid:       false,
			Invalidated: true,
			Reason:      reason,
			Stages:      map[string]Image{},
			CompleteSet: map[string]bool{},
		}, nil
	}
	expected := allCommitKeys(grid)
	if len(rows) > len(expected) {
		return invalid("checkpoint contains more blocks than the current grid")
	}
	for i, row := range rows {
		if row.ParamVersion != expectedVersion {
			return invalid("parameter version mismatch")
		}
		if row.AlgorithmVersion != algorithmVersion {
			return invalid("algorithm version mismatch")
		}
		if row.ParamsJSON != canonicalParams {
			return invalid("parameter snapshot mismatch")
		}
		if row.Stage != expected[i].Stage || row.BlockIndex != expected[i].Block {
			return invalid("commit sequence mismatch")
		}
		block, ok := grid.Block(row.BlockIndex)
		if !ok || block.Index != row.BlockIndex || block.Row != row.BlockRow || block.Col != row.BlockCol ||
			block.X != row.X || block.Y != row.Y || block.Width != row.Width || block.Height != row.Height {
			return invalid("block geometry mismatch")
		}
		if row.Channels != imageChannels || row.Width <= 0 || row.Height <= 0 {
			return invalid("block channel or size mismatch")
		}
		if len(row.Pixels) != row.Width*row.Height*row.Channels {
			return invalid("block pixel length mismatch")
		}
		actual := FingerprintBytes(row.Width, row.Height, row.Channels, row.Pixels)
		if actual != row.ContentHash {
			return invalid("content hash mismatch")
		}
	}
	snapshot := &Snapshot{
		Valid:       true,
		Stages:      map[string]Image{},
		CompleteSet: map[string]bool{},
		Rows:        len(rows),
	}
	for _, row := range rows {
		snapshot.CommitKeys = append(snapshot.CommitKeys, CommitKey{Stage: row.Stage, Block: row.BlockIndex})
	}
	perStage := len(grid.Blocks)
	fullRows := len(rows) / perStage
	for stageIdx := 0; stageIdx < fullRows; stageIdx++ {
		stage := StageOrder[stageIdx]
		im := NewImage(imageWidth, imageHeight, imageChannels)
		start := stageIdx * perStage
		for i := start; i < start+perStage; i++ {
			row := rows[i]
			block := grid.Blocks[row.BlockIndex]
			for by := 0; by < block.Height; by++ {
				src := by * block.Width * imageChannels
				dst := ((block.Y+by)*imageWidth + block.X) * imageChannels
				copy(im.Pix[dst:dst+block.Width*imageChannels], row.Pixels[src:src+block.Width*imageChannels])
			}
		}
		snapshot.Stages[stage] = im
		snapshot.CompleteSet[stage] = true
	}
	snapshot.Complete = len(snapshot.Stages) == len(StageOrder)
	if !snapshot.Complete {
		snapshot.Reason = "partial checkpoint prefix"
	}
	return snapshot, nil
}

func (s *CheckpointStore) loadRows(ctx context.Context, imageFP string) ([]checkpointRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT param_version,algorithm_version,params_json,stage,block_index,
		block_row,block_col,x,y,width,height,channels,content_hash,coverage,pixels,commit_seq
		FROM snapshots WHERE image_fp=? ORDER BY commit_seq ASC`, imageFP)
	if err != nil {
		return nil, WrapError(CodeCheckpoint, "load", "cannot query checkpoint rows", err)
	}
	defer rows.Close()
	var out []checkpointRow
	for rows.Next() {
		var row checkpointRow
		if err := rows.Scan(&row.ParamVersion, &row.AlgorithmVersion, &row.ParamsJSON, &row.Stage, &row.BlockIndex,
			&row.BlockRow, &row.BlockCol, &row.X, &row.Y, &row.Width, &row.Height, &row.Channels,
			&row.ContentHash, &row.Coverage, &row.Pixels, &row.CommitSeq); err != nil {
			return nil, WrapError(CodeCheckpoint, "scan", "cannot scan checkpoint row", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, WrapError(CodeCheckpoint, "rows", "cannot iterate checkpoint rows", err)
	}
	return out, nil
}

func (s *CheckpointStore) WipeImage(ctx context.Context, imageFP string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WrapError(CodeCheckpoint, "wipe-begin", "cannot begin checkpoint wipe", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE image_fp=?`, imageFP); err != nil {
		return WrapError(CodeCheckpoint, "wipe-snapshots", "cannot delete checkpoint snapshots", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commits WHERE image_fp=?`, imageFP); err != nil {
		return WrapError(CodeCheckpoint, "wipe-commits", "cannot delete checkpoint commits", err)
	}
	if err := tx.Commit(); err != nil {
		return WrapError(CodeCheckpoint, "wipe-commit", "cannot commit checkpoint wipe", err)
	}
	return nil
}

func (s *CheckpointStore) CommitCount(ctx context.Context, imageFP string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commits WHERE image_fp=?`, imageFP).Scan(&n)
	if err != nil {
		return 0, WrapError(CodeCheckpoint, "count", "cannot count checkpoint commits", err)
	}
	return n, nil
}

func (s *CheckpointStore) WipeAll(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WrapError(CodeCheckpoint, "wipe-all-begin", "cannot begin full checkpoint wipe", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots`); err != nil {
		return WrapError(CodeCheckpoint, "wipe-all-snapshots", "cannot erase snapshots", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM commits`); err != nil {
		return WrapError(CodeCheckpoint, "wipe-all-commits", "cannot erase commits", err)
	}
	return tx.Commit()
}

func allCommitKeys(grid Grid) []CommitKey {
	keys := make([]CommitKey, 0, len(StageOrder)*len(grid.Blocks))
	for _, stage := range StageOrder {
		for _, block := range grid.Blocks {
			keys = append(keys, CommitKey{Stage: stage, Block: block.Index})
		}
	}
	return keys
}

func stageIndex(stage string) int {
	for i, s := range StageOrder {
		if s == stage {
			return i
		}
	}
	return -1
}

func blockHash(block Block, im Image) string {
	h := FingerprintImage(im)
	raw, _ := json.Marshal(map[string]any{"block": block, "hash": h})
	return FingerprintBytes(0, 0, 0, raw)
}
