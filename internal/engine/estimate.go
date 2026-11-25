package engine

import (
	"math"
	"sort"

	"atelier/internal/model"
)

func (e *Engine) estimateZone(zone model.Zone, samples []model.SamplePoint) model.DensityEstimate {
	byTerrain := map[string][]model.SamplePoint{}
	for _, s := range samples {
		byTerrain[s.Terrain] = append(byTerrain[s.Terrain], s)
	}
	areas := e.zoneTerrainAreas(zone)
	var terrains []string
	for terrain := range areas {
		terrains = append(terrains, terrain)
	}
	sort.Strings(terrains)
	est := model.DensityEstimate{Layers: []model.TerrainEstimate{}}
	var totalDensityArea float64
	var totalArea float64
	var allRarityCounts = map[string]int{}
	var allDensities []float64
	for _, terrain := range terrains {
		area := areas[terrain]
		layerSamples := byTerrain[terrain]
		if len(layerSamples) == 0 {
			continue
		}
		sort.Slice(layerSamples, func(i, j int) bool {
			if layerSamples[i].GridRow != layerSamples[j].GridRow {
				return layerSamples[i].GridRow < layerSamples[j].GridRow
			}
			return layerSamples[i].GridCol < layerSamples[j].GridCol
		})
		mean, se := meanAndSE(layerSamples)
		mean *= zone.Params.DensityScale
		counts := map[string]int{}
		for _, s := range layerSamples {
			allDensities = append(allDensities, s.Density)
		}
		for _, o := range e.observations[zone.ID] {
			if o.Terrain != terrain {
				continue
			}
			matched := false
			for _, s := range layerSamples {
				if s.ID == o.ID {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			for _, kind := range model.RarityKinds {
				counts[kind] += o.RarityCounts[kind]
			}
		}
		for kind, n := range counts {
			allRarityCounts[kind] += n
		}
		layer := model.TerrainEstimate{
			Terrain:       terrain,
			SampleCount:   len(layerSamples),
			Area:          area,
			Density:       mean,
			StandardError: se,
			RarityCounts:  counts,
			Rarity:        ratiosFromCounts(counts),
		}
		est.Layers = append(est.Layers, layer)
		totalDensityArea += mean * area
		totalArea += area
		est.SampleCount += len(layerSamples)
	}
	if totalArea > 0 {
		est.Density = totalDensityArea / totalArea
	}
	est.EffectiveArea = totalArea
	est.Rarity = ratiosFromCounts(allRarityCounts)
	applyRarityShift(est.Rarity, zone.Params.RarityShift)
	for i := range est.Layers {
		est.Layers[i].Rarity = cloneRatios(est.Rarity)
	}
	return est
}

func meanAndSE(samples []model.SamplePoint) (float64, float64) {
	if len(samples) == 0 {
		return 0, 0
	}
	var sum float64
	for _, s := range samples {
		sum += s.Quantized
	}
	mean := sum / float64(len(samples))
	if len(samples) < 2 {
		return mean, 0
	}
	var variance float64
	for _, s := range samples {
		d := s.Quantized - mean
		variance += d * d
	}
	variance /= float64(len(samples) - 1)
	return mean, math.Sqrt(variance / float64(len(samples)))
}

func ratiosFromCounts(counts map[string]int) map[string]float64 {
	total := 0
	for _, n := range counts {
		total += n
	}
	out := map[string]float64{}
	for _, kind := range model.RarityKinds {
		if total > 0 {
			out[kind] = float64(counts[kind]) / float64(total)
		}
	}
	return out
}

func applyRarityShift(r map[string]float64, shift float64) {
	if shift == 0 {
		return
	}
	r["common"] = model.Clamp01(r["common"] - shift*0.10)
	r["rare"] = model.Clamp01(r["rare"] + shift*0.10)
	for kind, v := range r {
		r[kind] = model.Clamp01(v)
	}
	var sum float64
	for _, v := range r {
		sum += v
	}
	if sum > 0 {
		for kind := range r {
			r[kind] /= sum
		}
	}
}

func cloneRatios(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
