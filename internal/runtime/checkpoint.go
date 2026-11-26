// Package runtime runs parallel pod scoring under explicit concurrency and
// buffer budgets and provides crash-safe, version-checked checkpointing.
//
// Persistence uses an in-repository file store (no third-party driver) with the
// same durability contract as SQLite: data files are fsynced and a global
// commit pointer is updated atomically only after the node snapshot and score
// cache are durable, so a crash at any commit point can never expose a cache
// entry pointing at an unfinished snapshot.
package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	magic        = "SCHEDLAB-CP"
	stateVersion = 3
	commitName   = "commit.json"
)

// Frame is one durable scoring result.
type Frame struct {
	PodID       string `json:"pod_id"`
	Selected    string `json:"selected_node"`
	Normalized  int64  `json:"normalized_bps"`
	RuleVersion string `json:"rule_version"`
	SnapshotSeq int64  `json:"snapshot_seq"`
}

// Snapshot is the node snapshot a frame references.
type Snapshot struct {
	Seq      int64      `json:"seq"`
	RuleVer  string     `json:"rule_version"`
	NodeHash string     `json:"node_hash"`
	Nodes    []NodeView `json:"nodes"`
}

// NodeView is the minimal persisted node state needed to validate a snapshot.
type NodeView struct {
	ID       string `json:"id"`
	CPU      int64  `json:"cpu_milli"`
	Mem      int64  `json:"mem_bytes"`
	Topology string `json:"topology_digest"`
}

type envelope struct {
	Magic        string          `json:"magic"`
	StateVersion int             `json:"state_version"`
	Payload      json.RawMessage `json:"payload"`
	Checksum     string          `json:"checksum"`
}

type commitPointer struct {
	Magic        string `json:"magic"`
	StateVersion int    `json:"state_version"`
	SnapshotSeq  int64  `json:"snapshot_seq"`
	RuleVersion  string `json:"rule_version"`
	Checksum     string `json:"checksum"`
}

// Store is the crash-safe checkpoint directory.
type Store struct {
	dir string
}

// OpenStore prepares (or reopens) a checkpoint directory.
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir returns the checkpoint directory path.
func (s *Store) Dir() string { return s.dir }

func snapshotPath(seq int64) string { return fmt.Sprintf("snapshot-%d.json", seq) }
func cachePath(seq int64) string    { return fmt.Sprintf("cache-%d.json", seq) }

// Commit persists a snapshot then its cache, and only afterwards atomically
// updates the global commit pointer. The ordering is the required
// snapshot-before-commit-sequence guarantee.
func (s *Store) Commit(snap Snapshot, frames []Frame) error {
	if err := s.writeChecked(snapshotPath(snap.Seq), snap); err != nil {
		return err
	}
	if err := s.fsyncDir(); err != nil {
		return err
	}
	if err := s.writeChecked(cachePath(snap.Seq), frames); err != nil {
		return err
	}
	if err := s.fsyncDir(); err != nil {
		return err
	}
	ptr := commitPointer{
		Magic: magic, StateVersion: stateVersion,
		SnapshotSeq: snap.Seq, RuleVersion: snap.RuleVer,
		Checksum: snap.NodeHash,
	}
	if err := s.writeCommitAtomic(ptr); err != nil {
		return err
	}
	return s.fsyncDir()
}

func (s *Store) writeChecked(name string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	env := envelope{
		Magic: magic, StateVersion: stateVersion,
		Payload: payload, Checksum: hex.EncodeToString(sum[:]),
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func (s *Store) writeCommitAtomic(ptr commitPointer) error {
	data, err := json.MarshalIndent(ptr, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(s.dir, commitName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func (s *Store) fsyncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	_ = d.Close()
	return err
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	err = f.Sync()
	_ = f.Close()
	return err
}

// ErrNoCheckpoint means no committed pointer exists yet.
var ErrNoCheckpoint = errors.New("no committed checkpoint")

// LoadCommit reads and validates the current commit pointer.
func (s *Store) LoadCommit() (commitPointer, error) {
	var ptr commitPointer
	data, err := os.ReadFile(filepath.Join(s.dir, commitName))
	if os.IsNotExist(err) {
		return ptr, ErrNoCheckpoint
	}
	if err != nil {
		return ptr, err
	}
	if err := json.Unmarshal(data, &ptr); err != nil {
		return ptr, err
	}
	if ptr.Magic != magic || ptr.StateVersion != stateVersion {
		return ptr, ErrStateVersion
	}
	return ptr, nil
}

// LoadSnapshot validates magic, state version and checksum before decoding.
func (s *Store) LoadSnapshot(seq int64) (*Snapshot, error) {
	var snap Snapshot
	if err := s.readChecked(snapshotPath(seq), &snap); err != nil {
		return nil, mapStoreErr(err)
	}
	return &snap, nil
}

// LoadCache validates and decodes the frame cache for a snapshot.
func (s *Store) LoadCache(seq int64) ([]Frame, error) {
	var frames []Frame
	if err := s.readChecked(cachePath(seq), &frames); err != nil {
		return nil, mapStoreErr(err)
	}
	return frames, nil
}

func (s *Store) readChecked(name string, dst any) error {
	data, err := os.ReadFile(filepath.Join(s.dir, name))
	if os.IsNotExist(err) {
		return ErrCheckpointTruncated
	}
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return ErrCheckpointTruncated
	}
	if env.Magic != magic {
		return ErrCheckpointTruncated
	}
	if env.StateVersion != stateVersion {
		return ErrStateVersion
	}
	// json.MarshalIndent re-indents the embedded RawMessage; compact it back
	// to the exact bytes that were checksummed at write time.
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, env.Payload); err != nil {
		return ErrCheckpointTruncated
	}
	env.Payload = compacted.Bytes()
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.Checksum {
		return ErrCheckpointTruncated
	}
	if err := json.Unmarshal(env.Payload, dst); err != nil {
		return ErrCheckpointTruncated
	}
	return nil
}

// Reset removes all checkpoint files, forcing a from-scratch recompute.
func (s *Store) Reset() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Sentinel errors mapped to stable business codes by callers.
var (
	ErrCheckpointTruncated = errors.New("checkpoint truncated or corrupt")
	ErrStateVersion        = errors.New("checkpoint state version mismatch")
)
