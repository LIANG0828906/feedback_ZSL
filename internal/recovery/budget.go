package recovery

import "enginelab.local/internal/model"

const (
	DefaultWorkers               = 4
	DefaultQueueDepth            = 16
	HardMaxWorkers               = 32
	HardMaxQueueDepth            = 256
	DefaultMaxSegmentMemoryBytes = int64(16 << 20)
	HardMaxSegmentMemoryBytes    = int64(64 << 20)
	RecordEstimateBytes          = int64(512)
	SegmentOverheadBytes         = int64(4096)
)

func NormalizeBudget(b model.Budget) (model.Budget, error) {
	if b.Workers == 0 {
		b.Workers = DefaultWorkers
	}
	if b.MaxSegmentMemoryBytes == 0 {
		b.MaxSegmentMemoryBytes = DefaultMaxSegmentMemoryBytes
	}
	if b.Workers < 1 || b.Workers > HardMaxWorkers {
		return b, model.NewError(model.EBudgetThreads, "$.budget.workers",
			"workers must be in [1,%d], got %d", HardMaxWorkers, b.Workers).
			WithDetails(map[string]any{"min": 1, "max": HardMaxWorkers, "actual": b.Workers})
	}
	if b.QueueDepth < 0 || b.QueueDepth > HardMaxQueueDepth {
		return b, model.NewError(model.EBudgetThreads, "$.budget.queue_depth",
			"queue_depth must be in [0,%d], got %d", HardMaxQueueDepth, b.QueueDepth)
	}
	if b.MaxSegmentMemoryBytes < 0 || b.MaxSegmentMemoryBytes > HardMaxSegmentMemoryBytes {
		return b, model.NewError(model.EBudgetMemory, "$.budget.max_segment_memory_bytes",
			"segment memory cap must be in [1,%d], got %d", HardMaxSegmentMemoryBytes, b.MaxSegmentMemoryBytes).
			WithDetails(map[string]any{"min": 1, "max": HardMaxSegmentMemoryBytes, "actual": b.MaxSegmentMemoryBytes})
	}
	return b, nil
}

func EstimateSegmentMemory(seg *model.Segment) int64 {
	estimate := SegmentOverheadBytes + int64(len(seg.Records))*RecordEstimateBytes
	for _, rec := range seg.Records {
		estimate += int64(len(rec.Key) + len(rec.Value) + len(rec.ID) + len(rec.TransactionID))
	}
	return estimate
}

type Admission struct {
	Workers    int
	QueueDepth int
	Capacity   int
	used       chan struct{}
}

func NewAdmission(b model.Budget) *Admission {
	capacity := b.Workers + b.QueueDepth
	return &Admission{Workers: b.Workers, QueueDepth: b.QueueDepth, Capacity: capacity, used: make(chan struct{}, capacity)}
}

func (a *Admission) TryAdmit(locator string) error {
	select {
	case a.used <- struct{}{}:
		return nil
	default:
		return model.NewError(model.EBackpressure, locator,
			"worker/queue budget exhausted: capacity=%d workers=%d queue=%d", a.Capacity, a.Workers, a.QueueDepth).
			WithDetails(map[string]any{"capacity": a.Capacity, "workers": a.Workers, "queue_depth": a.QueueDepth})
	}
}

func (a *Admission) Release() {
	select {
	case <-a.used:
	default:
	}
}
