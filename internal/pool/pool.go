package pool

import (
	"sync"

	"github.com/gameaudio/workbench/internal/model"
)

// BackpressureEvent 记录一次缓冲耗尽导致的阻塞排队。
type BackpressureEvent struct {
	Reason string
}

// Job 是一个声源-听者对计算任务。
type Job struct {
	SourceID   string
	ListenerID string
}

// Config 声明资源边界。
type Config struct {
	MaxWorkers      int // 最大并行度
	PerWorkerBuffer int // 每个工作线程的几何/采样缓冲上限（任务槽）
}

// ComputeFunc 由计算引擎实现。
type ComputeFunc func(sourceID, listenerID string) model.Result

// Pool 是固定并行度的工作线程池。
type Pool struct {
	cfg          Config
	compute      ComputeFunc
	jobs         chan Job
	results      chan model.Result
	wg           sync.WaitGroup
	mu           sync.Mutex
	backpressure []BackpressureEvent
}

// New 构造线程池并启动固定数量的工作线程。
func New(cfg Config, compute ComputeFunc) *Pool {
	if cfg.MaxWorkers < 1 {
		cfg.MaxWorkers = 1
	}
	if cfg.PerWorkerBuffer < 1 {
		cfg.PerWorkerBuffer = 1
	}
	// 全局排队容量 = 并行度 * 每线程缓冲上限，缓冲耗尽时提交方阻塞。
	p := &Pool{
		cfg:     cfg,
		compute: compute,
		jobs:    make(chan Job, cfg.MaxWorkers*cfg.PerWorkerBuffer),
		results: make(chan model.Result, cfg.MaxWorkers*cfg.PerWorkerBuffer),
	}
	for i := 0; i < cfg.MaxWorkers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
	return p
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	for j := range p.jobs {
		// 每个任务使用独立的有界缓冲语义：几何与采样切片在 ComputePair 内
		// 就地分配并在返回后释放，不跨任务累积，保证每线程缓冲上限可证。
		r := p.compute(j.SourceID, j.ListenerID)
		p.results <- r
	}
}

// Submit 提交一个任务；排队缓冲耗尽时阻塞并记录原因，绝不静默丢弃。
func (p *Pool) Submit(j Job) {
	select {
	case p.jobs <- j:
	default:
		p.recordBackpressure("per-worker buffer saturated; blocking enqueue")
		p.jobs <- j // 阻塞直到有空位
	}
}

// Result 返回结果通道。
func (p *Pool) Result() <-chan model.Result { return p.results }

// CloseAndWait 关闭任务通道并等待全部工作线程退出。
func (p *Pool) CloseAndWait() {
	close(p.jobs)
	p.wg.Wait()
	close(p.results)
}

func (p *Pool) recordBackpressure(reason string) {
	p.mu.Lock()
	p.backpressure = append(p.backpressure, BackpressureEvent{Reason: reason})
	p.mu.Unlock()
}

// Backpressure 返回已记录的阻塞排队事件副本。
func (p *Pool) Backpressure() []BackpressureEvent {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]BackpressureEvent, len(p.backpressure))
	copy(out, p.backpressure)
	return out
}
