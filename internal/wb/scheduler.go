package wb

import (
	"sync"
	"sync/atomic"
	"time"
)

type Scheduler struct {
	maxParallel  int
	bufferCap    int
	workerSlots  chan struct{}
	bufferTokens chan []CurveSample

	mu             sync.Mutex
	activeTasks    int
	maxActiveSeen  int
	completedTasks int64
	events         []QueueEvent
}

func NewScheduler(spec PrecisionSpec) *Scheduler {
	buffers := make(chan []CurveSample, spec.MaxParallel)
	for i := 0; i < spec.MaxParallel; i++ {
		buffers <- make([]CurveSample, 0, spec.BufferSampleCap)
	}
	return &Scheduler{
		maxParallel:  spec.MaxParallel,
		bufferCap:    spec.BufferSampleCap,
		workerSlots:  make(chan struct{}, spec.MaxParallel),
		bufferTokens: buffers,
	}
}

func (s *Scheduler) MaxParallel() int { return s.maxParallel }
func (s *Scheduler) BufferCap() int   { return s.bufferCap }

// Run executes all tasks while enforcing the worker semaphore. Individual
// task errors are returned by index so one failed room cannot cancel peers.
func (s *Scheduler) Run(taskIDs []string, run func(index int) *APIError) []*APIError {
	errors := make([]*APIError, len(taskIDs))
	var wg sync.WaitGroup
	for i := range taskIDs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			taskID := taskIDs[index]
			waitStart := time.Now()
			queuedReason := ""
			select {
			case s.workerSlots <- struct{}{}:
			default:
				queuedReason = "max-parallel-workers"
				s.workerSlots <- struct{}{}
			}
			waitMS := time.Since(waitStart).Milliseconds()
			if queuedReason != "" {
				s.recordQueue(QueueEvent{TaskID: taskID, Reason: queuedReason, WaitMS: waitMS})
			}
			s.setActive(1)
			defer func() {
				<-s.workerSlots
				s.setActive(-1)
				atomic.AddInt64(&s.completedTasks, 1)
			}()
			errors[index] = run(index)
		}(i)
	}
	wg.Wait()
	return errors
}

// AcquireSampleBuffer returns a fixed-capacity buffer. Callers must chunk
// large curves and always invoke release. Exhaustion blocks and is recorded;
// samples are never dropped.
func (s *Scheduler) AcquireSampleBuffer(taskID string) ([]CurveSample, func()) {
	waitStart := time.Now()
	exhausted := false
	select {
	case buf := <-s.bufferTokens:
		return buf, func() {
			buf = buf[:0]
			s.bufferTokens <- buf
		}
	default:
		exhausted = true
	}
	if exhausted {
		buf := <-s.bufferTokens
		waitMS := time.Since(waitStart).Milliseconds()
		if waitMS == 0 {
			waitMS = 1
		}
		s.recordQueue(QueueEvent{
			TaskID: taskID,
			Reason: "sample-buffer-exhausted",
			WaitMS: waitMS,
		})
		return buf, func() {
			buf = buf[:0]
			s.bufferTokens <- buf
		}
	}
	buf := <-s.bufferTokens
	return buf, func() {
		buf = buf[:0]
		s.bufferTokens <- buf
	}
}

func (s *Scheduler) recordQueue(event QueueEvent) {
	if event.AtUnixMilli == 0 {
		event.AtUnixMilli = time.Now().UnixMilli()
	}
	s.mu.Lock()
	s.events = append(s.events, event)
	if len(s.events) > 200 {
		s.events = s.events[len(s.events)-200:]
	}
	s.mu.Unlock()
}

func (s *Scheduler) setActive(delta int) {
	s.mu.Lock()
	s.activeTasks += delta
	if s.activeTasks > s.maxActiveSeen {
		s.maxActiveSeen = s.activeTasks
	}
	s.mu.Unlock()
}

func (s *Scheduler) Diagnostics() SchedulerDiagnostics {
	s.mu.Lock()
	events := append([]QueueEvent(nil), s.events...)
	active := s.activeTasks
	s.mu.Unlock()
	return SchedulerDiagnostics{
		MaxParallel:     s.maxParallel,
		BufferSampleCap: s.bufferCap,
		ActiveTasks:     active,
		CompletedTasks:  atomic.LoadInt64(&s.completedTasks),
		QueuedEvents:    events,
	}
}

func (s *Scheduler) MaxActiveSeen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxActiveSeen
}
