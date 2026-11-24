package store

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

	"ocrworkbench/internal/core"

	_ "modernc.org/sqlite"
)

var ErrSnapshotInvalid = errors.New("checkpoint snapshot is invalid")

type Key struct {
	ImageID          string `json:"image_id"`
	ContentHash      string `json:"content_hash"`
	TruthHash        string `json:"truth_hash"`
	TemplateVersion  string `json:"template_version"`
	ParameterVersion string `json:"parameter_version"`
	AlgorithmVersion string `json:"algorithm_version"`
}

func (k Key) Matches(other Key) bool {
	return k.ImageID == other.ImageID &&
		k.ContentHash == other.ContentHash &&
		k.TruthHash == other.TruthHash &&
		k.TemplateVersion == other.TemplateVersion &&
		k.ParameterVersion == other.ParameterVersion &&
		k.AlgorithmVersion == other.AlgorithmVersion
}

func (k Key) mismatch(other Key) string {
	switch {
	case k.ImageID != other.ImageID:
		return "image_id"
	case k.ContentHash != other.ContentHash:
		return "content_hash"
	case k.TruthHash != other.TruthHash:
		return "truth_hash"
	case k.TemplateVersion != other.TemplateVersion:
		return "template_version"
	case k.ParameterVersion != other.ParameterVersion:
		return "parameter_version"
	case k.AlgorithmVersion != other.AlgorithmVersion:
		return "algorithm_version"
	default:
		return ""
	}
}

type Snapshot struct {
	ID          int64
	Key         Key
	Complete    bool
	Tiles       []core.TileResult
	Lines       []core.LineResult
	TileByID    map[int]core.TileResult
	LineByID    map[int]core.LineResult
	Invalidated bool
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	ctx := context.Background()
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS snapshots (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  image_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  truth_hash TEXT NOT NULL,
  template_version TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  valid INTEGER NOT NULL DEFAULT 1,
  complete INTEGER NOT NULL DEFAULT 0,
  invalid_reason TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_snapshots_image ON snapshots(image_id, id DESC);
CREATE TABLE IF NOT EXISTS tile_units (
  snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  image_id TEXT NOT NULL,
  unit_index INTEGER NOT NULL,
  sequence INTEGER NOT NULL,
  state TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  template_version TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  result_json TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, unit_index)
);
CREATE TABLE IF NOT EXISTS line_units (
  snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
  image_id TEXT NOT NULL,
  unit_index INTEGER NOT NULL,
  sequence INTEGER NOT NULL,
  state TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  template_version TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  result_json TEXT NOT NULL,
  PRIMARY KEY(snapshot_id, unit_index)
);
`
	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// Prepare returns a verified matching snapshot, or creates a new empty one.
// A key or unit mismatch invalidates the entire previous image snapshot.
func (s *Store) Prepare(ctx context.Context, key Key) (int64, *Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		id        int64
		valid     bool
		complete  bool
		storedKey Key
	)
	err := s.db.QueryRowContext(ctx, `
SELECT id, content_hash, truth_hash, template_version, parameter_version,
       algorithm_version, valid, complete
FROM snapshots WHERE image_id=? ORDER BY id DESC LIMIT 1`, key.ImageID).Scan(
		&id, &storedKey.ContentHash, &storedKey.TruthHash,
		&storedKey.TemplateVersion, &storedKey.ParameterVersion,
		&storedKey.AlgorithmVersion, &valid, &complete,
	)
	storedKey.ImageID = key.ImageID
	if errors.Is(err, sql.ErrNoRows) {
		newID, err := s.createSnapshotLocked(ctx, key)
		return newID, nil, false, err
	}
	if err != nil {
		return 0, nil, false, err
	}
	if !valid {
		newID, err := s.createSnapshotLocked(ctx, key)
		return newID, nil, true, err
	}
	if !storedKey.Matches(key) {
		reason := "version mismatch on " + storedKey.mismatch(key)
		if err := s.invalidateLocked(ctx, id, reason); err != nil {
			return 0, nil, false, err
		}
		newID, err := s.createSnapshotLocked(ctx, key)
		return newID, nil, true, err
	}
	snapshot, err := s.loadSnapshotLocked(ctx, id, key)
	if err != nil {
		if errors.Is(err, ErrSnapshotInvalid) {
			if invalidateErr := s.invalidateLocked(ctx, id, err.Error()); invalidateErr != nil {
				return 0, nil, false, invalidateErr
			}
			newID, createErr := s.createSnapshotLocked(ctx, key)
			return newID, nil, true, createErr
		}
		return 0, nil, false, err
	}
	snapshot.Complete = complete
	return id, snapshot, false, nil
}

func (s *Store) createSnapshotLocked(ctx context.Context, key Key) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
INSERT INTO snapshots(image_id, content_hash, truth_hash, template_version,
                      parameter_version, algorithm_version, valid, complete, created_at)
VALUES(?,?,?,?,?,?,1,0,?)`,
		key.ImageID, key.ContentHash, key.TruthHash, key.TemplateVersion,
		key.ParameterVersion, key.AlgorithmVersion, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) invalidateLocked(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE snapshots SET valid=0, invalid_reason=? WHERE id=?`, reason, id)
	return err
}

func (s *Store) Invalidate(ctx context.Context, id int64, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invalidateLocked(ctx, id, reason)
}

func (s *Store) loadSnapshotLocked(ctx context.Context, id int64, key Key) (*Snapshot, error) {
	snapshot := &Snapshot{
		ID:       id,
		Key:      key,
		TileByID: map[int]core.TileResult{},
		LineByID: map[int]core.LineResult{},
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT image_id, unit_index, sequence, state, content_hash, template_version,
       parameter_version, algorithm_version, result_json
FROM tile_units WHERE snapshot_id=? ORDER BY sequence, unit_index`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var (
			imageID, state, hash, template, parameter, algorithm, raw string
			index, sequence                                           int
		)
		if err := rows.Scan(&imageID, &index, &sequence, &state, &hash, &template, &parameter, &algorithm, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err := validateRecord(key, imageID, state, template, parameter, algorithm); err != nil {
			rows.Close()
			return nil, err
		}
		var tile core.TileResult
		if err := json.Unmarshal([]byte(raw), &tile); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: decode tile %d: %v", ErrSnapshotInvalid, index, err)
		}
		if tile.CommittedSeq != sequence || tile.Index != index {
			rows.Close()
			return nil, fmt.Errorf("%w: tile %d sequence/index mismatch", ErrSnapshotInvalid, index)
		}
		if TileHash(key, tile) != hash || tile.ContentHash != hash {
			rows.Close()
			return nil, fmt.Errorf("%w: tile %d content hash mismatch", ErrSnapshotInvalid, index)
		}
		snapshot.Tiles = append(snapshot.Tiles, tile)
		snapshot.TileByID[index] = tile
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `
SELECT image_id, unit_index, sequence, state, content_hash, template_version,
       parameter_version, algorithm_version, result_json
FROM line_units WHERE snapshot_id=? ORDER BY sequence, unit_index`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			imageID, state, hash, template, parameter, algorithm, raw string
			index, sequence                                           int
		)
		if err := rows.Scan(&imageID, &index, &sequence, &state, &hash, &template, &parameter, &algorithm, &raw); err != nil {
			return nil, err
		}
		if err := validateRecord(key, imageID, state, template, parameter, algorithm); err != nil {
			return nil, err
		}
		var line core.LineResult
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			return nil, fmt.Errorf("%w: decode line %d: %v", ErrSnapshotInvalid, index, err)
		}
		if line.CommittedSeq != sequence || line.Number != index {
			return nil, fmt.Errorf("%w: line %d sequence/number mismatch", ErrSnapshotInvalid, index)
		}
		if LineHash(key, line) != hash || line.ContentHash != hash {
			return nil, fmt.Errorf("%w: line %d content hash mismatch", ErrSnapshotInvalid, index)
		}
		snapshot.Lines = append(snapshot.Lines, line)
		snapshot.LineByID[index] = line
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func validateRecord(key Key, imageID, state, template, parameter, algorithm string) error {
	switch {
	case imageID != key.ImageID:
		return fmt.Errorf("%w: unit image id mismatch", ErrSnapshotInvalid)
	case state != "committed":
		return fmt.Errorf("%w: unit is not committed", ErrSnapshotInvalid)
	case template != key.TemplateVersion:
		return fmt.Errorf("%w: template version mismatch", ErrSnapshotInvalid)
	case parameter != key.ParameterVersion:
		return fmt.Errorf("%w: parameter version mismatch", ErrSnapshotInvalid)
	case algorithm != key.AlgorithmVersion:
		return fmt.Errorf("%w: algorithm version mismatch", ErrSnapshotInvalid)
	}
	return nil
}

func (s *Store) SaveTile(ctx context.Context, snapshotID int64, key Key, tile core.TileResult) error {
	hash := TileHash(key, tile)
	tile.ContentHash = hash
	raw, err := json.Marshal(tile)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO tile_units(snapshot_id, image_id, unit_index, sequence, state,
                       content_hash, template_version, parameter_version,
                       algorithm_version, result_json)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		snapshotID, key.ImageID, tile.Index, tile.CommittedSeq, "committed", hash,
		key.TemplateVersion, key.ParameterVersion, key.AlgorithmVersion, string(raw))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) SaveLine(ctx context.Context, snapshotID int64, key Key, line core.LineResult) error {
	hash := LineHash(key, line)
	line.ContentHash = hash
	raw, err := json.Marshal(line)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO line_units(snapshot_id, image_id, unit_index, sequence, state,
                       content_hash, template_version, parameter_version,
                       algorithm_version, result_json)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		snapshotID, key.ImageID, line.Number, line.CommittedSeq, "committed", hash,
		key.TemplateVersion, key.ParameterVersion, key.AlgorithmVersion, string(raw))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkComplete(ctx context.Context, snapshotID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := s.db.ExecContext(ctx, `UPDATE snapshots SET complete=1 WHERE id=? AND valid=1`, snapshotID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrSnapshotInvalid
	}
	return nil
}

func (s *Store) LatestSnapshotID(ctx context.Context, imageID string) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	var valid bool
	err := s.db.QueryRowContext(ctx, `SELECT id, valid FROM snapshots WHERE image_id=? ORDER BY id DESC LIMIT 1`, imageID).Scan(&id, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, valid, err
}

func (s *Store) LoadSnapshot(ctx context.Context, snapshotID int64) (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		key      Key
		valid    bool
		complete bool
	)
	err := s.db.QueryRowContext(ctx, `
SELECT image_id, content_hash, truth_hash, template_version, parameter_version,
       algorithm_version, valid, complete FROM snapshots WHERE id=?`, snapshotID).Scan(
		&key.ImageID, &key.ContentHash, &key.TruthHash, &key.TemplateVersion,
		&key.ParameterVersion, &key.AlgorithmVersion, &valid, &complete,
	)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrSnapshotInvalid
	}
	snapshot, err := s.loadSnapshotLocked(ctx, snapshotID, key)
	if err != nil {
		return nil, err
	}
	snapshot.Complete = complete
	return snapshot, nil
}

func TileHash(key Key, tile core.TileResult) string {
	copyTile := tile
	copyTile.ContentHash = ""
	hash, err := core.HashAny(struct {
		Key  Key             `json:"key"`
		Tile core.TileResult `json:"tile"`
	}{key, copyTile})
	if err != nil {
		return ""
	}
	return hash
}

func LineHash(key Key, line core.LineResult) string {
	copyLine := line
	copyLine.ContentHash = ""
	hash, err := core.HashAny(struct {
		Key  Key             `json:"key"`
		Line core.LineResult `json:"line"`
	}{key, copyLine})
	if err != nil {
		return ""
	}
	return hash
}
