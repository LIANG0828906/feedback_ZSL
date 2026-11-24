package audiocal

import "sync"

// Pool 限制分段拟合/编码的最大并行度与每任务频响缓冲占用（业务要求 8）。
// 检查点写入与设备下发不经过本池，由各自串行锁保证段序稳定。
type Pool struct {
	wg     sync.WaitGroup
	sem    chan struct{}
	maxBuf int
	bufMu  sync.Mutex
	bufUse int
	maxP   int
}

// NewPool 构建并发池：maxParallel 为最大并行 worker 数，maxBufferPerTask 为每任务频响缓冲上限。
func NewPool(maxParallel, maxBufferPerTask int) *Pool {
	if maxParallel < 1 {
		maxParallel = 1
	}
	if maxBufferPerTask < 1 {
		maxBufferPerTask = 1
	}
	return &Pool{
		sem:    make(chan struct{}, maxParallel),
		maxBuf: maxBufferPerTask,
		maxP:   maxParallel,
	}
}

// Submit 在有界 worker 上执行任务；缓冲申请超过单任务上限直接拒绝。
func (p *Pool) Submit(bufferSlots int, fn func() error) error {
	if bufferSlots > p.maxBuf {
		return newErrf(CodePara, "buffer_slots",
			"频响缓冲 %d 超过每线程上限 %d", bufferSlots, p.maxBuf)
	}
	p.sem <- struct{}{}
	p.wg.Add(1)
	p.bufMu.Lock()
	p.bufUse += bufferSlots
	p.bufMu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() {
			p.bufMu.Lock()
			p.bufUse -= bufferSlots
			p.bufMu.Unlock()
			<-p.sem
		}()
		_ = fn()
	}()
	return nil
}

// Wait 等待全部 worker 结束。
func (p *Pool) Wait() {
	p.wg.Wait()
}

// BufferInUse 返回当前频响缓冲占用。
func (p *Pool) BufferInUse() int {
	p.bufMu.Lock()
	defer p.bufMu.Unlock()
	return p.bufUse
}

// MaxParallel 返回最大并行度。
func (p *Pool) MaxParallel() int { return p.maxP }

// ParallelFit 在有界池上并行拟合多个测量记录，结果按输入顺序返回。
func ParallelFit(samples []*Sample, cfg FitConfig, maxParallel, maxBuffer int) ([]*CalibrationModel, []error) {
	pool := NewPool(maxParallel, maxBuffer)
	models := make([]*CalibrationModel, len(samples))
	errs := make([]error, len(samples))
	var mu sync.Mutex
	for i, s := range samples {
		i, s := i, s
		bufSlots := len(s.Target) * len(s.Channels)
		_ = pool.Submit(bufSlots, func() error {
			m, err := Fit(s, cfg)
			mu.Lock()
			models[i] = m
			errs[i] = err
			mu.Unlock()
			return err
		})
	}
	pool.Wait()
	return models, errs
}
