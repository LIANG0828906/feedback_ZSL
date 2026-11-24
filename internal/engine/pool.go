package engine

import (
	"sync"

	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

// MatchJob 是一局回放的并行结算任务。每局内部严格串行，局间可并行。
type MatchJob struct {
	Replay    *model.Replay
	RawReplay []byte
	TruthHash string
	OpenDB    func() (*store.Store, error) // 每局独立 DB 文件，避免跨局写竞争
	Mode      RunMode
}

// RunMode 标识三条路径。
type RunMode string

const (
	ModeLive     RunMode = "live"
	ModeRecovery RunMode = "recovery"
	ModeFull     RunMode = "full"
)

// MatchOutcome 是单局结果。
type MatchOutcome struct {
	MatchID string
	Result  *Result
	Err     error
}

// Pool 是带最大并行度与每线程缓冲上限的工作线程池。
// 缓冲耗尽时任务投递阻塞排队，绝不静默丢弃。
type Pool struct {
	params    model.RunParams
	sem       chan struct{}
	wg        sync.WaitGroup
	collectMu sync.Mutex
	outcomes  []MatchOutcome
}

// NewPool 按参数构造线程池；并行度与缓冲容量做下限保护。
func NewPool(params model.RunParams) *Pool {
	maxP := params.MaxParallel
	if maxP < 1 {
		maxP = 1
	}
	buf := params.WorkerBufferCap
	if buf < 1 {
		buf = 1
	}
	return &Pool{
		params: params,
		// 带缓冲信号量同时承担“每线程状态缓冲上限”：容量=并行度*单线程缓冲。
		sem: make(chan struct{}, maxP*buf),
	}
}

// Submit 投递一局任务；信号量满时阻塞，体现背压而非丢弃。
func (p *Pool) Submit(job MatchJob) {
	p.sem <- struct{}{}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer func() { <-p.sem }()

		st, err := job.OpenDB()
		if err != nil {
			p.collect(MatchOutcome{MatchID: job.Replay.MatchID, Err: err})
			return
		}
		defer st.Close()

		runner := NewRunner(job.RawReplay, job.TruthHash, p.params)
		var res *Result
		switch job.Mode {
		case ModeRecovery:
			res, err = runner.RunRecovery(job.Replay, st)
		case ModeFull:
			res, err = runner.RunFullReplay(job.Replay, st)
		default:
			res, err = runner.RunLive(job.Replay, st)
		}
		p.collect(MatchOutcome{MatchID: job.Replay.MatchID, Result: res, Err: err})
	}()
}

func (p *Pool) collect(o MatchOutcome) {
	p.collectMu.Lock()
	p.outcomes = append(p.outcomes, o)
	p.collectMu.Unlock()
}

// Wait 等待全部任务完成并返回按对局 ID 排序的结果，保证可重复。
func (p *Pool) Wait() []MatchOutcome {
	p.wg.Wait()
	out := append([]MatchOutcome(nil), p.outcomes...)
	sortOutcomes(out)
	return out
}
