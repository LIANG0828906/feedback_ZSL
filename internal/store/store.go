package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

type Checkpoint struct {
	CommitSeq         int64  `json:"commitSeq"`
	TileID            string `json:"tileId"`
	Generation        int64  `json:"generation"`
	Attempt           int    `json:"attempt"`
	ParamVersion      int    `json:"paramVersion"`
	AlgorithmVersion  string `json:"algorithmVersion"`
	VertexDigest      string `json:"vertexDigest"`
	TriangleDigest    string `json:"triangleDigest"`
	BoundaryStatsJSON string `json:"boundaryStatsJson"`
	DependenciesJSON  string `json:"dependenciesJson"`
	ContentHash       string `json:"contentHash"`
	ContentJSON       []byte `json:"contentJson"`
	VertexCount       int    `json:"vertexCount"`
	TriangleCount     int    `json:"triangleCount"`
	CommittedOrder    int    `json:"committedOrder"`
	CommittedAt       int64  `json:"committedAt"`
}

type CommitRecord struct {
	Seq         int64  `json:"seq"`
	TileID      string `json:"tileId"`
	ContentHash string `json:"contentHash"`
	Reason      string `json:"reason"`
	CommittedAt int64  `json:"committedAt"`
}

type Store interface {
	Save(ctx context.Context, cp Checkpoint, reason string) (CommitRecord, error)
	Latest(ctx context.Context) (map[string]Checkpoint, error)
	History(ctx context.Context) ([]CommitRecord, error)
	Reset(ctx context.Context, reason string) error
	GetMeta(ctx context.Context, key string) (string, error)
	SetMeta(ctx context.Context, key, value string) error
	Close() error
}

type MemoryStore struct {
	mu      sync.Mutex
	seq     int64
	checks  []Checkpoint
	commits []CommitRecord
	meta    map[string]string
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{meta: map[string]string{}}
}

func (s *MemoryStore) Save(_ context.Context, cp Checkpoint, reason string) (CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	cp.CommitSeq = s.seq
	if cp.CommittedAt == 0 {
		cp.CommittedAt = time.Now().UnixNano()
	}
	s.checks = append(s.checks, cp)
	rec := CommitRecord{Seq: s.seq, TileID: cp.TileID, ContentHash: cp.ContentHash, Reason: reason, CommittedAt: cp.CommittedAt}
	s.commits = append(s.commits, rec)
	return rec, nil
}

func (s *MemoryStore) Latest(_ context.Context) (map[string]Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Checkpoint{}
	for _, cp := range s.checks {
		out[cp.TileID] = cp
	}
	return out, nil
}

func (s *MemoryStore) History(_ context.Context) ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]CommitRecord{}, s.commits...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (s *MemoryStore) Reset(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = nil
	s.commits = nil
	s.seq = 0
	return nil
}

func (s *MemoryStore) GetMeta(_ context.Context, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.meta[key], nil
}

func (s *MemoryStore) SetMeta(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.meta[key] = value
	return nil
}

func (s *MemoryStore) Close() error { return nil }
