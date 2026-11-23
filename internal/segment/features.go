package segment

import (
	"math"

	"maskpipeline/internal/model"
)

type Features struct {
	Luma []float64
	Grad []float64
}

func BuildFeatures(im *model.Image) Features {
	w, h := im.Width, im.Height
	luma := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			p := im.RGBAt(x, y)
			luma[y*w+x] = 0.299*float64(p[0]) + 0.587*float64(p[1]) + 0.114*float64(p[2])
		}
	}
	grad := make([]float64, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			xm, xp := maxi(0, x-1), mini(w-1, x+1)
			ym, yp := maxi(0, y-1), mini(h-1, y+1)
			tl := luma[ym*w+xm]
			tc := luma[ym*w+x]
			tr := luma[ym*w+xp]
			ml := luma[y*w+xm]
			mr := luma[y*w+xp]
			bl := luma[yp*w+xm]
			bc := luma[yp*w+x]
			br := luma[yp*w+xp]
			gx := (tr + 2*mr + br - tl - 2*ml - bl) / 4.0
			gy := (bl + 2*bc + br - tl - 2*tc - tr) / 4.0
			grad[y*w+x] = math.Sqrt(gx*gx + gy*gy)
		}
	}
	return Features{Luma: luma, Grad: grad}
}

func colorDistanceSquared(a, b model.RGB) float64 {
	dr := float64(a[0]) - float64(b[0])
	dg := float64(a[1]) - float64(b[1])
	db := float64(a[2]) - float64(b[2])
	return dr*dr + dg*dg + db*db
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}
