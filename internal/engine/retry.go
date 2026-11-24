package engine

import (
	"errors"
	"sort"

	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

// RetryFailedWindow 在不污染其他窗口的前提下重试一个失败窗口。
// 状态机要求 failed -> pending -> running -> committed；回放内若仍带
// stage 注入标记则会再次失败，调用方应在真实重试前清除该标记（演示管线
// 通过“修复后的回放文件”验证重试成功）。
func (r *Runner) RetryFailedWindow(replay *model.Replay, st *store.Store, windowNo int) (*model.WindowReport, error) {
	decision := ChooseAlgorithm(replay.MatchID, len(replay.Turns), len(replay.Units), r.params)
	meta := r.buildMeta(replay, decision.Algorithm, decision.WindowSize)

	plan, err := BuildWindowPlan(replay.MatchID, len(replay.Turns), decision.WindowSize)
	if err != nil {
		return nil, err
	}
	if windowNo < 0 || windowNo >= len(plan.Windows) {
		return nil, model.NewError(model.EState, "window %d out of range", windowNo).WithWindow(windowNo)
	}

	// 从回放起点确定性重建到失败窗口之前，杜绝部分混用。
	rt := newMatchRuntime(replay)
	for w := 0; w < windowNo; w++ {
		if _, err := ExecuteWindow(rt, plan, w, decision.Algorithm); err != nil {
			return nil, err
		}
	}

	// 显式演练 failed -> pending 的合法重试迁移。
	failed := WindowState{No: windowNo, Status: model.WindowFailed}
	if err := failed.Transition(model.WindowPending); err != nil {
		return nil, err
	}
	plan.Windows[windowNo].Status = model.WindowPending
	if err := st.PurgeFailedWindow(replay.MatchID, windowNo); err != nil {
		return nil, err
	}

	report, err := ExecuteWindow(rt, plan, windowNo, decision.Algorithm)
	if err != nil {
		var ke *model.KernelError
		if errors.As(err, &ke) {
			_ = st.MarkFailed(replay.MatchID, windowNo, ke.Code)
		}
		return nil, err
	}

	prev := store.RootChain
	if windowNo > 0 {
		prev, err = r.chainAt(st, replay.MatchID, windowNo-1)
		if err != nil {
			return nil, err
		}
	}
	if _, err := st.CommitWindow(meta, report, prev); err != nil {
		return nil, err
	}
	return report, nil
}

func sortOutcomes(o []MatchOutcome) {
	sort.Slice(o, func(i, j int) bool { return o[i].MatchID < o[j].MatchID })
}
