package level

import (
	"fmt"
	"sync"
)

type GenJob struct {
	PartitionID int
	Fn          func() (*PartitionResult, error)
}

type GenOutcome struct {
	PartitionID int
	Result      *PartitionResult
	Err         error
}

// Pool bounds regeneration concurrency: at most `workers` partitions
// generate in parallel, and each in-flight job must hold one of
// `maxBuffers` vertex/instance buffer tokens. When buffers are exhausted
// jobs block in the queue; work is never silently dropped. A panic inside
// one partition's generation is isolated to that partition's outcome.
type Pool struct {
	workers int
	buffers chan struct{}
}

func NewPool(workers, maxBuffers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	if maxBuffers < 1 {
		maxBuffers = 1
	}
	return &Pool{workers: workers, buffers: make(chan struct{}, maxBuffers)}
}

// Run executes all jobs and returns outcomes in job submission order, so
// results are deterministic regardless of completion order.
func (p *Pool) Run(jobs []GenJob) []GenOutcome {
	out := make([]GenOutcome, len(jobs))
	sem := make(chan struct{}, p.workers)
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func(i int, job GenJob) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			p.buffers <- struct{}{}
			defer func() { <-p.buffers }()
			defer func() {
				if r := recover(); r != nil {
					out[i] = GenOutcome{
						PartitionID: job.PartitionID,
						Err:         fmt.Errorf("partition %d generation panic: %v", job.PartitionID, r),
					}
				}
			}()
			res, err := job.Fn()
			out[i] = GenOutcome{PartitionID: job.PartitionID, Result: res, Err: err}
		}(i, job)
	}
	wg.Wait()
	return out
}
