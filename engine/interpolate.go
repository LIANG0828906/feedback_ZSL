package engine

import (
	"math"
	"sort"

	"performanceexpressions/workspace/domain"
)

func Interpolate(points []domain.ControlPoint, beat float64, mode string) (float64, error) {
	if len(points) == 0 {
		return 0, domain.NewError(domain.CodeSchema, "cannot interpolate an empty layer")
	}
	if !KnownInterpolation(mode) {
		return 0, domain.NewError(domain.CodeSchema, "unsupported interpolation mode %q", mode)
	}
	if beat <= points[0].Beat {
		return points[0].Value, nil
	}
	last := points[len(points)-1]
	if beat >= last.Beat {
		return last.Value, nil
	}
	i := sort.Search(len(points), func(i int) bool { return points[i].Beat >= beat })
	if i < len(points) && points[i].Beat == beat {
		return points[i].Value, nil
	}
	left := points[i-1]
	right := points[i]
	span := right.Beat - left.Beat
	if span <= 0 {
		return left.Value, nil
	}
	if mode == domain.InterpolationHold {
		return left.Value, nil
	}
	t := (beat - left.Beat) / span
	if mode == domain.InterpolationSmoothstep {
		t = t * t * (3 - 2*t)
	}
	return left.Value + (right.Value-left.Value)*t, nil
}

func ChannelValue(ch domain.Channel, beat float64, mode string) (float64, error) {
	layers := make([]domain.Layer, len(ch.Layers))
	copy(layers, ch.Layers)
	sort.Slice(layers, func(i, j int) bool { return layers[i].ID < layers[j].ID })
	sum := 0.0
	for _, layer := range layers {
		v, err := Interpolate(layer.Points, beat, mode)
		if err != nil {
			return 0, err
		}
		sum += v
	}
	return sum, nil
}

func clamp(v, lo, hi float64) float64 {
	return math.Max(lo, math.Min(hi, v))
}
