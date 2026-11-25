package dialoguecal

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// FrameGate is the bounded number of frames that may be computed/buffered
// across all worker threads. Capacity is workers * framesPerWorker.
type FrameGate struct {
	budget   Budget
	inFlight int
	mu       sync.Mutex
	Events   []BackpressureEvent
}

func NewFrameGate(budget Budget) (*FrameGate, error) {
	if err := budget.Validate(); err != nil {
		return nil, err
	}
	return &FrameGate{budget: budget}, nil
}

func (g *FrameGate) Capacity() int { return g.budget.Capacity() }

// TryAcquire returns E_BACKPRESSURE when the bounded buffer is exhausted. It
// never grows the buffer or drops a frame silently.
func (g *FrameGate) TryAcquire(segmentID string, frameSeq int) (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.inFlight >= g.budget.Capacity() {
		event := BackpressureEvent{Code: CodeBackpressure, SegmentID: segmentID, FrameSeq: frameSeq, Capacity: g.budget.Capacity(), InFlight: g.inFlight}
		g.Events = append(g.Events, event)
		be := &BusinessError{Code: CodeBackpressure, SegmentID: segmentID, FrameIndex: frameSeq, Path: fmt.Sprintf("buffer.capacity[%d]", g.budget.Capacity()), Detail: "frame buffer budget exhausted", Context: map[string]any{"capacity": g.budget.Capacity(), "in_flight": g.inFlight, "workers": g.budget.Workers, "frames_per_worker": g.budget.FramesPerWorker}}
		return nil, be
	}
	g.inFlight++
	released := false
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if !released {
			released = true
			g.inFlight--
		}
	}, nil
}

// Acquire waits for capacity. If it had to wait, it records a deterministic
// E_BACKPRESSURE observation in the gate event log.
func (g *FrameGate) Acquire(ctx context.Context, segmentID string, frameSeq int) (func(), error) {
	waited := false
	for {
		release, err := g.TryAcquire(segmentID, frameSeq)
		if err == nil {
			if waited {
				g.mu.Lock()
				g.Events = append(g.Events, BackpressureEvent{Code: CodeBackpressure, SegmentID: segmentID, FrameSeq: frameSeq, Capacity: g.budget.Capacity(), InFlight: g.inFlight})
				g.mu.Unlock()
			}
			return release, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Millisecond):
			waited = true
		}
	}
}

type segmentJob struct {
	segment Segment
	frames  []Frame
}

// ParallelComputeAll computes segments with a bounded worker budget. Segment
// scheduling and goroutine timing do not affect indexed results.
func ParallelComputeAll(ctx context.Context, p *Project, budget Budget) (RecomputeReport, error) {
	if err := budget.Validate(); err != nil {
		return RecomputeReport{}, err
	}
	frames, err := BuildFrames(p)
	if err != nil {
		return RecomputeReport{}, err
	}
	jobs := map[string]*segmentJob{}
	order := []string{}
	for _, frame := range frames {
		job := jobs[frame.SegmentID]
		if job == nil {
			seg := segmentByID(p, frame.SegmentID)
			if seg == nil {
				return RecomputeReport{}, &BusinessError{Code: CodeFrameBounds, SegmentID: frame.SegmentID, FrameIndex: frame.GlobalIndex, Detail: "segment not found"}
			}
			job = &segmentJob{segment: *seg}
			jobs[frame.SegmentID] = job
			order = append(order, frame.SegmentID)
		}
		job.frames = append(job.frames, frame)
	}
	sort.SliceStable(order, func(i, j int) bool { return jobs[order[i]].segment.Index < jobs[order[j]].segment.Index })

	results := make([]FrameResult, len(frames))
	recomputed := make([]int, 0, len(frames))
	timings := map[string]time.Duration{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, budget.Workers)
	errOnce := make(chan error, 1)
	gate, _ := NewFrameGate(budget)

	for _, id := range order {
		job := jobs[id]
		select {
		case <-ctx.Done():
			return RecomputeReport{}, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(job *segmentJob) {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			var carry *calibrationCarry
			for _, frame := range job.frames {
				release, acquireErr := gate.Acquire(ctx, job.segment.ID, frame.GlobalIndex)
				if acquireErr != nil {
					select {
					case errOnce <- acquireErr:
					default:
					}
					return
				}
				provenance := Provenance{Revision: 1, CacheState: "parallel_computed", Sources: []string{"parallel_full_recompute"}}
				result, next, calcErr := computeFrameResult(p, frame, 1, provenance, carry)
				release()
				if calcErr != nil {
					select {
					case errOnce <- calcErr:
					default:
					}
					return
				}
				results[frame.GlobalIndex] = result
				mu.Lock()
				recomputed = append(recomputed, frame.GlobalIndex)
				mu.Unlock()
				carry = &next
			}
			mu.Lock()
			timings[job.segment.ID] = time.Since(start)
			mu.Unlock()
		}(job)
	}
	wg.Wait()
	select {
	case err := <-errOnce:
		return RecomputeReport{SegmentTimings: timings}, err
	default:
	}

	sort.Ints(recomputed)
	return RecomputeReport{Revision: 1, Results: results, RecomputedSeqs: recomputed, SegmentTimings: timings, Backpressure: gate.Events}, nil
}

// FillGateForTest occupies every buffer slot and returns the next acquisition
// error. It is also used by the demo's backpressure injection.
func FillGateForTest(budget Budget) (*FrameGate, error) {
	gate, err := NewFrameGate(budget)
	if err != nil {
		return nil, err
	}
	for i := 0; i < budget.Capacity(); i++ {
		if _, err := gate.TryAcquire("seg-fill", i); err != nil {
			return nil, err
		}
	}
	_, err = gate.TryAcquire("seg-overflow", budget.Capacity())
	return gate, err
}
