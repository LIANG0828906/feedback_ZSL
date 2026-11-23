// Package worker 提供有界工作线程池：限制最大并行度与每线程栅格缓冲上限，
// 支持上下文取消；共享 SQLite 写入仍由 persist.Store 自行串行化。
package worker

import (
	"context"
	"sync"

	"occlbench/internal/core"
)

// Pool 固定大小的工作线程池。
type Pool struct {
	sem       chan struct{}
	maxWorker int
	// maxRasterCells 单任务允许的栅格格子上限（每线程栅格缓冲边界）。
	maxRasterCells int
	wg             sync.WaitGroup
}

// New 构造线程池。maxWorker 与 maxRasterCells 必须为正。
func New(maxWorker, maxRasterCells int) (*Pool, error) {
	if maxWorker <= 0 {
		return nil, core.NewError(core.EPara, "maxWorker must be positive",
			map[string]any{"field": "pool.maxWorker", "value": maxWorker})
	}
	if maxRasterCells <= 0 {
		return nil, core.NewError(core.EPara, "maxRasterCells must be positive",
			map[string]any{"field": "pool.maxRasterCells", "value": maxRasterCells})
	}
	return &Pool{
		sem:            make(chan struct{}, maxWorker),
		maxWorker:      maxWorker,
		maxRasterCells: maxRasterCells,
	}, nil
}

// MaxWorker 返回最大并行度。
func (p *Pool) MaxWorker() int { return p.maxWorker }

// CheckRasterBuffer 校验栅格缓冲是否超出每线程上限，越界返回 E_PARA。
func (p *Pool) CheckRasterBuffer(gridResolution int) error {
	if gridResolution <= 0 {
		return core.NewError(core.EPara, "gridResolution must be positive",
			map[string]any{"field": "gridResolution", "resolution": gridResolution})
	}
	cells := gridResolution * gridResolution
	if cells > p.maxRasterCells {
		return core.NewError(core.EPara, "raster buffer exceeds per-worker cap",
			map[string]any{"field": "gridResolution", "resolution": gridResolution,
				"cells": cells, "cap": p.maxRasterCells})
	}
	return nil
}

// Submit 提交一个任务到线程池，受最大并行度与上下文取消约束。
func (p *Pool) Submit(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		select {
		case p.sem <- struct{}{}:
			defer func() { <-p.sem }()
			if err := ctx.Err(); err != nil {
				return
			}
			_ = fn()
		case <-ctx.Done():
			return
		}
	}()
	return nil
}

// Wait 等待全部已提交任务完成或上下文取消。
func (p *Pool) Wait() { p.wg.Wait() }

// Parallel 并行执行一组任务，按索引收集错误；返回首个发生的错误。
// 任务结果由调用方按固定索引写入，保证输出与执行顺序无关。
func (p *Pool) Parallel(ctx context.Context, n int, task func(i int) error) error {
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		i := i
		if err := p.Submit(ctx, func() error {
			errs[i] = task(i)
			return errs[i]
		}); err != nil {
			return err
		}
	}
	p.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
