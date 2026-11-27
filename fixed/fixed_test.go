package fixed

import "testing"

func TestMulDivDeclaredWidthSaturates(t *testing.T) {
	r, err := MulDiv(100, 100, 1, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Overflow || !r.Saturated || r.Value != 255 {
		t.Fatalf("expected saturation at 255, got %+v", r)
	}
	r, err = MulDiv(12, 3, 4, 32)
	if err != nil {
		t.Fatal(err)
	}
	if r.Value != 9 || r.Overflow {
		t.Fatalf("expected exact 9, got %+v", r)
	}
}

func TestAddSaturates(t *testing.T) {
	r, err := Add(250, 20, 8)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Overflow || r.Value != 255 {
		t.Fatalf("expected 255 saturation, got %+v", r)
	}
}

func TestInvalidFixedOperands(t *testing.T) {
	if _, err := MulDiv(1, 2, 0, 32); err == nil {
		t.Fatal("expected divide-by-zero error")
	}
	if _, err := Add(-1, 2, 32); err == nil {
		t.Fatal("expected negative operand error")
	}
}
