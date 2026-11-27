// Package pool 提供受限预算的工作线程池：限制最大并行度与每线程状态缓冲上限，
// 缓冲耗尽时调用方阻塞排队，并记录排队原因与等待时长；单任务失败被隔离，不污染其他任务。
package pool

import (
	"sort"
	"sync"
	"time"
)

// Job 是一个可执行结算任务。
type Job struct {
	ID    string
	Apply func() error
}

// Result 记录任务执行结果与排队诊断信息。
type Result struct {
	JobID       string        `json:"job_id"`
	Seq         int           `json:"seq"`
	Err         string        `json:"error,omitempty"`
	QueueReason string        `json:"queue_reason"`
	Wait        time.Duration `json:"wait"`
	Worker      int           `json:"worker"`
}

// Pool 是有界 goroutine 池。
type Pool struct {
	maxParallel     int
	perWorkerBuffer int
	jobs            chan *slot
	wg              sync.WaitGroup
	mu              sync.Mutex
	closed          bool
	seq             int
	results         []Result
}

type slot struct {
	job      Job
	queuedAt time.Time
	reason   string
	seq      int
}

// New 构造池：workers 为最大并行度，perWorkerBuffer 为每线程缓冲上限，
// 总在途缓冲 = workers*perWorkerBuffer。
func New(workers, perWorkerBuffer int) *Pool {
	if workers <= 0 {
		workers = 1
	}
	if perWorkerBuffer <= 0 {
		perWorkerBuffer = 1
	}
	p := &Pool{
		maxParallel:     workers,
		perWorkerBuffer: perWorkerBuffer,
		jobs:            make(chan *slot, workers*perWorkerBuffer),
	}
	for w := 0; w < workers; w++ {
		p.wg.Add(1)
		go p.worker(w)
	}
	return p
}

func (p *Pool) worker(id int) {
	defer p.wg.Done()
	for s := range p.jobs {
		res := Result{JobID: s.job.ID, Seq: s.seq, Worker: id, QueueReason: s.reason, Wait: time.Since(s.queuedAt)}
		func() {
			defer func() {
				if r := recover(); r != nil {
					res.Err = "panic recovered"
				}
			}()
			if err := s.job.Apply(); err != nil {
				res.Err = err.Error()
			}
		}()
		p.mu.Lock()
		p.results = append(p.results, res)
		p.mu.Unlock()
	}
}

// Submit 提交任务；缓冲耗尽时阻塞直到有空位，记录排队原因与等待时长。
func (p *Pool) Submit(j Job) {
	queuedAt := time.Now()
	reason := ""
	p.mu.Lock()
	seq := p.seq
	p.seq++
	p.mu.Unlock()
	// 非阻塞探测一次以记录“缓冲耗尽”原因；随后阻塞等待。
	select {
	case p.jobs <- &slot{job: j, queuedAt: queuedAt, reason: reason, seq: seq}:
		return
	default:
		reason = "per_worker_buffer_full"
	}
	p.jobs <- &slot{job: j, queuedAt: queuedAt, reason: reason, seq: seq}
}

// CloseAndWait 关闭队列并等待全部任务完成，返回按提交顺序稳定排序的结果。
func (p *Pool) CloseAndWait() []Result {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.sortedResults()
	}
	p.closed = true
	p.mu.Unlock()
	close(p.jobs)
	p.wg.Wait()
	return p.sortedResults()
}

func (p *Pool) sortedResults() []Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 结果按 JobID 稳定排序，消除线程完成顺序影响。
	out := append([]Result{}, p.results...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}
