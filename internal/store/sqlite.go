package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"volumediag/internal/diag"
)

type Manifest struct {
	SourceHash       string            `json:"source_hash"`
	SceneHash        string            `json:"scene_hash"`
	ParamsVersion    string            `json:"params_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	MediumVersion    string            `json:"medium_version"`
	GeometryVersion  string            `json:"geometry_version"`
	LightVersions    map[string]string `json:"light_versions"`
	BatchSize        int               `json:"batch_size"`
}

type BlockRecord struct {
	CameraIndex      int               `json:"camera_index"`
	CameraID         string            `json:"camera_id"`
	X                int               `json:"x"`
	Y                int               `json:"y"`
	Payload          []byte            `json:"-"`
	Digest           string            `json:"digest"`
	ParamsVersion    string            `json:"params_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	MediumVersion    string            `json:"medium_version"`
	GeometryVersion  string            `json:"geometry_version"`
	LightVersions    map[string]string `json:"light_versions"`
}

type Restore struct {
	Invalidated bool
	Reason      string
	Blocks      []BlockRecord
	BatchCount  int
	Complete    bool
}

type Counts struct {
	Blocks  int `json:"blocks"`
	Batches int `json:"batches"`
	History int `json:"history"`
}

type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

func Open(path string, want Manifest) (*Store, Restore, error) {
	if path == "" {
		return nil, Restore{}, diag.New(diag.EPara, "checkpoint database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, Restore{}, diag.IO("create checkpoint directory", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, Restore{}, diag.IO("open checkpoint database", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, path: path}
	if err := s.configure(); err != nil {
		_ = db.Close()
		return nil, Restore{}, err
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, Restore{}, err
	}
	restore, err := s.restoreManifest(want)
	if err != nil {
		_ = db.Close()
		return nil, Restore{}, err
	}
	return s, restore, nil
}

func (s *Store) configure() error {
	stmts := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return diag.Wrap(diag.EIO, "configure SQLite", err)
		}
	}
	return nil
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS manifest (
			singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
			source_hash TEXT NOT NULL,
			scene_hash TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			medium_version TEXT NOT NULL,
			geometry_version TEXT NOT NULL,
			light_versions TEXT NOT NULL,
			batch_size INTEGER NOT NULL,
			updated_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS batches (
			batch_no INTEGER PRIMARY KEY AUTOINCREMENT,
			item_count INTEGER NOT NULL,
			digest TEXT NOT NULL,
			committed_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS blocks (
			camera_index INTEGER NOT NULL,
			camera_id TEXT NOT NULL,
			bx INTEGER NOT NULL,
			by INTEGER NOT NULL,
			payload BLOB NOT NULL,
			digest TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			medium_version TEXT NOT NULL,
			geometry_version TEXT NOT NULL,
			light_versions TEXT NOT NULL,
			revision INTEGER NOT NULL,
			batch_no INTEGER NOT NULL,
			committed_ns INTEGER NOT NULL,
			PRIMARY KEY (camera_index, bx, by)
		)`,
		`CREATE TABLE IF NOT EXISTS block_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			camera_index INTEGER NOT NULL,
			camera_id TEXT NOT NULL,
			bx INTEGER NOT NULL,
			by INTEGER NOT NULL,
			payload BLOB NOT NULL,
			digest TEXT NOT NULL,
			revision INTEGER NOT NULL,
			replaced_ns INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return diag.Wrap(diag.EIO, "migrate checkpoint schema", err)
		}
	}
	return nil
}

func (s *Store) restoreManifest(want Manifest) (Restore, error) {
	rows, err := s.db.Query(`SELECT source_hash, scene_hash, params_version, algorithm_version,
		medium_version, geometry_version, light_versions, batch_size FROM manifest WHERE singleton=1`)
	if err != nil {
		return Restore{}, diag.Wrap(diag.EIO, "read checkpoint manifest", err)
	}
	var got Manifest
	var lightsJSON string
	found := false
	if rows.Next() {
		found = true
		if err := rows.Scan(&got.SourceHash, &got.SceneHash, &got.ParamsVersion, &got.AlgorithmVersion,
			&got.MediumVersion, &got.GeometryVersion, &lightsJSON, &got.BatchSize); err != nil {
			_ = rows.Close()
			return Restore{}, diag.Wrap(diag.EIO, "scan checkpoint manifest", err)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return Restore{}, diag.Wrap(diag.EIO, "read checkpoint manifest", err)
	}
	_ = rows.Close()
	if found {
		if err := json.Unmarshal([]byte(lightsJSON), &got.LightVersions); err != nil {
			if err := s.wipeAll(); err != nil {
				return Restore{}, err
			}
			return s.installFresh(want, "corrupt_manifest_versions")
		}
	}
	if !found {
		if err := s.wipeAll(); err != nil {
			return Restore{}, err
		}
		return s.installFresh(want, "")
	}
	if !manifestsEqual(got, want) {
		if err := s.wipeAll(); err != nil {
			return Restore{}, err
		}
		return s.installFresh(want, "manifest_mismatch")
	}
	blocks, count, invalidated, err := s.loadBlocks(want)
	if err != nil {
		return Restore{}, err
	}
	if invalidated {
		return Restore{Invalidated: true, Reason: "block_version_mismatch"}, nil
	}
	complete, _ := s.meta("complete")
	reason := ""
	if complete == "" {
		reason = "partial_snapshot"
	}
	return Restore{
		Invalidated: false,
		Reason:      reason,
		Blocks:      blocks,
		BatchCount:  count,
		Complete:    complete == "1",
	}, nil
}

func (s *Store) installFresh(want Manifest, reason string) (Restore, error) {
	if err := s.writeManifest(nil, want, false); err != nil {
		return Restore{}, err
	}
	return Restore{Invalidated: reason != "", Reason: reason}, nil
}

func manifestsEqual(a, b Manifest) bool {
	if a.SourceHash != b.SourceHash || a.SceneHash != b.SceneHash ||
		a.ParamsVersion != b.ParamsVersion || a.AlgorithmVersion != b.AlgorithmVersion ||
		a.MediumVersion != b.MediumVersion || a.GeometryVersion != b.GeometryVersion ||
		len(a.LightVersions) != len(b.LightVersions) {
		return false
	}
	for id, v := range a.LightVersions {
		if b.LightVersions[id] != v {
			return false
		}
	}
	return true
}

func (s *Store) loadBlocks(want Manifest) ([]BlockRecord, int, bool, error) {
	rows, err := s.db.Query(`SELECT camera_index, camera_id, bx, by, payload, digest,
		params_version, algorithm_version, medium_version, geometry_version, light_versions
		FROM blocks ORDER BY camera_index, by, bx`)
	if err != nil {
		return nil, 0, false, diag.Wrap(diag.EIO, "read checkpoint blocks", err)
	}
	var out []BlockRecord
	for rows.Next() {
		var r BlockRecord
		var lightsJSON string
		if err := rows.Scan(&r.CameraIndex, &r.CameraID, &r.X, &r.Y, &r.Payload, &r.Digest,
			&r.ParamsVersion, &r.AlgorithmVersion, &r.MediumVersion, &r.GeometryVersion, &lightsJSON); err != nil {
			_ = rows.Close()
			return nil, 0, false, diag.Wrap(diag.EIO, "scan checkpoint block", err)
		}
		if err := json.Unmarshal([]byte(lightsJSON), &r.LightVersions); err != nil {
			_ = rows.Close()
			return nil, 0, false, diag.Wrap(diag.EIO, "decode checkpoint block versions", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, 0, false, diag.Wrap(diag.EIO, "read checkpoint blocks", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, false, diag.Wrap(diag.EIO, "close checkpoint block cursor", err)
	}
	for _, r := range out {
		valid := r.ParamsVersion == want.ParamsVersion &&
			r.AlgorithmVersion == want.AlgorithmVersion &&
			r.MediumVersion == want.MediumVersion
		if valid {
			for id, version := range r.LightVersions {
				if want.LightVersions[id] != version {
					valid = false
					break
				}
			}
		}
		if !valid {
			if err := s.wipeAll(); err != nil {
				return nil, 0, false, err
			}
			if err := s.writeManifest(nil, want, false); err != nil {
				return nil, 0, false, err
			}
			return nil, 0, true, nil
		}
	}
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM batches`).Scan(&count)
	return out, count, false, nil
}

func (s *Store) writeManifest(tx *sql.Tx, m Manifest, complete bool) error {
	lights, err := json.Marshal(m.LightVersions)
	if err != nil {
		return diag.Wrap(diag.EPara, "encode light versions", err)
	}
	now := time.Now().UnixNano()
	run := func(q interface {
		Exec(query string, args ...any) (sql.Result, error)
	}) error {
		_, err := q.Exec(`INSERT INTO manifest(singleton, source_hash, scene_hash, params_version,
			algorithm_version, medium_version, geometry_version, light_versions, batch_size, updated_ns)
			VALUES(1, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(singleton) DO UPDATE SET source_hash=excluded.source_hash,
			scene_hash=excluded.scene_hash, params_version=excluded.params_version,
			algorithm_version=excluded.algorithm_version, medium_version=excluded.medium_version,
			geometry_version=excluded.geometry_version, light_versions=excluded.light_versions,
			batch_size=excluded.batch_size, updated_ns=excluded.updated_ns`,
			m.SourceHash, m.SceneHash, m.ParamsVersion, m.AlgorithmVersion, m.MediumVersion,
			m.GeometryVersion, string(lights), m.BatchSize, now)
		return err
	}
	if tx != nil {
		err = run(tx)
	} else {
		err = run(s.db)
	}
	if err != nil {
		return diag.Wrap(diag.EIO, "write checkpoint manifest", err)
	}
	return s.setMeta(tx, "complete", boolString(complete))
}

func (s *Store) meta(key string) (string, error) {
	var value string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *Store) setMeta(tx *sql.Tx, key, value string) error {
	run := func(q interface {
		Exec(query string, args ...any) (sql.Result, error)
	}) error {
		_, err := q.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		return err
	}
	var err error
	if tx != nil {
		err = run(tx)
	} else {
		err = run(s.db)
	}
	if err != nil {
		return diag.Wrap(diag.EIO, "write checkpoint meta", err)
	}
	return nil
}

func (s *Store) CommitBatch(records []BlockRecord, m Manifest, complete bool) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if len(records) == 0 {
		if err := s.writeManifest(nil, m, complete); err != nil {
			return 0, err
		}
		var last int64
		_ = s.db.QueryRow(`SELECT COALESCE(MAX(batch_no),0) FROM batches`).Scan(&last)
		return last, nil
	}
	sorted := append([]BlockRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].CameraIndex != sorted[j].CameraIndex {
			return sorted[i].CameraIndex < sorted[j].CameraIndex
		}
		if sorted[i].Y != sorted[j].Y {
			return sorted[i].Y < sorted[j].Y
		}
		return sorted[i].X < sorted[j].X
	})
	tx, err := s.db.Begin()
	if err != nil {
		return 0, diag.Wrap(diag.EIO, "begin checkpoint batch", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.writeManifest(tx, m, complete); err != nil {
		return 0, err
	}
	var next int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(batch_no),0)+1 FROM batches`).Scan(&next); err != nil {
		return 0, diag.Wrap(diag.EIO, "allocate checkpoint batch", err)
	}
	for _, r := range sorted {
		var oldPayload []byte
		var oldDigest string
		var oldRevision int64
		err := tx.QueryRow(`SELECT payload, digest, revision FROM blocks
			WHERE camera_index=? AND bx=? AND by=?`, r.CameraIndex, r.X, r.Y).
			Scan(&oldPayload, &oldDigest, &oldRevision)
		if err == nil {
			if _, err := tx.Exec(`INSERT INTO block_history(camera_index, camera_id, bx, by, payload,
				digest, revision, replaced_ns) VALUES(?,?,?,?,?,?,?,?)`,
				r.CameraIndex, r.CameraID, r.X, r.Y, oldPayload, oldDigest, oldRevision, time.Now().UnixNano()); err != nil {
				return 0, diag.Wrap(diag.EIO, "archive replaced block", err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, diag.Wrap(diag.EIO, "read replaced block", err)
		}
		lights, err := json.Marshal(r.LightVersions)
		if err != nil {
			return 0, diag.Wrap(diag.EPara, "encode block light versions", err)
		}
		_, err = tx.Exec(`INSERT INTO blocks(camera_index, camera_id, bx, by, payload, digest,
			params_version, algorithm_version, medium_version, geometry_version, light_versions,
			revision, batch_no, committed_ns)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(camera_index,bx,by) DO UPDATE SET camera_id=excluded.camera_id,
			payload=excluded.payload, digest=excluded.digest, params_version=excluded.params_version,
			algorithm_version=excluded.algorithm_version, medium_version=excluded.medium_version,
			geometry_version=excluded.geometry_version, light_versions=excluded.light_versions,
			revision=excluded.revision, batch_no=excluded.batch_no, committed_ns=excluded.committed_ns`,
			r.CameraIndex, r.CameraID, r.X, r.Y, r.Payload, r.Digest, r.ParamsVersion,
			r.AlgorithmVersion, r.MediumVersion, r.GeometryVersion, string(lights),
			oldRevision+1, next, time.Now().UnixNano())
		if err != nil {
			return 0, diag.Wrap(diag.EIO, "write checkpoint block", err)
		}
	}
	batchDigest := BatchDigest(sorted)
	if _, err := tx.Exec(`INSERT INTO batches(batch_no,item_count,digest,committed_ns)
		VALUES(?,?,?,?)`, next, len(sorted), batchDigest, time.Now().UnixNano()); err != nil {
		return 0, diag.Wrap(diag.EIO, "write checkpoint batch", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, diag.Wrap(diag.EIO, "commit checkpoint batch", err)
	}
	return next, nil
}

func BatchDigest(records []BlockRecord) string {
	parts := make([]string, 0, len(records))
	for _, r := range records {
		parts = append(parts, fmt.Sprintf("%d:%s:%d:%d:%s", r.CameraIndex, r.CameraID, r.X, r.Y, r.Digest))
	}
	sort.Strings(parts)
	raw, _ := json.Marshal(parts)
	h := make([]byte, 0, 32)
	h = append(h, raw...)
	return fmt.Sprintf("%x", sha256Sum(h))
}

func sha256Sum(b []byte) [32]byte {
	return sha256.Sum256(b)
}

func (s *Store) wipeAll() error {
	tx, err := s.db.Begin()
	if err != nil {
		return diag.Wrap(diag.EIO, "begin checkpoint invalidation", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"blocks", "batches", "manifest", "meta"} {
		if _, err := tx.Exec("DELETE FROM " + table); err != nil {
			return diag.Wrap(diag.EIO, "wipe checkpoint snapshot", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return diag.Wrap(diag.EIO, "commit checkpoint invalidation", err)
	}
	return nil
}

// WipeSnapshot is used by the engine when a restored block fails an additional
// invariant check. It prevents any partial mixing of old and new batches.
func (s *Store) WipeSnapshot() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.wipeAll()
}

func (s *Store) SetComplete(m Manifest, complete bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeManifest(nil, m, complete)
}

func (s *Store) Counts() (Counts, error) {
	var c Counts
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blocks`).Scan(&c.Blocks); err != nil {
		return c, diag.Wrap(diag.EIO, "count checkpoint blocks", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM batches`).Scan(&c.Batches); err != nil {
		return c, diag.Wrap(diag.EIO, "count checkpoint batches", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM block_history`).Scan(&c.History); err != nil {
		return c, diag.Wrap(diag.EIO, "count checkpoint history", err)
	}
	return c, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func boolString(v bool) string {
	if v {
		return "1"
	}
	return "0"
}
