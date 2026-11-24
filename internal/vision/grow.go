package vision

import (
	"math"

	"maskpipe/internal/model"
)

type RGB struct {
	R, G, B float64
}

func clampedPixel(img *nrgbaImage, x, y int) (r, g, b float64) {
	bounds := img.Bounds()
	if x < bounds.Min.X {
		x = bounds.Min.X
	} else if x >= bounds.Max.X {
		x = bounds.Max.X - 1
	}
	if y < bounds.Min.Y {
		y = bounds.Min.Y
	} else if y >= bounds.Max.Y {
		y = bounds.Max.Y - 1
	}
	i := img.PixOffset(x, y)
	return float64(img.Pix[i]), float64(img.Pix[i+1]), float64(img.Pix[i+2])
}

func colorDistance(a, b RGB) float64 {
	dr := a.R - b.R
	dg := a.G - b.G
	db := a.B - b.B
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

func luminance(r, g, b float64) float64 {
	return 0.299*r + 0.587*g + 0.114*b
}

func gradientAt(img *nrgbaImage, x, y int) float64 {
	at := func(dx, dy int) float64 {
		r, g, b := clampedPixel(img, x+dx, y+dy)
		return luminance(r, g, b)
	}
	tl := at(-1, -1)
	t := at(0, -1)
	tr := at(1, -1)
	l := at(-1, 0)
	r := at(1, 0)
	bl := at(-1, 1)
	b := at(0, 1)
	br := at(1, 1)
	gx := -tl - 2*l - bl + tr + 2*r + br
	gy := -tl - 2*t - tr + bl + 2*b + br
	return math.Hypot(gx, gy) / 4.0
}

func averageSeedColor(img *nrgbaImage, seed model.IntPoint) RGB {
	var sr, sg, sb, n float64
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			r, g, b := clampedPixel(img, seed.X+dx, seed.Y+dy)
			sr += r
			sg += g
			sb += b
			n++
		}
	}
	return RGB{sr / n, sg / n, sb / n}
}

// RegionGrowTile performs deterministic FIFO region growing for one tile. The same
// inputs always produce the same bytes; worker identity is never consulted.
func RegionGrowTile(img *nrgbaImage, seeds []model.IntPoint, rect model.BBox, p model.ParameterSet) []byte {
	tileW := rect.MaxX - rect.MinX + 1
	tileH := rect.MaxY - rect.MinY + 1
	out := make([]byte, tileW*tileH)
	for _, seed := range seeds {
		if seed.X < rect.MinX || seed.X > rect.MaxX || seed.Y < rect.MinY || seed.Y > rect.MaxY {
			continue
		}
		seedColor := averageSeedColor(img, seed)
		visited := make([]byte, tileW*tileH)
		type qPoint struct{ x, y int }
		queue := make([]qPoint, 0, tileW*tileH/4)
		sx, sy := seed.X-rect.MinX, seed.Y-rect.MinY
		si := sy*tileW + sx
		if visited[si] == 0 {
			visited[si] = 1
			out[si] = 1
			queue = append(queue, qPoint{seed.X, seed.Y})
		}
		iterations := 0
		for len(queue) > 0 && iterations < p.MaxIterations {
			cur := queue[0]
			queue = queue[1:]
			iterations++
			neighbors := [4][2]int{{0, -1}, {1, 0}, {0, 1}, {-1, 0}}
			for _, d := range neighbors {
				nx, ny := cur.x+d[0], cur.y+d[1]
				if nx < rect.MinX || nx > rect.MaxX || ny < rect.MinY || ny > rect.MaxY {
					continue
				}
				idx := (ny-rect.MinY)*tileW + (nx - rect.MinX)
				if visited[idx] != 0 {
					continue
				}
				visited[idx] = 1
				r, g, b := clampedPixel(img, nx, ny)
				col := RGB{r, g, b}
				gv := gradientAt(img, nx, ny)
				dist := colorDistance(col, seedColor)
				strongColorMatch := dist <= p.ColorTolerance*0.7
				gradientOK := gv <= p.GradientThreshold || strongColorMatch
				if dist <= p.ColorTolerance && gradientOK {
					out[idx] = 1
					queue = append(queue, qPoint{nx, ny})
				}
			}
		}
	}

	return out
}
