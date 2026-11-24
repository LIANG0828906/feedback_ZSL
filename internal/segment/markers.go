package segment

import (
	"math"

	"iconbench/internal/imaging"
	"iconbench/internal/model"
)

type plane struct{ c [3]float64 }

func BuildMarkers(rgb *imaging.RGBImage, p model.Params) MarkerResult {
	blurred := imaging.GaussianBlurRGB(rgb, maxInt(1, p.SmoothKernel))
	w, h := rgb.Width, rgb.Height
	planes := fitBackgroundPlanes(blurred)
	residual := make([]float64, w*h)
	hist := make([]int, 256)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			pr := planes[0].c[0] + planes[0].c[1]*float64(x) + planes[0].c[2]*float64(y)
			pg := planes[1].c[0] + planes[1].c[1]*float64(x) + planes[1].c[2]*float64(y)
			pb := planes[2].c[0] + planes[2].c[1]*float64(x) + planes[2].c[2]*float64(y)
			dr := blurred.R[i] - pr
			dg := blurred.G[i] - pg
			db := blurred.B[i] - pb
			score := math.Sqrt(dr*dr+dg*dg+db*db) / math.Sqrt(3)
			if score > 255 {
				score = 255
			}
			residual[i] = score
			hist[int(score)]++
		}
	}

	otsu := otsuThreshold(hist, w*h)
	fgThreshold := math.Max(42, math.Min(88, otsu))
	bgThreshold := math.Max(26, fgThreshold*0.58)

	confident := make([]byte, w*h)
	for i, v := range residual {
		if v >= fgThreshold {
			confident[i] = 1
		}
	}
	core := imaging.Erode(confident, w, h, 1)
	coreLabels, coreStats := imaging.ConnectedComponents(core, w, h)

	labels := make([]int32, w*h)
	var markers []Marker
	var fgID int32
	for idx, st := range coreStats {
		if st.Area < 5 {
			continue
		}
		fgID++
		originalID := int32(idx + 1)
		for i := range labels {
			if coreLabels[i] == originalID {
				labels[i] = fgID
			}
		}
		markers = append(markers, Marker{
			ID: fgID, Class: 1,
			X: int(math.Round(st.CentroidX)), Y: int(math.Round(st.CentroidY)),
			Score: averageScore(residual, coreLabels, originalID), Area: st.Area,
		})
	}

	bgID := fgID + 1
	bgCore := imaging.Erode(bytesThreshold(residual, func(v float64) bool { return v < bgThreshold }), w, h, 1)
	bgArea := 0
	var bgSX, bgSY int64
	var bgScore float64
	for i, v := range bgCore {
		if v != 0 {
			labels[i] = bgID
			bgArea++
			bgSX += int64(i % w)
			bgSY += int64(i / w)
			bgScore += residual[i]
		}
	}
	if bgArea == 0 {
		best := 0
		bestScore := math.MaxFloat64
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				border := x == 0 || y == 0 || x == w-1 || y == h-1
				i := y*w + x
				if border && residual[i] < bestScore {
					bestScore = residual[i]
					best = i
				}
			}
		}
		if bestScore == math.MaxFloat64 {
			best = 0
			bestScore = residual[0]
		}
		labels[best] = bgID
		bgArea = 1
		bgSX = int64(best % w)
		bgSY = int64(best / w)
		bgScore = bestScore
	}
	markers = append(markers, Marker{
		ID: bgID, Class: 0,
		X: int(bgSX / int64(bgArea)), Y: int(bgSY / int64(bgArea)),
		Score: bgScore / float64(bgArea), Area: bgArea,
	})

	return MarkerResult{
		Labels: labels, Markers: markers, Residual: residual,
		FGThreshold: fgThreshold, BGThreshold: bgThreshold, OtsuThreshold: otsu,
	}
}

func bytesThreshold(v []float64, keep func(float64) bool) []byte {
	out := make([]byte, len(v))
	for i, x := range v {
		if keep(x) {
			out[i] = 1
		}
	}
	return out
}

func averageScore(scores []float64, labels []int32, id int32) float64 {
	var sum float64
	n := 0
	for i, label := range labels {
		if label == id {
			sum += scores[i]
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func otsuThreshold(hist []int, total int) float64 {
	var sum float64
	for i, n := range hist {
		sum += float64(i * n)
	}
	var sumB float64
	var wB int
	maxVar := -1.0
	threshold := 55.0
	for t := 0; t < 256; t++ {
		wB += hist[t]
		if wB == 0 {
			continue
		}
		wF := total - wB
		if wF == 0 {
			break
		}
		sumB += float64(t * hist[t])
		mB := sumB / float64(wB)
		mF := (sum - sumB) / float64(wF)
		between := float64(wB*wF) * (mB - mF) * (mB - mF)
		if between > maxVar {
			maxVar = between
			threshold = float64(t)
		}
	}
	return threshold
}

func fitBackgroundPlanes(rgb *imaging.RGBImage) [3]plane {
	w, h := rgb.Width, rgb.Height
	channels := [3][]float64{rgb.R, rgb.G, rgb.B}
	var out [3]plane
	for c := 0; c < 3; c++ {
		var s00, s01, s02, s11, s12, s22 float64
		var t0, t1, t2 float64
		n := 0.0
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if x != 0 && x != w-1 && y != 0 && y != h-1 {
					continue
				}
				xf, yf := float64(x), float64(y)
				v := channels[c][y*w+x]
				s00 += 1
				s01 += xf
				s02 += yf
				s11 += xf * xf
				s12 += xf * yf
				s22 += yf * yf
				t0 += v
				t1 += xf * v
				t2 += yf * v
				n++
			}
		}
		if n < 3 {
			out[c] = plane{c: [3]float64{channels[c][0], 0, 0}}
			continue
		}
		a := [3][4]float64{
			{s00, s01, s02, t0},
			{s01, s11, s12, t1},
			{s02, s12, s22, t2},
		}
		out[c] = plane{c: solve3(a)}
	}
	return out
}

func solve3(a [3][4]float64) [3]float64 {
	for col := 0; col < 3; col++ {
		pivot := col
		for r := col + 1; r < 3; r++ {
			if math.Abs(a[r][col]) > math.Abs(a[pivot][col]) {
				pivot = r
			}
		}
		a[col], a[pivot] = a[pivot], a[col]
		if math.Abs(a[col][col]) < 1e-12 {
			continue
		}
		for r := 0; r < 3; r++ {
			if r == col {
				continue
			}
			f := a[r][col] / a[col][col]
			for k := col; k <= 3; k++ {
				a[r][k] -= f * a[col][k]
			}
		}
	}
	var x [3]float64
	for i := 0; i < 3; i++ {
		if math.Abs(a[i][i]) > 1e-12 {
			x[i] = a[i][3] / a[i][i]
		}
	}
	return x
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
