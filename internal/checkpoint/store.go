package checkpoint

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrSnapshotMismatch = errors.New("checkpoint snapshot mismatch")

type MismatchError struct {
	Reason string
	RoomID string
	Field  string
	Stored string
	Wanted string
}

func (e *MismatchError) Error() string {
	if e.RoomID != "" {
		return fmt.Sprintf("%s: room=%s field=%s stored=%q wanted=%q", ErrSnapshotMismatch, e.RoomID, e.Field, e.Stored, e.Wanted)
	}
	return fmt.Sprintf("%s: %s stored=%q wanted=%q", ErrSnapshotMismatch, e.Reason, e.Stored, e.Wanted)
}

func (e *MismatchError) Unwrap() error { return ErrSnapshotMismatch }

type RoomSnapshot struct {
	RoomID           string          `json:"room_id"`
	RoomIndex        int             `json:"room_index"`
	CommitSeq        int             `json:"commit_seq"`
	Payload          json.RawMessage `json:"payload"`
	PayloadHash      string          `json:"payload_hash"`
	SHDigest         string          `json:"sh_digest"`
	Dependencies     []string        `json:"dependencies"`
	InputHash        string          `json:"input_hash"`
	SamplingVersion  string          `json:"sampling_version"`
	AlgorithmVersion string          `json:"algorithm_version"`
}

type Meta struct {
	InputHash        string
	SamplingVersion  string
	AlgorithmVersion string
	CreatedAt        time.Time
}

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
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
	store := &Store{db: db, path: path}
	if err := store.init(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func OpenValidOrReset(ctx context.Context, path, inputHash, samplingVersion, algorithmVersion string) (*Store, []RoomSnapshot, bool, error) {
	store, err := Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	snapshots, err := store.Load(ctx, inputHash, samplingVersion, algorithmVersion)
	if err == nil {
		return store, snapshots, false, nil
	}
	if !errors.Is(err, ErrSnapshotMismatch) {
		_ = store.Close()
		return nil, nil, false, err
	}
	_ = store.Close()
	if err := RemoveFiles(path); err != nil {
		return nil, nil, false, err
	}
	store, err = Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	return store, nil, true, nil
}

func RemoveFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS rooms (
	room_id TEXT PRIMARY KEY,
	room_index INTEGER NOT NULL,
	commit_seq INTEGER NOT NULL UNIQUE,
	payload_json BLOB NOT NULL,
	payload_hash TEXT NOT NULL,
	sh_digest TEXT NOT NULL,
	dependencies_json TEXT NOT NULL,
	input_hash TEXT NOT NULL,
	sampling_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	committed_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_rooms_commit_seq ON rooms(commit_seq);
`)
	return err
}

func HashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func HashJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return HashBytes(data), nil
}

func (s *Store) SaveRoom(ctx context.Context, snapshot RoomSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(snapshot.Payload) == 0 {
		return errors.New("checkpoint: empty room payload")
	}
	payloadHash := HashBytes(snapshot.Payload)
	if snapshot.PayloadHash == "" {
		snapshot.PayloadHash = payloadHash
	} else if snapshot.PayloadHash != payloadHash {
		return &MismatchError{Reason: "payload hash collision before write", RoomID: snapshot.RoomID, Field: "payload_hash"}
	}
	depsJSON, err := json.Marshal(snapshot.Dependencies)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	expectedMeta := map[string]string{
		"input_hash":        snapshot.InputHash,
		"sampling_version":  snapshot.SamplingVersion,
		"algorithm_version": snapshot.AlgorithmVersion,
	}
	for key, value := range expectedMeta {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO meta(key, value) VALUES(?, ?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value
`, key, value); err != nil {
			return err
		}
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO rooms(
	room_id, room_index, commit_seq, payload_json, payload_hash, sh_digest,
	dependencies_json, input_hash, sampling_version, algorithm_version, committed_at
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(room_id) DO UPDATE SET
	room_index=excluded.room_index,
	commit_seq=excluded.commit_seq,
	payload_json=excluded.payload_json,
	payload_hash=excluded.payload_hash,
	sh_digest=excluded.sh_digest,
	dependencies_json=excluded.dependencies_json,
	input_hash=excluded.input_hash,
	sampling_version=excluded.sampling_version,
	algorithm_version=excluded.algorithm_version,
	committed_at=excluded.committed_at`,
		snapshot.RoomID, snapshot.RoomIndex, snapshot.CommitSeq, snapshot.Payload, snapshot.PayloadHash,
		snapshot.SHDigest, string(depsJSON), snapshot.InputHash, snapshot.SamplingVersion,
		snapshot.AlgorithmVersion, time.Now().UTC().Format(time.RFC3339Nano),
	)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Load(ctx context.Context, inputHash, samplingVersion, algorithmVersion string) ([]RoomSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	meta := make(map[string]string)
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			_ = rows.Close()
			return nil, err
		}
		meta[key] = value
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	if len(meta) > 0 {
		checks := []struct {
			key, field, wanted string
		}{
			{"input_hash", "input_hash", inputHash},
			{"sampling_version", "sampling_version", samplingVersion},
			{"algorithm_version", "algorithm_version", algorithmVersion},
		}
		for _, check := range checks {
			if meta[check.key] != check.wanted {
				return nil, &MismatchError{
					Reason: "global version/hash mismatch", Field: check.field,
					Stored: meta[check.key], Wanted: check.wanted,
				}
			}
		}
	}

	rows, err = s.db.QueryContext(ctx, `
SELECT room_id, room_index, commit_seq, payload_json, payload_hash, sh_digest,
       dependencies_json, input_hash, sampling_version, algorithm_version
FROM rooms ORDER BY commit_seq ASC, room_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []RoomSnapshot
	for rows.Next() {
		var snap RoomSnapshot
		var depsJSON string
		if err := rows.Scan(
			&snap.RoomID, &snap.RoomIndex, &snap.CommitSeq, &snap.Payload, &snap.PayloadHash,
			&snap.SHDigest, &depsJSON, &snap.InputHash, &snap.SamplingVersion, &snap.AlgorithmVersion,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(depsJSON), &snap.Dependencies); err != nil {
			return nil, err
		}
		if got := HashBytes(snap.Payload); got != snap.PayloadHash {
			return nil, &MismatchError{
				Reason: "payload hash mismatch", RoomID: snap.RoomID, Field: "payload",
				Stored: snap.PayloadHash, Wanted: got,
			}
		}
		for _, check := range []struct {
			field, stored, wanted string
		}{
			{"input_hash", snap.InputHash, inputHash},
			{"sampling_version", snap.SamplingVersion, samplingVersion},
			{"algorithm_version", snap.AlgorithmVersion, algorithmVersion},
		} {
			if check.stored != check.wanted {
				return nil, &MismatchError{
					Reason: "room version/hash mismatch", RoomID: snap.RoomID, Field: check.field,
					Stored: check.stored, Wanted: check.wanted,
				}
			}
		}
		snapshots = append(snapshots, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return snapshots, nil
}
