package version

import (
	"fmt"
	"sync"

	"maskpipeline/internal/apperr"
	"maskpipeline/internal/model"
)

type Snapshot struct {
	Version          uint64       `json:"version"`
	Params           model.Params `json:"params"`
	ParamsHash       string       `json:"params_hash"`
	AlgorithmVersion string       `json:"algorithm_version"`
}

type Store struct {
	mu        sync.RWMutex
	width     int
	height    int
	algorithm string
	next      uint64
	latest    uint64
	byVersion map[uint64]Snapshot
}

func New(width, height int, algorithm string, initial model.Params) (*Store, Snapshot, error) {
	if width <= 0 || height <= 0 {
		return nil, Snapshot{}, apperr.New(apperr.CodeMeta, "new_versions", "invalid image dimensions")
	}
	if algorithm == "" {
		algorithm = model.AlgorithmVersion
	}
	normalized, err := model.ValidateParams(initial, width, height)
	if err != nil {
		return nil, Snapshot{}, err
	}
	s := &Store{
		width:     width,
		height:    height,
		algorithm: algorithm,
		next:      1,
		latest:    1,
		byVersion: make(map[uint64]Snapshot),
	}
	snap := Snapshot{
		Version:          1,
		Params:           normalized,
		ParamsHash:       model.HashJSON(normalized),
		AlgorithmVersion: algorithm,
	}
	s.byVersion[1] = snap
	return s, snap, nil
}

func (s *Store) Update(params model.Params) (Snapshot, error) {
	normalized, err := model.ValidateParams(params, s.width, s.height)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next++
	snap := Snapshot{
		Version:          s.next,
		Params:           normalized.Clone(),
		ParamsHash:       model.HashJSON(normalized),
		AlgorithmVersion: s.algorithm,
	}
	s.byVersion[snap.Version] = snap
	s.latest = snap.Version
	return cloneSnapshot(snap), nil
}

func (s *Store) Latest() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.byVersion[s.latest])
}

func (s *Store) Get(version uint64) (Snapshot, error) {
	s.mu.RLock()
	snap, ok := s.byVersion[version]
	latest := s.latest
	s.mu.RUnlock()
	if !ok {
		return Snapshot{}, apperr.New(apperr.CodeVersion, "lock_snapshot", "requested parameter version does not exist").
			WithDetail("left_version", latest).
			WithDetail("right_version", version)
	}
	return cloneSnapshot(snap), nil
}

func (s *Store) LockLatest() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneSnapshot(s.byVersion[s.latest])
}

func (s *Store) AlgorithmVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.algorithm
}

func (s *Store) Width() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.width
}

func (s *Store) Height() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.height
}

func cloneSnapshot(s Snapshot) Snapshot {
	s.Params = s.Params.Clone()
	return s
}

func MixedVersionError(left, right uint64) error {
	return apperr.New(apperr.CodeVersion, "version_boundary", "parameter versions were mixed across a boundary").
		WithDetail("left_version", left).
		WithDetail("right_version", right)
}

func (s Snapshot) String() string {
	return fmt.Sprintf("v%d(%s)", s.Version, s.ParamsHash)
}
