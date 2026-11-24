package fixed

import "testing"

func TestMulDivRoundIsHalfAwayFromZeroAndKeepsResidual(t *testing.T) {
	q, rem, err := MulDivRound(1, 1, 2)
	if err != nil || q != 1 || rem != -1 {
		t.Fatalf("positive half rounding: q=%d rem=%d err=%v", q, rem, err)
	}
	q, rem, err = MulDivRound(-1, 1, 2)
	if err != nil || q != -1 || rem != 1 {
		t.Fatalf("negative half rounding: q=%d rem=%d err=%v", q, rem, err)
	}
	q, rem, err = MulDivRound(7, 3, 5)
	if err != nil || q != 4 || rem != 1 {
		t.Fatalf("general rounding: q=%d rem=%d err=%v", q, rem, err)
	}
}

func TestDotShiftRoundKeepsExactResidual(t *testing.T) {
	q, rem, err := DotShiftRound(3, 0, 0, 1, 0, 0, 1)
	if err != nil || q != 2 || rem != -1 {
		t.Fatalf("dot shift rounding: q=%d rem=%d err=%v", q, rem, err)
	}
}
