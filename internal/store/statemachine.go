package store

import (
	"haptickernel/internal/validate"
)

// PacketState 是设备包在量化状态机中的状态。
type PacketState string

const (
	StatePending    PacketState = "pending"    // 待量化
	StateQuantizing PacketState = "quantizing" // 量化中
	StateSliced     PacketState = "sliced"     // 已切分
	StateCommitted  PacketState = "committed"  // 已提交（冻结）
	StateFailed     PacketState = "failed"     // 失败
)

// legalTransitions 声明唯一合法的迁移边。
var legalTransitions = map[PacketState]map[PacketState]bool{
	StatePending:    {StateQuantizing: true, StateFailed: true},
	StateQuantizing: {StateSliced: true, StateFailed: true, StatePending: true},
	StateSliced:     {StateCommitted: true, StateFailed: true},
	StateCommitted:  {},                   // 冻结：任何迁出都非法
	StateFailed:     {StatePending: true}, // 重试入口
}

// StateMachine 跟踪单个包的状态，非法迁移保持原状态并返回 E_STATE。
type StateMachine struct {
	state PacketState
}

func NewStateMachine() *StateMachine {
	return &StateMachine{state: StatePending}
}

func (s *StateMachine) State() PacketState {
	return s.state
}

// Transition 尝试迁移；非法迁移返回 E_STATE 且原状态不变。
func (s *StateMachine) Transition(next PacketState) error {
	if legalTransitions[s.state][next] {
		s.state = next
		return nil
	}
	return validate.NewBiz(validate.EState,
		"非法状态迁移: "+string(s.state)+" -> "+string(next),
		"state", -1, -1)
}
