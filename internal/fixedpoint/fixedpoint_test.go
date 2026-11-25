package fixedpoint

import "testing"

func TestKnownWeightShares(t *testing.T) {
	oneThird, err := Share(1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if oneThird != 21845 {
		t.Fatalf("1/3 Q16 = %d, want 21845", oneThird)
	}
	twoThirds, err := Share(2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if twoThirds != 43691 {
		t.Fatalf("2/3 Q16 = %d, want 43691", twoThirds)
	}
}

func TestPermilleConversionExact(t *testing.T) {
	got, err := PermilleToPermyriad(123)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1230 {
		t.Fatalf("123 permille = %d myriad, want 1230", got)
	}
}

func TestSaturatingWindow(t *testing.T) {
	const cap = int64(65535)
	got, overflow := SaturatingAddWindow(60000, 10000, cap)
	if !overflow || got != cap {
		t.Fatalf("got=%d overflow=%v", got, overflow)
	}
	if _, ok := CheckedAdd(1<<62, 1<<62); ok {
		t.Fatal("expected overflow detection")
	}
}
