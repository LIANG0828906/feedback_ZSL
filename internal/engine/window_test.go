package engine

import (
	"strings"
	"testing"

	"replaykernel/internal/model"
)

func TestBuildPlanAndOverlap(t *testing.T) {
	plan, err := BuildWindowPlan("m", 120, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Windows) != 3 {
		t.Fatalf("windows=%d", len(plan.Windows))
	}
	last := plan.Windows[2]
	if last.FromTurn != 100 || last.ToTurn != 120 {
		t.Fatalf("last window wrong: %+v", last)
	}
	committed, total := plan.Progress()
	if committed != 0 || total != 3 {
		t.Fatalf("progress wrong: %d/%d", committed, total)
	}

	// 标记首窗口已提交，再用不同划分构造计划，必须报重叠 E_STATE。
	plan.Windows[0].Status = model.WindowCommitted
	other := &WindowPlan{MatchID: "m", WindowSize: 60, Windows: []WindowState{
		{No: 0, FromTurn: 0, ToTurn: 60, Status: model.WindowPending},
	}}
	err = assertNoOverlap(plan, other)
	if !codeIs(err, model.EState) || !strings.Contains(err.Error(), "window=0") {
		t.Fatalf("overlap must be E_STATE with window id, got %v", err)
	}
}

func TestSchemaFieldPath(t *testing.T) {
	r, raw, _ := fixtureReplay(t)
	cp := clone(r)
	cp.Units[0].ID = ""
	bad, _ := jsonMarshal(cp)
	_, err := LoadReplay(bad)
	if !codeIs(err, model.ESchema) {
		t.Fatalf("want E_SCHEMA got %v", err)
	}
	ke, _ := model.AsKernelError(err)
	if !strings.HasPrefix(ke.Field, "units[") {
		t.Fatalf("field path missing: %+v", ke)
	}
	_ = raw
}
