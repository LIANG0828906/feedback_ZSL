package pipeline

import (
	"context"
	"mcapture/internal/store"
	"sync"
)

// unitOfWork 是一个待处理的（通道，分段）单元，顺序固定。
type unitOfWork struct {
	channelID string
	segIdx    int
}

// plan 返回确定性工作顺序：先分段、后通道。串行与并行使用同一份计划。
func (e *Engine) plan() []unitOfWork {
	bounds := e.segmentBounds()
	var plan []unitOfWork
	for si := range bounds {
		for _, ch := range e.Sess.Channels {
			plan = append(plan, unitOfWork{ch.Spec.ID, si})
		}
	}
	return plan
}

// schedule 在受限线程池中推进任务，并保证提交序列与串行完全一致。
func (e *Engine) schedule(ctx context.Context, workers int, mode Mode) error {
	if workers < 1 {
		workers = 1
	}
	if workers > e.Cfg.MaxWorkers {
		workers = e.Cfg.MaxWorkers
	}
	plan := e.plan()

	type result struct {
		order int
		seg   *Segment
		err   error
	}
	results := make([]result, len(plan))
	type slot struct{ idx int }

	workerSem := make(chan slot, workers)
	for i := 0; i < workers; i++ {
		workerSem <- slot{i}
	}
	// 全局重采样缓存预算（按采样点计），耗尽时任务在入口阻塞排队。
	resampleTokens := make(chan struct{}, e.Cfg.ResampleBudget/e.Cfg.BufferCap)
	if cap(resampleTokens) < 1 {
		resampleTokens = make(chan struct{}, 1)
	}
	budgetTokens := resampleTokens
	for i := 0; i < cap(budgetTokens); i++ {
		budgetTokens <- struct{}{}
	}

	var wg sync.WaitGroup
	var failMu sync.Mutex
	var firstErr error
	setErr := func(err error) {
		failMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		failMu.Unlock()
	}

loop:
	for order, u := range plan {
		if err := ctx.Err(); err != nil {
			break
		}
		seg := e.segments[u.channelID][u.segIdx]
		if seg.IsFrozen() {
			// 已恢复的分段：跳过计算，但保持计划位置与提交序列。
			results[order] = result{order: order, seg: seg}
			continue
		}
		select {
		case <-workerSem:
		case <-ctx.Done():
			break loop
		}
		// 预算获取：记录阻塞原因而非丢弃任务。
		select {
		case <-budgetTokens:
		default:
			e.log("budget_blocked", u.channelID, u.segIdx,
				"resample cache budget exhausted, task queued")
			select {
			case <-budgetTokens:
			case <-ctx.Done():
				workerSem <- slot{}
				setErr(ctx.Err())
				break loop
			}
		}
		wg.Add(1)
		go func(order int, u unitOfWork) {
			defer wg.Done()
			defer func() { workerSem <- slot{} }()
			defer func() { budgetTokens <- struct{}{} }()
			seg, err := e.processUnit(u.channelID, u.segIdx)
			results[order] = result{order: order, seg: seg, err: err}
			if err != nil {
				setErr(err)
			}
		}(order, u)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}

	// 确定性提交：严格按计划顺序提交，已冻结分段跳过实际写库。
	for order := range plan {
		seg := results[order].seg
		if seg == nil || seg.IsFrozen() {
			continue
		}
		if err := e.commit(seg, mode); err != nil {
			return err
		}
	}
	return nil
}

// commit 执行 pending 结果的状态迁移与串行原子落盘。
func (e *Engine) commit(seg *Segment, mode Mode) error {
	if err := seg.Transition(StateCommitted); err != nil {
		return err
	}
	e.jobLog = append(e.jobLog, seg.ChannelID+"#"+itoa(seg.Index))
	e.log("committed", seg.ChannelID, seg.Index, string(mode))
	if e.st == nil {
		return nil
	}
	driftJSON, err := store.MarshalDrift(seg.Drift)
	if err != nil {
		return err
	}
	rec := store.SegmentRecord{
		ChannelID:    seg.ChannelID,
		SegmentIndex: seg.Index,
		Start:        seg.Start,
		End:          seg.End,
		DriftJSON:    driftJSON,
		Output:       encodeF64(seg.Output),
		ContentHash:  seg.Hash,
		ParamVersion: e.Cfg.ParamVersion,
		AlgoVersion:  e.Cfg.AlgoVersion,
	}
	if _, err := e.st.CommitSegment(rec); err != nil {
		return err
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
