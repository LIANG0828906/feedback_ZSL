package fixedint

import "testing"

func TestMulOverflowSaturates(t *testing.T) {
	if got, ov := Mul(Max, 2); !ov || got != Max {
		t.Fatalf("positive overflow = %d ov=%v, want Max saturated", got, ov)
	}
	if got, ov := Mul(Min, 2); !ov || got != Min {
		t.Fatalf("negative overflow = %d ov=%v, want Min saturated", got, ov)
	}
	if got, ov := Mul(1000, 10); ov || got != 10000 {
		t.Fatalf("exact mul = %d ov=%v", got, ov)
	}
}

func TestScaleHalfAwayFromZero(t *testing.T) {
	cases := []struct {
		amount, num, den, want int64
	}{
		{1, 1, 2, 1},            // 0.5 rounds up
		{3, 1, 2, 2},            // 1.5 rounds up
		{1, 1, 3, 0},            // 0.33 rounds down
		{2, 1, 3, 1},            // 0.67 rounds up
		{8000, 125, 1000, 1000}, // 12.5% of 8000
	}
	for _, c := range cases {
		got, ov := Scale(c.amount, c.num, c.den)
		if ov || got != c.want {
			t.Fatalf("Scale(%d,%d,%d)=%d ov=%v want %d", c.amount, c.num, c.den, got, ov, c.want)
		}
	}
}

func TestParseRational(t *testing.T) {
	r, err := ParseRational("12.5%")
	if err != nil {
		t.Fatal(err)
	}
	if !r.IsPercent || r.Num != 125 || r.Den != 10 {
		t.Fatalf("unexpected rational %+v", r)
	}
	if _, err := ParseRational("abc"); err == nil {
		t.Fatal("expected syntax error")
	}
}
