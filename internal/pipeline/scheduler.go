package pipeline

import (
	"fmt"
	"sync"
	"time"

	"msworkbench/internal/domain"
	"msworkbench/internal/e"
	"msworkbench/internal/graph"
	"msworkbench/internal/quant"
	"msworkbench/internal/recon"
	"msworkbench/internal/sample"
)

type WaitEvent struct {
	TaskIndex  int    `json:"task_index"`
	Block      string `json:"block"`
	Reason     string `json:"reason"`
	WaitMicros int64  `json:"wait_micros"`
}

type computedBlock struct {
	qr *quant.Result
	rr *recon.BlockResult
}

type schedulerJob struct {
	index int
	coord domain.BlockCoord
	cells int
}

type jobResult struct {
	index int
	block string
	out   *computedBlock
	err   error
}

func normalizeBudget(parallel, bufferCap, queueCap int) (int, int, int) {
	if parallel <= 0 {
		parallel = 2
	}
	if bufferCap <= 0 {
		bufferCap = 4096
	}
	if queueCap <= 0 {
		queueCap = parallel
	}
	if queueCap < 1 {
		queueCap = 1
	}
	return parallel, bufferCap, queueCap
}

func computeBlocks(p *domain.Project, set *sample.SampleSet, g *graph.Graph, profile domain.SampleScale, thresholds quant.Thresholds, coords []domain.BlockCoord, parallel, bufferCap, queueCap int) (map[string]*computedBlock, []WaitEvent, error) {
	parallel, bufferCap, queueCap = normalizeBudget(parallel, bufferCap, queueCap)
	for _, c := range coords {
		pixels := p.BlockWidth * p.BlockHeight
		cells := pixels * len(p.Bands)
		if cells > bufferCap {
			return nil, nil, e.Param("scheduler.pixel_buffer_per_worker", fmt.Sprintf(">=%d cells", cells), "block %s needs %d cells but per-worker cap is %d", c, cells, bufferCap)
		}
	}

	jobs := make(chan schedulerJob, queueCap)
	results := make(chan jobResult, len(coords))
	var workerErr error
	workers := parallel
	if workers > len(coords) {
		workers = len(coords)
	}
	if workers == 0 {
		return map[string]*computedBlock{}, nil, nil
	}
	for w := 0; w < workers; w++ {
		go func() {
			buffer := make([]int64, 0, bufferCap)
			for job := range jobs {
				block := job.coord.String()
				decision := quant.Decision{}
				if g != nil {
					decision = g.Decisions[block]
				}
				if decision.Method == "" {
					obs := len(p.Bands) * p.BlockWidth * p.BlockHeight
					for _, list := range set.Summaries[profile] {
						if list.Block == block {
							obs = list.ObservationCount
							break
						}
					}
					decision = quant.Select(profile, obs, thresholds)
					decision.Block = block
				}
				qr, err := quant.QuantizeBlock(p, set, job.coord, decision, thresholds)
				if err != nil {
					results <- jobResult{index: job.index, block: block, err: err}
					continue
				}
				indices, _, _ := p.BlockPixels(job.coord)
				buffer = buffer[:job.cells]
				var scratchSum int64
				k := 0
				for bi := range p.Bands {
					for _, gi := range indices {
						v := set.Radiance[bi].Values[gi]
						buffer[k] = v
						scratchSum += v
						k++
					}
				}
				if scratchSum != qr.BlockRadianceSum {
					results <- jobResult{index: job.index, block: block, err: e.New(e.Diff, "scheduler scratch/checksum mismatch at block %s", block)}
					continue
				}
				rr, err := recon.ReconstructBlock(p, qr)
				if err != nil {
					results <- jobResult{index: job.index, block: block, err: err}
					continue
				}
				results <- jobResult{index: job.index, block: block, out: &computedBlock{qr: qr, rr: rr}}
			}
		}()
	}

	waits := []WaitEvent{}
	var waitMu sync.Mutex
	var producerWG sync.WaitGroup
	producerWG.Add(1)
	go func() {
		defer producerWG.Done()
		for i, c := range coords {
			job := schedulerJob{index: i, coord: c, cells: p.BlockWidth * p.BlockHeight * len(p.Bands)}
			if len(jobs) == cap(jobs) {
				start := time.Now()
				jobs <- job
				event := WaitEvent{TaskIndex: i, Block: c.String(), Reason: "pending_block_queue_full; producer blocked until a worker drained the budget", WaitMicros: time.Since(start).Microseconds()}
				waitMu.Lock()
				waits = append(waits, event)
				waitMu.Unlock()
			} else {
				jobs <- job
			}
		}
		close(jobs)
	}()

	out := map[string]*computedBlock{}
	for i := 0; i < len(coords); i++ {
		res := <-results
		if res.err != nil {
			workerErr = res.err
			continue
		}
		out[res.block] = res.out
	}
	producerWG.Wait()
	waitMu.Lock()
	waitEvents := append([]WaitEvent(nil), waits...)
	waitMu.Unlock()
	if workerErr != nil {
		return nil, waitEvents, workerErr
	}
	return out, waitEvents, nil
}
