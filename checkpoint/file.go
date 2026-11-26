package checkpoint

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"iconflow/errs"
)

type snapshot struct {
	StageSeq  int64          `json:"stage_seq"`
	WindowSeq int64          `json:"window_seq"`
	Stages    []StageCommit  `json:"stages"`
	Windows   []WindowCommit `json:"windows"`
}

type FileStore struct {
	path string
	mu   sync.Mutex
	data snapshot
}

func OpenFileStore(path string) (*FileStore, error) {
	store := &FileStore{path: path}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, errs.New(errs.ECheck, path, "create checkpoint directory: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return store, nil
		}
		return nil, errs.New(errs.ECheck, path, "read checkpoint: %v", err)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &store.data); err != nil {
			return nil, errs.New(errs.ECheck, path, "decode checkpoint: %v", err)
		}
	}
	return store, nil
}

func (s *FileStore) persistLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return errs.New(errs.ECheck, s.path, "encode checkpoint: %v", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return errs.New(errs.ECheck, s.path, "create transaction temp file: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return errs.New(errs.ECheck, s.path, "write transaction: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return errs.New(errs.ECheck, s.path, "fsync transaction: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return errs.New(errs.ECheck, s.path, "close transaction: %v", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return errs.New(errs.ECheck, s.path, "atomic commit rename: %v", err)
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func (s *FileStore) CommitStage(_ context.Context, commit StageCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.StageSeq++
	commit.Seq = s.data.StageSeq
	if commit.CommittedAt.IsZero() {
		commit.CommittedAt = time.Now().UTC()
	}
	s.data.Stages = append(s.data.Stages, commit)
	if err := s.persistLocked(); err != nil {
		s.data.Stages = s.data.Stages[:len(s.data.Stages)-1]
		s.data.StageSeq--
		return 0, err
	}
	return commit.Seq, nil
}

func (s *FileStore) CommitWindow(_ context.Context, commit WindowCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.WindowSeq++
	commit.Seq = s.data.WindowSeq
	if commit.CommittedAt.IsZero() {
		commit.CommittedAt = time.Now().UTC()
	}
	s.data.Windows = append(s.data.Windows, commit)
	if err := s.persistLocked(); err != nil {
		s.data.Windows = s.data.Windows[:len(s.data.Windows)-1]
		s.data.WindowSeq--
		return 0, err
	}
	return commit.Seq, nil
}

func (s *FileStore) StageHistory(_ context.Context, projectID string) ([]StageCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []StageCommit{}
	for _, commit := range s.data.Stages {
		if projectID == "" || commit.ProjectID == projectID {
			out = append(out, commit)
		}
	}
	return out, nil
}

func (s *FileStore) WindowHistory(_ context.Context, projectID string) ([]WindowCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []WindowCommit{}
	for _, commit := range s.data.Windows {
		if projectID == "" || commit.ProjectID == projectID {
			out = append(out, commit)
		}
	}
	return out, nil
}

func (s *FileStore) Close() error { return nil }
