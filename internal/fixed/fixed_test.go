package fixed

import (
	"errors"
	"testing"

	"skillsched/internal/errx"
)

func TestScalePermillePermyriadRoundTrip(t *testing.T) {
	tr := &Trace{}
	// 万分比 3500 -> 千分比 350 -> 回万分比 3500，完全一致（10 的倍数无损）。
	perm, err := FromInternal(tr, "cdr", 3500, Permille, RoundDown)
	if err != nil || perm != 350 {
		t.Fatalf("perm=%d err=%v", perm, err)
	}
	back, err := ToInternal(tr, "cdr", perm, Permille, RoundDown)
	if err != nil || back != 3500 {
		t.Fatalf("back=%d err=%v", back, err)
	}
}

func TestScaleTruncationResidualRecorded(t *testing.T) {
	tr := &Trace{}
	// 万分比 3555 -> 千分比向下取整 = 355，记录残差。
	perm, err := FromInternal(tr, "x", 3555, Permille, RoundDown)
	if err != nil || perm != 355 {
		t.Fatalf("perm=%d err=%v", perm, err)
	}
	if len(tr.Items) == 0 {
		t.Fatal("expected truncation record")
	}
	if tr.Items[0].Remainder != 5000 {
		t.Fatalf("remainder=%d want 5000", tr.Items[0].Remainder)
	}
	// RoundUp 方向：3555/10 -> 356。
	up, err := FromInternal(&Trace{}, "x", 3555, Permille, RoundUp)
	if err != nil || up != 356 {
		t.Fatalf("up=%d err=%v", up, err)
	}
}

func TestTickMsRoundTrip(t *testing.T) {
	tr := &Trace{}
	ms, err := TicksToMs(tr, "cd", 3)
	if err != nil || ms != 300 {
		t.Fatalf("ms=%d err=%v", ms, err)
	}
	ticks, err := MsToTicks(tr, "cd", ms, RoundDown)
	if err != nil || ticks != 3 {
		t.Fatalf("ticks=%d err=%v", ticks, err)
	}
	// 非整 tick 的毫秒向下取整并记录残差。
	tr2 := &Trace{}
	t2, err := MsToTicks(tr2, "cd", 250, RoundDown)
	if err != nil || t2 != 2 || len(tr2.Items) != 1 || tr2.Items[0].Remainder != 50 {
		t.Fatalf("t2=%d tr=%v err=%v", t2, tr2.Items, err)
	}
}

func TestOverflowReportsOperands(t *testing.T) {
	big := int64(1 << 62)
	_, err := TicksToMs(nil, "f", big)
	if errx.CodeOf(err) != errx.EOvf {
		t.Fatalf("want E_OVF got %v", err)
	}
	var de *errx.Error
	if !errors.As(err, &de) {
		t.Fatal("not structured error")
	}
	if de.Fields["operand_a"] == "" || de.Fields["operand_b"] == "" || de.Fields["field"] != "f" {
		t.Fatalf("missing operands/field: %+v", de.Fields)
	}
}

func TestSqrtNegativeIsEval(t *testing.T) {
	if _, err := Sqrt("n", -4); errx.CodeOf(err) != errx.EEval {
		t.Fatalf("want E_EVAL got %v", err)
	}
	vals := map[int64]int64{0: 0, 1: 1, 4: 2, 15: 3, 16: 4, 100: 10, 101: 10}
	for in, want := range vals {
		got, err := Sqrt("n", in)
		if err != nil || got != want {
			t.Fatalf("sqrt(%d)=%d want %d err=%v", in, got, want, err)
		}
	}
}
