package colormetric

import (
	"math/bits"

	"semanticshading/internal/apierr"
	"semanticshading/internal/model"
)

type Spec struct {
	Name         string
	WindowRadius int
}

func DefaultSpec() Spec {
	return Spec{Name: "rgb_l2_q8", WindowRadius: 1}
}

func (s Spec) Validate() error {
	if s.Name != "rgb_l2_q8" && s.Name != "rgb_l1_q8" {
		return apierr.New(apierr.ESchema, "metric", "unsupported color metric %q", s.Name)
	}
	if s.WindowRadius < 0 || s.WindowRadius > 4 {
		return apierr.New(apierr.EPara, "window_radius", "window_radius must be in [0,4], got %d", s.WindowRadius).
			WithDetail("valid_range", [2]int{0, 4})
	}
	return nil
}

func RequiredBand(radius int) int {
	return radius + 1
}

func RecommendedBand(tile, radius int) int {
	if tile <= 0 {
		return 0
	}
	extra := tile / 16
	if extra < 1 {
		extra = 1
	}
	band := radius + extra
	if band > tile {
		return tile
	}
	return band
}

func isqrt64(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := int64(1) << uint((bits.Len64(uint64(n))+1)/2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

func PixelDistanceQ8(ref, recon []byte, channels, offset int, metric string) int64 {
	dr := int64(ref[offset]) - int64(recon[offset])
	dg := int64(ref[offset+1]) - int64(recon[offset+1])
	db := int64(ref[offset+2]) - int64(recon[offset+2])
	if dr < 0 {
		dr = -dr
	}
	if dg < 0 {
		dg = -dg
	}
	if db < 0 {
		db = -db
	}
	switch metric {
	case "rgb_l1_q8":
		return (dr + dg + db) * model.FixedScale
	default:
		sumSq := dr*dr + dg*dg + db*db
		return isqrt64(sumSq * model.FixedScale * model.FixedScale)
	}
}

func WindowDistanceQ8(ref, recon []byte, width, height, channels int, x, y, radius int, metric string) (int64, model.TruncationMark) {
	x0 := x - radius
	x1 := x + radius
	y0 := y - radius
	y1 := y + radius
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 >= width {
		x1 = width - 1
	}
	if y1 >= height {
		y1 = height - 1
	}
	var sum int64
	var count int64
	for yy := y0; yy <= y1; yy++ {
		row := yy * width * channels
		for xx := x0; xx <= x1; xx++ {
			sum += PixelDistanceQ8(ref, recon, channels, row+xx*channels, metric)
			count++
		}
	}
	mean := sum / count
	return mean, model.TruncationMark{
		Stage:       "window_mean_div",
		Position:    "colormetric.WindowDistanceQ8",
		Numerator:   sum,
		Denominator: count,
		Remainder:   sum % count,
	}
}

func DifferenceJumpQ8(ref, recon []byte, width, height, channels int, p1, p2 model.Point, radius int, metric string) (int64, model.TruncationMark) {
	d1, mark1 := WindowDistanceQ8(ref, recon, width, height, channels, p1.X, p1.Y, radius, metric)
	d2, mark2 := WindowDistanceQ8(ref, recon, width, height, channels, p2.X, p2.Y, radius, metric)
	jump := d1 - d2
	if jump < 0 {
		jump = -jump
	}
	_ = mark2
	return jump, mark1
}

func SqrtQ16ToQ8(sumSquaredQ16, area int64) (int64, model.TruncationMark) {
	if area <= 0 {
		return 0, model.TruncationMark{Stage: "variance_area_div", Position: "colormetric.SqrtQ16ToQ8"}
	}
	meanSquaredQ16 := sumSquaredQ16 / area
	varianceQ16 := meanSquaredQ16
	stdQ8 := isqrt64(varianceQ16)
	return stdQ8, model.TruncationMark{
		Stage:       "std_sqrt_q8_floor",
		Position:    "colormetric.SqrtQ16ToQ8",
		Numerator:   sumSquaredQ16,
		Denominator: area,
		Remainder:   sumSquaredQ16 % area,
	}
}

func StdFromSums(sumQ8, sumSqQ16, area int64) (int64, []model.TruncationMark) {
	if area <= 0 {
		return 0, nil
	}
	meanQ8 := sumQ8 / area
	meanRemainder := sumQ8 % area
	meanSquaredQ16 := sumSqQ16 / area
	sqRemainder := sumSqQ16 % area
	meanSquareQ16 := meanQ8 * meanQ8
	varianceQ16 := meanSquaredQ16 - meanSquareQ16
	if varianceQ16 < 0 {
		varianceQ16 = 0
	}
	stdQ8 := isqrt64(varianceQ16)
	marks := []model.TruncationMark{
		{Stage: "region_mean_div_area", Position: "colormetric.StdFromSums", Numerator: sumQ8, Denominator: area, Remainder: meanRemainder},
		{Stage: "region_second_moment_div_area", Position: "colormetric.StdFromSums", Numerator: sumSqQ16, Denominator: area, Remainder: sqRemainder},
		{Stage: "std_sqrt_q8_floor", Position: "colormetric.StdFromSums", Numerator: varianceQ16},
	}
	return stdQ8, marks
}
