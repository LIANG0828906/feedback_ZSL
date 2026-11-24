package prng

import "testing"

func TestDeterministic(t *testing.T) {
	a := New(TurnSeed(42, 7))
	b := New(TurnSeed(42, 7))
	for i := 0; i < 100; i++ {
		if a.Uint64() != b.Uint64() {
			t.Fatal("same seed must produce same stream")
		}
	}
	c := New(TurnSeed(42, 8))
	if c.Uint64() == a.Uint64() {
		t.Fatal("different turn must derive different stream")
	}
}

func TestChanceAndIntn(t *testing.T) {
	r := New(1)
	for i := 0; i < 50; i++ {
		n := r.Intn(4)
		if n < 0 || n >= 4 {
			t.Fatalf("Intn out of range: %d", n)
		}
	}
}
