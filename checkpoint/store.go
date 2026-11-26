// Package checkpoint provides the task's allowed SQLite-equivalent local
// durable store. Commit order is: tenant/node/queue/reserve entity file
// first, atomic global committed-tick pointer second.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"quotakernel/model"
)

const metadataFile = "committed.json"

var (
	ErrNoCheckpoint    = errors.New("no committed checkpoint")
	ErrVersionMismatch = errors.New("checkpoint version mismatch")
)

type Metadata struct {
	FormatVersion      int        `json:"formatVersion"`
	SchemaVersions     []string   `json:"schemaVersions"`
	ProjectID          string     `json:"projectId"`
	ProjectFingerprint string     `json:"projectFingerprint"`
	CommittedTick      model.Tick `json:"committedTick"`
	EntityFile         string     `json:"entityFile"`
	EntitySHA256       string     `json:"entitySha256"`
}

type VersionMismatchError struct {
	Field string
	Want  string
	Got   string
}

func (e *VersionMismatchError) Error() string {
	return fmt.Sprintf("%s: %s want %s got %s", ErrVersionMismatch, e.Field, e.Want, e.Got)
}

func (e *VersionMismatchError) Unwrap() error { return ErrVersionMismatch }

type Store struct {
	Dir string

	// AfterEntityWrite is a test/diagnostic hook invoked after the entity
	// file and its directory fsync complete, but before committed.json moves.
	AfterEntityWrite func(tick model.Tick)
}

func NewStore(dir string) *Store {
	return &Store{Dir: dir}
}

func (s *Store) Ensure() error {
	if err := os.MkdirAll(filepath.Join(s.Dir, "ticks"), 0o755); err != nil {
		return err
	}
	return nil
}

// Commit writes the full entity snapshot first and only then advances the
// global committed tick pointer.
func (s *Store) Commit(snapshot model.Snapshot) (Metadata, error) {
	if err := s.Ensure(); err != nil {
		return Metadata{}, err
	}
	entityName, sum, err := s.WriteEntity(snapshot)
	if err != nil {
		return Metadata{}, err
	}
	meta := Metadata{
		FormatVersion:      model.StateFormatVersion,
		SchemaVersions:     []string{string(model.SchemaV1), string(model.SchemaV2), string(model.SchemaV3)},
		ProjectID:          snapshot.ProjectID,
		ProjectFingerprint: snapshot.ProjectFingerprint,
		CommittedTick:      snapshot.CommittedTick,
		EntityFile:         entityName,
		EntitySHA256:       sum,
	}
	if err := atomicWriteJSON(filepath.Join(s.Dir, metadataFile), meta); err != nil {
		return Metadata{}, fmt.Errorf("write commit pointer: %w", err)
	}
	return meta, nil
}

// WriteEntity persists tenant/node/queue/reserve state and invokes the crash
// hook after fsync. It does not advance committed.json.
func (s *Store) WriteEntity(snapshot model.Snapshot) (string, string, error) {
	if err := s.Ensure(); err != nil {
		return "", "", err
	}
	entityName := filepath.ToSlash(filepath.Join("ticks", fmt.Sprintf("%012d.entities.json", snapshot.CommittedTick)))
	entityPath := filepath.Join(s.Dir, filepath.FromSlash(entityName))
	sum, err := atomicWriteJSONWithChecksum(entityPath, snapshot)
	if err != nil {
		return "", "", fmt.Errorf("write entity state: %w", err)
	}
	if s.AfterEntityWrite != nil {
		s.AfterEntityWrite(snapshot.CommittedTick)
	}
	return entityName, sum, nil
}

func (s *Store) Load(expectedFingerprint string) (model.Snapshot, Metadata, error) {
	metaPath := filepath.Join(s.Dir, metadataFile)
	data, err := os.ReadFile(metaPath)
	if errors.Is(err, fs.ErrNotExist) {
		return model.Snapshot{}, Metadata{}, ErrNoCheckpoint
	}
	if err != nil {
		return model.Snapshot{}, Metadata{}, err
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return model.Snapshot{}, Metadata{}, fmt.Errorf("parse commit pointer: %w", err)
	}
	if meta.FormatVersion != model.StateFormatVersion {
		return model.Snapshot{}, meta, &VersionMismatchError{
			Field: "formatVersion", Want: fmt.Sprint(model.StateFormatVersion), Got: fmt.Sprint(meta.FormatVersion),
		}
	}
	if expectedFingerprint != "" && meta.ProjectFingerprint != expectedFingerprint {
		return model.Snapshot{}, meta, &VersionMismatchError{
			Field: "projectFingerprint", Want: expectedFingerprint, Got: meta.ProjectFingerprint,
		}
	}
	entityPath := filepath.Join(s.Dir, filepath.FromSlash(meta.EntityFile))
	entityData, err := os.ReadFile(entityPath)
	if err != nil {
		return model.Snapshot{}, meta, fmt.Errorf("read committed entities: %w", err)
	}
	sum := sha256.Sum256(entityData)
	if got := hex.EncodeToString(sum[:]); meta.EntitySHA256 != "" && got != meta.EntitySHA256 {
		return model.Snapshot{}, meta, fmt.Errorf("entity checksum mismatch: want %s got %s", meta.EntitySHA256, got)
	}
	var snapshot model.Snapshot
	if err := json.Unmarshal(entityData, &snapshot); err != nil {
		return model.Snapshot{}, meta, fmt.Errorf("parse committed entities: %w", err)
	}
	if snapshot.FormatVersion != model.StateFormatVersion {
		return model.Snapshot{}, meta, &VersionMismatchError{
			Field: "snapshot.formatVersion", Want: fmt.Sprint(model.StateFormatVersion), Got: fmt.Sprint(snapshot.FormatVersion),
		}
	}
	return snapshot, meta, nil
}

// Reset removes all old ledger state. It is used only after a version or
// fingerprint mismatch, ensuring no partial old ledger is reused.
func (s *Store) Reset() error {
	if err := os.RemoveAll(s.Dir); err != nil {
		return err
	}
	return s.Ensure()
}

func atomicWriteJSON(path string, value any) error {
	_, err := atomicWriteJSONWithChecksum(path, value)
	return err
}

func atomicWriteJSONWithChecksum(path string, value any) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return "", err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

type ExitReport struct {
	Signal        string     `json:"signal,omitempty"`
	CompletedTick model.Tick `json:"completedTick"`
	Mode          string     `json:"mode"`
	OK            bool       `json:"ok"`
	Message       string     `json:"message,omitempty"`
}

func WriteExitReport(dir string, report ExitReport) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "exit-report.json")
	if err := atomicWriteJSON(path, report); err != nil {
		return "", err
	}
	return path, nil
}
