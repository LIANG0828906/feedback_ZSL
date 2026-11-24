// Package pipeline 编排分帧提取、窗口状态机、检查点与事件聚合，
// 提供实时增量、断点恢复与全量重算三条确定性一致的执行路径。
package pipeline

import (
	"context"
	"sync"
	"time"

	"audiobench/internal/dsp"
	"audiobench/internal/match"
	"audiobench/internal/model"
	"audiobench/internal/window"
)

// Limits 并发资源边界。
type Limits struct {
	MaxWorkers        int // 最大并行工作线程
	MaxQueuedWindows  int // 待处理窗口队列上限，耗尽时提交方阻塞排队
	FrameBufPerWorker int // 每线程采样/频谱缓冲上限（帧数）
}

// DefaultLimits 保守默认值，适配普通开发机。
func DefaultLimits() Limits {
	return Limits{MaxWorkers: 2, MaxQueuedWindows: 4, FrameBufPerWorker: 256}
}

// BlockEvent 记录一次“缓冲耗尽、阻塞排队”事件，绝不静默丢窗。
type BlockEvent struct {
	WindowIndex int    `json:"window_index"`
	Reason      string `json:"reason"`
}

// CheckpointStore 抽象检查点存储，便于内存/ SQLite 两种实现复用同一引擎。
type CheckpointStore interface {
	CommitWindow(ctx context.Context, w window.Window) error
}

// Engine 窗口级增量标注引擎。
type Engine struct {
	mu        sync.Mutex
	plan      *window.Plan
	samples   []float64
	params    model.Params
	templates match.TemplateSet
	store     CheckpointStore
	limits    Limits
	blocks    []BlockEvent
	budget    chan struct{}
	paused    bool
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// NewEngine 构建引擎并注入窗口计划、素材与模板。
func NewEngine(plan *window.Plan, samples []float64, p model.Params,
	categories []model.Category, sampleRate int, store CheckpointStore, limits Limits) *Engine {
	return &Engine{
		plan:      plan,
		samples:   samples,
		params:    p,
		templates: match.BuildTemplates(categories, sampleRate, p),
		store:     store,
		limits:    limits,
		// 每线程采样/频谱缓冲预算：每个在制窗口占用一个令牌，
		// 总在制帧数不超过 MaxWorkers × FrameBufPerWorker。
		budget:    make(chan struct{}, limits.MaxWorkers*limits.FrameBufPerWorker),
	}
}

// Plan 返回窗口计划（只读快照）。
func (e *Engine) Plan() *window.Plan { return e.plan }

// BlockEvents 返回阻塞排队记录。
func (e *Engine) BlockEvents() []BlockEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]BlockEvent, len(e.blocks))
	copy(cp, e.blocks)
	return cp
}

// Pause / Resume 控制调度循环；已在运行的窗口会跑完，不再派发新窗口。
func (e *Engine) Pause() {
	e.mu.Lock()
	e.paused = true
	e.mu.Unlock()
}

func (e *Engine) Resume() {
	e.mu.Lock()
	e.paused = false
	e.mu.Unlock()
}

// Cancel 取消全部未完成工作。
func (e *Engine) Cancel() {
	e.mu.Lock()
	if e.cancel != nil {
		e.cancel()
	}
	e.mu.Unlock()
}

// Wait 等待当前运行批次结束。
func (e *Engine) Wait() { e.wg.Wait() }

// RunToEnd 实时/恢复路径：从首个未提交窗口开始，按窗口顺序并行提取、
// 串行提交，直到全部完成或 ctx 取消。返回 ctx 错误（取消时通常为 nil 结果）。
func (e *Engine) RunToEnd(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	e.cancel = cancel
	e.paused = false
	e.mu.Unlock()
	defer cancel()

	sem := make(chan struct{}, e.limits.MaxWorkers)
	results := make(chan window.Window, e.limits.MaxWorkers)

	var collector sync.WaitGroup
	collector.Add(1)
	go func() {
		defer collector.Done()
		e.collectCommits(ctx, results)
	}()

dispatch:
	for idx := range e.plan.Windows {
		if ctx.Err() != nil {
			break
		}
		e.mu.Lock()
		paused := e.paused
		state := e.plan.Windows[idx].State
		e.mu.Unlock()
		if state == window.StateCommitted {
			continue
		}
		if paused {
			// 暂停：停在当前窗口，等待恢复或取消。
			for paused && ctx.Err() == nil {
				e.mu.Lock()
				paused = e.paused
				e.mu.Unlock()
				if paused {
					select {
					case <-ctx.Done():
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
			if ctx.Err() != nil {
				break dispatch
			}
		}
		// 有界缓冲：工作槽占满时在此阻塞排队，并登记原因而非静默丢窗。
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break dispatch
		}
		if len(sem) == cap(sem) {
			e.RecordBlock(idx, "工作线程缓冲耗尽，窗口阻塞排队等待空闲槽")
		}
		e.wg.Add(1)
		go func(windowIdx int) {
			defer e.wg.Done()
			defer func() { <-sem }()
			w := e.processWindow(ctx, windowIdx)
			select {
			case results <- w:
			case <-ctx.Done():
			}
		}(idx)
	}
	e.wg.Wait()
	close(results)
	collector.Wait()
	return ctx.Err()
}

// processWindow 执行单个窗口的状态迁移与确定性指纹提取。
func (e *Engine) processWindow(ctx context.Context, idx int) window.Window {
	e.mu.Lock()
	w := &e.plan.Windows[idx]
	if err := w.Transition(window.StateExtracting); err != nil {
		// 理论不可达：派发前已过滤；保留 E_STATE 语义。
		e.mu.Unlock()
		return *w
	}
	start, end := w.StartFrame, w.EndFrame
	e.mu.Unlock()

	// 缓冲预算门：预算耗尽时阻塞排队并登记原因，绝不静默丢窗。
	for f := start; f < end; f++ {
		select {
		case e.budget <- struct{}{}:
		case <-ctx.Done():
			e.markFailed(idx, "上下文取消，窗口未取得缓冲预算")
			return e.plan.Windows[idx]
		default:
			e.RecordBlock(idx, "每线程频谱缓冲预算耗尽，阻塞等待缓冲回收")
			select {
			case e.budget <- struct{}{}:
			case <-ctx.Done():
				e.markFailed(idx, "上下文取消，等待缓冲预算期间终止")
				return e.plan.Windows[idx]
			}
		}
	}

	fps := make([]model.Fingerprint, 0, end-start)
	for f := start; f < end; f++ {
		if ctx.Err() != nil {
			for released := start; released < f; released++ {
				<-e.budget
			}
			e.markFailed(idx, "上下文取消，窗口未完成")
			return e.plan.Windows[idx]
		}
		fp := dsp.ExtractFrame(e.samples, f,
			e.params.FrameLength, e.params.FrameShift, e.params.Bands)
		fps = append(fps, fp)
		<-e.budget // 该帧频谱缓冲使用完毕，回收预算
	}

	// 窗口内事件片段（最终由全局聚合重算并冻结到窗口，保证区间一致）。
	localEvents := match.Aggregate(fps, e.templates, e.params)

	e.mu.Lock()
	defer e.mu.Unlock()
	w = &e.plan.Windows[idx]
	w.Fingerprints = fps
	w.Events = localEvents
	w.Params = e.params
	if err := w.Transition(window.StateCommitted); err != nil {
		w.FailReason = err.Error()
		_ = w.Transition(window.StateFailed)
		return *w
	}
	return *w
}

// collectCommits 单一提交协程：检查点写入严格串行、按窗口序号稳定排序。
func (e *Engine) collectCommits(ctx context.Context, results <-chan window.Window) {
	pending := map[int]window.Window{}
	e.mu.Lock()
	next := e.firstUncommittedLocked()
	e.mu.Unlock()
	flush := func() bool {
		for {
			w, ok := pending[next]
			if !ok {
				return false
			}
			if err := e.store.CommitWindow(ctx, w); err != nil {
				e.markFailed(w.Index, err.Error())
				delete(pending, next)
				next++
				continue
			}
			delete(pending, next)
			next++
		}
	}
	for {
		select {
		case w, ok := <-results:
			if !ok {
				flush()
				return
			}
			pending[w.Index] = w
			flush()
		case <-ctx.Done():
			return
		}
	}
}

func (e *Engine) firstUncommitted() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.firstUncommittedLocked()
}

func (e *Engine) firstUncommittedLocked() int {
	for i := range e.plan.Windows {
		if e.plan.Windows[i].State != window.StateCommitted {
			return i
		}
	}
	return len(e.plan.Windows)
}

func (e *Engine) markFailed(idx int, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w := &e.plan.Windows[idx]
	if w.State == window.StateExtracting || w.State == window.StatePending {
		_ = w.Transition(window.StateFailed)
	}
	w.FailReason = reason
}

// RetryWindow 把失败窗口重置为待提取，等待下一轮 RunToEnd。
func (e *Engine) RetryWindow(idx int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if idx < 0 || idx >= len(e.plan.Windows) {
		return model.NewErr(model.ERange, "窗口编号 %d 越界", idx).
			With("window_index", idx)
	}
	w := &e.plan.Windows[idx]
	if w.State != window.StateFailed {
		return model.NewErr(model.EState,
			"窗口 %d 状态为 %s，仅失败窗口可重试", idx, w.State).
			With("window_index", idx).With("state", w.State)
	}
	w.FailReason = ""
	return w.Transition(window.StatePending)
}

// RecordBlock 由外层在队列阻塞时登记原因（接口层使用）。
func (e *Engine) RecordBlock(idx int, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.blocks = append(e.blocks, BlockEvent{WindowIndex: idx, Reason: reason})
}

// AllFingerprints 按帧号顺序汇总全部已提交窗口的帧指纹。
func (e *Engine) AllFingerprints() []model.Fingerprint {
	var out []model.Fingerprint
	for i := range e.plan.Windows {
		w := &e.plan.Windows[i]
		if w.State == window.StateCommitted {
			out = append(out, w.Fingerprints...)
		}
	}
	return out
}

// IsComplete 报告全部窗口是否已提交。
func (e *Engine) IsComplete() bool {
	committed, total := e.plan.Progress()
	return committed == total
}
