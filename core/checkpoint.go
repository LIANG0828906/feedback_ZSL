package dialoguecal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

type Checkpoint struct {
	ProjectVersion  string           `json:"project_version"`
	LastFrameSeq    int64            `json:"last_frame_seq"`
	Frames          []FrameResult    `json:"frames"`
	SegmentProgress map[string]int64 `json:"segment_progress"`
}

type CheckpointStore interface {
	CommitFrame(ctx context.Context, version string, result FrameResult, segmentLastLocal int64) error
	LoadCheckpoint(ctx context.Context, expectedVersion string) (*Checkpoint, error)
	Reset(ctx context.Context) error
	Close() error
}

type FileCheckpointStore struct {
	dir string
}

func NewFileCheckpointStore(dir string) (*FileCheckpointStore, error) {
	if err := os.MkdirAll(filepath.Join(dir, "frames"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "segments"), 0o755); err != nil {
		return nil, err
	}
	return &FileCheckpointStore{dir: dir}, nil
}

func (s *FileCheckpointStore) Close() error { return nil }

func (s *FileCheckpointStore) Reset(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, "meta.json")); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func writeAtomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *FileCheckpointStore) framePath(seq int64) string {
	return filepath.Join(s.dir, "frames", fmt.Sprintf("frame-%08d.json", seq))
}

func (s *FileCheckpointStore) segmentPath(id string) string {
	return filepath.Join(s.dir, "segments", id+".json")
}

// CommitFrame persists in the required order: frame state, segment progress,
// and only then the global sequence cursor.
func (s *FileCheckpointStore) CommitFrame(ctx context.Context, version string, result FrameResult, segmentLastLocal int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writeAtomicJSON(s.framePath(int64(result.Frame.GlobalIndex)), result); err != nil {
		return err
	}
	progress := map[string]any{"segment_id": result.Frame.SegmentID, "last_local_index": segmentLastLocal, "last_global_seq": result.Frame.GlobalIndex, "project_version": version}
	if err := writeAtomicJSON(s.segmentPath(result.Frame.SegmentID), progress); err != nil {
		return err
	}
	meta := map[string]any{"project_version": version, "last_frame_seq": result.Frame.GlobalIndex}
	if err := writeAtomicJSON(filepath.Join(s.dir, "meta.json"), meta); err != nil {
		return err
	}
	return nil
}

func classifyCheckpointReadError(err error, path string) error {
	if err == nil {
		return nil
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrInvalid) {
		return &BusinessError{Code: CodeCheckpointTruncated, Path: path, Detail: err.Error()}
	}
	if errors.As(err, &syntaxErr) {
		return &BusinessError{Code: CodeCheckpointTruncated, Path: path, Detail: fmt.Sprintf("truncated or invalid JSON at byte %d", syntaxErr.Offset), Context: map[string]any{"offset": syntaxErr.Offset}}
	}
	if errors.As(err, &typeErr) {
		return &BusinessError{Code: CodeCheckpointCorrupt, Path: path, Detail: err.Error()}
	}
	return &BusinessError{Code: CodeCheckpointCorrupt, Path: path, Detail: err.Error()}
}

func (s *FileCheckpointStore) LoadCheckpoint(ctx context.Context, expectedVersion string) (*Checkpoint, error) {
	metaPath := filepath.Join(s.dir, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Checkpoint{ProjectVersion: expectedVersion, LastFrameSeq: -1, Frames: []FrameResult{}, SegmentProgress: map[string]int64{}}, nil
		}
		return nil, classifyCheckpointReadError(err, metaPath)
	}
	var meta struct {
		ProjectVersion string `json:"project_version"`
		LastFrameSeq   int64  `json:"last_frame_seq"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, classifyCheckpointReadError(err, metaPath)
	}
	if meta.ProjectVersion != expectedVersion {
		return nil, &BusinessError{Code: CodeCheckpointVersion, Path: metaPath, Detail: "checkpoint project version does not match current snapshot", Context: map[string]any{"checkpoint_version": meta.ProjectVersion, "expected_version": expectedVersion, "last_frame_seq": meta.LastFrameSeq}}
	}

	cp := &Checkpoint{ProjectVersion: meta.ProjectVersion, LastFrameSeq: meta.LastFrameSeq, SegmentProgress: map[string]int64{}}
	for seq := int64(0); seq <= meta.LastFrameSeq; seq++ {
		path := s.framePath(seq)
		frameData, err := os.ReadFile(path)
		if err != nil {
			return nil, classifyCheckpointReadError(err, path)
		}
		var result FrameResult
		if err := json.Unmarshal(frameData, &result); err != nil {
			return nil, classifyCheckpointReadError(err, path)
		}
		cp.Frames = append(cp.Frames, result)
	}
	segEntries, err := os.ReadDir(filepath.Join(s.dir, "segments"))
	if err == nil {
		for _, entry := range segEntries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			segData, err := os.ReadFile(filepath.Join(s.dir, "segments", entry.Name()))
			if err != nil {
				return nil, classifyCheckpointReadError(err, entry.Name())
			}
			var progress struct {
				SegmentID      string `json:"segment_id"`
				LastLocalIndex int64  `json:"last_local_index"`
			}
			if err := json.Unmarshal(segData, &progress); err != nil {
				return nil, classifyCheckpointReadError(err, entry.Name())
			}
			cp.SegmentProgress[progress.SegmentID] = progress.LastLocalIndex
		}
	}
	return cp, nil
}

// WriteTruncatedMeta is used by failure injection and recovery demos.
func WriteTruncatedMeta(dir, version string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "meta.json")
	payload := []byte(fmt.Sprintf("{\n  \"project_version\": %q,\n  \"last_frame_seq\": 12,\n  \"frames\": [", version))
	return os.WriteFile(path, payload, 0o644)
}
