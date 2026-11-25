package bounds

import (
	"math/bits"

	"semanticshading/internal/apierr"
	"semanticshading/internal/model"
)

type Measurements struct {
	MeanQ8        int64 `json:"mean_q8"`
	StdQ8         int64 `json:"std_q8"`
	BoundaryQ8    int64 `json:"boundary_q8"`
	PropagationQ8 int64 `json:"propagation_q8"`
}

func maxRGBL2Q8(fracBits int) int64 {
	scale := int64(1) << fracBits
	sq := 3 * 255 * 255 * scale * scale
	x := int64(1) << uint((bits.Len64(uint64(sq))+1)/2)
	for {
		y := (x + sq/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

func Compute(gear, bits, tile, fracBits int, regionLabel int) model.ErrorBound {
	if fracBits <= 0 {
		fracBits = model.FixedFractionBits
	}
	scale := int64(1) << fracBits
	if bits < 1 {
		bits = 1
	}
	if bits > 8 {
		bits = 8
	}
	if tile <= 0 {
		tile = 1
	}
	step := 256 / (1 << bits)
	if step < 1 {
		step = 1
	}
	quantStepQ8 := int64(step) * scale
	maxDistanceQ8 := maxRGBL2Q8(fracBits)
	gearTermQ8 := int64(gear) * scale / 8
	tileTermQ8 := (256 / int64(tile)) * scale / 16
	truncTermQ8 := int64(1) << (fracBits / 2)

	return model.ErrorBound{
		RegionLabel:        regionLabel,
		MeanUpperQ8:        maxDistanceQ8 + quantStepQ8/2 + gearTermQ8 + truncTermQ8,
		StdUpperQ8:         maxDistanceQ8 + quantStepQ8/4 + gearTermQ8 + truncTermQ8*4,
		BoundaryUpperQ8:    maxDistanceQ8 + quantStepQ8 + truncTermQ8*2,
		PropagationUpperQ8: maxDistanceQ8 + quantStepQ8 + tileTermQ8 + truncTermQ8*4,
		QuantGear:          gear,
		QuantBits:          bits,
		TileSize:           tile,
		FixedFractionBits:  fracBits,
	}
}

func Check(regionLabel int, measured Measurements, bound model.ErrorBound) *apierr.Error {
	values := [4]int64{measured.MeanQ8, measured.StdQ8, measured.BoundaryQ8, measured.PropagationQ8}
	uppers := [4]int64{bound.MeanUpperQ8, bound.StdUpperQ8, bound.BoundaryUpperQ8, bound.PropagationUpperQ8}
	fields := [4]string{"mean_q8", "std_q8", "boundary_jump_q8", "propagation_q8"}
	for i := range values {
		if values[i] > uppers[i] {
			return apierr.New(apierr.EBound, fields[i],
				"region %d measured %s=%d exceeds upper bound %d", regionLabel, fields[i], values[i], uppers[i]).
				WithDetail("region_id", regionLabel).
				WithDetail("measured_values", values).
				WithDetail("upper_bounds", uppers).
				WithDetail("all_four_measured_values", map[string]int64{
					"mean_q8":        measured.MeanQ8,
					"std_q8":         measured.StdQ8,
					"boundary_q8":    measured.BoundaryQ8,
					"propagation_q8": measured.PropagationQ8,
				})
		}
	}
	return nil
}
