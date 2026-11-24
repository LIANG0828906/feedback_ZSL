package engine

import (
	"errors"

	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

// Runner 在同一套确定性原语上驱动三条路径。
type Runner struct {
	params     model.RunParams
	meta       store.MetaRecord
	replayHash string
	truthHash  string
}

// NewRunner 构造运行器。rawReplay 用于计算内容哈希；truthHash 是真值指纹。
func NewRunner(rawReplay []byte, truthHash string, params model.RunParams) *Runner {
	return &Runner{
		params:     params,
		replayHash: HashReplay(rawReplay),
		truthHash:  truthHash,
	}
}

// RunLive 实时结算路径：边加载边按窗口提交，模拟无历史检查点的首次运行。
func (r *Runner) RunLive(replay *model.Replay, st *store.Store) (*Result, error) {
	return r.run(replay, st, false)
}

// RunRecovery 断点恢复路径：加载持久化检查点，校验通过则从断点继续，否则整体作废重算。
func (r *Runner) RunRecovery(replay *model.Replay, st *store.Store) (*Result, error) {
	return r.run(replay, st, true)
}

// RunFullReplay 一次性全量重放路径：忽略任何既有检查点，整体作废后从头一次性结算。
func (r *Runner) RunFullReplay(replay *model.Replay, st *store.Store) (*Result, error) {
	if err := st.Invalidate(replay.MatchID); err != nil {
		return nil, err
	}
	return r.run(replay, st, false)
}

// Result 汇总一次运行的产物（内存中仅用于返回摘要；比对必须读持久化快照）。
type Result struct {
	MatchID      string
	Decision     model.Decision
	Committed    int
	TotalWindows int
	FirstFailure *model.KernelError
	Recovered    bool
	Invalidated  bool
}

func (r *Runner) buildMeta(replay *model.Replay, algo model.Algorithm, windowSize int) store.MetaRecord {
	return store.MetaRecord{
		MatchID:      replay.MatchID,
		ReplayHash:   r.replayHash,
		TruthHash:    r.truthHash,
		ParamVersion: model.ParamVersion,
		AlgoVersion:  model.AlgoVersion,
		Seed:         replay.Seed,
		WindowSize:   windowSize,
		Algorithm:    algo,
	}
}

func (r *Runner) run(replay *model.Replay, st *store.Store, recovery bool) (*Result, error) {
	decision := ChooseAlgorithm(replay.MatchID, len(replay.Turns), len(replay.Units), r.params)
	meta := r.buildMeta(replay, decision.Algorithm, decision.WindowSize)

	res := &Result{MatchID: replay.MatchID, Decision: decision}

	plan, err := BuildWindowPlan(replay.MatchID, len(replay.Turns), decision.WindowSize)
	if err != nil {
		return nil, err
	}
	res.TotalWindows = len(plan.Windows)

	startWindow := 0
	if recovery {
		got, invalidated, err := r.restorePoint(replay, st, meta)
		if err != nil {
			return nil, err
		}
		res.Invalidated = invalidated
		if !invalidated {
			startWindow = got
			res.Recovered = got > 0
		}
	}

	if err := st.InitMatch(meta, decision); err != nil {
		// InitMatch 发现哈希/版本冲突时整体作废并重算。
		var ke *model.KernelError
		if errors.As(err, &ke) && ke.Code == model.EState {
			if err := st.Invalidate(replay.MatchID); err != nil {
				return nil, err
			}
			startWindow = 0
			res.Invalidated = true
			if err := st.InitMatch(meta, decision); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	// 确定性重建内存状态：始终从回放起点重放到 startWindow，
	// 绝不混用旧窗口内存结果；恢复路径的“继续”只体现在不再重复写库。
	rt := newMatchRuntime(replay)
	for w := 0; w < startWindow; w++ {
		if _, err := ExecuteWindow(rt, plan, w, decision.Algorithm); err != nil {
			return nil, err
		}
	}

	chain := store.RootChain
	if startWindow > 0 {
		chain, err = r.chainAt(st, replay.MatchID, startWindow-1)
		if err != nil {
			return nil, err
		}
	}

	for w := startWindow; w < len(plan.Windows); w++ {
		report, err := ExecuteWindow(rt, plan, w, decision.Algorithm)
		if err != nil {
			var ke *model.KernelError
			if errors.As(err, &ke) {
				if ke.Ruling != nil {
					if apErr := st.AppendRuling(replay.MatchID, w, *ke.Ruling); apErr != nil {
						return nil, apErr
					}
				}
				res.FirstFailure = ke
				if markErr := st.MarkFailed(replay.MatchID, w, ke.Code); markErr != nil {
					return nil, markErr
				}
				return res, ke
			}
			return nil, err
		}
		chain, err = st.CommitWindow(meta, report, chain)
		if err != nil {
			return nil, err
		}
		res.Committed++
	}
	res.Committed += startWindow
	return res, nil
}

// restorePoint 校验既有检查点并返回可恢复到的窗口数；不通过则返回 invalidated=true。
func (r *Runner) restorePoint(replay *model.Replay, st *store.Store, want store.MetaRecord) (int, bool, error) {
	meta, err := st.ReadMeta(replay.MatchID)
	if err != nil {
		var ke *model.KernelError
		if errors.As(err, &ke) && ke.Code == model.EState {
			return 0, true, st.Invalidate(replay.MatchID)
		}
		return 0, false, nil // 无历史检查点，视为全新运行
	}
	if meta.ReplayHash != want.ReplayHash ||
		meta.TruthHash != want.TruthHash ||
		meta.ParamVersion != want.ParamVersion ||
		meta.AlgoVersion != want.AlgoVersion ||
		meta.WindowSize != want.WindowSize ||
		meta.Algorithm != want.Algorithm {
		if err := st.Invalidate(replay.MatchID); err != nil {
			return 0, false, err
		}
		return 0, true, nil
	}
	windows, err := st.LoadWindows(replay.MatchID)
	if err != nil {
		return 0, false, err
	}
	committed := 0
	for _, w := range windows {
		if w.Status != string(model.WindowCommitted) {
			break
		}
		if w.ParamVersion != want.ParamVersion || w.AlgoVersion != want.AlgoVersion {
			if err := st.Invalidate(replay.MatchID); err != nil {
				return 0, false, err
			}
			return 0, true, nil
		}
		committed++
	}
	return committed, false, nil
}

func (r *Runner) chainAt(st *store.Store, matchID string, windowNo int) (string, error) {
	windows, err := st.LoadWindows(matchID)
	if err != nil {
		return "", err
	}
	for _, w := range windows {
		if w.WindowNo == windowNo {
			return w.ChainHash, nil
		}
	}
	return "", model.NewError(model.EState, "chain hash missing for window %d", windowNo).WithWindow(windowNo)
}
