package sched

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

type Options struct {
	MaxParallel       int
	PerTaskPixelBytes int64
	PerTaskMeshBytes  int64
	TotalPixelBytes   int64
	TotalMeshBytes    int64
}

type Task struct {
	ID         int
	Label      string
	PixelBytes int64
	MeshBytes  int64
	Run        func(context.Context) (any, error)
}

type Result struct {
	TaskID int
	Value  any
	Err    error
}

type QueueRecord struct {
	TaskID     int           `json:"taskId"`
	Label      string        `json:"label"`
	Reason     string        `json:"reason"`
	Wait       time.Duration `json:"waitNs"`
	PixelBytes int64         `json:"pixelBytes"`
	MeshBytes  int64         `json:"meshBytes"`
	Started    bool          `json:"started"`
	Cancelled  bool          `json:"cancelled"`
}

type Metrics struct {
	Elapsed        time.Duration `json:"elapsedNs"`
	PeakParallel   int           `json:"peakParallel"`
	PeakPixelBytes int64         `json:"peakPixelBytes"`
	PeakMeshBytes  int64         `json:"peakMeshBytes"`
	Queue          []QueueRecord `json:"queue"`
	Completed      int           `json:"completed"`
	Cancelled      int           `json:"cancelled"`
}

type Scheduler struct {
	opts Options
}

func New(opts Options) (*Scheduler, error) {
	if opts.MaxParallel < 1 {
		return nil, fmt.Errorf("maxParallel must be >= 1")
	}
	if opts.PerTaskPixelBytes < 1 || opts.PerTaskMeshBytes < 1 || opts.TotalPixelBytes < 1 || opts.TotalMeshBytes < 1 {
		return nil, fmt.Errorf("buffer budgets must be positive")
	}
	if opts.PerTaskPixelBytes > opts.TotalPixelBytes || opts.PerTaskMeshBytes > opts.TotalMeshBytes {
		return nil, fmt.Errorf("per-task buffers cannot exceed total budgets")
	}
	return &Scheduler{opts: opts}, nil
}

type taskState struct {
	task       Task
	result     *Result
	queuedAt   time.Time
	startedAt  time.Time
	waitReason string
	started    bool
	cancelled  bool
}

func (s *Scheduler) Run(ctx context.Context, tasks []Task) ([]Result, Metrics, error) {
	start := time.Now()
	if len(tasks) == 0 {
		return nil, Metrics{Elapsed: time.Since(start)}, nil
	}
	states := make([]*taskState, len(tasks))
	for i, t := range tasks {
		states[i] = &taskState{task: t, queuedAt: start, waitReason: "ready"}
	}
	workers := s.opts.MaxParallel
	if workers > len(states) {
		workers = len(states)
	}
	for i := workers; i < len(states); i++ {
		states[i].waitReason = "parallel-limit"
	}

	var mu sync.Mutex
	cond := sync.NewCond(&mu)
	pending := append([]*taskState(nil), states...)
	records := make(map[int]QueueRecord, len(states))
	pixelUsed, meshUsed := int64(0), int64(0)
	running, peakParallel := 0, 0
	peakPixels, peakMesh := int64(0), int64(0)
	var firstTaskErr error
	active := workers
	runDone := make(chan struct{})
	defer close(runDone)
	go func() {
		select {
		case <-ctx.Done():
			mu.Lock()
			cond.Broadcast()
			mu.Unlock()
		case <-runDone:
		}
	}()

	for w := 0; w < workers; w++ {
		go func() {
			defer func() {
				mu.Lock()
				active--
				cond.Broadcast()
				mu.Unlock()
			}()
			for {
				mu.Lock()
				var ts *taskState
				for {
					if ctx.Err() != nil {
						for _, p := range pending {
							if p.result == nil {
								p.cancelled = true
								r := Result{TaskID: p.task.ID, Err: ctx.Err()}
								p.result = &r
								records[p.task.ID] = s.record(p, false)
							}
						}
						pending = nil
						cond.Broadcast()
						mu.Unlock()
						return
					}
					if len(pending) == 0 && running == 0 {
						cond.Broadcast()
						mu.Unlock()
						return
					}
					idx := -1
					reason := ""
					for i, p := range pending {
						if p.task.PixelBytes > s.opts.PerTaskPixelBytes {
							r := Result{TaskID: p.task.ID, Err: fmt.Errorf("task %d pixel buffer exceeds per-thread limit", p.task.ID)}
							p.result = &r
							records[p.task.ID] = s.record(p, false)
							pending = append(pending[:i], pending[i+1:]...)
							idx = -2
							break
						}
						if p.task.MeshBytes > s.opts.PerTaskMeshBytes {
							r := Result{TaskID: p.task.ID, Err: fmt.Errorf("task %d mesh buffer exceeds per-thread limit", p.task.ID)}
							p.result = &r
							records[p.task.ID] = s.record(p, false)
							pending = append(pending[:i], pending[i+1:]...)
							idx = -2
							break
						}
						if running >= s.opts.MaxParallel {
							reason = "parallel-limit"
							p.waitReason = reason
							break
						}
						if pixelUsed+p.task.PixelBytes > s.opts.TotalPixelBytes {
							reason = "pixel-budget"
							p.waitReason = reason
							break
						}
						if meshUsed+p.task.MeshBytes > s.opts.TotalMeshBytes {
							reason = "mesh-budget"
							p.waitReason = reason
							break
						}
						idx = i
						break
					}
					if idx == -2 {
						cond.Broadcast()
						continue
					}
					if idx >= 0 {
						ts = pending[idx]
						pending = append(pending[:idx], pending[idx+1:]...)
						if reason != "" {
							ts.waitReason = reason
						}
						pixelUsed += ts.task.PixelBytes
						meshUsed += ts.task.MeshBytes
						running++
						if running > peakParallel {
							peakParallel = running
						}
						if pixelUsed > peakPixels {
							peakPixels = pixelUsed
						}
						if meshUsed > peakMesh {
							peakMesh = meshUsed
						}
						ts.started = true
						ts.startedAt = time.Now()
						cond.Broadcast()
						break
					}
					if running == 0 && len(pending) > 0 {
						// No task can ever fit with the configured totals.
						for _, p := range pending {
							r := Result{TaskID: p.task.ID, Err: fmt.Errorf("task %d cannot fit aggregate buffer budget", p.task.ID)}
							p.result = &r
							records[p.task.ID] = s.record(p, false)
						}
						pending = nil
						cond.Broadcast()
						continue
					}
					if ts == nil && reason == "" {
						reason = "waiting"
					}
					cond.Wait()
				}
				mu.Unlock()

				value, err := runTask(ctx, ts.task)
				end := time.Now()

				mu.Lock()
				r := Result{TaskID: ts.task.ID, Value: value, Err: err}
				ts.result = &r
				if err != nil && firstTaskErr == nil {
					firstTaskErr = err
				}
				records[ts.task.ID] = s.recordAt(ts, end)
				running--
				pixelUsed -= ts.task.PixelBytes
				meshUsed -= ts.task.MeshBytes
				cond.Broadcast()
				mu.Unlock()
			}
		}()
	}

	mu.Lock()
	for active > 0 {
		cond.Wait()
	}
	mu.Unlock()

	out := make([]Result, 0, len(states))
	for i, st := range states {
		if st.result != nil {
			out = append(out, *st.result)
		} else {
			out = append(out, Result{TaskID: tasks[i].ID, Err: errors.New("scheduler lost task")})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	metrics := Metrics{
		Elapsed: time.Since(start), PeakParallel: peakParallel,
		PeakPixelBytes: peakPixels, PeakMeshBytes: peakMesh,
	}
	for _, st := range states {
		if st.result != nil && st.result.Err != nil {
			if errors.Is(st.result.Err, context.Canceled) || errors.Is(st.result.Err, context.DeadlineExceeded) {
				metrics.Cancelled++
			}
			continue
		}
		metrics.Completed++
	}
	for _, rec := range records {
		if rec.TaskID >= 0 {
			metrics.Queue = append(metrics.Queue, rec)
		}
	}
	sort.Slice(metrics.Queue, func(i, j int) bool { return metrics.Queue[i].TaskID < metrics.Queue[j].TaskID })
	if ctx.Err() != nil {
		return out, metrics, ctx.Err()
	}
	if firstTaskErr != nil {
		return out, metrics, firstTaskErr
	}
	return out, metrics, nil
}

func (s *Scheduler) record(ts *taskState, started bool) QueueRecord {
	now := time.Now()
	rec := QueueRecord{
		TaskID: ts.task.ID, Label: ts.task.Label, Reason: ts.waitReason,
		Wait: now.Sub(ts.queuedAt), PixelBytes: ts.task.PixelBytes,
		MeshBytes: ts.task.MeshBytes, Started: started, Cancelled: ts.cancelled,
	}
	return rec
}

func (s *Scheduler) recordAt(ts *taskState, end time.Time) QueueRecord {
	wait := end.Sub(ts.queuedAt)
	if !ts.startedAt.IsZero() {
		wait = ts.startedAt.Sub(ts.queuedAt)
	}
	rec := QueueRecord{
		TaskID: ts.task.ID, Label: ts.task.Label, Reason: ts.waitReason,
		Wait: wait, PixelBytes: ts.task.PixelBytes,
		MeshBytes: ts.task.MeshBytes, Started: ts.started, Cancelled: ts.cancelled,
	}
	return rec
}

func runTask(ctx context.Context, task Task) (value any, err error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("task %d panic: %v", task.ID, r)
		}
	}()
	if task.Run == nil {
		return nil, fmt.Errorf("task %d has no runner", task.ID)
	}
	return task.Run(ctx)
}
