package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type fileSnapshot struct {
	Chunks  []ChunkRecord  `json:"chunks"`
	Commits []CommitRecord `json:"commits"`
}

type fileStore struct {
	mu     sync.Mutex
	path   string
	snap   fileSnapshot
	closed bool
}

func openFileStore(path string) (Store, error) {
	s := &fileStore{path: path + ".fallback.json"}
	b, err := os.ReadFile(s.path)
	if err == nil {
		if err := json.Unmarshal(b, &s.snap); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return s, nil
}

func (s *fileStore) Path() string    { return s.path }
func (s *fileStore) Backend() string { return "atomic-file-fallback" }

func (s *fileStore) flushLocked() error {
	if s.closed {
		return errors.New("store closed")
	}
	sort.Slice(s.snap.Chunks, func(i, j int) bool {
		return s.snap.Chunks[i].CommitSeq < s.snap.Chunks[j].CommitSeq
	})
	sort.Slice(s.snap.Commits, func(i, j int) bool { return s.snap.Commits[i].Seq < s.snap.Commits[j].Seq })
	b, err := json.Marshal(s.snap)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *fileStore) PutChunk(rec ChunkRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i := range s.snap.Chunks {
		if s.snap.Chunks[i].ChunkX == rec.ChunkX && s.snap.Chunks[i].ChunkY == rec.ChunkY {
			s.snap.Chunks[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		s.snap.Chunks = append(s.snap.Chunks, rec)
	}
	found := false
	for i := range s.snap.Commits {
		if s.snap.Commits[i].Seq == rec.CommitSeq {
			s.snap.Commits[i] = CommitRecord{Seq: rec.CommitSeq, ChunkX: rec.ChunkX, ChunkY: rec.ChunkY,
				CacheKey: rec.CacheKey, CommittedUnixUS: rec.CreatedUnixUS}
			found = true
			break
		}
	}
	if !found {
		s.snap.Commits = append(s.snap.Commits, CommitRecord{Seq: rec.CommitSeq, ChunkX: rec.ChunkX,
			ChunkY: rec.ChunkY, CacheKey: rec.CacheKey, CommittedUnixUS: rec.CreatedUnixUS})
	}
	return s.flushLocked()
}

func (s *fileStore) GetChunk(x, y int) (ChunkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.snap.Chunks {
		if rec.ChunkX == x && rec.ChunkY == y {
			return rec, nil
		}
	}
	return ChunkRecord{}, ErrNotFound
}

func (s *fileStore) ListChunks() ([]ChunkRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]ChunkRecord(nil), s.snap.Chunks...)
	sort.Slice(out, func(i, j int) bool { return out[i].CommitSeq < out[j].CommitSeq })
	return out, nil
}

func (s *fileStore) DeleteChunk(x, y int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunks := s.snap.Chunks[:0]
	for _, rec := range s.snap.Chunks {
		if rec.ChunkX != x || rec.ChunkY != y {
			chunks = append(chunks, rec)
		}
	}
	s.snap.Chunks = chunks
	commits := s.snap.Commits[:0]
	for _, c := range s.snap.Commits {
		if c.ChunkX != x || c.ChunkY != y {
			commits = append(commits, c)
		}
	}
	s.snap.Commits = commits
	return s.flushLocked()
}

func (s *fileStore) TruncateFrom(seq int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	chunks := s.snap.Chunks[:0]
	for _, rec := range s.snap.Chunks {
		if rec.CommitSeq < seq {
			chunks = append(chunks, rec)
		}
	}
	s.snap.Chunks = chunks
	commits := s.snap.Commits[:0]
	for _, c := range s.snap.Commits {
		if c.Seq < seq {
			commits = append(commits, c)
		}
	}
	s.snap.Commits = commits
	return s.flushLocked()
}

func (s *fileStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = fileSnapshot{}
	return s.flushLocked()
}

func (s *fileStore) Commits() ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]CommitRecord(nil), s.snap.Commits...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (s *fileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
