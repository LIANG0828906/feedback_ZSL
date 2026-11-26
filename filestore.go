package vpf

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileStore is the zero-third-party durable fallback. It uses the same
// CheckpointRecord contract and atomic temp-file rename per chunk.
type FileStore struct {
	dir string
	mu  sync.Mutex
	seq int64
}

func NewFileStore(dir string) *FileStore {
	return &FileStore{dir: dir}
}

func (s *FileStore) Init(context.Context) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		var record CheckpointRecord
		if json.Unmarshal(data, &record) == nil && record.CommitSeq > s.seq {
			s.seq = record.CommitSeq
		}
	}
	return nil
}

func (s *FileStore) path(frame, chunk int) string {
	return filepath.Join(s.dir, fmt.Sprintf("f%04d-c%04d.json", frame, chunk))
}

func (s *FileStore) PutCheckpoint(_ context.Context, record CheckpointRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	record.CommitSeq = s.seq
	if record.ChunkID == "" {
		record.ChunkID = ChunkID(record.FrameIndex, record.ChunkIndex)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return 0, err
	}
	path := s.path(record.FrameIndex, record.ChunkIndex)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	return record.CommitSeq, nil
}

func (s *FileStore) GetCheckpoint(_ context.Context, frame, chunk int) (CheckpointRecord, bool, error) {
	data, err := os.ReadFile(s.path(frame, chunk))
	if os.IsNotExist(err) {
		return CheckpointRecord{}, false, nil
	}
	if err != nil {
		return CheckpointRecord{}, false, err
	}
	var record CheckpointRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return CheckpointRecord{}, false, err
	}
	return record, true, nil
}

func (s *FileStore) ListCheckpoints(ctx context.Context, frame int) ([]CheckpointRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var records []CheckpointRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record CheckpointRecord
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if record.FrameIndex == frame {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].ChunkIndex < records[j].ChunkIndex })
	return records, nil
}

func (s *FileStore) Close() error { return nil }
