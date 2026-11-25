package audiofp

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

type SnapshotMode int

const (
	SnapshotFresh SnapshotMode = iota
	SnapshotResume
	SnapshotIncremental
)

type SnapshotMeta struct {
	ProjectID        string `json:"project_id"`
	ProjectHash      string `json:"project_hash"`
	RecipeVersion    string `json:"recipe_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	LayoutVersion    string `json:"layout_version"`
}

func (m SnapshotMeta) fingerprint() string {
	return digestString("snapshot|" + m.ProjectID + "|" + m.ProjectHash + "|" +
		m.RecipeVersion + "|" + m.AlgorithmVersion + "|" + m.LayoutVersion)
}

type SnapshotPreparation struct {
	Existing        bool   `json:"existing"`
	Reset           bool   `json:"reset"`
	ResetReason     string `json:"reset_reason,omitempty"`
	AllowCacheReuse bool   `json:"allow_cache_reuse"`
}

type ShardPayload struct {
	AssetID      string             `json:"asset_id"`
	TotalSamples int                `json:"total_samples"`
	FirstFrame   int                `json:"first_frame"`
	LastFrame    int                `json:"last_frame"`
	Frames       []FrameFingerprint `json:"frames"`
	Index        []IndexEntry       `json:"index"`
}

type ShardCommit struct {
	ProjectID        string
	Seq              int
	AssetID          string
	ShardNo          int
	CacheKey         string
	ContentHash      string
	RecipeVersion    string
	AlgorithmVersion string
	FirstFrame       int
	LastFrame        int
	Payload          ShardPayload
}

type CommitRef struct {
	Seq        int    `json:"seq"`
	AssetID    string `json:"asset_id"`
	ShardNo    int    `json:"shard_no"`
	CacheKey   string `json:"cache_key"`
	FirstFrame int    `json:"first_frame"`
	LastFrame  int    `json:"last_frame"`
}

type CachedShard struct {
	Payload          ShardPayload
	CacheKey         string
	ContentHash      string
	RecipeVersion    string
	AlgorithmVersion string
	Digest           string
	FromStore        bool
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			project_id TEXT PRIMARY KEY,
			project_hash TEXT NOT NULL,
			recipe_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			layout_version TEXT NOT NULL,
			fingerprint TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS cache_shards (
			cache_key TEXT PRIMARY KEY,
			asset_id TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			recipe_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			first_frame INTEGER NOT NULL,
			last_frame INTEGER NOT NULL,
			payload TEXT NOT NULL,
			payload_digest TEXT NOT NULL,
			frames_digest TEXT NOT NULL,
			index_digest TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS snapshot_shards (
			project_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			asset_id TEXT NOT NULL,
			shard_no INTEGER NOT NULL,
			cache_key TEXT NOT NULL,
			first_frame INTEGER NOT NULL,
			last_frame INTEGER NOT NULL,
			PRIMARY KEY (project_id, seq),
			UNIQUE (project_id, asset_id, shard_no)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

// PrepareSnapshot implements resume, fresh, and incremental modes. Resume mode
// rejects the entire snapshot if any version, content, or layout field differs.
func (s *Store) PrepareSnapshot(meta SnapshotMeta, mode SnapshotMode) (SnapshotPreparation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return SnapshotPreparation{}, err
	}
	defer tx.Rollback()
	_, existingFingerprint, ok, err := readSnapshotMeta(tx, meta.ProjectID)
	if err != nil {
		return SnapshotPreparation{}, err
	}
	result := SnapshotPreparation{}
	shouldWrite := true
	switch mode {
	case SnapshotResume:
		if ok && existingFingerprint == meta.fingerprint() {
			result.Existing = true
			result.AllowCacheReuse = true
			shouldWrite = false
		} else if ok {
			result.Reset = true
			result.ResetReason = fmt.Sprintf("snapshot version mismatch: have %s want %s", existingFingerprint, meta.fingerprint())
			if _, err := tx.Exec(`DELETE FROM snapshot_shards WHERE project_id=?`, meta.ProjectID); err != nil {
				return SnapshotPreparation{}, err
			}
		}
	case SnapshotIncremental:
		result.AllowCacheReuse = true
		if _, err := tx.Exec(`DELETE FROM snapshot_shards WHERE project_id=?`, meta.ProjectID); err != nil {
			return SnapshotPreparation{}, err
		}
	case SnapshotFresh:
		if _, err := tx.Exec(`DELETE FROM snapshot_shards WHERE project_id=?`, meta.ProjectID); err != nil {
			return SnapshotPreparation{}, err
		}
		if _, err := tx.Exec(`DELETE FROM snapshots WHERE project_id=?`, meta.ProjectID); err != nil {
			return SnapshotPreparation{}, err
		}
	default:
		return SnapshotPreparation{}, parameterError("snapshot_mode", "unsupported snapshot mode")
	}
	if shouldWrite {
		if _, err := tx.Exec(`INSERT INTO snapshots
			(project_id,project_hash,recipe_version,algorithm_version,layout_version,fingerprint)
			VALUES (?,?,?,?,?,?)
			ON CONFLICT(project_id) DO UPDATE SET
			project_hash=excluded.project_hash,
			recipe_version=excluded.recipe_version,
			algorithm_version=excluded.algorithm_version,
			layout_version=excluded.layout_version,
			fingerprint=excluded.fingerprint`,
			meta.ProjectID, meta.ProjectHash, meta.RecipeVersion, meta.AlgorithmVersion,
			meta.LayoutVersion, meta.fingerprint()); err != nil {
			return SnapshotPreparation{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return SnapshotPreparation{}, err
	}
	return result, nil
}

func readSnapshotMeta(tx *sql.Tx, projectID string) (SnapshotMeta, string, bool, error) {
	var m SnapshotMeta
	var fingerprint string
	err := tx.QueryRow(`SELECT project_hash,recipe_version,algorithm_version,layout_version,fingerprint
		FROM snapshots WHERE project_id=?`, projectID).Scan(
		&m.ProjectHash, &m.RecipeVersion, &m.AlgorithmVersion, &m.LayoutVersion, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotMeta{}, "", false, nil
	}
	if err != nil {
		return SnapshotMeta{}, "", false, err
	}
	m.ProjectID = projectID
	return m, fingerprint, true, nil
}

// LoadCachedShard returns a persistent shard only if every version and range
// field matches the requested cache key.
func (s *Store) LoadCachedShard(ctx context.Context, expected ShardCommit) (*CachedShard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		assetID, contentHash, recipeVersion, algorithmVersion string
		first, last                                           int
		payloadText, payloadDigest, framesDigest, indexDigest string
	)
	err := s.db.QueryRowContext(ctx, `SELECT asset_id,content_hash,recipe_version,algorithm_version,
		first_frame,last_frame,payload,payload_digest,frames_digest,index_digest
		FROM cache_shards WHERE cache_key=?`, expected.CacheKey).Scan(
		&assetID, &contentHash, &recipeVersion, &algorithmVersion,
		&first, &last, &payloadText, &payloadDigest, &framesDigest, &indexDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if assetID != expected.AssetID || contentHash != expected.ContentHash ||
		recipeVersion != expected.RecipeVersion || algorithmVersion != expected.AlgorithmVersion ||
		first != expected.FirstFrame || last != expected.LastFrame {
		return nil, DiffError(expected.AssetID, expected.FirstFrame, "cache_metadata",
			"persisted shard metadata does not match expected cache key inputs")
	}
	if digestString(payloadText) != payloadDigest {
		return nil, DiffError(expected.AssetID, expected.FirstFrame, "payload_digest",
			"persisted shard payload digest mismatch")
	}
	var payload ShardPayload
	if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
		return nil, DiffError(expected.AssetID, expected.FirstFrame, "payload", err.Error())
	}
	if payload.AssetID != expected.AssetID || payload.FirstFrame != expected.FirstFrame ||
		payload.LastFrame != expected.LastFrame || len(payload.Frames) != expected.LastFrame-expected.FirstFrame {
		return nil, DiffError(expected.AssetID, expected.FirstFrame, "payload_range",
			"persisted shard payload range mismatch")
	}
	framesJSON, _ := json.Marshal(payload.Frames)
	indexJSON, _ := json.Marshal(payload.Index)
	if digest(framesJSON) != framesDigest || digest(indexJSON) != indexDigest {
		return nil, DiffError(expected.AssetID, expected.FirstFrame, "content_digest",
			"persisted shard frames or index digest mismatch")
	}
	return &CachedShard{
		Payload: payload, CacheKey: expected.CacheKey, ContentHash: contentHash,
		RecipeVersion: recipeVersion, AlgorithmVersion: algorithmVersion,
		Digest: payloadDigest, FromStore: true,
	}, nil
}

// CommitShard atomically writes one cache shard and its snapshot commit row.
func (s *Store) CommitShard(ctx context.Context, commit ShardCommit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payloadBytes, err := json.Marshal(commit.Payload)
	if err != nil {
		return err
	}
	framesBytes, _ := json.Marshal(commit.Payload.Frames)
	indexBytes, _ := json.Marshal(commit.Payload.Index)
	payloadDigest := digest(payloadBytes)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existingDigest string
	err = tx.QueryRowContext(ctx, `SELECT payload_digest FROM cache_shards WHERE cache_key=?`,
		commit.CacheKey).Scan(&existingDigest)
	if err == nil {
		if existingDigest != payloadDigest {
			return DiffError(commit.AssetID, commit.FirstFrame, "cache_key_collision",
				"same cache key produced different payload")
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO cache_shards
			(cache_key,asset_id,content_hash,recipe_version,algorithm_version,
			 first_frame,last_frame,payload,payload_digest,frames_digest,index_digest)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			commit.CacheKey, commit.AssetID, commit.ContentHash, commit.RecipeVersion,
			commit.AlgorithmVersion, commit.FirstFrame, commit.LastFrame, string(payloadBytes),
			payloadDigest, digest(framesBytes), digest(indexBytes)); err != nil {
			return err
		}
	} else {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_shards
		(project_id,seq,asset_id,shard_no,cache_key,first_frame,last_frame)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(project_id,seq) DO UPDATE SET
		asset_id=excluded.asset_id,shard_no=excluded.shard_no,cache_key=excluded.cache_key,
		first_frame=excluded.first_frame,last_frame=excluded.last_frame`,
		commit.ProjectID, commit.Seq, commit.AssetID, commit.ShardNo,
		commit.CacheKey, commit.FirstFrame, commit.LastFrame); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitSequence(projectID string) ([]CommitRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq,asset_id,shard_no,cache_key,first_frame,last_frame
		FROM snapshot_shards WHERE project_id=? ORDER BY seq`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRef
	for rows.Next() {
		var c CommitRef
		if err := rows.Scan(&c.Seq, &c.AssetID, &c.ShardNo, &c.CacheKey, &c.FirstFrame, &c.LastFrame); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LoadCommittedShards returns all persisted shard payloads in stable commit
// sequence order.
func (s *Store) LoadCommittedShards(ctx context.Context, projectID string) ([]CachedShard, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT ss.cache_key,c.payload_digest,c.payload,
		c.content_hash,c.recipe_version,c.algorithm_version
		FROM snapshot_shards ss JOIN cache_shards c ON c.cache_key=ss.cache_key
		WHERE ss.project_id=? ORDER BY ss.seq`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CachedShard
	for rows.Next() {
		var key, wantDigest, payloadText, contentHash, recipeVersion, algorithmVersion string
		if err := rows.Scan(&key, &wantDigest, &payloadText, &contentHash, &recipeVersion, &algorithmVersion); err != nil {
			return nil, err
		}
		if digestString(payloadText) != wantDigest {
			return nil, DiffError("", 0, "payload_digest", "persisted shard payload digest mismatch")
		}
		var payload ShardPayload
		if err := json.Unmarshal([]byte(payloadText), &payload); err != nil {
			return nil, err
		}
		out = append(out, CachedShard{
			Payload: payload, CacheKey: key, ContentHash: contentHash,
			RecipeVersion: recipeVersion, AlgorithmVersion: algorithmVersion,
			Digest: wantDigest, FromStore: true,
		})
	}
	return out, rows.Err()
}

func (s *Store) SnapshotMeta(projectID string) (SnapshotMeta, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m SnapshotMeta
	var fingerprint string
	err := s.db.QueryRow(`SELECT project_hash,recipe_version,algorithm_version,layout_version,fingerprint
		FROM snapshots WHERE project_id=?`, projectID).Scan(
		&m.ProjectHash, &m.RecipeVersion, &m.AlgorithmVersion, &m.LayoutVersion, &fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotMeta{}, false, nil
	}
	if err != nil {
		return SnapshotMeta{}, false, err
	}
	m.ProjectID = projectID
	return m, true, nil
}

// ClearProject removes snapshot state but leaves content-addressed cache
// entries available for a deliberate incremental rebuild.
func (s *Store) ClearProject(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM snapshot_shards WHERE project_id=?`, projectID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshots WHERE project_id=?`, projectID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearAll is used when a resume detects corrupt/version-mismatched data and a
// full rebuild is required.
func (s *Store) ClearAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"snapshot_shards", "snapshots", "cache_shards"} {
		if _, err := tx.Exec(`DELETE FROM ` + table); err != nil {
			return err
		}
	}
	return tx.Commit()
}
