package fixed

import (
	"errors"
	"math"
	"testing"

	"terrainkernel/internal/kerr"
)

func TestMulDivOverflow(t *testing.T) {
	_, err := MulDiv(math.MaxInt64, 10, 1, "height")
	if err == nil {
		t.Fatal("expected overflow")
	}
	var ke *kerr.Error
	if !errors.As(err, &ke) || ke.Code != kerr.CodeOverflow || ke.Path != "height" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDivRounded(t *testing.T) {
	got, err := MulDivRounded(2, 1, 3, "test", Nearest)
	if err != nil || got != 1 {
		t.Fatalf("nearest got %d err %v", got, err)
	}
	got, err = MulDivRounded(-2, 1, 3, "test", Nearest)
	if err != nil || got != -1 {
		t.Fatalf("negative nearest got %d err %v", got, err)
	}
}

func TestClampAbs(t *testing.T) {
	if Clamp(9, 1, 5) != 5 || Clamp(-1, 0, 2) != 0 {
		t.Fatal("clamp failed")
	}
	if Abs(math.MinInt64) != 0 || Abs(-42) != 42 {
		t.Fatal("abs failed")
	}
}
