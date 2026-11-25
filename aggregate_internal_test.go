package audiofp

import "testing"

func TestGroupHitsRespectsGapTolerance(t *testing.T) {
	got := groupHits([]int{10, 0, 1, 3, 4, 9}, 1)
	if len(got) != 2 {
		t.Fatalf("expected two groups, got %v", got)
	}
	if len(got[0]) != 4 || got[0][0] != 0 || got[0][3] != 4 {
		t.Fatalf("expected 0,1,3,4 merged through one missing frame, got %v", got[0])
	}
	if len(got[1]) != 2 || got[1][0] != 9 || got[1][1] != 10 {
		t.Fatalf("expected 9,10 as second adjacent group, got %v", got[1])
	}
}

func TestQuantumStep(t *testing.T) {
	p := DefaultParams()
	if got := p.QuantumStep(); got != 256 {
		t.Fatalf("expected gcd(1024,256)=256, got %d", got)
	}
	p.FrameLength = 768
	p.FrameShift = 256
	if got := p.QuantumStep(); got != 256 {
		t.Fatalf("expected gcd(768,256)=256, got %d", got)
	}
}
