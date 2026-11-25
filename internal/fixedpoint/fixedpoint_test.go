package fixedpoint

import "testing"

func TestCheckedMulOverflow(t *testing.T) {
	// Genuine 64-bit multiplication overflow.
	r := Mul(64, maxInt64, 2)
	if !r.Overflowed || !r.Saturated {
		t.Fatalf("expected overflow+saturation, got %+v", r)
	}
	if r.Value != maxInt64 {
		t.Fatalf("expected int64 max clamp, got %d", r.Value)
	}
	// In-width multiplication that exceeds a narrower declared width only
	// saturates at that width.
	narrow := Mul(32, 1<<30, 2) // 2^31 > int32 max
	if narrow.Overflowed || !narrow.Saturated || narrow.Value != (int64(1)<<31)-1 {
		t.Fatalf("expected narrow saturation, got %+v", narrow)
	}
}

func TestAddSubWithinWidth(t *testing.T) {
	if r := Add(32, 100, 200); r.Value != 300 || r.Overflowed {
		t.Fatalf("add wrong: %+v", r)
	}
	if r := Sub(32, 200, 250); r.Value != -50 || r.Overflowed {
		t.Fatalf("sub wrong: %+v", r)
	}
}

func TestDivByZeroAndRemainder(t *testing.T) {
	if r := Div(64, 10, 0); !r.Overflowed {
		t.Fatal("division by zero must overflow")
	}
	r := Div(64, 250, 100) // 250ms / 100ms -> 2 ticks, rem 50
	if r.Value != 2 || r.Remainder != 50 {
		t.Fatalf("tick conversion wrong: %+v", r)
	}
}

func TestBudgetScaleConversion(t *testing.T) {
	r, err := BudgetRatio(64, 500, ScalePermille) // 500‰ -> 5000‱
	if err != nil || r.Value != 5000 {
		t.Fatalf("permille convert: v=%d err=%v", r.Value, err)
	}
	if _, err := BudgetRatio(64, 1001, ScalePermille); err == nil {
		t.Fatal("out-of-range ‰ must error")
	}
	if _, err := BudgetRatio(64, 10001, ScalePerTenThou); err == nil {
		t.Fatal("out-of-range ‱ must error")
	}
}

func TestAccumulatorTolerance(t *testing.T) {
	acc := &Accumulator{Width: 64, Tolerance: 2}
	if err := acc.AddRemainder(1); err != nil {
		t.Fatal(err)
	}
	if err := acc.AddRemainder(1); err != nil {
		t.Fatal(err)
	}
	if !acc.WithinTolerance() {
		t.Fatal("expected within tolerance")
	}
	_ = acc.AddRemainder(1)
	if acc.WithinTolerance() {
		t.Fatal("expected tolerance breach")
	}
}

func TestAbsoluteFromRemaining(t *testing.T) {
	if r := AbsoluteFromRemaining(64, 3, 7); r.Value != 10 || r.Overflowed {
		t.Fatalf("remaining->absolute wrong: %+v", r)
	}
}
