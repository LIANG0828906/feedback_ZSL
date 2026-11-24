package dsp

import (
	"math"
	"testing"
)

func TestSincShiftRecoversShiftedImpulse(t *testing.T) {
	// 在整数位置放单位脉冲，分数移位后应在邻近位置重建峰值。
	x := make([]float64, 128)
	x[64] = 1
	v := SincShift(x, 64.25, 16, 1.0)
	// 加窗 sinc 对窄脉冲有小幅滚降，主峰应显著且接近 1。
	if v < 0.75 || v > 1.05 {
		t.Fatalf("fractional shift peak=%.4f out of expected band", v)
	}
	if !InBounds(128, 100, 16) {
		t.Fatal("in-bounds position rejected")
	}
	if InBounds(128, 5, 16) {
		t.Fatal("near-edge position must be out of bounds")
	}
}

func TestSincTableMatchesAtInteger(t *testing.T) {
	x := make([]float64, 128)
	for i := range x {
		x[i] = math.Sin(float64(i) * 0.2)
	}
	tbl := NewSincTable(16, 1.0, 256)
	got := tbl.Shift(x, 70.0)
	if math.Abs(got-x[70]) > 1e-12 {
		t.Fatalf("integer-position table shift=%.12f want %.12f", got, x[70])
	}
}

func TestDetectMarkerLocalizesPeak(t *testing.T) {
	tpl := MarkerTemplate("pulse", 8)
	x := make([]float64, 256)
	center := 128
	for k := -8; k <= 8; k++ {
		x[center+k] += tpl[k+8] * 0.5
	}
	pos, peak, found := DetectMarker(x, tpl, center-20, center+20)
	if !found || peak <= 0 {
		t.Fatal("marker not detected")
	}
	if math.Abs(pos-float64(center)) > 0.25 {
		t.Fatalf("detected pos=%.3f want %d", pos, center)
	}
}

func TestHasClip(t *testing.T) {
	x := make([]float64, 64)
	x[30], x[31] = 0.97, 0.97
	idx, ok := HasClip(x, 0.95)
	if !ok || idx != 31 {
		t.Fatalf("clip detection failed ok=%v idx=%d", ok, idx)
	}
}
