package server

import (
	"math"

	"ms-workbench/internal/engine"
)

type Histogram struct {
	Label    string  `json:"label"`
	Kind     string  `json:"kind"`
	Bins     []int   `json:"bins"`
	Peak     float64 `json:"peak"`
	MaxCount int     `json:"maxCount"`
}

func buildHistograms(comp *engine.Composite) []Histogram {
	if comp == nil || comp.Width <= 0 || comp.Height <= 0 {
		return nil
	}
	bands := len(comp.BandIDs)
	out := make([]Histogram, 0, bands+1)
	const binCount = 64
	for bi, id := range comp.BandIDs {
		vals := make([]float64, comp.Width*comp.Height)
		peak := 0.0
		for p := range vals {
			vals[p] = comp.Fused[p*bands+bi]
			if vals[p] > peak {
				peak = vals[p]
			}
		}
		out = append(out, makeHistogram("Radiance "+id, "band", vals, binCount, peak))
	}
	luma := make([]float64, comp.Width*comp.Height)
	peak := 0.0
	for p := range luma {
		idx := p * 3
		luma[p] = 0.2126*comp.LinearRGB[idx] + 0.7152*comp.LinearRGB[idx+1] + 0.0722*comp.LinearRGB[idx+2]
		if luma[p] > peak {
			peak = luma[p]
		}
	}
	out = append(out, makeHistogram("Composite Luma", "rgb", luma, binCount, peak))
	return out
}

func makeHistogram(label, kind string, values []float64, bins int, peak float64) Histogram {
	h := Histogram{Label: label, Kind: kind, Bins: make([]int, bins), Peak: peak}
	if len(values) == 0 {
		return h
	}
	if peak <= 0 {
		peak = 1
	}
	for _, v := range values {
		idx := int(math.Floor(v / peak * float64(bins-1)))
		if idx < 0 {
			idx = 0
		} else if idx >= bins {
			idx = bins - 1
		}
		h.Bins[idx]++
		if h.Bins[idx] > h.MaxCount {
			h.MaxCount = h.Bins[idx]
		}
	}
	return h
}
