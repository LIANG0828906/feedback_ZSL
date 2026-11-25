package engine

import (
	"sort"

	"atelier/internal/geometry"
	"atelier/internal/model"
)

func (e *Engine) selectSamples(zone model.Zone) []model.SamplePoint {
	obs := e.observations[zone.ID]
	byTerrain := map[string][]model.Observation{}
	for _, o := range obs {
		p := model.Point{X: o.X, Y: o.Y}
		if !geometry.StrictlyInside(p, zone.Polygon) || e.excluded(p) {
			continue
		}
		byTerrain[o.Terrain] = append(byTerrain[o.Terrain], o)
	}
	terrains := make([]string, 0, len(byTerrain))
	for terrain := range byTerrain {
		terrains = append(terrains, terrain)
	}
	sort.Strings(terrains)
	if len(terrains) == 0 {
		return nil
	}
	budget := e.model.Config.SamplesPerZone
	if budget < len(terrains) {
		budget = len(terrains)
	}
	alloc := allocateSamples(budget, terrains, byTerrain)
	var samples []model.SamplePoint
	for _, terrain := range terrains {
		candidates := byTerrain[terrain]
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].GridRow != candidates[j].GridRow {
				return candidates[i].GridRow < candidates[j].GridRow
			}
			return candidates[i].GridCol < candidates[j].GridCol
		})
		n := alloc[terrain]
		for k := 0; k < n; k++ {
			idx := int((float64(k) + 0.5) * float64(len(candidates)) / float64(n))
			if idx >= len(candidates) {
				idx = len(candidates) - 1
			}
			o := candidates[idx]
			samples = append(samples, model.SamplePoint{
				ID:        o.ID,
				ZoneID:    zone.ID,
				Terrain:   o.Terrain,
				X:         o.X,
				Y:         o.Y,
				GridRow:   o.GridRow,
				GridCol:   o.GridCol,
				Density:   o.Density,
				Quantized: o.QuantizedDensity,
			})
		}
	}
	sort.Slice(samples, func(i, j int) bool {
		if samples[i].Terrain != samples[j].Terrain {
			return samples[i].Terrain < samples[j].Terrain
		}
		if samples[i].GridRow != samples[j].GridRow {
			return samples[i].GridRow < samples[j].GridRow
		}
		return samples[i].GridCol < samples[j].GridCol
	})
	return samples
}

func allocateSamples(total int, terrains []string, byTerrain map[string][]model.Observation) map[string]int {
	out := map[string]int{}
	remaining := total - len(terrains)
	var candidateTotal int
	for _, terrain := range terrains {
		candidateTotal += len(byTerrain[terrain])
	}
	type fractional struct {
		terrain string
		value   float64
	}
	var fractions []fractional
	assigned := 0
	for _, terrain := range terrains {
		share := 0.0
		if candidateTotal > 0 {
			share = float64(remaining) * float64(len(byTerrain[terrain])) / float64(candidateTotal)
		}
		base := int(share)
		out[terrain] = 1 + base
		assigned += base
		fractions = append(fractions, fractional{terrain: terrain, value: share - float64(base)})
	}
	left := remaining - assigned
	sort.SliceStable(fractions, func(i, j int) bool {
		if fractions[i].value == fractions[j].value {
			return fractions[i].terrain < fractions[j].terrain
		}
		return fractions[i].value > fractions[j].value
	})
	for i := 0; left > 0; i = (i + 1) % len(fractions) {
		out[fractions[i].terrain]++
		left--
	}
	for _, terrain := range terrains {
		if out[terrain] > len(byTerrain[terrain]) {
			out[terrain] = len(byTerrain[terrain])
		}
	}
	return out
}
