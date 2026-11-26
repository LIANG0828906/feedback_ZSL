package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"warmcache/internal/domain"
)

type Service struct {
	mu         sync.Mutex
	project    domain.Project
	requests   []domain.Request
	baseKey    string
	baseResult domain.RunResult
	snapshots  map[int64]*simSnapshot
}

func NewService(project domain.Project, requests []domain.Request) *Service {
	return &Service{project: project.Clone(), requests: append([]domain.Request(nil), requests...)}
}

func Run(project domain.Project, requests []domain.Request, opts domain.Options) (domain.RunResult, error) {
	sim, err := newSimulator(project, requests, opts)
	if err != nil {
		return domain.RunResult{}, err
	}
	targetTicks := sim.opts.Ticks
	if err := sim.run(targetTicks); err != nil {
		return domain.RunResult{}, err
	}
	return sim.result(opts.Edits, false), nil
}

func (s *Service) Project() domain.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.project.Clone()
}

func (s *Service) Requests() []domain.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Request(nil), s.requests...)
}

func (s *Service) Run(opts domain.Options) (domain.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(opts.Edits) == 0 {
		result, _, err := s.ensureBase(opts)
		return result, err
	}
	if opts.FullRecompute {
		return s.full(opts)
	}
	return s.incremental(opts)
}

func (s *Service) RunVerified(opts domain.Options) (domain.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(opts.Edits) == 0 {
		result, _, err := s.ensureBase(opts)
		if err != nil {
			return domain.RunResult{}, err
		}
		result.MatchesFull = true
		result.FullComparison = ComparisonKey(result)
		return result, nil
	}
	incremental, err := s.incremental(opts)
	if err != nil {
		return domain.RunResult{}, err
	}
	full, err := s.full(opts)
	if err != nil {
		return domain.RunResult{}, err
	}
	if err := CompareResults(incremental, full); err != nil {
		return incremental, err
	}
	incremental.MatchesFull = true
	incremental.FullComparison = ComparisonKey(full)
	return incremental, nil
}

func (s *Service) CompareFull(opts domain.Options) (domain.RunResult, domain.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	incremental, err := s.incremental(opts)
	if err != nil {
		return domain.RunResult{}, domain.RunResult{}, err
	}
	full, err := s.full(opts)
	if err != nil {
		return incremental, domain.RunResult{}, err
	}
	if err := CompareResults(incremental, full); err != nil {
		return incremental, full, err
	}
	incremental.MatchesFull = true
	incremental.FullComparison = ComparisonKey(full)
	full.MatchesFull = true
	full.FullComparison = ComparisonKey(incremental)
	return incremental, full, nil
}

func (s *Service) full(opts domain.Options) (domain.RunResult, error) {
	sim, err := newSimulator(s.project, s.requests, opts)
	if err != nil {
		return domain.RunResult{}, err
	}
	if err := sim.run(sim.opts.Ticks); err != nil {
		return domain.RunResult{}, err
	}
	return sim.result(opts.Edits, false), nil
}

func (s *Service) incremental(opts domain.Options) (domain.RunResult, error) {
	if len(opts.Edits) == 0 {
		result, _, err := s.ensureBase(opts)
		return result, err
	}
	minTick := opts.Edits[0].AtTick
	for _, edit := range opts.Edits[1:] {
		if edit.AtTick < minTick {
			minTick = edit.AtTick
		}
	}
	baseOpts := opts
	baseOpts.Edits = nil
	baseOpts.FullRecompute = false
	_, snapshots, err := s.ensureBaseLocked(baseOpts)
	if err != nil {
		return domain.RunResult{}, err
	}
	if minTick < 1 {
		minTick = 1
	}
	snapshot := snapshots[minTick-1]
	if snapshot == nil {
		return s.full(opts)
	}
	sim, err := simulatorFromSnapshot(s.project, s.requests, opts, snapshot)
	if err != nil {
		return domain.RunResult{}, err
	}
	sim.recomputed = nil
	sim.invalidations = nil
	sim.reusedTicks = len(sim.timeline)
	targetTicks := sim.opts.Ticks
	if err := sim.runFrom(int64(sim.reusedTicks)+1, targetTicks); err != nil {
		return domain.RunResult{}, err
	}
	result := sim.result(opts.Edits, true)
	result.ReusedTicks = len(snapshot.Timeline)
	result.Diagnostics.ReusedTicks = result.ReusedTicks
	if len(result.Timeline) > 0 && result.ReusedTicks > 0 {
		reuseEnd := result.ReusedTicks
		if reuseEnd > len(result.Timeline) {
			reuseEnd = len(result.Timeline)
		}
		for index := 0; index < reuseEnd; index++ {
			result.Timeline[index].CacheHit = true
		}
	}
	return result, nil
}

func (s *Service) ensureBase(opts domain.Options) (domain.RunResult, map[int64]*simSnapshot, error) {
	return s.ensureBaseLocked(opts)
}

func (s *Service) ensureBaseLocked(opts domain.Options) (domain.RunResult, map[int64]*simSnapshot, error) {
	key := baseOptionsKey(opts)
	if s.baseKey == key && s.baseResult.RunID != "" {
		return s.baseResult, s.snapshots, nil
	}
	baseOpts := opts
	baseOpts.Edits = nil
	baseOpts.FullRecompute = false
	sim, err := newSimulator(s.project, s.requests, baseOpts)
	if err != nil {
		return domain.RunResult{}, nil, err
	}
	snapshots := map[int64]*simSnapshot{0: sim.snapshot()}
	targetTicks := sim.opts.Ticks
	for tick := int64(1); tick <= targetTicks; tick++ {
		state, err := sim.step(tick)
		if err != nil {
			return domain.RunResult{}, nil, err
		}
		sim.timeline = append(sim.timeline, state)
		sim.recomputed = append(sim.recomputed, tick)
		snapshots[tick] = sim.snapshot()
		if sim.stoppedAt == tick {
			break
		}
	}
	result := sim.result(nil, false)
	s.baseKey = key
	s.baseResult = result
	s.snapshots = snapshots
	return result, snapshots, nil
}

func baseOptionsKey(opts domain.Options) string {
	copyOpts := opts
	copyOpts.Edits = nil
	copyOpts.FullRecompute = false
	copyOpts.Interrupts = append([]domain.Interrupt(nil), opts.Interrupts...)
	sort.Slice(copyOpts.Interrupts, func(i, j int) bool { return copyOpts.Interrupts[i].ID < copyOpts.Interrupts[j].ID })
	value, _ := json.Marshal(copyOpts)
	return fmt.Sprintf("%x", value)
}
