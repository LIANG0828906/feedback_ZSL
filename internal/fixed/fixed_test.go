package fixed

import "testing"

func TestMulDivRoundTrip(t *testing.T) {
	tr := NewTracer()
	a := FromInt(7)
	b := FromInt(13)
	got := Mul(a, b, "test.mul", tr)
	if got.Int() != 91 {
		t.Fatalf("7*16 = %d, want 91", got.Int())
	}
	back := Div(got, FromInt(13), "test.div", tr)
	if back.Int() != 7 {
		t.Fatalf("91/13 = %d, want 7", back.Int())
	}
}

func TestMulNegative(t *testing.T) {
	tr := NewTracer()
	got := Mul(FromInt(-6), FromInt(9), "neg", tr)
	if got.Int() != -54 {
		t.Fatalf("-6*9 = %d, want -54", got.Int())
	}
}

func TestSqrtKnownSquares(t *testing.T) {
	tr := NewTracer()
	for n := int64(1); n <= 100; n++ {
		got := Sqrt(FromInt(n*n), "sqrt", tr)
		if got.Int() != n {
			t.Fatalf("sqrt(%d^2) = %d, want %d", n, got.Int(), n)
		}
	}
}

func TestTruncationRecorded(t *testing.T) {
	tr := NewTracer()
	// 10/3 cannot be represented exactly in Q16.16.
	_ = Div(FromInt(10), FromInt(3), "frac", tr)
	if len(tr.Events) == 0 {
		t.Fatal("expected at least one truncation event")
	}
	if tr.Events[0].Location != "frac" {
		t.Fatalf("location = %q, want frac", tr.Events[0].Location)
	}
}

func TestSqrtNegativePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on negative sqrt")
		}
	}()
	_ = Sqrt(FromInt(-1), "neg", nil)
}
