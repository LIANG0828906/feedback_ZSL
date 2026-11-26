package locklab

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
)

type ParallelOptions struct {
	Workers           int
	MemoryBudgetBytes int64
	BatchSize         int
	ScheduleSeed      int64
}

type ParallelDiagnostics struct {
	WorkersConfigured int               `json:"workers_configured"`
	WorkersUsed       int               `json:"workers_used"`
	BatchSize         int               `json:"batch_size"`
	ScheduleSeed      int64             `json:"schedule_seed"`
	PrecomputeDigest  string            `json:"precompute_digest"`
	PeakInFlight      int               `json:"peak_in_flight"`
	PeakWorkerMemory  int64             `json:"peak_worker_memory_bytes"`
	StepObservations  []StepObservation `json:"step_observations"`
}

type ParallelResult struct {
	Run         *RunResult
	Diagnostics ParallelDiagnostics
}

type parallelTask struct {
	step     int
	txID     string
	localSeq int
	kind     OpKind
	estimate int64
	digest   string
	path     string
}

type parallelIssue struct {
	step    int
	code    ErrorCode
	path    string
	message string
}

// RunParallel performs bounded parallel per-operation preparation and then a
// deterministic lock-arbitration pass. The coordinator's single ordered
// decision stream is intentional: it makes worker count, batch split and
// schedule shuffling observationally irrelevant to lock/visibility semantics.
func RunParallel(project *Project, options ParallelOptions) (*ParallelResult, error) {
	p := project.Clone()
	if p == nil {
		return nil, NewError(EFieldRequired, "$", "project is nil")
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	workers := options.Workers
	if workers == 0 {
		workers = p.WorkerBudget
	}
	if workers < 1 || workers > MaxWorkers || workers > p.WorkerBudget {
		return nil, NewError(EBackpressureWorkers, "$.parallel.workers",
			"worker budget %d is outside [1,%d] or exceeds declared project budget %d",
			workers, MaxWorkers, p.WorkerBudget)
	}
	memory := options.MemoryBudgetBytes
	if memory == 0 {
		memory = p.MemoryBudgetBytes
	}
	if memory < MinWorkerMemoryBytes {
		return nil, NewError(EBackpressureMemory, "$.parallel.memory_budget_bytes",
			"per-worker memory budget %d is below minimum %d", memory, MinWorkerMemoryBytes)
	}
	if memory > p.MemoryBudgetBytes {
		return nil, NewError(EBackpressureMemory, "$.parallel.memory_budget_bytes",
			"per-worker memory budget %d exceeds declared project budget %d", memory, p.MemoryBudgetBytes)
	}
	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = workers
	}

	schedule := BuildSchedule(p)
	tasks := make([]parallelTask, len(schedule))
	for i, scheduled := range schedule {
		op := p.Transactions[scheduled.TxIndex].Ops[scheduled.OpIndex]
		estimate := int64(1024 + len(op.ItemID) + len(op.Table) + len(op.Value) + 256)
		tasks[i] = parallelTask{
			step: scheduled.GlobalStep, txID: scheduled.TxID, localSeq: scheduled.LocalSeq,
			kind: op.Kind, estimate: estimate,
			digest: fmt.Sprintf("%d|%s|%s|%s|%s|%s|%d",
				scheduled.GlobalStep, scheduled.TxID, op.Kind, op.ItemID,
				op.Mode, op.Value, op.AtMs),
			path: fmt.Sprintf("$.transactions[%d].operations[%d]", scheduled.TxIndex, scheduled.OpIndex),
		}
	}
	rand.New(rand.NewSource(options.ScheduleSeed)).Shuffle(len(tasks), func(i, j int) {
		tasks[i], tasks[j] = tasks[j], tasks[i]
	})
	digestParts := make([]string, len(tasks))
	for i, task := range tasks {
		digestParts[i] = task.digest
	}
	precomputeDigest := ShortDigest(digestParts...)

	jobQueue := make(chan []parallelTask)
	var issues []parallelIssue
	var issuesMu sync.Mutex
	var inFlight int64
	var peakInFlight int64
	var workerMemory int64
	var peakWorkerMemory int64
	var mu sync.Mutex
	var waitGroup sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for batch := range jobQueue {
				current := atomic.AddInt64(&inFlight, int64(len(batch)))
				for {
					peak := atomic.LoadInt64(&peakInFlight)
					if current <= peak || atomic.CompareAndSwapInt64(&peakInFlight, peak, current) {
						break
					}
				}
				var batchMemory int64
				for _, task := range batch {
					if task.estimate > memory {
						issuesMu.Lock()
						issues = append(issues, parallelIssue{
							step: task.step, code: EBackpressureMemory, path: task.path,
							message: fmt.Sprintf("operation requires %d bytes, worker budget is %d",
								task.estimate, memory),
						})
						issuesMu.Unlock()
						continue
					}
					if task.estimate > batchMemory {
						batchMemory = task.estimate
					}
				}
				mu.Lock()
				if batchMemory > workerMemory {
					workerMemory = batchMemory
				}
				if workerMemory > peakWorkerMemory {
					peakWorkerMemory = workerMemory
				}
				mu.Unlock()
				atomic.AddInt64(&inFlight, -int64(len(batch)))
			}
		}()
	}
	for start := 0; start < len(tasks); start += batchSize {
		end := start + batchSize
		if end > len(tasks) {
			end = len(tasks)
		}
		jobQueue <- append([]parallelTask(nil), tasks[start:end]...)
	}
	close(jobQueue)
	waitGroup.Wait()
	collected := issues
	if len(collected) > 0 {
		sort.Slice(collected, func(i, j int) bool {
			if collected[i].step != collected[j].step {
				return collected[i].step < collected[j].step
			}
			return collected[i].path < collected[j].path
		})
		first := collected[0]
		return nil, NewError(first.code, first.path, "%s", first.message)
	}

	observations := make([]StepObservation, 0, len(schedule))
	result, err := Run(p, RunOptions{ObserveStep: func(observation StepObservation) {
		observations = append(observations, observation)
	}})
	if err != nil {
		return nil, err
	}
	sort.Slice(observations, func(i, j int) bool {
		if observations[i].ExecutedStep != observations[j].ExecutedStep {
			return observations[i].ExecutedStep < observations[j].ExecutedStep
		}
		return observations[i].GlobalStep < observations[j].GlobalStep
	})
	return &ParallelResult{
		Run: result,
		Diagnostics: ParallelDiagnostics{
			WorkersConfigured: workers, WorkersUsed: workers, BatchSize: batchSize,
			ScheduleSeed: options.ScheduleSeed, PrecomputeDigest: precomputeDigest,
			PeakInFlight:     int(atomic.LoadInt64(&peakInFlight)),
			PeakWorkerMemory: peakWorkerMemory, StepObservations: observations,
		},
	}, nil
}

func ShortDigest(parts ...string) string {
	h := fnv1a()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

type fnv64a struct {
	state uint64
}

func fnv1a() *fnv64a {
	return &fnv64a{state: 1469598103934665603}
}

func (f *fnv64a) Write(data []byte) (int, error) {
	for _, value := range data {
		f.state ^= uint64(value)
		f.state *= 1099511628211
	}
	return len(data), nil
}

func (f *fnv64a) Sum64() uint64 { return f.state }
