package streaming

import (
	"context"
	"fmt"
	"sync"

	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
)

type Pool struct {
	workers     int
	bufferCells int
	tokens      chan struct{}
	mu          sync.Mutex
	events      []BudgetEvent
}

func NewPool(params model.Params) (*Pool, *model.AppError) {
	if params.MaxWorkers < 1 {
		return nil, model.ParamError("max_workers", "工作线程数必须大于零")
	}
	if params.RasterResolution*params.RasterResolution > params.RasterBufferCells {
		return nil, model.ParamError("raster_resolution",
			"单线程栅格缓冲预算不足，已阻塞任务并记录原因")
	}
	return &Pool{
		workers:     params.MaxWorkers,
		bufferCells: params.RasterBufferCells,
		tokens:      make(chan struct{}, params.MaxWorkers),
	}, nil
}

func (p *Pool) EvaluateChunks(ctx context.Context, eval *geometry.Evaluator,
	window int, indices []int) ([]geometry.ChunkVerdict, *model.AppError) {

	results := make([]geometry.ChunkVerdict, len(indices))
	errs := make([]*model.AppError, len(indices))
	var wg sync.WaitGroup
	for i, chunkIndex := range indices {
		i, chunkIndex := i, chunkIndex
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case p.tokens <- struct{}{}:
				defer func() { <-p.tokens }()
			case <-ctx.Done():
				errs[i] = model.InternalError(window, "遮挡剔除被取消", ctx.Err())
				return
			default:
				p.record(BudgetEvent{
					Window: window, Chunk: chunkIndex, Reason: "worker_slot_wait",
					Detail: fmt.Sprintf("并发预算 %d 已耗尽；任务阻塞排队，不丢弃分块", p.workers),
				})
				select {
				case p.tokens <- struct{}{}:
					defer func() { <-p.tokens }()
				case <-ctx.Done():
					errs[i] = model.InternalError(window, "遮挡剔除被取消", ctx.Err())
					return
				}
			}
			results[i] = eval.EvaluateChunk(chunkIndex)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (p *Pool) record(event BudgetEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, event)
}

func (p *Pool) Events() []BudgetEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]BudgetEvent, len(p.events))
	copy(out, p.events)
	return out
}

func (p *Pool) DrainEvents() []BudgetEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.events
	p.events = nil
	return out
}
