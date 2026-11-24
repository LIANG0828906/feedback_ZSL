package fixed

import "testing"

func TestScaleToRoundModes(t *testing.T) {
	cases := []struct {
		value int64
		mode  RoundMode
		want  int64
	}{
		{10001, RoundFloor, 1000},
		{10001, RoundCeil, 1001},
		{10005, RoundNearest, 1001},
		{10004, RoundNearest, 1000},
	}
	for _, tc := range cases {
		got, err := ScaleTo(tc.value, WorkerScale, 1000, tc.mode)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("ScaleTo(%d,%s)=%d want %d", tc.value, tc.mode, got, tc.want)
		}
	}
}

func TestMulOverflow(t *testing.T) {
	if _, err := Mul(1<<62, 4); err != ErrOverflow {
		t.Fatalf("expected overflow, got %v", err)
	}
}
