package session

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"volumelab/pkg/model"
)

type SnapshotPayload struct {
	Version          int    `json:"version"`
	AlgorithmVersion string `json:"algorithmVersion"`
	VolumeID         string `json:"volumeId"`
	ContentHash      string `json:"contentHash"`
	ManifestHash     string `json:"manifestHash"`
	State            State  `json:"state"`
}

type SnapshotInfo struct {
	Path             string `json:"path"`
	Exists           bool   `json:"exists"`
	Version          int    `json:"version"`
	AlgorithmVersion string `json:"algorithmVersion"`
	Sequence         uint64 `json:"sequence"`
	Checksum         string `json:"checksum,omitempty"`
}

type LoadResult struct {
	Payload       *SnapshotPayload
	Info          SnapshotInfo
	Recovered     bool
	InvalidReason string
}

type snapshotEnvelope struct {
	Format   string `json:"format"`
	Payload  string `json:"payload"`
	Checksum string `json:"checksum"`
}

type Store struct {
	Path string
	mu   sync.Mutex
}

func NewStore(path string) *Store {
	return &Store{Path: path}
}

func (s *Store) Load() LoadResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := LoadResult{Info: SnapshotInfo{Path: s.Path}}
	if s.Path == "" {
		result.InvalidReason = "snapshot path is empty"
		return result
	}
	fileBytes, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result
		}
		result.InvalidReason = "snapshot cannot be read: " + err.Error()
		return result
	}
	result.Info.Exists = true
	var envelope snapshotEnvelope
	if err := json.Unmarshal(fileBytes, &envelope); err != nil {
		result.InvalidReason = "snapshot envelope is not valid JSON"
		return result
	}
	if envelope.Format != SnapshotFileFormat {
		result.InvalidReason = "snapshot envelope format mismatch"
		return result
	}
	payloadBytes, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		result.InvalidReason = "snapshot payload is not valid base64"
		return result
	}
	actualChecksum := checksum(payloadBytes)
	result.Info.Checksum = actualChecksum
	if envelope.Checksum != actualChecksum {
		result.InvalidReason = "snapshot checksum mismatch"
		return result
	}
	var payload SnapshotPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		result.InvalidReason = "snapshot payload is not valid JSON"
		return result
	}
	result.Info.Version = payload.Version
	result.Info.AlgorithmVersion = payload.AlgorithmVersion
	result.Info.Sequence = payload.State.Sequence
	if payload.Version != SnapshotFormatVersion {
		result.InvalidReason = "snapshot version mismatch"
		return result
	}
	if payload.AlgorithmVersion != AlgorithmVersion {
		result.InvalidReason = "snapshot algorithm version mismatch"
		return result
	}
	result.Payload = &payload
	result.Recovered = true
	return result
}

func (s *Store) Commit(ctx context.Context, payload SnapshotPayload) (SnapshotInfo, *model.AppError) {
	if err := ctx.Err(); err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_CANCEL", Message: "snapshot commit canceled", Details: map[string]any{"cause": err.Error()}}
	}
	if s.Path == "" {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "snapshot path is empty", Field: "snapshotPath"}
	}
	if payload.Version != SnapshotFormatVersion || payload.AlgorithmVersion != AlgorithmVersion {
		return SnapshotInfo{}, &model.AppError{Code: "E_VERSION", Message: "refusing to write an incompatible snapshot"}
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot encode snapshot payload", Details: map[string]any{"cause": err.Error()}}
	}
	sum := checksum(payloadBytes)
	envelope := snapshotEnvelope{
		Format:   SnapshotFileFormat,
		Payload:  base64.StdEncoding.EncodeToString(payloadBytes),
		Checksum: sum,
	}
	fileBytes, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot encode snapshot envelope", Details: map[string]any{"cause": err.Error()}}
	}
	fileBytes = append(fileBytes, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_CANCEL", Message: "snapshot commit canceled before atomic rename", Details: map[string]any{"cause": err.Error()}}
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot create snapshot directory", Details: map[string]any{"cause": err.Error()}}
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".session-*.tmp")
	if err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot create snapshot temporary file", Details: map[string]any{"cause": err.Error()}}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot set snapshot permissions", Details: map[string]any{"cause": err.Error()}}
	}
	if _, err := tmp.Write(fileBytes); err != nil {
		tmp.Close()
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot write snapshot", Details: map[string]any{"cause": err.Error()}}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot fsync snapshot", Details: map[string]any{"cause": err.Error()}}
	}
	if err := tmp.Close(); err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot close snapshot", Details: map[string]any{"cause": err.Error()}}
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return SnapshotInfo{}, &model.AppError{Code: "E_IO", Message: "cannot atomically publish snapshot", Details: map[string]any{"cause": err.Error()}}
	}
	syncDir(filepath.Dir(s.Path))
	return SnapshotInfo{
		Path:             s.Path,
		Exists:           true,
		Version:          payload.Version,
		AlgorithmVersion: payload.AlgorithmVersion,
		Sequence:         payload.State.Sequence,
		Checksum:         sum,
	}, nil
}

func (s *Store) Invalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Path == "" {
		return nil
	}
	err := os.Remove(s.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(s.Path))
	return nil
}

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func syncDir(path string) {
	dir, err := os.Open(path)
	if err != nil {
		return
	}
	_ = dir.Sync()
	_ = dir.Close()
}
