package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
)

type SnapshotStore struct {
	mu    sync.Mutex
	dir   string
	path  string
	items []model.ParamSnapshot
}

type snapshotFile struct {
	Format    string                `json:"format"`
	Latest    int64                 `json:"latest"`
	Snapshots []model.ParamSnapshot `json:"snapshots"`
}

func NewSnapshotStore(dir string) *SnapshotStore {
	return &SnapshotStore{dir: dir, path: filepath.Join(dir, "snapshots.json")}
}

func (s *SnapshotStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot create parameter directory", err)
	}
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		now := time.Now().UTC()
		p := model.DefaultParams()
		s.items = []model.ParamSnapshot{{Version: 1, Params: p, Hash: p.Hash(), CreatedAt: now}}
		return s.saveLocked()
	}
	if err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot read parameter snapshots", err)
	}
	var f snapshotFile
	if err := json.Unmarshal(b, &f); err != nil {
		return errs.Wrap(errs.ECheckpoint, "parameter snapshots are corrupt", err)
	}
	if len(f.Snapshots) == 0 {
		return errs.New(errs.ECheckpoint, "parameter snapshots are empty")
	}
	s.items = f.Snapshots
	sort.Slice(s.items, func(i, j int) bool { return s.items[i].Version < s.items[j].Version })
	for i, item := range s.items {
		if err := model.ValidateParams(item.Params); err != nil {
			return errs.Wrap(errs.ECheckpoint, fmt.Sprintf("snapshot version %d is invalid", item.Version), err)
		}
		if i > 0 && item.Version <= s.items[i-1].Version {
			return errs.New(errs.ECheckpoint, "parameter snapshot versions are not strictly increasing")
		}
	}
	return nil
}

func (s *SnapshotStore) Latest() model.ParamSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.items[len(s.items)-1]
}

func (s *SnapshotStore) Get(version int64) (model.ParamSnapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.items {
		if item.Version == version {
			return item, true
		}
	}
	return model.ParamSnapshot{}, false
}

func (s *SnapshotStore) All() []model.ParamSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.ParamSnapshot(nil), s.items...)
}

func (s *SnapshotStore) Update(partial map[string]json.RawMessage) (model.ParamSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.items[len(s.items)-1]
	next, err := cur.Params.ApplyPartial(partial)
	if err != nil {
		return model.ParamSnapshot{}, errs.Field(errs.EPara, err.Error(), "")
	}
	if err := model.ValidateParams(next); err != nil {
		return model.ParamSnapshot{}, err
	}
	if next.Hash() == cur.Hash {
		return cur, nil
	}
	item := model.ParamSnapshot{
		Version: cur.Version + 1, Params: next, Hash: next.Hash(),
		CreatedAt: time.Now().UTC(),
	}
	s.items = append(s.items, item)
	if err := s.saveLocked(); err != nil {
		s.items = s.items[:len(s.items)-1]
		return model.ParamSnapshot{}, err
	}
	return item, nil
}

func (s *SnapshotStore) saveLocked() error {
	f := snapshotFile{Format: "stitchworkbench.params/v1", Latest: s.items[len(s.items)-1].Version, Snapshots: s.items}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot encode parameter snapshots", err)
	}
	return atomicWrite(s.path, append(b, '\n'))
}

type Store struct {
	mu   sync.Mutex
	dir  string
	path string
}

func NewStore(dir string) *Store {
	return &Store{dir: dir, path: filepath.Join(dir, "checkpoint.json")}
}

func (s *Store) Load() (*model.CheckpointSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.ECheckpoint, "cannot read checkpoint", err)
	}
	var snap model.CheckpointSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, errs.Wrap(errs.ECheckpoint, "checkpoint is corrupt", err)
	}
	return &snap, nil
}

func (s *Store) Save(snap model.CheckpointSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(snap.Records, func(i, j int) bool {
		if snap.Records[i].PairIndex == snap.Records[j].PairIndex {
			return snap.Records[i].PairID < snap.Records[j].PairID
		}
		return snap.Records[i].PairIndex < snap.Records[j].PairIndex
	})
	snap.Header.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot encode checkpoint", err)
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot create checkpoint directory", err)
	}
	return atomicWrite(s.path, append(b, '\n'))
}

func (s *Store) Invalidate(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path); os.IsNotExist(err) {
		return nil
	}
	target := filepath.Join(s.dir, fmt.Sprintf("checkpoint.invalid-%d.json", time.Now().UnixNano()))
	if err := os.Rename(s.path, target); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot invalidate checkpoint", err)
	}
	_ = os.WriteFile(target+".reason", []byte(reason+"\n"), 0o644)
	return syncDir(s.dir)
}

func (s *Store) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot create persistence directory", err)
	}
	f, err := os.CreateTemp(dir, ".stitch-tmp-*")
	if err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot create temporary snapshot", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return errs.Wrap(errs.ECheckpoint, "cannot write temporary snapshot", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errs.Wrap(errs.ECheckpoint, "cannot fsync temporary snapshot", err)
	}
	if err := f.Close(); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot close temporary snapshot", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot atomically replace snapshot", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot open persistence directory", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return errs.Wrap(errs.ECheckpoint, "cannot fsync persistence directory", err)
	}
	return nil
}
