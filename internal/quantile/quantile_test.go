package quantile

import (
	"testing"

	"edgerefine/internal/errs"
)

func TestLevelDeterministicAndInvalidQuantile(t *testing.T) {
	h := make([]int, 256)
	for i := 0; i < 128; i++ {
		h[i] = 4
	}
	for i := 128; i < 256; i++ {
		h[i] = 8
	}
	a, err := Level(h, .25)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Level(h, .25)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("quantile not bitwise deterministic: %d != %d", a, b)
	}
	if _, err := Estimate(h, .8, .2); err == nil {
		t.Fatal("expected invalid quantile ordering/range error")
	} else if ee, ok := errs.As(err); !ok || ee.Code != errs.EPara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	if _, err := Estimate([]int{1, 2}, .1, .9); err == nil {
		t.Fatal("expected histogram bit depth mismatch E_PARA")
	}
}

func TestTighteningMovesClassEstimatesInward(t *testing.T) {
	h := make([]int, 256)
	for i := 30; i < 70; i++ {
		h[i] = 10
	}
	for i := 180; i < 230; i++ {
		h[i] = 10
	}
	bgQ, fgQ := .05, .95
	var lastBG, lastFG int
	for step := 0; step < 8; step++ {
		est, err := Estimate(h, bgQ, fgQ)
		if err != nil {
			t.Fatal(err)
		}
		if step > 0 {
			if est.BGLevel < lastBG {
				t.Fatalf("bg level not nondecreasing: %d < %d", est.BGLevel, lastBG)
			}
			if est.FGLevel > lastFG {
				t.Fatalf("fg level not nonincreasing: %d > %d", est.FGLevel, lastFG)
			}
			if est.FGLevel-est.BGLevel > lastFG-lastBG {
				t.Fatal("quantile interval did not tighten")
			}
		}
		lastBG, lastFG = est.BGLevel, est.FGLevel
		bgQ, fgQ = TightenedPair(bgQ, fgQ, .04)
	}
}
