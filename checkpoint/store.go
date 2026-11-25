// Package checkpoint provides a pure-Go SQLite atomic unit checkpoint store.
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"terrainbake/bake"
	"terrainbake/model"
)

// Store persists each chunk/time unit in its own transaction. A single
// connection and mutex guarantee that writes never interleave.
type Store struct {
	mu       sync.Mutex
	db       *sql.DB
	path     string
	identity string
	params   model.BakeParams
}

// Open creates or opens a snapshot store. Identity or version mismatch clears
// the whole snapshot and returns E_SNAP.
func Open(ctx context.Context, path, identity string, params model.BakeParams) (*Store, error) {
	if path == "" {
		return nil, model.NewError(model.CodeValue, "checkpoint path is empty").WithField("checkpoint.path")
	}
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, model.NewError(model.CodeIO, "create checkpoint directory: %v", err).WithField("checkpoint.path")
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, model.NewError(model.CodeIO, "open sqlite checkpoint: %v", err).WithField("checkpoint.path")
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, model.NewError(model.CodeIO, "configure sqlite checkpoint: %v", err)
	}
	s := &Store{db: db, path: path, identity: identity, params: params}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.ensureSnapshot(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS snapshot (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	identity TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	algorithm_version TEXT NOT NULL,
	revision INTEGER NOT NULL DEFAULT 1,
	updated_unix INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS units (
	chunk_id TEXT NOT NULL,
	sun_time INTEGER NOT NULL,
	commit_seq INTEGER NOT NULL UNIQUE,
	cache_key TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	height_hash TEXT NOT NULL,
	sun_hash TEXT NOT NULL,
	sky_hash TEXT NOT NULL,
	param_version INTEGER NOT NULL,
	algorithm_version TEXT NOT NULL,
	shadow_digest TEXT NOT NULL,
	sky_digest TEXT NOT NULL,
	irradiance_digest TEXT NOT NULL,
	deps_json TEXT NOT NULL,
	points_json BLOB NOT NULL,
	PRIMARY KEY (chunk_id, sun_time)
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return model.NewError(model.CodeIO, "migrate sqlite checkpoint: %v", err)
	}
	return nil
}

func (s *Store) ensureSnapshot(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var identity, algorithm string
	var paramVersion int
	err := s.db.QueryRowContext(ctx,
		`SELECT identity, param_version, algorithm_version FROM snapshot WHERE id=1`).
		Scan(&identity, &paramVersion, &algorithm)
	if err == sql.ErrNoRows {
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO snapshot(id, identity, param_version, algorithm_version, revision, updated_unix)
			 VALUES(1, ?, ?, ?, 1, ?)`,
			s.identity, s.params.ParameterVersion, s.params.AlgorithmVersion, time.Now().Unix())
		if err != nil {
			return model.NewError(model.CodeIO, "initialize snapshot: %v", err)
		}
		return nil
	}
	if err != nil {
		return model.NewError(model.CodeIO, "read snapshot metadata: %v", err)
	}
	if identity == s.identity && paramVersion == s.params.ParameterVersion && algorithm == s.params.AlgorithmVersion {
		return nil
	}
	if err := s.resetAllLocked(ctx); err != nil {
		return err
	}
	return model.NewError(model.CodeSnap,
		"snapshot identity or version mismatch (identity %t, param %t, algorithm %t)",
		identity == s.identity, paramVersion == s.params.ParameterVersion, algorithm == s.params.AlgorithmVersion)
}

func (s *Store) resetAllLocked(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewError(model.CodeIO, "begin snapshot reset: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM units`); err != nil {
		return model.NewError(model.CodeIO, "clear snapshot units: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot`); err != nil {
		return model.NewError(model.CodeIO, "clear snapshot metadata: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO snapshot(id, identity, param_version, algorithm_version, revision, updated_unix)
		 VALUES(1, ?, ?, ?, 1, ?)`,
		s.identity, s.params.ParameterVersion, s.params.AlgorithmVersion, time.Now().Unix()); err != nil {
		return model.NewError(model.CodeIO, "rewrite snapshot metadata: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.CodeIO, "commit snapshot reset: %v", err)
	}
	return nil
}

// Reset clears all persisted units but retains the current snapshot identity.
func (s *Store) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewError(model.CodeIO, "begin snapshot unit reset: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM units`); err != nil {
		return model.NewError(model.CodeIO, "clear snapshot units: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot SET revision=revision+1, updated_unix=? WHERE id=1`, time.Now().Unix()); err != nil {
		return model.NewError(model.CodeIO, "update snapshot revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.CodeIO, "commit snapshot unit reset: %v", err)
	}
	return nil
}

// SaveUnit atomically stores one complete unit. It is serialized with every
// other writer and assigns a monotonically increasing commit sequence.
func (s *Store) SaveUnit(ctx context.Context, unit *bake.BakedUnit) error {
	if unit == nil {
		return model.NewError(model.CodeValue, "cannot checkpoint nil unit")
	}
	if unit.ParameterVersion != s.params.ParameterVersion || unit.AlgorithmVersion != s.params.AlgorithmVersion {
		return s.invalidateWhole(ctx, "unit version does not match snapshot")
	}
	depsJSON, err := json.Marshal(unit.Dependencies)
	if err != nil {
		return model.NewError(model.CodeValue, "encode unit dependencies: %v", err)
	}
	pointsJSON, err := json.Marshal(unit.Points)
	if err != nil {
		return model.NewError(model.CodeValue, "encode unit points: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewError(model.CodeIO, "begin unit checkpoint: %v", err)
	}
	defer tx.Rollback()
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(commit_seq),0)+1 FROM units`).Scan(&seq); err != nil {
		return model.NewError(model.CodeIO, "allocate commit sequence: %v", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO units(
	chunk_id, sun_time, commit_seq, cache_key, content_hash, height_hash, sun_hash, sky_hash,
	param_version, algorithm_version, shadow_digest, sky_digest, irradiance_digest, deps_json, points_json
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(chunk_id, sun_time) DO UPDATE SET
	commit_seq=excluded.commit_seq, cache_key=excluded.cache_key, content_hash=excluded.content_hash,
	height_hash=excluded.height_hash, sun_hash=excluded.sun_hash, sky_hash=excluded.sky_hash,
	param_version=excluded.param_version, algorithm_version=excluded.algorithm_version,
	shadow_digest=excluded.shadow_digest, sky_digest=excluded.sky_digest,
	irradiance_digest=excluded.irradiance_digest, deps_json=excluded.deps_json, points_json=excluded.points_json`,
		unit.ChunkID, unit.Time, seq, unit.CacheKey, unit.ContentHash, unit.HeightHash, unit.SunHash, unit.SkyHash,
		unit.ParameterVersion, unit.AlgorithmVersion, unit.ShadowDigest, unit.SkyDigest, unit.IrradianceDigest,
		string(depsJSON), pointsJSON)
	if err != nil {
		return model.NewError(model.CodeIO, "write unit checkpoint: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot SET revision=revision+1, updated_unix=? WHERE id=1`, time.Now().Unix()); err != nil {
		return model.NewError(model.CodeIO, "advance snapshot revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.CodeIO, "commit unit checkpoint: %v", err)
	}
	return nil
}

// DeleteUnits removes a batch atomically after checking snapshot versions.
func (s *Store) DeleteUnits(ctx context.Context, refs []bake.UnitRef) error {
	if len(refs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewError(model.CodeIO, "begin invalidation transaction: %v", err)
	}
	defer tx.Rollback()
	var identity, algorithm string
	var paramVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT identity, param_version, algorithm_version FROM snapshot WHERE id=1`).
		Scan(&identity, &paramVersion, &algorithm); err != nil {
		return model.NewError(model.CodeIO, "read snapshot metadata: %v", err)
	}
	if identity != s.identity || paramVersion != s.params.ParameterVersion || algorithm != s.params.AlgorithmVersion {
		return s.invalidateWholeLockedTx(ctx, tx, "snapshot metadata changed before delete")
	}
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM units WHERE chunk_id=? AND sun_time=?`, ref.ChunkID, ref.Time); err != nil {
			return model.NewError(model.CodeIO, "delete unit checkpoint: %v", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshot SET revision=revision+1, updated_unix=? WHERE id=1`, time.Now().Unix()); err != nil {
		return model.NewError(model.CodeIO, "advance snapshot revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.CodeIO, "commit invalidation transaction: %v", err)
	}
	return nil
}

// LoadUnits reads complete units. Corrupt or version-mismatched rows clear the
// entire snapshot and return E_SNAP.
func (s *Store) LoadUnits(ctx context.Context) ([]*bake.BakedUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `
SELECT chunk_id, sun_time, cache_key, content_hash, height_hash, sun_hash, sky_hash,
       param_version, algorithm_version, shadow_digest, sky_digest, irradiance_digest,
       deps_json, points_json
FROM units ORDER BY sun_time ASC, chunk_id ASC`)
	if err != nil {
		return nil, model.NewError(model.CodeIO, "query unit checkpoints: %v", err)
	}
	defer rows.Close()
	var units []*bake.BakedUnit
	for rows.Next() {
		var unit bake.BakedUnit
		var depsJSON, pointsJSON []byte
		if err := rows.Scan(
			&unit.ChunkID, &unit.Time, &unit.CacheKey, &unit.ContentHash, &unit.HeightHash,
			&unit.SunHash, &unit.SkyHash, &unit.ParameterVersion, &unit.AlgorithmVersion,
			&unit.ShadowDigest, &unit.SkyDigest, &unit.IrradianceDigest, &depsJSON, &pointsJSON,
		); err != nil {
			_ = rows.Close()
			return nil, s.invalidateAndSnap(ctx, "scan unit checkpoint: %v", err)
		}
		if unit.ParameterVersion != s.params.ParameterVersion || unit.AlgorithmVersion != s.params.AlgorithmVersion {
			_ = rows.Close()
			return nil, s.invalidateAndSnap(ctx, "stored unit version mismatch for %s@%d", unit.ChunkID, unit.Time)
		}
		if err := json.Unmarshal(depsJSON, &unit.Dependencies); err != nil {
			_ = rows.Close()
			return nil, s.invalidateAndSnap(ctx, "decode dependencies for %s@%d: %v", unit.ChunkID, unit.Time, err)
		}
		if err := json.Unmarshal(pointsJSON, &unit.Points); err != nil {
			_ = rows.Close()
			return nil, s.invalidateAndSnap(ctx, "decode irradiance buffer for %s@%d: %v", unit.ChunkID, unit.Time, err)
		}
		units = append(units, &unit)
	}
	if err := rows.Err(); err != nil {
		return nil, s.invalidateAndSnap(ctx, "iterate unit checkpoints: %v", err)
	}
	return units, nil
}

// CommitOrder returns persisted units ordered by actual transaction sequence.
func (s *Store) CommitOrder(ctx context.Context) ([]bake.UnitRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT chunk_id, sun_time FROM units ORDER BY commit_seq ASC`)
	if err != nil {
		return nil, model.NewError(model.CodeIO, "query commit order: %v", err)
	}
	defer rows.Close()
	var out []bake.UnitRef
	for rows.Next() {
		var ref bake.UnitRef
		if err := rows.Scan(&ref.ChunkID, &ref.Time); err != nil {
			return nil, model.NewError(model.CodeIO, "scan commit order: %v", err)
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// Count returns the persisted unit count.
func (s *Store) Count(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM units`).Scan(&count); err != nil {
		return 0, model.NewError(model.CodeIO, "count checkpoints: %v", err)
	}
	return count, nil
}

// Path returns the SQLite file path.
func (s *Store) Path() string { return s.path }

// Close closes the database.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.db.Close()
}

func (s *Store) invalidateWhole(ctx context.Context, format string, args ...any) error {
	if err := s.Reset(ctx); err != nil {
		return err
	}
	return model.NewError(model.CodeSnap, format, args...).WithField("snapshot") // result discards reset detail
}

func (s *Store) invalidateAndSnap(ctx context.Context, format string, args ...any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewError(model.CodeSnap, format, args...).WithField("snapshot")
	}
	defer tx.Rollback()
	_, _ = tx.ExecContext(ctx, `DELETE FROM units`)
	_, _ = tx.ExecContext(ctx, `UPDATE snapshot SET revision=revision+1, updated_unix=? WHERE id=1`, time.Now().Unix())
	_ = tx.Commit()
	return model.NewError(model.CodeSnap, format, args...).WithField("snapshot")
}

func (s *Store) invalidateWholeLockedTx(ctx context.Context, tx *sql.Tx, message string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM units`); err != nil {
		return model.NewError(model.CodeIO, "clear snapshot units: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot SET revision=revision+1, updated_unix=? WHERE id=1`, time.Now().Unix()); err != nil {
		return model.NewError(model.CodeIO, "advance snapshot revision: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.CodeIO, "commit snapshot invalidation: %v", err)
	}
	return model.NewError(model.CodeSnap, "%s", message).WithField("snapshot")
}
