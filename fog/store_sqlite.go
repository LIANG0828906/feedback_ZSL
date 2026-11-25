package fog

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

type CheckpointStore struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
}

type RevisionRecord struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	SnapshotVersion  string `json:"snapshot_version"`
	CameraID         string `json:"camera_id"`
	CameraVersion    string `json:"camera_version"`
	RecipeVersion    string `json:"recipe_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

type CommitEvent struct {
	EventID    int64  `json:"event_id"`
	RevisionID string `json:"revision_id"`
	Sequence   int    `json:"sequence"`
	BlockID    int    `json:"block_id"`
	CacheKey   string `json:"cache_key"`
}

const checkpointSchema = `
CREATE TABLE IF NOT EXISTS snapshot_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS revisions (
  revision_id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  snapshot_version TEXT NOT NULL,
  camera_id TEXT NOT NULL,
  camera_version TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS block_checkpoints (
  revision_id TEXT NOT NULL,
  block_id INTEGER NOT NULL,
  sequence_no INTEGER NOT NULL,
  tile_x INTEGER NOT NULL,
  tile_y INTEGER NOT NULL,
  x0 INTEGER NOT NULL,
  y0 INTEGER NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  ray_digest TEXT NOT NULL,
  segment_count INTEGER NOT NULL,
  transmittance_json TEXT NOT NULL,
  radiance_json TEXT NOT NULL,
  result_json TEXT NOT NULL,
  cache_key TEXT NOT NULL,
  segment_keys_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  medium_hashes_json TEXT NOT NULL,
  camera_version TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  steps_used INTEGER NOT NULL,
  shadow_samples_used INTEGER NOT NULL,
  status TEXT NOT NULL,
  PRIMARY KEY (revision_id, block_id),
  UNIQUE (revision_id, sequence_no)
);
CREATE TABLE IF NOT EXISTS commit_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  revision_id TEXT NOT NULL,
  sequence_no INTEGER NOT NULL,
  block_id INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  UNIQUE (revision_id, sequence_no)
);
CREATE TABLE IF NOT EXISTS block_cache (
  cache_key TEXT PRIMARY KEY,
  ray_digest TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  medium_hashes_json TEXT NOT NULL,
  camera_version TEXT NOT NULL,
  recipe_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  result_json TEXT NOT NULL
);
`

func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	dsn := path
	if path != ":memory:" {
		dsn = path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	} else {
		dsn = "file::memory:?cache=shared&_pragma=busy_timeout(5000)&_pragma=synchronous(FULL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, &Error{Code: EStore, Field: "sqlite.open", Message: err.Error()}
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(checkpointSchema); err != nil {
		_ = db.Close()
		return nil, &Error{Code: EStore, Field: "sqlite.schema", Message: err.Error()}
	}
	return &CheckpointStore{path: path, db: db}, nil
}

func (s *CheckpointStore) Close() error { return s.db.Close() }

func TargetRevisionID(sc *Scenario, cameraID string) string {
	cam, ok := sc.Camera(cameraID)
	if !ok {
		cam = sc.ActiveCamera()
	}
	return "rev-" + ContentHash("target", struct {
		Snapshot string `json:"snapshot"`
		Camera   string `json:"camera"`
	}{SnapshotVersion(sc), cam.Version})
}

func EnsureRevision(st *CheckpointStore, rev RevisionRecord) error {
	_, err := st.db.Exec(`
INSERT INTO revisions(revision_id,status,snapshot_version,camera_id,camera_version,recipe_version,algorithm_version)
VALUES(?,?,?,?,?,?,?)
ON CONFLICT(revision_id) DO UPDATE SET status=excluded.status`,
		rev.ID, rev.Status, rev.SnapshotVersion, rev.CameraID, rev.CameraVersion, rev.RecipeVersion, rev.AlgorithmVersion)
	if err != nil {
		return &Error{Code: EStore, Field: "revisions.insert", Message: err.Error()}
	}
	return nil
}

func SetRevisionStatus(st *CheckpointStore, revisionID, status string) error {
	_, err := st.db.Exec(`UPDATE revisions SET status=? WHERE revision_id=?`, status, revisionID)
	return err
}

func GetRevision(st *CheckpointStore, revisionID string) (RevisionRecord, error) {
	var rev RevisionRecord
	err := st.db.QueryRow(`SELECT revision_id,status,snapshot_version,camera_id,camera_version,recipe_version,algorithm_version FROM revisions WHERE revision_id=?`, revisionID).
		Scan(&rev.ID, &rev.Status, &rev.SnapshotVersion, &rev.CameraID, &rev.CameraVersion, &rev.RecipeVersion, &rev.AlgorithmVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return rev, err
	}
	if err != nil {
		return rev, &Error{Code: EStore, Field: "revisions.select", Message: err.Error()}
	}
	return rev, nil
}

// CommitBlock atomically writes the checkpoint, immutable cache row, and
// ordered commit event in one SQLite transaction. The caller is expected to
// invoke blocks in ascending canonical block order.
func (s *CheckpointStore) CommitBlock(revisionID string, br *BlockResult, sequence int) error {
	if br == nil || br.Error != nil {
		return &Error{Code: EBlock, Field: "checkpoint.commit", Message: "refusing to commit failed or nil block", Detail: br}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	transJSON, err := json.Marshal(br.Transmittance)
	if err != nil {
		return err
	}
	radJSON, err := json.Marshal(br.Radiance)
	if err != nil {
		return err
	}
	resultJSON, err := json.Marshal(br)
	if err != nil {
		return err
	}
	mediumJSON, err := json.Marshal(br.Cache.MediumHashes)
	if err != nil {
		return err
	}
	segmentKeysJSON, err := json.Marshal(br.Cache.SegmentKeys)
	if err != nil {
		return err
	}
	contentHash := br.Cache.SnapshotVersion
	if contentHash == "" {
		contentHash = SnapshotVersionFromHashes(br.Cache.MediumHashes, br.Cache.CameraVersion, br.Cache.RecipeVersion)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return &Error{Code: EStore, BlockID: blockIDPtr(br.BlockID), Field: "sqlite.begin", Message: err.Error()}
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`
INSERT INTO block_checkpoints(
 revision_id,block_id,sequence_no,tile_x,tile_y,x0,y0,width,height,ray_digest,segment_count,
 transmittance_json,radiance_json,result_json,cache_key,segment_keys_json,content_hash,
 medium_hashes_json,camera_version,recipe_version,algorithm_version,steps_used,
 shadow_samples_used,status)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(revision_id,block_id) DO UPDATE SET
 sequence_no=excluded.sequence_no, ray_digest=excluded.ray_digest,
 transmittance_json=excluded.transmittance_json, radiance_json=excluded.radiance_json,
 result_json=excluded.result_json, cache_key=excluded.cache_key,
 segment_keys_json=excluded.segment_keys_json, content_hash=excluded.content_hash,
 medium_hashes_json=excluded.medium_hashes_json, camera_version=excluded.camera_version,
 recipe_version=excluded.recipe_version, algorithm_version=excluded.algorithm_version,
 steps_used=excluded.steps_used, shadow_samples_used=excluded.shadow_samples_used,
 status=excluded.status`,
		revisionID, br.BlockID, sequence, br.TX, br.TY, br.X0, br.Y0, br.Width, br.Height,
		br.RayDigest, br.SegmentCount, string(transJSON), string(radJSON), string(resultJSON),
		br.Cache.BlockKey, string(segmentKeysJSON), contentHash, string(mediumJSON),
		br.Cache.CameraVersion, br.Cache.RecipeVersion, br.Cache.AlgorithmVersion,
		br.StepsUsed, br.ShadowSamplesUsed, br.Status)
	if err != nil {
		return &Error{Code: EStore, BlockID: blockIDPtr(br.BlockID), Field: "block_checkpoints.upsert", Message: err.Error()}
	}
	_, err = tx.Exec(`
INSERT INTO commit_events(revision_id,sequence_no,block_id,cache_key)
VALUES(?,?,?,?)
ON CONFLICT(revision_id,sequence_no) DO UPDATE SET block_id=excluded.block_id,cache_key=excluded.cache_key`,
		revisionID, sequence, br.BlockID, br.Cache.BlockKey)
	if err != nil {
		return &Error{Code: EStore, BlockID: blockIDPtr(br.BlockID), Field: "commit_events.insert", Message: err.Error()}
	}
	_, err = tx.Exec(`
INSERT INTO block_cache(cache_key,ray_digest,content_hash,medium_hashes_json,camera_version,recipe_version,algorithm_version,result_json)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(cache_key) DO UPDATE SET result_json=excluded.result_json`,
		br.Cache.BlockKey, br.RayDigest, contentHash, string(mediumJSON), br.Cache.CameraVersion,
		br.Cache.RecipeVersion, br.Cache.AlgorithmVersion, string(resultJSON))
	if err != nil {
		return &Error{Code: EStore, BlockID: blockIDPtr(br.BlockID), Field: "block_cache.upsert", Message: err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return &Error{Code: EStore, BlockID: blockIDPtr(br.BlockID), Field: "sqlite.commit", Message: err.Error()}
	}
	return nil
}

func SnapshotVersionFromHashes(hashes map[string]string, cameraVersion, recipeVersion string) string {
	return ContentHash("snapshot", struct {
		Media  map[string]string `json:"media"`
		Camera string            `json:"camera"`
		Recipe string            `json:"recipe"`
		Algo   string            `json:"algorithm"`
	}{hashes, cameraVersion, recipeVersion, AlgorithmVersion})
}

func (s *CheckpointStore) CommitSequence(revisionID string) ([]int, error) {
	rows, err := s.db.Query(`SELECT block_id FROM commit_events WHERE revision_id=? ORDER BY event_id ASC`, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *CheckpointStore) LoadCommittedBlocks(revisionID string) (map[int]*BlockResult, error) {
	rows, err := s.db.Query(`SELECT result_json FROM block_checkpoints WHERE revision_id=? ORDER BY sequence_no ASC`, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]*BlockResult{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var br BlockResult
		if err := json.Unmarshal([]byte(raw), &br); err != nil {
			return nil, &Error{Code: EDecode, Field: "block_checkpoints.result_json", Message: err.Error()}
		}
		out[br.BlockID] = &br
	}
	return out, rows.Err()
}

func (s *CheckpointStore) GetCache(cacheKey string) (*BlockResult, bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT result_json FROM block_cache WHERE cache_key=?`, cacheKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, &Error{Code: EStore, Field: "block_cache.select", Message: err.Error()}
	}
	var br BlockResult
	if err := json.Unmarshal([]byte(raw), &br); err != nil {
		return nil, false, &Error{Code: EDecode, Field: "block_cache.result_json", Message: err.Error()}
	}
	br.CacheHit = true
	return &br, true, nil
}

// ValidateRecovery rejects an entire revision when any committed block has a
// mismatching content hash or parameter/algorithm version.
func (s *CheckpointStore) ValidateRecovery(revisionID string, sc *Scenario) (RevisionRecord, error) {
	rev, err := GetRevision(s, revisionID)
	if err != nil {
		return rev, err
	}
	cam := sc.ActiveCamera()
	if rev.CameraVersion != cam.Version || rev.RecipeVersion != sc.Recipe.Version || rev.AlgorithmVersion != AlgorithmVersion {
		_ = SetRevisionStatus(s, revisionID, "invalid_version")
		return rev, &Error{Code: EStore, Field: "revision.versions", Message: fmt.Sprintf("stored=%+v current camera=%s recipe=%s algo=%s", rev, cam.Version, sc.Recipe.Version, AlgorithmVersion)}
	}
	blocks, err := s.LoadCommittedBlocks(revisionID)
	if err != nil {
		return rev, err
	}
	current := map[string]*Medium{}
	for _, m := range sc.Media {
		current[m.ID] = m
	}
	expectedSnapshot := SnapshotVersion(sc)
	for blockID, br := range blocks {
		if br.Cache.CameraVersion != cam.Version || br.Cache.RecipeVersion != sc.Recipe.Version || br.Cache.AlgorithmVersion != AlgorithmVersion {
			_ = SetRevisionStatus(s, revisionID, "invalid_version")
			return rev, &Error{Code: EStore, BlockID: blockIDPtr(blockID), Field: "block.versions", Message: "checkpoint version mismatch"}
		}
		var storedContentHash string
		if err := s.db.QueryRow(`SELECT content_hash FROM block_checkpoints WHERE revision_id=? AND block_id=?`, revisionID, blockID).Scan(&storedContentHash); err != nil {
			return rev, &Error{Code: EStore, BlockID: blockIDPtr(blockID), Field: "content_hash", Message: err.Error()}
		}
		if storedContentHash != expectedSnapshot || br.Cache.SnapshotVersion != expectedSnapshot {
			_ = SetRevisionStatus(s, revisionID, "invalid_content_hash")
			return rev, &Error{
				Code: EStore, BlockID: blockIDPtr(blockID), Field: "content_hash",
				Message: "checkpoint snapshot content hash mismatch; whole revision invalidated",
				Detail:  map[string]any{"stored_hash": storedContentHash, "current_hash": expectedSnapshot, "cache_hash": br.Cache.SnapshotVersion},
			}
		}
		var storedHashes map[string]string
		var rawHashes string
		if err := s.db.QueryRow(`SELECT medium_hashes_json FROM block_checkpoints WHERE revision_id=? AND block_id=?`, revisionID, blockID).Scan(&rawHashes); err != nil {
			return rev, &Error{Code: EStore, BlockID: blockIDPtr(blockID), Field: "medium_hashes_json", Message: err.Error()}
		}
		if err := json.Unmarshal([]byte(rawHashes), &storedHashes); err != nil {
			return rev, &Error{Code: EDecode, BlockID: blockIDPtr(blockID), Field: "medium_hashes_json", Message: err.Error()}
		}
		for mediumID, storedHash := range storedHashes {
			m, ok := current[mediumID]
			if !ok || m.ContentHash != storedHash {
				_ = SetRevisionStatus(s, revisionID, "invalid_content_hash")
				return rev, &Error{
					Code: EStore, MediaID: mediumID, BlockID: blockIDPtr(blockID),
					Field: "content_hash", Expected: 0, Actual: 0,
					Message: "checkpoint content hash mismatch; whole snapshot revision invalidated",
					Detail: map[string]string{"stored_hash": storedHash, "current_hash": func() string {
						if ok {
							return m.ContentHash
						}
						return "<missing>"
					}()},
				}
			}
		}
	}
	return rev, nil
}
