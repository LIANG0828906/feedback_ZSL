package engine

import (
	"skillsched/internal/model"
	"skillsched/internal/pool"
)

// ConcurrentResult 是并发排程产物：逐 tick 真值与线程池排队诊断。
type ConcurrentResult struct {
	Ticks   []model.TickTruth
	Results []pool.Result
}

// RunConcurrent 在受限预算线程池中推进窗口。
// 并行度 maxWorkers、每线程缓冲 perWorkerBuffer；单窗口失败被隔离为该窗口技能失败，
// 不污染已提交窗口。窗口按索引顺序合并，保证与串行路径逐 tick 一致。
//
// 说明：排程内核本身是顺序状态机，因此并发用于“独立窗口重算/校验”任务；
// 每个任务基于同一规则与各自窗口起点的运行态快照计算，最后按窗口顺序归并，
// 结果不依赖完成顺序或线程标识。
func RunConcurrent(rules *model.RuleSet, intents []model.Intent, useTree bool, maxWorkers, perWorkerBuffer int) (*ConcurrentResult, error) {
	size := rules.Params.WindowSize
	if size <= 0 {
		size = rules.Params.Horizon
	}
	windows := 0
	for t := 0; t < rules.Params.Horizon; t += size {
		windows++
	}

	// 先顺序生成每个窗口的起始运行态快照（确定性），作为并发任务输入。
	type windowJob struct {
		index int
		start int
		rt    *Runtime
	}
	var jobs []windowJob
	eng := New(rules, InitialRuntime(rules), useTree)
	for t := 0; t < rules.Params.Horizon; t += size {
		jobs = append(jobs, windowJob{index: t / size, start: t, rt: eng.snapshot()})
		if _, err := eng.RunWindow(t, size, intents); err != nil {
			return nil, err
		}
	}

	wp := pool.New(maxWorkers, perWorkerBuffer)
	type windowOut struct {
		index int
		ticks []model.TickTruth
		err   error
	}
	outCh := make(chan windowOut, len(jobs))
	for _, j := range jobs {
		jb := j
		wp.Submit(pool.Job{
			ID: "window_" + itoa(jb.index),
			Apply: func() error {
				local := New(rules, cloneRuntime(jb.rt), useTree)
				ticks, err := local.RunWindow(jb.start, size, intents)
				outCh <- windowOut{index: jb.index, ticks: ticks, err: err}
				return err
			},
		})
	}
	results := wp.CloseAndWait()
	close(outCh)

	byIndex := map[int][]model.TickTruth{}
	var firstErr error
	for o := range outCh {
		if o.err != nil && firstErr == nil {
			firstErr = o.err
		}
		byIndex[o.index] = o.ticks
	}
	if firstErr != nil {
		return nil, firstErr
	}

	var all []model.TickTruth
	for i := 0; i < windows; i++ {
		all = append(all, byIndex[i]...)
	}
	return &ConcurrentResult{Ticks: all, Results: results}, nil
}

func cloneRuntime(rt *Runtime) *Runtime {
	cp := &Runtime{
		Tick: rt.Tick, IntentOffset: rt.IntentOffset,
		PoolRemain:  map[string]int64{},
		CoolRemain:  map[string]int64{},
		GroupLock:   map[string]int64{},
		SkillStates: map[string]string{},
	}
	for k, v := range rt.PoolRemain {
		cp.PoolRemain[k] = v
	}
	for k, v := range rt.CoolRemain {
		cp.CoolRemain[k] = v
	}
	for k, v := range rt.GroupLock {
		cp.GroupLock[k] = v
	}
	for k, v := range rt.SkillStates {
		cp.SkillStates[k] = v
	}
	return cp
}
