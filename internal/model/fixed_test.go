package model

import "testing"

func TestAddCheckedOverflow(t *testing.T) {
	if _, err := AddChecked(MaxInt64, 1); err != ErrOverflow {
		t.Fatalf("expected overflow, got %v", err)
	}
	if _, err := AddChecked(MinInt64, -1); err != ErrOverflow {
		t.Fatalf("expected negative overflow, got %v", err)
	}
	v, err := AddChecked(SafeLimit, -SafeLimit)
	if err != nil || v != 0 {
		t.Fatalf("unexpected: v=%d err=%v", v, err)
	}
}

func TestScaleMilli(t *testing.T) {
	// 10 单位 * 1.5 = 15 单位（milli 定点）。
	got, err := ScaleMilli(10_000, 1500)
	if err != nil || got != 15_000 {
		t.Fatalf("got=%d err=%v", got, err)
	}
	if _, err := ScaleMilli(MaxInt64, 2); err != ErrOverflow {
		t.Fatalf("expected overflow, got %v", err)
	}
}

func TestClampAndSafeLimit(t *testing.T) {
	if Clamp(20, 0, 10) != 10 || Clamp(-1, 0, 10) != 0 {
		t.Fatal("clamp boundary failed")
	}
	if !WithinSafeLimit(SafeLimit) || WithinSafeLimit(SafeLimit+1) {
		t.Fatal("safe limit check failed")
	}
}
