package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"volumekernel/internal/model"
)

type Meta struct {
	SessionID        string
	SceneHash        string
	CameraVersion    string
	RecipeVersion    string
	AlgorithmVersion string
}

type SessionState struct {
	Invalidated bool
	Reason      string
	Blocks      int
}

type BlockRecord struct {
	Seq         int
	X           int
	Y           int
	RayDigest   string
	CacheKey    string
	ContentHash string
	Recipe      string
	Algorithm   string
	Block       model.BlockResult
}

type Store struct {
	mu      sync.Mutex
	db      *sql.DB
	meta    *Meta
	nextSeq int
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS session_meta (
			session_id TEXT PRIMARY KEY,
			scene_hash TEXT NOT NULL,
			camera_version TEXT NOT NULL,
			recipe_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoint_blocks (
			session_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			block_x INTEGER NOT NULL,
			block_y INTEGER NOT NULL,
			ray_digest TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			recipe_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			pixels_json TEXT NOT NULL,
			PRIMARY KEY(session_id, block_x, block_y)
		)`,
		`CREATE TABLE IF NOT EXISTS block_cache (
			cache_key TEXT PRIMARY KEY,
			block_x INTEGER NOT NULL,
			block_y INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			parameter_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			block_json TEXT NOT NULL,
			deps_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_block_cache_xy ON block_cache(block_x, block_y)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("sqlite init: %w", err)
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) ResetBlocks(sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID == "" {
		sessionID = "active-render"
	}
	if err := s.purgeSessionLocked(sessionID); err != nil {
		return err
	}
	s.nextSeq = 0
	return nil
}

func (s *Store) BeginSession(meta Meta) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta.SessionID == "" {
		meta.SessionID = "active-render"
	}
	var old Meta
	var updated string
	err := s.db.QueryRow(`SELECT scene_hash,camera_version,recipe_version,algorithm_version,updated_at FROM session_meta WHERE session_id=?`, meta.SessionID).
		Scan(&old.SceneHash, &old.CameraVersion, &old.RecipeVersion, &old.AlgorithmVersion, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = s.db.Exec(`INSERT INTO session_meta(session_id,scene_hash,camera_version,recipe_version,algorithm_version,updated_at) VALUES(?,?,?,?,?,?)`,
			meta.SessionID, meta.SceneHash, meta.CameraVersion, meta.RecipeVersion, meta.AlgorithmVersion, time.Now().UTC().Format(time.RFC3339Nano))
		if err != nil {
			return SessionState{}, err
		}
		s.meta = &meta
		s.nextSeq = 0
		return SessionState{}, nil
	}
	if err != nil {
		return SessionState{}, err
	}
	old.SessionID = meta.SessionID
	if old.SceneHash != meta.SceneHash || old.CameraVersion != meta.CameraVersion ||
		old.RecipeVersion != meta.RecipeVersion || old.AlgorithmVersion != meta.AlgorithmVersion {
		reason := fmt.Sprintf("session meta mismatch: scene %s->%s camera %s->%s recipe %s->%s algorithm %s->%s",
			short(old.SceneHash), short(meta.SceneHash), short(old.CameraVersion), short(meta.CameraVersion),
			old.RecipeVersion, meta.RecipeVersion, old.AlgorithmVersion, meta.AlgorithmVersion)
		if err := s.purgeSessionLocked(meta.SessionID); err != nil {
			return SessionState{}, err
		}
		_, err = s.db.Exec(`UPDATE session_meta SET scene_hash=?,camera_version=?,recipe_version=?,algorithm_version=?,updated_at=? WHERE session_id=?`,
			meta.SceneHash, meta.CameraVersion, meta.RecipeVersion, meta.AlgorithmVersion, time.Now().UTC().Format(time.RFC3339Nano), meta.SessionID)
		if err != nil {
			return SessionState{}, err
		}
		s.meta = &meta
		s.nextSeq = 0
		return SessionState{Invalidated: true, Reason: reason}, nil
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_blocks WHERE session_id=?`, meta.SessionID).Scan(&count); err != nil {
		return SessionState{}, err
	}
	var maxSeq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM checkpoint_blocks WHERE session_id=?`, meta.SessionID).Scan(&maxSeq); err != nil {
		return SessionState{}, err
	}
	if maxSeq.Valid {
		s.nextSeq = int(maxSeq.Int64) + 1
	}
	s.meta = &meta
	return SessionState{Blocks: count}, nil
}

func (s *Store) purgeSessionLocked(sessionID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM checkpoint_blocks WHERE session_id=?`, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitBlock(rec BlockRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta == nil {
		return fmt.Errorf("checkpoint session has not been started")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sceneHash, cameraVersion, recipeVersion, algorithmVersion string
	err = tx.QueryRow(`SELECT scene_hash,camera_version,recipe_version,algorithm_version FROM session_meta WHERE session_id=?`, s.meta.SessionID).
		Scan(&sceneHash, &cameraVersion, &recipeVersion, &algorithmVersion)
	if err != nil {
		return err
	}
	if sceneHash != s.meta.SceneHash || cameraVersion != s.meta.CameraVersion ||
		recipeVersion != s.meta.RecipeVersion || algorithmVersion != s.meta.AlgorithmVersion {
		return fmt.Errorf("checkpoint session changed during render")
	}
	if rec.Seq == 0 {
		rec.Seq = s.nextSeq
	}
	if rec.Seq != s.nextSeq {
		return fmt.Errorf("checkpoint commit order violation: got seq %d want %d", rec.Seq, s.nextSeq)
	}
	if rec.ContentHash != s.meta.SceneHash || rec.Recipe != s.meta.RecipeVersion || rec.Algorithm != s.meta.AlgorithmVersion {
		return fmt.Errorf("checkpoint block metadata does not match session")
	}
	pixels, err := json.Marshal(rec.Block)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO checkpoint_blocks
		(session_id,seq,block_x,block_y,ray_digest,cache_key,content_hash,recipe_version,algorithm_version,pixels_json)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		s.meta.SessionID, rec.Seq, rec.X, rec.Y, rec.RayDigest, rec.CacheKey, rec.ContentHash, rec.Recipe, rec.Algorithm, string(pixels))
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.nextSeq++
	return nil
}

func (s *Store) LoadCommitted(meta Meta) ([]BlockRecord, SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta.SessionID == "" {
		meta.SessionID = "active-render"
	}
	var sceneHash, cameraVersion, recipeVersion, algorithmVersion string
	err := s.db.QueryRow(`SELECT scene_hash,camera_version,recipe_version,algorithm_version FROM session_meta WHERE session_id=?`, meta.SessionID).
		Scan(&sceneHash, &cameraVersion, &recipeVersion, &algorithmVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, SessionState{}, nil
	}
	if err != nil {
		return nil, SessionState{}, err
	}
	if sceneHash != meta.SceneHash || cameraVersion != meta.CameraVersion ||
		recipeVersion != meta.RecipeVersion || algorithmVersion != meta.AlgorithmVersion {
		reason := "checkpoint metadata mismatch; all checkpointed blocks invalidated"
		if err := s.purgeSessionLocked(meta.SessionID); err != nil {
			return nil, SessionState{}, err
		}
		return nil, SessionState{Invalidated: true, Reason: reason}, nil
	}
	rows, err := s.db.Query(`SELECT seq,block_x,block_y,ray_digest,cache_key,content_hash,recipe_version,algorithm_version,pixels_json
		FROM checkpoint_blocks WHERE session_id=? ORDER BY seq`, meta.SessionID)
	if err != nil {
		return nil, SessionState{}, err
	}
	var out []BlockRecord
	valid := true
	var scanErr error
	for rows.Next() {
		var rec BlockRecord
		var payload string
		if err := rows.Scan(&rec.Seq, &rec.X, &rec.Y, &rec.RayDigest, &rec.CacheKey, &rec.ContentHash, &rec.Recipe, &rec.Algorithm, &payload); err != nil {
			scanErr = err
			valid = false
			break
		}
		if rec.ContentHash != meta.SceneHash || rec.Recipe != meta.RecipeVersion || rec.Algorithm != meta.AlgorithmVersion {
			valid = false
			break
		}
		if err := json.Unmarshal([]byte(payload), &rec.Block); err != nil {
			valid = false
			break
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil && scanErr == nil {
		scanErr = err
	}
	if err := rows.Close(); err != nil && scanErr == nil {
		scanErr = err
	}
	if scanErr != nil {
		return nil, SessionState{}, scanErr
	}
	if !valid {
		if err := s.purgeSessionLocked(meta.SessionID); err != nil {
			return nil, SessionState{}, err
		}
		return nil, SessionState{Invalidated: true, Reason: "per-block content hash or version mismatch; session invalidated"}, nil
	}
	return out, SessionState{Blocks: len(out)}, nil
}

func (s *Store) GetCache(cacheKey, parameterVersion, algorithmVersion string) (model.BlockResult, string, bool, error) {
	var blockJSON, depsJSON, contentHash, gotParam, gotAlgo string
	var bx, by int
	err := s.db.QueryRow(`SELECT block_x,block_y,content_hash,parameter_version,algorithm_version,block_json,deps_json
		FROM block_cache WHERE cache_key=?`, cacheKey).
		Scan(&bx, &by, &contentHash, &gotParam, &gotAlgo, &blockJSON, &depsJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return model.BlockResult{}, "", false, nil
	}
	if err != nil {
		return model.BlockResult{}, "", false, err
	}
	if gotParam != parameterVersion || gotAlgo != algorithmVersion {
		_, _ = s.db.Exec(`DELETE FROM block_cache WHERE cache_key=?`, cacheKey)
		return model.BlockResult{}, "", false, nil
	}
	var block model.BlockResult
	if err := json.Unmarshal([]byte(blockJSON), &block); err != nil {
		_, _ = s.db.Exec(`DELETE FROM block_cache WHERE cache_key=?`, cacheKey)
		return model.BlockResult{}, "", false, nil
	}
	if block.X != bx || block.Y != by || block.CacheKey != cacheKey || block.ContentHash != contentHash {
		_, _ = s.db.Exec(`DELETE FROM block_cache WHERE cache_key=?`, cacheKey)
		return model.BlockResult{}, "", false, nil
	}
	return block, depsJSON, true, nil
}

func (s *Store) PutCache(cacheKey, parameterVersion, contentHash, algorithmVersion string, block model.BlockResult, deps []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(block)
	if err != nil {
		return err
	}
	d, err := json.Marshal(deps)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO block_cache
		(cache_key,block_x,block_y,content_hash,parameter_version,algorithm_version,block_json,deps_json,created_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(cache_key) DO UPDATE SET
		block_json=excluded.block_json,deps_json=excluded.deps_json,created_at=excluded.created_at`,
		cacheKey, block.X, block.Y, contentHash, parameterVersion, algorithmVersion, string(b), string(d), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeleteCacheForBlocks(blocks [][2]int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, b := range blocks {
		res, err := tx.Exec(`DELETE FROM block_cache WHERE block_x=? AND block_y=?`, b[0], b[1])
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return total, err
	}
	return total, nil
}

func (s *Store) CacheCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM block_cache`).Scan(&n)
	return n, err
}

func (s *Store) CheckpointCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoint_blocks`).Scan(&n)
	return n, err
}

func (s *Store) PurgeCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`DELETE FROM block_cache`)
	return err
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
