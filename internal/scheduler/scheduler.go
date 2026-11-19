package scheduler

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"feedback_zsl/internal/priority"
)

var ErrClosed = errors.New("scheduler closed")

type Config struct {
	MaxInflight int
	MaxWait     time.Duration
	AgingPeriod time.Duration
	OnDispatch  func(priority string, waited time.Duration)
}

type Lease struct {
	s    *Scheduler
	once sync.Once
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() { l.s.release() })
}

type request struct {
	class    priority.Class
	enqueued time.Time
	deadline time.Time
	seq      uint64
	ready    chan struct{}
	granted  bool
	canceled bool
	ctx      context.Context
}

type Scheduler struct {
	cfg Config

	mu       sync.Mutex
	queue    []*request
	inflight int
	seq      uint64
	closed   bool
	wake     chan struct{}
	done     chan struct{}
	now      func() time.Time
}

func New(cfg Config) *Scheduler {
	if cfg.MaxInflight < 1 {
		cfg.MaxInflight = 1
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = 150 * time.Millisecond
	}
	if cfg.AgingPeriod <= 0 {
		cfg.AgingPeriod = 40 * time.Millisecond
	}
	s := &Scheduler{
		cfg:  cfg,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
		now:  time.Now,
	}
	go s.run()
	return s
}

func (s *Scheduler) Acquire(ctx context.Context, class priority.Class) (*Lease, time.Duration, error) {
	start := s.now()
	req := &request{
		class:    class,
		enqueued: start,
		deadline: start.Add(s.cfg.MaxWait),
		ready:    make(chan struct{}),
		ctx:      ctx,
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, 0, ErrClosed
	}
	s.seq++
	req.seq = s.seq
	s.queue = append(s.queue, req)
	s.mu.Unlock()
	s.signal()

	select {
	case <-req.ready:
		waited := s.now().Sub(start)
		if ctx.Err() != nil {
			s.release()
			return nil, waited, ctx.Err()
		}
		if s.cfg.OnDispatch != nil {
			s.cfg.OnDispatch(class.Name, waited)
		}
		return &Lease{s: s}, waited, nil
	case <-ctx.Done():
		s.mu.Lock()
		req.canceled = true
		s.mu.Unlock()
		s.signal()
		return nil, s.now().Sub(start), ctx.Err()
	case <-s.done:
		return nil, s.now().Sub(start), ErrClosed
	}
}

func (s *Scheduler) release() {
	s.mu.Lock()
	if s.inflight > 0 {
		s.inflight--
	}
	s.mu.Unlock()
	s.signal()
}

func (s *Scheduler) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
	s.signal()
}

func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) run() {
	for {
		s.mu.Lock()
		for !s.closed && (len(s.queue) == 0 || s.inflight >= s.cfg.MaxInflight) {
			s.mu.Unlock()
			select {
			case <-s.wake:
			case <-s.done:
				return
			}
			s.mu.Lock()
		}
		if s.closed {
			s.mu.Unlock()
			return
		}
		idx := s.pickLocked(s.now())
		req := s.queue[idx]
		s.queue = append(s.queue[:idx], s.queue[idx+1:]...)
		if req.canceled || req.ctx.Err() != nil {
			s.mu.Unlock()
			continue
		}
		req.granted = true
		s.inflight++
		close(req.ready)
		s.mu.Unlock()
	}
}

func (s *Scheduler) pickLocked(now time.Time) int {
	best := 0
	for i := 1; i < len(s.queue); i++ {
		if s.beforeLocked(now, s.queue[i], s.queue[best]) {
			best = i
		}
	}
	return best
}

func (s *Scheduler) beforeLocked(now time.Time, a, b *request) bool {
	ar := effectiveRank(a, now, s.cfg.AgingPeriod)
	br := effectiveRank(b, now, s.cfg.AgingPeriod)
	if ar != br {
		return ar < br
	}
	return a.seq < b.seq
}

func effectiveRank(r *request, now time.Time, aging time.Duration) int {
	if aging <= 0 {
		return r.class.Rank
	}
	steps := int(now.Sub(r.enqueued) / aging)
	rank := r.class.Rank - steps
	if rank < -1024 {
		rank = -1024
	}
	return rank
}

type QueueDepth struct {
	Priority string `json:"priority"`
	Depth    int    `json:"depth"`
	Inflight bool   `json:"inflight"`
}

func (s *Scheduler) Queues() []QueueDepth {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := make(map[string]int)
	for _, r := range s.queue {
		counts[r.class.Name]++
	}
	seen := make(map[string]bool)
	var out []QueueDepth
	for _, r := range s.queue {
		if seen[r.class.Name] {
			continue
		}
		seen[r.class.Name] = true
		out = append(out, QueueDepth{Priority: r.class.Name, Depth: counts[r.class.Name]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

func (s *Scheduler) Inflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight
}

func (s *Scheduler) Queued() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}
