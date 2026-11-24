// Package checkpoint implements per-zone atomic SQLite checkpoints. Writes are
// serialized through one connection and committed one zone per transaction.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"collectionrecon/internal/errors"
	"collectionrecon/internal/model"

	_ "modernc.org/sqlite"
)

// Store is a SQLite-backed zone checkpoint store.
type Store struct {
	db   *sql.DB
	mu   chan struct{}
	algo string
}

// Open opens (or creates) the checkpoint database and ensures the schema.
func Open(path, algoVersion string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS zone_checkpoint (
			zone_id        INTEGER PRIMARY KEY,
			cell_indices   TEXT NOT NULL,
			estimate       TEXT NOT NULL,
			point_digest   TEXT NOT NULL,
			heat_digest    TEXT NOT NULL,
			content_hash   TEXT NOT NULL,
			param_version  INTEGER NOT NULL,
			algo_version   TEXT NOT NULL,
			payload        TEXT NOT NULL,
			seq            INTEGER NOT NULL
		)`); err != nil {
		return nil, err
	}
	return &Store{db: db, mu: make(chan struct{}, 1), algo: algoVersion}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// Save atomically writes one zone checkpoint. A mutex makes concurrent worker
// submissions strictly serial, and seq records the stable commit order.
func (s *Store) Save(seq int, result model.ZoneResult) error {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()

	cells, err := json.Marshal(result.CellIndices)
	if err != nil {
		return err
	}
	estimate, err := json.Marshal(result.Estimate)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO zone_checkpoint
		(zone_id, cell_indices, estimate, point_digest, heat_digest,
		 content_hash, param_version, algo_version, payload, seq)
		VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(zone_id) DO UPDATE SET
		 cell_indices=excluded.cell_indices, estimate=excluded.estimate,
		 point_digest=excluded.point_digest, heat_digest=excluded.heat_digest,
		 content_hash=excluded.content_hash, param_version=excluded.param_version,
		 algo_version=excluded.algo_version, payload=excluded.payload, seq=excluded.seq`,
		result.ZoneID, string(cells), string(estimate), result.PointDigest,
		result.HeatDigest, result.ContentHash, result.ParamVersion,
		result.AlgoVersion, string(payload), seq)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// LoadAll returns checkpoints ordered by the stable commit sequence.
func (s *Store) LoadAll() ([]model.ZoneResult, error) {
	rows, err := s.db.Query(`SELECT payload FROM zone_checkpoint ORDER BY seq ASC, zone_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]model.ZoneResult, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var result model.ZoneResult
		if err := json.Unmarshal([]byte(payload), &result); err != nil {
			return nil, err
		}
		out = append(out, result)
	}
	return out, rows.Err()
}

// Verify checks every loaded row against the current content hash, parameter
// version and algorithm version. On the first mismatch it returns E_VER and
// discards the whole snapshot so stale zones can never be mixed with new ones.
func (s *Store) Verify(expected map[int]ExpectedVersion) ([]model.ZoneResult, error) {
	loaded, err := s.LoadAll()
	if err != nil {
		return nil, err
	}
	for _, result := range loaded {
		want, ok := expected[result.ZoneID]
		if !ok {
			return nil, errors.WithContext(errors.EVer,
				map[string]any{"zoneId": result.ZoneID},
				"checkpoint zone %d no longer exists", result.ZoneID)
		}
		if result.AlgoVersion != s.algo {
			return nil, versionError(result.ZoneID, "algo", result.AlgoVersion, s.algo)
		}
		if result.ContentHash != want.ContentHash {
			return nil, versionError(result.ZoneID, "content", result.ContentHash, want.ContentHash)
		}
		if result.ParamVersion != want.ParamVersion {
			return nil, versionError(result.ZoneID, "param",
				fmt.Sprint(result.ParamVersion), fmt.Sprint(want.ParamVersion))
		}
	}
	return loaded, nil
}

// Reset drops every checkpoint row, used after a failed version verification.
func (s *Store) Reset() error {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	_, err := s.db.Exec(`DELETE FROM zone_checkpoint`)
	return err
}

// ExpectedVersion bundles the current identity of one zone.
type ExpectedVersion struct {
	ContentHash  string
	ParamVersion int64
}

func versionError(zoneID int, kind, got, want string) error {
	return errors.WithContext(errors.EVer,
		map[string]any{"zoneId": zoneID, "kind": kind, "got": got, "want": want},
		"first mismatching checkpoint at zone %d: %s version got=%s want=%s",
		zoneID, kind, got, want)
}
