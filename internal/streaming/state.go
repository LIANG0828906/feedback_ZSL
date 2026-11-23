package streaming

import "occlbench/internal/core"

// State 流式窗口的五类状态。
type State string

const (
	StatePending   State = "pending"   // 待剔除
	StateCulling   State = "culling"   // 剔除中
	StateLoading   State = "loading"   // 加载中
	StateCommitted State = "committed" // 已提交
	StateFailed    State = "failed"    // 失败
)

// legalTransitions 定义唯一合法的状态迁移表。
var legalTransitions = map[State]map[State]bool{
	StatePending:   {StateCulling: true, StateFailed: true},
	StateCulling:   {StateLoading: true, StateFailed: true, StatePending: true},
	StateLoading:   {StateCommitted: true, StateFailed: true},
	StateCommitted: {},                                       // 终态：已提交窗口冻结，不允许再迁移。
	StateFailed:    {StateCulling: true, StatePending: true}, // 允许重试回到流程起点。
}

// CanTransition 判断迁移是否合法。
func CanTransition(from, to State) bool {
	return legalTransitions[from][to]
}

// Transition 执行状态迁移；非法迁移返回 E_STATE 且保持原状态。
func Transition(current, target State) (State, error) {
	if !CanTransition(current, target) {
		return current, core.NewError(core.EState, "illegal state transition",
			map[string]any{"from": string(current), "to": string(target)})
	}
	return target, nil
}
