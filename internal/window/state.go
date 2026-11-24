// Package window 实现帧窗口划分与四态状态机。
package window

import "audiobench/internal/model"

// 窗口四态。
const (
	StatePending    = "pending"    // 待提取
	StateExtracting = "extracting" // 提取中
	StateCommitted  = "committed"  // 已提交（冻结）
	StateFailed     = "failed"     // 失败（可重试）
)

// allowedTransitions 声明唯一合法的状态迁移表。
var allowedTransitions = map[string]map[string]bool{
	StatePending:    {StateExtracting: true, StateFailed: true},
	StateExtracting: {StateCommitted: true, StateFailed: true},
	StateFailed:     {StateExtracting: true},
	StateCommitted:  {}, // 终态：提交后冻结，任何迁移都被拒绝
}

// Window 一个帧窗口及其当前状态与冻结内容。
type Window struct {
	Index        int                 `json:"index"`
	StartFrame   int                 `json:"start_frame"`
	EndFrame     int                 `json:"end_frame"`
	State        string              `json:"state"`
	Fingerprints []model.Fingerprint `json:"fingerprints,omitempty"`
	Events       []model.Event       `json:"events,omitempty"`
	Params       model.Params        `json:"params"`
	FailReason   string              `json:"fail_reason,omitempty"`
}

// Plan 按固定窗口大小切分全部帧。
type Plan struct {
	WindowFrames int      `json:"window_frames"`
	TotalFrames  int      `json:"total_frames"`
	Windows      []Window `json:"windows"`
}

// BuildPlan 依据总帧数与窗口大小生成窗口序列（尾窗可能不足一窗）。
func BuildPlan(totalFrames, windowFrames int, p model.Params) *Plan {
	if windowFrames <= 0 {
		windowFrames = totalFrames
	}
	plan := &Plan{WindowFrames: windowFrames, TotalFrames: totalFrames}
	for start := 0; start < totalFrames; start += windowFrames {
		end := start + windowFrames
		if end > totalFrames {
			end = totalFrames
		}
		plan.Windows = append(plan.Windows, Window{
			Index: len(plan.Windows), StartFrame: start, EndFrame: end,
			State: StatePending, Params: p,
		})
	}
	return plan
}

// Transition 尝试状态迁移；非法迁移返回 E_STATE 且保持原状态不变。
func (w *Window) Transition(target string) error {
	if !allowedTransitions[w.State][target] {
		return model.NewErr(model.EState,
			"窗口 %d 非法状态迁移 %s -> %s", w.Index, w.State, target).
			With("window_index", w.Index).
			With("from", w.State).
			With("to", target)
	}
	w.State = target
	return nil
}

// Progress 以已提交窗口数/总窗口数表示进度。
func (p *Plan) Progress() (committed, total int) {
	for _, w := range p.Windows {
		if w.State == StateCommitted {
			committed++
		}
	}
	return committed, len(p.Windows)
}
