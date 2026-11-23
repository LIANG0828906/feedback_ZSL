package homography

import (
	"math"
	"math/rand"
	"testing"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
)

func TestEstimateExact(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	truth := model.Matrix3{1.02, -0.04, 15, 0.03, 0.98, -8, 1e-5, -2e-5, 1}
	var pts []model.PointPair
	for i := 0; i < 100; i++ {
		s := model.Point{X: rng.Float64()*600 + 20, Y: rng.Float64()*400 + 20}
		x, y := truth.Apply(s.X, s.Y)
		tx, ty := x+(rng.Float64()*2-1)*0.4, y+(rng.Float64()*2-1)*0.4
		pts = append(pts, model.PointPair{Source: s, Target: model.Point{X: tx, Y: ty}})
	}
	h, cond, err := dlt(pts)
	t.Logf("dlt h=%v cond=%g err=%v diff=%v", h, cond, err, h.MaxAbsDiff(truth))
	res, err := Estimate(pts, 2.0, 1000, 1e9)
	if err != nil {
		t.Fatal(err)
	}
	if res.InlierCount < 80 {
		t.Fatalf("inliers=%d", res.InlierCount)
	}
	t.Logf("h=%v diff=%v cond=%g", res.H, res.H.MaxAbsDiff(truth), res.Condition)
}

func TestResidualMonotonicAndDegenerate(t *testing.T) {
	rng := rand.New(rand.NewSource(19))
	truth := model.Matrix3{1, -0.03, 22, 0.03, 1, -7, 0, 0, 1}
	var pts []model.PointPair
	for i := 0; i < 120; i++ {
		p := model.Point{X: 10 + rng.Float64()*500, Y: 10 + rng.Float64()*300}
		x, y := truth.Apply(p.X, p.Y)
		// Deterministic bounded noise plus a few outliers.
		x += (rng.Float64()*2 - 1) * 1.1
		y += (rng.Float64()*2 - 1) * 1.1
		if i%31 == 0 {
			x += 80
		}
		pts = append(pts, model.PointPair{Source: p, Target: model.Point{X: x, Y: y}})
	}
	prev := math.Inf(1)
	for _, threshold := range []float64{3.0, 2.0, 1.2, 0.8} {
		res, err := Estimate(pts, threshold, 2500, 1e9)
		if err != nil {
			t.Fatal(err)
		}
		if res.ResidualMedian > prev+1e-9 {
			t.Fatalf("residual is not monotonic: threshold %.1f median %.4f previous %.4f", threshold, res.ResidualMedian, prev)
		}
		prev = res.ResidualMedian
	}
	var line []model.PointPair
	for i := 0; i < 20; i++ {
		line = append(line, model.PointPair{Source: model.Point{X: float64(i), Y: 2 * float64(i)}, Target: model.Point{X: float64(i), Y: float64(i)}})
	}
	_, err := Estimate(line, 1, 500, 1e9)
	if err == nil || errs.As(err).Code != errs.EHomo {
		t.Fatalf("expected E_HOMO, got %v", err)
	}
	var narrow []model.PointPair
	for i := 0; i < 90; i++ {
		x := 20 + float64(i)*5
		y := 100 + x*0.01 + (rng.Float64()-0.5)*4
		narrow = append(narrow, model.PointPair{Source: model.Point{X: x, Y: y}, Target: model.Point{X: x * 1.001, Y: y * 0.999}})
	}
	_, err = Estimate(narrow, 2, 1200, 10)
	if err == nil || errs.As(err).Code != errs.EHomo {
		t.Fatalf("expected condition E_HOMO, got %v", err)
	}
	if errs.As(err).Actual == nil {
		t.Fatalf("condition error must expose measured value: %#v", errs.As(err))
	}
}
