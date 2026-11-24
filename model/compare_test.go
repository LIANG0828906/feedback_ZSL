package model

import "testing"

func TestCompareRunSetsLocatesFirstPixel(t *testing.T) {
	a := Frame{Index: 0, Width: 3, Height: 2, Channels: 4, TimeNS: 0,
		RGBA: make([]byte, 3*2*4), Hash: "a"}
	b := a.Clone()
	b.RGBA[4*4+1] = 9
	b.Hash = HashBytes(b.RGBA)
	expected := RunFrameSet{Frames: []Frame{a}, Commits: []int{0}}
	actual := RunFrameSet{Frames: []Frame{b}, Commits: []int{0}}
	mismatch := CompareRunSets(expected, actual, 1)
	if mismatch == nil {
		t.Fatal("expected mismatch")
	}
	if mismatch.WindowIndex != 0 || mismatch.Frame != 0 || mismatch.X != 1 || mismatch.Y != 1 {
		t.Fatalf("mismatch location=%+v", mismatch)
	}
}

func TestRenderContributionMasksHiddenParams(t *testing.T) {
	visible := EffectiveLayer{ID: "a", Visible: true, Opacity: 0.5}
	hiddenHigh := EffectiveLayer{ID: "a", Visible: false, Opacity: 0.9}
	hiddenLow := EffectiveLayer{ID: "a", Visible: false, Opacity: 0.1}
	if renderContribution(hiddenHigh) != renderContribution(hiddenLow) {
		t.Fatal("hidden opacity must not contribute to render hash")
	}
	if renderContribution(visible) == renderContribution(hiddenHigh) {
		t.Fatal("visible contribution was masked")
	}
}
