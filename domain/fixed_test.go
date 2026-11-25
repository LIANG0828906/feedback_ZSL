package domain

import (
	"math"
	"testing"
)

func TestFixedWidthAndSaturation(t *testing.T) {
	if got := MaskWidth(0xffff, 8); got != 0xff {
		t.Fatalf("mask: got %#x", got)
	}
	if got := MaskWidth(0xffff, 64); got != 0xffff {
		t.Fatalf("64-bit mask changed value: %#x", got)
	}
	if got := HammingDistance(0, 0b1011, 8); got != 3 {
		t.Fatalf("hamming: got %d", got)
	}
	if got, overflow := SatAddInt64(math.MaxInt64-1, 2); !overflow || got != math.MaxInt64 {
		t.Fatalf("sat add: got %d overflow=%t", got, overflow)
	}
	if got, overflow := SatMulInt64(math.MaxInt64, 2); !overflow || got != math.MaxInt64 {
		t.Fatalf("sat mul: got %d overflow=%t", got, overflow)
	}
	if got, overflow, under := SatAddDelta(10, 5); overflow || under || got != 15 {
		t.Fatalf("delta add: %d %t %t", got, overflow, under)
	}
	if got, _, under := SatAddDelta(10, -11); !under || got != 0 {
		t.Fatalf("delta underflow: %d %t", got, under)
	}
	if got, overflow, _ := SatAddDelta(math.MaxInt64, 1); !overflow || got != math.MaxInt64 {
		t.Fatalf("delta overflow: %d %t", got, overflow)
	}
}

func TestDigestWidthParsing(t *testing.T) {
	v, err := ParseDigestHex("0xff", 8)
	if err != nil {
		t.Fatal(err)
	}
	if v != 0xff {
		t.Fatalf("got %#x", v)
	}
	if _, err := ParseDigestHex("0x1ff", 8); CodeOf(err) != CodeDigestInvalid {
		t.Fatalf("expected width error, got %v", err)
	}
	if _, err := ParseDigestHex("ff", 8); CodeOf(err) != CodeDigestInvalid {
		t.Fatalf("expected prefix error, got %v", err)
	}
}

func TestMix64IsDeterministic(t *testing.T) {
	a := Mix64(1, 2, 3)
	b := Mix64(1, 2, 3)
	c := Mix64(1, 2, 4)
	if a != b || a == c {
		t.Fatalf("mix determinism failed: %x %x %x", a, b, c)
	}
}
