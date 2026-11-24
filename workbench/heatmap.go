package workbench

import (
	"image"
	"image/color"
	"math"
	"strconv"

	"yuvench/errs"
	"yuvench/yuv"
)

// Heatmap computes a deterministic difference heatmap. It depends only on the
// two final images and the declared normalization, never on filter state.
func Heatmap(original, enhanced *yuv.Composite, normalization float64) (*image.RGBA, HeatmapStats, error) {
	stats := HeatmapStats{Normalization: normalization}
	if math.IsNaN(normalization) || math.IsInf(normalization, 0) || normalization < 1e-6 || normalization > 4*65535 {
		return nil, stats, &errs.Error{Code: errs.CodeParameter, Field: "heatmap.normalization", Message: "normalization outside allowed range", Expected: "(0,262140]", Actual: normalization}
	}
	if original == nil || enhanced == nil {
		return nil, stats, errs.New(errs.CodeDiff, "missing image for heatmap")
	}
	if original.Width != enhanced.Width || original.Height != enhanced.Height {
		return nil, stats, &errs.Error{Code: errs.CodeDiff, Message: "heatmap image dimensions differ", Plane: "YUV", Expected: [2]int{original.Width, original.Height}, Actual: [2]int{enhanced.Width, enhanced.Height}}
	}
	out := image.NewRGBA(image.Rect(0, 0, original.Width, original.Height))
	total := 0.0
	for y := 0; y < original.Height; y++ {
		for x := 0; x < original.Width; x++ {
			index := y*original.Width + x
			difference := maxAbsDifference(original.Y[index], enhanced.Y[index], original.U[index], enhanced.U[index], original.V[index], enhanced.V[index])
			if difference > stats.MaxDifference {
				stats.MaxDifference = difference
			}
			if difference > 0 {
				stats.ChangedPixels++
			}
			total += difference
			r, g, b := heatColor(difference / normalization)
			out.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
		}
	}
	pixels := original.Width * original.Height
	if pixels > 0 {
		stats.MeanDifference = total / float64(pixels)
	}
	stats.Summary = yuv.HashBytes([]byte("heatmap-v1:"), []byte(strconv.Itoa(original.Width)), []byte("x"), []byte(strconv.Itoa(original.Height)), []byte(":"), []byte(strconv.FormatFloat(normalization, 'g', -1, 64)), []byte(":"), out.Pix)
	return out, stats, nil
}

func maxAbsDifference(values ...uint16) float64 {
	if len(values) < 2 {
		return 0
	}
	maxDifference := 0.0
	for index := 0; index+1 < len(values); index += 2 {
		difference := math.Abs(float64(values[index]) - float64(values[index+1]))
		if difference > maxDifference {
			maxDifference = difference
		}
	}
	return maxDifference
}

func heatColor(value float64) (uint8, uint8, uint8) {
	if value <= 0 {
		return 0, 0, 0
	}
	if value > 1 {
		value = 1
	}
	type stop struct {
		position float64
		r        float64
		g        float64
		b        float64
	}
	stops := []stop{
		{0.0, 0, 0, 0},
		{0.25, 0, 0, 255},
		{0.50, 0, 255, 255},
		{0.75, 255, 255, 0},
		{1.0, 255, 0, 0},
	}
	for index := 1; index < len(stops); index++ {
		if value <= stops[index].position {
			left := stops[index-1]
			right := stops[index]
			fraction := (value - left.position) / (right.position - left.position)
			return uint8(math.Round(left.r + (right.r-left.r)*fraction)),
				uint8(math.Round(left.g + (right.g-left.g)*fraction)),
				uint8(math.Round(left.b + (right.b-left.b)*fraction))
		}
	}
	return 255, 0, 0
}
