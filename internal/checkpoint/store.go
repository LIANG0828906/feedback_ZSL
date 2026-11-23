package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"mocapworkbench/internal/model"
	"mocapworkbench/internal/retarget"
)

type SnapshotMeta struct {
	ClipID           string `json:"clip_id"`
	TargetID         string `json:"target_id"`
	ParameterVersion string `json:"parameter_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	PlanJSON         string `json:"plan_json"`
}

type PrepareResult struct {
	Frames []model.FrameResult
	Action string
	Reason string
}

type Store struct {
	mu   sync.Mutex
	db   *sql.DB
	meta SnapshotMeta
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, model.NewError(model.ErrParam, "checkpoint", "db", "database path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
PRAGMA busy_timeout=5000;
PRAGMA foreign_keys=ON;
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}
	store := &Store{db: db}
	if err := store.createSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *Store) createSchema() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshot_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  clip_id TEXT NOT NULL,
  target_id TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  plan_json TEXT NOT NULL,
  committed_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS frames (
  frame INTEGER PRIMARY KEY,
  status TEXT NOT NULL,
  rotations_json TEXT NOT NULL,
  root_json TEXT NOT NULL,
  contacts_json TEXT NOT NULL,
  stats_json TEXT NOT NULL,
  decisions_json TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  parameter_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL
);
`)
	return err
}

// Prepare validates the whole snapshot before returning any frame. A mismatch
// in parameter version, algorithm version, per-frame hash or sequence clears
// every row in one transaction so old and new frames can never be mixed.
func (s *Store) Prepare(meta SnapshotMeta) (PrepareResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return PrepareResult{}, model.NewError(model.ErrState, "snapshot", "db", "store is closed")
	}
	stored, found, err := s.readMetaLocked()
	if err != nil {
		return PrepareResult{}, err
	}
	if !found {
		if err := s.resetAndWriteMetaLocked(meta); err != nil {
			return PrepareResult{}, err
		}
		return PrepareResult{Action: "fresh", Reason: "no snapshot found"}, nil
	}
	reason := ""
	switch {
	case stored.ClipID != meta.ClipID:
		reason = fmt.Sprintf("clip_id changed: %s -> %s", stored.ClipID, meta.ClipID)
	case stored.TargetID != meta.TargetID:
		reason = fmt.Sprintf("target_id changed: %s -> %s", stored.TargetID, meta.TargetID)
	case stored.ParameterVersion != meta.ParameterVersion:
		reason = "parameter_version mismatch"
	case stored.AlgorithmVersion != meta.AlgorithmVersion:
		reason = fmt.Sprintf("algorithm_version mismatch: %s -> %s", stored.AlgorithmVersion, meta.AlgorithmVersion)
	case stored.PlanJSON != meta.PlanJSON:
		reason = "plan differs from the snapshot's recorded plan"
	}
	if reason != "" {
		if err := s.resetAndWriteMetaLocked(meta); err != nil {
			return PrepareResult{}, err
		}
		return PrepareResult{Action: "recomputed", Reason: reason}, nil
	}
	frames, err := s.loadFramesLocked(meta)
	if err != nil {
		if resetErr := s.resetAndWriteMetaLocked(meta); resetErr != nil {
			return PrepareResult{}, resetErr
		}
		return PrepareResult{Action: "recomputed", Reason: err.Error()}, nil
	}
	return PrepareResult{Frames: frames, Action: "resumed", Reason: "validated snapshot prefix"}, nil
}

func (s *Store) Append(frame model.FrameResult, meta SnapshotMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return model.NewError(model.ErrState, "snapshot", "db", "store is closed")
	}
	stored, found, err := s.readMetaLocked()
	if err != nil {
		return err
	}
	if !found || stored != meta {
		return model.NewError(model.ErrState, "snapshot", "meta", "snapshot metadata changed before append")
	}
	contentHash := retarget.FrameContentHash(frame, meta.ParameterVersion)
	rotationsJSON, err := json.Marshal(frame.Rotations)
	if err != nil {
		return err
	}
	rootJSON, err := json.Marshal(frame.RootTranslation)
	if err != nil {
		return err
	}
	contactsJSON, err := json.Marshal(frame.Contacts)
	if err != nil {
		return err
	}
	statsJSON, err := json.Marshal(frame.Stats)
	if err != nil {
		return err
	}
	decisionsJSON, err := json.Marshal(frame.Decisions)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT committed_count FROM snapshot_meta WHERE id=1`).Scan(&count); err != nil {
		return err
	}
	if frame.Frame != count {
		return model.FrameError(model.ErrState, "frame", frame.Frame, "order", "concurrent or out-of-order commit rejected")
	}
	_, err = tx.Exec(`
INSERT INTO frames(frame,status,rotations_json,root_json,contacts_json,stats_json,decisions_json,content_hash,parameter_version,algorithm_version)
VALUES(?,?,?,?,?,?,?,?,?,?)`,
		frame.Frame, frame.State, string(rotationsJSON), string(rootJSON), string(contactsJSON),
		string(statsJSON), string(decisionsJSON), contentHash, meta.ParameterVersion, meta.AlgorithmVersion)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE snapshot_meta SET committed_count=? WHERE id=1`, count+1); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Reset(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM frames`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshot_meta`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommittedCount() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return 0, model.NewError(model.ErrState, "snapshot", "db", "store is closed")
	}
	var count int
	err := s.db.QueryRow(`SELECT committed_count FROM snapshot_meta WHERE id=1`).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return count, err
}

func (s *Store) readMetaLocked() (SnapshotMeta, bool, error) {
	var meta SnapshotMeta
	var count int
	err := s.db.QueryRow(`SELECT clip_id,target_id,parameter_version,algorithm_version,plan_json,committed_count FROM snapshot_meta WHERE id=1`).
		Scan(&meta.ClipID, &meta.TargetID, &meta.ParameterVersion, &meta.AlgorithmVersion, &meta.PlanJSON, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return meta, false, nil
	}
	return meta, err == nil, err
}

func (s *Store) resetAndWriteMetaLocked(meta SnapshotMeta) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM frames`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM snapshot_meta`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO snapshot_meta(id,clip_id,target_id,parameter_version,algorithm_version,plan_json,committed_count) VALUES(1,?,?,?,?,?,0)`,
		meta.ClipID, meta.TargetID, meta.ParameterVersion, meta.AlgorithmVersion, meta.PlanJSON); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func (s *Store) loadFramesLocked(meta SnapshotMeta) ([]model.FrameResult, error) {
	rows, err := s.db.Query(`SELECT frame,status,rotations_json,root_json,contacts_json,stats_json,decisions_json,content_hash,parameter_version,algorithm_version FROM frames ORDER BY frame ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var frames []model.FrameResult
	expected := 0
	for rows.Next() {
		var frame model.FrameResult
		var rotationsJSON, rootJSON, contactsJSON, statsJSON, decisionsJSON, hash, param, algorithm string
		if err := rows.Scan(&frame.Frame, &frame.State, &rotationsJSON, &rootJSON, &contactsJSON, &statsJSON, &decisionsJSON, &hash, &param, &algorithm); err != nil {
			return nil, err
		}
		if frame.Frame != expected {
			return nil, fmt.Errorf("non-contiguous frame sequence: got %d want %d", frame.Frame, expected)
		}
		if param != meta.ParameterVersion || algorithm != meta.AlgorithmVersion {
			return nil, fmt.Errorf("frame %d version mismatch", frame.Frame)
		}
		if err := json.Unmarshal([]byte(rotationsJSON), &frame.Rotations); err != nil {
			return nil, fmt.Errorf("frame %d rotations: %w", frame.Frame, err)
		}
		if err := json.Unmarshal([]byte(rootJSON), &frame.RootTranslation); err != nil {
			return nil, fmt.Errorf("frame %d root: %w", frame.Frame, err)
		}
		if err := json.Unmarshal([]byte(contactsJSON), &frame.Contacts); err != nil {
			return nil, fmt.Errorf("frame %d contacts: %w", frame.Frame, err)
		}
		if err := json.Unmarshal([]byte(statsJSON), &frame.Stats); err != nil {
			return nil, fmt.Errorf("frame %d stats: %w", frame.Frame, err)
		}
		if err := json.Unmarshal([]byte(decisionsJSON), &frame.Decisions); err != nil {
			return nil, fmt.Errorf("frame %d decisions: %w", frame.Frame, err)
		}
		if got := retarget.FrameContentHash(frame, meta.ParameterVersion); got != hash {
			return nil, fmt.Errorf("frame %d content hash mismatch", frame.Frame)
		}
		frames = append(frames, frame)
		expected++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var committed int
	if err := s.db.QueryRow(`SELECT committed_count FROM snapshot_meta WHERE id=1`).Scan(&committed); err != nil {
		return nil, err
	}
	if committed != len(frames) {
		return nil, fmt.Errorf("committed_count %d != frame rows %d", committed, len(frames))
	}
	return frames, nil
}

func SnapshotTableInfo(path string) (int, error) {
	store, err := Open(path)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	return store.CommittedCount()
}

func NormalizeReason(reason string) string {
	return strings.TrimSpace(reason)
}
