package window

import (
	"testing"

	"audiobench/internal/model"
)

func TestStateMachineTransitions(t *testing.T) {
	p := model.DefaultParams()
	plan := BuildPlan(20, 8, p)
	if len(plan.Windows) != 3 {
		t.Fatalf("期望 3 个窗口，实际 %d", len(plan.Windows))
	}
	w := &plan.Windows[0]

	// 合法链路 pending → extracting → committed。
	if err := w.Transition(StateExtracting); err != nil {
		t.Fatalf("合法迁移被拒: %v", err)
	}
	if err := w.Transition(StateCommitted); err != nil {
		t.Fatalf("合法迁移被拒: %v", err)
	}
	// 提交后冻结，任何迁移都必须 E_STATE 且保持 committed。
	err := w.Transition(StateExtracting)
	if err == nil || model.ErrCode(err) != model.EState {
		t.Fatalf("期望 E_STATE，实际 %v", err)
	}
	if w.State != StateCommitted {
		t.Fatalf("非法迁移改变了原状态: %s", w.State)
	}
}

func TestFailedRetryAndProgress(t *testing.T) {
	plan := BuildPlan(16, 8, model.DefaultParams())
	w := &plan.Windows[1]
	if err := w.Transition(StateExtracting); err != nil {
		t.Fatal(err)
	}
	if err := w.Transition(StateFailed); err != nil {
		t.Fatal(err)
	}
	if err := w.Transition(StateExtracting); err != nil {
		t.Fatalf("失败窗口应可重试: %v", err)
	}
	plan.Windows[0].State = StateCommitted
	committed, total := plan.Progress()
	if committed != 1 || total != 2 {
		t.Fatalf("进度错误: %d/%d", committed, total)
	}
	// pending 不能直接 committed。
	bad := &plan.Windows[1]
	bad.State = StatePending
	if err := bad.Transition(StateCommitted); err == nil ||
		model.ErrCode(err) != model.EState {
		t.Fatalf("期望跳级迁移被 E_STATE 拒绝")
	}
}
