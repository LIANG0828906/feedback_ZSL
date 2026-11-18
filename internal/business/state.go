package business

import (
	"errors"
	"sort"
	"sync"

	"configcenter/internal/model"
)

var ErrProtocol = errors.New("watch response violates the version contract")

type ApplyResult struct {
	Applied bool
	Stale   bool
	Keys    []string
}

type State struct {
	mu      sync.RWMutex
	version int64
	values  map[string]string
	history []int64
	stale   uint64
}

func NewState() *State {
	return &State{
		values: make(map[string]string),
	}
}

func (s *State) Apply(response model.WatchResponse) (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if response.Version < s.version {
		s.stale++
		return ApplyResult{Stale: true}, nil
	}
	if response.Version == s.version {
		return ApplyResult{}, nil
	}
	if response.BaseVersion != s.version {
		s.stale++
		return ApplyResult{Stale: true}, nil
	}
	if !response.Changed || len(response.Changes) == 0 {
		return ApplyResult{}, ErrProtocol
	}

	changes := append([]model.Item(nil), response.Changes...)
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Version == changes[j].Version {
			return changes[i].Key < changes[j].Key
		}
		return changes[i].Version < changes[j].Version
	})

	nextValues := make(map[string]string, len(s.values)+len(changes))
	for key, value := range s.values {
		nextValues[key] = value
	}
	appliedKeys := make([]string, 0, len(changes))
	for _, change := range changes {
		if change.Version <= s.version || change.Version > response.Version {
			return ApplyResult{}, ErrProtocol
		}
		if change.Key == "" {
			return ApplyResult{}, ErrProtocol
		}
		nextValues[change.Key] = change.Value
		appliedKeys = append(appliedKeys, change.Key)
	}

	s.values = nextValues
	s.version = response.Version
	s.history = append(s.history, response.Version)
	return ApplyResult{Applied: true, Keys: appliedKeys}, nil
}

func (s *State) Snapshot() (int64, map[string]string, []int64, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make(map[string]string, len(s.values))
	for key, value := range s.values {
		values[key] = value
	}
	history := append([]int64(nil), s.history...)
	return s.version, values, history, s.stale
}

func (s *State) Version() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

func (s *State) Values() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	values := make(map[string]string, len(s.values))
	for key, value := range s.values {
		values[key] = value
	}
	return values
}
