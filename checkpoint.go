package vpf

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

type CheckpointRecord struct {
	FrameIndex           int    `json:"frame_index"`
	ChunkIndex           int    `json:"chunk_index"`
	ChunkID              string `json:"chunk_id"`
	Bounds               Rect   `json:"bounds"`
	Pix                  []byte `json:"pix"`
	PixelDigest          string `json:"pixel_digest"`
	StackSnapshotVersion int    `json:"stack_snapshot_version"`
	RippleSourceVersion  int    `json:"ripple_source_version"`
	CacheKey             string `json:"cache_key"`
	ContentHash          string `json:"content_hash"`
	ParamVersion         int    `json:"param_version"`
	AlgorithmVersion     string `json:"algorithm_version"`
	PixelCount           int    `json:"pixel_count"`
	CommitSeq            int64  `json:"commit_seq"`
}

func ChunkID(frameIndex, chunkIndex int) string {
	return fmt.Sprintf("f%03d-c%03d", frameIndex, chunkIndex)
}

type CheckpointStore interface {
	Init(ctx context.Context) error
	PutCheckpoint(ctx context.Context, record CheckpointRecord) (int64, error)
	GetCheckpoint(ctx context.Context, frameIndex, chunkIndex int) (CheckpointRecord, bool, error)
	ListCheckpoints(ctx context.Context, frameIndex int) ([]CheckpointRecord, error)
	Close() error
}

type MemoryStore struct {
	mu      sync.Mutex
	records map[string]CheckpointRecord
	seq     int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{records: map[string]CheckpointRecord{}}
}

func (s *MemoryStore) Init(context.Context) error { return nil }

func checkpointKey(frame, chunk int) string {
	return fmt.Sprintf("%d/%d", frame, chunk)
}

func (s *MemoryStore) PutCheckpoint(_ context.Context, record CheckpointRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	record.CommitSeq = s.seq
	if record.ChunkID == "" {
		record.ChunkID = ChunkID(record.FrameIndex, record.ChunkIndex)
	}
	s.records[checkpointKey(record.FrameIndex, record.ChunkIndex)] = record
	return record.CommitSeq, nil
}

func (s *MemoryStore) GetCheckpoint(_ context.Context, frame, chunk int) (CheckpointRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[checkpointKey(frame, chunk)]
	return record, ok, nil
}

func (s *MemoryStore) ListCheckpoints(_ context.Context, frame int) ([]CheckpointRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []CheckpointRecord
	for _, record := range s.records {
		if record.FrameIndex == frame {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ChunkIndex < out[j].ChunkIndex })
	return out, nil
}

func (s *MemoryStore) Close() error { return nil }
