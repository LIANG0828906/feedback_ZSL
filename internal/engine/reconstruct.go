package engine

import (
	"math"
	"sort"

	"atelier/internal/geometry"
	"atelier/internal/hashutil"
	"atelier/internal/model"
)

func (e *Engine) reconstructZone(zone model.Zone, estimate model.DensityEstimate) []model.ResourcePoint {
	if len(estimate.Layers) == 0 {
		return nil
	}
	seeded := hashutil.Uint64(
		e.model.Seed,
		"reconstruct",
		zone.ID,
		zone.Params.DensityScale,
		zone.Params.RarityShift,
		estimate.Density,
		estimate.SampleCount,
	)
	var points []model.ResourcePoint
	for _, layer := range estimate.Layers {
		target := int(math.Round(layer.Density * layer.Area))
		if target <= 0 {
			continue
		}
		layerSeed := hashutil.Uint64(seeded, layer.Terrain, layer.Density, layer.Area)
		rng := hashutil.NewSplitMix(layerSeed)
		b := zone.Polygon.Bounds()
		generated := 0
		attempts := 0
		maxAttempts := target*400 + 10000
		for generated < target && attempts < maxAttempts {
			attempts++
			x := b.MinX + rng.Float64()*b.Width()
			y := b.MinY + rng.Float64()*b.Height()
			p := model.Point{X: x, Y: y}
			if !geometry.StrictlyInside(p, zone.Polygon) ||
				e.model.Region.Terrain.AtXY(x, y) != layer.Terrain ||
				e.excluded(p) {
				continue
			}
			points = append(points, model.ResourcePoint{
				ZoneID:  zone.ID,
				X:       math.Round(x*1e5) / 1e5,
				Y:       math.Round(y*1e5) / 1e5,
				Terrain: layer.Terrain,
				Rarity:  chooseRarity(rng.Next(), estimate.Rarity),
			})
			generated++
		}
	}
	sort.SliceStable(points, func(i, j int) bool {
		if points[i].X != points[j].X {
			return points[i].X < points[j].X
		}
		if points[i].Y != points[j].Y {
			return points[i].Y < points[j].Y
		}
		return points[i].Terrain < points[j].Terrain
	})
	for i := range points {
		points[i].ID = canonicalPointID(zone.ID, i)
	}
	return points
}

func canonicalPointID(zoneID string, index int) string {
	return zoneID + "-resource-" + leftPad(index, 5)
}

func leftPad(v int, width int) string {
	if v == 0 {
		return "00000"
	}
	digits := make([]byte, 0, width)
	for v > 0 {
		digits = append(digits, byte('0'+v%10))
		v /= 10
	}
	for len(digits) < width {
		digits = append(digits, '0')
	}
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

func chooseRarity(hash uint64, ratios map[string]float64) string {
	u := float64(hash>>11) / float64(1<<53)
	var cumulative float64
	for _, kind := range model.RarityKinds {
		cumulative += ratios[kind]
		if u <= cumulative {
			return kind
		}
	}
	return model.RarityKinds[len(model.RarityKinds)-1]
}
