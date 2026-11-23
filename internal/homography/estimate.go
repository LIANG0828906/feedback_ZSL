package homography

import (
	"math"
	"math/rand"
	"sort"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
)

type Result struct {
	H              model.Matrix3
	Inliers        []bool
	InlierCount    int
	ResidualMedian float64
	Residuals      []float64
	Condition      float64
	Iterations     int
}

func Estimate(points []model.PointPair, threshold float64, iterations int, maxCondition float64) (*Result, error) {
	if len(points) < 4 {
		return nil, &errs.Error{Code: errs.EHomo, Message: "not enough correspondences for homography", Actual: len(points), Threshold: 4}
	}
	if threshold <= 0 || threshold > 100 {
		return nil, errs.Field(errs.EPara, "reprojection threshold must be in (0,100]", "ransac_threshold")
	}
	if iterations < 20 || iterations > 200000 {
		return nil, errs.Field(errs.EPara, "RANSAC iterations out of range [20,200000]", "ransac_iterations")
	}
	if maxCondition < 10 || math.IsInf(maxCondition, 0) || math.IsNaN(maxCondition) {
		return nil, errs.Field(errs.EPara, "homography condition limit must be finite and >= 10", "homography_max_cond")
	}
	rng := rand.New(rand.NewSource(seedFromPoints(points)))
	bestInliers := -1
	var best model.Matrix3
	var bestMask []bool
	for iter := 0; iter < iterations; iter++ {
		idx := randomSample(rng, len(points), 4)
		sample := make([]model.PointPair, 4)
		for i, k := range idx {
			sample[i] = points[k]
		}
		h, _, err := dlt(sample)
		if err != nil {
			continue
		}
		mask, _ := classify(points, h, threshold)
		n := countTrue(mask)
		if n > bestInliers {
			bestInliers, best, bestMask = n, h, mask
		}
	}
	if bestInliers < 4 {
		return nil, &errs.Error{Code: errs.EHomo, Message: "RANSAC did not find a valid homography", Details: map[string]any{"inliers": max(0, bestInliers)}}
	}
	for refine := 0; refine < 3; refine++ {
		var in []model.PointPair
		for i, ok := range bestMask {
			if ok {
				in = append(in, points[i])
			}
		}
		h, _, err := dlt(in)
		if err != nil {
			break
		}
		mask, _ := classify(points, h, threshold)
		if countTrue(mask) >= countTrue(bestMask) {
			best, bestMask = h, mask
		}
	}
	mask, residuals := classify(points, best, threshold)
	n := countTrue(mask)
	if n < 4 {
		return nil, &errs.Error{Code: errs.EHomo, Message: "refined homography is degenerate", Actual: n, Threshold: 4}
	}
	var inRes []float64
	for i, ok := range mask {
		if ok {
			inRes = append(inRes, residuals[i])
		}
	}
	sort.Float64s(inRes)
	median := inRes[len(inRes)/2]
	if len(inRes)%2 == 0 {
		median = (inRes[len(inRes)/2-1] + inRes[len(inRes)/2]) / 2
	}
	_, cond, err := dlt(pointsForMask(points, mask))
	if err != nil {
		return nil, &errs.Error{Code: errs.EHomo, Message: "homography covariance is degenerate", Cause: err}
	}
	if math.IsInf(cond, 0) || math.IsNaN(cond) || cond > maxCondition {
		return nil, &errs.Error{Code: errs.EHomo, Message: "homography covariance condition number exceeds limit", Actual: cond, Threshold: maxCondition}
	}
	best = best.Normalized()
	if !best.Finite() {
		return nil, &errs.Error{Code: errs.EHomo, Message: "homography contains non-finite values"}
	}
	return &Result{
		H: best, Inliers: mask, InlierCount: n, ResidualMedian: median,
		Residuals: residuals, Condition: cond, Iterations: iterations,
	}, nil
}

func pointsForMask(points []model.PointPair, mask []bool) []model.PointPair {
	out := make([]model.PointPair, 0, len(points))
	for i, ok := range mask {
		if ok {
			out = append(out, points[i])
		}
	}
	return out
}

func randomSample(rng *rand.Rand, n, k int) []int {
	used := make(map[int]bool, k)
	out := make([]int, 0, k)
	for len(out) < k {
		v := rng.Intn(n)
		if !used[v] {
			used[v] = true
			out = append(out, v)
		}
	}
	return out
}

func seedFromPoints(points []model.PointPair) int64 {
	var s int64 = 1469598103934665603
	mix := func(v float64) {
		u := math.Float64bits(v)
		s ^= int64(u)
		s *= 1099511628211
	}
	for _, p := range points {
		mix(p.Source.X)
		mix(p.Source.Y)
		mix(p.Target.X)
		mix(p.Target.Y)
	}
	return s
}

func classify(points []model.PointPair, h model.Matrix3, threshold float64) ([]bool, []float64) {
	mask := make([]bool, len(points))
	res := make([]float64, len(points))
	for i, p := range points {
		x, y := h.Apply(p.Source.X, p.Source.Y)
		d := math.Hypot(x-p.Target.X, y-p.Target.Y)
		res[i] = d
		mask[i] = d <= threshold && !math.IsNaN(d) && !math.IsInf(d, 0)
	}
	return mask, res
}

func countTrue(mask []bool) int {
	n := 0
	for _, v := range mask {
		if v {
			n++
		}
	}
	return n
}

func dlt(points []model.PointPair) (model.Matrix3, float64, error) {
	if len(points) < 4 {
		return model.Matrix3{}, math.Inf(1), errs.New(errs.EHomo, "DLT needs at least four points")
	}
	src, ts := normalize(points, true)
	dst, td := normalize(points, false)
	var ata [8][8]float64
	var atb [8]float64
	for i := range points {
		x, y := src[i].X, src[i].Y
		u, v := dst[i].X, dst[i].Y
		rows := [2][8]float64{
			{x, y, 1, 0, 0, 0, -u * x, -u * y},
			{0, 0, 0, x, y, 1, -v * x, -v * y},
		}
		rhs := [2]float64{u, v}
		for r := 0; r < 2; r++ {
			for a := 0; a < 8; a++ {
				atb[a] += rows[r][a] * rhs[r]
				for b := 0; b < 8; b++ {
					ata[a][b] += rows[r][a] * rows[r][b]
				}
			}
		}
	}
	eig := jacobiEigen(ata)
	minE, maxE := math.Inf(1), 0.0
	for _, v := range eig {
		if v < minE {
			minE = v
		}
		if v > maxE {
			maxE = v
		}
	}
	cond := math.Inf(1)
	if minE > 1e-14 {
		cond = math.Sqrt(maxE / minE)
	}
	vec, ok := solve8(ata, atb)
	if !ok {
		return model.Matrix3{}, cond, errs.New(errs.EHomo, "normal equations are singular")
	}
	hn := model.Matrix3{vec[0], vec[1], vec[2], vec[3], vec[4], vec[5], vec[6], vec[7], 1}
	tdInv, ok := td.Inverse()
	if !ok {
		return model.Matrix3{}, cond, errs.New(errs.EHomo, "target normalization is singular")
	}
	h := tdInv.Mul(hn).Mul(ts)
	h = h.Normalized()
	if !h.Finite() {
		return model.Matrix3{}, cond, errs.New(errs.EHomo, "DLT produced non-finite values")
	}
	return h, cond, nil
}

func solve8(a [8][8]float64, b [8]float64) ([8]float64, bool) {
	for i := 0; i < 8; i++ {
		pivot := i
		for j := i + 1; j < 8; j++ {
			if math.Abs(a[j][i]) > math.Abs(a[pivot][i]) {
				pivot = j
			}
		}
		if math.Abs(a[pivot][i]) < 1e-12 {
			return [8]float64{}, false
		}
		if pivot != i {
			a[i], a[pivot] = a[pivot], a[i]
			b[i], b[pivot] = b[pivot], b[i]
		}
		pv := a[i][i]
		for k := i; k < 8; k++ {
			a[i][k] /= pv
		}
		b[i] /= pv
		for j := 0; j < 8; j++ {
			if j == i {
				continue
			}
			f := a[j][i]
			if f == 0 {
				continue
			}
			for k := i; k < 8; k++ {
				a[j][k] -= f * a[i][k]
			}
			b[j] -= f * b[i]
		}
	}
	return b, true
}

func normalize(points []model.PointPair, source bool) ([]model.Point, model.Matrix3) {
	out := make([]model.Point, len(points))
	meanX, meanY := 0.0, 0.0
	for i, p := range points {
		q := p.Target
		if source {
			q = p.Source
		}
		out[i] = q
		meanX += q.X
		meanY += q.Y
	}
	meanX /= float64(len(points))
	meanY /= float64(len(points))
	meanDist := 0.0
	for _, p := range out {
		meanDist += math.Hypot(p.X-meanX, p.Y-meanY)
	}
	meanDist /= float64(len(points))
	scale := 1.0
	if meanDist > 1e-12 {
		scale = math.Sqrt2 / meanDist
	}
	t := model.Matrix3{scale, 0, -scale * meanX, 0, scale, -scale * meanY, 0, 0, 1}
	for i := range out {
		out[i].X = scale * (out[i].X - meanX)
		out[i].Y = scale * (out[i].Y - meanY)
	}
	return out, t
}

func jacobiEigen(a [8][8]float64) [8]float64 {
	var v [8][8]float64
	for i := 0; i < 8; i++ {
		v[i][i] = 1
	}
	for iter := 0; iter < 80; iter++ {
		p, q := 0, 1
		maxOff := math.Abs(a[p][q])
		for i := 0; i < 8; i++ {
			for j := i + 1; j < 8; j++ {
				if math.Abs(a[i][j]) > maxOff {
					maxOff = math.Abs(a[i][j])
					p, q = i, j
				}
			}
		}
		if maxOff < 1e-12 {
			break
		}
		theta := 0.5 * math.Atan2(2*a[p][q], a[q][q]-a[p][p])
		c, s := math.Cos(theta), math.Sin(theta)
		for k := 0; k < 8; k++ {
			akp, akq := a[k][p], a[k][q]
			a[k][p] = c*akp - s*akq
			a[k][q] = s*akp + c*akq
		}
		for k := 0; k < 8; k++ {
			apk, aqk := a[p][k], a[q][k]
			a[p][k] = c*apk - s*aqk
			a[q][k] = s*apk + c*aqk
		}
		for k := 0; k < 8; k++ {
			vkp, vkq := v[k][p], v[k][q]
			v[k][p] = c*vkp - s*vkq
			v[k][q] = s*vkp + c*vkq
		}
	}
	var out [8]float64
	for i := 0; i < 8; i++ {
		out[i] = a[i][i]
	}
	return out
}

func smallestEigenvector(a [8][8]float64) ([8]float64, bool) {
	var v [8][8]float64
	for i := 0; i < 8; i++ {
		v[i][i] = 1
	}
	for iter := 0; iter < 100; iter++ {
		p, q := 0, 1
		maxOff := math.Abs(a[p][q])
		for i := 0; i < 8; i++ {
			for j := i + 1; j < 8; j++ {
				if math.Abs(a[i][j]) > maxOff {
					maxOff = math.Abs(a[i][j])
					p, q = i, j
				}
			}
		}
		if maxOff < 1e-13 {
			break
		}
		theta := 0.5 * math.Atan2(2*a[p][q], a[q][q]-a[p][p])
		c, s := math.Cos(theta), math.Sin(theta)
		for k := 0; k < 8; k++ {
			akp, akq := a[k][p], a[k][q]
			a[k][p] = c*akp - s*akq
			a[k][q] = s*akp + c*akq
		}
		for k := 0; k < 8; k++ {
			apk, aqk := a[p][k], a[q][k]
			a[p][k] = c*apk - s*aqk
			a[q][k] = s*apk + c*aqk
		}
		for k := 0; k < 8; k++ {
			vkp, vkq := v[k][p], v[k][q]
			v[k][p] = c*vkp - s*vkq
			v[k][q] = s*vkp + c*vkq
		}
	}
	minIdx := 0
	for i := 1; i < 8; i++ {
		if a[i][i] < a[minIdx][minIdx] {
			minIdx = i
		}
	}
	var out [8]float64
	norm := 0.0
	for i := 0; i < 8; i++ {
		out[i] = v[i][minIdx]
		norm += out[i] * out[i]
	}
	if norm < 1e-20 {
		return out, false
	}
	norm = math.Sqrt(norm)
	for i := range out {
		out[i] /= norm
	}
	return out, true
}

func TransformPoints(h model.Matrix3, pts []model.Point) []model.Point {
	out := make([]model.Point, len(pts))
	for i, p := range pts {
		out[i].X, out[i].Y = h.Apply(p.X, p.Y)
	}
	return out
}

func MedianResidual(points []model.PointPair, h model.Matrix3) float64 {
	if len(points) == 0 {
		return math.NaN()
	}
	_, res := classify(points, h, math.MaxFloat64)
	sort.Float64s(res)
	if len(res)%2 == 1 {
		return res[len(res)/2]
	}
	return (res[len(res)/2-1] + res[len(res)/2]) / 2
}
