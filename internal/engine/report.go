package engine

import (
	"math"
	"sort"

	"atelier/internal/model"
)

type BoundDerivation struct {
	Formula string         `json:"formula"`
	Inputs  map[string]any `json:"inputs"`
}

type ZoneBiasReport struct {
	ZoneID                       string                     `json:"zoneId"`
	Ordinal                      int                        `json:"ordinal"`
	SampleCount                  int                        `json:"sampleCount"`
	Area                         float64                    `json:"area"`
	QuantizationError            float64                    `json:"quantizationError"`
	StandardError                float64                    `json:"standardError"`
	LayerStandardErrors          map[string]float64         `json:"layerStandardErrors"`
	RarityRoundingResidual       float64                    `json:"rarityRoundingResidual"`
	HeatmapReconstructionError   float64                    `json:"heatmapReconstructionError"`
	QuantizationUpperBound       float64                    `json:"quantizationUpperBound"`
	StandardErrorUpperBound      float64                    `json:"standardErrorUpperBound"`
	RarityUpperBound             float64                    `json:"rarityUpperBound"`
	HeatmapUpperBound            float64                    `json:"heatmapUpperBound"`
	CumulativeQuantization       float64                    `json:"cumulativeQuantizationError"`
	CumulativeStandardError      float64                    `json:"cumulativeStandardError"`
	CumulativeRarityResidual     float64                    `json:"cumulativeRarityResidual"`
	CumulativeHeatmapError       float64                    `json:"cumulativeHeatmapError"`
	CumulativeQuantizationBound  float64                    `json:"cumulativeQuantizationBound"`
	CumulativeStandardErrorBound float64                    `json:"cumulativeStandardErrorBound"`
	CumulativeRarityBound        float64                    `json:"cumulativeRarityBound"`
	CumulativeHeatmapBound       float64                    `json:"cumulativeHeatmapBound"`
	Bounds                       map[string]BoundDerivation `json:"bounds"`
}

type BiasReport struct {
	Zones         []ZoneBiasReport `json:"zones"`
	Inputs        map[string]any   `json:"inputs"`
	GeneratedFrom string           `json:"generatedFrom"`
}

func (e *Engine) buildReport(states []model.ZoneState, heat model.Heatmap) BiasReport {
	report := BiasReport{
		GeneratedFrom: "observed samples, zone effective area, heatmap raster dimensions",
		Inputs: map[string]any{
			"heatmapWidth":     heat.Width,
			"heatmapHeight":    heat.Height,
			"heatmapCellSize":  heat.CellSize,
			"quantizationStep": e.model.Config.QuantizationStep,
			"zoneOrder":        zoneIDs(e.zoneOrder),
		},
	}
	var cumQuant, cumSE, cumRarity, cumHeat float64
	var cumQuantBound, cumSEBound, cumRarityBound, cumHeatBound float64
	for _, state := range states {
		samples := state.Samples
		if len(samples) == 0 {
			continue
		}
		var quantSum float64
		var minDensity, maxDensity = math.Inf(1), math.Inf(-1)
		var heatSum float64
		heatCount := 0
		for _, s := range samples {
			quantSum += math.Abs(s.Density - s.Quantized)
			minDensity = math.Min(minDensity, s.Quantized)
			maxDensity = math.Max(maxDensity, s.Quantized)
			if v, ok := heat.ValueAtXY(s.X, s.Y); ok {
				heatSum += math.Abs(v - s.Quantized)
				heatCount++
			}
		}
		quantErr := quantSum / float64(len(samples))
		standardErr := combinedStandardError(state.Estimate, state.Estimate.EffectiveArea)
		layerSE := map[string]float64{}
		for _, layer := range state.Estimate.Layers {
			layerSE[layer.Terrain] = layer.StandardError
		}
		rarityResidual := rarityRoundingResidual(state.Estimate.Rarity, len(state.Points))
		heatErr := 0.0
		if heatCount > 0 {
			heatErr = heatSum / float64(heatCount)
		}
		cumQuant += quantErr
		cumSE += standardErr
		cumRarity += rarityResidual
		cumHeat += heatErr
		rangeSE := 0.0
		if len(samples) > 0 {
			rangeSE = (maxDensity - minDensity) / math.Sqrt(float64(len(samples)))
		}
		quantBound := e.model.Config.QuantizationStep / 2
		rarityBound := rarityResidual + float64(len(model.RarityKinds))*0.5/math.Max(1, float64(len(state.Points)))
		heatBound := quantBound + rangeSE
		cumQuantBound += quantBound
		cumSEBound += rangeSE
		cumRarityBound += rarityBound
		cumHeatBound += heatBound
		zr := ZoneBiasReport{
			ZoneID:                       state.ZoneID,
			Ordinal:                      state.Ordinal,
			SampleCount:                  len(samples),
			Area:                         state.Estimate.EffectiveArea,
			QuantizationError:            quantErr,
			StandardError:                standardErr,
			LayerStandardErrors:          layerSE,
			RarityRoundingResidual:       rarityResidual,
			HeatmapReconstructionError:   heatErr,
			CumulativeQuantization:       cumQuant,
			CumulativeStandardError:      cumSE,
			CumulativeRarityResidual:     cumRarity,
			CumulativeHeatmapError:       cumHeat,
			QuantizationUpperBound:       quantBound,
			StandardErrorUpperBound:      rangeSE,
			RarityUpperBound:             rarityBound,
			HeatmapUpperBound:            heatBound,
			CumulativeQuantizationBound:  cumQuantBound,
			CumulativeStandardErrorBound: cumSEBound,
			CumulativeRarityBound:        cumRarityBound,
			CumulativeHeatmapBound:       cumHeatBound,
			Bounds: map[string]BoundDerivation{
				"quantization": {
					Formula: "quantizationStep / 2",
					Inputs: map[string]any{
						"sampleCount":      len(samples),
						"effectiveArea":    state.Estimate.EffectiveArea,
						"quantizationStep": e.model.Config.QuantizationStep,
					},
				},
				"standardError": {
					Formula: "(maxObserved - minObserved) / sqrt(n)",
					Inputs: map[string]any{
						"sampleCount":        len(samples),
						"effectiveArea":      state.Estimate.EffectiveArea,
						"minObservedDensity": minDensity,
						"maxObservedDensity": maxDensity,
					},
				},
				"rarity": {
					Formula: "observedRoundingResidual + rarityKinds*0.5/max(1, observedPoints)",
					Inputs: map[string]any{
						"reconstructedPoints": len(state.Points),
						"sampleCount":         len(samples),
						"effectiveArea":       state.Estimate.EffectiveArea,
					},
				},
				"heatmap": {
					Formula: "quantizationStep/2 + (maxObserved-minObserved)/sqrt(n), rasterized at configured cell size",
					Inputs: map[string]any{
						"sampleCount":        len(samples),
						"effectiveArea":      state.Estimate.EffectiveArea,
						"rasterWidth":        heat.Width,
						"rasterHeight":       heat.Height,
						"cellSize":           heat.CellSize,
						"quantizationStep":   e.model.Config.QuantizationStep,
						"minObservedDensity": minDensity,
						"maxObservedDensity": maxDensity,
					},
				},
			},
		}
		report.Zones = append(report.Zones, zr)
	}
	return report
}

func combinedStandardError(est model.DensityEstimate, area float64) float64 {
	if area <= 0 {
		return 0
	}
	var variance float64
	for _, layer := range est.Layers {
		weight := layer.Area / area
		variance += weight * weight * layer.StandardError * layer.StandardError
	}
	return math.Sqrt(variance)
}

func rarityRoundingResidual(ratios map[string]float64, points int) float64 {
	if points <= 0 {
		return 0
	}
	var assigned int
	for _, kind := range model.RarityKinds {
		assigned += int(math.Round(ratios[kind] * float64(points)))
	}
	return math.Abs(float64(assigned-points)) / float64(points)
}

func zoneIDs(zones []model.Zone) []string {
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.ID)
	}
	sort.Strings(out)
	return out
}
