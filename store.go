package textureconv

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// VersionInfo is the snapshot-wide identity. A mismatch in any field causes
// the complete snapshot to be discarded.
type VersionInfo struct {
	ImageID          string `json:"imageId"`
	SourceHash       string `json:"sourceHash"`
	PlanFingerprint  string `json:"planFingerprint"`
	ParamsVersion    string `json:"paramsVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	Geometry         string `json:"geometry"`
}

type StageRecord struct {
	Tile             int              `json:"tile"`
	Entry            int              `json:"entry"`
	X                int              `json:"x"`
	Y                int              `json:"y"`
	W                int              `json:"w"`
	H                int              `json:"h"`
	BitDepth         BitDepth         `json:"bitDepth"`
	Channels         int              `json:"channels"`
	AlphaIndex       int              `json:"alphaIndex"`
	CacheKey         string           `json:"cacheKey"`
	ContentHash      string           `json:"contentHash"`
	PlanFingerprint  string           `json:"planFingerprint"`
	ParamsVersion    string           `json:"paramsVersion"`
	AlgorithmVersion string           `json:"algorithmVersion"`
	Pixels           []uint16         `json:"-"`
	Dropped          []DroppedChannel `json:"dropped,omitempty"`
}

func (r StageRecord) StageImage() *StageImage {
	return &StageImage{
		Width: r.W, Height: r.H, BitDepth: r.BitDepth, Channels: r.Channels,
		AlphaIndex: r.AlphaIndex, Pixels: append([]uint16(nil), r.Pixels...),
		Dropped: append([]DroppedChannel(nil), r.Dropped...),
	}
}

type TileCommit struct {
	Tile        int           `json:"tile"`
	SubmitOrder int           `json:"submitOrder"`
	CommitHash  string        `json:"commitHash"`
	Stages      []StageRecord `json:"stages"`
}

// Store is a single-snapshot SQLite checkpoint store. All writes are
// serialized even though the engine computes tiles in parallel.
type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	path string
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, paramErr(0, "dbPath", "database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, ioErr("createDBDir", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, ioErr("openStore", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, ioErr("sqlitePragma", err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.initSchema(); err != nil {
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

// ReadAll returns the persisted snapshot and chunk rows without mutating
// storage. The independent verifier uses this read-only path.
func (s *Store) ReadAll() (VersionInfo, []TileCommit, error) {
	if s == nil {
		return VersionInfo{}, nil, stateErr(0, 0, "checkpoint store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok, err := s.readSnapshotLocked()
	if err != nil {
		return VersionInfo{}, nil, err
	}
	if !ok {
		return VersionInfo{}, nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "snapshot", Msg: "no checkpoint snapshot exists"}
	}
	rows, err := s.loadLocked(v)
	if err != nil {
		return v, nil, &Error{Code: CodeDiff, Entry: 0, Tile: -1, Field: "contentHash", Msg: err.Error(), Err: err}
	}
	return v, rows, nil
}

func (s *Store) initSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshot (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  image_id TEXT NOT NULL,
  source_hash TEXT NOT NULL,
  plan_fingerprint TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  geometry TEXT NOT NULL,
  updated_unix INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS chunk_commits (
  tile INTEGER PRIMARY KEY,
  submit_order INTEGER NOT NULL UNIQUE,
  commit_hash TEXT NOT NULL,
  x INTEGER NOT NULL,
  y INTEGER NOT NULL,
  w INTEGER NOT NULL,
  h INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stage_results (
  tile INTEGER NOT NULL,
  entry_seq INTEGER NOT NULL,
  x INTEGER NOT NULL,
  y INTEGER NOT NULL,
  w INTEGER NOT NULL,
  h INTEGER NOT NULL,
  bit_depth INTEGER NOT NULL,
  channels INTEGER NOT NULL,
  alpha_index INTEGER NOT NULL,
  cache_key TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  plan_fingerprint TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  pixels BLOB NOT NULL,
  dropped TEXT NOT NULL,
  PRIMARY KEY (tile, entry_seq),
  FOREIGN KEY (tile) REFERENCES chunk_commits(tile) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_stage_entry ON stage_results(entry_seq, tile);
`)
	if err != nil {
		return ioErr("initSchema", err)
	}
	return nil
}

// Prepare either loads a complete matching snapshot or atomically discards
// the old snapshot and creates a fresh one. It never returns a partial mix.
func (s *Store) Prepare(v VersionInfo) ([]TileCommit, bool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok, err := s.readSnapshotLocked()
	if err != nil {
		return nil, false, "", err
	}
	if ok {
		if reason := versionMismatch(current, v); reason != "" {
			if err := s.wipeLocked(); err != nil {
				return nil, false, "", err
			}
			if err := s.createSnapshotLocked(v); err != nil {
				return nil, false, "", err
			}
			return nil, true, reason, nil
		}
		rows, err := s.loadLocked(v)
		if err != nil {
			// Any structural/content mismatch invalidates the whole snapshot.
			if err := s.wipeLocked(); err != nil {
				return nil, false, "", err
			}
			if err := s.createSnapshotLocked(v); err != nil {
				return nil, false, "", err
			}
			return nil, true, err.Error(), nil
		}
		return rows, false, "", nil
	}
	if err := s.createSnapshotLocked(v); err != nil {
		return nil, false, "", err
	}
	return nil, false, "", nil
}

func versionMismatch(a, b VersionInfo) string {
	switch {
	case a.ImageID != b.ImageID:
		return "image id changed"
	case a.SourceHash != b.SourceHash:
		return "source content hash changed"
	case a.PlanFingerprint != b.PlanFingerprint:
		return "plan fingerprint changed"
	case a.ParamsVersion != b.ParamsVersion:
		return "parameter version changed"
	case a.AlgorithmVersion != b.AlgorithmVersion:
		return "algorithm version changed"
	case a.Geometry != b.Geometry:
		return "geometry changed"
	default:
		return ""
	}
}

func (s *Store) readSnapshotLocked() (VersionInfo, bool, error) {
	var v VersionInfo
	err := s.db.QueryRow(`SELECT image_id, source_hash, plan_fingerprint, params_version,
		algorithm_version, geometry FROM snapshot WHERE id=1`).
		Scan(&v.ImageID, &v.SourceHash, &v.PlanFingerprint, &v.ParamsVersion, &v.AlgorithmVersion, &v.Geometry)
	if err == sql.ErrNoRows {
		return VersionInfo{}, false, nil
	}
	if err != nil {
		return VersionInfo{}, false, ioErr("readSnapshot", err)
	}
	return v, true, nil
}

func (s *Store) createSnapshotLocked(v VersionInfo) error {
	_, err := s.db.Exec(`INSERT INTO snapshot
		(id,image_id,source_hash,plan_fingerprint,params_version,algorithm_version,geometry,updated_unix)
		VALUES (1,?,?,?,?,?,?,?)`,
		v.ImageID, v.SourceHash, v.PlanFingerprint, v.ParamsVersion, v.AlgorithmVersion, v.Geometry, time.Now().Unix())
	if err != nil {
		return ioErr("createSnapshot", err)
	}
	return nil
}

func (s *Store) wipeLocked() error {
	tx, err := s.db.Begin()
	if err != nil {
		return ioErr("beginWipe", err)
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM stage_results`,
		`DELETE FROM chunk_commits`,
		`DELETE FROM snapshot`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return ioErr("wipeSnapshot", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ioErr("commitWipe", err)
	}
	return nil
}

// Wipe discards the entire snapshot. This is explicit, never a silent fallback.
func (s *Store) Wipe() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wipeLocked()
}

func (s *Store) loadLocked(v VersionInfo) ([]TileCommit, error) {
	rows, err := s.db.Query(`SELECT tile, submit_order, commit_hash, x, y, w, h
		FROM chunk_commits ORDER BY tile`)
	if err != nil {
		return nil, ioErr("loadChunks", err)
	}
	defer rows.Close()
	commits := map[int]*TileCommit{}
	for rows.Next() {
		var c TileCommit
		var x, y, w, h int
		if err := rows.Scan(&c.Tile, &c.SubmitOrder, &c.CommitHash, &x, &y, &w, &h); err != nil {
			return nil, ioErr("scanChunk", err)
		}
		if c.SubmitOrder <= 0 {
			return nil, fmt.Errorf("chunk submit order must be positive")
		}
		c.Stages = nil
		commits[c.Tile] = &c
	}
	if err := rows.Err(); err != nil {
		return nil, ioErr("iterChunks", err)
	}
	srows, err := s.db.Query(`SELECT tile,entry_seq,x,y,w,h,bit_depth,channels,alpha_index,
		cache_key,content_hash,plan_fingerprint,params_version,algorithm_version,pixels,dropped
		FROM stage_results ORDER BY tile,entry_seq`)
	if err != nil {
		return nil, ioErr("loadStages", err)
	}
	defer srows.Close()
	for srows.Next() {
		var r StageRecord
		var blob []byte
		var droppedJSON string
		if err := srows.Scan(&r.Tile, &r.Entry, &r.X, &r.Y, &r.W, &r.H, &r.BitDepth,
			&r.Channels, &r.AlphaIndex, &r.CacheKey, &r.ContentHash, &r.PlanFingerprint,
			&r.ParamsVersion, &r.AlgorithmVersion, &blob, &droppedJSON); err != nil {
			return nil, ioErr("scanStage", err)
		}
		if r.PlanFingerprint != v.PlanFingerprint || r.ParamsVersion != v.ParamsVersion ||
			r.AlgorithmVersion != v.AlgorithmVersion {
			return nil, fmt.Errorf("stage %d/%d has mismatched version metadata", r.Tile, r.Entry)
		}
		pix, err := bytesToPixels(blob)
		if err != nil {
			return nil, err
		}
		r.Pixels = pix
		if droppedJSON != "" && droppedJSON != "null" {
			if err := json.Unmarshal([]byte(droppedJSON), &r.Dropped); err != nil {
				return nil, fmt.Errorf("stage %d/%d dropped metadata: %w", r.Tile, r.Entry, err)
			}
		}
		stage := r.StageImage()
		if got := stageTileHash(stage, Tile{Index: r.Tile, X: r.X, Y: r.Y, W: r.W, H: r.H}); got != r.ContentHash {
			return nil, fmt.Errorf("stage %d/%d content hash mismatch", r.Tile, r.Entry)
		}
		c, ok := commits[r.Tile]
		if !ok {
			return nil, fmt.Errorf("stage %d/%d has no chunk commit", r.Tile, r.Entry)
		}
		c.Stages = append(c.Stages, r)
	}
	if err := srows.Err(); err != nil {
		return nil, ioErr("iterStages", err)
	}
	var out []TileCommit
	for _, c := range commits {
		if len(c.Stages) == 0 {
			return nil, fmt.Errorf("chunk %d has no stage rows", c.Tile)
		}
		sort.Slice(c.Stages, func(i, j int) bool { return c.Stages[i].Entry < c.Stages[j].Entry })
		want := commitHash(c.Tile, c.Stages)
		if want != c.CommitHash {
			return nil, fmt.Errorf("chunk %d commit hash mismatch", c.Tile)
		}
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tile < out[j].Tile })
	return out, nil
}

// CommitTile writes all stages for one tile in one transaction and assigns a
// stable monotonically increasing submit order.
func (s *Store) CommitTile(v VersionInfo, c TileCommit) (int, error) {
	if s == nil {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, ioErr("beginCommitTile", err)
	}
	defer tx.Rollback()
	var snap VersionInfo
	if err := tx.QueryRow(`SELECT image_id,source_hash,plan_fingerprint,params_version,
		algorithm_version,geometry FROM snapshot WHERE id=1`).
		Scan(&snap.ImageID, &snap.SourceHash, &snap.PlanFingerprint, &snap.ParamsVersion,
			&snap.AlgorithmVersion, &snap.Geometry); err != nil {
		return 0, ioErr("commitSnapshotRead", err)
	}
	if reason := versionMismatch(snap, v); reason != "" {
		return 0, stateErr(0, c.Tile, "snapshot changed during run: %s", reason)
	}
	var next int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(submit_order),0)+1 FROM chunk_commits`).Scan(&next); err != nil {
		return 0, ioErr("nextSubmitOrder", err)
	}
	sort.Slice(c.Stages, func(i, j int) bool { return c.Stages[i].Entry < c.Stages[j].Entry })
	if len(c.Stages) == 0 {
		return 0, schemaErr(0, "stages", "tile commit contains no stages")
	}
	first := c.Stages[0]
	ch := commitHash(c.Tile, c.Stages)
	if _, err := tx.Exec(`INSERT INTO chunk_commits(tile,submit_order,commit_hash,x,y,w,h)
		VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(tile) DO UPDATE SET submit_order=excluded.submit_order,
		commit_hash=excluded.commit_hash,x=excluded.x,y=excluded.y,w=excluded.w,h=excluded.h`,
		c.Tile, next, ch, first.X, first.Y, first.W, first.H); err != nil {
		return 0, ioErr("writeChunkCommit", err)
	}
	if _, err := tx.Exec(`DELETE FROM stage_results WHERE tile=?`, c.Tile); err != nil {
		return 0, ioErr("clearTileStages", err)
	}
	for _, r := range c.Stages {
		if r.Tile != c.Tile {
			return 0, schemaErr(r.Entry, "tile", "stage tile %d != chunk tile %d", r.Tile, c.Tile)
		}
		dropped, _ := json.Marshal(r.Dropped)
		if _, err := tx.Exec(`INSERT INTO stage_results
			(tile,entry_seq,x,y,w,h,bit_depth,channels,alpha_index,cache_key,content_hash,
			 plan_fingerprint,params_version,algorithm_version,pixels,dropped)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			r.Tile, r.Entry, r.X, r.Y, r.W, r.H, r.BitDepth, r.Channels, r.AlphaIndex,
			r.CacheKey, r.ContentHash, v.PlanFingerprint, v.ParamsVersion, v.AlgorithmVersion,
			pixelsToBytes(r.Pixels), string(dropped)); err != nil {
			return 0, ioErr("writeStage", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, ioErr("commitTileTx", err)
	}
	return next, nil
}

// UnfreezeAndRebase explicitly removes selected committed tiles and rebases
// the remaining snapshot to a new plan identity in one transaction.
func (s *Store) UnfreezeAndRebase(v VersionInfo, tiles []int) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ioErr("beginRebase", err)
	}
	defer tx.Rollback()
	for _, tile := range tiles {
		if _, err := tx.Exec(`DELETE FROM stage_results WHERE tile=?`, tile); err != nil {
			return ioErr("rebaseDeleteStages", err)
		}
		if _, err := tx.Exec(`DELETE FROM chunk_commits WHERE tile=?`, tile); err != nil {
			return ioErr("rebaseDeleteChunk", err)
		}
	}
	if _, err := tx.Exec(`UPDATE snapshot SET image_id=?,source_hash=?,plan_fingerprint=?,
		params_version=?,algorithm_version=?,geometry=?,updated_unix=? WHERE id=1`,
		v.ImageID, v.SourceHash, v.PlanFingerprint, v.ParamsVersion, v.AlgorithmVersion,
		v.Geometry, time.Now().Unix()); err != nil {
		return ioErr("rebaseSnapshot", err)
	}
	if _, err := tx.Exec(`UPDATE stage_results SET plan_fingerprint=?,params_version=?,
		algorithm_version=?`, v.PlanFingerprint, v.ParamsVersion, v.AlgorithmVersion); err != nil {
		return ioErr("rebaseStageVersions", err)
	}
	if err := tx.Commit(); err != nil {
		return ioErr("rebaseCommit", err)
	}
	return nil
}

func commitHash(tile int, stages []StageRecord) string {
	h := sha256.New()
	fmt.Fprintf(h, "tile=%d;", tile)
	for _, r := range stages {
		fmt.Fprintf(h, "%d|%s|%s|%d|%d|%d|%d|%d|%d;",
			r.Entry, r.CacheKey, r.ContentHash, r.X, r.Y, r.W, r.H, r.BitDepth, r.Channels)
	}
	return hex.EncodeToString(h.Sum(nil))
}
