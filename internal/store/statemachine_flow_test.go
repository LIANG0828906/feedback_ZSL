package store

import "testing"

func TestStateMachineFullHappyPath(t *testing.T) {
	sm := NewStateMachine()
	steps := []PacketState{StateQuantizing, StateSliced, StateCommitted}
	for _, next := range steps {
		if err := sm.Transition(next); err != nil {
			t.Fatalf("合法迁移被拒绝: %v", err)
		}
	}
	if sm.State() != StateCommitted {
		t.Fatalf("最终状态错误: %s", sm.State())
	}

	// 失败后允许回到待量化（重试入口）。
	failing := NewStateMachine()
	if err := failing.Transition(StateQuantizing); err != nil {
		t.Fatal(err)
	}
	if err := failing.Transition(StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := failing.Transition(StatePending); err != nil {
		t.Fatalf("失败后应允许重试回到待量化: %v", err)
	}
}

func TestIllegalTransitionKeepsState(t *testing.T) {
	sm := NewStateMachine()
	// 待量化不能直接跳到已切分。
	err := sm.Transition(StateSliced)
	if err == nil {
		t.Fatal("期望非法迁移报错")
	}
	if sm.State() != StatePending {
		t.Fatalf("非法迁移后状态被改变: %s", sm.State())
	}
}
