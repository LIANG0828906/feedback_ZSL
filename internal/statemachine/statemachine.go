// Package statemachine 实现逐帧四态状态机与提交冻结边界。
package statemachine

import (
	"spatialir/internal/model"
)

// legalTransitions 是唯一允许的迁移表；失败帧可重试回到待估计。
var legalTransitions = map[model.FrameStatus]map[model.FrameStatus]bool{
	model.StatusPending: {
		model.StatusEstimated: true,
		model.StatusFailed:    true,
	},
	model.StatusEstimated: {
		model.StatusCommitted: true,
		model.StatusFailed:    true,
		model.StatusPending:   true, // 重新估计
	},
	model.StatusFailed: {
		model.StatusPending: true, // 重试
	},
	model.StatusCommitted: {}, // 终态：冻结，禁止任何迁出
}

// Machine 维护全部帧的状态，非法迁移返回 E_STATE 且保持原状态不变。
type Machine struct {
	statuses []model.FrameStatus
}

func New(total int) *Machine {
	m := &Machine{statuses: make([]model.FrameStatus, total)}
	for i := range m.statuses {
		m.statuses[i] = model.StatusPending
	}
	return m
}

func (m *Machine) Status(frame int) model.FrameStatus {
	return m.statuses[frame]
}

// Transition 尝试迁移；非法时返回 E_STATE，原状态保持不变。
func (m *Machine) Transition(frame int, to model.FrameStatus) error {
	cur := m.statuses[frame]
	if !legalTransitions[cur][to] {
		return model.NewError(model.CodeState, "illegal transition "+string(cur)+"->"+string(to)).
			WithFrame(frame)
	}
	m.statuses[frame] = to
	return nil
}

// Progress 返回已提交帧数与总帧数。
func (m *Machine) Progress() (committed, total int) {
	for _, s := range m.statuses {
		if s == model.StatusCommitted {
			committed++
		}
	}
	return committed, len(m.statuses)
}

// IsFrozen 报告帧是否已提交冻结；重新估计只能作用于未提交帧。
func (m *Machine) IsFrozen(frame int) bool {
	return m.statuses[frame] == model.StatusCommitted
}
