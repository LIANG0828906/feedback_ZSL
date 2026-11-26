package bonerig

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Budget declares the bounded parallel execution resources.
type Budget struct {
	MaxWorkers              int `json:"max_workers"`
	MaxJointBufferPerWorker int `json:"max_joint_buffer_per_worker"`
	MaxPendingFrames        int `json:"max_pending_frames"`
}

func DefaultBudget() Budget {
	return Budget{MaxWorkers: 2, MaxJointBufferPerWorker: 8, MaxPendingFrames: 2}
}

func (b Budget) Normalized() Budget {
	if b.MaxWorkers <= 0 {
		b.MaxWorkers = 1
	}
	if b.MaxJointBufferPerWorker <= 0 {
		b.MaxJointBufferPerWorker = 1
	}
	if b.MaxPendingFrames <= 0 {
		b.MaxPendingFrames = 1
	}
	return b
}

// WaitRecord explains a blocking queue/buffer wait.
type WaitRecord struct {
	Reason            string `json:"reason"`
	WorkerID          int    `json:"worker_id,omitempty"`
	Frame             int    `json:"frame,omitempty"`
	Joint             string `json:"joint,omitempty"`
	WaitedNanoseconds int64  `json:"waited_nanoseconds"`
}

// WaitRecorder is goroutine-safe instrumentation for budget waits.
type WaitRecorder struct {
	mu      sync.Mutex
	records []WaitRecord
}

func (r *WaitRecorder) Add(rec WaitRecord) {
	r.mu.Lock()
	r.records = append(r.records, rec)
	r.mu.Unlock()
}

func (r *WaitRecorder) Records() []WaitRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WaitRecord(nil), r.records...)
}

func (r *WaitRecorder) Count(reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, x := range r.records {
		if x.Reason == reason {
			n++
		}
	}
	return n
}

// JointState is one buffered per-joint result.
type JointState struct {
	Frame     int
	Joint     string
	Output    JointOutput
	Precision PrecisionRecord
}

// BoundedJointBuffer is a fixed-capacity per-worker state buffer.
type BoundedJointBuffer struct {
	workerID int
	frame    int
	ch       chan JointState
	rec      *WaitRecorder
}

func NewBoundedJointBuffer(workerID, frame, capacity int, rec *WaitRecorder) *BoundedJointBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &BoundedJointBuffer{workerID: workerID, frame: frame, ch: make(chan JointState, capacity), rec: rec}
}

// Push blocks when the buffer is full; it never drops a state.
func (b *BoundedJointBuffer) Push(ctx context.Context, st JointState) error {
	select {
	case b.ch <- st:
		return nil
	default:
	}
	start := time.Now()
	select {
	case b.ch <- st:
		if b.rec != nil {
			b.rec.Add(WaitRecord{Reason: "joint_buffer_full", WorkerID: b.workerID, Frame: b.frame, Joint: st.Joint, WaitedNanoseconds: time.Since(start).Nanoseconds()})
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *BoundedJointBuffer) Close() { close(b.ch) }

func (b *BoundedJointBuffer) Channel() <-chan JointState { return b.ch }

// Collected drains a buffer channel into a slice.
func CollectJointStates(ch <-chan JointState) []JointState {
	out := []JointState{}
	for st := range ch {
		out = append(out, st)
	}
	return out
}

// FrameJob is a frame index plus optional failure injection key.
type FrameJob struct {
	Frame int
}

// FramePoolMetrics contains observed parallelism and wait instrumentation.
type FramePoolMetrics struct {
	MaxObservedWorkers int32        `json:"max_observed_workers"`
	Waits              []WaitRecord `json:"waits"`
}

// RunFramePool runs frame jobs with bounded workers and bounded pending queue.
// Process is responsible for its own per-joint buffer if needed.
func RunFramePool(
	ctx context.Context,
	budget Budget,
	frames []int,
	rec *WaitRecorder,
	process func(ctx context.Context, workerID, frame int) *Error,
) ([]*Error, FramePoolMetrics) {
	budget = budget.Normalized()
	if rec == nil {
		rec = &WaitRecorder{}
	}
	type ordinalJob struct {
		ordinal int
		frame   int
	}
	jobs := make(chan ordinalJob, budget.MaxPendingFrames)
	errs := make([]*Error, len(frames))
	var wg sync.WaitGroup
	var active int32
	var maxWorkers int32

	for w := 0; w < budget.MaxWorkers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for job := range jobs {
				frame := job.frame
				cur := atomic.AddInt32(&active, 1)
				for {
					old := atomic.LoadInt32(&maxWorkers)
					if cur <= old || atomic.CompareAndSwapInt32(&maxWorkers, old, cur) {
						break
					}
				}
				errs[job.ordinal] = process(ctx, workerID, frame)
				atomic.AddInt32(&active, -1)
			}
		}(w)
	}

	for ordinal, frame := range frames {
		select {
		case jobs <- ordinalJob{ordinal: ordinal, frame: frame}:
		default:
			start := time.Now()
			select {
			case jobs <- ordinalJob{ordinal: ordinal, frame: frame}:
				rec.Add(WaitRecord{Reason: "frame_queue_full", Frame: frame, WaitedNanoseconds: time.Since(start).Nanoseconds()})
			case <-ctx.Done():
				f := frame
				errs[ordinal] = &Error{Code: CodeState, Message: ctx.Err().Error(), Frame: &f}
			}
		}
	}
	close(jobs)
	wg.Wait()
	return errs, FramePoolMetrics{MaxObservedWorkers: atomic.LoadInt32(&maxWorkers), Waits: rec.Records()}
}
