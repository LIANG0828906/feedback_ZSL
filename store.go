package acousticfield

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNoSnapshot = errors.New("no snapshot")

type SnapshotMeta struct {
	ID               string    `json:"id"`
	LevelID          string    `json:"level_id"`
	LevelHash        string    `json:"level_hash"`
	TruthFingerprint string    `json:"truth_fingerprint"`
	ParamsVersion    string    `json:"params_version"`
	AlgorithmVersion string    `json:"algorithm_version"`
	Complete         bool      `json:"complete"`
	LastRound        int64     `json:"last_round"`
	NextSequence     int       `json:"next_sequence"`
	CreatedAt        time.Time `json:"created_at"`
}

type CommitLogEntry struct {
	SnapshotID string `json:"snapshot_id"`
	RoundID    int64  `json:"round_id"`
	Sequence   int    `json:"sequence"`
	ChunkID    int    `json:"chunk_id"`
	Reused     bool   `json:"reused"`
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, valueError("store", "path", path, "non-empty", "non-empty")
	}
	if path != ":memory:" && !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		path = abs
	}
	dsn := path
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		dsn = "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection makes the writer order explicit and also makes
	// ":memory:" behave as one durable test database.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS snapshots (
			id TEXT PRIMARY KEY,
			level_id TEXT NOT NULL,
			level_hash TEXT NOT NULL,
			truth_fingerprint TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			complete INTEGER NOT NULL DEFAULT 0,
			last_round INTEGER NOT NULL DEFAULT 0,
			next_sequence INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS chunk_results (
			snapshot_id TEXT NOT NULL,
			chunk_id INTEGER NOT NULL,
			round_id INTEGER NOT NULL,
			path_digest TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			result_json TEXT NOT NULL,
			PRIMARY KEY(snapshot_id, chunk_id),
			FOREIGN KEY(snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS commit_log (
			snapshot_id TEXT NOT NULL,
			round_id INTEGER NOT NULL,
			sequence INTEGER NOT NULL,
			chunk_id INTEGER NOT NULL,
			reused INTEGER NOT NULL,
			PRIMARY KEY(snapshot_id, round_id, sequence),
			UNIQUE(snapshot_id, round_id, chunk_id),
			FOREIGN KEY(snapshot_id) REFERENCES snapshots(id) ON DELETE CASCADE
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// EnsureSnapshot creates a snapshot, or resets an existing snapshot when any
// identity/version input differs.
func (s *Store) EnsureSnapshot(ctx context.Context, meta SnapshotMeta) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var existing SnapshotMeta
	var created string
	var complete, lastRound, nextSeq int
	err = tx.QueryRowContext(ctx, `SELECT id, level_id, level_hash, truth_fingerprint, params_version, algorithm_version, complete, last_round, next_sequence, created_at
		FROM snapshots WHERE id=?`, meta.ID).Scan(
		&existing.ID, &existing.LevelID, &existing.LevelHash, &existing.TruthFingerprint,
		&existing.ParamsVersion, &existing.AlgorithmVersion, &complete, &lastRound, &nextSeq, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		if meta.CreatedAt.IsZero() {
			meta.CreatedAt = time.Now().UTC()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO snapshots
			(id, level_id, level_hash, truth_fingerprint, params_version, algorithm_version, complete, last_round, next_sequence, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?)`,
			meta.ID, meta.LevelID, meta.LevelHash, meta.TruthFingerprint, meta.ParamsVersion, meta.AlgorithmVersion, meta.CreatedAt.Format(time.RFC3339Nano))
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	existing.Complete = complete != 0
	existing.LastRound = int64(lastRound)
	existing.NextSequence = nextSeq
	if existing.LevelHash == meta.LevelHash && existing.TruthFingerprint == meta.TruthFingerprint &&
		existing.ParamsVersion == meta.ParamsVersion && existing.AlgorithmVersion == meta.AlgorithmVersion {
		return false, tx.Commit()
	}
	// Version or structural mismatch: invalidate the whole snapshot.
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log WHERE snapshot_id=?`, meta.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_results WHERE snapshot_id=?`, meta.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET level_id=?, level_hash=?, truth_fingerprint=?, params_version=?,
		algorithm_version=?, complete=0, last_round=0, next_sequence=0, created_at=? WHERE id=?`,
		meta.LevelID, meta.LevelHash, meta.TruthFingerprint, meta.ParamsVersion, meta.AlgorithmVersion,
		time.Now().UTC().Format(time.RFC3339Nano), meta.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s *Store) SnapshotMeta(ctx context.Context, id string) (SnapshotMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m SnapshotMeta
	var created string
	var complete, lastRound, nextSeq int
	err := s.db.QueryRowContext(ctx, `SELECT id, level_id, level_hash, truth_fingerprint, params_version, algorithm_version,
		complete, last_round, next_sequence, created_at FROM snapshots WHERE id=?`, id).Scan(
		&m.ID, &m.LevelID, &m.LevelHash, &m.TruthFingerprint, &m.ParamsVersion, &m.AlgorithmVersion,
		&complete, &lastRound, &nextSeq, &created,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotMeta{}, ErrNoSnapshot
	}
	if err != nil {
		return SnapshotMeta{}, err
	}
	m.Complete = complete != 0
	m.LastRound = int64(lastRound)
	m.NextSequence = nextSeq
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return m, nil
}

func (s *Store) BeginCommitRound(ctx context.Context, snapshotID string, roundID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE snapshots SET last_round=?, next_sequence=0, complete=0 WHERE id=?`, roundID, snapshotID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrNoSnapshot
	}
	return nil
}

// CommitChunk writes one chunk result and its commit-log entry in one
// transaction. seq must equal the snapshot's current next_sequence.
func (s *Store) CommitChunk(ctx context.Context, snapshotID string, roundID int64, seq int, result ChunkResult, reused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if result.PathDigest == "" {
		result.PathDigest = PathDigest(result.Paths)
	}
	if result.ContentHash == "" || result.ParamsVersion == "" || result.AlgorithmVersion == "" {
		return schemaError(fmt.Sprintf("chunk-%d", result.ChunkID), "checkpoint", "content hash/version fields are required")
	}
	if got := PathDigest(result.Paths); got != result.PathDigest {
		return &FieldError{
			Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", result.ChunkID), FieldPath: "path_digest",
			Measured: got, Message: "refusing to checkpoint a mutated path list",
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var nextSeq int
	var currentRound int64
	if err := tx.QueryRowContext(ctx, `SELECT next_sequence, last_round FROM snapshots WHERE id=?`, snapshotID).Scan(&nextSeq, &currentRound); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNoSnapshot
		}
		return err
	}
	if currentRound != roundID {
		return &FieldError{Code: CodeState, ObjectID: snapshotID, FieldPath: "round_id", Measured: roundID, Message: fmt.Sprintf("active round is %d", currentRound)}
	}
	if seq != nextSeq {
		return &FieldError{
			Code: CodeState, ObjectID: snapshotID, FieldPath: "sequence",
			Measured: seq, Min: nextSeq, Max: nextSeq,
			Message: "commit sequence must advance without gaps or interleaving",
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commit_log(snapshot_id, round_id, sequence, chunk_id, reused) VALUES(?,?,?,?,?)`,
		snapshotID, roundID, seq, result.ChunkID, boolInt(reused)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_results
		(snapshot_id, chunk_id, round_id, path_digest, content_hash, params_version, algorithm_version, cache_key, result_json)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(snapshot_id, chunk_id) DO UPDATE SET round_id=excluded.round_id, path_digest=excluded.path_digest,
			content_hash=excluded.content_hash, params_version=excluded.params_version,
			algorithm_version=excluded.algorithm_version, cache_key=excluded.cache_key, result_json=excluded.result_json`,
		snapshotID, result.ChunkID, roundID, result.PathDigest, result.ContentHash,
		result.ParamsVersion, result.AlgorithmVersion, result.CacheKey, string(raw)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET next_sequence=? WHERE id=?`, seq+1, snapshotID); err != nil {
		return err
	}
	return tx.Commit()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) MarkComplete(ctx context.Context, snapshotID string, complete bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, `UPDATE snapshots SET complete=? WHERE id=?`, boolInt(complete), snapshotID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrNoSnapshot
	}
	return nil
}

func (s *Store) LoadResults(ctx context.Context, snapshotID string) ([]ChunkResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT result_json, path_digest, content_hash, params_version, algorithm_version
		FROM chunk_results WHERE snapshot_id=? ORDER BY chunk_id`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChunkResult
	for rows.Next() {
		var raw, pathDigest, contentHash, paramsVersion, algo string
		if err := rows.Scan(&raw, &pathDigest, &contentHash, &paramsVersion, &algo); err != nil {
			return nil, err
		}
		var res ChunkResult
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			return nil, &FieldError{Code: CodeState, ObjectID: snapshotID, FieldPath: "result_json", Message: err.Error()}
		}
		if got := PathDigest(res.Paths); got != pathDigest || got != res.PathDigest {
			return nil, &FieldError{
				Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", res.ChunkID), FieldPath: "path_digest",
				Measured: pathDigest, Message: "persisted path digest mismatch",
			}
		}
		if res.ContentHash != contentHash || res.ParamsVersion != paramsVersion || res.AlgorithmVersion != algo {
			return nil, &FieldError{
				Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", res.ChunkID), FieldPath: "persisted_identity",
				Message: "result columns and JSON identity disagree",
			}
		}
		out = append(out, res)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) WipeSnapshot(ctx context.Context, snapshotID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log WHERE snapshot_id=?`, snapshotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_results WHERE snapshot_id=?`, snapshotID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET complete=0, last_round=0, next_sequence=0 WHERE id=?`, snapshotID); err != nil {
		return err
	}
	return tx.Commit()
}

// InvalidateChunks removes only the affected committed rows and advances the
// snapshot identity to the edited level/truth. Unaffected rows remain valid
// recovery inputs.
func (s *Store) InvalidateChunks(ctx context.Context, snapshotID string, chunkIDs []int, levelHash, truthFingerprint, paramsVersion, algorithmVersion string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range chunkIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_results WHERE snapshot_id=? AND chunk_id=?`, snapshotID, id); err != nil {
			return err
		}
	}
	// The previous round's commit log no longer describes a complete field.
	if _, err := tx.ExecContext(ctx, `DELETE FROM commit_log WHERE snapshot_id=?`, snapshotID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE snapshots SET level_hash=?, truth_fingerprint=?, params_version=?, algorithm_version=?,
		complete=0, next_sequence=0 WHERE id=?`,
		levelHash, truthFingerprint, paramsVersion, algorithmVersion, snapshotID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNoSnapshot
	}
	return tx.Commit()
}

func (s *Store) LoadCommitLog(ctx context.Context, snapshotID string, roundID int64) ([]CommitLogEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT snapshot_id, round_id, sequence, chunk_id, reused
		FROM commit_log WHERE snapshot_id=? AND round_id=? ORDER BY sequence`, snapshotID, roundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitLogEntry
	for rows.Next() {
		var e CommitLogEntry
		var reused int
		if err := rows.Scan(&e.SnapshotID, &e.RoundID, &e.Sequence, &e.ChunkID, &reused); err != nil {
			return nil, err
		}
		e.Reused = reused != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// ValidateLoadedSnapshot verifies every persisted identity against the current
// level. A single mismatch invalidates the whole snapshot atomically.
func (s *Store) ValidateLoadedSnapshot(ctx context.Context, snapshotID string, level *Level, options Options, truthFingerprint string) ([]ChunkResult, bool, error) {
	meta, err := s.SnapshotMeta(ctx, snapshotID)
	if err != nil {
		return nil, false, err
	}
	if meta.ParamsVersion != options.Version() || meta.AlgorithmVersion != AlgorithmVersion || meta.TruthFingerprint != truthFingerprint {
		_ = s.WipeSnapshot(ctx, snapshotID)
		return nil, false, &FieldError{Code: CodeState, ObjectID: snapshotID, FieldPath: "snapshot_identity", Message: "version or truth fingerprint mismatch; snapshot invalidated"}
	}
	results, err := s.LoadResults(ctx, snapshotID)
	if err != nil {
		_ = s.WipeSnapshot(ctx, snapshotID)
		return nil, false, err
	}
	chunks := make(map[int]Chunk, len(level.Chunks))
	for _, c := range level.Chunks {
		chunks[c.ID] = c
	}
	for _, res := range results {
		c, ok := chunks[res.ChunkID]
		if !ok {
			_ = s.WipeSnapshot(ctx, snapshotID)
			return nil, false, &FieldError{Code: CodeState, ObjectID: snapshotID, FieldPath: "chunk_id", Measured: res.ChunkID, Message: "unknown persisted chunk; snapshot invalidated"}
		}
		if res.ContentHash != ChunkContentHash(&c) || res.ParamsVersion != options.Version() || res.AlgorithmVersion != AlgorithmVersion {
			_ = s.WipeSnapshot(ctx, snapshotID)
			return nil, false, &FieldError{Code: CodeState, ObjectID: fmt.Sprintf("chunk-%d", res.ChunkID), FieldPath: "content_hash", Message: "content/version mismatch; snapshot invalidated"}
		}
	}
	return results, true, nil
}

// CommitChunksSequential is a convenience wrapper used by tests and tools.
func (s *Store) CommitChunksSequential(ctx context.Context, snapshotID string, roundID int64, results []ChunkResult, reused []bool) error {
	if err := s.BeginCommitRound(ctx, snapshotID, roundID); err != nil {
		return err
	}
	for i := range results {
		r := false
		if i < len(reused) {
			r = reused[i]
		}
		if err := s.CommitChunk(ctx, snapshotID, roundID, i, results[i], r); err != nil {
			return err
		}
	}
	return nil
}
