package checkpoint

import (
	"context"
	"sync"
	"time"
)

type StageCommit struct {
	Seq          int64     `json:"seq"`
	ProjectID    string    `json:"project_id"`
	BlockKey     string    `json:"block_key"`
	Stage        string    `json:"stage"`
	Status       string    `json:"status"`
	Summary      string    `json:"summary"`
	Threshold    int       `json:"threshold"`
	CacheKey     string    `json:"cache_key"`
	InputHash    string    `json:"input_hash"`
	ContentHash  string    `json:"content_hash"`
	ParamVersion string    `json:"param_version"`
	AlgoVersion  string    `json:"algo_version"`
	ArtifactJSON []byte    `json:"artifact_json,omitempty"`
	CommittedAt  time.Time `json:"committed_at"`
}

type WindowCommit struct {
	Seq          int64     `json:"seq"`
	ProjectID    string    `json:"project_id"`
	WindowID     string    `json:"window_id"`
	Status       string    `json:"status"`
	Summary      string    `json:"summary"`
	ContentHash  string    `json:"content_hash"`
	ParamVersion string    `json:"param_version"`
	AlgoVersion  string    `json:"algo_version"`
	CommittedAt  time.Time `json:"committed_at"`
}

type Store interface {
	CommitStage(ctx context.Context, commit StageCommit) (int64, error)
	CommitWindow(ctx context.Context, commit WindowCommit) (int64, error)
	StageHistory(ctx context.Context, projectID string) ([]StageCommit, error)
	WindowHistory(ctx context.Context, projectID string) ([]WindowCommit, error)
	Close() error
}

type MemoryStore struct {
	mu        sync.Mutex
	stageSeq  int64
	windowSeq int64
	Stages    []StageCommit
	Windows   []WindowCommit
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (s *MemoryStore) CommitStage(_ context.Context, commit StageCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stageSeq++
	commit.Seq = s.stageSeq
	if commit.CommittedAt.IsZero() {
		commit.CommittedAt = time.Now().UTC()
	}
	s.Stages = append(s.Stages, commit)
	return commit.Seq, nil
}

func (s *MemoryStore) CommitWindow(_ context.Context, commit WindowCommit) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.windowSeq++
	commit.Seq = s.windowSeq
	if commit.CommittedAt.IsZero() {
		commit.CommittedAt = time.Now().UTC()
	}
	s.Windows = append(s.Windows, commit)
	return commit.Seq, nil
}

func (s *MemoryStore) StageHistory(_ context.Context, projectID string) ([]StageCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []StageCommit{}
	for _, commit := range s.Stages {
		if projectID == "" || commit.ProjectID == projectID {
			out = append(out, commit)
		}
	}
	return out, nil
}

func (s *MemoryStore) WindowHistory(_ context.Context, projectID string) ([]WindowCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []WindowCommit{}
	for _, commit := range s.Windows {
		if projectID == "" || commit.ProjectID == projectID {
			out = append(out, commit)
		}
	}
	return out, nil
}

func (s *MemoryStore) Close() error { return nil }
