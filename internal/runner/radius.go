package runner

import (
	"edgerefine/internal/model"
	"edgerefine/internal/sample"
)

type RadiusPoint struct {
	Radius       int     `json:"radius"`
	SignedMedian float64 `json:"signed_median_px"`
	Median       float64 `json:"median_px"`
	Upper        float64 `json:"upper_px"`
	P95          float64 `json:"p95_px"`
	MaskHash     string  `json:"mask_hash"`
}

func RadiusSweep(b *sample.Bundle, base model.Params, radii ...int) ([]RadiusPoint, bool, error) {
	if len(radii) == 0 {
		radii = []int{0, 1, 2, 3, 4}
	}
	out := make([]RadiusPoint, 0, len(radii))
	for _, r := range radii {
		p := base
		p.Radius = r
		rep, err := Run(b, Options{Workers: 1, Params: p})
		if err != nil {
			return nil, false, err
		}
		point := RadiusPoint{Radius: r, MaskHash: rep.MaskHash}
		if rep.Accuracy != nil {
			point.SignedMedian = rep.Accuracy.SignedMedian
			point.Median = rep.Accuracy.Median
			point.Upper = rep.Accuracy.Upper
			point.P95 = rep.Accuracy.P95
		}
		out = append(out, point)
	}
	monotonic := true
	for i := 1; i < len(out); i++ {
		if out[i].SignedMedian > out[i-1].SignedMedian+1e-9 {
			monotonic = false
		}
	}
	return out, monotonic, nil
}
