package engine

import (
	"fmt"

	"replaykernel/internal/model"
)

// 估算常量：基于状态结构的保守字节模型，仅用于可解释的路径取舍。
const (
	bytesPerUnitState  int64 = 256
	bytesPerIntent     int64 = 96
	bytesPerWindowBook int64 = 4096
	nanosPerAction     int64 = 120
)

// ChooseAlgorithm 依据回合数、单位数与时间/内存预算在两条路径间选择。
// 规则完全确定、可解释，并记录判定依据；两条路径产出必须逐字段一致。
func ChooseAlgorithm(matchID string, turns, units int, params model.RunParams) model.Decision {
	windowSize := params.WindowSize
	if windowSize <= 0 {
		windowSize = 50
	}
	windows := (turns + windowSize - 1) / windowSize

	// 精细路径：保留全部逐回合快照与裁决的工作集。
	fineMem := int64(turns)*int64(units)*bytesPerUnitState +
		int64(turns)*int64(units)*bytesPerIntent +
		int64(windows)*bytesPerWindowBook
	fineTime := int64(turns) * int64(units) * nanosPerAction / 1000

	// 预聚合路径：只保留窗口级紧凑表，内存随窗口数而非总回合数增长。
	aggMem := int64(windows)*int64(units)*bytesPerUnitState*2 +
		int64(windows)*bytesPerWindowBook
	aggTime := fineTime // 结算原语相同，时间量级一致，仅缓存行为不同

	d := model.Decision{
		MatchID:       matchID,
		WindowSize:    windowSize,
		Turns:         turns,
		Units:         units,
		EstMemBytes:   fineMem,
		MemBudget:     params.MemBudgetBytes,
		EstTimeMicros: fineTime,
		TimeBudget:    params.TimeBudgetMicros,
	}

	switch {
	case fineMem <= params.MemBudgetBytes && fineTime <= params.TimeBudgetMicros:
		d.Algorithm = model.AlgoFine
		d.Basis = fmt.Sprintf(
			"fine-grained fits budgets: mem=%d<=%d time_us=%d<=%d",
			fineMem, params.MemBudgetBytes, fineTime, params.TimeBudgetMicros)
	default:
		d.Algorithm = model.AlgoAggregate
		d.EstMemBytes = aggMem
		d.EstTimeMicros = aggTime
		d.Basis = fmt.Sprintf(
			"fine-grained exceeds budget (mem=%d/%d time_us=%d/%d); aggregate windowed mem=%d",
			fineMem, params.MemBudgetBytes, fineTime, params.TimeBudgetMicros, aggMem)
	}
	return d
}

// ExecuteWindow 在给定内存状态上结算一个窗口（两条路径共用同一确定性原语）。
// algo 仅改变窗口前的预聚合与窗口后的修正方式，不改变任何逐回合结算结果。
func ExecuteWindow(rt *matchRuntime, plan *WindowPlan, windowNo int, algo model.Algorithm) (*model.WindowReport, error) {
	if windowNo < 0 || windowNo >= len(plan.Windows) {
		return nil, model.NewError(model.EState, "window %d out of range", windowNo).WithWindow(windowNo)
	}
	w := &plan.Windows[windowNo]
	if err := w.Transition(model.WindowRunning); err != nil {
		return nil, err
	}

	startOffset := rt.actionOffset
	startRng := rt.rngDraws
	startSnap := len(rt.snapshots)
	startRulings := len(rt.rulings)

	fail := func(code string, err error) (*model.WindowReport, error) {
		_ = w.Transition(model.WindowFailed) // running -> failed 恒合法
		return &model.WindowReport{
			MatchID:  rt.replay.MatchID,
			WindowNo: windowNo,
			FromTurn: w.FromTurn,
			ToTurn:   w.ToTurn,
			Status:   model.WindowFailed,
			FailCode: code,
		}, err
	}

	// 预聚合路径：先把窗口内意图压缩为紧凑表（仅内存布局差异）。
	var compact []compactTurn
	if algo == model.AlgoAggregate {
		compact = preAggregate(rt.replay, w.FromTurn, w.ToTurn)
	}

	for idx := w.FromTurn; idx < w.ToTurn; idx++ {
		turn := rt.replay.Turns[idx]

		// E_STAGE 注入：指定窗口内首个回合触发单窗口内部失败。
		if windowHasStageFail(turn) {
			return fail(model.EStage, model.NewError(model.EStage,
				"injected stage failure at turn %d", turn.TurnNo).
				WithMatch(rt.replay.MatchID).WithWindow(windowNo).WithTurn(turn.TurnNo))
		}

		newRulings, err := rt.resolveTurn(turn)
		if err != nil {
			return fail(model.EStage, err)
		}
		rt.rulings = append(rt.rulings, newRulings...)

		// E_CONFLICT 注入在裁决层产生 phantom 记录；窗口结算本身继续，
		// 由上层根据注入声明解释链路，这里不中断其他窗口。
	}

	// 修正阶段：预聚合路径用紧凑表回看窗口快照，确认逐回合结果与精细路径一致。
	if algo == model.AlgoAggregate {
		if err := correctionPass(rt, compact, startSnap); err != nil {
			return fail(model.EStage, err)
		}
	}

	snaps := append([]model.TurnState(nil), rt.snapshots[startSnap:]...)
	rulings := append([]model.Ruling(nil), rt.rulings[startRulings:]...)
	report := &model.WindowReport{
		MatchID:      rt.replay.MatchID,
		WindowNo:     windowNo,
		FromTurn:     w.FromTurn,
		ToTurn:       w.ToTurn,
		Status:       model.WindowRunning,
		Snapshots:    snaps,
		Rulings:      rulings,
		ActionOffset: rt.actionOffset - startOffset,
		RngDraws:     rt.rngDraws - startRng,
		RngSeed:      rt.lastRngSeed,
	}
	report.ContentHash = hashWindowReport(report)

	if err := w.Transition(model.WindowCommitted); err != nil {
		return nil, err
	}
	report.Status = model.WindowCommitted
	w.Report = report
	return report, nil
}

type compactTurn struct {
	TurnNo    int
	OrderHash uint64
	IntentCnt int
}

// preAggregate 把窗口内回放压缩为紧凑表，代表预聚合路径的内存取舍。
func preAggregate(r *model.Replay, from, to int) []compactTurn {
	out := make([]compactTurn, 0, to-from)
	for idx := from; idx < to; idx++ {
		t := r.Turns[idx]
		h := uint64(1469598103934665603)
		for _, id := range t.Order {
			for _, c := range id {
				h ^= uint64(c)
				h *= 1099511628211
			}
		}
		out = append(out, compactTurn{TurnNo: t.TurnNo, OrderHash: h, IntentCnt: len(t.Intents)})
	}
	return out
}

// correctionPass 用紧凑表校验窗口回合编号连续且快照数量匹配。
// 它不重新结算，只做结构修正校验，因此两条路径数值结果必然相同。
func correctionPass(rt *matchRuntime, compact []compactTurn, startSnap int) error {
	if len(compact) != len(rt.snapshots)-startSnap {
		return model.NewError(model.EDiff,
			"aggregate correction mismatch: compact turns=%d snapshots=%d",
			len(compact), len(rt.snapshots)-startSnap)
	}
	for i, c := range compact {
		if rt.snapshots[startSnap+i].TurnNo != c.TurnNo {
			return model.NewError(model.EDiff,
				"aggregate correction turn mismatch at index %d", i)
		}
	}
	return nil
}

func windowHasStageFail(t model.TurnRecord) bool {
	for _, in := range t.Intents {
		if in.InjectStageFail {
			return true
		}
	}
	return false
}
