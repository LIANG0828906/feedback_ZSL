package tactical

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type WindowSpec struct {
	Index     int
	MemberIDs []string
	StartTick int
	EndTick   int
	Label     string
}

type SchedulerStats struct {
	MaxParallel    int   `json:"max_parallel"`
	PeakParallel   int   `json:"peak_parallel"`
	BufferLimit    int   `json:"buffer_limit"`
	WindowCount    int   `json:"window_count"`
	CommitCount    int   `json:"commit_count"`
	BlockedPushes  int64 `json:"blocked_pushes"`
	SerializedTail int64 `json:"serialized_commit_ns"`
}

type WindowScheduler struct {
	maxParallel int
	bufferLimit int
	stats       SchedulerStats
	mu          sync.Mutex
}

func NewWindowScheduler(maxParallel, bufferLimit int) (*WindowScheduler, error) {
	if maxParallel <= 0 {
		return nil, fmt.Errorf("max parallel must be positive")
	}
	if bufferLimit <= 0 {
		return nil, fmt.Errorf("buffer limit must be positive")
	}
	return &WindowScheduler{
		maxParallel: maxParallel,
		bufferLimit: bufferLimit,
		stats: SchedulerStats{
			MaxParallel: maxParallel,
			BufferLimit: bufferLimit,
		},
	}, nil
}

// StateBuffer is a hard-bounded per-worker queue. Push blocks when all slots
// are occupied; it never drops a state.
type StateBuffer struct {
	limit   int
	slots   chan struct{}
	mu      sync.Mutex
	states  []TickState
	blocked atomic.Int64
}

func NewStateBuffer(limit int) (*StateBuffer, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("state buffer limit must be positive")
	}
	return &StateBuffer{limit: limit, slots: make(chan struct{}, limit)}, nil
}

func (b *StateBuffer) Push(ctx context.Context, state TickState) error {
	if len(b.slots) == cap(b.slots) {
		b.blocked.Add(1)
	}
	select {
	case b.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	b.mu.Lock()
	b.states = append(b.states, state)
	b.mu.Unlock()
	return nil
}

func (b *StateBuffer) Pop() (TickState, bool) {
	b.mu.Lock()
	if len(b.states) == 0 {
		b.mu.Unlock()
		return TickState{}, false
	}
	state := b.states[0]
	copy(b.states, b.states[1:])
	b.states = b.states[:len(b.states)-1]
	b.mu.Unlock()
	<-b.slots
	return state, true
}

func (b *StateBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.states)
}

func (b *StateBuffer) Limit() int { return b.limit }
func (b *StateBuffer) BlockedPushes() int64 {
	return b.blocked.Load()
}

type windowResult[T any] struct {
	position int
	value    T
	err      error
}

// RunWindows computes windows in a fixed-size worker pool and commits results
// strictly in input order. The commit callback is the only place where global
// decisions and checkpoint writes should occur.
func RunWindows[T any](
	ctx context.Context,
	scheduler *WindowScheduler,
	windows []WindowSpec,
	compute func(context.Context, WindowSpec, *StateBuffer) (T, error),
	commit func(context.Context, WindowSpec, T) error,
) ([]T, SchedulerStats, error) {
	outputs := make([]T, len(windows))
	if len(windows) == 0 {
		return outputs, scheduler.stats, nil
	}
	seen := make(map[int]struct{}, len(windows))
	for i, window := range windows {
		if window.Index != i {
			return outputs, scheduler.stats, fmt.Errorf("window index %d is not in stable input order %d", window.Index, i)
		}
		if _, exists := seen[window.Index]; exists {
			return outputs, scheduler.stats, fmt.Errorf("duplicate window index %d", window.Index)
		}
		seen[window.Index] = struct{}{}
		if window.EndTick < window.StartTick {
			return outputs, scheduler.stats, fmt.Errorf("window %d has an inverted tick interval", window.Index)
		}
	}
	workers := scheduler.maxParallel
	if workers > len(windows) {
		workers = len(windows)
	}
	jobs := make(chan WindowSpec, len(windows))
	for _, window := range windows {
		jobs <- window
	}
	close(jobs)
	results := make(chan windowResult[T], maxInt(workers*scheduler.bufferLimit, workers))
	var active atomic.Int64
	var peak atomic.Int64
	var blockedPushes atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buffer, _ := NewStateBuffer(scheduler.bufferLimit)
			defer func() { blockedPushes.Add(buffer.BlockedPushes()) }()
			for {
				select {
				case <-ctx.Done():
					return
				case window, ok := <-jobs:
					if !ok {
						return
					}
					current := active.Add(1)
					for {
						previous := peak.Load()
						if current <= previous || peak.CompareAndSwap(previous, current) {
							break
						}
					}
					value, err := compute(ctx, window, buffer)
					active.Add(-1)
					select {
					case results <- windowResult[T]{position: window.Index, value: value, err: err}:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	pending := make(map[int]windowResult[T], len(windows))
	nextCommit := 0
	committed := 0
	var firstErr error
	var serializedCommitNanos int64
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		if result.err == nil {
			pending[result.position] = result
		}
		for {
			item, ok := pending[nextCommit]
			if !ok {
				break
			}
			if firstErr == nil {
				commitStart := time.Now()
				if err := commit(ctx, windows[nextCommit], item.value); err != nil {
					firstErr = err
				} else {
					outputs[nextCommit] = item.value
					committed++
				}
				serializedCommitNanos += time.Since(commitStart).Nanoseconds()
			}
			delete(pending, nextCommit)
			nextCommit++
		}
	}
	if err := ctx.Err(); err != nil && firstErr == nil {
		firstErr = err
	}
	scheduler.mu.Lock()
	scheduler.stats.PeakParallel = int(peak.Load())
	scheduler.stats.WindowCount = len(windows)
	scheduler.stats.CommitCount = committed
	scheduler.stats.BlockedPushes = blockedPushes.Load()
	scheduler.stats.SerializedTail = serializedCommitNanos
	stats := scheduler.stats
	scheduler.mu.Unlock()
	return outputs, stats, firstErr
}

func (s *WindowScheduler) Stats() SchedulerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// StableCommitOrder verifies the committed windows are exactly the input order.
func StableCommitOrder(windows []WindowSpec, committed []int) error {
	if len(windows) != len(committed) {
		return fmt.Errorf("commit count mismatch: windows=%d commits=%d", len(windows), len(committed))
	}
	for i := range windows {
		if windows[i].Index != committed[i] {
			return fmt.Errorf("commit order diverged at position %d: want %d got %d", i, windows[i].Index, committed[i])
		}
	}
	return nil
}

// SortWindowsByIndex is a deterministic helper for callers constructing
// windows from map-like inputs.
func SortWindowsByIndex(windows []WindowSpec) {
	sort.SliceStable(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
