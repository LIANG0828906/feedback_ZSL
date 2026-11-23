package stitch

import (
	"math"
	"sort"

	"stitchworkbench/internal/model"
)

type gridPoint struct {
	x, y float64
}

func refinePhotometric(a, b *imageWork, ew *edgeWork, threshold float64) bool {
	pts := targetGrid(b.image.Width, b.image.Height, 2400)
	if len(pts) < 200 {
		return false
	}
	h0 := ew.HAB.Normalized()
	base, n := correlationCost(h0, pts, a, b)
	if n < 150 || math.IsNaN(base) {
		return false
	}
	h, best := searchSimilarity(h0, pts, a, b, ew, base)
	if best >= base-1e-5 {
		return false
	}
	if !h.Finite() {
		return false
	}
	mask := make([]bool, len(ew.matches))
	var residuals []float64
	inliers := 0
	for i, m := range ew.matches {
		x, y := h.Apply(m.Source.X, m.Source.Y)
		d := math.Hypot(x-m.Target.X, y-m.Target.Y)
		if d <= threshold {
			mask[i] = true
			inliers++
		}
		residuals = append(residuals, d)
	}
	if inliers < 8 {
		return false
	}
	var inRes []float64
	for i, ok := range mask {
		if ok {
			inRes = append(inRes, residuals[i])
		}
	}
	sort.Float64s(inRes)
	med := inRes[len(inRes)/2]
	if len(inRes)%2 == 0 {
		med = (inRes[len(inRes)/2-1] + inRes[len(inRes)/2]) / 2
	}
	ew.HAB = h
	ew.inliers = mask
	ew.inlierN = inliers
	ew.residual = med
	return true
}

func searchSimilarity(start model.Matrix3, pts []gridPoint, a, b *imageWork, ew *edgeWork, base float64) (model.Matrix3, float64) {
	theta0 := math.Atan2(start[3]-start[1], start[0]+start[4])
	scale0 := 0.5 * math.Hypot(start[0]+start[4], start[3]-start[1])
	src := make([]model.Point, 0)
	dst := make([]model.Point, 0)
	for i, m := range ew.matches {
		if ew.inliers != nil && (i >= len(ew.inliers) || !ew.inliers[i]) {
			continue
		}
		src = append(src, model.Point{X: m.Source.X, Y: m.Source.Y})
		dst = append(dst, model.Point{X: m.Target.X, Y: m.Target.Y})
	}
	if len(src) < 10 {
		return start, base
	}
	build := func(theta, scale, tx, ty float64) model.Matrix3 {
		c, s := math.Cos(theta), math.Sin(theta)
		return model.Matrix3{scale * c, -scale * s, tx, scale * s, scale * c, ty, 0, 0, 1}.Normalized()
	}
	translationFor := func(theta, scale float64) (float64, float64) {
		c, s := math.Cos(theta), math.Sin(theta)
		var xs, ys []float64
		for i := range src {
			xs = append(xs, dst[i].X-(scale*c*src[i].X-scale*s*src[i].Y))
			ys = append(ys, dst[i].Y-(scale*s*src[i].X+scale*c*src[i].Y))
		}
		sort.Float64s(xs)
		sort.Float64s(ys)
		return xs[len(xs)/2], ys[len(ys)/2]
	}
	bestH, bestCost := start, base
	eval := func(theta, scale float64) (model.Matrix3, float64) {
		tx, ty := translationFor(theta, scale)
		h := build(theta, scale, tx, ty)
		c, n := correlationCost(h, pts, a, b)
		if n < 150 || math.IsNaN(c) {
			return h, math.Inf(1)
		}
		return h, c
	}
	// Coarse, then fine grid. The bounds cover the slight camera scale and
	// rotation drift observed between local screenshots.
	bestTheta, bestScale := theta0, scale0
	for _, radius := range [][2]float64{{0.024, 0.012}, {0.003, 0.002}} {
		thetaStep, scaleStep := radius[0]/8, radius[1]/6
		centerTheta, centerScale := bestTheta, bestScale
		for it := -8; it <= 8; it++ {
			for si := -6; si <= 6; si++ {
				th := centerTheta + float64(it)*thetaStep
				sc := centerScale + float64(si)*scaleStep
				if sc < 0.3 {
					continue
				}
				h, cost := eval(th, sc)
				if cost < bestCost {
					bestH, bestCost = h, cost
					bestTheta, bestScale = th, sc
				}
			}
		}
	}
	// Local translation polish.
	tx, ty := translationFor(bestTheta, bestScale)
	for _, step := range []float64{1.0, 0.5, 0.25} {
		improved := false
		for _, dv := range [][2]float64{{step, 0}, {-step, 0}, {0, step}, {0, -step}} {
			h := build(bestTheta, bestScale, tx+dv[0], ty+dv[1])
			c, n := correlationCost(h, pts, a, b)
			if n >= 150 && c < bestCost-1e-8 {
				bestH, bestCost = h, c
				tx, ty = tx+dv[0], ty+dv[1]
				improved = true
			}
		}
		if !improved && step < 0.5 {
			break
		}
	}
	return bestH, bestCost
}

func targetGrid(w, h int, target int) []gridPoint {
	if w <= 0 || h <= 0 {
		return nil
	}
	step := math.Sqrt(float64(w*h) / float64(target))
	if step < 3 {
		step = 3
	}
	var out []gridPoint
	margin := 2.0
	for y := margin; y < float64(h)-margin; y += step {
		for x := margin; x < float64(w)-margin; x += step {
			out = append(out, gridPoint{x: x, y: y})
		}
	}
	return out
}

func correlationCost(h model.Matrix3, pts []gridPoint, a, b *imageWork) (float64, int) {
	inv, ok := h.Inverse()
	if !ok {
		return math.Inf(1), 0
	}
	sf, tf := float64(a.pixelScale), float64(b.pixelScale)
	var n, sa, sb, saa, sbb, sab float64
	for _, q := range pts {
		px, py := inv.Apply(q.x, q.y)
		if px < 1 || py < 1 || px >= float64(a.gw-1) || py >= float64(a.gh-1) {
			continue
		}
		va := bilinearGray(a.gray, a.gw, a.gh, px/sf, py/sf)
		vb := bilinearGray(b.gray, b.gw, b.gh, q.x/tf, q.y/tf)
		n++
		sa += va
		sb += vb
		saa += va * va
		sbb += vb * vb
		sab += va * vb
	}
	if n < 2 {
		return math.Inf(1), 0
	}
	cov := sab/n - (sa/n)*(sb/n)
	va := saa/n - (sa/n)*(sa/n)
	vb := sbb/n - (sb/n)*(sb/n)
	if va < 1e-9 || vb < 1e-9 {
		return 2, int(n)
	}
	corr := cov / math.Sqrt(va*vb)
	if corr > 1 {
		corr = 1
	}
	if corr < -1 {
		corr = -1
	}
	return 1 - corr, int(n)
}

func bilinearGray(pix []float32, w, h int, x, y float64) float64 {
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = 0
	}
	if x > float64(w-1) {
		x = float64(w - 1)
	}
	if y > float64(h-1) {
		y = float64(h - 1)
	}
	x0, y0 := int(x), int(y)
	x1, y1 := x0+1, y0+1
	if x1 >= w {
		x1 = w - 1
	}
	if y1 >= h {
		y1 = h - 1
	}
	fx, fy := x-float64(x0), y-float64(y0)
	v0 := float64(pix[y0*w+x0])*(1-fx) + float64(pix[y0*w+x1])*fx
	v1 := float64(pix[y1*w+x0])*(1-fx) + float64(pix[y1*w+x1])*fx
	return v0*(1-fy) + v1*fy
}
