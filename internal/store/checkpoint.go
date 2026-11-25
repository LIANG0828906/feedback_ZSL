package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"

	"ms-workbench/internal/apperr"
	"ms-workbench/internal/pipeline"

	_ "modernc.org/sqlite"
)

type Key struct {
	SceneID          string
	SceneFingerprint string
	ParamVersion     string
	BandVersion      string
	AlgorithmVersion string
	Width            int
	Height           int
	BandCount        int
	ChunkW           int
	ChunkH           int
}

func (k Key) Validate() error {
	if k.SceneID == "" || k.SceneFingerprint == "" || k.ParamVersion == "" ||
		k.BandVersion == "" || k.AlgorithmVersion == "" {
		return apperr.New(apperr.EPARAM, "snapshot key is missing a required version")
	}
	if k.Width <= 0 || k.Height <= 0 || k.BandCount <= 0 || k.ChunkW <= 0 || k.ChunkH <= 0 {
		return apperr.New(apperr.EPARAM, "snapshot key has invalid geometry")
	}
	return nil
}

func (k Key) TotalSegments() int {
	cols := (k.Width + k.ChunkW - 1) / k.ChunkW
	rows := (k.Height + k.ChunkH - 1) / k.ChunkH
	return cols * rows
}

type SegmentRecord struct {
	SegmentID        int
	Ordinal          int
	Fused            []float64
	LinearRGB        []float64
	OutOfGamut       []int
	Boundary         []pipeline.BoundaryPoint
	BoundaryJSON     []byte
	PeakRadiance     float64
	ContentHash      string
	BandVersion      string
	ParamVersion     string
	AlgorithmVersion string
}

type SnapshotInfo struct {
	Key            Key
	Committed      int
	Total          int
	Complete       bool
	CommitSequence []int
}

type Checkpoint struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Checkpoint, error) {
	if path == "" {
		return nil, apperr.New(apperr.EPARAM, "checkpoint path is empty")
	}
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, apperr.New(apperr.EIO, "create checkpoint directory: %v", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, apperr.New(apperr.EIO, "open sqlite: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, apperr.New(apperr.EIO, "sqlite pragma: %v", err)
		}
	}
	c := &Checkpoint{db: db, path: path}
	if err := c.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Checkpoint) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

func (c *Checkpoint) Path() string { return c.path }

func (c *Checkpoint) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS snapshot (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  scene_id TEXT NOT NULL,
  scene_fingerprint TEXT NOT NULL,
  param_version TEXT NOT NULL,
  band_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  band_count INTEGER NOT NULL,
  chunk_w INTEGER NOT NULL,
  chunk_h INTEGER NOT NULL,
  complete INTEGER NOT NULL DEFAULT 0,
  updated_unix INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS segments (
  segment_id INTEGER PRIMARY KEY,
  ordinal INTEGER NOT NULL UNIQUE,
  state TEXT NOT NULL,
  fused BLOB NOT NULL,
  fused_count INTEGER NOT NULL,
  rgb BLOB NOT NULL,
  rgb_count INTEGER NOT NULL,
  oog BLOB NOT NULL,
  boundary BLOB NOT NULL,
  peak REAL NOT NULL,
  content_hash TEXT NOT NULL,
  band_version TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  committed_unix INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS segment_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  segment_id INTEGER NOT NULL,
  event TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  committed_unix INTEGER NOT NULL
);
`
	if _, err := c.db.Exec(schema); err != nil {
		return apperr.New(apperr.EIO, "sqlite migration: %v", err)
	}
	return nil
}

func (c *Checkpoint) Ensure(key Key) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureLocked(key)
}

func (c *Checkpoint) ensureLocked(key Key) error {
	if err := key.Validate(); err != nil {
		return err
	}
	var existing Key
	var complete int
	err := c.db.QueryRow(`SELECT scene_id, scene_fingerprint, param_version, band_version,
		algorithm_version, width, height, band_count, chunk_w, chunk_h, complete
		FROM snapshot WHERE singleton=1`).Scan(
		&existing.SceneID, &existing.SceneFingerprint, &existing.ParamVersion,
		&existing.BandVersion, &existing.AlgorithmVersion, &existing.Width,
		&existing.Height, &existing.BandCount, &existing.ChunkW, &existing.ChunkH, &complete)
	if err == sql.ErrNoRows {
		_, err = c.db.Exec(`INSERT INTO snapshot(singleton, scene_id, scene_fingerprint,
			param_version, band_version, algorithm_version, width, height, band_count,
			chunk_w, chunk_h, complete, updated_unix)
			VALUES(1,?,?,?,?,?,?,?,?,?,?,0,strftime('%s','now'))`,
			key.SceneID, key.SceneFingerprint, key.ParamVersion, key.BandVersion,
			key.AlgorithmVersion, key.Width, key.Height, key.BandCount, key.ChunkW, key.ChunkH)
		if err != nil {
			return apperr.New(apperr.EIO, "create snapshot: %v", err)
		}
		return nil
	}
	if err != nil {
		return apperr.New(apperr.EIO, "read snapshot: %v", err)
	}
	if existing != key {
		return apperr.New(apperr.EVERSION, "snapshot key mismatch")
	}
	return nil
}

// Load validates the entire snapshot. If any row, hash, or version is
// inconsistent the full snapshot is deleted before the error is returned.
func (c *Checkpoint) Load(key Key) ([]SegmentRecord, *SnapshotInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := key.Validate(); err != nil {
		return nil, nil, err
	}
	var existing Key
	var complete int
	err := c.db.QueryRow(`SELECT scene_id, scene_fingerprint, param_version, band_version,
		algorithm_version, width, height, band_count, chunk_w, chunk_h, complete
		FROM snapshot WHERE singleton=1`).Scan(
		&existing.SceneID, &existing.SceneFingerprint, &existing.ParamVersion,
		&existing.BandVersion, &existing.AlgorithmVersion, &existing.Width,
		&existing.Height, &existing.BandCount, &existing.ChunkW, &existing.ChunkH, &complete)
	if err == sql.ErrNoRows {
		return nil, &SnapshotInfo{Key: key, Total: key.TotalSegments()}, nil
	}
	if err != nil {
		return nil, nil, apperr.New(apperr.EIO, "read snapshot: %v", err)
	}
	if existing != key {
		_ = c.resetLocked()
		return nil, nil, apperr.New(apperr.EVERSION, "snapshot version/scene mismatch; entire snapshot discarded")
	}
	rows, err := c.db.Query(`SELECT segment_id, ordinal, state, fused, fused_count, rgb, rgb_count,
		oog, boundary, peak, content_hash, band_version, param_version, algorithm_version
		FROM segments ORDER BY segment_id ASC`)
	if err != nil {
		return nil, nil, apperr.New(apperr.EIO, "read segments: %v", err)
	}
	defer rows.Close()
	fail := func(reason error) ([]SegmentRecord, *SnapshotInfo, error) {
		_ = rows.Close()
		_ = c.resetLocked()
		return nil, nil, reason
	}
	var records []SegmentRecord
	lastID := -1
	for rows.Next() {
		var rec SegmentRecord
		var state string
		var fusedBlob, rgbBlob, oogBlob, boundary []byte
		var fusedCount, rgbCount int
		if err := rows.Scan(&rec.SegmentID, &rec.Ordinal, &state, &fusedBlob, &fusedCount,
			&rgbBlob, &rgbCount, &oogBlob, &boundary, &rec.PeakRadiance, &rec.ContentHash,
			&rec.BandVersion, &rec.ParamVersion, &rec.AlgorithmVersion); err != nil {
			return fail(apperr.New(apperr.EHASH, "read segment row: %v", err))
		}
		if state != "committed" || rec.Ordinal != rec.SegmentID || rec.SegmentID <= lastID ||
			rec.SegmentID < 0 || rec.SegmentID >= key.TotalSegments() {
			return fail(apperr.New(apperr.EHASH, "segment state/order is inconsistent; entire snapshot discarded"))
		}
		if rec.BandVersion != key.BandVersion || rec.ParamVersion != key.ParamVersion ||
			rec.AlgorithmVersion != key.AlgorithmVersion {
			return fail(apperr.New(apperr.EVERSION, "segment version mismatch; entire snapshot discarded"))
		}
		if len(fusedBlob) != fusedCount*8 || len(rgbBlob) != rgbCount*8 {
			return fail(apperr.New(apperr.EHASH, "segment buffer length mismatch; entire snapshot discarded"))
		}
		rec.Fused, _ = decodeFloats(fusedBlob)
		rec.LinearRGB, _ = decodeFloats(rgbBlob)
		rec.OutOfGamut, _ = decodeInts(oogBlob)
		rec.BoundaryJSON = append([]byte(nil), boundary...)
		if len(boundary) > 0 {
			if err := json.Unmarshal(boundary, &rec.Boundary); err != nil {
				return fail(apperr.New(apperr.EHASH, "gamut boundary payload corrupt; entire snapshot discarded"))
			}
		}
		hash := ContentHash(rec.SegmentID, rec.Fused, rec.LinearRGB, rec.OutOfGamut, rec.BoundaryJSON, rec.PeakRadiance)
		if hash != rec.ContentHash {
			return fail(apperr.New(apperr.EHASH, "segment %d content hash mismatch; entire snapshot discarded", rec.SegmentID))
		}
		records = append(records, rec)
		lastID = rec.SegmentID
	}
	if err := rows.Err(); err != nil {
		return fail(apperr.New(apperr.EHASH, "read segments: %v", err))
	}
	if complete == 1 {
		if len(records) != key.TotalSegments() || len(records) == 0 ||
			records[0].SegmentID != 0 || records[len(records)-1].SegmentID != key.TotalSegments()-1 {
			return fail(apperr.New(apperr.EHASH, "complete snapshot has partial segments; entire snapshot discarded"))
		}
	}
	if len(records) > key.TotalSegments() {
		return fail(apperr.New(apperr.EHASH, "snapshot contains too many segments; entire snapshot discarded"))
	}
	info := &SnapshotInfo{
		Key:            key,
		Committed:      len(records),
		Total:          key.TotalSegments(),
		Complete:       complete == 1,
		CommitSequence: make([]int, len(records)),
	}
	for i, rec := range records {
		info.CommitSequence[i] = rec.SegmentID
	}
	return records, info, nil
}

func (c *Checkpoint) CommitSegment(key Key, rec SegmentRecord) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := key.Validate(); err != nil {
		return 0, err
	}
	if err := c.ensureLocked(key); err != nil {
		return 0, err
	}
	bufFused := encodeFloats(rec.Fused)
	bufRGB := encodeFloats(rec.LinearRGB)
	bufOOG := encodeInts(rec.OutOfGamut)
	if rec.BoundaryJSON == nil {
		rec.BoundaryJSON, _ = json.Marshal(rec.Boundary)
	}
	rec.ContentHash = ContentHash(rec.SegmentID, rec.Fused, rec.LinearRGB, rec.OutOfGamut, rec.BoundaryJSON, rec.PeakRadiance)
	tx, err := c.db.Begin()
	if err != nil {
		return 0, apperr.New(apperr.EIO, "begin segment transaction: %v", err)
	}
	defer tx.Rollback()
	ordinal := rec.SegmentID
	_, err = tx.Exec(`INSERT INTO segments(segment_id, ordinal, state, fused, fused_count,
		rgb, rgb_count, oog, boundary, peak, content_hash, band_version, param_version,
		algorithm_version, committed_unix)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,strftime('%s','now'))
		ON CONFLICT(segment_id) DO UPDATE SET
		  ordinal=excluded.ordinal, state=excluded.state, fused=excluded.fused,
		  fused_count=excluded.fused_count, rgb=excluded.rgb, rgb_count=excluded.rgb_count,
		  oog=excluded.oog, boundary=excluded.boundary, peak=excluded.peak,
		  content_hash=excluded.content_hash, band_version=excluded.band_version,
		  param_version=excluded.param_version, algorithm_version=excluded.algorithm_version,
		  committed_unix=excluded.committed_unix`,
		rec.SegmentID, ordinal, "committed", bufFused, len(rec.Fused), bufRGB, len(rec.LinearRGB),
		bufOOG, rec.BoundaryJSON, rec.PeakRadiance, rec.ContentHash, key.BandVersion,
		key.ParamVersion, key.AlgorithmVersion)
	if err != nil {
		return 0, apperr.New(apperr.EIO, "insert segment: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO segment_events(segment_id, event, content_hash, committed_unix)
		VALUES(?,?,?,strftime('%s','now'))`, rec.SegmentID, "committed", rec.ContentHash); err != nil {
		return 0, apperr.New(apperr.EIO, "insert commit event: %v", err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM segments`).Scan(&count); err != nil {
		return 0, apperr.New(apperr.EIO, "count segments: %v", err)
	}
	complete := 0
	if count == key.TotalSegments() {
		complete = 1
	}
	if _, err := tx.Exec(`UPDATE snapshot SET complete=?, updated_unix=strftime('%s','now') WHERE singleton=1`, complete); err != nil {
		return 0, apperr.New(apperr.EIO, "update snapshot completeness: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, apperr.New(apperr.EIO, "commit segment transaction: %v", err)
	}
	rec.Ordinal = ordinal
	return ordinal, nil
}

// ApplyEdit atomically removes invalidated segments and re-stamps the remaining
// valid segment rows with the new global versions.
func (c *Checkpoint) ApplyEdit(key Key, deleteIDs []int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := key.Validate(); err != nil {
		return err
	}
	var exists int
	err := c.db.QueryRow(`SELECT COUNT(*) FROM snapshot WHERE singleton=1`).Scan(&exists)
	if err != nil {
		return apperr.New(apperr.EIO, "read snapshot: %v", err)
	}
	if exists == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return apperr.New(apperr.EIO, "begin edit transaction: %v", err)
	}
	defer tx.Rollback()
	for _, id := range deleteIDs {
		if _, err := tx.Exec(`DELETE FROM segments WHERE segment_id=?`, id); err != nil {
			return apperr.New(apperr.EIO, "delete invalid segment: %v", err)
		}
	}
	if _, err := tx.Exec(`UPDATE segments SET band_version=?, param_version=?, algorithm_version=?`,
		key.BandVersion, key.ParamVersion, key.AlgorithmVersion); err != nil {
		return apperr.New(apperr.EIO, "restamp segment versions: %v", err)
	}
	if _, err := tx.Exec(`UPDATE snapshot SET scene_id=?, scene_fingerprint=?, param_version=?,
		band_version=?, algorithm_version=?, width=?, height=?, band_count=?, chunk_w=?, chunk_h=?,
		complete=0, updated_unix=strftime('%s','now') WHERE singleton=1`,
		key.SceneID, key.SceneFingerprint, key.ParamVersion, key.BandVersion, key.AlgorithmVersion,
		key.Width, key.Height, key.BandCount, key.ChunkW, key.ChunkH); err != nil {
		return apperr.New(apperr.EIO, "update snapshot key: %v", err)
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM segments`).Scan(&count); err != nil {
		return apperr.New(apperr.EIO, "count segments: %v", err)
	}
	if count == key.TotalSegments() {
		if _, err := tx.Exec(`UPDATE snapshot SET complete=1 WHERE singleton=1`); err != nil {
			return apperr.New(apperr.EIO, "mark snapshot complete: %v", err)
		}
	}
	return tx.Commit()
}

func (c *Checkpoint) ResetAll() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resetLocked()
}

func (c *Checkpoint) resetLocked() error {
	tx, err := c.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"segments", "segment_events", "snapshot"} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ContentHash(segmentID int, fused, rgb []float64, oog []int, boundary []byte, peak float64) string {
	h := sha256.New()
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(segmentID))
	h.Write(n[:])
	binary.LittleEndian.PutUint64(n[:], uint64(len(fused)))
	h.Write(n[:])
	h.Write(encodeFloats(fused))
	binary.LittleEndian.PutUint64(n[:], uint64(len(rgb)))
	h.Write(n[:])
	h.Write(encodeFloats(rgb))
	h.Write(encodeInts(oog))
	h.Write(boundary)
	binary.LittleEndian.PutUint64(n[:], math.Float64bits(peak))
	h.Write(n[:])
	return hex.EncodeToString(h.Sum(nil))
}

func encodeFloats(v []float64) []byte {
	out := make([]byte, len(v)*8)
	for i, x := range v {
		binary.LittleEndian.PutUint64(out[i*8:], math.Float64bits(x))
	}
	return out
}

func decodeFloats(data []byte) ([]float64, error) {
	if len(data)%8 != 0 {
		return nil, fmt.Errorf("invalid float buffer length")
	}
	out := make([]float64, len(data)/8)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[i*8:]))
	}
	return out, nil
}

func encodeInts(v []int) []byte {
	out := make([]byte, len(v)*4)
	for i, x := range v {
		binary.LittleEndian.PutUint32(out[i*4:], uint32(x))
	}
	return out
}

func decodeInts(data []byte) ([]int, error) {
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("invalid integer buffer length")
	}
	out := make([]int, len(data)/4)
	for i := range out {
		out[i] = int(int32(binary.LittleEndian.Uint32(data[i*4:])))
	}
	return out, nil
}

func (c *Checkpoint) SegmentCount() (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var count int
	if err := c.db.QueryRow(`SELECT COUNT(*) FROM segments`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

func (c *Checkpoint) EventSequence() ([]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows, err := c.db.Query(`SELECT segment_id FROM segment_events ORDER BY event_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
