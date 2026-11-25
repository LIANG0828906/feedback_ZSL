package iconcut

import (
	"context"
	"sync"
	"time"
)

type QueueWait struct {
	TileIndex int    `json:"tile_index"`
	TX        int    `json:"tx"`
	TY        int    `json:"ty"`
	Stage     string `json:"stage"`
	Reason    string `json:"reason"`
	WaitMS    int64  `json:"wait_ms"`
	WaitNS    int64  `json:"wait_ns"`
	Pixels    int    `json:"pixels"`
}

type Job[T any] struct {
	Tile   Tile
	Stage  string
	Pixels int
	Fn     func(context.Context) (T, error)
}

type JobResult[T any] struct {
	Tile  Tile
	Stage string
	Value T
	Err   error
	Wait  QueueWait
}

type Scheduler struct {
	Workers         int
	MaxQueuedTasks  int
	MaxThreadPixels int

	mu    sync.Mutex
	waits []QueueWait
}

func NewScheduler(b Budget) *Scheduler {
	return &Scheduler{Workers: maxInt(1, b.Workers), MaxQueuedTasks: maxInt(1, b.MaxQueuedTasks), MaxThreadPixels: b.MaxThreadPixels}
}

func (s *Scheduler) Waits() []QueueWait {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]QueueWait, len(s.waits))
	copy(out, s.waits)
	return out
}

func (s *Scheduler) recordWait(w QueueWait) {
	s.mu.Lock()
	s.waits = append(s.waits, w)
	s.mu.Unlock()
}

type queuedJob[T any] struct {
	job Job[T]
}

func RunJobs[T any](ctx context.Context, s *Scheduler, jobs []Job[T], ctl *Controller) []JobResult[T] {
	if s == nil {
		s = NewScheduler(Budget{Workers: 1, MaxQueuedTasks: len(jobs) + 1, MaxThreadPixels: 1 << 30})
	}
	queue := make(chan queuedJob[T], s.MaxQueuedTasks)
	results := make(chan JobResult[T], len(jobs))
	var wg sync.WaitGroup
	for i := 0; i < s.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for qj := range queue {
				j := qj.job
				if ctl != nil {
					if err := ctl.Gate(ctx); err != nil {
						results <- JobResult[T]{Tile: j.Tile, Stage: j.Stage, Err: err}
						continue
					}
				}
				v, err := j.Fn(ctx)
				results <- JobResult[T]{Tile: j.Tile, Stage: j.Stage, Value: v, Err: err}
			}
		}()
	}

	var submitWg sync.WaitGroup
	submitWg.Add(1)
	go func() {
		defer submitWg.Done()
		for _, j := range jobs {
			if j.Pixels > s.MaxThreadPixels {
				results <- JobResult[T]{Tile: j.Tile, Stage: j.Stage, Err: WithDetails(NewError(EPara, "declared.budget.max_thread_pixels", "task pixel buffer exceeds per-thread cap"), map[string]any{
					"pixels": j.Pixels, "limit": s.MaxThreadPixels, "tile_index": j.Tile.Index, "stage": j.Stage,
				})}
				continue
			}
			wait := QueueWait{TileIndex: j.Tile.Index, TX: j.Tile.TX, TY: j.Tile.TY, Stage: j.Stage, Pixels: j.Pixels}
			start := time.Now()
			if len(queue) == cap(queue) {
				wait.Reason = "queue_budget_exhausted"
			}
			select {
			case queue <- queuedJob[T]{job: j}:
				if wait.Reason != "" {
					wait.WaitMS = time.Since(start).Milliseconds()
					wait.WaitNS = time.Since(start).Nanoseconds()
					s.recordWait(wait)
				}
			case <-ctx.Done():
				results <- JobResult[T]{Tile: j.Tile, Stage: j.Stage, Err: ctx.Err()}
				return
			}
		}
	}()

	go func() {
		submitWg.Wait()
		close(queue)
		wg.Wait()
		close(results)
	}()

	out := make([]JobResult[T], 0, len(jobs))
	for r := range results {
		out = append(out, r)
	}
	return out
}

type Controller struct {
	mu     sync.Mutex
	paused bool
	waitCh chan struct{}
	ctx    context.Context
	cancel context.CancelFunc
}

func NewController() *Controller {
	ctx, cancel := context.WithCancel(context.Background())
	return &Controller{ctx: ctx, cancel: cancel, waitCh: make(chan struct{})}
}

func (c *Controller) Pause() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.paused {
		return
	}
	c.paused = true
	c.waitCh = make(chan struct{})
}

func (c *Controller) Resume() {
	c.mu.Lock()
	ch := c.waitCh
	c.paused = false
	c.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

func (c *Controller) Cancel() {
	c.cancel()
	c.Resume()
}

func (c *Controller) Gate(ctx context.Context) error {
	c.mu.Lock()
	if !c.paused {
		c.mu.Unlock()
	} else {
		ch := c.waitCh
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	default:
		return nil
	}
}
