package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"lipsync.local/prototype/pkg/domain"
)

type Checkpoint struct {
	ShardID          string          `json:"shard_id"`
	DialogueID       string          `json:"dialogue_id"`
	CommitSeq        int64           `json:"commit_seq"`
	PackageSummary   []string        `json:"package_summary"`
	Dispatch         json.RawMessage `json:"dispatch"`
	ContentHash      string          `json:"content_hash"`
	RecipeVersion    string          `json:"recipe_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
}

type Store struct {
	db       *sql.DB
	commitMu sync.Mutex
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, &domain.Error{Code: domain.ErrStage, Path: path, Reason: "open sqlite", Cause: err}
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;`); err != nil {
		db.Close()
		return nil, &domain.Error{Code: domain.ErrStage, Path: path, Reason: "configure sqlite", Cause: err}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
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

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS checkpoints (
    shard_id TEXT NOT NULL,
    dialogue_id TEXT NOT NULL,
    commit_seq INTEGER NOT NULL UNIQUE,
    package_summary_json TEXT NOT NULL,
    dispatch_json TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    recipe_version TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    committed_at TEXT NOT NULL,
    PRIMARY KEY(shard_id, recipe_version, algorithm_version)
);
CREATE INDEX IF NOT EXISTS checkpoints_version
    ON checkpoints(recipe_version, algorithm_version, commit_seq);
CREATE TABLE IF NOT EXISTS cache_entries (
    scope TEXT NOT NULL,
    cache_key TEXT NOT NULL,
    payload BLOB NOT NULL,
    recipe_version TEXT NOT NULL,
    algorithm_version TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(scope, cache_key)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return &domain.Error{Code: domain.ErrStage, Field: "migration", Reason: "create schema", Cause: err}
	}
	return nil
}

// CommitCheckpoint atomically writes one shard's summary, frame dispatch data,
// content hash and both versions. The store mutex and one SQL connection make
// commit ordering serial and stable.
func (s *Store) CommitCheckpoint(ctx context.Context, cp Checkpoint) error {
	if cp.ShardID == "" || cp.DialogueID == "" || cp.ContentHash == "" ||
		cp.RecipeVersion == "" || cp.AlgorithmVersion == "" || len(cp.Dispatch) == 0 {
		return &domain.Error{
			Code: domain.ErrSchema, ShardID: cp.ShardID, DialogueID: cp.DialogueID,
			Reason: "checkpoint fields must not be empty",
		}
	}
	summary, err := json.Marshal(cp.PackageSummary)
	if err != nil {
		return &domain.Error{
			Code: domain.ErrStage, ShardID: cp.ShardID, DialogueID: cp.DialogueID,
			Field: "package_summary", Reason: "marshal summary", Cause: err,
		}
	}
	s.commitMu.Lock()
	defer s.commitMu.Unlock()

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return stageError(cp, "begin checkpoint transaction", err)
	}
	defer tx.Rollback()
	var nextSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq),0)+1 FROM checkpoints`).Scan(&nextSeq); err != nil {
		return stageError(cp, "next commit sequence", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO checkpoints(
    shard_id, dialogue_id, commit_seq, package_summary_json, dispatch_json,
    content_hash, recipe_version, algorithm_version, committed_at
) VALUES(?,?,?,?,?,?,?,?,?)`,
		cp.ShardID, cp.DialogueID, nextSeq, string(summary), string(cp.Dispatch),
		cp.ContentHash, cp.RecipeVersion, cp.AlgorithmVersion, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return stageError(cp, "insert checkpoint", err)
	}
	if err := tx.Commit(); err != nil {
		return stageError(cp, "commit checkpoint transaction", err)
	}
	return nil
}

func (s *Store) Committed(ctx context.Context, recipeVersion, algorithmVersion string) ([]Checkpoint, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT shard_id, dialogue_id, commit_seq, package_summary_json, dispatch_json,
       content_hash, recipe_version, algorithm_version
FROM checkpoints
WHERE recipe_version=? AND algorithm_version=?
ORDER BY commit_seq`, recipeVersion, algorithmVersion)
	if err != nil {
		return nil, &domain.Error{Code: domain.ErrStage, Field: "recovery_query", Reason: "query committed shards", Cause: err}
	}
	defer rows.Close()
	var out []Checkpoint
	for rows.Next() {
		var cp Checkpoint
		var summary string
		var dispatch string
		if err := rows.Scan(&cp.ShardID, &cp.DialogueID, &cp.CommitSeq, &summary, &dispatch,
			&cp.ContentHash, &cp.RecipeVersion, &cp.AlgorithmVersion); err != nil {
			return nil, &domain.Error{Code: domain.ErrStage, Field: "recovery_scan", Reason: "scan committed shard", Cause: err}
		}
		if err := json.Unmarshal([]byte(summary), &cp.PackageSummary); err != nil {
			return nil, &domain.Error{Code: domain.ErrDecode, ShardID: cp.ShardID, Field: "package_summary_json", Reason: "decode checkpoint", Cause: err}
		}
		cp.Dispatch = json.RawMessage(dispatch)
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, &domain.Error{Code: domain.ErrStage, Field: "recovery_rows", Reason: "iterate committed shards", Cause: err}
	}
	return out, nil
}

// Recover returns only committed shards whose content hash still matches the
// expected shard hash and whose recipe/algorithm versions match.
func (s *Store) Recover(ctx context.Context, expectedHashes map[string]string, recipeVersion, algorithmVersion string) ([]Checkpoint, error) {
	committed, err := s.Committed(ctx, recipeVersion, algorithmVersion)
	if err != nil {
		return nil, err
	}
	out := committed[:0]
	for _, cp := range committed {
		expected, ok := expectedHashes[cp.ShardID]
		if ok && expected == cp.ContentHash {
			out = append(out, cp)
		}
	}
	return out, nil
}

func (s *Store) PutCache(ctx context.Context, scope, key string, payload []byte, recipeVersion, algorithmVersion string) error {
	if scope == "" || key == "" {
		return &domain.Error{Code: domain.ErrSchema, Field: "cache_key", Reason: "cache identity missing"}
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO cache_entries(scope, cache_key, payload, recipe_version, algorithm_version, updated_at)
VALUES(?,?,?,?,?,?)
ON CONFLICT(scope, cache_key) DO UPDATE SET
  payload=excluded.payload, recipe_version=excluded.recipe_version,
  algorithm_version=excluded.algorithm_version, updated_at=excluded.updated_at`,
		scope, key, payload, recipeVersion, algorithmVersion, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return &domain.Error{Code: domain.ErrStage, Field: "cache_write", Reason: "store cache entry", Cause: err}
	}
	return nil
}

func (s *Store) GetCache(ctx context.Context, scope, key, recipeVersion, algorithmVersion string) ([]byte, bool, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `
SELECT payload FROM cache_entries
WHERE scope=? AND cache_key=? AND recipe_version=? AND algorithm_version=?`,
		scope, key, recipeVersion, algorithmVersion).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &domain.Error{Code: domain.ErrStage, Field: "cache_read", Reason: "read cache entry", Cause: err}
	}
	return payload, true, nil
}

func (s *Store) ShardCount(ctx context.Context) (int, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM checkpoints`).Scan(&count); err != nil {
		return 0, &domain.Error{Code: domain.ErrStage, Field: "shard_count", Reason: "count checkpoints", Cause: err}
	}
	return count, nil
}

func stageError(cp Checkpoint, reason string, cause error) error {
	return &domain.Error{
		Code: domain.ErrStage, ShardID: cp.ShardID, DialogueID: cp.DialogueID,
		Field: "checkpoint", Reason: reason + ": " + fmt.Sprint(cause), Cause: cause,
	}
}
