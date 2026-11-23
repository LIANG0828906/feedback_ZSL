package ortho

import (
	"encoding/json"
	"image"
	"os"
	"path/filepath"
	"sync"
)

// StageRecord 为快照中单个阶段的持久化记录。
type StageRecord struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	File        string `json:"file"`
	ContentHash string `json:"content_hash"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Channels    int    `json:"channels"`
}

// Snapshot 为各阶段输出、缓存键、参数版本与输入内容哈希的整体快照。
type Snapshot struct {
	Version     string        `json:"version"`
	SourceHash  string        `json:"source_hash"`
	CommitOrder []string      `json:"commit_order"`
	Stages      []StageRecord `json:"stages"`
}

// Store 负责快照的原子读写。写入串行化，不得交错。
type Store struct {
	dir string
	mu  sync.Mutex
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

const snapshotFile = "snapshot.json"

// Save 将快照整体原子写入：先写各阶段图像（临时文件+重命名），
// 最后原子替换 snapshot.json 作为提交点。调用按互斥锁串行化。
func (s *Store) Save(snap *Snapshot, images map[string]image.Image) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	for _, rec := range snap.Stages {
		img, ok := images[rec.ID]
		if !ok {
			continue
		}
		b, err := EncodePNG(img)
		if err != nil {
			return err
		}
		if err := atomicWrite(filepath.Join(s.dir, rec.File), b); err != nil {
			return err
		}
	}
	sb, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, snapshotFile), sb)
}

// Load 加载快照并校验：参数版本、输入内容哈希、各阶段文件内容哈希
// 任一不匹配即整体作废快照（删除目录内容）并返回空，禁止部分混用。
func (s *Store) Load(version, srcHash string) (*Snapshot, map[string]image.Image, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(filepath.Join(s.dir, snapshotFile))
	if err != nil {
		return nil, nil, nil // 无快照
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		s.discardLocked()
		return nil, nil, nil
	}
	if snap.Version != version || snap.SourceHash != srcHash {
		s.discardLocked()
		return nil, nil, nil
	}
	images := make(map[string]image.Image)
	for _, rec := range snap.Stages {
		img, derr := LoadImageFile(filepath.Join(s.dir, rec.File))
		if derr != nil || ContentHash(img) != rec.ContentHash {
			s.discardLocked()
			return nil, nil, nil
		}
		images[rec.ID] = img
	}
	return &snap, images, nil
}

// discardLocked 整体作废快照目录内容。
func (s *Store) discardLocked() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(s.dir, e.Name()))
	}
}

// RestoreCache 将快照内容装载进缓存（整体装载，不部分混用）。
func RestoreCache(snap *Snapshot, images map[string]image.Image, cache *Cache) {
	for _, rec := range snap.Stages {
		img := images[rec.ID]
		_ = cache.Commit(rec.ID, &CacheEntry{Key: rec.Key, Img: img, ContentHash: rec.ContentHash})
	}
}

// BuildSnapshot 由缓存当前状态构造快照对象。
func BuildSnapshot(version, srcHash string, stages []Stage, cache *Cache) *Snapshot {
	snap := &Snapshot{Version: version, SourceHash: srcHash, CommitOrder: cache.CommitOrder()}
	for _, st := range stages {
		entry, ok := cache.Get(st.ID())
		if !ok {
			continue
		}
		b := entry.Img.Bounds()
		snap.Stages = append(snap.Stages, StageRecord{
			ID:          st.ID(),
			Key:         entry.Key,
			File:        st.ID() + ".png",
			ContentHash: entry.ContentHash,
			Width:       b.Dx(),
			Height:      b.Dy(),
			Channels:    Channels(entry.Img),
		})
	}
	return snap
}

// CollectImages 从缓存收集各阶段图像。
func CollectImages(stages []Stage, cache *Cache) map[string]image.Image {
	out := make(map[string]image.Image)
	for _, st := range stages {
		if entry, ok := cache.Get(st.ID()); ok {
			out[st.ID()] = entry.Img
		}
	}
	return out
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
