package vision

import (
	"math"

	"maskpipe/internal/model"
)

func nearestSeed(seeds []model.IntPoint, x, y int) model.IntPoint {
	if len(seeds) == 0 {
		return model.IntPoint{X: x, Y: y}
	}
	best := seeds[0]
	bestD := math.MaxFloat64
	for _, seed := range seeds {
		dx := float64(seed.X - x)
		dy := float64(seed.Y - y)
		d := dx*dx + dy*dy
		if d < bestD {
			bestD = d
			best = seed
		}
	}
	return best
}

// ThresholdTile converts the soft color/gradient score into the raw binary
// binary mask before morphology.
func ThresholdTile(img *nrgbaImage, grown []byte, seeds []model.IntPoint, rect model.BBox, p model.ParameterSet) []byte {
	tileW := rect.MaxX - rect.MinX + 1
	seedColors := make(map[model.IntPoint]RGB, len(seeds))
	for _, seed := range seeds {
		seedColors[seed] = averageSeedColor(img, seed)
	}
	out := append([]byte(nil), grown...)
	for i, v := range grown {
		if v == 0 {
			continue
		}
		x := rect.MinX + i%tileW
		y := rect.MinY + i/tileW
		r, g, b := clampedPixel(img, x, y)
		seed := nearestSeed(seeds, x, y)
		seedColor := seedColors[seed]
		colorScore := maxFloat01(1 - colorDistance(RGB{r, g, b}, seedColor)/p.ColorTolerance)
		gradScore := 1 - minFloat(1, gradientAt(img, x, y)/p.GradientThreshold)
		score := 0.85*colorScore + 0.15*gradScore
		if score < p.BinaryThreshold {
			out[i] = 0
		}
	}
	return out
}

func maxFloat01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
