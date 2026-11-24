package engine

import (
	"replaykernel/internal/model"
)

// legalTransitions 声明窗口状态机唯一合法迁移表。
var legalTransitions = map[model.WindowStatus]map[model.WindowStatus]bool{
	model.WindowPending: {
		model.WindowRunning: true,
	},
	model.WindowRunning: {
		model.WindowCommitted: true,
		model.WindowFailed:    true,
	},
	model.WindowFailed: {
		// 失败窗口允许重试：重置回待结算，再重新结算。
		model.WindowPending: true,
	},
	model.WindowCommitted: {
		// 已提交窗口冻结，不存在任何迁出边。
	},
}

// WindowState 是单窗口的状态机实例。
type WindowState struct {
	No       int
	FromTurn int
	ToTurn   int
	Status   model.WindowStatus
	Report   *model.WindowReport
}

// Transition 尝试迁移状态；非法迁移返回 E_STATE 并保持原状态不变。
func (w *WindowState) Transition(next model.WindowStatus) error {
	if !legalTransitions[w.Status][next] {
		return model.NewError(model.EState,
			"illegal window transition: window=%d %s -> %s",
			w.No, w.Status, next).WithWindow(w.No)
	}
	w.Status = next
	return nil
}

// WindowPlan 描述一局回放的固定窗口划分。
type WindowPlan struct {
	MatchID    string
	WindowSize int
	Windows    []WindowState
}

// BuildWindowPlan 按固定窗口大小划分回合，窗口互不重叠且连续。
func BuildWindowPlan(matchID string, turnCount, windowSize int) (*WindowPlan, error) {
	if windowSize <= 0 {
		return nil, model.NewError(model.ESchema, "window size must be positive")
	}
	plan := &WindowPlan{MatchID: matchID, WindowSize: windowSize}
	for start, no := 0, 0; start < turnCount; start, no = start+windowSize, no+1 {
		end := start + windowSize
		if end > turnCount {
			end = turnCount
		}
		plan.Windows = append(plan.Windows, WindowState{
			No:       no,
			FromTurn: start, // 0-based 切片下标，对应回合号 start+1
			ToTurn:   end,
			Status:   model.WindowPending,
		})
	}
	return plan, nil
}

// assertNoOverlap 校验新窗口计划与既有已提交窗口不重叠，重叠返回 E_STATE。
func assertNoOverlap(existing *WindowPlan, next *WindowPlan) error {
	for _, ew := range existing.Windows {
		if ew.Status != model.WindowCommitted {
			continue
		}
		for _, nw := range next.Windows {
			if nw.No == ew.No && (nw.FromTurn != ew.FromTurn || nw.ToTurn != ew.ToTurn) {
				return model.NewError(model.EState,
					"overlapping window definition at window=%d: existing [%d,%d) new [%d,%d)",
					ew.No, ew.FromTurn, ew.ToTurn, nw.FromTurn, nw.ToTurn).
					WithWindow(ew.No)
			}
		}
	}
	return nil
}

// Progress 以已提交窗口数 / 总窗口数表示进度。
func (p *WindowPlan) Progress() (committed, total int) {
	for _, w := range p.Windows {
		if w.Status == model.WindowCommitted {
			committed++
		}
	}
	return committed, len(p.Windows)
}
